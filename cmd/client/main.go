// Command client is the Farstu Client: runs on the user's home server,
// supervises frpc, terminates TLS and proxies to apps.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/viedin/farstu-client/api"
	"github.com/viedin/farstu-client/internal/control"
	"github.com/viedin/farstu-client/internal/discover"
	"github.com/viedin/farstu-client/internal/frpc"
	"github.com/viedin/farstu-client/internal/proxy"
	"github.com/viedin/farstu-client/internal/recipes"
	"github.com/viedin/farstu-client/internal/ui"
	recipefs "github.com/viedin/farstu-client/recipes"
)

// frpc forwards to this listener; it must stay on loopback.
const listenPort = 8443

func main() {
	server := flag.String("server", "https://api.farstu.bytenode.se:8443", "Farstu Server URL")
	dataDir := flag.String("data", "/data", "state directory: device, certs, frpc config")
	frpcBin := flag.String("frpc", "/frpc", "frpc binary")
	ca := flag.String("ca", certmagic.LetsEncryptProductionCA, "ACME directory URL")
	uiAddr := flag.String("ui", ":7080", "address of the local page with app toggles; only the home network may use it")
	dockerSock := flag.String("docker", "/var/run/docker.sock", "Docker socket, to find installed apps")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: client [flags]\n\nApps are found through Docker and start off; turn them on at the -ui page.")
		flag.PrintDefaults()
	}
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *server, *dataDir, *frpcBin, *ca, *uiAddr, *dockerSock); err != nil {
		slog.Error("farstu client stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, server, dataDir, frpcBin, ca, uiAddr, dockerSock string) (err error) {
	if server == "" {
		return errors.New("-server is required")
	}
	uiLn, err := net.Listen("tcp", uiAddr)
	if err != nil {
		return fmt.Errorf("listen for local page: %w", err)
	}

	// The first one to fail stops the others; run returns once all have.
	ctx, cancel := context.WithCancel(ctx)
	errc, running := make(chan error, 5), 0
	spawn := func(f func() error) {
		running++
		go func() { errc <- f(); cancel() }()
	}
	defer func() {
		cancel()
		for range running {
			err = errors.Join(err, <-errc)
		}
	}()

	// Up before registering: the page shows the link to connect this server.
	var page ui.Page
	spawn(func() error { return page.Serve(ctx, uiLn) })
	slog.Info("local page", "addr", uiAddr)
	dev, err := control.Register(ctx, server, filepath.Join(dataDir, "device.json"), page.ShowCode)
	if err != nil {
		return err
	}

	rs, err := recipes.Load(recipefs.FS)
	if err != nil {
		return err
	}
	router := proxy.NewRouter()
	toggles := ui.Load(filepath.Join(dataDir, "apps.json"), router, dev.Subdomain+"."+dev.Domain)
	toggles.Logins = map[string][]recipes.Login{}
	for _, r := range rs {
		toggles.Supported = append(toggles.Supported, r.App)
		toggles.Logins[r.App] = r.Login
	}

	tlsConf, stopCerts, err := wildcardCert(ctx, server, dataDir, ca, dev)
	if err != nil {
		return err
	}
	defer stopCerts()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// frpc's admin API, for tunnel status. A free port, not frp's default 7400:
	// with host networking another frpc may already have that one.
	adminPort, err := freePort()
	if err != nil {
		return err
	}
	frpcConf := frpc.Config{
		Bin:           frpcBin,
		ConfigPath:    filepath.Join(dataDir, "frpc.toml"),
		RelayAddr:     dev.RelayAddr,
		RelayCert:     dev.RelayCert,
		Subdomain:     dev.Subdomain,
		Domain:        dev.Domain,
		Token:         dev.Token,
		LocalPort:     listenPort,
		AdminPort:     adminPort,
		AdminPassword: rand.Text(),
	}
	toggles.Tunnel = func(ctx context.Context) error { return frpc.Tunnel(ctx, frpcConf) }
	toggles.Probe = func(ctx context.Context, host string) (api.ProbeResponse, error) {
		return control.Probe(ctx, server, dev.Token, host)
	}
	page.Ready(toggles)

	spawn(func() error { return proxy.Serve(ctx, ln, tlsConf, router) })
	restartFrpc := make(chan struct{}, 1)
	spawn(func() error { return frpc.Run(ctx, frpcConf, restartFrpc) })
	spawn(func() error { watchPlan(ctx, server, dev.Token, toggles, restartFrpc); return nil })
	spawn(func() error { discover.Watch(ctx, dockerSock, rs, toggles.SetApps); return nil })
	slog.Info("farstu client started", "address", "*."+dev.Subdomain+"."+dev.Domain)
	<-ctx.Done()
	return nil
}

// watchPlan keeps the app limit in step with the account's plan, and
// reconnects the tunnel when the plan changes so the relay applies its speed
// limit. Until the first answer there is no app limit; the relay caps a free
// plan's speed regardless.
func watchPlan(ctx context.Context, server, token string, toggles *ui.UI, restartFrpc chan<- struct{}) {
	last := ""
	for {
		if me, err := control.Me(ctx, server, token); err != nil {
			slog.Warn("could not check the plan", "err", err)
		} else {
			toggles.SetAppLimit(me.AppLimit)
			if last != "" && me.Plan != last {
				slog.Info("plan changed, reconnecting the tunnel", "plan", me.Plan)
				select {
				case restartFrpc <- struct{}{}:
				default: // a restart is already pending
				}
			}
			last = me.Plan
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// freePort returns a loopback port that is free right now.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// wildcardCert manages *.<user>.<domain> via ACME DNS-01 through Farstu
// Server. The private key is generated and kept in dataDir; it never leaves
// this machine.
func wildcardCert(ctx context.Context, server, dataDir, ca string, dev control.Device) (*tls.Config, func(), error) {
	var cfg *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil },
	})
	cfg = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: filepath.Join(dataDir, "certs")},
	})
	cfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:                      ca,
		Agreed:                  true,
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: &control.DNSProvider{ServerURL: server, Token: dev.Token},
		}},
	})}
	// Async: obtaining and renewing retry in the background; handshakes fail
	// until a cert exists.
	if err := cfg.ManageAsync(ctx, []string{"*." + dev.Subdomain + "." + dev.Domain}); err != nil {
		cache.Stop()
		return nil, nil, fmt.Errorf("Certificate problem: %w", err)
	}
	tc := cfg.TLSConfig()
	tc.NextProtos = []string{"h2", "http/1.1"}
	return tc, cache.Stop, nil
}
