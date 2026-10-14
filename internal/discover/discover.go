// Package discover finds installed apps: running Docker containers, those
// that match a recipe and, marked unsupported, the others with a port. It
// talks to the Docker API over its socket.
package discover

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/viedin/farstu-client/internal/recipes"
)

// App is a running app: where to reach it, or why it can't be used.
type App struct {
	Upstream string // e.g. "172.18.0.5:2283"
	Problem  string // user-facing, e.g. not set up yet; set means don't route it
	// Unsupported: no recipe knows the container, so nothing checks it has
	// an admin account, limits password guessing at its login, or knows it
	// asks every visitor for a password.
	Unsupported bool
	Image       string // an unsupported app's image repository, e.g. "someone/app"
}

// Watch calls found with the apps running now, by name, and again whenever
// a container starts or stops and every recheck, until ctx is done. While
// Docker is unreachable it calls found with a user-facing error and retries.
//
// Upstreams are container IPs, which Docker hands out again after a
// container stops. Following start/die events keeps a stale route from
// sending one app's visitors to another for longer than one listing.
func Watch(ctx context.Context, socket string, rs []recipes.Recipe, found func(map[string]App, error)) {
	c := docker(socket)
	for {
		err := watch(ctx, c, rs, found)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("app discovery failed, retrying", "err", err)
		found(nil, reason(socket, err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func docker(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 10 * time.Second, // a hung dockerd must surface as an error
	}}
}

// Gateways are the gateways of this client's container networks, when it
// runs on Docker networks rather than the host's. Docker's port proxy (for
// IPv6, localhost, and in rootless Docker) hands every visitor of a
// published port on, the internet's too, from a gateway: a private address.
// None on host networking, outside Docker, or with a container hostname
// other than its ID, which Docker's own is.
func Gateways(ctx context.Context, socket string) ([]netip.Addr, error) {
	id, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var self struct {
		HostConfig      struct{ NetworkMode string }
		NetworkSettings struct {
			Networks map[string]struct{ Gateway, IPv6Gateway string }
		}
	}
	err = getJSON(ctx, docker(socket), "/containers/"+url.PathEscape(id)+"/json", &self)
	if errors.Is(err, statusError(http.StatusNotFound)) {
		return nil, nil // not a container Docker knows by this name: the host's
	}
	if err != nil {
		return nil, err
	}
	if self.HostConfig.NetworkMode == "host" {
		return nil, nil
	}
	var gws []netip.Addr
	for _, n := range self.NetworkSettings.Networks {
		for _, g := range []string{n.Gateway, n.IPv6Gateway} {
			if ip, err := netip.ParseAddr(g); err == nil {
				gws = append(gws, ip)
			}
		}
	}
	return gws, nil
}

// recheck is how often apps are listed without a Docker event: an app
// finishes starting, or gets its admin account, without one.
const recheck = 15 * time.Second

func watch(ctx context.Context, c *http.Client, rs []recipes.Recipe, found func(map[string]App, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the event reader
	// Subscribe before listing, so a container that starts meanwhile is not missed.
	body, err := get(ctx, c, "/events?"+url.Values{"filters": {`{"type":["container"],"event":["start","die"]}`}}.Encode())
	if err != nil {
		return err
	}
	defer body.Close()
	events := make(chan error)
	go func() {
		dec := json.NewDecoder(body)
		for {
			err := dec.Decode(&struct{}{})
			select {
			case events <- err:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		apps, err := list(ctx, c, rs)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if a, ok := apps[r.App]; ok && a.Problem == "" && !r.NoLogin {
				a.Problem = setupProblem(ctx, a.Upstream, r)
				apps[r.App] = a
			}
		}
		found(apps, nil)
		select {
		case err := <-events:
			if err != nil {
				return fmt.Errorf("Docker events: %w", err)
			}
		case <-time.After(recheck):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// appClient asks apps whether they are set up. Not the Docker client: it
// dials the apps over TCP.
var appClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// setupProblem says why an app must not be turned on yet: it has no admin
// account, so whoever opens it first would become its admin, or it can't
// tell. Empty means set up.
func setupProblem(ctx context.Context, upstream string, r recipes.Recipe) string {
	s := r.SetupDone
	name := strings.ToUpper(r.App[:1]) + r.App[1:] // starts a sentence
	notDone := cmp.Or(s.Message, name+" has no admin account yet. Create it from your home network first: whoever opens a new "+r.App+" first becomes its admin")
	req, err := http.NewRequestWithContext(ctx, cmp.Or(s.Method, http.MethodGet), "http://"+upstream+s.Path, nil)
	if err != nil {
		return "Can't check whether " + name + " is set up: " + err.Error()
	}
	// Not the container IP: Nextcloud answers hosts outside its
	// trusted_domains with 400, and always trusts localhost.
	req.Host = "localhost"
	// As visitors arrive: apps with an https URL (Ghost) redirect plain http.
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := appClient.Do(req)
	if err != nil {
		return name + " is not answering yet"
	}
	defer resp.Body.Close()
	// The app answers, but not as the recipe expects: often a fresh install
	// (Docmost answers 404 until its admin exists).
	unsure := func(why string) string {
		return "Can't check whether " + name + " is set up (" + why + "). If it has no admin account yet, create it from your home network first"
	}
	switch want := cmp.Or(s.Status, http.StatusOK); {
	case want != http.StatusOK && resp.StatusCode != want:
		return notDone // e.g. answers without asking for a login
	case resp.StatusCode != want:
		return unsure(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return name + " is not answering yet"
	}
	if !strings.Contains(string(body), s.Contains) {
		return notDone
	}
	if s.Field == "" && len(s.Unset) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return unsure("unexpected answer")
	}
	if s.Field != "" && !set(lookup(v, s.Field)) || slices.ContainsFunc(s.Unset, func(p string) bool { return set(lookup(v, p)) }) {
		return notDone
	}
	return ""
}

// lookup follows a dotted path into JSON: "data.account", "setup.0.status";
// "." is the whole answer. Missing is nil.
func lookup(v any, path string) any {
	for _, k := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			if k != "" {
				v = x[k]
			}
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			if k != "" {
				return nil
			}
		}
	}
	return v
}

// set reports whether a JSON value is true or a non-empty list or object.
// Anything else, including a missing field or "true", is not set up.
func set(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return false
}

type container struct {
	ID         string   // JSON field names match case-insensitively
	Names      []string // e.g. "/immich_server"
	Image      string
	HostConfig struct {
		NetworkMode string
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
	Ports []portMapping
}

type portMapping struct {
	PrivatePort int
	Type        string // "tcp", "udp"
}

func list(ctx context.Context, c *http.Client, rs []recipes.Recipe) (map[string]App, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cs []container
	if err := getJSON(ctx, c, "/containers/json", &cs); err != nil { // running only
		return nil, err
	}
	apps := map[string]App{}
	var unknown []container
	for _, ct := range cs {
		image, err := imageName(ctx, c, ct)
		if err != nil {
			slog.Warn("container image unknown, skipped", "container", ct.ID, "err", err) // e.g. removed meanwhile
			continue
		}
		r, ok := recipes.Find(rs, image)
		if !ok {
			ct.Image = image // the name, also for a re-pulled tag
			unknown = append(unknown, ct)
			continue
		}
		if !exposes(ct, r.UpstreamPort) {
			continue
		}
		if _, dup := apps[r.App]; dup {
			// Picking one could put a fresh, unconfigured copy on the internet.
			apps[r.App] = App{Problem: "More than one " + r.App + " container is running; stop the one you don't use"}
			continue
		}
		if ip := address(ct); ip != "" {
			apps[r.App] = App{Upstream: net.JoinHostPort(ip, strconv.Itoa(r.UpstreamPort))}
		}
	}
	self := ownIPs()
	for _, ct := range unknown {
		name, a, ok := unsupported(ct, rs, self)
		if !ok {
			continue
		}
		if _, dup := apps[name]; dup { // e.g. "my_app" and "my-app"
			a = App{Unsupported: true, Problem: "More than one container is named " + name + "; rename one"}
		}
		apps[name] = a
	}
	return apps, nil
}

// unsupported makes an app of a container no recipe knows: named after the
// container, at the one TCP port it listens on, or 80 among several.
// Skipped: names a recipe uses, so a switch left on for a recipe's app never
// routes to another one; containers without ports (host networking lists
// none, and its 127.0.0.1 could be anything); and this client's own
// container, whose local page holds the switches.
func unsupported(ct container, rs []recipes.Recipe, self map[string]bool) (string, App, bool) {
	if len(ct.Names) == 0 {
		return "", App{}, false
	}
	name := hostLabel(ct.Names[0])
	ip := address(ct)
	if name == "" || ip == "" || self[ip] || slices.ContainsFunc(rs, func(r recipes.Recipe) bool { return r.App == name }) {
		return "", App{}, false
	}
	var ports []int
	for _, p := range ct.Ports {
		if p.Type == "tcp" && !slices.Contains(ports, p.PrivatePort) {
			ports = append(ports, p.PrivatePort)
		}
	}
	slices.Sort(ports)
	var port int
	switch {
	case len(ports) == 0:
		return "", App{}, false
	case len(ports) == 1:
		port = ports[0]
	case slices.Contains(ports, 80):
		port = 80
	default:
		return name, App{Unsupported: true, Image: recipes.Repo(ct.Image), Problem: fmt.Sprintf("It listens on several ports %v; Farstu can't tell which one is its web page", ports)}, true
	}
	return name, App{Upstream: net.JoinHostPort(ip, strconv.Itoa(port)), Unsupported: true, Image: recipes.Repo(ct.Image)}, true
}

// hostLabel turns a container name into a DNS label: "/Immich_Server" is
// "immich-server". Empty if it can't be one.
func hostLabel(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		case r == '_' || r == '.' || r == '-':
			return '-'
		}
		return -1
	}, strings.TrimPrefix(name, "/"))
	return strings.Trim(name[:min(len(name), 63)], "-")
}

// ownIPs are this machine's addresses: a container at one of them is this
// client itself.
func ownIPs() map[string]bool {
	addrs, _ := net.InterfaceAddrs()
	ips := map[string]bool{}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ips[n.IP.String()] = true
		}
	}
	return ips
}

// imageName is the image the container was created from. Once its tag is
// pulled anew, the listing shows only an image ID; the container's own
// config still has the name.
func imageName(ctx context.Context, c *http.Client, ct container) (string, error) {
	if !strings.HasPrefix(ct.Image, "sha256:") {
		return ct.Image, nil
	}
	var inspect struct{ Config struct{ Image string } }
	err := getJSON(ctx, c, "/containers/"+url.PathEscape(ct.ID)+"/json", &inspect)
	return inspect.Config.Image, err
}

// exposes reports whether the container listens on port, e.g. not an
// "fpm" variant of an image whose recipe wants HTTP. Containers on host
// networking list no ports and are taken on trust.
func exposes(ct container, port int) bool {
	return len(ct.Ports) == 0 || slices.ContainsFunc(ct.Ports, func(p portMapping) bool { return p.PrivatePort == port })
}

// address is where the client reaches a container: loopback for host
// networking, else its IP. A client on host networking reaches any bridge
// IP; a client on a bridge network must share one with the app.
// ponytail: first network by name; prefer one shared with the client if apps sit on several.
func address(ct container) string {
	if ct.HostConfig.NetworkMode == "host" {
		return "127.0.0.1"
	}
	for _, n := range slices.Sorted(maps.Keys(ct.NetworkSettings.Networks)) {
		if ip := ct.NetworkSettings.Networks[n].IPAddress; ip != "" {
			return ip
		}
	}
	return ""
}

// statusError is Docker answering with something other than 200.
type statusError int

func (s statusError) Error() string { return fmt.Sprintf("Docker API: HTTP %d", int(s)) }

func get(ctx context.Context, c *http.Client, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, statusError(resp.StatusCode)
	}
	return resp.Body, nil
}

func getJSON(ctx context.Context, c *http.Client, path string, v any) error {
	body, err := get(ctx, c, path)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("Docker API %s: %w", path, err)
	}
	return nil
}

func reason(socket string, err error) error {
	var status statusError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("Can't see your apps: Docker socket %s is missing; mount it into the Farstu container: %w", socket, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("Can't see your apps: no permission to use the Docker socket %s: %w", socket, err)
	case errors.As(err, &status) && (status == http.StatusForbidden || status == http.StatusUnauthorized):
		return fmt.Errorf("Can't see your apps: Docker refused access: %w", err)
	case errors.As(err, &status):
		return fmt.Errorf("Can't see your apps: Docker answered with an error: %w", err)
	}
	return fmt.Errorf("Can't see your apps: Docker is not answering: %w", err)
}
