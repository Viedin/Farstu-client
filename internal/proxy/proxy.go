// Package proxy is the client's edge: it accepts PROXY v2 + TLS connections
// from frpc and routes requests by Host to enabled apps.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pires/go-proxyproto"
	"golang.org/x/time/rate"

	"github.com/viedin/farstu-client/internal/recipes"
)

// Router routes requests by Host to enabled apps. Unknown hosts are rejected.
type Router struct {
	mu     sync.RWMutex
	routes map[string]*route
	logins logins
	seen   activity
}

type route struct {
	proxy  *httputil.ReverseProxy
	logins []recipes.Login
	off    context.Context // done once the app is turned off
	stop   context.CancelFunc
}

const offMsg = "No app is turned on at this address"

func NewRouter() *Router {
	return &Router{routes: map[string]*route{}, logins: logins{by: map[string]*rate.Limiter{}}, seen: newActivity()}
}

// Activity returns the last 30 days of sign-ins and pages not found, newest first.
func (rt *Router) Activity() []Activity { return rt.seen.all() }

// Enable routes host to the plain-HTTP upstream address (e.g. "127.0.0.1:2283").
// Sign-in attempts at logins are rate limited per visitor.
func (rt *Router) Enable(host, upstream string, logins []recipes.Login) {
	target := &url.URL{Scheme: "http", Host: upstream}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // apps build links from Host
			// Rewrite already dropped the visitor's X-Forwarded-* and
			// Forwarded. Apps that log or rate limit by IP also read
			// X-Real-Ip; the visitor's own would let them pose as anyone.
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Real-Ip", pr.Out.Header.Get("X-Forwarded-For"))
			for k := range pr.Out.Header {
				if isIdentityHeader(k) {
					delete(pr.Out.Header, k) // not Del: it'd canonicalize k, missing a raw "Remote_User"
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Wrong passwords come back as 401, 403 or 400 (Vaultwarden); a
			// 404 there is a moved endpoint, not a guess.
			// ponytail: apps that answer a wrong password with 200 or a
			// redirect (Nextcloud's form login) aren't counted; the limit
			// still blocks them.
			switch {
			case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusNotFound && isLogin(resp.Request, logins):
				rt.seen.flag(normalize(host), visitor(resp.Request.RemoteAddr), failed)
			case resp.StatusCode == http.StatusNotFound:
				rt.seen.flag(normalize(host), visitor(resp.Request.RemoteAddr), notFound)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if context.Cause(r.Context()) == errTurnedOff {
				http.Error(w, offMsg, http.StatusNotFound)
				return
			}
			if errors.Is(err, context.Canceled) {
				return // visitor went away
			}
			slog.Warn("upstream failed", "host", r.Host, "err", err)
			http.Error(w, "App is not running", http.StatusBadGateway)
		},
	}
	off, stop := context.WithCancel(context.Background())
	rt.mu.Lock()
	old := rt.routes[normalize(host)]
	rt.routes[normalize(host)] = &route{proxy: p, logins: logins, off: off, stop: stop}
	rt.mu.Unlock()
	if old != nil {
		old.stop() // else its open streams would outlive a later Disable
	}
}

// identityHeaders are prefixes of the headers apps behind a sign-in proxy
// (Authelia, authentik, oauth2-proxy, Tailscale, Cloudflare Access) take as
// the signed-in user when they come from a trusted address, and those they
// take as the visitor's IP (CDNs' and others'). Every visitor comes from
// ours, so a visitor's own would sign them in as anyone, or let them pose
// as any address to the app's own brute-force limit.
var identityHeaders = []string{
	"remote-", "x-remote-", "x-webauth-", "x-auth-request-", "x-authentik-", "tailscale-", "cf-access-",
	"x-forwarded-user", "x-forwarded-email", "x-forwarded-preferred-username", "x-forwarded-groups", "x-forwarded-access-token",
	"true-client-ip", "cf-connecting-ip", "x-client-ip", "x-cluster-client-ip", "fastly-client-ip",
}

// isIdentityHeader matches "_" as "-": Python servers read Remote_User as
// Remote-User.
func isIdentityHeader(name string) bool {
	name = strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return slices.ContainsFunc(identityHeaders, func(p string) bool { return strings.HasPrefix(name, p) })
}

// Disable stops routing new requests to host and cuts its in-flight
// requests, streams and WebSockets.
func (rt *Router) Disable(host string) {
	rt.mu.Lock()
	r := rt.routes[normalize(host)]
	delete(rt.routes, normalize(host))
	rt.mu.Unlock()
	if r != nil {
		r.stop()
	}
}

var errTurnedOff = errors.New("app turned off")

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mu.RLock()
	rr := rt.routes[normalize(r.Host)]
	rt.mu.RUnlock()
	if rr == nil {
		rt.seen.flagUnknown(normalize(r.Host), visitor(r.RemoteAddr))
		http.Error(w, offMsg, http.StatusNotFound)
		return
	}
	if isLogin(r, rr.logins) && !rt.logins.allow(r.RemoteAddr) {
		rt.seen.flag(normalize(r.Host), visitor(r.RemoteAddr), blocked)
		http.Error(w, "Too many sign-in attempts from your network. Wait a minute and try again", http.StatusTooManyRequests)
		return
	}
	// ReverseProxy closes the upstream (and so the visitor's stream or
	// WebSocket) when the request context ends.
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	defer context.AfterFunc(rr.off, func() { cancel(errTurnedOff) })()
	rr.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// Sign-in attempts a visitor may make at once, and how fast that allowance
// comes back: enough for a family behind one address, too slow to guess
// passwords. Attempts, not failures: those are only known after the
// answer, by when parallel guesses would all be through.
const (
	loginBurst  = 10
	loginRefill = time.Minute
)

// isLogin reports whether r is a sign-in at one of logins. Paths are
// compared the way lenient app routers match them, so "/API/auth/login/"
// counts too.
func isLogin(r *http.Request, logins []recipes.Login) bool {
	return slices.ContainsFunc(logins, func(l recipes.Login) bool {
		return strings.EqualFold(r.Method, l.Method) && strings.EqualFold(path.Clean("/"+r.URL.Path), l.Path)
	})
}

// logins limits sign-in attempts per visitor.
type logins struct {
	mu sync.Mutex
	by map[string]*rate.Limiter
}

func (l *logins) allow(remoteAddr string) bool {
	k := visitor(remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	lim := l.by[k]
	if lim == nil {
		// ponytail: pruned only once big; a botnet with that many addresses
		// still gets one budget each. Add a cap across visitors if that shows up.
		if len(l.by) >= 10_000 {
			for k, v := range l.by {
				if v.Tokens() >= loginBurst {
					delete(l.by, k)
				}
			}
		}
		lim = rate.NewLimiter(rate.Every(loginRefill), loginBurst)
		l.by[k] = lim
	}
	return lim.Allow()
}

// visitor is who attempts are counted against: the IP from the PROXY
// header, or its /64 for IPv6, where one home or attacker has a whole /64.
func visitor(remoteAddr string) string {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	ip := ap.Addr().Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

func normalize(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// Serve accepts connections from frpc on ln, each starting with a PROXY v2
// header, terminates TLS and serves h until ctx is done.
func Serve(ctx context.Context, ln net.Listener, tlsConf *tls.Config, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// No ReadTimeout/WriteTimeout: big uploads and long streams must not be cut off.
	}
	// proxyproto's default policy is REQUIRE: connections without a header are dropped.
	pl := &proxyproto.Listener{Listener: ln}

	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(pl, "", "") }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	<-errc
	return nil
}
