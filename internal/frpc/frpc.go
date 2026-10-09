// Package frpc runs the pinned frpc binary as a supervised subprocess.
package frpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// proxyName is our one proxy in frpc's config and status.
const proxyName = "farstu"

type Config struct {
	Bin        string // frpc binary
	ConfigPath string // generated frpc.toml; holds the token, written 0600
	RelayAddr  string // frps host:port
	RelayCert  string // frps's cert (PEM); the only one frpc trusts for the relay
	Subdomain  string
	Domain     string
	Token      string
	LocalPort  int // the client's PROXY v2 + TLS listener on 127.0.0.1

	// frpc's admin API on 127.0.0.1, used for tunnel status. Its password is
	// random per run: the API can also rewrite frpc's config.
	AdminPort     int
	AdminPassword string
}

// certPath is where Run writes the relay's cert for frpc.
func certPath(c Config) string { return filepath.Join(filepath.Dir(c.ConfigPath), "relay.pem") }

// Render returns frpc's TOML config: TLS to the relay verified against its
// pinned cert, one https proxy for the device's wildcard, forwarded with
// PROXY v2 to the local TLS listener, and the admin API on loopback.
func Render(c Config) (string, error) {
	host, port, err := net.SplitHostPort(c.RelayAddr)
	if err != nil {
		return "", fmt.Errorf("relay address %q: %w", c.RelayAddr, err)
	}
	// Without a trusted cert frpc skips verification and would send the
	// token to anyone in between.
	if c.RelayCert == "" {
		return "", errors.New("no relay certificate")
	}
	// Values come from our server; %q is valid TOML for these plain ASCII strings.
	return fmt.Sprintf(`serverAddr = %q
serverPort = %s
transport.tls.enable = true
transport.tls.trustedCaFile = %q
user = %q
metadatas.token = %q
loginFailExit = false
log.to = "console"
log.disablePrintColor = true
webServer.addr = "127.0.0.1"
webServer.port = %d
webServer.user = "farstu"
webServer.password = %q

[[proxies]]
name = %q
type = "https"
customDomains = [%q]
localIP = "127.0.0.1"
localPort = %d
transport.proxyProtocolVersion = "v2"
`, host, port, certPath(c), c.Subdomain, c.Token, c.AdminPort, c.AdminPassword, proxyName, "*."+c.Subdomain+"."+c.Domain, c.LocalPort), nil
}

// Run writes the config and keeps frpc running until ctx is done, then stops
// it with SIGTERM (SIGKILL after 5s). A receive on restart reconnects it, so
// the relay applies a new plan's limits.
func Run(ctx context.Context, c Config, restart <-chan struct{}) error {
	conf, err := Render(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath(c), []byte(c.RelayCert), 0o600); err != nil {
		return fmt.Errorf("write relay cert: %w", err)
	}
	if err := os.WriteFile(c.ConfigPath, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("write frpc config: %w", err)
	}
	cred, err := dropRoot(certPath(c), c.ConfigPath)
	if err != nil {
		return err
	}

	backoff := time.Second
	for {
		runCtx, stop := context.WithCancel(ctx)
		go func() {
			select {
			case <-restart:
				stop()
			case <-runCtx.Done():
			}
		}()
		cmd := exec.CommandContext(runCtx, c.Bin, "-c", c.ConfigPath)
		cmd.Stdout = &logWriter{}
		cmd.Stderr = cmd.Stdout
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second

		started := time.Now()
		err := cmd.Run()
		restarted := runCtx.Err() != nil
		stop()
		if ctx.Err() != nil {
			return nil
		}
		if restarted {
			slog.Info("frpc restarted to reconnect")
			continue
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("frpc binary %s: %w", c.Bin, err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it ran fine for a while
		}
		slog.Warn("Tunnel is down: frpc exited, restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// frpcUID is who frpc runs as when the client is root (as in its image).
// frpc talks to the relay; a bug in it, or a hacked relay, must not get it
// the Docker socket (root on the host) or the TLS key. A uid of its own,
// not nobody's: frpc.toml holds the device token.
const frpcUID = 10001

// dropRoot gives frpc's files to frpcUID and returns the credential to run
// it with, or nil when the client isn't root and can't switch.
func dropRoot(files ...string) (*syscall.Credential, error) {
	if os.Getuid() != 0 {
		return nil, nil
	}
	for _, f := range files {
		if err := os.Chown(f, frpcUID, frpcUID); err != nil {
			return nil, fmt.Errorf("give frpc its files: %w", err)
		}
	}
	return &syscall.Credential{Uid: frpcUID, Gid: frpcUID}, nil // and no supplementary groups
}

// logWriter forwards frpc's console output to slog, one line per record.
type logWriter struct{ buf []byte }

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		line, rest, ok := strings.Cut(string(w.buf), "\n")
		if !ok {
			return len(p), nil
		}
		slog.Info("frpc", "msg", line)
		w.buf = []byte(rest)
	}
}

// Tunnel reports whether frpc is connected to the relay with its proxy up,
// via frpc's admin API.
func Tunnel(ctx context.Context, c Config) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/status", c.AdminPort), nil)
	req.SetBasicAuth("farstu", c.AdminPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("Tunnel is down: frpc is not running")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Tunnel is down: frpc status unavailable (HTTP %d)", resp.StatusCode)
	}
	var status map[string][]struct {
		Name, Status, Err string
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("Tunnel is down: frpc status: HTTP %d: %w", resp.StatusCode, err)
	}
	for _, p := range status["https"] {
		switch {
		case p.Name != proxyName:
		case p.Status == "running":
			return nil
		case p.Err != "":
			return fmt.Errorf("Tunnel is down: %s: %s", p.Status, p.Err)
		default:
			return fmt.Errorf("Tunnel is down: %s", p.Status)
		}
	}
	return errors.New("Tunnel is down: not connected to the relay")
}
