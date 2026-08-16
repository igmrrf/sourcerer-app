# Changelog

All notable changes to the Sourcerer App will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

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
