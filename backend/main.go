package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/go-github/v60/github"
	"github.com/lib/pq"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
)

// Configure OAuth
var sessionSecret []byte

// sessionTTL bounds how long a signed session cookie stays valid. The expiry is
// part of the signed payload, so a stolen cookie cannot outlive it even though
// sessions are stateless.
const sessionTTL = 24 * time.Hour

// signCookie signs "<value>|<expiryUnix>" so that verification is
// self-contained: tampering breaks the MAC and replay stops at the expiry.
func signCookie(value string) string {
	return signCookieAt(value, time.Now().Add(sessionTTL))
}

func signCookieAt(value string, expiry time.Time) string {
	payload := value + "|" + strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))
	encoded := base64.URLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + signature
}

func verifyCookie(signed string) (string, bool) {
	parts := strings.SplitN(signed, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	decoded, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write(decoded)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return "", false
	}

	payload := string(decoded)
	sep := strings.LastIndex(payload, "|")
	if sep < 0 {
		// Unversioned cookie issued before expiries were signed in.
		return "", false
	}
	expiryUnix, err := strconv.ParseInt(payload[sep+1:], 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() >= expiryUnix {
		return "", false
	}
	return payload[:sep], true
}

func getSessionEmail(r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	email, ok := verifyCookie(cookie.Value)
	if !ok {
		return ""
	}
	return email
}

var oauthConfig = &oauth2.Config{
	ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
	ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
	Scopes:       []string{"public_repo", "user:email"},
	Endpoint:     githuboauth.Endpoint,
}

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// assetVersion fingerprints the bundled stylesheet so a redeploy invalidates
// the browser cache without anyone remembering to bump a number.
var assetVersion = fingerprintAssets()

func fingerprintAssets() string {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(css)
	return hex.EncodeToString(sum[:])[:12]
}

//go:embed schema.sql
var schemaSQL string

//go:embed data/technologies.json
var technologiesJSON []byte

var (
	prodTmpl *template.Template
	db       *sql.DB
)

// seriesPalette is the categorical ramp used for language bars and dots. It
// matches --series-N in static/app.css; keep the two in step.
var seriesPalette = []template.CSS{"#7c3aed", "#2f4bff", "#0891b2", "#0e9f6e", "#c2660a", "#e5484d", "#64748b"}

var templateFuncs = template.FuncMap{
	"series": func(i int) template.CSS { return seriesPalette[i%len(seriesPalette)] },
	"add":    func(a, b int) int { return a + b },
	"sub":    func(a, b int) int { return a - b },
	"limitTokens": func(tokens []string, limit int) []string {
		if len(tokens) <= limit {
			return tokens
		}
		return tokens[:limit]
	},
	"hasMoreTokens": func(tokens []string, limit int) bool {
		return len(tokens) > limit
	},
	"moreTokenCount": func(tokens []string, limit int) int {
		if len(tokens) <= limit {
			return 0
		}
		return len(tokens) - limit
	},
	// asset builds a cache-busted URL for a file under static/.
	"asset": func(name string) string { return "/static/" + name + "?v=" + assetVersion },
	// pctOf renders a's share of a+b as a CSS percentage, used by the diff
	// ribbon. Returns 0 when there is nothing to divide.
	"pctOf": func(a, b int) string {
		total := a + b
		if total <= 0 {
			return "0"
		}
		return strconv.FormatFloat(float64(a)*100/float64(total), 'f', 2, 64)
	},
	// dec1 renders a float with a single decimal, for chart geometry that goes
	// straight into an attribute.
	"dec1": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
}

// staticHandler serves the stylesheet. Dev mode reads from disk so edits show
// up on reload; production serves the embedded copy with a long cache, which is
// safe because the URL carries the content fingerprint.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("Error opening embedded static assets", "error", err)
		os.Exit(1)
	}
	embedded := http.FileServer(http.FS(sub))
	onDisk := http.FileServer(http.Dir("static"))

	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDevMode() {
			w.Header().Set("Cache-Control", "no-store")
			if _, err := os.Stat("static"); err == nil {
				onDisk.ServeHTTP(w, r)
				return
			}
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		embedded.ServeHTTP(w, r)
	}))
}

func isDevMode() bool {
	env := strings.ToLower(os.Getenv("ENV"))
	if env == "development" || env == "dev" {
		return true
	}
	return false
}

// publicReadOnlyCORSPrefixes are the anonymous, read-only JSON endpoints that
// third-party pages (README embeds, dashboards) are meant to fetch. Everything
// else — session-backed dashboard partials and the ingestion API — gets no CORS
// headers at all, so a foreign origin can never read it.
var publicReadOnlyCORSPrefixes = []string{
	"/api/hall-of-fame/",
	"/api/libraries",
	"/fame/",
	"/api/face/hof",
	"/api/fame",
	"/api/sync-status",
}

func allowedCORSOrigins() map[string]bool {
	raw := os.Getenv("CORS_ALLOWED_ORIGINS")
	if raw == "" {
		return nil
	}
	allowed := make(map[string]bool)
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return allowed
}

var corsOrigins = allowedCORSOrigins()

func isPublicReadOnlyPath(path string) bool {
	for _, prefix := range publicReadOnlyCORSPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			switch {
			case corsOrigins[origin]:
				// Explicitly configured origin: credentials are permitted.
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Add("Vary", "Origin")
			case isPublicReadOnlyPath(r.URL.Path):
				// Anonymous public data only; never credentialed.
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy matches what the templates actually load: htmx from
// unpkg (SRI-pinned), Google Fonts, inline styles/scripts emitted by the
// templates, and same-origin SVG badges.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://unpkg.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// maskEmail reduces an address to a non-harvestable display form:
// "ada@example.com" becomes "ad***@example.com".
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		if len(email) <= 2 {
			return "***"
		}
		return email[:2] + "***"
	}
	local, domain := email[:at], email[at+1:]
	if len(local) <= 2 {
		return "***@" + domain
	}
	return local[:2] + "***@" + domain
}

// renderTemplate renders HTML templates, dynamically hot-reloading from disk in dev mode
// or using embedded templates in production mode for optimal performance.
func renderTemplate(w http.ResponseWriter, name string, data any) {
	t := prodTmpl
	if isDevMode() {
		// Hot-reload from disk when the templates are actually there. Running
		// the production image with ENV=development has no templates/ next to
		// the binary (they are embedded), so fall back rather than 500.
		if reloaded, err := template.New("").Funcs(templateFuncs).ParseGlob("templates/*.html"); err != nil {
			slog.Debug("Template hot reload unavailable, using embedded templates", "error", err)
		} else {
			t = reloaded
		}
	}

	if err := t.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("Failed to execute template", "template", name, "error", err)
		http.Error(w, "Template execution error", http.StatusInternalServerError)
	}
}

func main() {
	// Initialize structured logger (log/slog)
	var logHandler slog.Handler
	if isDevMode() {
		logHandler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	} else {
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(logHandler))

	secretStr := os.Getenv("SESSION_SECRET")
	if secretStr != "" {
		sessionSecret = []byte(secretStr)
	} else {
		if !isDevMode() {
			slog.Error("SESSION_SECRET must be set in production mode")
			os.Exit(1)
		}
		sessionSecret = make([]byte, 32)
		if _, err := rand.Read(sessionSecret); err != nil {
			slog.Error("Failed to generate session secret", "error", err)
			os.Exit(1)
		}
		slog.Warn("SESSION_SECRET not set. Generated a random one. Sessions will not persist across restarts.")
	}

	if !isDevMode() && os.Getenv("API_INTERNAL_TOKEN") == "" {
		slog.Error("API_INTERNAL_TOKEN must be set in production mode")
		os.Exit(1)
	}

	// Parse embedded production templates
	var err error
	prodTmpl, err = template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		slog.Error("Error parsing embedded templates", "error", err)
		os.Exit(1)
	}

	if isDevMode() {
		slog.Info("Running in development mode: template hot-reloading enabled from disk")
	} else {
		slog.Info("Running in production mode: using embedded templates")
	}

	// Connect to Postgres
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "host=localhost user=postgres password=postgres dbname=postgres sslmode=disable"
	}
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		slog.Error("Failed to open database connection", "error", err)
		os.Exit(1)
	}

	defer db.Close()

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		// In production an unreachable database means every request would
		// serve empty or broken data; fail fast so the orchestrator restarts
		// us once Postgres is actually up.
		if !isDevMode() {
			slog.Error("Cannot reach database on startup", "error", err)
			os.Exit(1)
		}
		slog.Warn("Could not ping database on startup. Ensure Postgres is running", "error", err)
	} else {
		slog.Info("Successfully connected to PostgreSQL")
		// Automatically apply database schema. The statements are idempotent
		// (CREATE ... IF NOT EXISTS / ADD COLUMN IF NOT EXISTS), so a failure
		// here means the schema is genuinely wrong — running on against it
		// would produce query errors on every request.
		if schemaSQL != "" {
			if _, err := db.Exec(schemaSQL); err != nil {
				slog.Error("Failed to apply database schema", "error", err)
				if !isDevMode() {
					os.Exit(1)
				}
			} else {
				slog.Info("Database schema verified and applied successfully")
			}
		}
		// Automatically seed Awesome Libraries catalog
		seedTechnologies(db)
	}

	api := &API{db: db}

	jarPath, err := resolveJarPath()
	if err != nil {
		slog.Error("Ingestion CLI unavailable. Build it with ./build_cli.sh (or `docker compose --profile build run --rm cli-build`).", "error", err)
		os.Exit(1)
	}
	slog.Info("Ingestion CLI located", "path", jarPath)

	// Start background worker
	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		StartWorker(ctxWorker)
	}()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Rate limiting / request throttling (protects backend against excessive concurrent requests)
	r.Use(middleware.ThrottleBacklog(100, 50, 5*time.Second))

	r.Use(securityHeadersMiddleware)
	r.Use(robotsHeaderMiddleware)
	r.Use(corsMiddleware)

	// API routes for Sourcerer CLI ingestion
	r.Mount("/api", api.Routes())

	// Bundled stylesheet and generated icons
	r.Handle("/static/*", staticHandler())

	// Crawler surfaces. Icons and the manifest live under /static but are also
	// aliased at the root, where browsers and crawlers look for them first.
	r.Get("/robots.txt", handleRobotsTxt)
	r.Get("/sitemap.xml", handleSitemapXML)
	for path, asset := range map[string]string{
		"/favicon.ico":          "favicon.ico",
		"/favicon.svg":          "favicon.svg",
		"/apple-touch-icon.png": "apple-touch-icon.png",
		"/site.webmanifest":     "site.webmanifest",
	} {
		name := asset
		r.Get(path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/static/"+name+"?v="+assetVersion, http.StatusMovedPermanently)
		})
	}

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			slog.Warn("Health check failed: database unreachable", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("db: unhealthy"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// OAuth & Auth Routes
	r.Get("/auth/github/login", handleGitHubLogin)
	r.Get("/auth/github/callback", handleGitHubCallback)
	r.Get("/auth/logout", handleLogout)

	// HTMX Dashboard Routes
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		sessionEmail := getSessionEmail(r)
		if sessionEmail == "" {
			// Signed-out visitors — including crawlers — get a real page here.
			// Redirecting straight into OAuth left the site with no indexable
			// front door at all.
			handleLanding(w, r)
			return
		}

		type Author struct {
			Email string
			Name  string
		}
		type UserInfo struct {
			Name            string
			Email           string
			PublicProfileID string
		}

		profileID, err := ensureProfileID(sessionEmail)
		if err != nil {
			slog.Error("Failed to resolve public profile id", "email", sessionEmail, "error", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		user := &UserInfo{Name: sessionEmail, Email: sessionEmail, PublicProfileID: profileID}
		authors := []Author{{Email: sessionEmail, Name: sessionEmail}}

		base := getPublicBaseURL(r)
		seo := newSEO(base, "/", "Sourcerer — engineering profiles from git history",
			"Sourcerer reads your git history and turns it into a profile: commit volume, language split, coding habits, recognized libraries and a shareable README badge.")
		// The dashboard is behind a session; only the marketing-facing root is
		// worth indexing, and this page is what a signed-in user sees.
		seo.NoIndex = true
		seo.JSONLD = newJSONLD(map[string]any{
			"@context":            "https://schema.org",
			"@type":               "SoftwareApplication",
			"name":                siteName,
			"url":                 base,
			"applicationCategory": "DeveloperApplication",
			"operatingSystem":     "Web",
			"description":         seo.Description,
		})
		seo.Nav = navFor(r, "overview")

		data := struct {
			Authors []Author
			User    *UserInfo
			SEO     SEOMeta
		}{
			Authors: authors,
			User:    user,
			SEO:     seo,
		}
		renderTemplate(w, "index.html", data)
	})

	r.Get("/dashboard/languages", func(w http.ResponseWriter, r *http.Request) {
		email := getSessionEmail(r)
		if email == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		renderTemplate(w, "languages.html", buildLanguageDonut(getLanguagesForEmail(email)))
	})

	// Repositories live on their own page rather than in an overview card:
	// there is more to say per repository than a panel has room for.
	r.Get("/repositories", handleRepositoriesPage)
	r.Get("/dashboard/repo-cards", handleDashboardRepoCards)

	r.Get("/dashboard/facts", handleDashboardFacts)
	r.Get("/dashboard/activity", handleDashboardActivity)
	r.Get("/dashboard/punchcard", handleDashboardPunchcard)

	// Public Sharable Profile Routes (e.g. /u/username or /p/email)
	r.Get("/u/{username}", handlePublicProfile)
	r.Get("/p/{email}", handlePublicProfile)

	// Dynamic SVG Profile Badge for GitHub READMEs
	r.Get("/badge/{identifier}", handleBadgeSVG)
	r.Get("/badge/{identifier}.svg", handleBadgeSVG)

	// Repository Hall of Fame Routes (Feature: hall-of-fame)
	r.Get("/hall-of-fame/{repo}", handleHallOfFameHTML)
	r.Get("/hall-of-fame/{repo}.svg", handleHallOfFameSVG)
	r.Get("/r/{repo}", handleHallOfFameHTML)
	r.Get("/r/{repo}.svg", handleHallOfFameSVG)
	r.Get("/api/hall-of-fame/{repo}", handleHallOfFameAPI)

	// Individual Contributor Fame Avatar Badges & Links (Feature: hall-of-fame widget)
	r.Get("/fame/{repo}/images/{num}", handleFameImage)
	r.Get("/fame/{owner}/{repo}/images/{num}", handleFameImage)
	r.Get("/fame/{user}/{owner}/{repo}/images/{num}", handleFameImage)
	r.Get("/fame/{repo}/links/{num}", handleFameLink)
	r.Get("/fame/{owner}/{repo}/links/{num}", handleFameLink)
	r.Get("/fame/{user}/{owner}/{repo}/links/{num}", handleFameLink)

	// Hall of Fame GitHub username matching, token, and management APIs
	r.Get("/api/face/hof/match", handleFaceHofMatch)
	r.Get("/api/hall-of-fame/match", handleFaceHofMatch)
	r.Get("/api/face/hof/token", handleFaceHofToken)
	r.Post("/api/face/hof/manage", handleFaceHofManage)
	r.Post("/api/fame/manage", handleFaceHofManage)

	// Framework & Library Hall of Fame Routes (Feature: library hall-of-fame)
	r.Get("/fame/lib/{tech}/images/{num}", handleLibraryFameImage)
	r.Get("/fame/lib/{tech}/links/{num}", handleLibraryFameLink)
	r.Get("/libraries/{tech}/fame/images/{num}", handleLibraryFameImage)
	r.Get("/libraries/{tech}/fame/links/{num}", handleLibraryFameLink)
	r.Get("/libraries/{tech}.svg", handleLibraryFameSVG)
	r.Get("/api/hall-of-fame/lib/{tech}", handleLibraryFameAPI)
	r.Get("/api/libraries/{tech}/fame", handleLibraryFameAPI)

	// Real-time Repository Sync Status
	r.Get("/dashboard/sync-status", handleDashboardSyncStatus)
	r.Get("/api/sync-status", handleAPISyncStatus)

	// Awesome Libraries Routes (Feature: awesome-libraries)
	r.Get("/libraries", handleLibrariesCatalogHTML)
	r.Get("/libraries/{tech}", handleLibraryDetailHTML)
	r.Get("/api/libraries", handleLibrariesCatalogAPI)
	r.Get("/api/libraries/{tech}", handleLibraryDetailAPI)
	r.Get("/dashboard/libraries", handleDashboardLibraries)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("Server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server listen error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	slog.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
	}

	// Let the ingestion worker finish the repository it is currently on rather
	// than killing a half-written clone. Jobs still queued are dropped; they
	// are re-enqueued on the user's next login.
	cancelWorker()
	select {
	case <-workerDone:
		slog.Info("Ingestion worker drained")
	case <-ctx.Done():
		slog.Warn("Ingestion worker did not drain before shutdown deadline")
	}

	slog.Info("Server exited cleanly")
}

// ensureProfileID returns the caller's stable public profile identifier,
// minting one on first use. Every public URL (/p, /u, /badge) is keyed on this
// value rather than on an email address.
func ensureProfileID(email string) (string, error) {
	var profileID string
	err := db.QueryRow("SELECT profile_id FROM public_profiles WHERE email = $1", email).Scan(&profileID)
	if err == nil && profileID != "" {
		return profileID, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	profileID = hex.EncodeToString(b)

	// Concurrent dashboard loads can race here; the upsert keeps whichever id
	// landed first and returns it.
	err = db.QueryRow(`
		INSERT INTO public_profiles (email, profile_id) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET profile_id = COALESCE(public_profiles.profile_id, EXCLUDED.profile_id)
		RETURNING profile_id`, email, profileID).Scan(&profileID)
	if err != nil {
		return "", err
	}
	return profileID, nil
}

func handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		slog.Error("Failed to generate OAuth state", "error", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(stateBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		HttpOnly: true,
		Secure:   !isDevMode(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
		Path:     "/",
	})

	url := oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil {
		slog.Warn("OAuth state cookie not found", "remote_addr", r.RemoteAddr)
		http.Error(w, "State cookie not found", http.StatusBadRequest)
		return
	}
	queryState := r.URL.Query().Get("state")
	if !hmac.Equal([]byte(stateCookie.Value), []byte(queryState)) {
		slog.Warn("OAuth CSRF state mismatch", "remote_addr", r.RemoteAddr)
		http.Error(w, "Invalid state", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		MaxAge: -1,
		Path:   "/",
	})

	code := r.URL.Query().Get("code")
	if code == "" {
		slog.Warn("OAuth code parameter missing", "remote_addr", r.RemoteAddr)
		http.Error(w, "Code not found", http.StatusBadRequest)
		return
	}

	token, err := oauthConfig.Exchange(context.Background(), code)
	if err != nil {
		slog.Error("Failed to exchange OAuth token", "error", err)
		http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
		return
	}

	client := github.NewClient(oauthConfig.Client(context.Background(), token))

	user, _, err := client.Users.Get(context.Background(), "")
	if err != nil {
		slog.Error("Failed to get authenticated GitHub user", "error", err)
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		return
	}

	email := ""
	if user.Email != nil {
		email = *user.Email
	} else {
		email = user.GetLogin() + "@users.noreply.github.com"
	}

	slog.Info("User logged in via GitHub OAuth", "user", user.GetLogin(), "email", email)

	// Register the account so Hall of Fame can mark it as a Sourcerer member.
	if _, err := db.Exec(`
		INSERT INTO users (email, primary_email, verified) VALUES ($1, TRUE, TRUE)
		ON CONFLICT (email) DO UPDATE SET primary_email = TRUE, verified = TRUE`, email); err != nil {
		slog.Error("Failed to record authenticated user", "email", email, "error", err)
	}

	// Set a signed session cookie. Secure is relaxed only in development,
	// where the app is served over plain HTTP and the browser would otherwise
	// discard the cookie, making local login impossible.
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    signCookie(email),
		Path:     "/",
		HttpOnly: true,
		Secure:   !isDevMode(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	// Fetch repos in background to avoid blocking request
	// Fetch ALL repos across all pages in background to avoid blocking request
	go func(email string, token *oauth2.Token) {
		client := github.NewClient(oauthConfig.Client(context.Background(), token))
		var allRepos []*github.Repository
		opt := &github.RepositoryListByAuthenticatedUserOptions{
			Type:        "owner",
			ListOptions: github.ListOptions{PerPage: 100, Page: 1},
		}

		for {
			repos, resp, err := client.Repositories.ListByAuthenticatedUser(context.Background(), opt)
			if err != nil {
				slog.Error("Failed to fetch repositories for user", "email", email, "page", opt.Page, "error", err)
				break
			}
			allRepos = append(allRepos, repos...)
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
			// Gentle 50ms pause between pages to stay safely below GitHub secondary rate limits
			time.Sleep(50 * time.Millisecond)
		}

		slog.Info("Discovered repositories across all pages for user", "total_count", len(allRepos), "email", email)
		skippedCount := 0
		enqueuedCount := 0

		for _, repo := range allRepos {
			// The OAuth scope is public_repo and the worker clones anonymously,
			// so private repositories can never be fetched. Skip rather than
			// queueing jobs that are guaranteed to fail.
			if repo.GetPrivate() {
				slog.Debug("Skipping private repository: anonymous clone not possible", "repo", repo.GetFullName())
				skippedCount++
				continue
			}

			cloneURL := repo.GetCloneURL()
			repoName := repo.GetFullName()
			pushedAt := ""
			if repo.PushedAt != nil {
				pushedAt = repo.PushedAt.String()
			}

			// Fast Tier-1 verification: check if repo was already synced with identical pushed_at timestamp
			if db != nil && pushedAt != "" {
				var existingPushedAt string
				var lastCommit string
				err := db.QueryRow(`
					SELECT COALESCE(github_pushed_at, ''), COALESCE(last_commit_rehash, '') 
					FROM repos 
					WHERE repo_url = $1 OR repo_name = $2`, cloneURL, repoName).Scan(&existingPushedAt, &lastCommit)
				if err == nil && existingPushedAt != "" && existingPushedAt == pushedAt && lastCommit != "" {
					slog.Debug("Fast-skipping repository: GitHub pushed_at timestamp unchanged", "repo", repoName, "pushed_at", pushedAt)
					skippedCount++
					continue
				}
			}

			EnqueueJob(IngestionJob{
				RepoURL:        cloneURL,
				RepoName:       repoName,
				UserEmail:      email,
				GitHubPushedAt: pushedAt,
			})
			enqueuedCount++
		}

		if enqueuedCount > 0 {
			globalSyncTracker.StartBatch(email, enqueuedCount)
		}

		slog.Info("Repository sync planning complete", "total_repos", len(allRepos), "skipped_unchanged", skippedCount, "enqueued_for_sync", enqueuedCount, "email", email)
	}(email, token)

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !isDevMode(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	slog.Info("User logged out", "remote_addr", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

type LangStat struct {
	Tech       string
	Lines      int
	Percentage int
}

// withPercentages fills in each language's share of the total, rounding any
// non-zero contribution up to 1% so small languages stay visible.
func withPercentages(stats []LangStat) []LangStat {
	totalLines := 0
	for _, s := range stats {
		totalLines += s.Lines
	}
	if totalLines <= 0 {
		return stats
	}
	for i := range stats {
		stats[i].Percentage = int((float64(stats[i].Lines) / float64(totalLines)) * 100.0)
		if stats[i].Percentage == 0 && stats[i].Lines > 0 {
			stats[i].Percentage = 1
		}
	}
	return stats
}

func getLanguagesForEmail(email string) []LangStat {
	rows, err := db.Query(`
		SELECT s.tech, SUM(s.num_lines_added) as lines
		FROM commit_stats s
		JOIN commits c ON c.rehash = s.commit_rehash
		WHERE c.author_email = $1
		GROUP BY s.tech
		ORDER BY lines DESC
		LIMIT 10`, email)
	if err != nil {
		slog.Error("Error querying languages", "email", email, "error", err)
		return nil
	}
	defer rows.Close()

	var stats []LangStat
	for rows.Next() {
		var s LangStat
		if err := rows.Scan(&s.Tech, &s.Lines); err == nil && s.Tech != "" {
			stats = append(stats, s)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating languages", "email", email, "error", err)
	}
	return withPercentages(stats)
}

// getReposForEmail lists the repositories the given author has commits in,
// ordered by contribution volume. Shared by the dashboard partial and the
// public profile so both stay consistent.
func getReposForEmail(email string) []RepoInfo {
	rows, err := db.Query(`
		SELECT r.rehash, COALESCE(r.repo_name, '') as repo_name,
		       COUNT(c.rehash) as commit_count,
		       COALESCE(SUM(c.num_lines_added), 0) as lines_added,
		       COALESCE(SUM(c.num_lines_deleted), 0) as lines_deleted
		FROM repos r
		JOIN commits c ON c.repo_rehash = r.rehash
		WHERE c.author_email = $1
		GROUP BY r.rehash, r.repo_name
		ORDER BY commit_count DESC`, email)
	if err != nil {
		slog.Error("Error querying repos", "email", email, "error", err)
		return nil
	}
	defer rows.Close()

	var repos []RepoInfo
	for rows.Next() {
		var repo RepoInfo
		if err := rows.Scan(&repo.Rehash, &repo.Name, &repo.CommitCount, &repo.LinesAdded, &repo.LinesDeleted); err == nil {
			repos = append(repos, repo)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating repos", "email", email, "error", err)
	}
	return repos
}

type FactsData struct {
	HasData       bool
	WorkStyle     string
	WorkStyleIcon string
	WorkStyleDesc string
	IndentHabit   string
	IndentIcon    string
	IndentDesc    string
	NamingStyle   string
	NamingIcon    string
	NamingDesc    string
	ScheduleStyle string
	ScheduleIcon  string
	ScheduleDesc  string
	AvgCommitSize string
	AvgLineLength string
}

func computeFacts(email string) FactsData {
	facts := FactsData{}

	var rows *sql.Rows
	var err error
	if email != "" {
		rows, err = db.Query("SELECT code, key, value1 FROM facts WHERE email = $1", email)
	} else {
		rows, err = db.Query("SELECT code, key, value1 FROM facts")
	}

	nightCommits := 0
	morningCommits := 0
	afternoonCommits := 0
	weekdayCommits := 0
	weekendCommits := 0
	tabsCount := 0
	spacesCount := 0
	snakeCount := 0
	camelCount := 0
	var avgLinesPerCommit float64
	var avgLineLen float64
	factCount := 0

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var code, key int
			var valStr string
			if err := rows.Scan(&code, &key, &valStr); err != nil {
				continue
			}
			factCount++
			val, _ := strconv.Atoi(valStr)
			valF, _ := strconv.ParseFloat(valStr, 64)

			switch code {
			case 1: // Day of week (0=Mon .. 6=Sun)
				if key >= 0 && key <= 4 {
					weekdayCommits += val
				} else {
					weekendCommits += val
				}
			case 2: // Hour of day (0 .. 23)
				if key >= 5 && key < 12 {
					morningCommits += val
				} else if key >= 12 && key < 18 {
					afternoonCommits += val
				} else {
					nightCommits += val
				}
			case 13: // Variable naming (0=snake, 1=camel)
				if key == 0 {
					snakeCount += val
				} else if key == 1 {
					camelCount += val
				}
			case 14: // Indentation (0=tabs, 1=spaces)
				if key == 0 {
					tabsCount += val
				} else if key == 1 {
					spacesCount += val
				}
			case 8: // Avg commit size
				if valF > 0 {
					avgLinesPerCommit = valF
				}
			case 10: // Avg line length
				if valF > 0 {
					avgLineLen = valF
				}
			}
		}
	}

	// If facts table has no rows or is sparse, dynamically calculate from commits table
	if factCount == 0 || (nightCommits == 0 && morningCommits == 0 && afternoonCommits == 0) {
		var commitRows *sql.Rows
		if email != "" {
			commitRows, _ = db.Query("SELECT date, num_lines_added, num_lines_deleted FROM commits WHERE author_email = $1", email)
		} else {
			commitRows, _ = db.Query("SELECT date, num_lines_added, num_lines_deleted FROM commits")
		}

		if commitRows != nil {
			defer commitRows.Close()
			totalLines := 0
			cCount := 0
			for commitRows.Next() {
				var dateEpoch int64
				var added, deleted int
				if err := commitRows.Scan(&dateEpoch, &added, &deleted); err == nil {
					cCount++
					totalLines += added + deleted
					t := time.Unix(dateEpoch, 0)
					h := t.Hour()
					wd := t.Weekday()

					if h >= 5 && h < 12 {
						morningCommits++
					} else if h >= 12 && h < 18 {
						afternoonCommits++
					} else {
						nightCommits++
					}

					if wd == time.Saturday || wd == time.Sunday {
						weekendCommits++
					} else {
						weekdayCommits++
					}
				}
			}
			if cCount > 0 {
				avgLinesPerCommit = float64(totalLines) / float64(cCount)
			}
		}
	}

	totalTimeCommits := nightCommits + morningCommits + afternoonCommits
	if totalTimeCommits > 0 {
		facts.HasData = true
		if nightCommits >= morningCommits && nightCommits >= afternoonCommits {
			facts.WorkStyle = "Night Owl"
			facts.WorkStyleIcon = "🌙"
			facts.WorkStyleDesc = fmt.Sprintf("%d%% commits late at night", int(float64(nightCommits)/float64(totalTimeCommits)*100))
		} else if morningCommits >= afternoonCommits {
			facts.WorkStyle = "Early Bird"
			facts.WorkStyleIcon = "🌅"
			facts.WorkStyleDesc = fmt.Sprintf("%d%% commits in the morning", int(float64(morningCommits)/float64(totalTimeCommits)*100))
		} else {
			facts.WorkStyle = "Daytime Builder"
			facts.WorkStyleIcon = "⚡"
			facts.WorkStyleDesc = fmt.Sprintf("%d%% commits during daytime", int(float64(afternoonCommits)/float64(totalTimeCommits)*100))
		}
	}

	totalDays := weekdayCommits + weekendCommits
	if totalDays > 0 {
		facts.HasData = true
		if float64(weekendCommits)/float64(totalDays) >= 0.25 {
			facts.ScheduleStyle = "Weekend Warrior"
			facts.ScheduleIcon = "⚔️"
			facts.ScheduleDesc = fmt.Sprintf("%d%% commits on weekends", int(float64(weekendCommits)/float64(totalDays)*100))
		} else {
			facts.ScheduleStyle = "Weekday Master"
			facts.ScheduleIcon = "💼"
			facts.ScheduleDesc = fmt.Sprintf("%d%% commits Mon-Fri", int(float64(weekdayCommits)/float64(totalDays)*100))
		}
	}

	totalIndent := tabsCount + spacesCount
	if totalIndent > 0 {
		facts.HasData = true
		if spacesCount >= tabsCount {
			facts.IndentHabit = "Spaces Enthusiast"
			facts.IndentIcon = "␣"
			facts.IndentDesc = fmt.Sprintf("%d%% indentation with spaces", int(float64(spacesCount)/float64(totalIndent)*100))
		} else {
			facts.IndentHabit = "Tabs Purist"
			facts.IndentIcon = "📑"
			facts.IndentDesc = fmt.Sprintf("%d%% indentation with tabs", int(float64(tabsCount)/float64(totalIndent)*100))
		}
	}

	totalNaming := snakeCount + camelCount
	if totalNaming > 0 {
		facts.HasData = true
		if snakeCount >= camelCount {
			facts.NamingStyle = "snake_case Fan"
			facts.NamingIcon = "🐍"
			facts.NamingDesc = fmt.Sprintf("%d%% snake_case identifiers", int(float64(snakeCount)/float64(totalNaming)*100))
		} else {
			facts.NamingStyle = "camelCase Stylist"
			facts.NamingIcon = "🐫"
			facts.NamingDesc = fmt.Sprintf("%d%% camelCase identifiers", int(float64(camelCount)/float64(totalNaming)*100))
		}
	}

	if avgLinesPerCommit > 0 {
		facts.HasData = true
		facts.AvgCommitSize = fmt.Sprintf("%.0f", avgLinesPerCommit)
	}
	if avgLineLen > 0 {
		facts.HasData = true
		facts.AvgLineLength = fmt.Sprintf("%.0f", avgLineLen)
	}

	return facts
}

func handleDashboardFacts(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	facts := computeFacts(email)
	renderTemplate(w, "facts.html", facts)
}

type RepoInfo struct {
	Rehash string
	// Name is the owner/repository name recorded at ingestion. It is empty for
	// repositories read before the worker started storing it, which is why
	// every surface falls back to the rehash.
	Name         string
	CommitCount  int
	LinesAdded   int
	LinesDeleted int
}

type ProfileData struct {
	// ProfileID is the opaque public identifier; every shareable URL is built
	// from it so email addresses never appear in links.
	ProfileID string
	Name      string
	// Email is the masked display form. The raw address is never serialized
	// into the cached profile JSON that anonymous visitors receive.
	Email         string
	TotalCommits  int
	LinesAdded    int
	LinesDeleted  int
	Languages     []LangStat
	Facts         FactsData
	Libraries     []TechnologyMeta
	Repos         []RepoInfo
	PublicBaseURL string
	// SEO is request-derived page metadata, deliberately excluded from the
	// cached profile JSON.
	SEO SEOMeta `json:"-"`
}

// buildProfileSEO writes the page metadata for a public profile. The
// description leads with the numbers a searcher would recognise.
func buildProfileSEO(r *http.Request, data *ProfileData, baseURL string) {
	description := fmt.Sprintf("%s has %s and %s across %s on Sourcerer. See the language split, coding habits and libraries behind the work.",
		data.Name,
		plural(data.TotalCommits, "commit"),
		plural(data.LinesAdded, "line")+" written",
		plural(len(data.Repos), "repository"),
	)

	seo := newSEO(baseURL, "/p/"+data.ProfileID,
		data.Name+" — engineering profile | "+siteName,
		truncateDescription(description, 200))
	// og:image stays the raster share card: the profile badge is an SVG, which
	// social crawlers will not render.
	seo.Type = "profile"
	seo.ImageAlt = data.Name + "'s engineering profile on Sourcerer"
	seo.JSONLD = newJSONLD(map[string]any{
		"@context": "https://schema.org",
		"@type":    "ProfilePage",
		"url":      seo.Canonical,
		"name":     seo.Title,
		"mainEntity": map[string]any{
			"@type": "Person",
			"name":  data.Name,
			"url":   seo.Canonical,
		},
	})
	seo.Nav = navFor(r, "profile")
	data.SEO = seo
}

func handlePublicProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "email")
	if profileID == "" {
		profileID = chi.URLParam(r, "username")
	}

	var email string
	var cachedJSON sql.NullString
	err := db.QueryRow("SELECT email, profile_data_json FROM public_profiles WHERE profile_id = $1", profileID).Scan(&email, &cachedJSON)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Error("Failed to look up public profile", "profile_id", profileID, "error", err)
		}
		http.Error(w, "Profile not found", http.StatusNotFound)
		return
	}

	sessionEmail := getSessionEmail(r)
	if sessionEmail != email {
		if !cachedJSON.Valid || cachedJSON.String == "" {
			http.Error(w, "Profile not published yet", http.StatusNotFound)
			return
		}
		var data ProfileData
		if err := json.Unmarshal([]byte(cachedJSON.String), &data); err != nil {
			slog.Error("Failed to unmarshal profile data", "error", err)
			http.Error(w, "Error parsing profile", http.StatusInternalServerError)
			return
		}
		data.ProfileID = profileID
		data.PublicBaseURL = getPublicBaseURL(r)
		buildProfileSEO(r, &data, data.PublicBaseURL)
		renderTemplate(w, "profile.html", data)
		return
	}

	var data ProfileData
	data.ProfileID = profileID
	data.Email = maskEmail(email)
	data.Name = email

	// Look up author name if available
	if err := db.QueryRow("SELECT name FROM authors WHERE email = $1 LIMIT 1", email).Scan(&data.Name); err != nil && err != sql.ErrNoRows {
		slog.Warn("Failed to look up author name", "error", err)
	}
	if data.Name == "" {
		data.Name = maskEmail(email)
	}

	// Total commits, lines
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", email).Scan(&data.TotalCommits, &data.LinesAdded, &data.LinesDeleted); err != nil && err != sql.ErrNoRows {
		slog.Error("Failed to aggregate profile commit stats", "error", err)
	}

	data.Languages = getLanguagesForEmail(email)
	data.Facts = computeFacts(email)
	data.Libraries = getContributorLibraries(email)
	data.Repos = getReposForEmail(email)

	// Cache the published view for anonymous visitors. PublicBaseURL is
	// request-derived and deliberately excluded from the cached copy.
	b, err := json.Marshal(data)
	if err != nil {
		slog.Error("Failed to marshal profile data", "profile_id", profileID, "error", err)
	} else {
		svgBytes := generateBadgeSVG(data.Name, data.TotalCommits, data.LinesAdded, data.LinesDeleted, data.Languages)
		if _, err := db.Exec("UPDATE public_profiles SET profile_data_json = $1, badge_svg = $2 WHERE profile_id = $3", string(b), string(svgBytes), profileID); err != nil {
			slog.Error("Failed to publish profile cache", "profile_id", profileID, "error", err)
		}
	}

	data.PublicBaseURL = getPublicBaseURL(r)
	buildProfileSEO(r, &data, data.PublicBaseURL)
	renderTemplate(w, "profile.html", data)
}

func generateBadgeSVG(name string, totalCommits int, linesAdded int, linesDeleted int, languages []LangStat) []byte {
	if name == "" {
		name = "Sourcerer Engineer"
	}

	// Mirrors seriesPalette / --series-N, lightened where needed for the dark
	// badge background.
	palette := []string{"#a78bfa", "#6b8afd", "#22b8cf", "#34d399", "#fbbf24"}

	var langBars strings.Builder
	var langLegend strings.Builder

	currentX := 25.0
	barWidthTotal := 445.0

	if len(languages) == 0 {
		langLegend.WriteString(`<text x="25" y="168" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-style="italic">No languages detected yet</text>`)
	} else {
		for i, l := range languages {
			if i >= 5 {
				break
			}
			color := palette[i%len(palette)]
			w := (float64(l.Percentage) / 100.0) * barWidthTotal
			if w < 3 && l.Percentage > 0 {
				w = 3
			}
			langBars.WriteString(fmt.Sprintf(`<rect x="%.1f" y="138" width="%.1f" height="10" rx="3" fill="%s" />`, currentX, w, color))
			currentX += w + 1

			if i < 4 {
				legendX := 25 + (i * 115)
				langLegend.WriteString(fmt.Sprintf(`
					<circle cx="%d" cy="168" r="4.5" fill="%s"/>
					<text x="%d" y="172" fill="#94a3b8" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="12" font-weight="500">%s <tspan fill="#64748b">%d%%</tspan></text>`,
					legendX, color, legendX+12, template.HTMLEscapeString(l.Tech), l.Percentage))
			}
		}
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="495" height="195" viewBox="0 0 495 195" fill="none">
	<style>
		.header { font: 700 16px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #f8fafc; }
		.sub { font: 500 12px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #94a3b8; }
		.stat-val { font: 700 18px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #f8fafc; }
		.stat-label { font: 600 10px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #64748b; text-transform: uppercase; letter-spacing: 0.5px; }
		.added { fill: #10b981; }
		.deleted { fill: #f43f5e; }
		.brand { font: 800 12px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #a78bfa; }
	</style>
	<rect width="495" height="195" rx="12" fill="#090514" stroke="#241b4a" stroke-width="1.5"/>
	
	<!-- Header -->
	<g transform="translate(25, 25)">
		<text class="header" y="12">%s</text>
		<text class="sub" y="28">Sourcerer Engineering Profile</text>
		<text class="brand" x="445" y="12" text-anchor="end">&#9889; SOURCERER</text>
	</g>
	
	<!-- Metrics -->
	<g transform="translate(25, 75)">
		<!-- Commits -->
		<text class="stat-val" x="0" y="18">%d</text>
		<text class="stat-label" x="0" y="32">Commits</text>
		
		<!-- Added -->
		<text class="stat-val added" x="150" y="18">+%d</text>
		<text class="stat-label" x="150" y="32">Lines Added</text>
		
		<!-- Deleted -->
		<text class="stat-val deleted" x="300" y="18">-%d</text>
		<text class="stat-label" x="300" y="32">Lines Deleted</text>
	</g>
	
	<!-- Language Bar Background -->
	<rect x="25" y="138" width="445" height="10" rx="5" fill="#1a1336"/>
	<!-- Language Bar Segments -->
	%s
	<!-- Language Legend -->
	%s
</svg>`,
		template.HTMLEscapeString(name),
		totalCommits, linesAdded, linesDeleted,
		langBars.String(),
		langLegend.String(),
	)

	return []byte(svg)
}

// svgDownloadName turns an identifier that may contain slashes or other path
// characters into something safe to put in a Content-Disposition filename.
func svgDownloadName(identifier string) string {
	var b strings.Builder
	for _, r := range identifier {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "badge"
	}
	return name + ".svg"
}

// writeSVG serves a badge. With ?download=1 it is sent as an attachment so the
// browser saves the file instead of rendering it as a page.
func writeSVG(w http.ResponseWriter, r *http.Request, identifier string, svg []byte) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+svgDownloadName(identifier)+`"`)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(svg); err != nil {
		slog.Error("Failed to write SVG badge", "error", err)
	}
}

func handleBadgeSVG(w http.ResponseWriter, r *http.Request) {
	identifier := chi.URLParam(r, "identifier")
	identifier = strings.TrimSuffix(identifier, ".svg")

	var email, profileID, name string
	var totalCommits, linesAdded, linesDeleted int
	var cachedSVG sql.NullString

	// 1. Look up by profile_id OR email
	err := db.QueryRow(`
		SELECT p.profile_id, p.email, COALESCE(a.name, p.email), p.badge_svg
		FROM public_profiles p
		LEFT JOIN authors a ON a.email = p.email
		WHERE p.profile_id = $1 OR p.email = $1
		LIMIT 1`, identifier).Scan(&profileID, &email, &name, &cachedSVG)

	if err == nil && cachedSVG.Valid && cachedSVG.String != "" {
		writeSVG(w, r, "sourcerer-profile-"+identifier, []byte(cachedSVG.String))
		return
	}

	// 2. If not found in public_profiles, check if identifier is author name or commit email
	if err != nil {
		err = db.QueryRow(`
			SELECT email, COALESCE(name, email)
			FROM authors
			WHERE email = $1 OR name = $1
			LIMIT 1`, identifier).Scan(&email, &name)
	}
	if err != nil {
		err = db.QueryRow(`
			SELECT author_email, COALESCE(author_name, author_email)
			FROM commits
			WHERE author_email = $1 OR author_name = $1
			LIMIT 1`, identifier).Scan(&email, &name)
	}

	if err != nil || email == "" {
		http.Error(w, "Badge not found", http.StatusNotFound)
		return
	}

	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", email).Scan(&totalCommits, &linesAdded, &linesDeleted)
	langs := getLanguagesForEmail(email)
	if name == "" {
		name = maskEmail(email)
	}
	svgBytes := generateBadgeSVG(name, totalCommits, linesAdded, linesDeleted, langs)

	if profileID != "" {
		_, _ = db.Exec("UPDATE public_profiles SET badge_svg = $1 WHERE profile_id = $2", string(svgBytes), profileID)
	}

	writeSVG(w, r, "sourcerer-profile-"+identifier, svgBytes)
}

// Hall of Fame Data Structures & Handlers (Feature: hall-of-fame)

// ContributorStat is rendered on fully public surfaces (Hall of Fame HTML, SVG
// and JSON). Email is always the masked display form — raw addresses are never
// exposed anonymously. ProfileID, when present, links to the contributor's own
// published profile.
type ContributorStat struct {
	Email        string `json:"email"`
	ProfileID    string `json:"profile_id,omitempty"`
	Name         string `json:"name"`
	Commits      int    `json:"commits"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesDeleted int    `json:"lines_deleted,omitempty"`
	IsSourcerer  bool   `json:"is_sourcerer"`
}

// ProfileURL links to the contributor's published profile, or to an inert
// anchor when they have never signed in and so have no public profile.
func (c ContributorStat) ProfileURL() string {
	if c.ProfileID == "" {
		return "#"
	}
	return "/p/" + c.ProfileID
}

// sanitize masks the address and falls back to the masked form when the commit
// carries no author name, so a raw email can never leak through Name either.
func (c *ContributorStat) sanitize() {
	masked := maskEmail(c.Email)
	if c.Name == "" || c.Name == c.Email || strings.Contains(c.Name, "@") {
		c.Name = masked
	}
	c.Email = masked
}

type HallOfFameData struct {
	RepoRehash           string            `json:"repo_rehash"`
	RepoName             string            `json:"repo_name"`
	TotalCommits         int               `json:"total_commits"`
	TotalLinesAdded      int               `json:"total_lines_added"`
	TotalLinesDeleted    int               `json:"total_lines_deleted"`
	TotalContributors    int               `json:"total_contributors"`
	TopContributors      []ContributorStat `json:"top"`
	TrendingContributors []ContributorStat `json:"trending"`
	NewContributors      []ContributorStat `json:"new"`
	Languages            []LangStat        `json:"languages"`
	PublicBaseURL        string            `json:"public_base_url"`
	// SEO is request-derived page metadata, never part of the API payload.
	SEO SEOMeta `json:"-"`
}

func getHallOfFameData(repoRehash string) HallOfFameData {
	data := HallOfFameData{
		RepoRehash: repoRehash,
		RepoName:   repoRehash,
	}

	if db == nil || repoRehash == "" {
		return data
	}

	// 1. Overall stats
	_ = db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0), COUNT(DISTINCT author_email)
		FROM commits
		WHERE repo_rehash = $1`, repoRehash).Scan(&data.TotalCommits, &data.TotalLinesAdded, &data.TotalLinesDeleted, &data.TotalContributors)

	// 2. All-time Top Contributors
	topRows, err := db.Query(`
		SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
		       COUNT(*) as commit_count, COALESCE(SUM(c.num_lines_added), 0) as added,
		       COALESCE(SUM(c.num_lines_deleted), 0) as deleted,
		       bool_or(u.email IS NOT NULL) as is_sourcerer,
		       COALESCE(MAX(p.profile_id), '') as profile_id
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		LEFT JOIN public_profiles p ON p.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		ORDER BY commit_count DESC
		LIMIT 6`, repoRehash)
	if err == nil {
		defer topRows.Close()
		for topRows.Next() {
			var cs ContributorStat
			if err := topRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer, &cs.ProfileID); err == nil {
				cs.sanitize()
				data.TopContributors = append(data.TopContributors, cs)
			}
		}
		if err := topRows.Err(); err != nil {
			slog.Error("Error iterating top contributors", "repo", repoRehash, "error", err)
		}
	}

	// 3. Determine recent active epoch window
	var maxDate int64
	_ = db.QueryRow("SELECT COALESCE(MAX(date), 0) FROM commits WHERE repo_rehash = $1", repoRehash).Scan(&maxDate)

	now := time.Now().Unix()
	cutoff := now - (7 * 86400) // Default 7 days
	// If repository has no commits in the last 7 days, scale relative to latest commit
	if maxDate > 0 && maxDate < cutoff {
		cutoff = maxDate - (30 * 86400) // 30 days prior to latest commit
	}

	// 4. Trending Contributors (velocity in recent active window)
	trendRows, err := db.Query(`
		SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
		       COUNT(*) as commit_count, COALESCE(SUM(c.num_lines_added), 0) as added,
		       COALESCE(SUM(c.num_lines_deleted), 0) as deleted,
		       bool_or(u.email IS NOT NULL) as is_sourcerer,
		       COALESCE(MAX(p.profile_id), '') as profile_id
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		LEFT JOIN public_profiles p ON p.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.date >= $2 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		ORDER BY commit_count DESC
		LIMIT 6`, repoRehash, cutoff)
	if err == nil {
		defer trendRows.Close()
		for trendRows.Next() {
			var cs ContributorStat
			if err := trendRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer, &cs.ProfileID); err == nil {
				cs.sanitize()
				data.TrendingContributors = append(data.TrendingContributors, cs)
			}
		}
		if err := trendRows.Err(); err != nil {
			slog.Error("Error iterating trending contributors", "repo", repoRehash, "error", err)
		}
	}

	// 5. New Contributors (initial commit in this repo within cutoff)
	newRows, err := db.Query(`
		SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
		       COUNT(*) as commit_count,
		       bool_or(u.email IS NOT NULL) as is_sourcerer,
		       COALESCE(MAX(p.profile_id), '') as profile_id
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		LEFT JOIN public_profiles p ON p.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		HAVING MIN(c.date) >= $2
		ORDER BY MIN(c.date) DESC
		LIMIT 6`, repoRehash, cutoff)
	if err == nil {
		defer newRows.Close()
		for newRows.Next() {
			var cs ContributorStat
			if err := newRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.IsSourcerer, &cs.ProfileID); err == nil {
				cs.sanitize()
				data.NewContributors = append(data.NewContributors, cs)
			}
		}
		if err := newRows.Err(); err != nil {
			slog.Error("Error iterating new contributors", "repo", repoRehash, "error", err)
		}
	}
	// Fallback if no new contributors within cutoff: fetch latest first-time joiners
	if len(data.NewContributors) == 0 {
		fallbackNewRows, err := db.Query(`
			SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
			       COUNT(*) as commit_count,
			       bool_or(u.email IS NOT NULL) as is_sourcerer,
			       COALESCE(MAX(p.profile_id), '') as profile_id
			FROM commits c
			LEFT JOIN authors a ON a.email = c.author_email
			LEFT JOIN users u ON u.email = c.author_email
			LEFT JOIN public_profiles p ON p.email = c.author_email
			WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
			GROUP BY c.author_email
			ORDER BY MIN(c.date) DESC
			LIMIT 6`, repoRehash)
		if err == nil {
			defer fallbackNewRows.Close()
			for fallbackNewRows.Next() {
				var cs ContributorStat
				if err := fallbackNewRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.IsSourcerer, &cs.ProfileID); err == nil {
					cs.sanitize()
					data.NewContributors = append(data.NewContributors, cs)
				}
			}
			if err := fallbackNewRows.Err(); err != nil {
				slog.Error("Error iterating fallback new contributors", "repo", repoRehash, "error", err)
			}
		}
	}

	// 6. Repo Language breakdown
	langRows, err := db.Query(`
		SELECT s.tech, SUM(s.num_lines_added) as lines
		FROM commit_stats s
		JOIN commits c ON c.rehash = s.commit_rehash
		WHERE c.repo_rehash = $1
		GROUP BY s.tech
		ORDER BY lines DESC
		LIMIT 5`, repoRehash)
	if err == nil {
		defer langRows.Close()
		for langRows.Next() {
			var s LangStat
			if err := langRows.Scan(&s.Tech, &s.Lines); err == nil && s.Tech != "" {
				data.Languages = append(data.Languages, s)
			}
		}
		if err := langRows.Err(); err != nil {
			slog.Error("Error iterating repo languages", "repo", repoRehash, "error", err)
		}
		data.Languages = withPercentages(data.Languages)
	}

	return data
}

// getPublicBaseURL returns an absolute origin ("https://example.com") suitable
// for embedding in README snippets and SVG footers. PUBLIC_BASE_URL wins; the
// proxy-forwarded scheme and host are the fallback.
func getPublicBaseURL(r *http.Request) string {
	if envURL := os.Getenv("PUBLIC_BASE_URL"); envURL != "" {
		return strings.TrimSuffix(envURL, "/")
	}
	if r != nil && r.Host != "" {
		scheme := "https"
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else if r.TLS == nil {
			scheme = "http"
		}
		return scheme + "://" + r.Host
	}
	return "http://localhost:8080"
}

// displayHost strips the scheme from a base URL for compact SVG footer text.
func displayHost(baseURL string) string {
	if i := strings.Index(baseURL, "://"); i >= 0 {
		return baseURL[i+3:]
	}
	return baseURL
}

func generateHallOfFameSVG(data HallOfFameData) []byte {
	// Mirrors seriesPalette / --series-N, lightened where needed for the dark
	// badge background.
	palette := []string{"#a78bfa", "#6b8afd", "#22b8cf", "#34d399", "#fbbf24"}

	var langBars strings.Builder
	var langLegend strings.Builder

	currentX := 25.0
	barWidthTotal := 790.0

	if len(data.Languages) == 0 {
		langLegend.WriteString(`<text x="25" y="329" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-style="italic">No languages detected yet</text>`)
	} else {
		for i, l := range data.Languages {
			if i >= 5 {
				break
			}
			color := palette[i%len(palette)]
			w := (float64(l.Percentage) / 100.0) * barWidthTotal
			if w < 4 && l.Percentage > 0 {
				w = 4
			}
			langBars.WriteString(fmt.Sprintf(`<rect x="%.1f" y="295" width="%.1f" height="8" rx="3" fill="%s" />`, currentX, w, color))
			currentX += w + 1

			if i < 4 {
				legendX := 25 + (i * 140)
				langLegend.WriteString(fmt.Sprintf(`
					<circle cx="%d" cy="325" r="4" fill="%s"/>
					<text x="%d" y="329" fill="#94a3b8" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-weight="500">%s <tspan fill="#64748b">%d%%</tspan></text>`,
					legendX, color, legendX+10, template.HTMLEscapeString(l.Tech), l.Percentage))
			}
		}
	}

	renderTierList := func(list []ContributorStat, startX int, accentColor string, badgeSuffix string) string {
		var b strings.Builder
		if len(list) == 0 {
			b.WriteString(fmt.Sprintf(`<text x="%d" y="145" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="12" font-style="italic">No contributors yet</text>`, startX))
			return b.String()
		}

		for i, c := range list {
			if i >= 3 {
				break
			}
			rowY := 115 + (i * 48)
			initial := "?"
			if len(c.Name) > 0 {
				initial = strings.ToUpper(string([]rune(c.Name)[0]))
			}

			displayName := c.Name
			runes := []rune(displayName)
			if len(runes) > 16 {
				displayName = string(runes[:14]) + "…"
			}

			haloAttr := ""
			if c.IsSourcerer {
				haloAttr = fmt.Sprintf(`<circle cx="%d" cy="%d" r="16" fill="none" stroke="#a78bfa" stroke-width="1.5" stroke-dasharray="2 2" />`, startX+16, rowY+12)
			}

			b.WriteString(fmt.Sprintf(`
				<g transform="translate(%d, %d)">
					<!-- Row Card -->
					<rect width="250" height="42" rx="8" fill="#120d24" stroke="#1a1336" stroke-width="1"/>
					<!-- Avatar -->
					%s
					<circle cx="20" cy="21" r="13" fill="#1c1538" stroke="%s" stroke-width="1"/>
					<text x="20" y="25" fill="#f8fafc" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-weight="700" text-anchor="middle">%s</text>
					<!-- Name -->
					<text x="42" y="24" fill="#f8fafc" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="12" font-weight="600">%s</text>
					<!-- Stat Badge -->
					<rect x="180" y="11" width="60" height="20" rx="4" fill="%s" fill-opacity="0.12" stroke="%s" stroke-opacity="0.3" stroke-width="1"/>
					<text x="210" y="24" fill="%s" font-family="ui-monospace,Menlo,monospace" font-size="10" font-weight="700" text-anchor="middle">%d %s</text>
				</g>`,
				startX, rowY,
				haloAttr, accentColor, template.HTMLEscapeString(initial),
				template.HTMLEscapeString(displayName),
				accentColor, accentColor, accentColor, c.Commits, badgeSuffix,
			))
		}
		return b.String()
	}

	topListSVG := renderTierList(data.TopContributors, 25, "#f59e0b", "top")
	trendingListSVG := renderTierList(data.TrendingContributors, 295, "#f43f5e", "rec")
	newListSVG := renderTierList(data.NewContributors, 565, "#10b981", "new")

	repoDisplay := data.RepoRehash
	repoRunes := []rune(repoDisplay)
	if len(repoRunes) > 28 {
		repoDisplay = string(repoRunes[:26]) + "…"
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="840" height="350" viewBox="0 0 840 350" fill="none">
	<style>
		.title { font: 800 13px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #a855f7; letter-spacing: 0.5px; }
		.repo-tag { font: 700 11px ui-monospace, Menlo, monospace; fill: #cbd5e1; }
		.col-header { font: 700 12px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; text-transform: uppercase; letter-spacing: 0.5px; }
		.stat-chip { font: 600 11px ui-monospace, Menlo, monospace; fill: #94a3b8; }
	</style>
	
	<!-- Background Frame -->
	<rect width="840" height="350" rx="14" fill="#090514" stroke="#241b4a" stroke-width="1.5"/>
	
	<!-- Header Bar -->
	<g transform="translate(25, 22)">
		<text class="title" y="14">&#9889; SOURCERER HALL OF FAME</text>
		
		<!-- Repo Tag -->
		<rect x="235" y="0" width="220" height="22" rx="6" fill="#1a1336" stroke="#241b4a" stroke-width="1"/>
		<text class="repo-tag" x="245" y="15">%s</text>
		
		<!-- Metric Badges -->
		<text class="stat-chip" x="650" y="15">%d Commits &bull; %d Contributors</text>
	</g>
	
	<!-- Divider -->
	<line x1="25" y1="58" x2="815" y2="58" stroke="#1a1336" stroke-width="1"/>

	<!-- Column Headers -->
	<!-- Top -->
	<text class="col-header" x="25" y="90" fill="#f59e0b">&#127775; TOP CONTRIBUTORS</text>
	<!-- Trending -->
	<text class="col-header" x="295" y="90" fill="#f43f5e">&#128293; TRENDING VELOCITY</text>
	<!-- New -->
	<text class="col-header" x="565" y="90" fill="#10b981">&#127793; NEW CONTRIBUTORS</text>

	<!-- Tier Content Rows -->
	%s
	%s
	%s

	<!-- Language Bar Background -->
	<rect x="25" y="295" width="790" height="8" rx="4" fill="#1a1336"/>
	<!-- Language Bar Segments -->
	%s
	<!-- Language Legend -->
	%s
	
	<!-- Footer Branding -->
	<text x="815" y="329" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10" font-weight="600" text-anchor="end">%s/r/%s</text>
</svg>`,
		template.HTMLEscapeString(repoDisplay),
		data.TotalCommits, data.TotalContributors,
		topListSVG, trendingListSVG, newListSVG,
		langBars.String(),
		langLegend.String(),
		template.HTMLEscapeString(displayHost(data.PublicBaseURL)),
		template.HTMLEscapeString(repoDisplay),
	)

	return []byte(svg)
}

func handleHallOfFameSVG(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)
	data.PublicBaseURL = getPublicBaseURL(r)

	writeSVG(w, r, "sourcerer-hall-of-fame-"+repoRehash, generateHallOfFameSVG(data))
}

func handleHallOfFameHTML(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)
	data.PublicBaseURL = getPublicBaseURL(r)

	description := fmt.Sprintf("Who builds %s: %s from %s, with the language split and newest arrivals, read from git history.",
		data.RepoName,
		plural(data.TotalCommits, "commit"),
		plural(data.TotalContributors, "contributor"),
	)
	seo := newSEO(data.PublicBaseURL, "/r/"+data.RepoRehash,
		data.RepoName+" contributors — hall of fame | "+siteName,
		truncateDescription(description, 200))
	seo.ImageAlt = "Contributor hall of fame for " + data.RepoName
	seo.Nav = navFor(r, "repository")
	seo.JSONLD = newJSONLD(map[string]any{
		"@context": "https://schema.org",
		"@type":    "CollectionPage",
		"url":      seo.Canonical,
		"name":     seo.Title,
		"about": map[string]any{
			"@type": "SoftwareSourceCode",
			"name":  data.RepoName,
		},
	})
	data.SEO = seo

	renderTemplate(w, "repo.html", data)
}

func handleHallOfFameAPI(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)
	data.PublicBaseURL = getPublicBaseURL(r)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode Hall of Fame API response", "error", err)
	}
}

// Awesome Libraries Models & Handlers (Feature: awesome-libraries)

type TechnologyMeta struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Lang         string   `json:"lang"`
	Category     string   `json:"category"`
	Icon         string   `json:"icon"`
	Description  string   `json:"description"`
	ImportTokens []string `json:"import_tokens"`
	Lines        int      `json:"lines,omitempty"`
	RepoCount    int      `json:"repo_count,omitempty"`
}

type LibraryCategoryGroup struct {
	Category string           `json:"category"`
	Items    []TechnologyMeta `json:"items"`
}

func seedTechnologies(db *sql.DB) {
	if db == nil || len(technologiesJSON) == 0 {
		return
	}
	var techs []TechnologyMeta
	if err := json.Unmarshal(technologiesJSON, &techs); err != nil {
		slog.Error("Failed to unmarshal technologies.json", "error", err)
		return
	}

	stmt, err := db.Prepare(`
		INSERT INTO technologies (id, name, lang, category, icon, description, import_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			lang = EXCLUDED.lang,
			category = EXCLUDED.category,
			icon = EXCLUDED.icon,
			description = EXCLUDED.description,
			import_tokens = EXCLUDED.import_tokens`)
	if err != nil {
		slog.Error("Failed to prepare technology upsert statement", "error", err)
		return
	}
	defer stmt.Close()

	for _, t := range techs {
		_, err := stmt.Exec(t.ID, t.Name, t.Lang, t.Category, t.Icon, t.Description, pq.Array(t.ImportTokens))
		if err != nil {
			slog.Error("Failed to seed technology", "id", t.ID, "error", err)
		}
	}
	slog.Info("Successfully seeded technologies catalog", "count", len(techs))
}

func getLibrariesCatalog() []LibraryCategoryGroup {
	if db == nil {
		return nil
	}

	query := `
		SELECT t.id, t.name, t.lang, t.category, COALESCE(t.icon, ''), COALESCE(t.description, ''),
		       COALESCE(t.import_tokens, '{}'),
		       COALESCE(SUM(s.num_lines_added), 0) as total_lines,
		       COUNT(DISTINCT c.repo_rehash) as repo_count
		FROM technologies t
		LEFT JOIN commit_stats s ON (s.tech = t.id OR s.tech = t.name OR s.tech = REPLACE(t.id, t.lang || '.', ''))
		LEFT JOIN commits c ON c.rehash = s.commit_rehash
		GROUP BY t.id, t.name, t.lang, t.category, t.icon, t.description, t.import_tokens
		ORDER BY t.category ASC, total_lines DESC, t.name ASC`

	rows, err := db.Query(query)
	if err != nil {
		slog.Error("Failed to query libraries catalog", "error", err)
		return nil
	}
	defer rows.Close()

	groupMap := make(map[string][]TechnologyMeta)
	var categoryOrder []string

	for rows.Next() {
		var tm TechnologyMeta
		if err := rows.Scan(&tm.ID, &tm.Name, &tm.Lang, &tm.Category, &tm.Icon, &tm.Description, pq.Array(&tm.ImportTokens), &tm.Lines, &tm.RepoCount); err == nil {
			if _, exists := groupMap[tm.Category]; !exists {
				categoryOrder = append(categoryOrder, tm.Category)
			}
			groupMap[tm.Category] = append(groupMap[tm.Category], tm)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating libraries catalog", "error", err)
	}

	var result []LibraryCategoryGroup
	for _, cat := range categoryOrder {
		result = append(result, LibraryCategoryGroup{
			Category: cat,
			Items:    groupMap[cat],
		})
	}
	return result
}

func getLibraryDetail(techID string) (*TechnologyMeta, []ContributorStat) {
	var tm TechnologyMeta
	if db == nil {
		return &tm, nil
	}

	err := db.QueryRow(`
		SELECT id, name, lang, category, COALESCE(icon, ''), COALESCE(description, ''), COALESCE(import_tokens, '{}')
		FROM technologies
		WHERE id = $1 OR name ILIKE $1 LIMIT 1`, techID).Scan(&tm.ID, &tm.Name, &tm.Lang, &tm.Category, &tm.Icon, &tm.Description, pq.Array(&tm.ImportTokens))
	if err != nil {
		tm = TechnologyMeta{
			ID:       techID,
			Name:     techID,
			Lang:     "framework",
			Category: "General Library",
		}
	}

	// Leaderboard for this library
	var contributors []ContributorStat
	strippedID := strings.TrimPrefix(tm.ID, tm.Lang+".")
	cRows, err := db.Query(`
		SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
		       COUNT(DISTINCT c.rehash) as commit_count,
		       COALESCE(SUM(s.num_lines_added), 0) as added,
		       COALESCE(SUM(s.num_lines_deleted), 0) as deleted,
		       bool_or(u.email IS NOT NULL) as is_sourcerer,
		       COALESCE(MAX(p.profile_id), '') as profile_id
		FROM commit_stats s
		JOIN commits c ON c.rehash = s.commit_rehash
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		LEFT JOIN public_profiles p ON p.email = c.author_email
		WHERE s.tech = $1 OR s.tech = $2 OR s.tech = $3
		GROUP BY c.author_email
		ORDER BY added DESC
		LIMIT 10`, tm.ID, tm.Name, strippedID)
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var cs ContributorStat
			if err := cRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer, &cs.ProfileID); err == nil {
				cs.sanitize()
				contributors = append(contributors, cs)
			}
		}
		if err := cRows.Err(); err != nil {
			slog.Error("Error iterating library contributors", "tech", techID, "error", err)
		}
	}

	return &tm, contributors
}

func getContributorLibraries(email string) []TechnologyMeta {
	if db == nil {
		return nil
	}

	rows, err := db.Query(`
		SELECT COALESCE(t.id, s.tech) as tech_id,
		       COALESCE(t.name, s.tech) as tech_name,
		       COALESCE(t.lang, 'framework') as tech_lang,
		       COALESCE(t.category, 'Recognized Library') as tech_cat,
		       COALESCE(SUM(s.num_lines_added), 0) as lines
		FROM commit_stats s
		JOIN commits c ON c.rehash = s.commit_rehash
		LEFT JOIN technologies t ON (t.id = s.tech OR t.name ILIKE s.tech OR t.id LIKE '%.' || s.tech OR s.tech LIKE '%.' || t.id OR SPLIT_PART(t.id, '.', 2) = s.tech)
		WHERE c.author_email = $1 AND (s.type = 2 OR t.id IS NOT NULL)
		GROUP BY tech_id, tech_name, tech_lang, tech_cat
		ORDER BY lines DESC
		LIMIT 12`, email)

	if err != nil {
		slog.Error("Error querying contributor libraries", "email", email, "error", err)
		return nil
	}
	defer rows.Close()

	var result []TechnologyMeta
	for rows.Next() {
		var tm TechnologyMeta
		if err := rows.Scan(&tm.ID, &tm.Name, &tm.Lang, &tm.Category, &tm.Lines); err == nil {
			if tm.Lines > 0 {
				result = append(result, tm)
			}
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating contributor libraries", "email", email, "error", err)
	}
	return result
}

func handleLibrariesCatalogHTML(w http.ResponseWriter, r *http.Request) {
	groups := getLibrariesCatalog()

	total := 0
	for _, g := range groups {
		total += len(g.Items)
	}

	base := getPublicBaseURL(r)
	seo := newSEO(base, "/libraries",
		"Library catalog — every framework Sourcerer recognizes | "+siteName,
		truncateDescription(fmt.Sprintf(
			"The %s Sourcerer detects in source code, across %s. Open one to see who has written the most with it.",
			plural(total, "library"), plural(len(groups), "category")), 200))
	seo.JSONLD = newJSONLD(map[string]any{
		"@context":      "https://schema.org",
		"@type":         "CollectionPage",
		"url":           seo.Canonical,
		"name":          seo.Title,
		"numberOfItems": total,
		"isPartOf":      map[string]any{"@type": "WebSite", "name": siteName, "url": base},
		"inLanguage":    "en",
		"dateModified":  time.Now().UTC().Format("2006-01-02"),
		"mainContentOfPage": map[string]any{
			"@type": "WebPageElement",
			"name":  "Recognized libraries",
		},
	})

	seo.Nav = navFor(r, "libraries")

	data := struct {
		Groups []LibraryCategoryGroup
		SEO    SEOMeta
	}{Groups: groups, SEO: seo}

	renderTemplate(w, "libraries.html", data)
}

func handleLibraryDetailHTML(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	if strings.HasSuffix(techID, ".svg") {
		handleLibraryFameSVG(w, r)
		return
	}
	base := getPublicBaseURL(r)
	slots, tech, top, trending, newContribs := getLibraryFameEntries(techID, base)

	seo := newSEO(base, "/libraries/"+techID, siteName+" library", "")
	if tech.Name != "" {
		seo = newSEO(base, "/libraries/"+tech.ID,
			tech.Name+" contributors — who writes the most "+tech.Lang+" with it | "+siteName,
			truncateDescription(fmt.Sprintf("%s %s Ranked by lines written across every repository Sourcerer has indexed.",
				tech.Name+".", tech.Description), 200))
		seo.ImageAlt = "Top " + tech.Name + " contributors on Sourcerer"
		seo.JSONLD = newJSONLD(map[string]any{
			"@context": "https://schema.org",
			"@type":    "CollectionPage",
			"url":      seo.Canonical,
			"name":     seo.Title,
			"about": map[string]any{
				"@type":               "SoftwareSourceCode",
				"name":                tech.Name,
				"description":         tech.Description,
				"programmingLanguage": tech.Lang,
			},
		})
	}

	seo.Nav = navFor(r, "libraries")

	data := struct {
		Library              *TechnologyMeta
		Contributors         []ContributorStat
		TopContributors      []ContributorStat
		TrendingContributors []ContributorStat
		NewContributors      []ContributorStat
		FameSlots            []FameEntry
		PublicBaseURL        string
		SEO                  SEOMeta
	}{
		Library:              &tech,
		Contributors:         top,
		TopContributors:      top,
		TrendingContributors: trending,
		NewContributors:      newContribs,
		FameSlots:            slots,
		PublicBaseURL:        base,
		SEO:                  seo,
	}

	renderTemplate(w, "library_detail.html", data)
}

func handleLibrariesCatalogAPI(w http.ResponseWriter, r *http.Request) {
	groups := getLibrariesCatalog()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(groups); err != nil {
		slog.Error("Failed to encode libraries API response", "error", err)
	}
}

func handleLibraryDetailAPI(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	tech, contributors := getLibraryDetail(techID)

	data := struct {
		Library      *TechnologyMeta   `json:"library"`
		Contributors []ContributorStat `json:"contributors"`
	}{
		Library:      tech,
		Contributors: contributors,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode library detail API response", "error", err)
	}
}

func handleDashboardLibraries(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	libs := getContributorLibraries(email)
	renderTemplate(w, "libraries_partial.html", libs)
}

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	if d < 10*time.Second {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	}
	if d < 2*time.Minute {
		return "1 minute ago"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	}
	if d < 2*time.Hour {
		return "1 hour ago"
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	if d < 48*time.Hour {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func handleDashboardSyncStatus(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	status := globalSyncTracker.GetStatus(email)

	type SyncViewData struct {
		UserSyncStatus
		TimeAgo string
	}
	data := SyncViewData{UserSyncStatus: status}
	if status.LastSyncedAt > 0 {
		data.TimeAgo = formatTimeAgo(time.Unix(status.LastSyncedAt, 0))
	}
	renderTemplate(w, "sync_status.html", data)
}

func handleAPISyncStatus(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		email = r.URL.Query().Get("email")
	}
	status := globalSyncTracker.GetStatus(email)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}
