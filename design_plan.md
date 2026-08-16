# Sourcerer Features — Design & Implementation Plan

## Phase 1 & 2: Repository Hall of Fame (Contributor Showcase & README Badge)

### 1. Architectural Overview
The **Hall of Fame** feature automates contributor recognition for indexed repositories by aggregating commit activity across lifetime and sliding time windows (e.g., 7 days or relative active intervals), dividing contributors into three clear tiers:
- **🌟 Top Contributors**: Lifetime highest commit and code contributors.
- **🔥 Trending Contributors**: Contributors with the highest velocity in the recent active window.
- **🌱 New Contributors**: Contributors whose initial commit in the repository occurred during the recent active window.

```
+-----------------------------------------------------------------------------------+
|  ⚡ SOURCERER HALL OF FAME  •  repo/identifier                                   |
+-----------------------------------------------------------------------------------+
|  🌟 TOP CONTRIBUTORS         🔥 TRENDING (Recent)        🌱 NEW CONTRIBUTORS      |
|  [AVATAR] Alice (250 commits) [AVATAR] Bob (18 commits)   [AVATAR] Carol (3 comm) |
|  [AVATAR] Dave  (180 commits) [AVATAR] Alice (12 commits) [AVATAR] Eve   (1 comm) |
+-----------------------------------------------------------------------------------+
|  AST LANGUAGES: TypeScript 55%  •  Go 30%  •  Python 15%                          |
+-----------------------------------------------------------------------------------+
```

---

### 2. Endpoints & Interfaces

1. **`GET /hall-of-fame/{repo}.svg` (and `GET /r/{repo}.svg`)**:
   - Dynamic SVG generator tailored with *Deep Violet Cyber-Intelligence* aesthetics (`#090514` dark surface, `#8b5cf6` glowing borders, `#10b981` / `#f59e0b` / `#06b6d4` tier badges).
   - Generates compact or expanded visual badges for GitHub `README.md`.
   - Embed snippet: `[![Hall of Fame](https://<host>/hall-of-fame/<repo>.svg)](https://<host>/r/<repo>)`
   - Cache headers: `Cache-Control: public, max-age=1800`.

2. **`GET /r/{repo}` (and `GET /hall-of-fame/{repo}`)**:
   - Public repository showcase page.
   - Header with repository metadata (total commits, lines added, lines deleted, unique contributors).
   - 3-column interactive tier cards with avatars, contributor links (`/p/{email}`), halos for Sourcerer users, and commit counts.
   - Technology stack breakdown.
   - Interactive **README Embed Hub**: One-click copy Markdown snippet, one-click copy HTML snippet, live SVG preview, and instant toast notifications.

3. **`GET /api/hall-of-fame/{repo}`**:
   - Structured JSON API returning:
     ```json
     {
       "repo_rehash": "string",
       "total_commits": 120,
       "total_lines_added": 45000,
       "total_lines_deleted": 8000,
       "total_contributors": 14,
       "top": [{ "email": "...", "name": "...", "commits": 50, "lines": 20000 }],
       "trending": [{ "email": "...", "name": "...", "commits": 12 }],
       "new": [{ "email": "...", "name": "...", "first_commit": 1723700000 }],
       "languages": [{ "tech": "Go", "lines": 35000, "percentage": 70 }]
     }
     ```

4. **Dashboard Integration**:
   - In `backend/templates/repos.html`: Add direct `"🌟 Hall of Fame"` and `"Embed Widget"` action buttons to repository cards.
   - In `backend/templates/index.html`: Add Hall of Fame quick modal / preview panel.

---

### 3. Database Queries & Performance

- **Composite index verification**:
  - `commits(repo_rehash, date)` for ultra-fast sliding time window aggregations.
  - `commits(repo_rehash, author_email)` for contributor commit grouping.
- **Dynamic window fallback**:
  - If a repository has no commits within the last 7 days, the query dynamically evaluates the last active 30-day epoch or repository peak activity window to guarantee the widget always renders meaningful telemetry.

---

### 4. Implementation Steps

1. **Backend Service Layer (`backend/main.go`)**:
   - Implement `HallOfFameData`, `ContributorStat`, and `getHallOfFameData(repoRehash string) HallOfFameData`.
   - Implement `generateHallOfFameSVG(data HallOfFameData) []byte`.
   - Register routes for `/hall-of-fame/{repo}.svg`, `/hall-of-fame/{repo}`, `/r/{repo}`, `/r/{repo}.svg`, and `/api/hall-of-fame/{repo}`.
2. **HTML Templates**:
   - Create `backend/templates/repo.html` (the full public repository Hall of Fame showcase page).
   - Update `backend/templates/repos.html` to link directly to `/r/{rehash}`.
3. **Automated Testing (`backend/main_test.go`)**:
   - Add unit tests verifying `generateHallOfFameSVG`, JSON serialization, and route handling.
4. **Build & Verify**:
   - Run `go test -v ./...`, rebuild Docker container, verify all live endpoints via curl.
