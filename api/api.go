// Package api holds the request/response types between Farstu Client and
// Farstu Server; the server imports this package. Change both sides together.
package api

import "time"

// POST /v1/device/code (RFC 8628 device authorization).
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// POST /v1/device/token.
type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

// DeviceTokenResponse is returned once the user approved the code. The token
// authenticates the device to the API and to the relay; it is shown once.
type DeviceTokenResponse struct {
	Token     string `json:"token"`
	Subdomain string `json:"subdomain"` // <user> in <app>.<user>.<domain>
	Domain    string `json:"domain"`
	RelayAddr string `json:"relay_addr"` // frps host:port
	// RelayCert is frps's self-signed cert (PEM). frpc trusts only this one
	// for the relay, so the token can't be phished by a man in the middle.
	RelayCert string `json:"relay_cert"`
}

// Error codes for /v1/device/token, as in RFC 8628 section 3.5.
const (
	ErrAuthorizationPending = "authorization_pending"
	ErrExpiredToken         = "expired_token"
	ErrInvalidGrant         = "invalid_grant"
)

type Error struct {
	Error string `json:"error"`
}

// PUT and DELETE /v1/dns/txt, authenticated with "Authorization: Bearer <token>".
// Only _acme-challenge.<subdomain>.<domain> may be written.
type TXTRecord struct {
	Name  string `json:"name"` // fully qualified, no trailing dot
	Value string `json:"value"`
}

// POST /v1/probe, authenticated like /v1/dns/txt. The server fetches
// https://<host>/ from outside, the way a visitor would. Only hosts in the
// device's own zone may be probed.
type ProbeRequest struct {
	Host string `json:"host"` // <app>.<subdomain>.<domain>
}

type ProbeResponse struct {
	Status    int    `json:"status,omitempty"` // HTTP status the app answered with
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"` // user-facing, e.g. "Certificate problem: ..."
}

// GET /v1/me, authenticated like /v1/dns/txt: the device owner's plan.
type Me struct {
	Plan     string `json:"plan"`      // "free" or "paid"
	AppLimit int    `json:"app_limit"` // apps that may be on at once; 0 means no limit
	// When paid time ends; zero when it doesn't, or on the free plan.
	PaidUntil time.Time `json:"paid_until,omitzero"`
}

// POST /v1/feedback, authenticated like /v1/dns/txt; answered with 204.
type Feedback struct {
	Message string `json:"message"`           // 1-5000 characters
	Version string `json:"version,omitempty"` // the client's, e.g. "v0.2.9"; at most 100 characters
}

// The device's one frps proxy, by plan. The relay only accepts these
// (frpc prefixes the name with "<subdomain>."):
//   - ProxyAll: customDomains ["*.<subdomain>.<domain>"]; paid plan only.
//   - ProxyApps: customDomains are the hosts of the apps that are on, at most
//     Me.AppLimit; speed capped.
const (
	ProxyAll  = "all"
	ProxyApps = "apps"
)
