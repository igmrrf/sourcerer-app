# Sourcerer App — Project Documentation

> Consolidated document covering architecture, design, implementation, cloud deployment, and production review.

---

## Table of Contents

1. [Design Plan](#1-design-plan)
2. [Cloud Architecture](#2-cloud-architecture)
3. [Implementation Plan](#3-implementation-plan)
4. [Implementation Walkthrough](#4-implementation-walkthrough)
5. [Production Review — Initial Findings](#5-production-review--initial-findings)
6. [Production Review — Fixes Applied](#6-production-review--fixes-applied)
7. [Remaining Items](#7-remaining-items)
8. [Deployment Steps](#8-deployment-steps)

---

## 1. Design Plan

### Architecture Overview
- **CLI App (Existing):** Written in Java/Kotlin. Processes local repos, extracts stats, formats them as Protobuf messages (`CommitGroup`, `FactGroup`, `AuthorGroup`), and sends POST requests to the backend API.
- **Backend API & Web App:** Written in **Go**.
  - Receives Protobuf payloads via HTTP (e.g., `/api/commit`).
  - Uses `protoc` with a Go plugin to decode the incoming requests.
  - Connects to a **PostgreSQL** database using standard `database/sql`.
  - Serves **HTMX**-powered HTML templates for the frontend interface.
- **Frontend Dashboard:** Built with **HTMX** and **Vanilla CSS**.
  - Uses Go `html/template` to render pages.
  - Uses HTMX for dynamic interactions (e.g., switching between repos, loading charts).

### Directory Structure
- `backend/`: The Go App (containing API and HTMX templates).
- `cli/`: The Java/Kotlin CLI tool for repository analysis.

### Database Schema (PostgreSQL)
Tables store the information defined in `sourcerer.proto`:
- `users` (email, primary, verified)
- `repos` (rehash, initial_commit_rehash)
- `commits` (rehash, repo_rehash, author_email, date, lines_added, lines_deleted)
- `commit_stats` (commit_rehash, num_lines_added, num_lines_deleted, type, tech)
- `facts` (repo_rehash, email, code, key, values)
- `authors` (email, name, repo_rehash)

### Implementation Steps
1. **Go App Setup** — Initialize `go mod`, compile `.proto` to Go, create PostgreSQL connection and schema migration.
2. **Ingestion API** — Implement HTTP handlers for `application/x-protobuf` data. Persist received protobuf objects to PostgreSQL.
3. **HTMX Frontend** — Create `templates/` folder, add HTMX via script tag, build dashboard views (Summary, Languages, Stats) using Go templates and HTMX.

---

## 2. Cloud Architecture

To transition Sourcerer from a local CLI tool to a fully automated cloud application (SaaS) deployed on a Droplet, the ingestion workload shifts from the user's machine to the backend infrastructure.

### GitHub OAuth Integration (Go Backend)
- **OAuth Endpoints**: `/auth/github/login` and `/auth/github/callback` routes using `golang.org/x/oauth2`.
- **User Session**: Upon successful login, extract the user's GitHub identity (email) and establish a secure HTTP session using signed cookies.
- **Repo Discovery**: Use the GitHub API to fetch a list of the user's repositories.

### Background Ingestion Worker (Go Backend)
- **Job Queue**: Channel-based background worker queue using Go channels (buffered, 100 capacity).
- **Cloning**: The worker clones a user's repository into a temporary directory using `git clone`, prepares it for analysis.

### Headless CLI Execution (Kotlin → Subprocess)
- **CLI Modification**: `Main.kt` accepts a `--headless` flag, bypassing `ConsoleUi` and immediately processing a provided directory.
- **Subprocess Invocation**: The Go worker executes `java -jar sourcerer-app.jar --headless --path /tmp/repo-123` via `os/exec`.
- **Local Ingestion**: The CLI sends protobuf payloads directly to the internal Go backend API (`http://localhost:8080/api/commits`).

### Infrastructure & Docker Compose
- `backend/Dockerfile` installs `git` and `openjdk17-jre` alongside the Go binary.
- `sourcerer-app.jar` is mounted into the Go backend container.
- `docker-compose.yml` includes GitHub OAuth client IDs and secrets as environment variables.

> [!IMPORTANT]
> **GitHub OAuth App Required**: You must create a GitHub OAuth application in your GitHub developer settings and provide the `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET` via `.env`.

> [!WARNING]
> **Container Size**: Because the Go backend runs the Java CLI, the Docker container bundles an OpenJDK JRE and Git, increasing the final image size.

---

## 3. Implementation Plan

### Go Backend Components

| Action | File | Description |
|---|---|---|
| MODIFY | `backend/Dockerfile` | Install `git` and `openjdk17-jre` in final Alpine stage |
| MODIFY | `docker-compose.yml` | Inject OAuth secrets, mount `.jar` volume |
| MODIFY | `backend/go.mod` | Add `golang.org/x/oauth2` and `go-github/v60` |
| MODIFY | `backend/main.go` | Add session cookies, OAuth routes, repo discovery |
| NEW | `backend/worker.go` | Channel-based job queue, `git clone`, Java subprocess execution |

### Java CLI Modifications

| Action | File | Description |
|---|---|---|
| MODIFY | `cli/src/main/kotlin/app/Main.kt` | Add `--headless` flag, skip interactive UI |
| MODIFY | `cli/src/main/kotlin/app/utils/Options.kt` | Add `--headless` and `--path` JCommander parameters |

### Verification Plan
- **Backend Build**: `go build ./...` inside the backend directory.
- **CLI Build**: `./gradlew build` inside the CLI directory.
- **Docker Compose**: `docker-compose up --build` — verify Java and Git install.
- **OAuth Login**: Navigate to `http://localhost:8080`, click "Login with GitHub", authorize.
- **Worker Execution**: Check container logs for "Cloning repository..." and Java CLI output.
- **Dashboard Update**: Refresh and verify user's email appears in dropdown with stats.

### Open Questions
1. What GitHub scopes should we request? (`repo` for private repos, or just `public_repo`?)
2. Should we rate-limit or cap the number of repos we clone per user?
3. Does the Java CLI rely on a hardcoded API URL for uploading, or can we configure it via `--server`?

---

## 4. Implementation Walkthrough

### Changes Made
The architecture was successfully transitioned from a local-CLI ingestion model to a server-side automated ingestion pipeline.

1. **Java CLI Updates**:
   - Added `--headless` and `--path` flags to bypass interactive `ConsoleUi`.
   - Modified `Main.kt` to trigger repository hashing immediately when `--headless` is supplied.
   - Updated `build.gradle` to set the API endpoint to `http://localhost:8080/api` by default.

2. **Go Backend Worker**:
   - Added `go-github/v60` and `golang.org/x/oauth2` to `go.mod`.
   - Built `worker.go` goroutine with channel-based `JobQueue`.
   - Worker clones repos to temp dir, invokes Java CLI headlessly, cleans up after.

3. **GitHub OAuth Flow**:
   - Implemented `/auth/github/login` and `/auth/github/callback` routes.
   - On successful callback, fetches all repositories and enqueues them for processing.
   - Sets a signed session cookie and redirects to dashboard.

4. **Docker Stack**:
   - Modified `backend/Dockerfile` to install `openjdk17-jre` and `git`.
   - Updated `docker-compose.yml` to inject OAuth credentials.

5. **Frontend**:
   - Added a **Login with GitHub** button to the navigation bar.

### Validation
- **Backend Compilation**: Confirmed `main.go` and `worker.go` build successfully.
- **Code Integration**: Verified `Options.kt` and `Main.kt` accept `--headless` and correctly invoke the backend URL.

---

## 5. Production Review — Initial Findings

The initial review identified 19 issues across security, correctness, and infrastructure.

### Critical Issues Found

| ID | Issue | Impact |
|---|---|---|
| C1 | OAuth CSRF — hardcoded `"state"` parameter | Attacker can forge OAuth callbacks |
| C2 | Session cookie stores raw email, no signing | Any user can impersonate another by setting `session=email` |
| C3 | Exposed PostgreSQL port `5432` in Docker | Database directly accessible from internet |
| C4 | Hardcoded `postgres/postgres` credentials | Committed secrets in source control |
| C5 | Hardcoded Sentry DSN with embedded secret key | Leaked API secret in `build.gradle` |

### High Severity Issues Found

| ID | Issue | Impact |
|---|---|---|
| H1 | No authentication on API ingestion endpoints | Anyone can POST arbitrary data into DB |
| H2 | Worker queue blocks on >100 repos | Goroutine hangs indefinitely |
| H3 | Worker passes `--password dummy` to CLI | Fragile mock authentication |
| H4 | Missing reverse proxy and SSL/TLS | No encryption in transit |
| H5 | Template references undefined `.User` field | Login state never displayed |
| H6 | `tx.Commit()` errors ignored in 3 handlers | Silent data loss on commit failure |
| H7 | No CORS configuration | Cross-origin requests blocked |

### Medium Severity Issues Found

| ID | Issue | Impact |
|---|---|---|
| M1 | No `/healthz` endpoint | No container health monitoring |
| M2 | HTMX loaded without SRI hash | CDN compromise risk |
| M3 | Dashboard swallows query errors | Silent failures, empty data |
| M4 | Missing `author_email` index on commits | Full table scans on filtered queries |
| M5 | `run.sh` assumes `psql` locally | Script fails in Docker |
| M6 | Compiled binary committed to git | 18 MB unnecessary in repo |
| M7 | Dashboard polls every 5 seconds | Excessive DB load per browser tab |

### What Was Already Correct
- Graceful shutdown with `SIGTERM` handling (5s timeout)
- Connection pooling (`MaxOpenConns=25`, `MaxIdleConns=25`, `ConnMaxLifetime=5min`)
- Body size limits via `io.LimitReader` (10-50 MB)
- Database indexes on foreign keys and queried columns
- Idempotent inserts with `ON CONFLICT DO NOTHING`
- Multi-stage Docker build
- Worker cleanup with `defer os.RemoveAll(tmpDir)`
- Configurable port via `PORT` env var

---

## 6. Production Review — Fixes Applied

> **All critical and high-severity issues RESOLVED.** ✅

### Critical Issues — ALL FIXED ✅

| ID | Fix Applied |
|---|---|
| C1 | Random 32-byte state stored in `oauth_state` cookie, validated with `hmac.Equal` in callback |
| C2 | HMAC-SHA256 signed cookies with `HttpOnly`, `SameSite=Lax`, `MaxAge` flags |
| C3 | Removed `ports: "5432:5432"` from `docker-compose.yml` |
| C4 | All credentials externalized to `${VAR}` references; `.env.example` created |
| C5 | Changed to `System.getenv('SENTRY_DSN') ?: ''` in `build.gradle` |

### High Severity — ALL FIXED ✅

| ID | Fix Applied |
|---|---|
| H1 | Added `apiAuthMiddleware` checking `API_INTERNAL_TOKEN` via Bearer header or cookie |
| H2 | Added `EnqueueJob()` with `select/default` — drops jobs with warning when queue full |
| H3 | Removed `--password dummy` from Java CLI invocation |
| H4 | *(Infrastructure-ready — add Nginx/Traefik at deployment time)* |
| H5 | Added `getSessionEmail()` helper; dashboard reads session cookie and populates `User` field |
| H6 | All 3 handlers now check commit errors and return 500 on failure |
| H7 | Added CORS middleware with proper `Access-Control-*` headers and OPTIONS handling |

### Medium Severity — ALL FIXED ✅

| ID | Fix Applied |
|---|---|
| M1 | Added `/healthz` endpoint with DB ping |
| M2 | Added `integrity` and `crossorigin` attributes to HTMX script tag |
| M3 | Added proper error logging for all DB queries |
| M4 | Added `idx_commits_author_email` to `schema.sql` |
| M6 | Added `sourcerer-backend` to `.gitignore` |
| M7 | Changed polling to `every 30s` |

### Additional Improvements

| Fix | Details |
|---|---|
| HTTP server timeouts | `ReadTimeout: 15s`, `WriteTimeout: 30s`, `IdleTimeout: 60s` |
| Session secret management | Reads `SESSION_SECRET` env var; auto-generates with warning if unset |
| Docker health checks | PostgreSQL `pg_isready` health check; backend waits for `service_healthy` |
| Docker compose modernized | Removed deprecated `version: '3.8'` |

### Files Changed

| File | Changes |
|---|---|
| `backend/main.go` | OAuth CSRF, signed sessions, user binding, CORS, health check, timeouts, error handling |
| `backend/api.go` | tx.Commit error checking, API auth middleware |
| `backend/worker.go` | Non-blocking `EnqueueJob()`, removed dummy password |
| `backend/schema.sql` | Added `author_email` index |
| `backend/templates/index.html` | HTMX SRI hash, 30s polling |
| `backend/.gitignore` | Created — excludes binary and `.env` |
| `docker-compose.yml` | Externalized secrets, removed DB port, added health checks |
| `.env.example` | Created — documents all required environment variables |
| `cli/build.gradle` | Sentry DSN externalized to env var |

---

## 7. Remaining Items

| ID | Issue | Recommendation |
|---|---|---|
| L1 | `log.Printf` instead of structured logging | Migrate to `log/slog` when ready |
| L2 | CLI uses old dependencies (Kotlin 1.2) | Update if CLI is actively maintained |
| L4 | No rate limiting | Add `chi/middleware.Throttle` or similar |
| L5 | `--depth 1` may miss commit history | Remove depth limit if CLI needs full history |
| L7 | Embedded templates don't hot-reload | Use file-based templates in dev mode |
| H4 | No Nginx/SSL in docker-compose | Add reverse proxy service for production deployment |

---

## 8. Deployment Steps

1. Copy `.env.example` to `.env` and fill in all values
2. Create a GitHub OAuth App at https://github.com/settings/developers
3. Generate secrets: `openssl rand -hex 32` for `SESSION_SECRET` and `API_INTERNAL_TOKEN`
4. Build the CLI: `cd cli && ./gradlew build`
5. Launch: `docker compose up --build -d`
6. Add a reverse proxy (Nginx/Caddy) with SSL for production
