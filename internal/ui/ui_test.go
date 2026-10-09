package ui

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
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

	// Docker trouble keeps the routes and says why.
	u.SetApps(nil, errors.New("Can't see your apps: Docker is not answering"))
	if get() != 202 || !strings.Contains(page(u.handler()), "Docker is not answering") {
		t.Error("discovery error dropped routes or was not shown")
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
	u := Load(filepath.Join(t.TempDir(), "apps.json"), proxy.NewRouter(), "u.farstu.test")
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
	u.SetAppLimit(2)
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
	u.SetAppLimit(0)
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
