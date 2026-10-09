package frpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTunnel(t *testing.T) {
	body := ""
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "farstu" || p != "secret" || r.URL.Path != "/api/status" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer admin.Close()
	_, port, _ := net.SplitHostPort(admin.Listener.Addr().String())
	c := Config{AdminPassword: "secret"}
	c.AdminPort, _ = strconv.Atoi(port)

	for _, tt := range []struct{ body, want string }{
		{`{"https":[{"name":"farstu","type":"https","status":"running","err":""}]}`, ""},
		{`{"https":[{"name":"farstu","status":"start error","err":"router config conflict"}]}`, "Tunnel is down: start error: router config conflict"},
		{`{"https":[{"name":"farstu","status":"wait start","err":""}]}`, "Tunnel is down: wait start"},
		{`{}`, "Tunnel is down: not connected to the relay"},
	} {
		body = tt.body
		err := Tunnel(context.Background(), c)
		if got := fmt.Sprint(err); (err == nil && tt.want != "") || (err != nil && got != tt.want) {
			t.Errorf("%s: %v, want %q", tt.body, err, tt.want)
		}
	}

	c.AdminPassword = "wrong"
	if err := Tunnel(context.Background(), c); fmt.Sprint(err) != "Tunnel is down: frpc status unavailable (HTTP 401)" {
		t.Errorf("wrong admin password: %v", err)
	}
	admin.Close()
	if err := Tunnel(context.Background(), c); fmt.Sprint(err) != "Tunnel is down: frpc is not running" {
		t.Errorf("frpc gone: %v", err)
	}
}

// frpc skips verifying the relay unless trustedCaFile is set, and would then
// hand the device token to a man in the middle.
func TestRenderPinsRelay(t *testing.T) {
	c := Config{ConfigPath: "/data/frpc.toml", RelayAddr: "relay.test:7000", RelayCert: "PEM"}
	conf, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"transport.tls.enable = true\n", `transport.tls.trustedCaFile = "/data/relay.pem"` + "\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	c.RelayCert = ""
	if _, err := Render(c); err == nil {
		t.Error("rendered a config without the relay cert")
	}
}

func TestRunRestart(t *testing.T) {
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	bin := filepath.Join(dir, "frpc")
	os.WriteFile(bin, []byte("#!/bin/sh\necho x >> "+starts+"\nexec sleep 60\n"), 0o755)
	count := func() int {
		b, _ := os.ReadFile(starts)
		return strings.Count(string(b), "x")
	}
	waitFor := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); count() < n; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("frpc started %d times, want %d", count(), n)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	restart := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- Run(ctx, Config{Bin: bin, ConfigPath: filepath.Join(dir, "frpc.toml"), RelayAddr: "relay.test:7000", RelayCert: "PEM"}, restart)
	}()
	waitFor(1)
	restart <- struct{}{}
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// As root (the client's image), frpc runs as frpcUID and can still read its
// config. Run with: docker run --rm -v "$PWD":/src -w /src golang:1.27 go test ./internal/frpc/
func TestRunDropsRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o755) // frpc must reach its files, as in /data
	out := filepath.Join(dir, "uid")
	os.WriteFile(out, nil, 0o666)
	os.Chmod(out, 0o666)
	bin := filepath.Join(dir, "frpc")
	os.WriteFile(bin, []byte("#!/bin/sh\ncat \"$2\" >/dev/null && id -u > "+out+"\nexec sleep 60\n"), 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- Run(ctx, Config{Bin: bin, ConfigPath: filepath.Join(dir, "frpc.toml"), RelayAddr: "relay.test:7000", RelayCert: "PEM"}, nil)
	}()
	var uid []byte
	for deadline := time.Now().Add(5 * time.Second); len(uid) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		uid, _ = os.ReadFile(out)
	}
	cancel()
	<-done
	if string(uid) != strconv.Itoa(frpcUID)+"\n" {
		t.Fatalf("frpc ran as uid %q, want %d", uid, frpcUID)
	}
}
