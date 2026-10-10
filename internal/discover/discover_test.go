package discover

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/viedin/farstu-client/internal/recipes"
	recipefs "github.com/viedin/farstu-client/recipes"
)

type result struct {
	apps map[string]App
	err  error
}

// fakeDocker serves h on a unix socket and returns its path.
func fakeDocker(t *testing.T, h http.Handler) string {
	dir, _ := os.MkdirTemp("", "dk") // unix socket paths are short on macOS
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func watchResults(t *testing.T, sock string) <-chan result {
	rs, err := recipes.Load(recipefs.FS)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan result, 10)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Watch(ctx, sock, rs, func(apps map[string]App, err error) { got <- result{apps, err} })
	return got
}

func TestWatch(t *testing.T) {
	fakeApps(t, `{"isInitialized":true,"installed":true}`)
	immich := `{"Image":"ghcr.io/immich-app/immich-server:release","HostConfig":{"NetworkMode":"immich_default"},
		"NetworkSettings":{"Networks":{"immich_default":{"IPAddress":"172.18.0.5"},"a_unused":{"IPAddress":""}}},
		"Ports":[{"PrivatePort":2283}]}`
	others := `{"Image":"nextcloud:apache","HostConfig":{"NetworkMode":"host"},"NetworkSettings":{"Networks":{"host":{"IPAddress":""}}}},
		{"Image":"nextcloud:fpm","HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.3"}}},"Ports":[{"PrivatePort":9000}]},
		{"Image":"postgres:16","HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}`
	var mu sync.Mutex
	running := "[" + immich + "," + others + "]"
	events := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprint(w, running)
	})
	mux.HandleFunc("GET /containers/repulled/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Config":{"Image":"ghcr.io/immich-app/immich-server:release"}}`)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-events:
				fmt.Fprintln(w, `{"Type":"container","Action":"die"}`)
				w.(http.Flusher).Flush()
			}
		}
	})
	got := watchResults(t, fakeDocker(t, mux))
	next := func(want map[string]App, why string) {
		t.Helper()
		if r := <-got; r.err != nil || !maps.Equal(r.apps, want) {
			t.Fatalf("%s: %v %v", why, r.apps, r.err)
		}
	}

	// The fpm variant doesn't listen on the recipe's port and is skipped.
	next(map[string]App{"immich": {Upstream: "172.18.0.5:2283"}, "nextcloud": {Upstream: "127.0.0.1:80"}}, "first listing")

	set := func(s string) {
		mu.Lock()
		running = s
		mu.Unlock()
		events <- struct{}{}
	}
	set("[" + others + "]")
	next(map[string]App{"nextcloud": {Upstream: "127.0.0.1:80"}}, "after immich died")

	// Its tag pulled anew: the listing shows an image ID.
	set(`[{"Id":"repulled","Image":"sha256:0123","NetworkSettings":{"Networks":{"x":{"IPAddress":"172.18.0.9"}}}}]`)
	next(map[string]App{"immich": {Upstream: "172.18.0.9:2283"}}, "re-pulled image")

	// A second copy: neither is used.
	set("[" + immich + "," + immich + "]")
	if r := <-got; r.apps["immich"].Upstream != "" || !strings.Contains(r.apps["immich"].Problem, "More than one immich") {
		t.Fatalf("two immich containers: %v", r.apps)
	}
}

func TestWatchErrors(t *testing.T) {
	r := <-watchResults(t, filepath.Join(t.TempDir(), "nope.sock"))
	if r.err == nil || !strings.Contains(r.err.Error(), "is missing") {
		t.Errorf("missing socket: %v", r.err)
	}

	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	r = <-watchResults(t, fakeDocker(t, refuse))
	if r.err == nil || !strings.Contains(r.err.Error(), "Docker refused access") {
		t.Errorf("HTTP 403: %v", r.err)
	}
}

// fakeApps answers every app's setup check with body, whatever its address.
func fakeApps(t *testing.T, body string) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	t.Cleanup(app.Close)
	old := appClient
	t.Cleanup(func() { appClient = old })
	appClient = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", app.Listener.Addr().String())
		},
	}, CheckRedirect: old.CheckRedirect}
}

// A fresh install makes its first visitor admin: it must not be turned on.
func TestSetupProblem(t *testing.T) {
	rs, _ := recipes.Load(recipefs.FS)
	immich, _ := recipes.Find(rs, "ghcr.io/immich-app/immich-server")
	var status int
	var body string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/server/config" {
			http.NotFound(w, r)
			return
		}
		if status == http.StatusFound {
			http.Redirect(w, r, "/auth/register", status)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer app.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closed.Close()

	for _, tt := range []struct {
		status   int
		body     string
		upstream string
		want     string
	}{
		{200, `{"isInitialized": true, "isOnboarded": false}`, "", ""},
		{200, `{"isInitialized":false}`, "", "Immich has no admin account yet"},
		{200, `{}`, "", "Immich has no admin account yet"},
		{200, `{"isInitialized":"true"}`, "", "Immich has no admin account yet"},
		{200, `<html>`, "", "Can't check whether Immich is set up (unexpected answer)"},
		{500, `{"isInitialized":true}`, "", "Can't check whether Immich is set up (HTTP 500). If it has no admin account yet, create it from your home network first"},
		{302, ``, "", "Can't check whether Immich is set up (HTTP 302)"},
		{200, `{"isInitialized":true}`, closed.Addr().String(), "Immich is not answering yet"},
	} {
		status, body = tt.status, tt.body
		up := cmp.Or(tt.upstream, app.Listener.Addr().String())
		if got := setupProblem(context.Background(), up, immich); !strings.HasPrefix(got, tt.want) || (tt.want == "") != (got == "") {
			t.Errorf("%d %s %s: %q, want %q", tt.status, tt.body, tt.upstream, got, tt.want)
		}
	}
}

func TestWatchNotSetUp(t *testing.T) {
	fakeApps(t, `{"isInitialized":false}`)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"Image":"ghcr.io/immich-app/immich-server:release","NetworkSettings":{"Networks":{"x":{"IPAddress":"172.18.0.5"}}}}]`)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	r := <-watchResults(t, fakeDocker(t, mux))
	if a := r.apps["immich"]; a.Upstream != "172.18.0.5:2283" || !strings.HasPrefix(a.Problem, "Immich has no admin account yet") {
		t.Fatalf("fresh immich: %+v %v", a, r.err)
	}
}

// Each shipped recipe's check, against answers like the real app's: set up
// or not, and fail closed on anything else.
func TestSetupChecks(t *testing.T) {
	rs, err := recipes.Load(recipefs.FS)
	if err != nil {
		t.Fatal(err)
	}
	var want string // "METHOD path" the recipe must ask
	var status int
	var body string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost" || r.Header.Get("X-Forwarded-Proto") != "https" {
			http.Error(w, `{"error": "Trusted domain error.", "code": 15}`, http.StatusBadRequest) // as Nextcloud
			return
		}
		if r.Method+" "+r.URL.RequestURI() != want {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer app.Close()

	for _, tt := range []struct {
		app, ask string
		status   int
		body     string
		done     bool
	}{
		{"nextcloud", "GET /status.php", 200, `{"installed":true,"maintenance":false}`, true},
		{"nextcloud", "GET /status.php", 200, `{"installed":false}`, false},
		{"memos", "GET /api/v1/instance/profile", 200, `{"version":"0.28.0","admin":{"name":"users/1"}}`, true},
		{"memos", "GET /api/v1/instance/profile", 200, `{"version":"0.28.0","admin":null}`, false},
		{"memos", "GET /api/v1/instance/profile", 200, `{"needsSetup":false}`, false},
		{"docmost", "POST /api/workspace/public", 200, `{"data":{"id":"x","name":"Home"},"success":true,"status":200}`, true},
		{"docmost", "POST /api/workspace/public", 404, `{"message":"Workspace not found"}`, false},
		{"gitea", "GET /api/v1/users/search?limit=1", 200, `{"ok":true,"data":[{"id":1,"login":"me"}]}`, true},
		{"gitea", "GET /api/v1/users/search?limit=1", 200, `{"ok":true,"data":[]}`, false},
		{"gitea", "GET /api/v1/users/search?limit=1", 401, `{"message":"you must be signed in"}`, false},
		{"adguard", "GET /control/status", 401, ``, true},
		{"adguard", "GET /control/status", 200, `{"running":true}`, false},
		{"adguard", "GET /control/status", 302, ``, false},
		{"rsshub", "GET /healthz", 403, `Authentication failed. Access denied.`, true},
		{"rsshub", "GET /healthz", 200, `ok`, false},
		{"rsshub", "GET /nope", 403, ``, false}, // a 404 is not a 403
		{"jellyfin", "GET /System/Info/Public", 200, `{"Version":"10.11.11","StartupWizardCompleted":true}`, true},
		{"jellyfin", "GET /System/Info/Public", 200, `{"StartupWizardCompleted":false}`, false},
		{"audiobookshelf", "GET /status", 200, `{"app":"audiobookshelf","isInit":true}`, true},
		{"audiobookshelf", "GET /status", 200, `{"app":"audiobookshelf","isInit":false}`, false},
		{"vaultwarden", "GET /api/config", 200, `{"version":"2026.6.0","server":{"name":"Vaultwarden"}}`, true},
		{"vaultwarden", "GET /api/config", 200, `{"version":"2026.6.0"}`, false},
		{"forgejo", "GET /api/v1/users/search?limit=1", 200, `{"ok":true,"data":[{"id":1}]}`, true},
		{"forgejo", "GET /api/v1/users/search?limit=1", 200, `{"ok":true,"data":[]}`, false},
		{"seerr", "GET /api/v1/settings/public", 200, `{"initialized":true}`, true},
		{"seerr", "GET /api/v1/settings/public", 200, `{"initialized":false}`, false},
		{"actual", "GET /account/needs-bootstrap", 200, `{"status":"ok","data":{"bootstrapped":true}}`, true},
		{"actual", "GET /account/needs-bootstrap", 200, `{"status":"ok","data":{"bootstrapped":false}}`, false},
		{"actual", "GET /account/needs-bootstrap", 200, `{"status":"ok","data":"x"}`, false},
		{"n8n", "GET /rest/settings", 200, `{"data":{"userManagement":{"showSetupOnFirstLoad":false}}}`, true},
		{"n8n", "GET /rest/settings", 200, `{"data":{"userManagement":{"showSetupOnFirstLoad":true}}}`, false},
		{"n8n", "GET /rest/settings", 200, `{"data":{}}`, false}, // not n8n's answer: don't read a missing flag as false
		{"open-webui", "GET /api/config", 200, `{"status":true,"features":{"auth":true,"auth_trusted_header":false}}`, true},
		{"open-webui", "GET /api/config", 200, `{"onboarding":true,"status":true,"features":{"auth":true}}`, false},
		{"open-webui", "GET /api/config", 200, `{"status":true,"features":{"auth":false}}`, false},
		{"open-webui", "GET /api/config", 200, `{"status":true,"features":{"auth":true,"auth_trusted_header":true}}`, false},
		{"mealie", "GET /api/app/about/startup-info", 200, `{"isFirstLogin":false,"isDemo":false}`, true},
		{"mealie", "GET /api/app/about/startup-info", 200, `{"isFirstLogin":true,"isDemo":false}`, false},
		{"paperless", "GET /api/auth/headless/app/v1/config", 200, `{"status":200,"data":{"account":{"is_open_for_signup":false}}}`, true},
		{"paperless", "GET /api/auth/headless/app/v1/config", 200, `{"status":200,"data":{"account":{"is_open_for_signup":true}}}`, false},
		{"paperless", "GET /api/auth/headless/app/v1/config", 404, ``, false},
		{"trilium", "GET /bootstrap", 200, `{"loggedIn":false,"login":{"ssoEnabled":false}}`, true},
		{"trilium", "GET /bootstrap", 200, `{"passwordSet":false}`, false},
		{"trilium", "GET /bootstrap", 200, `{"dbInitialized":true,"passwordSet":true,"loggedIn":true}`, false}, // noAuthentication
		{"ghost", "GET /ghost/api/admin/authentication/setup/", 200, `{"setup":[{"status":true}]}`, true},
		{"ghost", "GET /ghost/api/admin/authentication/setup/", 200, `{"setup":[{"status":false,"title":"x"}]}`, false},
		{"ghost", "GET /ghost/api/admin/authentication/setup/", 200, `{"setup":[]}`, false},
		{"kavita", "GET /api/admin/exists", 200, `true`, true},
		{"kavita", "GET /api/admin/exists", 200, `false`, false},
		{"komga", "GET /api/v1/claim", 200, `{"isClaimed":true}`, true},
		{"komga", "GET /api/v1/claim", 200, `{"isClaimed":false}`, false},
		{"navidrome", "GET /app/", 200, `<script>window.__APP_CONFIG__ = "{\"firstTime\":false,\"version\":\"x\"}"</script>`, true},
		{"navidrome", "GET /app/", 200, `<script>window.__APP_CONFIG__ = "{\"firstTime\":true,\"version\":\"x\"}"</script>`, false},
	} {
		i := slices.IndexFunc(rs, func(r recipes.Recipe) bool { return r.App == tt.app })
		if i < 0 {
			t.Fatalf("no recipe %s", tt.app)
		}
		want, status, body = tt.ask, tt.status, tt.body
		got := setupProblem(context.Background(), app.Listener.Addr().String(), rs[i])
		if (got == "") != tt.done {
			t.Errorf("%s %d %s: %q, want set up %v", tt.ask, tt.status, tt.body, got, tt.done)
		}
	}

	excalidraw, _ := recipes.Find(rs, "excalidraw/excalidraw:latest")
	if !excalidraw.NoLogin {
		t.Error("excalidraw: want no_login, it has no setup to check")
	}
}

// Containers no recipe knows are listed as unsupported, except where routing
// them could reach this client's page or another app's switch.
func TestWatchUnsupported(t *testing.T) {
	ct := func(name, ip, ports string) string {
		return `{"Names":["/` + name + `"],"Image":"someone/app:1","HostConfig":{"NetworkMode":"bridge"},
			"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"` + ip + `"}}},"Ports":[` + ports + `]}`
	}
	tcp := func(p int) string { return fmt.Sprintf(`{"PrivatePort":%d,"Type":"tcp"}`, p) }
	running := "[" + strings.Join([]string{
		ct("My_Wiki", "172.17.0.2", tcp(3000)+","+tcp(3000)), // listed once per IPv4 and IPv6 binding
		ct("web", "172.17.0.3", tcp(443)+","+tcp(80)+`,{"PrivatePort":53,"Type":"udp"}`),
		ct("gitlike", "172.17.0.4", tcp(22)+","+tcp(3000)),
		ct("dup_a", "172.17.0.5", tcp(80)),
		ct("dup-a", "172.17.0.6", tcp(80)),
		ct("immich", "172.17.0.7", tcp(80)),  // a recipe's name
		ct("farstu", "127.0.0.1", tcp(7080)), // this client
		ct("db", "172.17.0.8", ""),           // no ports
		ct("noip", "", tcp(80)),
	}, ",") + "]"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, running) })
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	r := <-watchResults(t, fakeDocker(t, mux))
	if r.err != nil {
		t.Fatal(r.err)
	}
	if p := r.apps["gitlike"].Problem; !strings.Contains(p, "several ports [22 3000]") {
		t.Errorf("several ports: %q", p)
	}
	if p := r.apps["dup-a"].Problem; !strings.Contains(p, "More than one container is named dup-a") {
		t.Errorf("same label: %q", p)
	}
	delete(r.apps, "gitlike")
	delete(r.apps, "dup-a")
	want := map[string]App{
		"my-wiki": {Upstream: "172.17.0.2:3000", Unsupported: true},
		"web":     {Upstream: "172.17.0.3:80", Unsupported: true},
	}
	if !maps.Equal(r.apps, want) {
		t.Errorf("got %v, want %v", r.apps, want)
	}
}
