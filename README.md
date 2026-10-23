# <img src="internal/ui/icon.svg" width="32" alt=""> Farstu Client

**Farstu** (Swedish for a house's entrance hall: the space between your home
and the outside world).

A remote-access layer for self-hosters. Flip a switch next to an app (e.g.
Immich) running on your home server, and it becomes reachable at
`<app>.<user>.farstu.app` from anywhere, including from the app's own native
phone app. No domain to buy, no DNS, no router config, no terminal.

This repository is the **Farstu Client**: the part that runs on your home
server. It is open source so you can see exactly what runs next to your apps,
what it can reach, and that your TLS keys never leave your machine. The Farstu
Server (accounts, relay authorization, DNS, billing) is run by us and is not
part of this repository; `api/` is the full protocol between the two.

## Quick start

On the server that runs your apps (Linux with Docker), save this as
`compose.yaml`:

```yaml
services:
  farstu-client:
    image: ghcr.io/viedin/farstu-client:latest
    network_mode: host # reaches your apps; the local page is on :7080
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock # finds your apps
      - farstu-client:/data                       # registration, which apps are on, activity
    restart: unless-stopped

volumes:
  farstu-client:
```

```sh
docker compose up -d
```

1. Open `http://<your server's IP>:7080` from your home network.
2. Click **Connect to Farstu**, sign in or create an account, and approve
   the code shown on the page (it is also in `docker compose logs`).
3. Back on the local page, turn an app on. Once its status says
   "Reachable from the internet", open it at `https://<app>.<user>.farstu.app`,
   also from its phone app.

Apps start off, and turning one off takes effect at once. The local page only
answers from your home network.

Keep `network_mode: host`. On a Docker network instead (`ports: ["7080:7080"]`),
Docker's port forwarding hides who is asking for IPv6 and `localhost`: anyone
on the internet could reach the page over IPv6. So there, the page only
answers at `http://<your server's IPv4>:7080` from another device on your home
network, and refuses IPv6 and `localhost`.

## Product principles

1. **No extra login layer.** We rely on each app's own login and user
   management. We never put an auth wall in front of apps; it breaks native
   phone apps and forces double accounts.
2. **We secure what we open, not the apps.** Farstu owns the tunnel, relay,
   server, device token, client and its local page. You own your apps,
   passwords, containers and LAN. The exception is where flipping our switch
   is what makes an app unsafe: we refuse to expose a fresh install whose
   first visitor becomes admin, we rate limit sign-ins at the app's login
   path, and we don't support apps that skip their login for "local"
   addresses (through us, every visitor looks local). Containers without a
   recipe can be turned on too, as unsupported apps, under a warning that
   none of this is checked for them.
3. **Default deny.** Unknown hostnames are rejected. Missing/corrupt state means
   everything is OFF. Client offline means nothing is reachable.
4. **The off switch must be instant and trustworthy.** Enforcement is local in
   the client, never dependent on DNS TTLs or the relay.
5. **We never hold your TLS keys.** TLS terminates on your home server. The
   relay only sees encrypted bytes.

## How it works

```
visitor ──TLS──▶ relay (stock frps, routes on SNI, never decrypts)
             ──▶ frpc (supervised by Farstu Client)
             ──▶ Farstu Client TLS listener 127.0.0.1:8443
                 (PROXY v2 → real visitor IP, TLS termination, guardrails)
             ──▶ app upstream <container IP>:<port>
```

- **Registration** uses the OAuth 2.0 Device Authorization Grant (RFC 8628):
  the local page shows a code, you approve it at our site. The client gets a
  subdomain, a device token (the server keeps only its hash), the relay
  address and the relay's certificate, saved in `/data/device.json` (0600).
  The token only travels over verified TLS: system roots for the API, the
  pinned relay cert for frpc.
  If the server answers 401 (disconnected on the account page, replaced by
  another server, account deleted), the client forgets the device and which
  apps were on, and the local page asks to connect again.
- **Discovery** runs over the Docker socket (Engine API, following container
  start/die events), matching `recipes/*.yaml` against image names. Other
  containers with a TCP port are listed as unsupported, named after the
  container, at that port (80 if several, else none is guessed).
- **Updates:** 10 minutes after start and every 6 hours, Docker asks the
  registry of each listed app's image, anonymously, for its tag's current
  digest; the local page marks apps whose image has a newer one. It never
  pulls or restarts anything.
- **Tunnel:** a pinned [frp](https://github.com/fatedier/frp) `frpc` runs as
  a subprocess with one `https` proxy and PROXY v2: for `*.<user>.<ourdomain>`
  on a paid plan, for the hosts of the apps that are on on the free plan (the
  relay enforces the free plan's app limit through it).
- **TLS:** one wildcard certificate per user from Let's Encrypt via DNS-01
  ([certmagic](https://github.com/caddyserver/certmagic)); the server only
  writes the TXT record. The private key stays on your server. One wildcard
  also keeps app hostnames out of Certificate Transparency logs.
- **Routing** by Host header to apps that are switched on. Turning an app off
  removes it from the routing table and closes its open connections at once.
- **Guardrails:** sign-in attempts at each recipe's login paths are rate
  limited per visitor.
- **Local page** (`:7080`) with a switch and status per app; it only answers
  visitors from private addresses.

## Layout

```
cmd/client/      main
api/             request/response types shared with Farstu Server
internal/        control (server API, TXT records), discover, frpc, proxy, ui, recipes (parser)
recipes/         per-app YAML, embedded in the binary
```

## Build

```sh
go test ./...
docker build -t farstu-client .
```

The image bundles a pinned `frpc` (checked against the release checksums)
and frp's license under `/licenses/frp`.

## App recipes

One YAML file per supported app in `recipes/`. Recipes drive app detection,
proxy quirks and guardrails. Example:

```yaml
app: immich
match: { images: ["ghcr.io/immich-app/immich-server"] }
upstream_port: 2283
setup_done: { path: /api/server/config, field: isInitialized } # no admin yet: can't be turned on
login:                               # sign-in attempts rate limited per visitor, all paths together
  - { method: POST, path: /api/auth/login }
```

`setup_done` can also POST (`method`), or wait for a status instead of a
field (`status: 401`: the API wants a login once there is a user), with a
`message` saying what to do. `field` is a dotted JSON path (`data.account`,
`setup.0.status`, `.` for the whole answer); `unset` lists paths that must be
missing, false or empty (Open WebUI's `onboarding`); `contains` wants a text in
the answer (Navidrome's HTML). Apps without accounts that keep nothing on the
server (Excalidraw) say `no_login: true` instead. Every sign-in path counts:
an app that also takes passwords elsewhere (Memos' REST and Connect APIs)
lists each one.

Before adding a recipe, check the app doesn't relax its login for "local" or
"trusted network" addresses (e.g. Sonarr's "Disabled for Local Addresses",
Home Assistant's `trusted_networks`): apps see every visitor as coming from
the client's private address, so that would let the internet in without a
password.

## Conventions

- `gofmt`, `go vet`, `go test ./...` must pass. `CGO_ENABLED=0`.
- Prefer the standard library; add dependencies only when they save real work.
- Pass `context.Context` through anything that does I/O; every goroutine and
  the frpc subprocess must shut down cleanly.
- Wrap errors with `%w`. Log with `log/slog`; never log bodies, tokens or keys.
- Tests for Host routing, PROXY header handling, toggle-off, guardrails and
  recipes are required.

## License

Apache License 2.0, see [LICENSE](LICENSE). "Farstu" is not licensed for use
by forks (Apache 2.0, section 6).
