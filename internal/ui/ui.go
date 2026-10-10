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
	"math"
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
	// Unsupported: no recipe; the page warns Farstu can't vouch it's safe.
	Unsupported bool
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
	on        []string // names of apps that are on, in the order they were turned on
	apps      []App    // running now, by name
	problem   string
	goneSince map[string]time.Time // apps that are on but not running
	plan      api.Me               // zero until the server first answers: no app limit
	notice    string               // apps the plan turned off
	// ponytail: goneSince lives in memory, so a client restart restarts the
	// clock; persist it if restarts during an uninstall turn out to matter.

	// Set before serving. Supported names the apps Farstu knows, Logins
	// their sign-in paths by name; nil checks are skipped.
	Supported []string
	Logins    map[string][]recipes.Login
	Account   string // the account page on Farstu Server; empty hides the link
	Tunnel    func(context.Context) error
	Probe     func(ctx context.Context, host string) (api.ProbeResponse, error)
	// Restart reconnects frpc when Hosts changes. It must not block: it is
	// called with u's lock held.
	Restart func()
}

// Load reads which apps are on from path. They are routed once SetApps
// reports them running. Missing or corrupt state means everything is off.
func Load(path string, router *proxy.Router, zone string) *UI {
	u := &UI{path: path, router: router, zone: zone, now: time.Now, goneSince: map[string]time.Time{}}
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
		if !u.isOn(n) {
			u.on = append(u.on, n)
		}
	}
	return u
}

func (u *UI) isOn(name string) bool { return slices.Contains(u.on, name) }

func (u *UI) turnOff(name string) {
	u.router.Disable(name + "." + u.zone)
	u.on = slices.DeleteFunc(u.on, func(n string) bool { return n == name })
	delete(u.goneSince, name)
}

// restart reconnects frpc if its proxy lists the apps that are on.
func (u *UI) restart() {
	if u.plan.AppLimit > 0 && u.Restart != nil {
		u.Restart()
	}
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
		apps = append(apps, App{Name: n, Host: n + "." + u.zone, Upstream: r.Upstream, Problem: r.Problem, Unsupported: r.Unsupported})
	}
	now, forgot := u.now(), false
	for _, n := range slices.Clone(u.on) {
		a, ok := find(apps, n)
		since, gone := u.goneSince[n]
		switch {
		case gone && now.Sub(since) >= forgetAfter:
			slog.Info("app gone for a day, turning it off", "app", n)
			u.turnOff(n)
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
		u.restart()
	}
}

// Set turns an app on or off and saves it. Off takes effect in the router
// before saving, so it holds even if saving fails.
func (u *UI) Set(name string, on bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.isOn(name) == on {
		return nil // a second "Turn on" must not cut open streams
	}
	if !on {
		u.turnOff(name)
		u.restart()
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
	// The relay enforces this too; refusing here says why.
	if limit := u.plan.AppLimit; limit > 0 && len(u.on) >= limit {
		return refusal(fmt.Sprintf("The free plan allows %d apps at once. Turn one off first, or upgrade to turn on more", limit))
	}
	u.on = append(u.on, name)
	if err := u.save(); err != nil {
		u.on = u.on[:len(u.on)-1]
		return fmt.Errorf("could not save, %s stays off: %w", name, err)
	}
	u.router.Enable(a.Host, a.Upstream, u.Logins[name])
	u.notice = ""
	u.restart()
	return nil
}

// SetPlan applies the account's plan. Under a lower app limit the apps
// turned on last go off: the relay refuses a proxy with more.
func (u *UI) SetPlan(me api.Me) {
	u.mu.Lock()
	defer u.mu.Unlock()
	first, shape := u.plan.Plan == "", (u.plan.AppLimit == 0) != (me.AppLimit == 0)
	u.plan = me
	if limit := me.AppLimit; limit > 0 && len(u.on) > limit {
		off := slices.Clone(u.on[limit:])
		for _, n := range off {
			u.turnOff(n)
		}
		slog.Info("over the plan's app limit, turned apps off", "apps", off, "limit", limit)
		u.notice = fmt.Sprintf("The free plan allows %d apps at once, so %s turned off. Upgrade to turn on more.", limit, strings.Join(off, " and "))
		if err := u.save(); err != nil {
			slog.Error("could not save apps turned off by the plan; the plan turns them off again after a restart", "err", err)
		}
	} else if !shape {
		return
	}
	// The first answer comes before frpc starts (see Hosts).
	if !first && u.Restart != nil {
		u.Restart() // also from free to paid: a new proxy, without the speed cap
	}
}

// Hosts are what frpc's proxy serves: nil for the whole zone (no app limit,
// api.ProxyAll), else the hosts of the apps that are on (api.ProxyApps).
func (u *UI) Hosts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.plan.AppLimit == 0 {
		return nil
	}
	hosts := []string{}
	for _, n := range u.on {
		hosts = append(hosts, n+"."+u.zone)
	}
	return hosts
}

// planLine sums up the plan for the page; empty until the server answers.
func (u *UI) planLine() string {
	switch p := u.plan; {
	case p.Plan == "":
		return ""
	case p.AppLimit > 0:
		return fmt.Sprintf("Free plan: %d apps on at once, speed capped.", p.AppLimit)
	case p.PaidUntil.IsZero():
		return "Paid plan."
	}
	days := int(math.Ceil(u.plan.PaidUntil.Sub(u.now()).Hours() / 24)) // 1 on the last day
	if days == 1 {
		return "Paid plan: 1 day left."
	}
	return fmt.Sprintf("Paid plan: %d days left.", max(days, 0))
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
// the app back on. The order is kept: a lower app limit keeps the first.
func (u *UI) save() error {
	b, _ := json.Marshal(append([]string{}, u.on...)) // [] when none, not null
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

//go:embed icon.svg
var icon []byte

var pages = template.Must(template.ParseFS(pageFiles, "pages/*.html"))

func (u *UI) mux() http.Handler {
	mux := http.NewServeMux()
	render := func(w http.ResponseWriter, status int, errMsg string) {
		type row struct {
			App
			On, Missing bool
			Unchecked   bool // missing while Docker can't be asked
		}
		u.mu.Lock()
		// Apps: supported or on. Others: unsupported and off, folded away
		// so a server's databases and helpers don't bury the apps.
		var rows, others []row
		for _, a := range u.apps {
			if r := (row{App: a, On: u.isOn(a.Name)}); a.Unsupported && !r.On {
				others = append(others, r)
			} else {
				rows = append(rows, r)
			}
		}
		// On but gone: listed so it can still be turned off.
		for _, n := range slices.Sorted(slices.Values(u.on)) {
			if _, ok := find(u.apps, n); !ok {
				rows = append(rows, row{App: App{Name: n}, On: true, Missing: true, Unchecked: u.problem != ""})
			}
		}
		problem, plan, notice := u.problem, u.planLine(), u.notice
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		pages.ExecuteTemplate(w, "apps", struct {
			Apps, Others                       []row
			Problem, Error, Supported, Account string
			Incidents                          []incidentRow
			Plan, Notice                       string
		}{rows, others, problem, errMsg, strings.Join(u.Supported, ", "), u.Account, u.incidents(), plan, notice})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, http.StatusOK, "")
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(icon)
	})
	// Status is fetched by the page after it loads: the outside check takes
	// up to 5s and the toggles should not wait for it.
	mux.HandleFunc("GET /apps/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		a, ok := find(u.apps, r.PathValue("name"))
		on := u.isOn(a.Name)
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

type incidentRow struct {
	Name, Visitor, Ago string // Name "" for names not turned on
	SignIn             bool
	Count              int
}

// incidents are the router's newest 20, by app name.
func (u *UI) incidents() []incidentRow {
	all := u.router.Incidents()
	var rows []incidentRow
	for _, in := range all[:min(len(all), 20)] {
		rows = append(rows, incidentRow{strings.TrimSuffix(in.Host, "."+u.zone), in.Visitor, ago(u.now().Sub(in.Last)), in.SignIn, in.Count})
	}
	return rows
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h ago", int(d.Hours()))
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
	Disconnected bool // set before serving: the previous device was disconnected from its account

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
	pages.ExecuteTemplate(w, "connect", struct {
		api.DeviceCodeResponse
		Disconnected bool
	}{code, p.Disconnected})
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
