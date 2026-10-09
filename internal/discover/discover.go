// Package discover finds installed apps: running Docker containers that
// match a recipe. It talks to the Docker API over its socket.
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
	"net/url"
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
}

// Watch calls found with the apps running now, by name, and again whenever
// a container starts or stops and every recheck, until ctx is done. While
// Docker is unreachable it calls found with a user-facing error and retries.
//
// Upstreams are container IPs, which Docker hands out again after a
// container stops. Following start/die events keeps a stale route from
// sending one app's visitors to another for longer than one listing.
func Watch(ctx context.Context, socket string, rs []recipes.Recipe, found func(map[string]App, error)) {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 10 * time.Second, // a hung dockerd must surface as an error
	}}
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
	case resp.StatusCode == want && s.Field == "":
		return ""
	case want != http.StatusOK && resp.StatusCode != want:
		return notDone // e.g. answers without asking for a login
	case resp.StatusCode != want:
		return unsure(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	var v map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return unsure("unexpected answer")
	}
	if !set(v[s.Field]) {
		return notDone
	}
	return ""
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
	ID         string // JSON field names match case-insensitively
	Image      string
	HostConfig struct {
		NetworkMode string
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
	Ports []struct{ PrivatePort int }
}

func list(ctx context.Context, c *http.Client, rs []recipes.Recipe) (map[string]App, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cs []container
	if err := getJSON(ctx, c, "/containers/json", &cs); err != nil { // running only
		return nil, err
	}
	apps := map[string]App{}
	for _, ct := range cs {
		image, err := imageName(ctx, c, ct)
		if err != nil {
			slog.Warn("container image unknown, skipped", "container", ct.ID, "err", err) // e.g. removed meanwhile
			continue
		}
		r, ok := recipes.Find(rs, image)
		if !ok || !exposes(ct, r.UpstreamPort) {
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
	return apps, nil
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
	return len(ct.Ports) == 0 || slices.ContainsFunc(ct.Ports, func(p struct{ PrivatePort int }) bool { return p.PrivatePort == port })
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
