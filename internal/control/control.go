// Package control talks to Farstu Server: device registration, the plan,
// external probes and ACME DNS-01 TXT records.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"time"

	"github.com/viedin/farstu-client/api"
	"github.com/viedin/farstu-client/internal/atomicfile"
)

// No redirects: Go follows one from https to http on the same host, with
// the device token.
var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// CheckServerURL refuses a Farstu Server URL the device token would travel
// to unencrypted: anything but https, except on this machine for testing.
func CheckServerURL(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("-server %q is not a URL like https://api.farstu.app:8443", server)
	}
	ip, _ := netip.ParseAddr(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip.IsLoopback())) {
		return fmt.Errorf("-server %q must start with https://: the device token only goes over TLS", server)
	}
	return nil
}

// ErrUnknownDevice means Farstu Server no longer knows this device: it was
// disconnected on the account page, replaced by another server, or its
// account was deleted. Only registering again helps.
var ErrUnknownDevice = errors.New("this server was disconnected from its Farstu account")

// Device is what the client keeps after registering.
type Device = api.DeviceTokenResponse

// registerRetry is the first wait after a failed registration attempt;
// it doubles up to 5 minutes. Tests shorten it.
var registerRetry = 5 * time.Second

// Register loads the device saved at path, or runs the device flow and saves
// it there, passing each registration code to show. A corrupt file is an
// error, not a reason to register again: that would silently give the user a
// new subdomain.
func Register(ctx context.Context, serverURL, path string, show func(api.DeviceCodeResponse)) (Device, error) {
	var d Device
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &d); err != nil {
			return d, fmt.Errorf("device file %s is corrupt, delete it to register again: %w", path, err)
		}
		if d.Token == "" || d.RelayCert == "" {
			return d, fmt.Errorf("device file %s has no token or relay certificate, delete it to register again", path)
		}
		return d, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return d, fmt.Errorf("read device file: %w", err)
	}

	// An expired code returns an empty Device: start over. A server that's
	// down or busy is waited out, not a reason to stop.
	for wait := registerRetry; d.Token == ""; {
		if d, err = deviceFlow(ctx, serverURL, show); err == nil {
			wait = registerRetry
			continue
		}
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		slog.Warn("registration failed, retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
	if d.RelayCert == "" {
		return d, errors.New("Farstu Server sent no relay certificate; it needs an update")
	}
	b, _ = json.Marshal(d)
	// Whole or not at all: a half-written file stops the client, and the
	// token was shown only once.
	if err := atomicfile.Write(path, b); err != nil {
		return d, fmt.Errorf("save device file: %w", err)
	}
	fmt.Printf("Connected! Your apps will be at https://<app>.%s.%s\n\n", d.Subdomain, d.Domain)
	return d, nil
}

// deviceFlow runs one RFC 8628 attempt.
func deviceFlow(ctx context.Context, serverURL string, show func(api.DeviceCodeResponse)) (Device, error) {
	var dc api.DeviceCodeResponse
	status, body, err := do(ctx, http.MethodPost, serverURL+"/v1/device/code", "", nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	if err == nil {
		err = json.Unmarshal(body, &dc)
	}
	if err != nil {
		return Device{}, fmt.Errorf("start registration: %w", err)
	}
	fmt.Printf("\nTo connect this server to Farstu, open this link, sign in and click Connect:\n\n    %s\n\n(code %s, valid %d minutes)\n\n",
		dc.VerificationURIComplete, dc.UserCode, dc.ExpiresIn/60)
	if show != nil {
		show(dc)
	}

	req := api.DeviceTokenRequest{DeviceCode: dc.DeviceCode}
	for {
		select {
		case <-ctx.Done():
			return Device{}, ctx.Err()
		case <-time.After(time.Duration(dc.Interval) * time.Second):
		}
		status, body, err := do(ctx, http.MethodPost, serverURL+"/v1/device/token", "", req)
		if err == nil && (status >= 500 || status == http.StatusTooManyRequests) {
			err = fmt.Errorf("HTTP %d", status)
		}
		if err != nil {
			slog.Warn("registration: server unavailable, retrying", "err", err)
			continue
		}
		if status == http.StatusOK {
			var d Device
			if err := json.Unmarshal(body, &d); err != nil {
				return Device{}, fmt.Errorf("registration response: %w", err)
			}
			return d, nil
		}
		var e api.Error
		json.Unmarshal(body, &e)
		switch e.Error {
		case api.ErrAuthorizationPending:
		case api.ErrExpiredToken, api.ErrInvalidGrant:
			// invalid_grant: the server already purged the expired code, or
			// consumed it but its reply got lost. Either way: new code.
			slog.Info("registration code no longer valid, getting a new one")
			return Device{}, nil
		default:
			return Device{}, fmt.Errorf("registration rejected: HTTP %d %s", status, e.Error)
		}
	}
}

// Probe asks Farstu Server to fetch https://<host>/ from outside.
func Probe(ctx context.Context, serverURL, token, host string) (api.ProbeResponse, error) {
	var p api.ProbeResponse
	status, body, err := do(ctx, http.MethodPost, serverURL+"/v1/probe", token, api.ProbeRequest{Host: host})
	if err != nil {
		return p, fmt.Errorf("Farstu Server unreachable: %w", err)
	}
	if status != http.StatusOK {
		return p, fmt.Errorf("Farstu Server refused the check (HTTP %d)", status)
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("Farstu Server sent a bad answer: %w", err)
	}
	return p, nil
}

// Me returns the plan of the account this device belongs to.
func Me(ctx context.Context, serverURL, token string) (api.Me, error) {
	var m api.Me
	status, body, err := do(ctx, http.MethodGet, serverURL+"/v1/me", token, nil)
	if err != nil {
		return m, fmt.Errorf("Farstu Server unreachable: %w", err)
	}
	if status == http.StatusUnauthorized {
		return m, ErrUnknownDevice
	}
	if status != http.StatusOK {
		return m, fmt.Errorf("Farstu Server refused the plan check (HTTP %d)", status)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("Farstu Server sent a bad answer: %w", err)
	}
	return m, nil
}

// Feedback sends what the user wrote to Farstu.
func Feedback(ctx context.Context, serverURL, token string, f api.Feedback) error {
	status, _, err := do(ctx, http.MethodPost, serverURL+"/v1/feedback", token, f)
	if err != nil {
		return fmt.Errorf("Farstu Server unreachable: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("Farstu Server refused it (HTTP %d)", status)
	}
	return nil
}

// do sends v as JSON (if not nil) with an optional bearer token.
func do(ctx context.Context, method, url, token string, v any) (status int, body []byte, err error) {
	var rd io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, err
}
