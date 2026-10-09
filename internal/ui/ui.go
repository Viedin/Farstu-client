// Package ui keeps which apps are turned on and serves the client's local
// page with per-app toggles and status.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/viedin/farstu-client/api"
	"github.com/viedin/farstu-client/internal/discover"
	"github.com/viedin/farstu-client/internal/proxy"
	"github.com/viedin/farstu-client/internal/recipes"
)

type App struct {
	Name     string // e.g. "immich"
	Host     string // e.g. "immich.calm-otter-k3x9qf2m.farstu.dev"
	Upstream string // e.g. "172.18.0.5:2283"
	Problem  string // why it can't be turned on, e.g. two containers match
}

// An app that is on and gone this long was uninstalled, not updated: it is
// turned off, so a later reinstall (maybe without an admin account yet)
// starts off.
const forgetAfter = 24 * time.Hour

type UI struct {
	path   string // JSON list of the names of apps that are on
	router *proxy.Router
	zone   string // e.g. "calm-otter-k3x9qf2m.farstu.dev"
	now    func() time.Time

	mu        sync.Mutex
	on        map[string]bool
	apps      []App // running now, by name
	problem   string
	goneSince map[string]time.Time // apps that are on but not running
	appLimit  int                  // apps that may be on at once, 0 = no limit; from the plan
	// ponytail: goneSince lives in memory, so a client restart restarts the
	// clock; persist it if restarts during an uninstall turn out to matter.

	// Set before serving. Supported names the apps Farstu knows, Logins
	// their sign-in paths by name; nil checks are skipped.
	Supported []string
	Logins    map[string][]recipes.Login
	Tunnel    func(context.Context) error
	Probe     func(ctx context.Context, host string) (api.ProbeResponse, error)
}

// Load reads which apps are on from path. They are routed once SetApps
// reports them running. Missing or corrupt state means everything is off.
func Load(path string, router *proxy.Router, zone string) *UI {
	u := &UI{path: path, router: router, zone: zone, now: time.Now, on: map[string]bool{}, goneSince: map[string]time.Time{}}
	var names []string
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &names)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("app state unreadable, all apps are off", "path", path, "err", err)
		names = nil
	}
	for _, n := range names {
		u.on[n] = true
	}
	return u
}

// SetApps takes the apps running now, or why they can't be listed. An app
// that is on stays on while its container is gone, so an update that
// recreates it does not turn it off, until forgetAfter.
func (u *UI) SetApps(running map[string]discover.App, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.problem = err.Error() // keep routing what we last saw
		return
	}
	u.problem = ""
	apps := []App{}
	for _, n := range slices.Sorted(maps.Keys(running)) {
		r := running[n]
		apps = append(apps, App{Name: n, Host: n + "." + u.zone, Upstream: r.Upstream, Problem: r.Problem})
	}
	now, forgot := u.now(), false
	for n := range u.on {
		a, ok := find(apps, n)
		since, gone := u.goneSince[n]
		switch {
		case gone && now.Sub(since) >= forgetAfter:
			slog.Info("app gone for a day, turning it off", "app", n)
			u.router.Disable(n + "." + u.zone)
			delete(u.on, n)
			delete(u.goneSince, n)
			forgot = true
			continue
		case !ok && !gone:
			u.goneSince[n] = now
		case ok:
			delete(u.goneSince, n)
		}
		old, _ := find(u.apps, n)
		switch {
		case !ok || a.Problem != "":
			u.router.Disable(n + "." + u.zone)
		case a.Upstream != old.Upstream || old.Problem != "": // unchanged: leave open streams alone
			u.router.Enable(a.Host, a.Upstream, u.Logins[n])
		}
	}
	u.apps = apps
	if forgot {
		if err := u.save(); err != nil {
			slog.Error("could not save apps turned off after a day gone; they are off until a restart", "err", err)
		}
	}
}

// Set turns an app on or off and saves it. Off takes effect in the router
// before saving, so it holds even if saving fails.
func (u *UI) Set(name string, on bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.on[name] == on {
		return nil // a second "Turn on" must not cut open streams
	}
	if !on {
		u.router.Disable(name + "." + u.zone)
		delete(u.on, name)
		delete(u.goneSince, name)
		if err := u.save(); err != nil {
			return fmt.Errorf("%s is off now, but could not save it and would turn back on after a restart: %w", name, err)
		}
		return nil
	}
	a, ok := find(u.apps, name)
	if !ok {
		return refusal(name + " is not running, so it can't be turned on")
	}
	if a.Problem != "" {
		return refusal(a.Problem)
	}
	// ponytail: a downgrade leaves extra apps on; only new ones are refused.
	if u.appLimit > 0 && len(u.on) >= u.appLimit {
		return refusal(fmt.Sprintf("The free plan allows %d apps at once. Turn one off first, or upgrade to turn on more", u.appLimit))
	}
	u.on[name] = true
	if err := u.save(); err != nil {
		delete(u.on, name)
		return fmt.Errorf("could not save, %s stays off: %w", name, err)
	}
	u.router.Enable(a.Host, a.Upstream, u.Logins[name])
	return nil
}

// SetAppLimit sets how many apps may be on at once; 0 means no limit.
func (u *UI) SetAppLimit(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.appLimit = n
}

// refusal is a toggle that can't be done in this state, as opposed to a
// failure on our side.
type refusal string

func (r refusal) Error() string { return string(r) }

func find(apps []App, name string) (App, bool) {
	i := slices.IndexFunc(apps, func(a App) bool { return a.Name == name })
	if i < 0 {
		return App{}, false
	}
	return apps[i], true
}

// save writes u.on atomically and durably: a half-written file would mean
// everything is off, and an off that is lost on power failure would turn
// the app back on.
func (u *UI) save() error {
	names := []string{}
	for n := range u.on {
		names = append(names, n)
	}
	slices.Sort(names)
	b, _ := json.Marshal(names)
	f, err := os.CreateTemp(filepath.Dir(u.path), ".apps-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), u.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(u.path)) // make the rename itself durable
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// status names the first thing standing between a visitor and the app, or
// how fast it answers from outside; ok means it is reachable. Empty for a
// running app that is off.
func (u *UI) status(ctx context.Context, a App, on bool, tunnel error) (msg string, ok bool) {
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", a.Upstream)
	if err != nil {
		return "App is not running", false
	}
	c.Close()
	switch {
	case !on:
		return "", false
	case tunnel != nil:
		return tunnel.Error(), false
	case u.Probe == nil:
		return "", false
	}
	p, err := u.Probe(ctx, a.Host)
	if err != nil {
		slog.Warn("external check failed", "host", a.Host, "err", err)
		return "Could not check from outside: " + err.Error(), false
	}
	if p.Error != "" {
		return p.Error, false
	}
	return fmt.Sprintf("Reachable from the internet (%d ms)", p.LatencyMS), true
}

//go:embed pages/*.html
var pageFiles embed.FS

var pages = template.Must(template.ParseFS(pageFiles, "pages/*.html"))

func (u *UI) mux() http.Handler {
	mux := http.NewServeMux()
	render := func(w http.ResponseWriter, status int, errMsg string) {
		type row struct {
			App
			On, Missing bool
		}
		u.mu.Lock()
		var rows []row
		for _, a := range u.apps {
			rows = append(rows, row{App: a, On: u.on[a.Name]})
		}
		// On but gone: listed so it can still be turned off.
		for _, n := range slices.Sorted(maps.Keys(u.on)) {
			if _, ok := find(u.apps, n); !ok {
				rows = append(rows, row{App: App{Name: n}, On: true, Missing: true})
			}
		}
		problem := u.problem
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		pages.ExecuteTemplate(w, "apps", struct {
			Apps                      []row
			Problem, Error, Supported string
		}{rows, problem, errMsg, strings.Join(u.Supported, ", ")})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, http.StatusOK, "")
	})
	// Status is fetched by the page after it loads: the outside check takes
	// up to 5s and the toggles should not wait for it.
	mux.HandleFunc("GET /apps/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		a, ok := find(u.apps, r.PathValue("name"))
		on := u.on[a.Name]
		u.mu.Unlock()
		if !ok {
			http.Error(w, "App is not running", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second) // server probes time out at 5s
		defer cancel()
		var tunnel error
		if on && u.Tunnel != nil {
			tunnel = u.Tunnel(ctx)
		}
		var s struct {
			Text string `json:"text"`
			OK   bool   `json:"ok"`
		}
		s.Text, s.OK = u.status(ctx, a, on, tunnel)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("POST /apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := u.Set(r.PathValue("name"), r.FormValue("on") == "1"); err != nil {
			status := http.StatusConflict
			if !errors.As(err, new(refusal)) {
				status = http.StatusInternalServerError
				slog.Error("toggle failed", "err", err)
			}
			render(w, status, err.Error())
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	return mux
}

func (u *UI) handler() http.Handler { return guard(u.mux()) }

// guard: a button here exposes an app to the internet, and the connect page
// links this server to an account. Refuse requests from other sites, and
// from DNS-rebound names.
func guard(h http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection().Handler(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !homeNetwork(r.RemoteAddr, onLink()) {
			http.Error(w, "Open this page from your home network", http.StatusForbidden)
			return
		}
		if !localHost(r.Host) {
			http.Error(w, "Open this page by IP address or local name", http.StatusForbidden)
			return
		}
		cop.ServeHTTP(w, r)
	})
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10") // also Tailscale

// homeNetwork reports whether a visitor comes from a private address, or
// from a global IPv6 address on one of this server's own subnets (home
// networks hand those out next to the private ones). The page listens on
// all interfaces, and a server with a public IP must not hand its switches
// to the internet.
func homeNetwork(remoteAddr string, onLink []netip.Prefix) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip) ||
		slices.ContainsFunc(onLink, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// onLink returns this host's global IPv6 subnets. Not IPv4: a server with a
// public IPv4 often shares its subnet with strangers at the provider.
func onLink() []netip.Prefix {
	addrs, _ := net.InterfaceAddrs()
	var ps []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, _ := netip.AddrFromSlice(n.IP)
		bits, _ := n.Mask.Size()
		if ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsPrivate() && bits >= 48 {
			ps = append(ps, netip.PrefixFrom(ip, bits).Masked())
		}
	}
	return ps
}

// localHost reports whether host is an IP, localhost, or a LAN name an
// outside site cannot point at us.
func localHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	return net.ParseIP(host) != nil || host == "localhost" ||
		!strings.Contains(host, ".") || strings.HasSuffix(host, ".local")
}

// Page is the local web page: how to connect this server while it
// registers, then the app toggles.
type Page struct {
	mu   sync.Mutex
	code api.DeviceCodeResponse
	ui   http.Handler
}

// ShowCode shows the registration link until Ready.
func (p *Page) ShowCode(dc api.DeviceCodeResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code = dc
}

// Ready switches the page to u's toggles. Set u's checks before.
func (p *Page) Ready(u *UI) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ui = u.mux()
}

func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	ui, code := p.ui, p.code
	p.mu.Unlock()
	if ui != nil {
		ui.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pages.ExecuteTemplate(w, "connect", code)
}

// Serve serves the page on ln until ctx is done.
func (p *Page) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: guard(p), ReadHeaderTimeout: 10 * time.Second}
	defer context.AfterFunc(ctx, func() { srv.Close() })()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("local page: %w", err)
	}
	return nil
}
