# Sourcerer

Turns your git history into an engineering profile: language breakdown, commit
habits, recognized libraries, per-repository Hall of Fame, and embeddable SVG
badges.

The stack is three parts:

| Component | Path | Role |
|---|---|---|
| Go web app + ingestion API | `backend/` | OAuth, dashboard, public profiles, SVG badges, protobuf ingestion endpoints |
| Kotlin extractor CLI | `cli/` | Walks a cloned repository and posts commits, facts and authors to the API |
| nginx reverse proxy | `nginx/` | TLS termination, rate limiting, ACME challenge |

PostgreSQL 18 stores everything. A background worker in the backend clones each
repository and runs the extractor against it.

## Quick start (local development)

```bash
cp .env.example .env   # then fill in the values below
./build_cli.sh         # builds cli/build/libs/sourcerer-app.jar

# Option A: Standalone local development stack (includes Dozzle log viewer on :9999)
docker compose -f docker-compose.local.yml up -d --build

# Option B: Standard dev overlay (HTTP dev loop on :8080)
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

- Web App: <http://localhost:8080> (GitHub OAuth callback URL: `http://localhost:8080/auth/github/callback`)
- Log Viewer (Dozzle): <http://localhost:9999>

The backend refuses to start without a usable extractor jar, so
`./build_cli.sh` is not optional. Rebuild the image after rebuilding the jar —
it is copied in, not mounted.

### Local HTTPS

To exercise the TLS path locally, generate a self-signed pair and start the
proxy profile:

```bash
./generate_local_ssl.sh
docker compose up -d --build     # production config: proxy on :80/:443
```

Your browser will warn about the self-signed certificate, and the OAuth
callback URL must then be `https://localhost/auth/github/callback`.

## Configuration

All configuration is environment variables, read from `.env` by Compose.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `POSTGRES_PASSWORD` | yes | — | Database password |
| `POSTGRES_USER` | no | `postgres` | Database user |
| `POSTGRES_DB` | no | `sourcerer` | Database name |
| `GITHUB_CLIENT_ID` | yes | — | GitHub OAuth app client id |
| `GITHUB_CLIENT_SECRET` | yes | — | GitHub OAuth app secret |
| `SESSION_SECRET` | yes | — | HMAC key for session cookies; rotating it invalidates every session |
| `API_INTERNAL_TOKEN` | yes | — | Shared secret between the backend and the extractor CLI |
| `ENV` | no | `production` | `development` enables template hot-reload and relaxes cookie `Secure` |
| `PUBLIC_BASE_URL` | recommended | derived from request | Absolute origin used in README embed snippets, canonical URLs, Open Graph tags and the sitemap, e.g. `https://sourcerer.theldo.com`. Set it in production: without it every reachable hostname produces a different canonical URL. |
| `CORS_ALLOWED_ORIGINS` | no | none | Comma-separated origins allowed to make credentialed cross-origin calls |
| `SERVER_NAME` | no | `_` | nginx `server_name` |
| `SSL_CERTIFICATE` | no | `/etc/nginx/ssl/fullchain.pem` | TLS certificate path inside the proxy container |
| `SSL_CERTIFICATE_KEY` | no | `/etc/nginx/ssl/privkey.pem` | TLS key path inside the proxy container |

`ENV=production` is fail-fast: the backend exits at startup if `SESSION_SECRET`
or `API_INTERNAL_TOKEN` is unset, and the ingestion API rejects every request
rather than falling open.

## Production deployment

1. Point DNS at the host and set `SERVER_NAME` to the domain.
2. Issue a certificate:

   ```bash
   docker compose run --rm --entrypoint certbot certbot certonly \
     --webroot -w /var/www/certbot -d your.domain
   ```

3. Switch the proxy to the issued certificate in `.env`:

   ```
   SERVER_NAME=your.domain
   SSL_CERTIFICATE=/etc/letsencrypt/live/your.domain/fullchain.pem
   SSL_CERTIFICATE_KEY=/etc/letsencrypt/live/your.domain/privkey.pem
   PUBLIC_BASE_URL=https://your.domain
   ```

4. Start with the renewal sidecar:

   ```bash
   docker compose --profile prod up -d
   ```

   The certbot service renews every 12h; the proxy reloads every 6h so renewed
   certificates take effect without a restart.

No config file needs editing between environments — the nginx config is a
template rendered from these variables at container start.

### Log viewer (Dozzle)

Dozzle provides a real-time web UI for container logs.

#### Local Development Access
When running locally via `docker-compose.local.yml`:
- Start the stack: `docker compose -f docker-compose.local.yml up -d`
- Open your browser directly at: <http://localhost:9999>

#### Production Access Flow (SSH Tunnel)
In production (`docker-compose.yml`), Dozzle is placed behind the `debug` profile and bound strictly to the server's private loopback interface (`127.0.0.1:9999`) because it mounts the host Docker socket (`/var/run/docker.sock`, root-equivalent) with no default authentication.

To access the Dozzle UI from your local computer:

1. **Start Dozzle on the remote host**:
   ```bash
   docker compose --profile debug up -d dozzle
   ```

2. **Establish an SSH tunnel from your local machine**:
   ```bash
   ssh -N -L 9999:localhost:9999 user@your.host
   ```
   *(The `-N` flag forwards ports without executing a remote shell).*

3. **Open the Web UI in your local browser**:
   Navigate to <http://localhost:9999>.

```
┌────────────────────────────┐
│ Local Machine (Browser)    │ ───► http://localhost:9999
└─────────────┬──────────────┘
              │ (SSH Tunnel)
              ▼
┌────────────────────────────┐
│ Remote Host (127.0.0.1)    │ ───► Dozzle Container (:8080)
└────────────────────────────┘
```

## Development

The dev overlay (`docker-compose.dev.yml`) sets `ENV=development`, publishes the
backend on `127.0.0.1:8080`, mounts `backend/templates` and `backend/static` for
hot reload, and parks the TLS proxy behind a profile.

`ENV=development` also relaxes the `Secure` flag on session cookies — without
that a browser discards them over plain HTTP and login silently fails — and
lets the ingestion API run without a token. Never set it in production.

To run the backend outside Docker instead:

```bash
cd backend && ./run.sh   # ENV=development
go test ./...
```

`run.sh` expects Postgres on `localhost:5432` and the extractor jar at
`cli/build/libs/sourcerer-app.jar`.

### Rebuilding the extractor

`./build_cli.sh` runs Gradle 4.10.3 / JDK 8 in a container. It invokes
`assemble` rather than `build`: the Spek test dependencies resolve from
`dl.bintray.com`, which is sunset, so compiling the test sources fails. The
production jar has no such dependency.

## Frontend and assets

Every page is a Go template in `backend/templates`, sharing one stylesheet at
`backend/static/app.css`. The stylesheet is embedded in the binary and served
from `/static/app.css?v=<fingerprint>`, where the fingerprint is a hash of the
file — a redeploy invalidates the browser cache without anyone bumping a
version. In `ENV=development` the same route reads from disk instead.

`templates/_shell.html` holds the pieces every page shares: the `<head>` asset
links, the SEO meta block and the sidebar. Pages differ in their content, never
in their chrome.

### Charts

The signed-in overview is a chart surface; the public profile stays a summary,
so the two never read as the same page. `backend/charts.go` computes all the
geometry — paths, gridlines, hit areas, heatmap shading — and the templates
render it as plain SVG. That means the charts are complete in the HTML response,
before any script runs: no chart library, nothing to fetch, and no CSP
exceptions.

The inline script on `index.html` only adds interaction on top: hover and
keyboard readouts, and switching between commits, lines added, lines deleted and
net lines. All four series are in the DOM at once and hidden with CSS, so
switching costs no request. The range switcher (30 days / 90 days / 12 months /
all time) does refetch, writing its choice to a hidden input outside the swapped
panel so the 30-second background refresh keeps whatever was picked.

Buckets are computed in UTC, and the axis walks the calendar rather than the
query result, so a week with no commits stays visible as a gap.

### Repositories

`/repositories` is a signed-in page of its own rather than a card on the
overview: the churn chart, then one card per repository carrying its name, a
hall of fame link and the coding habit that repository pulls out of you.

The habit is computed per repository in `backend/repositories.go`, not reused
from the profile's overall figures — the same person commits at midnight on one
project and at lunchtime on another, and that difference is the point. It uses
the profile's vocabulary (Night Owl, Weekend Warrior) so nothing has to be
relearned, and a repository with no indexed commit times renders the name and
the hall of fame link without inventing a habit.

The icons and the social share card are committed build artifacts, regenerated
from `backend/static/favicon.svg` by:

```bash
cd backend/static && ./generate_assets.sh
```

That script needs ImageMagick 7 and produces `favicon.ico`, `icon-192.png`,
`icon-512.png`, `apple-touch-icon.png` and `og.png`. ImageMagick's built-in SVG
renderer drops stroked paths, which is why the source mark is drawn with filled
rects and circles only.

## Search engines

`/` serves a public landing page to signed-out visitors rather than redirecting
into OAuth, so the site has an indexable front door. Signed-in visitors get the
dashboard at the same URL, which is marked `noindex`.

`/robots.txt` disallows `/dashboard/`, `/auth/`, `/api/`, `/badge/` and
`/hall-of-fame/`, and points at `/sitemap.xml`. The sitemap is generated per
request from the database: the landing and catalog pages, every library, every
repository that has commits, and every published profile.

Data and image routes also carry `X-Robots-Tag: noindex` — badges must keep
loading inside READMEs, but they should never be a search result themselves.
Public profiles are indexable; they show masked email addresses only.

## How ingestion works

1. A user signs in with GitHub. The backend pages through their public
   repositories and enqueues one job per repository.
2. Repositories whose GitHub `pushed_at` is unchanged since the last sync are
   skipped without any network transfer.
3. For the rest the worker runs `git ls-remote` and skips any repository whose
   HEAD commit is already indexed.
4. Otherwise it clones into a temporary directory and runs the extractor, which
   authenticates against `/api/auth` and posts commits, stats, facts and
   authors.

The worker is a single consumer with a 200-job buffer: the extractor keeps its
configuration in one directory beside the jar and rewrites it on every run, so
concurrent runs would clobber each other. Clone and extraction are bounded at
15 and 30 minutes respectively.

## Privacy

Email addresses are never rendered on anonymous surfaces. Hall of Fame pages,
the library leaderboards and the public JSON APIs show a masked form
(`al***@example.com`); links point at opaque profile ids, not addresses. A
profile is only readable by others once its owner has visited it while signed
in, which publishes a cached copy.
