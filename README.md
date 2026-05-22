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

## Pre-flight checklist (read before deploying)

`jobcloud` runs with `network_mode: host` and binds 80/443 + 8090 directly on the VPS. Before bringing it up, make sure:

1. **No other process holds 80/443.** Common offenders: an existing nginx/Caddy/NPM container, a system caddy/nginx package, or a previous proxy you forgot about.
   ```bash
   ss -tlnp | grep -E ':80\b|:443\b'
   ```
   If anything is there, stop it (`docker stop <name>` or `systemctl stop <unit>`) and confirm the lines disappear before continuing.

2. **Docker installed**, user in the `docker` group (or run as root).

3. **DNS for at least one domain** already pointing at the VPS public IP — needed so Let's Encrypt can issue a cert on first use. (Other sites can come later.)

## Quick start (new VPS, from scratch)

```bash
# 1. Allow non-root processes to bind 80/443 (jobcloud runs as uid 1001).
#    Without this, the container starts but the listeners fail with
#    "permission denied" and the proxy crash-loops.
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-jobcloud.conf
sudo sysctl --system

# 2. Clone (or copy) jobcloud into /opt
sudo mkdir -p /opt/jobcloud && sudo chown $USER:$USER /opt/jobcloud
cd /opt/jobcloud
git clone <this-repo> .            # or scp the jobcloud/ folder here
cd jobcloud                        # if the repo nests it; otherwise stay put

# 3. Create the runtime dirs and hand them to uid 1001 (the container user).
#    Without this, the admin UI shows "permission denied" when you save a site.
mkdir -p sites certs data
sudo chown -R 1001:1001 sites certs data

# 4. Build the image so the hash subcommand is available.
docker compose build

# 5. Generate a bcrypt admin password hash.
docker compose run --rm jobcloud hash 'your-strong-password-here'
#    Copy the $2a$... line from the output.

# 6. Create config.yml.
cp config.example.yml config.yml
nano config.yml
#    - set acme_email to a real email (LE renewal notices)
#    - paste the bcrypt hash into admins[0].password_hash
#    - leave admin_addr as "127.0.0.1:8090"  (under host networking this
#      binds to the HOST's loopback, exactly what we want — admin UI
#      reachable only via SSH tunnel)
#    - leave http_addr/https_addr as ":80" / ":443"

# 7. Start.
docker compose up -d
docker compose logs -f jobcloud
```

You should see three `... listener up` lines (`admin UI`, `HTTP`, `HTTPS`) and no `permission denied`. Ctrl+C exits the log follow; the container keeps running.

## Reaching the admin UI

The admin UI is bound to `127.0.0.1:8090` on the VPS — **never publicly exposed**. From your laptop:

```bash
ssh -L 8090:127.0.0.1:8090 user@your-vps-ip
```

Leave that terminal open. In your browser go to `http://127.0.0.1:8090` (NOT the VPS IP — that will refuse). Log in with the admin credentials from `config.yml`, click **+ Add site**.

> If the browser shows "connection refused", the tunnel is dead (SSH session closed) or jobcloud isn't listening on 8090 on the VPS. Verify VPS-side with `ss -tlnp | grep 8090` — should show jobcloud bound on `127.0.0.1:8090`.

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

A project doesn't need any `jobcloud` awareness. Just publish its HTTP service on a **loopback** port on the host:

```yaml
# in any project's docker-compose.yml
services:
  web:
    # ...
    ports:
      - "127.0.0.1:8082:8000"   # 8082 on host, only reachable via jobcloud
```

Then add `127.0.0.1:8082` as an upstream in jobcloud. Done.

> ⚠ If you bind `0.0.0.0:8082` (or just `"8082:8000"`), the port is publicly reachable, bypassing jobcloud. Always prefix with `127.0.0.1:`.

### Picking a free host port

Two projects can't bind the same host port. Container-internal port can repeat — only the host side must be unique.

```bash
ss -tlnp | grep '127.0.0.1:' | awk '{print $4}' | sort -u
```

Lists every loopback port already in use. Pick anything not in the list. A simple convention: assign each project a range, e.g.:

| Project   | Port range |
|-----------|------------|
| fitclaw   | 8000-8099  |
| jobapp    | 8100-8199  |
| next one  | 8200-8299  |

### Per-project deployment checklist

1. DNS A-record for the domain → VPS public IP (do this first; LE needs it).
2. `cd /opt && git clone <repo> <project>`
3. Open the project's `docker-compose.yml`, confirm each public-facing service has `ports: "127.0.0.1:<free-port>:<container-port>"`. Edit if needed.
4. `docker compose up -d`
5. In jobcloud admin UI → **+ Add site** → domain + `127.0.0.1:<free-port>` → Save.
6. Wait ~10–30s for Let's Encrypt to issue the cert (watch `docker compose logs -f jobcloud`). Done.

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

The migration is just "stop on old, archive, restore on new, repoint DNS." Detail below:

```bash
# ===== On the OLD VPS =====
cd /opt/jobcloud/jobcloud
docker compose down                            # stop jobcloud cleanly

# Archive everything jobcloud needs: compose file, config, sites, certs, data.
# Sudo because certs/data are owned by uid 1001.
sudo tar czf /tmp/jobcloud-backup.tgz \
    docker-compose.yml \
    config.yml \
    sites \
    certs \
    data

scp /tmp/jobcloud-backup.tgz user@new-vps:/tmp/

# ===== On the NEW VPS =====
# 1. Pre-flight: nothing else holding 80/443.
ss -tlnp | grep -E ':80\b|:443\b'

# 2. Lower the unprivileged-port floor (same as fresh install).
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-jobcloud.conf
sudo sysctl --system

# 3. Restore.
sudo mkdir -p /opt/jobcloud/jobcloud
sudo chown $USER:$USER /opt/jobcloud /opt/jobcloud/jobcloud
cd /opt/jobcloud/jobcloud
tar xzf /tmp/jobcloud-backup.tgz

# 4. Re-apply ownership (tar preserves it, but only if extracted as root).
sudo chown -R 1001:1001 sites certs data

# 5. Bring it up.
docker compose up -d
docker compose logs -f jobcloud

# 6. Repoint DNS A records → new VPS IP.
#    Existing LE certs in certs/ keep working until they expire; renewals
#    via http-01 will succeed as soon as DNS propagates.
```

If you didn't bring `certs/` over, that's fine — sites with `tls.auto: true` will re-issue automatically on first request after DNS points to the new VPS. Just expect a 10–30s delay on the first HTTPS request per domain.

## Troubleshooting

### `docker compose up` fails with "address already in use"

Something else is bound to 80 or 443. Find it:

```bash
ss -tlnp | grep -E ':80\b|:443\b'
docker ps --format 'table {{.Names}}\t{{.Ports}}' | grep -E '80|443'
```

Typical culprits: a previous `caddy` / `nginx` / `npm` Docker container, or a host-installed nginx package. Stop the offending process (`docker stop <name>` or `systemctl stop <unit>`) and re-run `docker compose up -d`.

### Listeners crash-loop with "bind: permission denied"

The sysctl from step 1 of Quick Start wasn't applied. Re-run:

```bash
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-jobcloud.conf
sudo sysctl --system
docker restart jobcloud-jobcloud-1
```

Then `docker logs jobcloud-jobcloud-1 --tail 20` should show all three listeners up without errors.

### Admin UI: "permission denied" when saving a site

The `sites/` (or `certs/`, `data/`) dir is owned by root on the host, but jobcloud runs as uid 1001 inside the container. Fix:

```bash
sudo chown -R 1001:1001 /opt/jobcloud/jobcloud/sites \
                        /opt/jobcloud/jobcloud/certs \
                        /opt/jobcloud/jobcloud/data
```

No restart needed — just retry **Save** in the UI.

### Site loads but shows "Bad Gateway" (100% errors in dashboard)

jobcloud can't reach the upstream. Cause: the project's service is bound to `0.0.0.0:<port>` or the wrong loopback. Verify the project's `docker-compose.yml` has `ports: "127.0.0.1:<port>:..."` and that the container is running:

```bash
ss -tlnp | grep '127.0.0.1:<your-port>'
curl -I http://127.0.0.1:<your-port>/
```

If the curl works on the VPS but jobcloud still gets "Bad Gateway", confirm jobcloud is on host networking:

```bash
docker inspect jobcloud-jobcloud-1 --format '{{.HostConfig.NetworkMode}}'
# Should print: host
```

### SSH tunnel works once, then "channel 3: open failed: Connection refused"

The original SSH session that opened the tunnel disconnected (network blip, idle timeout, etc.). The tunnel is dead even if the terminal is still showing a prompt. Close that terminal completely, open a new one, and re-run `ssh -L 8090:127.0.0.1:8090 user@vps`.

### Admin UI loads from VPS public IP? It shouldn't

If `http://<vps-ip>:8090` works from the public internet, your `config.yml` has `admin_addr: ":8090"` (binds all interfaces). Change to `admin_addr: "127.0.0.1:8090"` and `docker compose restart jobcloud`. The admin UI MUST be reachable only via SSH tunnel.

### `Welcome` HTML appears for the wrong domain

You hit the IP directly or hit a domain that isn't configured. Add a site for that domain, or — for unrouted domains — that's expected behaviour (no default backend by design).

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
