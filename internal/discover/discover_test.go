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
		{200, `<html>`, "", "Can't check whether Immich is set up: unexpected answer"},
		{500, `{"isInitialized":true}`, "", "Can't check whether Immich is set up (HTTP 500)"},
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
		if r.Host != "localhost" {
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
