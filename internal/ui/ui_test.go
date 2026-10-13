package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/viedin/farstu-client/api"
	"github.com/viedin/farstu-client/internal/discover"
	"github.com/viedin/farstu-client/internal/proxy"
)

func TestToggleSurvivesRestart(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	running := map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String()}}
	path := filepath.Join(t.TempDir(), "apps.json")

	// start loads the state like a fresh client and reports whether immich answers.
	start := func() (*UI, func() int) {
		rt := proxy.NewRouter()
		u := Load(path, rt, "u.farstu.test")
		u.SetApps(running, nil)
		return u, func() int {
			req := httptest.NewRequest("GET", "/", nil)
			req.Host = "immich.u.farstu.test"
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, req)
			return rec.Code
		}
	}
	post := func(u *UI, host, origin, on string) int {
		req := httptest.NewRequest("POST", "/apps/immich", strings.NewReader(url.Values{"on": {on}}.Encode()))
		req.RemoteAddr = "192.168.1.10:51000"
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		u.handler().ServeHTTP(rec, req)
		return rec.Code
	}

	u, get := start()
	if body := page(u.handler()); !strings.Contains(body, `aria-checked="false" aria-label="immich"`) {
		t.Errorf("page: %s", body)
	}
	if get() != 404 {
		t.Fatal("app on without state, want default off")
	}
	if code := post(u, "evil.example:7080", "", "1"); code != 403 || get() != 404 {
		t.Errorf("DNS-rebound host: %d, app %d", code, get())
	}
	if code := post(u, "127.0.0.1:7080", "https://evil.example", "1"); code != 403 || get() != 404 {
		t.Errorf("cross-origin POST: %d, app %d", code, get())
	}
	if code := post(u, "CasaOS.LOCAL.:7080", "", "1"); code != 303 || get() != 200 {
		t.Fatalf("turn on: %d, app %d", code, get())
	}

	u, get = start()
	if get() != 200 {
		t.Fatal("on did not survive restart")
	}
	if code := post(u, "192.168.1.5:7080", "", "0"); code != 303 || get() != 404 {
		t.Fatalf("turn off: %d, app %d", code, get())
	}
	if _, get = start(); get() != 404 {
		t.Fatal("off did not survive restart")
	}

	u.Set("immich", true)
	os.WriteFile(path, []byte(`["immi`), 0o600)
	if _, get = start(); get() != 404 {
		t.Error("corrupt state, want everything off")
	}
	if err := u.Set("nextcloud", true); err == nil {
		t.Error("turned on an app that is not running")
	}
}

func TestAppComesAndGoes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	defer moved.Close()
	path := filepath.Join(t.TempDir(), "apps.json")
	os.WriteFile(path, []byte(`["immich"]`), 0o600)
	rt := proxy.NewRouter()
	get := func() int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "immich.u.farstu.test"
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code
	}

	u := Load(path, rt, "u.farstu.test")
	clock := time.Now()
	u.now = func() time.Time { return clock }
	if get() != 404 {
		t.Fatal("routed before discovery")
	}
	u.SetApps(map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String()}}, nil)
	if get() != 200 {
		t.Fatal("on app not routed once running")
	}

	// Recreated by an update: gone, then back at a new address, still on.
	u.SetApps(map[string]discover.App{}, nil)
	if get() != 404 {
		t.Fatal("gone app still routed")
	}
	if body := page(u.handler()); !strings.Contains(body, "Not running") || !strings.Contains(body, `aria-checked="true" aria-label="immich"`) {
		t.Errorf("gone app not shown as on and not running: %s", body)
	}
	u.SetApps(map[string]discover.App{"immich": {Upstream: moved.Listener.Addr().String()}}, nil)
	if get() != 202 {
		t.Fatal("not routed to the new address")
	}

	// Docker trouble keeps the routes for a while and says why.
	blind := errors.New("Can't see your apps: Docker is not answering")
	u.SetApps(nil, blind)
	clock = clock.Add(blindFor - time.Second)
	u.SetApps(nil, blind)
	if get() != 202 || !strings.Contains(page(u.handler()), "Docker is not answering") {
		t.Error("discovery error dropped routes or was not shown")
	}
	// Longer, and the address may be another container's by now: not
	// routed, but still on once seen again.
	clock = clock.Add(time.Second)
	u.SetApps(nil, blind)
	if get() != 404 || !strings.Contains(page(u.handler()), `aria-checked="true" aria-label="immich"`) {
		t.Fatal("still routed long after discovery failed, or turned off")
	}
	u.SetApps(map[string]discover.App{"immich": {Upstream: moved.Listener.Addr().String()}}, nil)
	if get() != 202 {
		t.Fatal("not routed again once seen")
	}
	// A new failure gets the full grace again.
	clock = clock.Add(time.Hour)
	u.SetApps(nil, blind)
	if get() != 202 {
		t.Fatal("grace not reset after discovery worked")
	}

	// Turned off while gone: stays off when it comes back.
	u.SetApps(map[string]discover.App{}, nil)
	if err := u.Set("immich", false); err != nil {
		t.Fatal(err)
	}
	u.SetApps(map[string]discover.App{"immich": {Upstream: moved.Listener.Addr().String()}}, nil)
	if get() != 404 {
		t.Fatal("app turned off while gone came back on")
	}
}

func TestForgetAfterADay(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	immich := map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String()}}
	path := filepath.Join(t.TempDir(), "apps.json")
	os.WriteFile(path, []byte(`["immich"]`), 0o600)
	rt := proxy.NewRouter()
	u := Load(path, rt, "u.farstu.test")
	clock := time.Now()
	u.now = func() time.Time { return clock }
	routed := func() bool {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "immich.u.farstu.test"
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code == 200
	}

	// Docker trouble at startup says so instead of claiming the app is down.
	u.SetApps(nil, errors.New("Can't see your apps: Docker is not answering"))
	if body := page(u.handler()); !strings.Contains(body, "Can't check whether it's running") || strings.Contains(body, "Not running") {
		t.Errorf("unknown state: %s", body)
	}

	// Gone for almost a day, as during a long update: still on.
	u.SetApps(map[string]discover.App{}, nil)
	clock = clock.Add(forgetAfter - time.Minute)
	u.SetApps(immich, nil)
	if !routed() {
		t.Fatal("app back within a day is not on")
	}

	// Uninstalled, then reinstalled more than a day later: starts off.
	u.SetApps(map[string]discover.App{}, nil)
	clock = clock.Add(forgetAfter)
	u.SetApps(immich, nil)
	if routed() {
		t.Fatal("reinstalled app came back on")
	}
	if b, _ := os.ReadFile(path); string(b) != `[]` {
		t.Errorf("forgotten app still saved: %s", b)
	}

	// Two matching containers: neither is routed, and it can't be turned on.
	u.SetApps(map[string]discover.App{"immich": {Problem: "More than one immich container is running"}}, nil)
	if err := u.Set("immich", true); err == nil || routed() {
		t.Fatalf("turned on an ambiguous app: %v", err)
	}
	if body := page(u.handler()); !strings.Contains(body, "More than one immich container") {
		t.Errorf("ambiguity not shown: %s", body)
	}
}

// page returns the toggle page's HTML.
func page(h http.Handler) string {
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "localhost:7080"
	req.RemoteAddr = "127.0.0.1:51000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closed.Close()
	running := App{Name: "nextcloud", Host: "nextcloud.u.farstu.test", Upstream: up.Listener.Addr().String()}
	stopped := App{Name: "immich", Host: "immich.u.farstu.test", Upstream: closed.Addr().String()}

	var probed string
	probe := api.ProbeResponse{LatencyMS: 42}
	var probeErr error
	u := &UI{Probe: func(_ context.Context, host string) (api.ProbeResponse, error) {
		probed = host
		return probe, probeErr
	}}
	tunnelDown := errors.New("Tunnel is down: not connected to the relay")
	ctx := context.Background()

	for _, tt := range []struct {
		name   string
		app    App
		on     bool
		tunnel error
		probe  api.ProbeResponse
		err    error
		want   string
	}{
		{"off and running", running, false, tunnelDown, probe, nil, ""},
		{"off and stopped", stopped, false, nil, probe, nil, "App is not running"},
		{"stopped beats tunnel", stopped, true, tunnelDown, probe, nil, "App is not running"},
		{"tunnel beats probe", running, true, tunnelDown, probe, nil, "Tunnel is down: not connected to the relay"},
		{"reachable", running, true, nil, probe, nil, "Reachable from the internet (42 ms)"},
		{"probe finds a problem", running, true, nil, api.ProbeResponse{Error: "Certificate problem: expired"}, nil, "Certificate problem: expired"},
		{"server unreachable", running, true, nil, probe, errors.New("Farstu Server unreachable"), "Could not check from outside: Farstu Server unreachable"},
	} {
		probe, probeErr, probed = tt.probe, tt.err, ""
		got, ok := u.status(ctx, tt.app, tt.on, tt.tunnel)
		if got != tt.want || ok != strings.HasPrefix(tt.want, "Reachable") {
			t.Errorf("%s: %q ok=%v, want %q", tt.name, got, ok, tt.want)
		}
		if tt.want == "Reachable from the internet (42 ms)" && probed != running.Host {
			t.Errorf("%s: probed %q", tt.name, probed)
		}
	}
}

func TestPage(t *testing.T) {
	var p Page
	remote := "[fd00::5]:51000"
	get := func(host, path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		guard(&p).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	if _, body := get("localhost:7080", "/"); !strings.Contains(body, "Contacting Farstu") {
		t.Errorf("before a code: %s", body)
	}
	framing := httptest.NewRecorder()
	guard(&p).ServeHTTP(framing, httptest.NewRequest("GET", "http://localhost:7080/", nil))
	if framing.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("page can be framed: %v", framing.Header())
	}
	if _, body := get("localhost:7080", "/"); strings.Contains(body, "disconnected") {
		t.Errorf("disconnected on first run: %s", body)
	}
	p.Disconnected = true
	if _, body := get("localhost:7080", "/"); !strings.Contains(body, "This server was disconnected from its Farstu account") {
		t.Errorf("after a disconnect: %s", body)
	}
	p.ShowCode(api.DeviceCodeResponse{UserCode: "ABCD-EFGH", VerificationURIComplete: "https://api.farstu.test/device?code=ABCD-EFGH"})
	if _, body := get("localhost:7080", "/"); !strings.Contains(body, `href="https://api.farstu.test/device?code=ABCD-EFGH"`) || !strings.Contains(body, "ABCD-EFGH</span>") {
		t.Errorf("connect page: %s", body)
	}
	if code, body := get("evil.example:7080", "/"); code != 403 || strings.Contains(body, "ABCD") {
		t.Errorf("DNS-rebound host read the code: %d %s", code, body)
	}
	for _, remote = range []string{"203.0.113.7:51000", "[2001:db8::1]:51000", "[::ffff:8.8.8.8]:51000"} {
		if code, body := get("203.0.113.1:7080", "/"); code != 403 || strings.Contains(body, "ABCD") {
			t.Errorf("visitor from the internet (%s) read the code: %d %s", remote, code, body)
		}
	}
	for _, remote = range []string{"10.0.0.2:1", "172.17.0.1:1", "100.101.102.103:1", "[::1]:1", "[::ffff:192.168.1.4]:1"} {
		if code, _ := get("192.168.1.5:7080", "/"); code != 200 {
			t.Errorf("visitor from home (%s): %d", remote, code)
		}
	}
	remote = "[fd00::5]:51000"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	rt := proxy.NewRouter()
	u := Load(filepath.Join(t.TempDir(), "apps.json"), rt, "u.farstu.test")
	u.SetApps(map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String()}}, nil)
	u.Account = "https://api.farstu.test/account"
	u.Probe = func(context.Context, string) (api.ProbeResponse, error) { return api.ProbeResponse{LatencyMS: 7}, nil }
	p.Ready(u)
	if _, body := get("localhost:7080", "/"); !strings.Contains(body, `aria-label="immich"`) || !strings.Contains(body, `href="https://api.farstu.test/account"`) {
		t.Errorf("toggles: %s", body)
	}
	if _, body := get("localhost:7080", "/apps/immich/status"); body != `{"text":"","ok":false}`+"\n" {
		t.Errorf("status while off: %s", body)
	}
	u.Set("immich", true)
	if _, body := get("localhost:7080", "/apps/immich/status"); body != `{"text":"Reachable from the internet (7 ms)","ok":true}`+"\n" {
		t.Errorf("status while on: %s", body)
	}
	if code, _ := get("localhost:7080", "/apps/nope/status"); code != 404 {
		t.Errorf("unknown app status: %d", code)
	}

	// Unsupported apps that are off are folded away; turned on, they join the apps.
	u.SetApps(map[string]discover.App{
		"immich": {Upstream: up.Listener.Addr().String()},
		"wiki":   {Upstream: up.Listener.Addr().String(), Unsupported: true},
	}, nil)
	if _, body := get("localhost:7080", "/"); !strings.Contains(body, "Other containers (1)") || strings.Index(body, `aria-label="wiki"`) < strings.Index(body, "<details>") {
		t.Errorf("unsupported app not folded away: %s", body)
	}
	u.Set("wiki", true)
	if _, body := get("localhost:7080", "/"); strings.Contains(body, "<details>") || !strings.Contains(body, `Unsupported</span>`) {
		t.Errorf("unsupported app that is on: %s", body)
	}

	// Nothing to see: no summary. A scan: summed up today, listed on the
	// activity page.
	if _, body := get("localhost:7080", "/"); strings.Contains(body, "Today:") || !strings.Contains(body, `href="/activity"`) {
		t.Errorf("activity without any: %s", body)
	}
	if _, body := get("localhost:7080", "/activity"); !strings.Contains(body, "Nothing in the last 30 days") {
		t.Errorf("activity page without any: %s", body)
	}
	for range 20 {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "admin.u.farstu.test"
		req.RemoteAddr = "192.0.2.1:1"
		rt.ServeHTTP(httptest.NewRecorder(), req)
	}
	if _, body := get("localhost:7080", "/"); !strings.Contains(body, `<a class="card" href="/activity">Today: 1 visitor scanning →</a>`) ||
		!strings.Contains(body, "</script>") { // rendered to the end
		t.Errorf("today: %s", body)
	}
	if _, body := get("localhost:7080", "/activity"); !strings.Contains(body, "20 requests for names that aren&#39;t turned on from 192.0.2.1") ||
		!strings.Contains(body, `class="bar" style="width:100%"`) {
		t.Errorf("activity page: %s", body)
	}
}

// Guardrail: guessing or scanning a little every day adds up over 30 days;
// a phone missing a few thumbnails doesn't.
func TestActivityAddsUp(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	u := Load(filepath.Join(t.TempDir(), "apps.json"), proxy.NewRouter(), "u.farstu.test")
	u.now = func() time.Time { return now }
	var all []proxy.Activity
	for d := range 5 {
		day := time.Date(2026, 10, 10-d, 0, 0, 0, 0, time.UTC)
		last := day.Add(time.Hour)
		all = append(all,
			proxy.Activity{Day: day, Host: "immich.u.farstu.test", Visitor: "203.0.113.7", Failed: 1, Last: last},
			proxy.Activity{Day: day, Host: "immich.u.farstu.test", Visitor: "198.51.100.1", NotFound: 3, Last: last},
			proxy.Activity{Day: day, Host: "immich.u.farstu.test", Visitor: "198.51.100.2", NotFound: 4, Last: last},
		)
	}
	var got []string
	for _, v := range u.visitors(all) {
		got = append(got, fmt.Sprint(v.Name, ": ", v.What, " over ", v.Days, " days from ", v.Visitor, ", ", v.Ago, " ", v.SignIn))
	}
	want := []string{
		"immich: 5 failed sign-ins over 5 days from 203.0.113.7, 11 h ago true",
		"immich: 20 requests for pages that don't exist over 5 days from 198.51.100.2, 11 h ago false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("visitors:\n%q, want\n%q", got, want)
	}
	if d := u.days(all); len(d) != 5 || d[0].Date != "Sat 10 Oct" || !d[0].SignIn || len(d[0].Visitors) != 3 ||
		fmt.Sprint(d[0].Visitors[0]) != "{immich 203.0.113.7 1 failed sign-in 01:00  true}" ||
		d[0].Visitors[1].Visitor != "198.51.100.2" || d[0].Visitors[1].What != "4 requests for pages that don't exist" {
		t.Errorf("days: %+v", d)
	}
	if msg, bad := u.today(all); msg != "Today: 1 sign-in failed or blocked" || !bad {
		t.Errorf("today: %q %v", msg, bad)
	}
	// One visitor scanning two names is one visitor.
	today := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	scan := []proxy.Activity{
		{Day: today, Host: "immich.u.farstu.test", Visitor: "192.0.2.9", NotFound: 20, Last: now},
		{Day: today, Visitor: "192.0.2.9", NotFound: 20, Last: now},
	}
	if msg, bad := u.today(scan); msg != "Today: 1 visitor scanning" || bad {
		t.Errorf("today: %q %v", msg, bad)
	}
	for d, want := range map[time.Duration]string{30 * time.Hour: "1 day ago", 20 * 24 * time.Hour: "20 days ago", 5 * time.Hour: "5 h ago"} {
		if got := ago(d); got != want {
			t.Errorf("ago(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestHomeNetwork(t *testing.T) {
	home := []netip.Prefix{netip.MustParsePrefix("2a01:4f8:1:2::/64")}
	for remote, want := range map[string]bool{
		"192.168.1.4:1":          true,
		"[2a01:4f8:1:2::99]:1":   true, // phone on the home LAN, global IPv6
		"[2a01:4f8:1:3::99]:1":   false,
		"[2001:db8::1]:1":        false,
		"203.0.113.7:1":          false,
		"not an address":         false,
		"[::ffff:192.168.1.4]:1": true,
		"[::ffff:8.8.8.8]:1":     false,
		"100.101.102.103:1":      true,
	} {
		if got := homeNetwork(remote, home); got != want {
			t.Errorf("%s: %v, want %v", remote, got, want)
		}
	}
}

func TestAppLimit(t *testing.T) {
	u := Load(filepath.Join(t.TempDir(), "apps.json"), proxy.NewRouter(), "u.farstu.test")
	u.SetApps(map[string]discover.App{"a": {Upstream: "127.0.0.1:1"}, "b": {Upstream: "127.0.0.1:2"}, "c": {Upstream: "127.0.0.1:3"}}, nil)
	u.SetPlan(api.Me{Plan: "free", AppLimit: 2})
	for _, n := range []string{"a", "b"} {
		if err := u.Set(n, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.Set("c", true); err == nil || !strings.Contains(err.Error(), "free plan allows 2 apps") {
		t.Fatalf("third app: %v", err)
	}
	// Through the page: a refusal, not a failure on our side.
	req := httptest.NewRequest("POST", "/apps/c", strings.NewReader("on=1"))
	req.RemoteAddr = "192.168.1.10:51000"
	req.Host = "casaos.local:7080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	u.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "free plan allows 2 apps") {
		t.Fatalf("third app via page: %d", rec.Code)
	}
	if err := u.Set("a", true); err != nil {
		t.Fatalf("turning on an app that is on: %v", err)
	}
	u.Set("a", false)
	if err := u.Set("c", true); err != nil {
		t.Fatalf("after turning one off: %v", err)
	}
	u.SetPlan(api.Me{Plan: "paid"})
	if err := u.Set("a", true); err != nil {
		t.Fatalf("no limit: %v", err)
	}
}

// An app without an admin account makes its first visitor admin: it can't
// be turned on, and one that is on (reinstalled within a day) stops being
// routed until it has one.
func TestNotSetUp(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	fresh := map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String(), Problem: "Immich has no admin account yet"}}
	setUp := map[string]discover.App{"immich": {Upstream: up.Listener.Addr().String()}}
	rt := proxy.NewRouter()
	routed := func() bool {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "immich.u.farstu.test"
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code == 200
	}
	u := Load(filepath.Join(t.TempDir(), "apps.json"), rt, "u.farstu.test")

	u.SetApps(fresh, nil)
	if err := u.Set("immich", true); err == nil || !strings.Contains(err.Error(), "no admin account") || routed() {
		t.Fatalf("turned on a fresh install: %v", err)
	}
	u.SetApps(setUp, nil)
	if err := u.Set("immich", true); err != nil || !routed() {
		t.Fatalf("set up: %v", err)
	}
	u.SetApps(fresh, nil) // reinstalled at the same address
	if routed() {
		t.Fatal("fresh reinstall still routed")
	}
	u.SetApps(setUp, nil)
	if !routed() {
		t.Fatal("not routed again once set up")
	}
}

// When paid time ends, the apps turned on last go off, so the relay accepts
// the proxy for the rest; frpc reconnects whenever its proxy changes.
func TestPlanDowngrade(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	path := filepath.Join(t.TempDir(), "apps.json")
	rt := proxy.NewRouter()
	u := Load(path, rt, "u.farstu.test")
	restarts := 0
	u.Restart = func() { restarts++ }
	running := map[string]discover.App{}
	for _, n := range []string{"a", "b", "c"} {
		running[n] = discover.App{Upstream: up.Listener.Addr().String()}
	}
	u.SetApps(running, nil)
	routed := func(name string) bool {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = name + ".u.farstu.test"
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code != 404
	}

	paidUntil := time.Now().Add(36 * time.Hour)
	u.SetPlan(api.Me{Plan: "paid", PaidUntil: paidUntil})
	if u.Hosts() != nil || restarts != 0 {
		t.Fatalf("paid: hosts %v, %d restarts; want the wildcard, no restart before frpc runs", u.Hosts(), restarts)
	}
	if body := page(u.handler()); !strings.Contains(body, "Paid plan: 2 days left.") {
		t.Errorf("page: %s", body)
	}
	for _, n := range []string{"c", "a", "b"} { // turn-on order, not alphabetical
		if err := u.Set(n, true); err != nil {
			t.Fatal(err)
		}
	}
	if restarts != 0 {
		t.Fatalf("paid toggles restarted frpc %d times", restarts)
	}

	u.SetPlan(api.Me{Plan: "free", AppLimit: 2})
	if !routed("c") || !routed("a") || routed("b") {
		t.Fatalf("routed c %v, a %v, b %v; want the first two turned on", routed("c"), routed("a"), routed("b"))
	}
	if got := u.Hosts(); !slices.Equal(got, []string{"c.u.farstu.test", "a.u.farstu.test"}) {
		t.Fatalf("hosts %v", got)
	}
	if restarts != 1 {
		t.Fatalf("%d restarts after the downgrade, want 1", restarts)
	}
	if body := page(u.handler()); !strings.Contains(body, "so b turned off") || !strings.Contains(body, "Free plan: 2 apps") {
		t.Errorf("page: %s", body)
	}
	if b, _ := os.ReadFile(path); string(b) != `["c","a"]` {
		t.Fatalf("saved %s", b)
	}

	// Free toggles change the proxy's hosts.
	u.Set("c", false)
	if got := u.Hosts(); !slices.Equal(got, []string{"a.u.farstu.test"}) || restarts != 2 {
		t.Fatalf("after off: hosts %v, %d restarts", got, restarts)
	}
	u.Set("c", false) // already off
	if restarts != 2 {
		t.Fatalf("turning off an app that is off restarted frpc")
	}
	u.SetPlan(api.Me{Plan: "free", AppLimit: 2}) // the next minute's answer
	if restarts != 2 {
		t.Fatalf("an unchanged plan restarted frpc")
	}

	// A restart keeps the order; with no apps the proxy serves nothing.
	u = Load(path, rt, "u.farstu.test")
	u.SetPlan(api.Me{Plan: "free", AppLimit: 2})
	u.Set("a", false)
	if got := u.Hosts(); got == nil || len(got) != 0 {
		t.Fatalf("free with no apps: %#v, want empty, not the wildcard", got)
	}
}
