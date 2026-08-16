# Sourcerer Frontend Redesign — Modern Purple Design System

## 1. Aesthetic Thesis & Direction

- **Aesthetic Stance**: *Deep Violet Cyber-Intelligence* — a modern, high-craft developer analytics UI combining rich purple tones (`#8b5cf6`, `#7c3aed`, `#090514`), glassmorphism, ambient luminescent gradients, and crisp telemetry typography (`Plus Jakarta Sans` & `JetBrains Mono`).
- **DFII Score**: `14/15` (Aesthetic: 5, Fit: 5, Feasibility: 5, Performance: 5, Consistency Risk: 1) -> **Excellent: Execute fully**.
- **Differentiation Anchor**: Precision telemetry data cards with glowing purple accents, ambient radial mesh backdrops, interactive copy-to-clipboard embed hub, and responsive grid layouts designed for mobile and desktop.

---

## 2. Design System Tokens & Tokens Architecture

### Color Palette
- **Canvas Base**: `--bg-base: #090514`
- **Surface**: `--bg-surface: #120d24`
- **Surface Raised / Hover**: `--bg-surface-raised: #1c1538`
- **Glass Card Background**: `--bg-glass: rgba(18, 13, 36, 0.72)` with `backdrop-filter: blur(16px)`
- **Primary Purple Scale**:
  - Primary Glow: `--primary-glow: rgba(139, 92, 246, 0.28)`
  - Primary Bright: `--primary-400: #a855f7`
  - Primary Base: `--primary-500: #8b5cf6`
  - Primary Dark: `--primary-600: #7c3aed`
- **Semantic Accents**:
  - Success / Added: `--accent-emerald: #10b981` (Glow: `rgba(16, 185, 129, 0.15)`)
  - Danger / Deleted: `--accent-rose: #f43f5e` (Glow: `rgba(244, 63, 94, 0.15)`)
  - Warning / Activity: `--accent-amber: #f59e0b`
  - Tech / Cyan: `--accent-cyan: #06b6d4`
- **Text & Contrast**:
  - Text Primary: `#f8fafc`
  - Text Secondary: `#cbd5e1`
  - Text Muted: `#94a3b8`
  - Text Dim: `#64748b`
- **Borders & Dividers**:
  - Border Subtle: `rgba(139, 92, 246, 0.15)`
  - Border Card: `rgba(139, 92, 246, 0.22)`
  - Border Hover: `rgba(168, 85, 247, 0.5)`

### Typography
- **Headings & Brand**: `'Plus Jakarta Sans', -apple-system, sans-serif` (Weights: 600, 700, 800)
- **Body & UI**: `'Plus Jakarta Sans', -apple-system, sans-serif` (Weights: 400, 500, 600)
- **Monospace Telemetry**: `'JetBrains Mono', ui-monospace, monospace` (For metrics, commit counts, diff numbers, badges)

---

## 3. Views to Upgrade

1. **`backend/templates/index.html`** (Main Dashboard)
   - Ambient purple background mesh glow.
   - Glassmorphic top navigation with responsive action bar.
   - Custom styled contributor dropdown with purple highlight.
   - Responsive CSS grid for HTMX dynamic cards with shimmer skeletons.
   - Mobile-first responsiveness with flexible headers and touch-friendly controls.

2. **`backend/templates/stats.html`** (Overall Commits & Velocity Partial)
   - Large hero stat with purple gradient text and monospace telemetry.
   - Added / Deleted diff counter cards with glowing indicator pills.

3. **`backend/templates/languages.html`** (Top Languages Partial)
   - Multi-color glowing segmented progress bar with purple dominant spectrum.
   - Interactive language list with line counts, percentage tags, and color dots.

4. **`backend/templates/facts.html`** (Coding Habits & Behavioral Facts Partial)
   - Cyber-intelligence cards with themed icons, glowing badges, and descriptive subtitles.

5. **`backend/templates/repos.html`** (Analyzed Repositories Partial)
   - Repository cards with custom commit badges, line count meters, and hash indicators.

6. **`backend/templates/profile.html`** (Public Developer Showcase)
   - Hero header with avatar glow, verified badge, copy-link button.
   - Full statistics overview, language chart, coding facts, repo list.
   - Embed Hub: Live interactive SVG preview, copy Markdown button, copy HTML button with real-time toast feedback.

7. **`backend/main.go`** (SVG Badge Generation)
   - Cohesive purple theme for GitHub README badge output (`#090514` background, `#2d1f60` borders, `#a855f7` accents).
