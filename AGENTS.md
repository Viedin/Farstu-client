# AGENTS.md

Farstu Client: runs on the user's home server; supervises `frpc`, terminates
TLS, proxies to apps. Design and rationale live in README.md; read it before
architectural changes. This repo is open source (Apache-2.0). The Farstu
Server lives in a separate, private repo and imports `api/` from here.

## Layout

- `cmd/client` – main
- `api` – request/response types with Farstu Server. A change here needs the
  matching change in the server repo; keep JSON fields backward compatible
  (old clients keep talking to the new server)
- `internal/...` – control (server API), discover, frpc, proxy, ui, recipes (parser)
- `recipes/` – per-app YAML, embedded

The relay is stock `frps`. Do not write relay code or fork frp.

## Commands

```sh
go test ./...
go vet ./...
docker build -t farstu-client .
```

Release: push a tag `vX.Y.Z` to GitHub (origin). The workflow publishes
`ghcr.io/viedin/farstu-client:vX.Y.Z` and `:latest` (amd64, arm64), which the
README quick start and the docs pull. An `api/` change then needs
`go get github.com/viedin/farstu-client@vX.Y.Z` in the server repo.

Manual end-to-end testing (VPS, Nextcloud test rig) is described in the
server repo's TESTING.md.

## Hard rules

- Never add an auth/login layer in front of apps. Apps keep their own login.
- We secure what Farstu opens (tunnel, relay, server, device token, client, local page), not the apps: app vulnerabilities, weak passwords and compromised containers are the user's. Exception: where our switch is what makes an app unsafe (README principle 2).
- No recipes for apps that skip their login for "local" addresses: through the client every visitor comes from a private IP.
- Default deny: unknown hosts are rejected; missing or corrupt state means everything is off.
- Toggling an app off is enforced in the client's routing table immediately, not via DNS or frp.
- TLS terminates only in the client. Private keys never leave the home server.
- The device token only travels over verified TLS: system roots for the API, the pinned relay cert for frps. frpc without `trustedCaFile` skips verification.
- User-facing errors name the specific cause ("App is not running", "Tunnel is down").
- Nothing private in this repo: no server code, hostnames of our infrastructure beyond the public API default, credentials or test-rig details.

## Conventions

- `CGO_ENABLED=0`; use `modernc.org/sqlite` or bbolt.
- frpc runs as a pinned subprocess; don't embed frp's Go packages.
- Pass `context.Context` to all I/O; every goroutine and the frpc process must shut down cleanly.
- Wrap errors with `%w`; log with `log/slog`; never log bodies, tokens or keys.
- Ask before adding dependencies beyond: frp (binary), `pires/go-proxyproto`, `caddyserver/certmagic`, `libdns/libdns`, `docker/docker/client`, `golang.org/x/time/rate`, `go.yaml.in/yaml/v3`, `go.etcd.io/bbolt`. They must be permissively licensed (Apache-2.0 compatible).
- Tests are required for Host routing, PROXY v2 parsing, toggle-off and guardrails.
