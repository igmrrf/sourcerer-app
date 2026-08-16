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
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

Open <http://localhost:8080>. The dev overlay serves plain HTTP and skips the
TLS proxy, so your GitHub OAuth app's callback URL should be
`http://localhost:8080/auth/github/callback`.

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
| `PUBLIC_BASE_URL` | no | derived from request | Absolute origin used in README embed snippets, e.g. `https://sourcerer.example` |
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

### Log viewer

Dozzle is behind the `debug` profile and bound to `127.0.0.1` because it mounts
the Docker socket and has no authentication of its own:

```bash
docker compose --profile debug up -d dozzle
ssh -L 9999:localhost:9999 your.host   # then open http://localhost:9999
```

## Development

The dev overlay (`docker-compose.dev.yml`) sets `ENV=development`, publishes the
backend on `127.0.0.1:8080`, mounts `backend/templates` for hot reload, and
parks the TLS proxy behind a profile.

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
