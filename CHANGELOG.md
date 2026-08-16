# Changelog

All notable changes to the Sourcerer App will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.2.0] - 2026-08-16

Production-readiness pass. Ingestion was non-functional before this release.

### Fixed
- **Ingestion authentication (critical)**: the Kotlin CLI SHA-256 hashes every password before sending it, while the backend compared against the raw `API_INTERNAL_TOKEN`. Every ingestion request returned 401, so no commit data was ever stored. The backend now accepts either the raw token or its digest, in constant time, and `/api/auth` echoes back the presented credential instead of the raw token.
- **Broken profile and badge links**: templates linked to `/p/{email}` and `/badge/{email}.svg` while the handlers are keyed on `profile_id`, so every link 404'd. `ProfileData` and `ContributorStat` now carry the profile id and all links are built from it.
- **Hardcoded `localhost:8080` in README embed snippets**: snippets now render from `PUBLIC_BASE_URL` (or the proxy-forwarded scheme and host).
- **`users` table was never written**, so the Hall of Fame "Sourcerer member" halo could never appear. The OAuth callback now upserts the account.
- **Private repositories were enqueued but unclonable** (the OAuth scope is `public_repo` and the worker clones anonymously); they are now skipped at discovery.
- **Unbounded subprocesses**: `git clone` and the extractor run under 15- and 30-minute timeouts with credential prompts disabled. A single hung repository previously stalled ingestion permanently.
- **Empty extractor jar passed startup validation**: a zero-byte placeholder satisfied the old existence check and failed once per job. Startup now rejects missing, empty and directory jars.
- Errors from `rows.Err()`, profile persistence and `crypto/rand` are checked and logged instead of discarded.
- **The extractor jar could not be built at all.** `cli/build.gradle` passed `'""'` as the value of three `String` buildConfig fields, but the plugin quotes String values itself, so it emitted `= """"` and `compileBuildConfig` failed. Nothing downstream of the extractor could ever have run.
- **The proxy crash-looped whenever the backend was not yet healthy.** nginx resolves upstream hostnames at startup and refuses to start if they do not resolve, so `depends_on: [backend]` (merely "started") took the proxy down with it. Now waits for `service_healthy`.
- **Local development could not log in.** The `session` and `oauth_state` cookies were unconditionally `Secure`, so a browser served over plain HTTP discarded both — the OAuth callback failed with "State cookie not found". `Secure` is now relaxed only when `ENV=development`.
- **`ENV=development` returned 500 on every page inside Docker**, because template hot-reload did a hard `ParseGlob("templates/*.html")` and the image has no `templates/` beside the binary (they are embedded). Now falls back to the embedded set.

### Added
- `docker-compose.dev.yml`: local overlay serving plain HTTP on `127.0.0.1:8080`, mounting `backend/templates` for hot reload, and parking the TLS proxy behind a profile — matching a GitHub OAuth app whose callback is `http://localhost:8080/auth/github/callback`.

### Security
- **Session cookies now expire.** The signed payload was the bare email address, so a captured cookie stayed valid forever. Expiry is signed in and enforced on every request; cookies in the old format are rejected.
- **Contributor email addresses are no longer published.** Hall of Fame HTML/SVG, the library leaderboards and `/api/hall-of-fame/{repo}` render a masked form, and author names that are themselves addresses are masked too.
- **Security headers are actually served over TLS.** nginx's `add_header` inheritance is all-or-nothing per level, so the `Strict-Transport-Security` header in the HTTPS server block silently dropped every header set at `http` level. Application headers moved to the backend; nginx sets only HSTS. Adds `Content-Security-Policy`.
- **CORS is no longer wildcard-on-everything.** Only anonymous read-only endpoints are embeddable; session-backed routes send no CORS grant. `CORS_ALLOWED_ORIGINS` allows explicit credentialed origins.
- The `/api/auth` `Token` cookie gained `Secure`, `SameSite=Strict` and a 12h lifetime.
- The ingestion API fails closed when `API_INTERNAL_TOKEN` is unset outside development.
- **Dozzle** moved behind a `debug` profile, bound to `127.0.0.1`, with a read-only Docker socket. It was published on all interfaces with no authentication and a writable root-equivalent socket.
- The backend container runs as an unprivileged user on pinned base images; it executes untrusted repository content.

### Changed
- nginx configuration is a template rendered from `SERVER_NAME`, `SSL_CERTIFICATE` and `SSL_CERTIFICATE_KEY`, so switching between local self-signed and Let's Encrypt certificates no longer means editing the config by hand.
- The proxy reloads every 6h so renewed certificates take effect without a restart.
- `./build_cli.sh` and a `cli-build` Compose profile build the extractor jar in a pinned Gradle container.
- Compose gained healthchecks, memory limits and required-variable guards.
- Graceful shutdown drains the in-flight ingestion job instead of killing a half-written clone.
- Removed dead upstream Sourcerer Inc. deployment tooling (`cli/deploy`, `cli/do.sh`, `cli/Dockerfile`, `cli/src/install`), superseded design notes, and committed debug artifacts.
- Added a root `README.md` covering configuration and deployment.

---

## [1.1.1] - 2026-08-16

### Security Fixes
- **Unauthenticated API Access (SEC-01)**: Enforced authentication on `POST /api/repos` and user profile endpoints to prevent unauthorized creation and modification of repositories and profiles.
- **Server-Side Request Forgery & Re-bind (SEC-02, SEC-03)**: Validated URLs and restricted outbound requests to prevent SSRF and DNS rebinding attacks during repository ingestion.
- **Cache Poisoning & Port Exposure (SEC-04, SEC-05, OPS-13)**: Standardized Nginx configurations and removed duplicate published ports in `docker-compose.yml`. Configured SSL readiness.

### Operational & Bug Fixes
- **Missing Repository Update Statement (BUG-01)**: Fixed the broken SQL `UPDATE` statement for the `repos` table in `api.go`, resolving the `sql: expected 4 arguments, got 3` error that crashed ingestion.
- **CLI Build Fix (OPS-09)**: Modernized `cli/build.gradle` by removing defunct repositories (jcenter, bintray) and updating the build wrapper to ensure reproducible builds.
- **Unwanted Analytics Telemetry (OPS-10)**: Disabled default Google Analytics (`IS_GA_ENABLED=false`) in the CLI worker to prevent data leaks.
- **Graceful Shutdown & Queue Handlers (OPS-12)**: Implemented `WaitGroup` in `main.go` and `worker.go` to properly drain in-flight ingestion jobs on shutdown without orphaned processes.
- **Test Suite Updates (OPS-11)**: Added missing integration tests, filling the void in automated coverage.
- **Documentation Drift (OPS-14)**: Synchronized `project_docs.md` schemas, endpoints, and CLI instructions with reality.
- **SVG Route Performance**: Re-enabled cache-control headers on `/{repo}.svg` and `/r/{repo}.svg` endpoints to protect against CPU spikes on high-traffic badge embeds.

## [1.1.0] - 2026-08-16

### Added
- **Classifier Retry Loop Bug Fix**: Fixed an infinite download retry loop in `ClassifierManager.kt` where unavailable cloud classifier models (`HTTP 403`) were repeatedly re-requested on every line of code. Added negative caching via an `unavailable` set to instantly bypass missing models.
- **Smart Repository Sync & Skip Optimization**: Built a 2-tier intelligent sync skipping engine. Tier 1 fast-skips unmodified repositories by comparing GitHub API `pushed_at` timestamps against PostgreSQL metadata before enqueueing. Tier 2 verifies the remote HEAD commit SHA via lightweight `git ls-remote` (~100ms) in the background worker to skip unnecessary disk cloning and JVM AST analysis for all fully indexed repositories.
- **Repository Hall of Fame (Feature 1 & 2)**: Added automated contributor recognition with 3-tier velocity breakdown (**Top Contributors**, **Trending Velocity**, and **New Arrivals**), live README SVG badge generation (`/hall-of-fame/{repo}.svg` and `/r/{repo}.svg`), public repository showcase page (`/r/{repo}`), and JSON API (`/api/hall-of-fame/{repo}`).
- **Awesome Libraries Technology Intelligence (Feature 3 & 4)**: Introduced a curated taxonomy of 38 software libraries and frameworks across 19 technical domains (`technologies.json`), automated database seeding and indexing (`technologies` table), full interactive library catalog (`/libraries`), individual library leaderboard & telemetry page (`/libraries/{tech}`), JSON API (`/api/libraries`), and developer domain matrix on public profiles (`/p/{email}`).
- **Modern Purple Frontend Redesign**: Full visual and architectural redesign with *Deep Violet Cyber-Intelligence* aesthetic thesis, responsive grid structure for desktop and mobile, Google Fonts (`Plus Jakarta Sans` & `JetBrains Mono`), glowing telemetry cards, interactive embed hub, and unified purple design tokens (`#8b5cf6`, `#7c3aed`, `#090514`).
- **Dynamic Public Developer Profiles**: Shareable public profiles accessible at `/u/{username}` and `/p/{email}` displaying hero banners, contributor metrics, language distribution progress bars, repository breakdowns, and coding habits.
- **Dynamic SVG Badges**: Endpoint at `/badge/{identifier}.svg` generating cache-enabled (`Cache-Control: max-age=1800`) SVG cards suitable for embedding directly in GitHub READMEs.
- **Coding Habits & Facts Card**: Decoder for `FactCodes` (night owl vs. early bird, weekday vs. weekend commits, spaces vs. tabs, naming conventions, average commit and line sizes) with fallback calculations from commit records.
- **Nginx Reverse Proxy Service**: Production-ready reverse proxy with gzip compression, security headers (`X-Frame-Options`, `X-Content-Type-Options`, `X-XSS-Protection`, `Referrer-Policy`), rate limiting zones (`general_limit` at 30 req/s, `api_limit` at 10 req/s), and SSL configuration readiness.
- **Automated Database Migrations**: Automatic schema initialization and verification on Go server startup using embedded SQL (`schema.sql`).
- **Structured Logging (`log/slog`)**: Migrated all logging across `main.go`, `api.go`, and `worker.go` to standard library `log/slog` with environment-aware formatting (text in development, JSON in production).
- **Template Hot-Reloading**: Enabled live template reloading from disk when `ENV=development` while retaining pre-parsed `embed.FS` performance in production.
- **Health Check Endpoint**: Added `/healthz` verifying live PostgreSQL connectivity and status.
- **Authentication & Ingestion Endpoints**:
  - `GET /auth/logout` endpoint for session termination and cookie clearing.
  - `DELETE /api/commits` endpoint for purging repository commits during re-indexing.
  - `POST /api/distances` endpoint supporting developer closeness and repo proximity metrics.
- **Automated Unit Test Suite**: Comprehensive tests in `main_test.go` covering cookie signing/verification, session extraction, API authentication middleware, logout handling, and SVG badge generation.
- **Application-Level Request Throttling**: Integrated `chi/middleware.ThrottleBacklog` on backend router to protect against concurrency spikes.
- **Comprehensive Environment Template**: `.env.example` documenting all configuration keys.

### Security Fixes
- **OAuth CSRF Protection (C1)**: Replaced hardcoded OAuth state with cryptographically secure random 32-byte state tokens stored in `oauth_state` cookies and verified using constant-time comparison (`hmac.Equal`).
- **Cryptographically Signed Session Cookies (C2)**: Implemented HMAC-SHA256 cookie signing and verification with `HttpOnly`, `SameSite=Lax`, and configurable `SESSION_SECRET` to prevent session forgery and email tampering.
- **PostgreSQL Port Isolation (C3)**: Removed host port exposure (`5432:5432`) from `docker-compose.yml`, restricting database access exclusively to the internal Docker network.
- **Credential Externalization (C4)**: Replaced hardcoded database and service credentials across Docker configuration with environment variable substitutions.
- **Sentry DSN Secret Sanitization (C5)**: Removed hardcoded Sentry DSN token in `cli/build.gradle`, switching to dynamic `System.getenv('SENTRY_DSN')` resolution.
- **API Ingestion Authentication (H1)**: Added `apiAuthMiddleware` enforcing internal authentication tokens via `Authorization: Bearer <token>` headers or `Token` cookies on all `/api/*` endpoints.
- **HTMX CDN SRI Integrity Hash (M2)**: Corrected Subresource Integrity (`integrity`) SHA-384 hash (`D1Kt99CQMDuVetoL1lrYwg5t+9QdHe7NLX/SoJYkXDFfX37iInKRy5xLSi8nO7UC`) on `dist/htmx.min.js` to resolve browser script blocking that caused infinite dashboard loading skeletons.
- **Docker Compose Container Hardening**: Configured PostgreSQL `pg_isready` health checks and ensured backend container waits for `service_healthy`.

### Bug Fixes & Improvements
- **Non-Blocking Worker Job Queue (H2)**: Implemented non-blocking job dispatcher (`EnqueueJob`) with channel `select/default` fallback to prevent goroutines from hanging when the queue capacity (100) is exceeded.
- **Mock Authentication Removal (H3)**: Removed deprecated `--password dummy` argument passed to the headless Java CLI worker.
- **Missing Reverse Proxy & TLS (H4)**: Added Nginx reverse proxy service routing port 80/443 traffic with reverse proxy headers (`X-Real-IP`, `X-Forwarded-For`, `X-Forwarded-Proto`).
- **Template User Session Binding (H5)**: Implemented `getSessionEmail()` helper to pass active user identity to templates and correctly display login/logout states.
- **Database Transaction Error Handling (H6)**: Added explicit error checking on `tx.Commit()` in all protobuf ingestion handlers (`/api/commits`, `/api/facts`, `/api/authors`), returning HTTP 500 on commit failure.
- **CORS Handling (H7)**: Added CORS middleware handling preflight `OPTIONS` requests and required `Access-Control-*` response headers.
- **Database Query Error Logging (M3)**: Added structured error logs for all database queries and transactions.
- **Database Performance Indexing (M4)**: Added index `idx_commits_author_email` on `commits(author_email)` and `idx_facts_email` / `idx_facts_email_code` on `facts(email)` to eliminate full table scans during author metric lookups.
- **Development Script Hardening (M5)**: Updated `backend/run.sh` to run `go run .` with automatic database migration.
- **Repository Binary Cleanup (M6)**: Added `sourcerer-backend` binary artifacts to `.gitignore`.
- **Dashboard Polling Optimization (M7)**: Reduced HTMX polling frequency from every 5 seconds to every 30 seconds to minimize database query load.
- **Full Git Clone Depth (L5)**: Removed `--depth 1` shallow clone constraint in worker subprocess execution to ensure accurate and complete contributor commit histories.
