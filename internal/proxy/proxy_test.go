package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
	"go.etcd.io/bbolt"

	"github.com/viedin/farstu-client/internal/recipes"
)

func echoUpstream(t *testing.T) string {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s [%s] %s %s", r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Real-Ip"), r.Header.Get("Forwarded"), r.Header.Get("X-Forwarded-Proto"), r.Host)
	}))
	t.Cleanup(up.Close)
	return up.Listener.Addr().String()
}

func TestRouter(t *testing.T) {
	rt := NewRouter()
	rt.Enable("immich.u.farstu.test", echoUpstream(t), nil)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closed.Close()
	rt.Enable("down.u.farstu.test", closed.Addr().String(), nil)

	get := func(host string) (int, string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, tt := range []struct {
		host string
		code int
		body string
	}{
		{"immich.u.farstu.test", 200, "immich.u.farstu.test"},
		{"IMMICH.u.farstu.test.:443", 200, "IMMICH.u.farstu.test.:443"},
		{"other.u.farstu.test", 404, "No app is turned on"},
		{"", 404, "No app is turned on"},
		{"down.u.farstu.test", 502, "App is not running"},
	} {
		code, body := get(tt.host)
		if code != tt.code || !strings.Contains(body, tt.body) {
			t.Errorf("%q: %d %q, want %d %q", tt.host, code, body, tt.code, tt.body)
		}
	}

	rt.Disable("immich.u.farstu.test")
	if code, _ := get("immich.u.farstu.test"); code != 404 {
		t.Errorf("after Disable: %d, want 404", code)
	}
}

// A visitor can't sign in to an app that trusts a sign-in proxy's headers.
func TestIdentityHeadersDropped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			fmt.Fprintln(w, k)
		}
	}))
	defer up.Close()
	rt := NewRouter()
	rt.Enable("app.u.farstu.test", up.Listener.Addr().String(), nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.u.farstu.test"
	for _, h := range []string{"Remote-User", "Remote_User", "X-Webauth-User", "X-Forwarded-User", "X-Forwarded-Email", "X-Authentik-Username", "Tailscale-User-Login", "Cf-Access-Authenticated-User-Email", "X-Auth-Request-Email"} {
		req.Header[h] = []string{"admin"} // as sent, not canonicalized
	}
	req.Header.Set("Authorization", "Bearer kept")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	got := strings.Fields(rec.Body.String())
	for _, h := range got {
		if isIdentityHeader(h) {
			t.Errorf("forwarded %s", h)
		}
	}
	if !slices.Contains(got, "Authorization") || !slices.Contains(got, "X-Forwarded-For") {
		t.Errorf("dropped too much: %v", got)
	}
}

func TestServeProxyV2(t *testing.T) {
	donor := httptest.NewTLSServer(nil) // borrow its self-signed cert
	certs := donor.TLS.Certificates
	donor.Close()

	rt := NewRouter()
	rt.Enable("immich.u.farstu.test", echoUpstream(t), nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error)
	go func() { served <- Serve(ctx, ln, &tls.Config{Certificates: certs}, rt) }()

	client := func(sendHeader bool) *http.Client {
		return &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
				if err != nil || !sendHeader {
					return c, err
				}
				h := &proxyproto.Header{
					Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4,
					SourceAddr:      &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 5555},
					DestinationAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443},
				}
				_, err = h.WriteTo(c)
				return c, err
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			ForceAttemptHTTP2: true,
		}}
	}

	req, _ := http.NewRequest("GET", "https://immich.u.farstu.test/", nil)
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		req.Header.Set(h, "for=1.2.3.4") // spoofed
	}
	resp, err := client(true).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := "203.0.113.7 203.0.113.7 [] https immich.u.farstu.test"; string(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("proto %s, want HTTP/2 via ALPN", resp.Proto)
	}

	if _, err := client(false).Get("https://immich.u.farstu.test/"); err == nil {
		t.Error("connection without PROXY header was accepted")
	}

	// WebSocket: browsers upgrade over HTTP/1.1 (Go's h2 server has extended CONNECT off).
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "want upgrade", 400)
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusSwitchingProtocols)
		c, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer c.Close()
		io.Copy(c, buf) // echo
	}))
	defer ws.Close()
	rt.Enable("ws.u.farstu.test", ws.Listener.Addr().String(), nil)
	tr := client(true).Transport.(*http.Transport)
	raw, err := tr.DialContext(context.Background(), "tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: ws.u.farstu.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	wsResp, err := http.ReadResponse(br, nil)
	if err != nil || wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", wsResp, err)
	}
	fmt.Fprint(conn, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Errorf("websocket echo %q %v", got, err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// Case 3: turning an app off cuts its open streams and WebSockets within 1s.
func TestDisableCutsOpenStreams(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.WriteHeader(http.StatusSwitchingProtocols)
			c, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer c.Close()
			io.Copy(io.Discard, c) // hold it open
			return
		}
		w.Write([]byte("hi"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stream forever
	}))
	defer up.Close()
	for _, reEnable := range []bool{false, true} {
		t.Run(fmt.Sprint("reEnable=", reEnable), func(t *testing.T) {
			rt := NewRouter()
			rt.Enable("a.u.farstu.test", up.Listener.Addr().String(), nil)
			edge := httptest.NewServer(rt)
			defer edge.Close()

			req, _ := http.NewRequest("GET", edge.URL, nil)
			req.Host = "a.u.farstu.test"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadFull(resp.Body, make([]byte, 2)); err != nil {
				t.Fatal(err)
			}

			ws, err := net.Dial("tcp", edge.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			fmt.Fprint(ws, "GET / HTTP/1.1\r\nHost: a.u.farstu.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			br := bufio.NewReader(ws)
			if r, err := http.ReadResponse(br, nil); err != nil || r.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("upgrade: %v %v", r, err)
			}

			streamDone := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, resp.Body); streamDone <- err }()

			if reEnable { // streams opened before a re-enable must not outlive Disable
				rt.Enable("a.u.farstu.test", up.Listener.Addr().String(), nil)
			}
			rt.Disable("a.u.farstu.test")
			timeout := time.After(time.Second)

			ws.SetDeadline(time.Now().Add(time.Second))
			if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("WebSocket still open after Disable: %v", err)
			}
			select {
			case err := <-streamDone:
				if err == nil {
					t.Error("stream ended cleanly, want it cut")
				}
			case <-timeout:
				t.Error("stream still open after Disable")
			}
		})
	}
}

// Guardrail: password guessing at the recipe's login paths is cut off per
// visitor; the rest of the app and other visitors are unaffected.
func TestLoginLimit(t *testing.T) {
	rt := NewRouter()
	rt.Enable("immich.u.farstu.test", echoUpstream(t), []recipes.Login{{Path: "/api/auth/login", Method: "POST"}, {Path: "/rpc/SignIn", Method: "POST"}})
	do := func(remote, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "immich.u.farstu.test"
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := range loginBurst {
		if code := do("203.0.113.7:1000", "POST", "/api/auth/login"); code != 200 {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	for _, p := range []string{"/api/auth/login", "/API/auth/login/", "//api/./auth/login", "/rpc/SignIn"} { // one budget for all of them
		if code := do("203.0.113.7:2000", "post", p); code != http.StatusTooManyRequests {
			t.Errorf("over the limit at %s: %d", p, code)
		}
	}
	if code := do("203.0.113.7:1000", "GET", "/api/assets"); code != 200 {
		t.Errorf("rest of the app blocked: %d", code)
	}
	if code := do("203.0.113.8:1000", "POST", "/api/auth/login"); code != 200 {
		t.Errorf("another visitor blocked: %d", code)
	}

	// IPv6: one /64 is one visitor.
	for range loginBurst {
		do("[2001:db8:1:2::1]:1", "POST", "/api/auth/login")
	}
	if code := do("[2001:db8:1:2::99]:1", "POST", "/api/auth/login"); code != http.StatusTooManyRequests {
		t.Errorf("same /64: %d", code)
	}
	if code := do("[2001:db8:1:3::1]:1", "POST", "/api/auth/login"); code != 200 {
		t.Errorf("other /64: %d", code)
	}
}

// Guardrail: wrong passwords, sign-ins the limit blocks and pages not found
// are counted per visitor and day, kept for 30 days, and survive a restart.
func TestActivity(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/login":
			http.Error(w, "wrong password", http.StatusUnauthorized)
		case "/moved":
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	const app = "immich.u.farstu.test"
	router := func() *Router {
		rt := NewRouter()
		rt.seen.now = func() time.Time { return now }
		rt.Enable(app, up.Listener.Addr().String(), []recipes.Login{{Path: "/login", Method: "POST"}, {Path: "/moved", Method: "POST"}})
		return rt
	}
	do := func(rt *Router, host, remote, method, path string, n int) {
		for range n {
			req := httptest.NewRequest(method, path, nil)
			req.Host = host
			req.RemoteAddr = remote
			rt.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	shown := func(rt *Router) string {
		var s []string
		for _, a := range rt.Activity() {
			s = append(s, fmt.Sprint(a.Day.Format(time.DateOnly), " ", a.Host, " ", a.Visitor, " ", a.Failed, " ", a.Blocked, " ", a.NotFound))
		}
		return strings.Join(s, ", ")
	}
	path := filepath.Join(t.TempDir(), "farstu.db")
	keep := func(rt *Router) { // load, save once, close
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt.Keep(ctx, path)
	}

	rt := router()
	do(rt, app, "203.0.113.7:1", "POST", "/login", loginBurst+5) // 10 wrong, 5 blocked
	do(rt, app, "203.0.113.7:1", "GET", "/missing", 1)
	do(rt, app, "203.0.113.7:1", "GET", "/", 5) // ordinary visits aren't kept
	now = now.Add(time.Second)
	do(rt, app, "198.51.100.9:1", "POST", "/moved", 1) // a moved sign-in page, not a wrong password
	now = now.Add(time.Second)
	do(rt, "admin.u.farstu.test", "192.0.2.1:1", "GET", "/", 3)
	now = now.Add(24 * time.Hour)
	do(rt, app, "203.0.113.8:1", "POST", "/login", 1)
	want := "2026-10-11 " + app + " 203.0.113.8 1 0 0, 2026-10-10  192.0.2.1 0 0 3, 2026-10-10 " + app + " 198.51.100.9 0 0 1, 2026-10-10 " + app + " 203.0.113.7 10 5 1"
	if got := shown(rt); got != want {
		t.Errorf("activity:\n%s, want\n%s", got, want)
	}

	// Saved and loaded again by the next start, corrupt and empty entries
	// dropped.
	keep(rt)
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bbolt.Tx) error {
		empty, _ := json.Marshal(Activity{Day: now, Visitor: "nothing", Last: now})
		return errors.Join(tx.Bucket(bucket).Put([]byte("x"), []byte("{")), tx.Bucket(bucket).Put([]byte("2026-10-11||nothing"), empty))
	})
	db.Close()
	rt = router()
	keep(rt)
	if got := shown(rt); got != want {
		t.Errorf("after a restart:\n%s, want\n%s", got, want)
	}

	// 30 days on, the first day is gone, also from the database.
	now = now.Add(window - time.Hour)
	keep(router())
	now = now.Add(-window + time.Hour)
	// Flagged before loading: added to what was saved.
	rt = router()
	do(rt, app, "203.0.113.8:1", "POST", "/login", 1)
	keep(rt)
	if got, want := shown(rt), "2026-10-11 "+app+" 203.0.113.8 2 0 0"; got != want {
		t.Errorf("30 days on:\n%s, want\n%s", got, want)
	}
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bbolt.Tx) error {
		if n := tx.Bucket(bucket).Stats().KeyN; n != 1 {
			t.Errorf("%d entries saved, want 1", n)
		}
		return nil
	})
}

// A broken database is set aside for a new one; it never stops the client.
func TestKeepBrokenDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "farstu.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not bbolt ", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := NewRouter()
	rt.seen.flag("", "192.0.2.1", notFound)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt.Keep(ctx, path)
	if _, err := os.Stat(path + ".broken"); err != nil {
		t.Errorf("not set aside: %v", err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(bucket); b == nil || b.Stats().KeyN != 1 {
			t.Errorf("not saved to a new database")
		}
		return nil
	})
}

// Full: scans go first, the oldest first; sign-ins last.
func TestEvict(t *testing.T) {
	s := newActivity()
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }
	for _, a := range []Activity{
		{Visitor: "guess", Failed: 1, Last: t0},
		{Visitor: "old scan", NotFound: 1, Last: t0.Add(time.Minute)},
		{Visitor: "new scan", NotFound: 1, Last: t0.Add(time.Hour)},
	} {
		s.by[a.key()] = &a
	}
	for _, want := range []string{"old scan", "new scan", "guess"} {
		before := s.all()
		s.evict()
		if len(s.by) != len(before)-1 || slices.ContainsFunc(s.all(), func(a Activity) bool { return a.Visitor == want }) {
			t.Fatalf("evicted %+v, want %s gone", s.all(), want)
		}
	}
}
