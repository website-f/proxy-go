# jobcloud

A single-binary reverse proxy + admin UI for hosting many projects on one VPS.

Stand up `jobcloud` once. From then on, every new project is "publish to `127.0.0.1:<port>` + add one row in the admin UI" — no nginx files to edit, no certbot to run, no per-project compose changes.

## Why this exists

You have a VPS. You want to host project A on `a.com`, project B on `b.com`, project C on `c.com`. The standard answers are:

- **nginx + certbot per project** → fragile, easy to break, lots of YAML/conf to keep in sync
- **Traefik with labels** → forces you to touch every project's compose
- **Nginx Proxy Manager (NPM)** → great, but the UI is dated and the metrics layer is shallow

`jobcloud` is what NPM would be if you rebuilt it today: dark-themed live dashboard with per-site request rates and p95 latencies, a clean YAML-driven config you can version-control, hot-reload on save, and a 12 MB Go binary that uses ~30 MB RAM idle.

## Features

- **Reverse proxy** with round-robin load balancing across multiple upstreams per site.
- **Auto Let's Encrypt** via [`certmagic`](https://github.com/caddyserver/certmagic) — same library Caddy uses in production. Auto-renewal, OCSP stapling, ACME http-01.
- **Live traffic dashboard** — per-site requests/min, bytes out, p50/p95/p99 latency, error rate. Polls every 3s.
- **Per-site rate limiting** — token bucket, per source IP. Configurable rps + burst.
- **Common-exploit pre-filter** — drops `/wp-admin`, `/.env`, scanner UAs, etc. before they reach your upstream.
- **WebSocket** + HTTP/2 upgrade passthrough.
- **Hot reload** — drop a YAML file in `sites/`, save in the UI, or `git pull` your sites repo. Routing updates within 200ms. No restart.
- **Bcrypt admin auth** with signed-cookie sessions. Admin UI binds to `127.0.0.1` only — reach it via SSH tunnel.
- **Portable** — back up the `/opt/jobcloud/` folder (compose + sites + certs + data), drop on a new VPS, done.

## Quick start

```bash
# On the VPS — one time
sudo mkdir -p /opt/jobcloud && sudo chown $USER:$USER /opt/jobcloud
cd /opt/jobcloud
# Copy the jobcloud/ folder of this repo here, OR `git clone <repo> .`
cd jobcloud

# Generate a bcrypt hash for your admin password
docker compose build
docker compose run --rm jobcloud hash 'your-strong-password-here'
# Copy the output

cp config.example.yml config.yml
nano config.yml
#   - set acme_email
#   - paste the bcrypt hash into admins[0].password_hash

docker compose up -d
docker compose logs -f jobcloud
```

From your laptop:

```bash
# Tunnel to the admin UI (never exposed publicly)
ssh -L 8090:127.0.0.1:8090 user@your-vps-ip
```

Open `http://localhost:8090`, sign in, click **Add site**.

## Adding a site

**Via the UI:** Click "+ Add site", fill out the form, save. The new site is live within ~200ms; if `TLS auto` is on, the cert arrives ~10–30s later (depends on Let's Encrypt).

**Via a YAML file:** drop one in `sites/<domain>.yml` — `jobcloud` picks it up automatically.

```yaml
domain: newdomain.com
aliases: [www.newdomain.com]
upstreams:
  - 127.0.0.1:8082
  - 127.0.0.1:8083    # add more lines for load balancing
enabled: true
tls:
  auto: true
http_to_https: true
websocket: true
block_common_exploits: true
rate_limit:
  enabled: true
  rps: 30
  burst: 60
```

DNS for the domain must already point at the VPS. Test with `dig +short newdomain.com`.

## Project-side requirements

A project doesn't need any `jobcloud` awareness. Just publish its HTTP service on a loopback port:

```yaml
# in any project's docker-compose.yml
services:
  web:
    # ...
    ports:
      - "127.0.0.1:8082:8000"   # 8082 on host, only reachable via jobcloud
```

Then add `127.0.0.1:8082` as an upstream in jobcloud. Done.

> ⚠ If you bind `0.0.0.0:8082` instead, the port is publicly reachable, bypassing jobcloud. Always use `127.0.0.1:`.

## Layout

```
/opt/jobcloud/jobcloud/
├── docker-compose.yml        # what runs jobcloud itself
├── config.yml                # global config (admins, ACME email)
├── sites/
│   ├── newdomain.com.yml     # per-site config files
│   └── anothersite.org.yml
├── certs/                    # certmagic stores LE certs here
└── data/                     # session secret, future SQLite, etc.
```

Back up `config.yml`, `sites/`, `certs/`, `data/`. That's everything.

## Architecture

```
                          ┌──────────────────────────────┐
   :80   ─── ACME ───────►│                              │
   :443  ─── TLS ────────►│  jobcloud (Go binary)        │
                          │                              │
   127.0.0.1:8090 ───────►│   ┌──────────────┐           │
   (admin UI via SSH)     │   │ admin UI     │           │
                          │   │ + login      │           │
                          │   └──────────────┘           │
                          │   ┌──────────────┐           │
                          │   │ reverse proxy│──┐        │
                          │   └──────────────┘  │        │
                          │   ┌──────────────┐  │        │
                          │   │ ACME/certmagic│ │        │
                          │   └──────────────┘  │        │
                          └─────────────────────┼────────┘
                                                ▼
                                127.0.0.1:8082 (project A — Django)
                                127.0.0.1:8083 (project B — Node)
                                127.0.0.1:8084 (project C — ...)
```

Reads `sites/*.yml` at startup. `fsnotify`-watches the dir; any change triggers a debounced (200ms) reload that rebuilds the in-memory routing table atomically, syncs ACME with the new domain set, and prunes metrics for deleted sites.

The hot path (one HTTP request):

```
client → ServeHTTP → Host lookup (O(1) map) → rate limit → reverse proxy → upstream
                                                                   │
                                                                   ▼
                                                          metric.Record(status, bytes, latency)
```

`httputil.ReverseProxy` does the byte-shoveling. Upstream pool round-robins; `http.Transport` is shared so TCP keep-alive amortizes across requests.

## Operations

**Logs:**
```bash
docker compose logs -f jobcloud
```

**Restart with zero downtime-ish** (existing connections drain for 30s, new ones queue ≤ a few hundred ms while listeners come back):
```bash
docker compose restart jobcloud
```

**Rebuild after a code change:**
```bash
docker compose build && docker compose up -d
```

**Add a new admin:**
```bash
docker compose run --rm jobcloud hash 'newpassword'
# add another `admins:` entry in config.yml
docker compose restart jobcloud
```

**Move to a new VPS:**
```bash
# Old VPS
tar czf jobcloud-backup.tgz -C /opt/jobcloud jobcloud
scp jobcloud-backup.tgz user@new-vps:/tmp/
# New VPS
sudo mkdir -p /opt/jobcloud && cd /opt/jobcloud
tar xzf /tmp/jobcloud-backup.tgz
cd jobcloud
docker compose up -d
# Repoint DNS A records → done
```

## Security notes

- Admin UI is bound to `127.0.0.1:8090`. **Never expose port 8090 publicly.** Reach it via SSH tunnel.
- Bcrypt password hashes (cost 10). Use a long unique password.
- Session cookies are HMAC-signed with a 48-byte secret stored at `data/secret.key`. Cookies are `HttpOnly`, `SameSite=Lax`, `Secure` when over HTTPS, 24h expiry.
- `trust_forwarded_headers: false` by default — `jobcloud` uses the direct peer IP for rate limiting + logs. Only flip true if you put another trusted L7 proxy in front (e.g. Cloudflare).
- The container runs as uid 1001 (non-root). Drops all capabilities except `NET_BIND_SERVICE`.

## Limitations / non-goals

- **No HTTP/3 / QUIC.** stdlib `net/http` doesn't natively serve HTTP/3 yet. Add later via a quic-go layer if needed.
- **No long-term metric storage.** Per-site stats are kept in a 60-second sliding window + 1024-sample latency ring. For real history, point a Prometheus exporter at jobcloud — not implemented yet.
- **No multi-node clustering.** Single instance per VPS. For high availability, run two VPSes behind DNS round-robin or a real load balancer.
- **No DNS-01 challenges.** Only HTTP-01 for now. Means certs can only issue for domains pointed at the VPS's public IP at issuance time.

## License

MIT.
