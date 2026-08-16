# Sourcerer App — System Documentation

> Comprehensive architecture, design, API reference, data schema, and deployment guide for the Sourcerer platform.

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

### 2.4 Data Store (`PostgreSQL 15`)
- Relational schema storing users, indexed repositories, commit histories, per-language technology statistics, facts/coding habits, and author records.
- Automated schema migrations applied during application initialization.

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
- **Capacity**: Channel-buffered job queue (`chan Job`, 100 capacity).
- **Concurrency**: Worker routines process repositories sequentially per container to manage system memory and CPU during intensive AST parsing.
- **Safety**: Non-blocking `EnqueueJob` with `select/default` semantics prevents incoming OAuth webhooks from blocking if queue capacity is saturated.
- **Full History Analysis**: Worker executes full depth clones (without shallow clone depth restrictions) to construct complete historical metrics.

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
    initial_commit_rehash TEXT
);

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

CREATE TABLE IF NOT EXISTS authors (
    email TEXT PRIMARY KEY,
    name TEXT,
    repo_rehash TEXT REFERENCES repos(rehash)
);
```

### Fact Codes Reference
Facts store computed behavioral attributes per author and repository:
- **Code 100 (Work Habits)**: Commit distribution across time of day (`value1` = daytime commits, `value2` = nighttime commits). Used to determine "Night Owl" vs. "Early Bird" style.
- **Code 101 (Weekend Activity)**: Commit frequency comparing weekdays vs. weekends (`value1` = weekday count, `value2` = weekend count).
- **Code 200 (Code Style - Indentation)**: Indentation style (`value1` = spaces count, `value2` = tabs count).
- **Code 201 (Code Style - Variable Naming)**: Identifier casing conventions (`value1` = `snake_case` count, `value2` = `camelCase` count).
- **Code 300 (Commit Granularity)**: Average lines changed per commit and aggregate commit size distributions.

---

## 5. API Reference

All ingestion endpoints require internal authentication via the `Authorization: Bearer <API_INTERNAL_TOKEN>` header or a valid `Token` session cookie.

### 5.1 Protobuf Ingestion Endpoints

| Method | Endpoint | Description | Payload Type |
|---|---|---|---|
| `POST` | `/api/auth` | Exchange CLI token / initialize ingestion session | JSON / Form |
| `GET` | `/api/user` | Fetch active user profile model | Protobuf (`app.User`) |
| `POST` | `/api/user` | Register or update user record | Protobuf (`app.User`) |
| `POST` | `/api/repo` | Register repository metadata | Protobuf (`app.Repo`) |
| `POST` | `/api/commits` | Ingest batch of commits & language stats | Protobuf (`app.CommitGroup`) |
| `DELETE`| `/api/commits` | Purge commits for a repository | Query Param `rehash` |
| `POST` | `/api/facts` | Ingest computed code facts & habits | Protobuf (`app.FactGroup`) |
| `POST` | `/api/authors` | Ingest author mappings | Protobuf (`app.AuthorGroup`) |
| `POST` | `/api/distances`| Ingest developer proximity & distance metrics | Protobuf (`app.Distances`) |
| `POST` | `/api/process` | Check or update asynchronous ingestion status | JSON |

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

---

## 6. Frontend & Visual Profiles

The frontend is constructed with Go templates, HTMX, and modern CSS without heavy client-side JavaScript frameworks.

### 6.1 Interactive HTMX Dashboard (`/`)
- **Summary Metrics (`/dashboard/stats?email=...`)**: Real-time aggregation of total commits, total lines added, and lines deleted.
- **Language Distribution (`/dashboard/languages?email=...`)**: Dynamic percentage progress bars showing language breakdowns with color coding.
- **Repository List (`/dashboard/repos?email=...`)**: Ingested repository cards displaying commit volumes and line stats.
- **Coding Habits & Facts (`/dashboard/facts?email=...`)**: Visual breakdown of developer traits (Night Owl / Early Bird, Weekday Warrior / Weekend Hacker, Spaces vs Tabs, CamelCase vs Snake_case, Average Commit Size).
- **Auto-Refresh**: Smooth background polling via HTMX (`hx-trigger="every 30s"`).

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

### 7.1 Cryptographic Session Management
- Sessions use signed HMAC-SHA256 cookies (`session=<base64_payload>.<hex_hmac>`).
- Cookies are configured with security attributes: `HttpOnly`, `SameSite=Lax`, and `Path=/`.
- Cryptographic verification uses constant-time comparison (`hmac.Equal`) to eliminate timing attacks.

### 7.2 OAuth CSRF Protection
- `/auth/github/login` generates a cryptographically random 32-byte state token stored in a short-lived `oauth_state` cookie.
- `/auth/github/callback` verifies the returned state parameter against the cookie using constant-time comparison before exchanging tokens.

### 7.3 Ingestion API Authentication
- Protected by `apiAuthMiddleware`. All requests to `/api/*` must present a matching `API_INTERNAL_TOKEN` via `Authorization: Bearer` or the `Token` cookie.

### 7.4 Traffic Throttling & Rate Limiting
- **Edge Throttling**: Nginx enforces rate limit zones (`general_limit: 30r/s`, `api_limit: 10r/s`).
- **Application Throttling**: Chi middleware (`middleware.ThrottleBacklog(100, 50, 5s)`) prevents thread pool exhaustion during request bursts.

### 7.5 Structured Logging & Graceful Shutdown
- Standard library `log/slog` structured logging formats logs as clean text in development and structured JSON in production.
- Handles `SIGINT` and `SIGTERM` signals with a 5-second graceful shutdown timeout to allow active database transactions and HTTP requests to complete.

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
| `SESSION_SECRET` | Yes | Auto-generated | 32-byte hex secret for signing session cookies |
| `API_INTERNAL_TOKEN` | Yes | - | Secret token securing `/api/*` ingestion routes |

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

For a complete record of all versions, bug fixes, security patches, and feature additions, please refer to [CHANGELOG.md](file:///Users/igmrrf/Desktop/tmp/sourcerer-app/CHANGELOG.md).
