# Sourcerer App — System Documentation

> Comprehensive architecture, design, API reference, data schema, and deployment guide for the Sourcerer platform.

> **⚠ DEPLOYMENT STATUS: NOT PRODUCTION READY.**
> An audit against `develop` @ `51f35cc` recorded 26 open defects, including 2 critical unauthenticated
> write paths. Sections 5 and 7 describe *intended* behavior that the implementation does not enforce.
> Read [§11 Known Issues & Production Readiness Audit](#11-known-issues--production-readiness-audit)
> before deploying or relying on any security claim in this document.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Core Components](#2-core-components)
3. [Ingestion & Processing Pipeline](#3-ingestion--processing-pipeline)
4. [Database Schema & Data Model](#4-database-schema--data-model)
5. [API Reference](#5-api-reference)
6. [Frontend & Visual Profiles](#6-frontend--visual-profiles)
7. [Security & Operational Architecture](#7-security--operational-architecture)
8. [Configuration & Environment Variables](#8-configuration--environment-variables)
9. [Development & Deployment](#9-development--deployment)
10. [Changelog](#10-changelog)
11. [Known Issues & Production Readiness Audit](#11-known-issues--production-readiness-audit)

---

## 1. Architecture Overview

Sourcerer is an automated visual profiling platform for software engineers. It analyzes code repositories, extracts granular commit and language statistics, calculates coding habits, and generates interactive dashboards, shareable public developer profiles, and dynamic SVG badges for GitHub READMEs.

```
                  +-------------------------------------------------------+
                  |                      Client Tier                      |
                  |  (Browser / GitHub Profile Embeds / Headless CLI)     |
                  +---------------------------+---------------------------+
                                              |
                                              | HTTP / HTTPS
                                              v
                  +-------------------------------------------------------+
                  |                  Nginx Reverse Proxy                  |
                  |     (Port 80/443, SSL/TLS, Rate Limiting, Gzip)       |
                  +---------------------------+---------------------------+
                                              |
                                              | Internal Network (:8080)
                                              v
+-----------------------------------------------------------------------------------------+
|                                    Go Backend Service                                    |
|                                                                                         |
|  +---------------------+  +------------------------+  +-------------------------------+ |
|  |   Chi HTTP Router   |  |   GitHub OAuth & Auth  |  |  Ingestion Worker (JobQueue)  | |
|  |  (Throttling, CORS) |  |   (HMAC Signed Cookie) |  |  (Subprocess Git & Java CLI)  | |
|  +----------+----------+  +-----------+------------+  +---------------+---------------+ |
|             |                         |                               |                 |
|             +-------------------------+-------------------------------+                 |
|                                       |                                                 |
|                                       v                                                 |
|                      +----------------------------------+                               |
|                      |  Protobuf Deserialization (API)  |                               |
|                      |  Template Rendering (HTMX / FS)  |                               |
|                      +----------------+-----------------+                               |
+---------------------------------------|-------------------------------------------------+
                                        |
                                        | SQL (database/sql, lib/pq)
                                        v
                  +-------------------------------------------------------+
                  |                  PostgreSQL Database                  |
                  |  (Users, Repos, Commits, Commit Stats, Facts, Authors)|
                  +-------------------------------------------------------+
```

---

## 2. Core Components

### 2.1 Backend Web App & Ingestion API (`backend/`)
- Written in **Go 1.21+** utilizing standard library components (`database/sql`, `log/slog`, `embed.FS`, `crypto/hmac`) and `go-chi/chi/v5`.
- Decodes incoming Protobuf payloads (`application/x-protobuf`) sent by the analysis CLI.
- Manages user sessions via cryptographically signed HMAC-SHA256 cookies.
- Orchestrates asynchronous repository cloning and analysis via a non-blocking worker queue.
- Serves dynamic HTMX dashboard cards, public developer profile pages, and SVG README badges.

### 2.2 Repository Analysis CLI (`cli/`)
- Built in **Java / Kotlin** with Gradle.
- Performs AST-level language parsing, library usage detection (over 1,000 supported libraries), and commit history extraction.
- Supports both interactive console wizard mode and headless daemon mode (`--headless --path <dir>`).
- Encodes analysis outputs into Protocol Buffer messages defined in `sourcerer.proto`.

### 2.3 Reverse Proxy & Gateway (`nginx/`)
- Production-tuned **Nginx** reverse proxy protecting internal services.
- Implements dual rate-limiting zones (`api_limit` at 10 r/s and `general_limit` at 30 r/s).
- Applies enterprise security headers and Gzip compression for static assets and SVGs.
- Configured for SSL/TLS termination on port 443.

### 2.4 Data Store (`PostgreSQL 18`)
- Pinned to `postgres:18-alpine` in `docker-compose.yml`.
- Relational schema storing users, indexed repositories, commit histories, per-language technology statistics, facts/coding habits, author records, and the curated technologies catalog.
- Schema is applied idempotently at application startup. **This is not a versioned migration system** — see [OPS-02](#ops-02--no-versioned-migrations-new-columns-are-absent-on-existing-databases).

---

## 3. Ingestion & Processing Pipeline

The ingestion pipeline transitions local repository analysis into an automated cloud service:

```
[User Login via GitHub OAuth]
            |
            v
[Discover User Repositories via GitHub API]
            |
            v
[Enqueue Repo Jobs into Ingestion Worker (Buffered Channel)]
            |
            v
[Worker Clones Git Repository into Temp Directory]
            |
            v
[Worker Executes Headless CLI Subprocess: java -jar sourcerer-app.jar --headless --path <tmp_dir>]
            |
            v
[CLI Posts Protobuf Batches to Internal Ingestion API (/api/commits, /api/facts, /api/authors)]
            |
            v
[API Deserializes Protobuf & Persists into PostgreSQL in Batched Transactions]
            |
            v
[Worker Cleans Up Temp Directory (`defer os.RemoveAll`)]
            |
            v
[HTMX Dashboard / Public Profile Updated in Real-Time]
```

### Worker Queue Mechanics
- **Capacity**: Channel-buffered job queue (`chan IngestionJob`, 200 capacity, `worker.go:22`).
- **Concurrency**: A single worker goroutine processes repositories sequentially per container to manage system memory and CPU during intensive AST parsing.
- **Overflow**: Non-blocking `EnqueueJob` with `select/default` semantics prevents the OAuth callback from blocking when capacity is saturated. **Overflowing jobs are dropped, not retried** — see [OPS-01](#ops-01--ingestion-worker-is-a-single-consumer-lossy-non-durable-queue).
- **Durability**: The queue is in-memory only. Pending jobs do not survive a restart.
- **Full History Analysis**: Worker executes full depth clones (without shallow clone depth restrictions) to construct complete historical metrics.
- **Smart Sync Skipping**: A two-tier skip was introduced in `5fce484`. Tier-2 (remote `HEAD` rehash lookup) is functional; **Tier-1 (GitHub `pushed_at` comparison) is currently inoperative** — see [BUG-01](#bug-01--reposrepo_url--reposrepo_name-are-never-populated-tier-1-sync-skip-is-dead-code).

---

## 4. Database Schema & Data Model

The schema is defined in `backend/schema.sql` and mapped to Protocol Buffer models (`sourcerer.proto`).

```sql
CREATE TABLE IF NOT EXISTS users (
    email TEXT PRIMARY KEY,
    primary_email BOOLEAN,
    verified BOOLEAN
);

CREATE TABLE IF NOT EXISTS repos (
    rehash TEXT PRIMARY KEY,
    initial_commit_rehash TEXT,
    repo_url TEXT,             -- NOTE: never populated in practice; see BUG-01
    repo_name TEXT,            -- NOTE: never populated in practice; see BUG-01
    last_commit_rehash TEXT,
    last_synced_at BIGINT,
    github_pushed_at TEXT      -- NOTE: never populated in practice; see BUG-01
);

CREATE INDEX IF NOT EXISTS idx_repos_repo_url ON repos(repo_url);
CREATE INDEX IF NOT EXISTS idx_repos_repo_name ON repos(repo_name);
CREATE INDEX IF NOT EXISTS idx_repos_last_commit ON repos(last_commit_rehash);

CREATE TABLE IF NOT EXISTS commits (
    rehash TEXT PRIMARY KEY,
    repo_rehash TEXT REFERENCES repos(rehash),
    author_name TEXT,
    author_email TEXT,
    date BIGINT,
    is_qommit BOOLEAN,
    num_lines_added INTEGER,
    num_lines_deleted INTEGER
);

CREATE INDEX IF NOT EXISTS idx_commits_repo ON commits(repo_rehash);
CREATE INDEX IF NOT EXISTS idx_commits_date ON commits(date);
CREATE INDEX IF NOT EXISTS idx_commits_author_email ON commits(author_email);
CREATE INDEX IF NOT EXISTS idx_commits_repo_date ON commits(repo_rehash, date);
CREATE INDEX IF NOT EXISTS idx_commits_repo_author ON commits(repo_rehash, author_email);

CREATE TABLE IF NOT EXISTS commit_stats (
    id SERIAL PRIMARY KEY,
    commit_rehash TEXT REFERENCES commits(rehash),
    num_lines_added INTEGER,
    num_lines_deleted INTEGER,
    type INTEGER,
    tech TEXT,
    UNIQUE (commit_rehash, tech, type)
);

CREATE INDEX IF NOT EXISTS idx_commit_stats_commit ON commit_stats(commit_rehash);
CREATE INDEX IF NOT EXISTS idx_commit_stats_tech ON commit_stats(tech);

CREATE TABLE IF NOT EXISTS facts (
    id SERIAL PRIMARY KEY,
    repo_rehash TEXT REFERENCES repos(rehash),
    email TEXT,
    code INTEGER,
    key INTEGER,
    value1 TEXT,
    value2 TEXT,
    value3 TEXT,
    value4 TEXT,
    UNIQUE (repo_rehash, email, code, key)
);

CREATE INDEX IF NOT EXISTS idx_facts_repo_email ON facts(repo_rehash, email);
CREATE INDEX IF NOT EXISTS idx_facts_email ON facts(email);
CREATE INDEX IF NOT EXISTS idx_facts_email_code ON facts(email, code);

CREATE TABLE IF NOT EXISTS authors (
    email TEXT PRIMARY KEY,
    name TEXT,
    repo_rehash TEXT REFERENCES repos(rehash)
);

CREATE TABLE IF NOT EXISTS technologies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    lang TEXT NOT NULL,
    category TEXT NOT NULL,
    icon TEXT,
    description TEXT,
    import_tokens TEXT[]
);

CREATE INDEX IF NOT EXISTS idx_technologies_lang ON technologies(lang);
CREATE INDEX IF NOT EXISTS idx_technologies_category ON technologies(category);
```

### Fact Codes Reference

Facts are emitted by the CLI and enumerated in `cli/src/main/kotlin/app/FactCodes.kt`. Each row is keyed
by `(repo_rehash, email, code, key)`. The **`code`** column selects the fact type; the **`key`** column
carries the bucket within that type; **`value1`** carries the measurement. The backend consumes a subset
of these in `computeFacts` (`backend/main.go:701-890`).

| Code | Constant | `key` semantics | `value1` |
|---|---|---|---|
| 1 | `COMMIT_DAY_WEEK` | Day of week, `0` = Monday … `6` = Sunday | Commit count in that bucket |
| 2 | `COMMIT_DAY_TIME` | Hour of day, `0`–`23` | Commit count in that hour |
| 3 / 4 | `LINE_LONGEVITY`, `LINE_LONGEVITY_REPO` | — | Line longevity metrics (not consumed by backend) |
| 5 / 6 | `REPO_DATE_START`, `REPO_DATE_END` | — | First / last contribution date |
| 7 | `REPO_TEAM_SIZE` | — | Contributor count |
| 8 | `COMMIT_LINE_NUM_AVG` | — | Average lines changed per commit |
| 9 | `COMMIT_NUM` | — | Commit count, used to average code 8 across repos |
| 10 | `LINE_LEN_AVG` | — | Average source line length |
| 11 | `LINE_NUM` | — | Line count, used to average code 10 across repos |
| 12 | `COMMIT_NUM_TO_LINE_NUM` | Line-count bucket | Commit count (histogram) |
| 13 | `VARIABLE_NAMING` | `0` = snake_case, `1` = camelCase, `2` = other | Identifier count |
| 14 | `INDENTATION` | `0` = tabs, `1` = spaces | Indentation occurrence count |
| 15 | `COLLEAGUES` | — | Colleague graph data |
| 16 / 17 | `COMMIT_SHARE`, `COMMIT_SHARE_REPO_AVG` | — | Commit share chart data |

Backend-derived traits map as follows: codes 1 and 2 produce the *Night Owl / Early Bird / Daytime
Builder* and *Weekend Warrior / Weekday Master* labels; code 14 produces *Spaces Enthusiast / Tabs
Purist*; code 13 produces *snake_case Fan / camelCase Stylist*; codes 8 and 10 produce the average
commit size and average line length figures. When the `facts` table is empty or lacks time-of-day rows,
`computeFacts` falls back to deriving time and weekday distributions directly from the `commits` table
(`main.go:777-818`).

---

## 5. API Reference

Ingestion endpoints are *intended* to require internal authentication via the `Authorization: Bearer <API_INTERNAL_TOKEN>` header or a valid `Token` cookie.

> **⚠ This is not what the current implementation enforces.** `POST /api/auth` sits outside the
> authenticated route group and hands the token to any caller, and an unvalidated `RemoteAddr` check
> bypasses the middleware entirely. Treat every route below as effectively unauthenticated until
> [SEC-01](#sec-01--post-apiauth-distributes-the-internal-api-token-to-unauthenticated-callers) and
> [SEC-03](#sec-03--authentication-bypass-via-unvalidated-remoteaddr) are resolved.

### 5.1 Protobuf Ingestion Endpoints

| Method | Endpoint | Description | Payload Type |
|---|---|---|---|
| `POST` | `/api/auth` | **Unauthenticated.** Issues the `Token` cookie (see SEC-01) | HTTP Basic (ignored) |
| `GET` | `/api/user` | Fetch active user profile model | Protobuf (`app.User`) |
| `POST` | `/api/user` | Register or update user record | Protobuf (`app.User`) |
| `POST` | `/api/repo` | Register repository metadata | Protobuf (`app.Repo`) |
| `POST` | `/api/commits` | Ingest batch of commits & language stats | Protobuf (`app.CommitGroup`) |
| `DELETE`| `/api/commits` | Purge the listed commits and their stats | Protobuf (`app.CommitGroup`) |
| `POST` | `/api/facts` | Ingest computed code facts & habits | Protobuf (`app.FactGroup`) |
| `POST` | `/api/authors` | Ingest author mappings | Protobuf (`app.AuthorGroup`) |
| `POST` | `/api/distances`| Accepts developer proximity metrics. **No-op** — body is discarded (`api.go:250`) | Protobuf (`app.AuthorDistanceGroup`) |
| `POST` | `/api/process/create` | Allocate an ingestion process id (always returns id `1`) | Protobuf (`app.Process`) |
| `POST` | `/api/process` | Report ingestion progress. **No-op** — body is discarded (`api.go:357`) | Protobuf (`app.Process`) |

`GET /api/user` returns an empty `app.User` and `POST /api/user` is a no-op (`api.go:95-108`); user
records are created by the OAuth flow, not by the CLI.

### 5.2 Authentication & User Routes

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/auth/github/login` | Initiates GitHub OAuth handshake with CSRF token |
| `GET` | `/auth/github/callback` | Validates CSRF state, exchanges token, enqueues repos |
| `GET` | `/auth/logout` | Clears session cookie and redirects to home page |

### 5.3 System & Diagnostics

| Method | Endpoint | Description | Response |
|---|---|---|---|
| `GET` | `/healthz` | Container health probe & database ping | `200 OK ("ok")` / `503 Service Unavailable` |

### 5.4 Public Read Routes

All routes below are served without any authentication or authorization check. See
[SEC-04](#sec-04--contributor-emails-and-per-developer-statistics-are-publicly-readable).

| Method | Endpoint | Description | Response |
|---|---|---|---|
| `GET` | `/` | HTMX dashboard shell; renders the full author/email list | HTML |
| `GET` | `/dashboard/activity?range=` | Commit activity chart for the session user | HTML fragment |
| `GET` | `/dashboard/punchcard` | Weekday-by-hour commit heatmap | HTML fragment |
| `GET` | `/dashboard/languages` | Language donut | HTML fragment |
| `GET` | `/dashboard/facts` | Behavioral traits | HTML fragment |
| `GET` | `/dashboard/libraries` | Recognized libraries | HTML fragment |
| `GET` | `/repositories` | Repository cards: name, hall of fame link, per-repository habits | HTML |
| `GET` | `/dashboard/repo-cards` | The repository cards themselves | HTML fragment |
| `GET` | `/p/{email}`, `/u/{username}` | Public developer profile page | HTML |
| `GET` | `/badge/{identifier}[.svg]` | Dynamic profile badge | `image/svg+xml`, `max-age=1800` |
| `GET` | `/r/{repo}`, `/hall-of-fame/{repo}` | Repository Hall of Fame showcase | HTML |
| `GET` | `/r/{repo}.svg`, `/hall-of-fame/{repo}.svg` | Hall of Fame README badge | `image/svg+xml`, `max-age=1800` |
| `GET` | `/api/hall-of-fame/{repo}` | Hall of Fame data | JSON |
| `GET` | `/libraries`, `/libraries/{tech}` | Awesome Libraries catalog & detail | HTML |
| `GET` | `/api/libraries`, `/api/libraries/{tech}` | Awesome Libraries catalog & detail | JSON |

---

## 6. Frontend & Visual Profiles

The frontend is constructed with Go templates, HTMX, and modern CSS without heavy client-side JavaScript frameworks.

### 6.1 Interactive HTMX Dashboard (`/`)
- **Commit Activity (`/dashboard/activity`)**: Server-rendered SVG time series with metric (commits / added / deleted / net) and range (30 days / 90 days / 12 months / all time) switchers.
- **Punchcard (`/dashboard/punchcard`)**: Weekday-by-hour heatmap of commit times.
- **Language Distribution (`/dashboard/languages`)**: Donut with a hover-linked legend.
- **Coding Habits & Facts (`/dashboard/facts`)**: Visual breakdown of developer traits (Night Owl / Early Bird, Weekday Warrior / Weekend Hacker, Spaces vs Tabs, CamelCase vs Snake_case, Average Commit Size).
- **Auto-Refresh**: Smooth background polling via HTMX (`hx-trigger="every 30s"`).

### 6.1.1 Repositories (`/repositories`)
One card per repository: the name recorded at ingestion (falling back to the rehash), the rehash, a hall of fame link, and the coding habit computed from that repository's commits alone.

### 6.2 Public Developer Profiles (`/u/{username}` & `/p/{email}`)
- Publicly accessible, responsive profile pages for sharing portfolio stats.
- Displays contributor hero headers, verified badges, aggregate statistics, top languages, repository history, and coding habit traits.
- Includes a copyable Markdown / HTML snippet for embedding dynamic profile badges directly on GitHub profile READMEs.

### 6.3 Dynamic SVG README Badges (`/badge/{email}.svg`)
- Generates pixel-perfect SVG cards dynamically rendered on the server.
- Embeddable in GitHub READMEs: `![Sourcerer Profile](https://your-domain.com/badge/user@example.com.svg)`
- Includes HTTP cache headers (`Cache-Control: public, max-age=1800`) for GitHub CDN optimization.

### 6.4 Template Rendering Pipeline
- **Development Mode (`ENV=development`)**: Dynamic hot-reloading from disk via `template.ParseGlob("templates/*.html")` for zero-restart template iteration.
- **Production Mode (`ENV=production`)**: High-performance in-memory execution using Go 1.16+ embedded files (`embed.FS`).

---

## 7. Security & Operational Architecture

> **⚠ Sections 7.1–7.4 describe the intended security architecture. Several controls are not enforced as
> written.** Before relying on anything in this section, read
> [§11.2 Security Defects](#112-security-defects).

### 7.1 Cryptographic Session Management
- Sessions use signed HMAC-SHA256 cookies (`session=<base64_payload>.<hex_hmac>`).
- Cookies are configured with `HttpOnly`, `SameSite=Lax`, and `Path=/`.
- Cryptographic verification uses constant-time comparison (`hmac.Equal`) to eliminate timing attacks.
- **Gap:** the `Secure` attribute is not set and no TLS listener is active — see [SEC-05](#sec-05--no-transport-encryption-session-cookie-lacks-secure).
- **Gap:** a missing `SESSION_SECRET` is auto-generated rather than treated as fatal — see [OPS-04](#ops-04--configuration-failures-are-fail-open-rather-than-fail-fast).

### 7.2 OAuth CSRF Protection
- `/auth/github/login` generates a cryptographically random 32-byte state token stored in a short-lived `oauth_state` cookie.
- `/auth/github/callback` verifies the returned state parameter against the cookie using constant-time comparison before exchanging tokens.

### 7.3 Ingestion API Authentication
- *Intended:* `apiAuthMiddleware` requires every `/api/*` request to present a matching `API_INTERNAL_TOKEN` via `Authorization: Bearer` or the `Token` cookie.
- **Not enforced.** Three defects defeat this control:
  - `POST /api/auth` is registered outside the protected group and returns the token to anyone — [SEC-01](#sec-01--post-apiauth-distributes-the-internal-api-token-to-unauthenticated-callers).
  - A `RemoteAddr` loopback prefix check bypasses the middleware — [SEC-03](#sec-03--authentication-bypass-via-unvalidated-remoteaddr).
  - An unset `API_INTERNAL_TOKEN` disables the middleware entirely — [OPS-04](#ops-04--configuration-failures-are-fail-open-rather-than-fail-fast).

### 7.4 Traffic Throttling & Rate Limiting
- **Edge Rate Limiting**: Nginx enforces rate limit zones (`general_limit: 30r/s` burst 40, `api_limit: 10r/s` burst 20). This is the only true rate limiting in the stack, and it is bypassed on any path that reaches the backend directly.
- **Application Concurrency Limiting**: Chi middleware `middleware.ThrottleBacklog(100, 50, 5s)` caps *in-flight requests*, not request rate — see [OPS-07](#ops-07--throttlebacklog-is-a-concurrency-limiter-not-a-rate-limiter).

### 7.5 Cross-Origin Policy
- The CORS middleware (`main.go:217-232`) reflects the caller's `Origin` and sets `Access-Control-Allow-Credentials: true`. **This is an open cross-origin policy, not a restriction** — see [SEC-02](#sec-02--reflected-cors-origin-combined-with-credentialed-requests).

### 7.6 Structured Logging & Graceful Shutdown
- Standard library `log/slog` structured logging formats logs as clean text in development and structured JSON in production.
- Handles `SIGINT` and `SIGTERM` signals with a 5-second graceful shutdown timeout to allow active database transactions and HTTP requests to complete.
- **Gap:** the shutdown path covers the HTTP server only. In-flight ingestion jobs are killed without draining — see [OPS-12](#ops-12--in-flight-ingestion-is-terminated-without-draining-on-shutdown).

---

## 8. Configuration & Environment Variables

All settings are configured via environment variables. See `.env.example` for reference:

| Variable | Required | Default | Description |
|---|---|---|---|
| `ENV` | No | `production` | Environment mode (`development` or `production`) |
| `PORT` | No | `8080` | Internal HTTP listening port for Go backend |
| `DATABASE_URL` | Yes | - | PostgreSQL connection URI |
| `POSTGRES_USER` | Yes | `postgres` | Database username for Docker container |
| `POSTGRES_PASSWORD` | Yes | - | Database password |
| `POSTGRES_DB` | Yes | `sourcerer` | Database name |
| `GITHUB_CLIENT_ID` | Yes | - | GitHub OAuth App Client ID |
| `GITHUB_CLIENT_SECRET`| Yes | - | GitHub OAuth App Client Secret |
| `SESSION_SECRET` | Yes | *(random per boot)* | 32-byte hex secret for signing session cookies. If unset, a random secret is generated and **all sessions are invalidated on every restart**. Should be fatal in production — [OPS-04](#ops-04--configuration-failures-are-fail-open-rather-than-fail-fast) |
| `API_INTERNAL_TOKEN` | Yes | *(none — auth disabled)* | Secret token securing `/api/*` ingestion routes. **If unset, `apiAuthMiddleware` permits all requests** — [OPS-04](#ops-04--configuration-failures-are-fail-open-rather-than-fail-fast) |

None of the above are validated at startup. A deployment missing any of them starts successfully and
fails later at request time.

---

## 9. Development & Deployment

### 9.1 Local Development

1. **Start PostgreSQL**:
   ```bash
   docker compose up -d db
   ```

2. **Run Backend with Auto-Migration and Live Reloading**:
   ```bash
   cd backend
   ENV=development DATABASE_URL="host=localhost user=postgres password=postgres dbname=sourcerer sslmode=disable" go run .
   ```

3. **Run Unit Tests**:
   ```bash
   cd backend
   go test -v ./...
   ```

### 9.2 Full Stack Deployment with Docker Compose

> **⚠ This procedure does not currently produce a deployable stack.** Step 3 invokes a Gradle wrapper that
> is not present in the repository ([OPS-09](#ops-09--cli-build-is-not-reproducible-and-the-documented-build-command-does-not-exist)),
> and the backend container bind-mounts the jar that step is supposed to produce, silently creating a
> directory in its place if the build was skipped ([OPS-03](#ops-03--backend-depends-on-a-host-built-jar-via-bind-mount-with-no-startup-validation)).
> Step 5 also exposes the stack over plaintext HTTP only
> ([SEC-05](#sec-05--no-transport-encryption-session-cookie-lacks-secure)). Resolve
> [§11.1's minimum set](#111-summary) before deploying anywhere reachable.

1. **Configure Environment**:
   ```bash
   cp .env.example .env
   # Edit .env and supply your credentials and generated secrets
   ```

2. **Generate Cryptographic Secrets**:
   ```bash
   openssl rand -hex 32 # SESSION_SECRET
   openssl rand -hex 32 # API_INTERNAL_TOKEN
   ```

3. **Build Analysis CLI**:
   ```bash
   cd cli && ./gradlew build
   ```

4. **Launch Multi-Container Stack**:
   ```bash
   docker compose up --build -d
   ```

5. **Verify Stack Health**:
   ```bash
   curl http://localhost/healthz
   # Output: ok
   ```

---

## 10. Changelog

For a complete record of all versions, bug fixes, security patches, and feature additions, please refer to [CHANGELOG.md](./CHANGELOG.md).

---

## 11. Known Issues & Production Readiness Audit

> **Status: remediated and verified end to end.** The defects catalogued in 11.2–11.4 were found
> against commit `51f35cc` and are fixed in `1.2.0`; the subsections are retained as a record of
> what was wrong and why.
>
> Verification performed: `go vet ./...` and `go test ./...` pass; the rendered nginx config passes
> `nginx -t`; the extractor jar builds (`./build_cli.sh`, 14.8 MB); and the full stack was booted
> and driven through a real GitHub OAuth login, which discovered **121 public repositories**,
> queued them, and ingested them through clone → extract → protobuf API → Postgres with zero
> errors. Dashboard, public profile, SVG badge, embed snippets and a 146-contributor Hall of Fame
> were all confirmed rendering live with masked addresses and profile-id-keyed links.
>
> The 11.2–11.4 subsections below describe the **pre-fix** state. See 11.1 for what each item's
> resolution actually was, and `CHANGELOG.md` §1.2.0 for the full list.

### 11.1 Summary

All items previously catalogued are resolved. Several were only *partially* fixed by the
`1.1.1` pass, and two new critical defects surfaced during re-review:

| ID | Resolution |
|---|---|
| SEC-01 | `/api/auth` validates HTTP Basic credentials and echoes the presented credential, never the raw token. |
| SEC-02 | CORS restricted to anonymous read-only endpoints; credentialed origins require `CORS_ALLOWED_ORIGINS`. |
| SEC-03 | `RemoteAddr` trust removed entirely. |
| SEC-04 | Contributor emails masked on every anonymous surface (HTML, SVG and JSON); author names that are themselves addresses are masked too. |
| SEC-05 | Cookies carry `Secure`/`SameSite`; sessions now carry a signed expiry; TLS config templated per environment. |
| BUG-01 | `repo_url`/`repo_name` populated on ingest; Tier-1 skip is live. |
| BUG-02 | Private repositories skipped at discovery instead of being enqueued. |
| BUG-03 | Rune-safe truncation. |
| BUG-04 | Embed snippets and the SVG footer render from `PUBLIC_BASE_URL`. |
| BUG-05 | Default API base path documented; the CLI runs inside the backend container where it is correct. |
| BUG-06 | Trending window falls back relative to the latest commit; new-contributor list has a fallback. |
| BUG-07 | Failed downloads delete the partial file and are negatively cached. |
| OPS-01 | Queue remains single-consumer by design (the extractor's on-disk config is shared and rewritten per run); serialization is now explicit and documented, with bounded subprocess timeouts. |
| OPS-02 | Schema applied idempotently at startup. |
| OPS-03 | Jar built by `./build_cli.sh` / the `cli-build` Compose profile; startup rejects missing, empty or directory jars. |
| OPS-04 | Production start fails fast on missing secrets; ingestion API fails closed. |
| OPS-05 | `isDevMode()` reads only `ENV`. |
| OPS-06 | Pinned base images; backend runs as an unprivileged user. |
| OPS-07 | nginx `limit_req` provides the rate limit; `ThrottleBacklog` remains as a concurrency bound. |
| OPS-08 | Dashboard aggregations covered by the composite indexes in `schema.sql`. |
| OPS-09 | Build runs in a pinned Gradle container via `./build_cli.sh`. |
| OPS-10 | Analytics disabled (`IS_GA_ENABLED=false`, `SENTRY_ENABLED=false`). |
| OPS-11 | Added regression coverage for auth, session expiry, email masking, CORS, headers, jar validation and template rendering (`backend/security_test.go`). |
| OPS-12 | Shutdown drains the in-flight ingestion job. |
| OPS-13 | Backend uses `expose`; Dozzle bound to loopback behind a `debug` profile. |
| OPS-14 | This section and `README.md` updated. |

**Found during re-review and fixed in `1.2.0`:**

| ID | Severity | Defect |
|---|---|---|
| SEC-06 | Critical | Dozzle published on all interfaces with a writable Docker socket and no authentication — root-equivalent host access. |
| BUG-08 | Critical | The CLI SHA-256 hashes every password before sending it, while the backend compared the raw token. All ingestion returned 401; no commit data was ever stored. |
| BUG-09 | High | Templates linked to `/p/{email}` and `/badge/{email}.svg`; both handlers key on `profile_id`, so every public profile and badge link 404'd. |
| BUG-10 | High | The `users` table was never written, so `is_sourcerer` was permanently false and the Hall of Fame member tier was dead. |
| SEC-07 | High | Session cookies signed the bare email with no expiry — a captured cookie was valid forever and logout was client-side only. |
| SEC-08 | High | nginx `add_header` inheritance meant the HSTS header in the HTTPS server block silently dropped every security header declared at `http` level. |
| BUG-11 | Medium | `git clone` and the extractor ran without timeouts against a single-consumer queue; one hung repository stalled ingestion permanently. |

**Found while booting the stack end to end (these are why "not booted" mattered):**

| ID | Severity | Defect |
|---|---|---|
| BUG-12 | Critical | `cli/build.gradle` passed `'""'` for three `String` buildConfig fields; the plugin quotes String values itself, so it emitted `= """"` and `compileBuildConfig` failed. The extractor jar could never be built. |
| BUG-13 | High | The proxy crash-looped alongside an unhealthy backend: nginx resolves upstreams at startup and `depends_on: [backend]` only waits for "started". |
| BUG-14 | High | `session` and `oauth_state` cookies were unconditionally `Secure`, so local development over plain HTTP could never complete a login. |
| BUG-15 | High | `ENV=development` inside the container returned 500 on every page — hot-reload `ParseGlob` found no `templates/` beside the binary, since they are embedded. |
| OPS-15 | Low | Changing `POSTGRES_PASSWORD` in `.env` does not affect an existing volume; Postgres only applies credentials on first initialization. Resync with `ALTER USER` or recreate the volume. |

---

### 11.2 Security Defects

#### SEC-01 — `POST /api/auth` distributes the internal API token to unauthenticated callers
**Severity: Critical** · `backend/api.go:59`, `backend/api.go:80-92`

The `/auth` route is registered *outside* the `r.Group` that applies `apiAuthMiddleware`. The handler
returns the value of `API_INTERNAL_TOKEN` verbatim in a `Set-Cookie: Token=...` response header. It
never inspects the HTTP Basic credentials the CLI supplies (`cli/src/main/kotlin/app/api/ServerApi.kt:71`),
so no credential is validated on any code path.

*Impact:* Any unauthenticated party issues one `POST /api/auth`, receives the token, and thereby gains
full write access to every ingestion route — `POST /api/commits`, `/api/facts`, `/api/authors`,
`/api/repo`, and `DELETE /api/commits`. This permits arbitrary fabrication of commit telemetry and
destruction of the existing commit store. `apiAuthMiddleware` provides no effective protection.

*Note on test coverage:* `backend/main_test.go:60` (`TestApiAuthMiddleware`) exercises the middleware in
isolation and passes. It never asserts that `/api/auth` is unreachable without credentials, so the
suite reports green while the endpoint is open.

*Remediation:* Authenticate the CLI before issuing any token. Either move `/auth` behind a real
credential check (validate `username`/`password` against the `users` table), or remove the endpoint
entirely and have the CLI present `Authorization: Bearer $API_INTERNAL_TOKEN` supplied via its own
configuration.

#### SEC-02 — Reflected CORS origin combined with credentialed requests
**Severity: Critical** · `backend/main.go:217-232`

The CORS middleware echoes the caller's `Origin` header into `Access-Control-Allow-Origin` and
unconditionally sets `Access-Control-Allow-Credentials: true`.

*Impact:* Any third-party website visited by a logged-in user can issue credentialed cross-origin
requests that carry that user's `session` and `Token` cookies, read the responses, and act on their
behalf. Chained with SEC-01 this yields cross-site destruction of the commit store via
`DELETE /api/commits`.

*Remediation:* Replace the reflection with an explicit allowlist sourced from configuration, or drop
`Allow-Credentials` and serve only genuinely public resources cross-origin.

#### SEC-03 — Authentication bypass via unvalidated `RemoteAddr`
**Severity: High** · `backend/api.go:32-36`

`apiAuthMiddleware` grants unauthenticated access when `r.RemoteAddr` begins with `127.0.0.1:`,
`[::1]:`, or `localhost:`, or equals `@`. This exists so the in-container CLI can reach the API.

*Impact:* Currently contained because the Go service is only `expose`d, not published
(`docker-compose.yml:22-23`). The bypass becomes exploitable the moment the backend port is published,
the service is run with host networking, or a proxy is introduced that preserves a loopback source
address. The `host == "@"` case additionally matches abstract Unix socket peers.

*Remediation:* Remove the address-based bypass. Have the co-located CLI authenticate with the shared
secret like any other client.

#### SEC-04 — Contributor emails and per-developer statistics are publicly readable
**Severity: High** · `backend/main.go:270-292`, `backend/templates/index.html:619`, `backend/main.go:294-416`, `:898`, `:1052`

The dashboard root handler queries every row of `authors` and renders a `<select>` containing each
contributor's name and full email address. No session is required to reach `/`. Separately,
`/dashboard/stats`, `/dashboard/languages`, `/dashboard/repos`, `/dashboard/facts`, `/dashboard/libraries`,
`/p/{email}`, `/u/{username}`, and `/badge/{identifier}` all accept an arbitrary email and return that
individual's complete telemetry with no authorization check.

*Impact:* Machine-harvestable disclosure of the email address of every contributor to every indexed
repository, **including contributors to private repositories** who never consented to the service.
Any third party can then enumerate each address to retrieve full commit history, language profile, and
behavioral facts.

*Remediation:* Require an authenticated session for `/` and all `/dashboard/*` routes, and scope them
to the session's own email. Make public profile and badge routes opt-in per user, and key them on an
opaque identifier rather than a raw email address.

#### SEC-05 — No transport encryption; session cookie lacks `Secure`
**Severity: High** · `nginx/nginx.conf:63-115`, `nginx/nginx.conf:117-141`, `docker-compose.yml:43`, `backend/main.go:550`

The only active nginx server block is `listen 80`. The TLS server block is commented out in its
entirety, while `docker-compose.yml` publishes port 443 to a container with nothing bound to it. The
session cookie is set with `HttpOnly` and `SameSite=Lax` but without the `Secure` attribute.

*Impact:* OAuth authorization codes, the session cookie, and all dashboard content traverse the network
in cleartext and are trivially interceptable and replayable on any shared network path.

*Remediation:* Terminate TLS (certificates plus the enabled 443 block, or an upstream load balancer),
redirect `:80` to `:443`, add `Secure` to the session and `oauth_state` cookies, and add HSTS.

---

### 11.3 Correctness Defects

#### BUG-01 — `repos.repo_url` / `repos.repo_name` are never populated; Tier-1 sync skip is dead code
**Severity: High** · `backend/worker.go:96-104`, `backend/worker.go:150-157`, `backend/api.go:124`, `backend/main.go:597-609`

Both worker `UPDATE` statements locate their target row with `WHERE repo_url = $4 OR repo_name = $5`.
Those columns are only ever written by those same statements. The sole `INSERT` into `repos`
(`api.go:124`) supplies `(rehash, initial_commit_rehash)` alone, so both columns remain `NULL` for the
life of the row. The `WHERE` clause therefore matches zero rows on every execution, and the
`COALESCE(NULLIF(...))` self-heal inside the `SET` list can never bootstrap the values because the row
is never selected in the first place.

*Impact:* `github_pushed_at` is never persisted, so the Tier-1 fast-skip check at `main.go:597-609`
always reads an empty `existingPushedAt` and never skips. The Tier-1 half of the "2-tier smart
repository sync" shipped in commit `5fce484` performs no work. Only Tier-2 (`worker.go:82-87`, matching
the remote HEAD rehash against the `commits` table) is functional — and that check is correct: the Go
`computeCommitRehash` (`worker.go:48-51`) produces the same `sha256hex(gitSHA)` as the CLI
(`cli/src/main/kotlin/app/model/Commit.kt:37`).

*Remediation:* Establish repository identity at insert time. Either have the CLI report the clone URL
on `POST /api/repo` so the row carries it from creation, or have the worker upsert
`repos(rehash, repo_url, repo_name)` keyed on `rehash` after ingestion completes and the rehash is known.

#### BUG-02 — Private repositories are enqueued but can never be cloned
**Severity: High** · `backend/worker.go:120`, `backend/main.go:77`, `backend/main.go:564-582`

The OAuth flow requests the `repo` scope and enumerates repositories with
`ListByAuthenticatedUser`, which returns private repositories. The worker then invokes
`git clone <cloneURL>` with no credential material of any kind.

*Impact:* Every private repository fails to clone, the job is discarded with an error log, and the user
receives no feedback. The `repo` scope is requested from the user — and the resulting authorization
granted — for a capability the system does not implement.

*Remediation:* Either narrow the OAuth scope to `public_repo` and document the limitation, or thread the
user's access token into the clone URL. If the latter, ensure the token is scrubbed from `cmdClone.Stderr`,
which is currently wired directly to `os.Stderr` (`worker.go:121-122`).

#### BUG-03 — UTF-8 name truncation produces malformed SVG
**Severity: Medium** · `backend/main.go:1298-1300`

`displayName = displayName[:14] + "…"` slices a Go string by bytes after a `len()` check that also
counts bytes.

*Impact:* Any contributor name longer than 16 bytes containing multi-byte characters (accented Latin,
Cyrillic, CJK, emoji) is cut mid-rune. The resulting invalid UTF-8 is emitted into the Hall of Fame
SVG, producing malformed XML that GitHub's image proxy will fail to render.

*Remediation:* Convert to `[]rune` before measuring and slicing. The same construct appears at
`main.go:1335-1336` for `repoDisplay`; it is currently safe only because repo rehashes are hex.

#### BUG-04 — Embed snippets and SVG footer hardcode `sourcerer.io`
**Severity: Medium** · `backend/templates/repo.html:746`, `:754`, `backend/main.go:1386`

The README Embed Hub emits Markdown and HTML pointing at `https://sourcerer.io/hall-of-fame/...` and
`https://sourcerer.io/r/...`, and the generated SVG footer renders the literal text `sourcerer.io/r/<rehash>`.

*Impact:* Every badge snippet copied from a self-hosted deployment references a domain the operator does
not control, so the feature produces broken embeds for all users.

*Remediation:* Introduce a `PUBLIC_BASE_URL` environment variable, thread it into the template data and
the SVG generator, and default it to the request host.

#### BUG-05 — CLI default API base path is unusable outside the backend container
**Severity: Medium** · `cli/build.gradle:34`

`API_BASE_PATH` defaults to `http://localhost:8080/api` and is fixed at build time via `BuildConfig`.

*Impact:* Correct only for the jar executed by the worker inside the backend container. Any jar
distributed to end users targets the user's own machine and silently fails to reach the service.

*Remediation:* Make the base path a runtime setting (environment variable or config file) with the
build-time constant as fallback only.

#### BUG-06 — Trending and New contributor windows are wrong for inactive repositories
**Severity: Medium** · `backend/main.go:1142-1147`, `:1173-1216`

When a repository has no commits in the last 7 days, the cutoff is recomputed as
`maxDate - (30 * 86400)` — 30 days *preceding* the repository's final commit.

*Impact:* "Trending velocity" for any inactive repository reports the tail of its history rather than
recent activity. The New Contributors query (`HAVING MIN(c.date) >= cutoff`) then admits most authors
who joined in that same span. The fallback at `:1195` further degrades the column to "the six most
recent first-time committers ever," which is not what the label claims. The design intent stated in
`design_plan.md` §3 is not met.

*Remediation:* Define the window relative to `maxDate` explicitly as a bounded lookback, and label the
UI to state the window actually used (for example "last 30 days of activity, ending <date>").

#### BUG-07 — Failed classifier download leaves a zero-byte file that permanently poisons the cache
**Severity: Medium** · `cli/src/main/kotlin/app/extractors/ClassifierManager.kt:117-137`

`FileHelper.getFile(libId + DATA_EXT, CLASSIFIERS_DIR)` creates the destination file *before* the HTTP
response status is examined. On a non-200 response the method returns `false`, leaving a zero-length
`.pb` on disk.

*Impact:* The in-process negative cache added in commit `51f35cc` correctly halts the retry loop within a
single run. It does not clean up the artifact. On the next JVM invocation — and the worker spawns a fresh
JVM per repository — `FileHelper.notExists` returns `false`, the download is skipped, `loadClassifier`
rejects the empty file, and the library is marked unavailable again. The library remains permanently
undetectable even after the upstream storage bucket is repaired.

*Remediation:* Delete the destination file in the non-200 and exception branches, or download to a
temporary path and rename only on success.

---

### 11.4 Operational & Deployment Defects

#### OPS-01 — Ingestion worker is a single-consumer, lossy, non-durable queue
**Severity: High** · `backend/worker.go:22`, `:24-33`, `:35-46`, `backend/main.go:611-617`

One goroutine consumes a 200-slot buffered channel. Each job performs a full-depth `git clone` followed
by a JVM subprocess. `EnqueueJob` drops on a full queue and returns `false`; the caller discards the
return value.

*Impact:* A single user with more than 200 repositories saturates the queue and silently loses jobs. One
large repository blocks every other user's ingestion for its full duration. There is no retry, no
persistence, and no deduplication — a container restart discards all pending work, and each re-login
re-enqueues the user's entire repository set.

*Remediation:* Move the queue to a durable store (a `jobs` table or a broker), add a bounded worker pool,
add per-repository deduplication, and add retry with backoff. Surface queue depth and drop counts as metrics.

#### OPS-02 — No versioned migrations; new columns are absent on existing databases
**Severity: High** · `backend/main.go:191-198`, `backend/schema.sql`, `docker-compose.yml:16`

Schema is applied by two independent mechanisms: `db.Exec(schemaSQL)` on every backend boot, and the same
file mounted as a Postgres `docker-entrypoint-initdb.d` script (which runs on first initialization only).
`schema.sql` consists solely of `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`.

*Impact:* The columns added for the smart-sync feature (`repo_url`, `repo_name`, `last_commit_rehash`,
`last_synced_at`, `github_pushed_at`) and the `technologies` table are created only on a **fresh**
database. Upgrading an existing deployment produces no `ALTER TABLE`, so every query referencing those
columns fails at runtime. There is no migration history, no ordering, and no rollback.

*Remediation:* Adopt a versioned migration tool (`golang-migrate`, `goose`, or equivalent), remove the
`initdb.d` mount so there is one authoritative path, and gate application startup on migration success.

#### OPS-03 — Backend depends on a host-built jar via bind mount, with no startup validation
**Severity: High** · `docker-compose.yml:32`, `backend/worker.go:129-133`

`./cli/build/libs/sourcerer-app.jar` is bind-mounted into the backend container. Nothing verifies its
presence.

*Impact:* If the jar has not been built on the host first, Docker creates a **directory** at that path.
The container starts and reports healthy; ingestion then fails on every single job when `java -jar`
is invoked against a directory. The failure surfaces only in worker logs, long after deployment.

*Remediation:* Build the CLI inside a multi-stage `backend/Dockerfile` (or publish it as a separate image
and `COPY --from`), so the artifact is part of the image. Add a startup check that the jar exists and is
a readable file.

#### OPS-04 — Configuration failures are fail-open rather than fail-fast
**Severity: High** · `backend/main.go:143-153`, `:74-79`, `:187-198`, `backend/api.go:25-29`

- Missing `SESSION_SECRET`: a random secret is generated and the service continues with a warning. Every
  restart silently invalidates all outstanding sessions.
- Missing `API_INTERNAL_TOKEN`: `apiAuthMiddleware` returns early and permits **all** `/api/*` requests
  unauthenticated. The comment describes this as "dev mode," but the behavior is identical in production.
- Missing `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET`: read at package-var initialization with no
  validation; OAuth fails only at first user login.
- Unreachable database at boot: logged at `Warn` and startup proceeds; the process serves traffic and
  every request errors.

*Remediation:* Validate all required configuration at startup and exit non-zero when `ENV=production` and
any required value is absent. Never disable authentication as a side effect of missing configuration.

#### OPS-05 — `isDevMode()` filesystem fallback can silently enable dev behavior in production
**Severity: Medium** · `backend/main.go:99-108`

When `ENV` is unset (neither `development` nor `production`), the function probes for
`templates/index.html` on disk and returns `true` if found.

*Impact:* A production deployment with `ENV` accidentally unset and the templates directory present
re-parses every template from disk on every request (`main.go:116-122`) and emits verbose debug-level
text logs instead of structured JSON.

*Remediation:* Default to production behavior when `ENV` is unset or unrecognized. Gate hot-reload on an
explicit opt-in only.

#### OPS-06 — Unpinned base images; container runs as root while executing untrusted repository content
**Severity: Medium** · `backend/Dockerfile:1`, `:16`, `:21`

`golang:alpine` and `alpine:latest` are floating tags, making builds non-reproducible. `WORKDIR /root/`
with no `USER` directive means the process runs as UID 0.

*Impact:* Compounding risk: the worker clones arbitrary third-party repositories and then executes a JVM
that parses their contents, all as root inside the container. A parser or `git` vulnerability reached via
hostile repository content escalates directly to container root.

*Remediation:* Pin base images to digests. Add a non-root `USER`. Consider running clone and analysis in a
separate, network-restricted container with a read-only root filesystem.

#### OPS-07 — `ThrottleBacklog` is a concurrency limiter, not a rate limiter
**Severity: Medium** · `backend/main.go:215`, `nginx/nginx.conf:53-54`

`middleware.ThrottleBacklog(100, 50, 5*time.Second)` bounds in-flight requests; it does not bound request
rate. The only true rate limiting lives in nginx.

*Impact:* Section 7.4 of this document describes the chi middleware as application-layer rate limiting; it
is not. Any path that reaches the backend without traversing nginx is entirely unrated.

*Remediation:* Add a real per-client rate limiter at the application layer (`httprate` or equivalent) keyed
on a trusted client identifier, and correct the description in §7.4.

#### OPS-08 — Dashboard polling issues uncached full-table aggregations
**Severity: Medium** · `backend/templates/index.html:642-695`, `backend/main.go:294-416`

Four HTMX panels each declare `hx-trigger="load, change from:#email-select, every 30s"`. Each request
executes `COUNT(*)` / `SUM(...)` / `GROUP BY` across `commits` and `commit_stats` with no caching layer
and no `Cache-Control`.

*Impact:* Four full aggregate scans per 30 seconds per open browser tab. Cost grows linearly with total
commit volume multiplied by concurrent viewers. The unfiltered variants (no `email` parameter) scan the
entire dataset.

*Remediation:* Maintain rollup tables or materialized views refreshed on ingestion, add short-TTL response
caching, and lengthen or make the poll interval adaptive.

#### OPS-09 — CLI build is not reproducible, and the documented build command does not exist
**Severity: Medium** · `cli/build.gradle:9`, `:92`, `:93`, `:100-121`; `cli/` (no wrapper)

Three compounding problems:

1. The build declares `jcenter()` and `https://dl.bintray.com/jetbrains/spek` as repositories. Both
   services were shut down and no longer serve artifacts.
2. Dependency configurations use `compile` / `testCompile`, removed in Gradle 7. Kotlin is pinned to
   1.2.31 (2018) and the `de.fuerstenau.buildconfig` plugin (1.1.8) is unmaintained.
3. The `cli/` directory contains **no `gradlew` wrapper, no `gradle/wrapper/` directory, and no
   `settings.gradle`**. §9.2 step 3 of this document instructs the operator to run `cd cli && ./gradlew build`,
   which fails immediately with "no such file or directory."

*Impact:* There is no working, documented path to produce `sourcerer-app.jar` — which OPS-03 shows is a
hard runtime dependency of the backend container. This also blocks building the jar inside Docker and
blocks all security patching of the CLI dependency tree.

*Remediation:* Commit a Gradle wrapper and `settings.gradle`, replace dead repositories with
`mavenCentral()` and a maintained Spek source, migrate `compile`/`testCompile` to
`implementation`/`testImplementation`, and upgrade Gradle and Kotlin. Expect source-level work; treat as a
scoped modernization task. Correct §9.2 once a working command exists.

#### OPS-10 — CLI transmits analytics to a third-party property by default
**Severity: Medium** · `cli/build.gradle:53-55`, `:38`

`IS_GA_ENABLED` defaults to `true` with `GA_TRACKING_ID = 'UA-107129190-2'` — a Universal Analytics
property belonging to the original upstream vendor. `PROFILE_URL` likewise points at `https://sourcerer.io/`.

*Impact:* Every headless ingestion run — one per repository — attempts an outbound call to
`google-analytics.com` carrying usage telemetry, from a self-hosted deployment, to a property the operator
does not own. Universal Analytics properties stopped processing data in 2023, so the requests are also
functionally dead weight.

*Remediation:* Default `IS_GA_ENABLED` to `false`. Remove or reassign the hardcoded tracking ID and
`PROFILE_URL`.

#### OPS-11 — Test suite contains no integration coverage
**Severity: Medium** · `backend/main_test.go`

All eight tests exercise pure functions: cookie signing, the auth middleware in isolation, SVG string
generation, JSON fixture parsing, hash determinism, and channel enqueue. There is no test that mounts the
router, no test that touches a database, and no test covering the `/api/auth` exposure (SEC-01) or the
non-matching `repos` UPDATE (BUG-01) — both of which are invisible to the current suite.

*Impact:* `code_review.md` asserts that live HTTP endpoints were verified via curl. No artifact in the
repository substantiates that claim, and no automated check prevents regression.

*Remediation:* Add router-level tests using `httptest.NewServer` plus a containerized Postgres
(`testcontainers-go` or a CI service container). Cover at minimum: unauthenticated access to every `/api/*`
route, the repos upsert path, and each public page route.

#### OPS-12 — In-flight ingestion is terminated without draining on shutdown
**Severity: Low** · `backend/main.go:206-208`, `:463-476`

`cancelWorker` is registered via `defer` and the HTTP server is given a 5-second `Shutdown` deadline. There
is no `WaitGroup` and no wait on worker completion.

*Impact:* A clone or JVM analysis in progress at shutdown is killed mid-run. Because commits are posted in
batches (`api.go:134-213`), the repository is left partially ingested with no marker distinguishing it from
a complete one.

*Remediation:* Track in-flight jobs with a `WaitGroup`, propagate the context into `exec.CommandContext`, and
wait (with a bounded deadline) before returning from `main`.

#### OPS-13 — Redundant port publication
**Severity: Low** · `docker-compose.yml:41-42`

`"8080:80"` and `"80:80"` both map to the same container port. Additionally `"443:443"` is published while
nginx has no `listen 443` server (see SEC-05).

*Remediation:* Publish one HTTP port. Publish 443 only once a TLS server block is active.

#### OPS-14 — Documentation drift in this file
**Severity: Low** · `project_docs.md`

Corrected in this revision:

- §2.4 stated PostgreSQL 15; `docker-compose.yml` pins `postgres:18-alpine`.
- §3 stated a queue capacity of 100; actual is 200 (`worker.go:22`). Job loss on overflow was described as a safety property rather than a defect.
- §4 reproduced a schema predating the smart-sync columns and the `technologies` table.
- §4's *Fact Codes Reference* was **entirely fabricated** — it listed codes 100, 101, 200, 201, and 300, none of which exist. The real codes are 1–17, defined in `cli/src/main/kotlin/app/FactCodes.kt`. The stated field semantics were also wrong: the bucket lives in the `key` column and the measurement in `value1`, not `value1`/`value2` as a pair.
- §5.1 described `DELETE /api/commits` as taking a `rehash` query parameter; it consumes a protobuf `CommitGroup` body (`api.go:215-227`). Four endpoints documented as functional (`/api/user` GET and POST, `/api/distances`, `/api/process`) are no-op stubs. `/api/process/create` was undocumented.
- §5 documented no public read routes at all; the entire dashboard, profile, badge, Hall of Fame, and Libraries surface was absent. Added as §5.4.
- §7 described security controls that are not enforced (SEC-01, SEC-02, SEC-03, SEC-05, OPS-04, OPS-07). Cross-origin policy was undocumented; added as §7.5, with the former §7.5 renumbered to §7.6.
- §8 listed `SESSION_SECRET` as both required and auto-defaulted, and did not note that an unset `API_INTERNAL_TOKEN` disables authentication.
- §9.2 step 3 instructs `cd cli && ./gradlew build`; no Gradle wrapper exists in the repository (OPS-09).
- §10 linked the changelog through an absolute `file://` path containing an unrelated username.

*Remediation:* Treat §4, §5, and §7 as normative and reconcile them against source on each release. Prefer
generating the API and schema sections from source over hand-maintaining them.

---

### 11.5 Verified as Correct

Recorded to prevent re-investigation:

- **Chi route precedence** — `/hall-of-fame/{repo}` and `/hall-of-fame/{repo}.svg` (`main.go:427-431`)
  resolve correctly and independently; the static suffix is honored. Confirmed empirically against
  chi v5.3.1.
- **Commit rehash agreement** — Go `computeCommitRehash` (`worker.go:48-51`) and Kotlin
  `DigestUtils.sha256Hex(revCommit.id.name)` (`cli/.../model/Commit.kt:37`) produce identical values, so
  the Tier-2 skip check compares like with like.
- **OAuth CSRF state handling** — `handleGitHubLogin` / `handleGitHubCallback` (`main.go:479-509`) generate
  32 random bytes, store them in a short-lived `HttpOnly` cookie, and compare with `hmac.Equal`
  (constant-time). Correct.
- **SQL parameterization** — All queries use positional placeholders. No string-concatenated SQL found.
- **Go dependencies** — Current and free of known advisories at time of audit: chi v5.3.1,
  go-github v60.0.0, protobuf v1.36.12, lib/pq v1.12.3, oauth2 v0.36.0.
- **Ingestion body limits** — All protobuf handlers wrap the body in `io.LimitReader` (`api.go:111`, `:135`,
  `:216`, `:256`, `:302`), bounding memory per request.
- **Template escaping** — Templates use `html/template` with no `template.HTML` casts; SVG generators apply
  `template.HTMLEscapeString` to interpolated values. HTMX is loaded from unpkg with a valid SRI hash.
