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
	_ "github.com/lib/pq"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
)

// Configure OAuth
var sessionSecret []byte

func signCookie(value string) string {
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(value))
	signature := hex.EncodeToString(mac.Sum(nil))
	encoded := base64.URLEncoding.EncodeToString([]byte(value))
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
	return string(decoded), true
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
	Scopes:       []string{"repo", "user:email"},
	Endpoint:     githuboauth.Endpoint,
}

//go:embed templates/*
var templateFS embed.FS

//go:embed schema.sql
var schemaSQL string

var (
	prodTmpl *template.Template
	db       *sql.DB
)


func isDevMode() bool {
	env := strings.ToLower(os.Getenv("ENV"))
	if env == "development" || env == "dev" {
		return true
	}
	if _, err := os.Stat("templates/index.html"); err == nil && env != "production" {
		return true
	}
	return false
}

// renderTemplate renders HTML templates, dynamically hot-reloading from disk in dev mode
// or using embedded templates in production mode for optimal performance.
func renderTemplate(w http.ResponseWriter, name string, data any) {
	var t *template.Template
	var err error

	if isDevMode() {
		t, err = template.ParseGlob("templates/*.html")
		if err != nil {
			slog.Error("Failed to parse templates from disk (hot reload)", "error", err)
			http.Error(w, "Template parse error", http.StatusInternalServerError)
			return
		}
	} else {
		t = prodTmpl
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
		sessionSecret = make([]byte, 32)
		if _, err := rand.Read(sessionSecret); err != nil {
			slog.Error("Failed to generate session secret", "error", err)
			os.Exit(1)
		}
		slog.Warn("SESSION_SECRET not set. Generated a random one. Sessions will not persist across restarts.")
	}

	// Parse embedded production templates
	var err error
	prodTmpl, err = template.ParseFS(templateFS, "templates/*.html")
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
		slog.Warn("Could not ping database on startup. Ensure Postgres is running", "error", err)
	} else {
		slog.Info("Successfully connected to PostgreSQL")
		// Automatically apply database schema
		if schemaSQL != "" {
			if _, err := db.Exec(schemaSQL); err != nil {
				slog.Error("Failed to apply database schema", "error", err)
			} else {
				slog.Info("Database schema verified and applied successfully")
			}
		}
	}

	api := &API{db: db}

	// Start background worker
	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go StartWorker(ctxWorker)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Rate limiting / request throttling (protects backend against excessive concurrent requests)
	r.Use(middleware.ThrottleBacklog(100, 50, 5*time.Second))

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	// API routes for Sourcerer CLI ingestion
	r.Mount("/api", api.Routes())

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
		type Author struct {
			Email string
			Name  string
		}
		type UserInfo struct {
			Name  string
			Email string
		}

		sessionEmail := getSessionEmail(r)
		var user *UserInfo
		if sessionEmail != "" {
			user = &UserInfo{Name: sessionEmail, Email: sessionEmail}
		}

		var authors []Author
		rows, err := db.Query("SELECT DISTINCT email, name FROM authors WHERE email IS NOT NULL AND email != ''")
		if err != nil {
			slog.Error("Error querying authors", "error", err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var a Author
				if err := rows.Scan(&a.Email, &a.Name); err == nil {
					authors = append(authors, a)
				}
			}
		}

		data := struct {
			Authors []Author
			User    *UserInfo
		}{
			Authors: authors,
			User:    user,
		}
		renderTemplate(w, "index.html", data)
	})

	r.Get("/dashboard/stats", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		var totalCommits int
		var totalLinesAdded int
		var totalLinesDeleted int

		if email != "" {
			if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", email).Scan(&totalCommits, &totalLinesAdded, &totalLinesDeleted); err != nil && err != sql.ErrNoRows {
				slog.Error("Error querying stats for author", "email", email, "error", err)
			}
		} else {
			if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits").Scan(&totalCommits, &totalLinesAdded, &totalLinesDeleted); err != nil && err != sql.ErrNoRows {
				slog.Error("Error querying overall stats", "error", err)
			}
		}

		data := struct {
			TotalCommits int
			LinesAdded   int
			LinesDeleted int
		}{
			TotalCommits: totalCommits,
			LinesAdded:   totalLinesAdded,
			LinesDeleted: totalLinesDeleted,
		}
		renderTemplate(w, "stats.html", data)
	})

	r.Get("/dashboard/languages", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		var rows *sql.Rows
		var err error

		if email != "" {
			rows, err = db.Query(`SELECT s.tech, SUM(s.num_lines_added) as lines FROM commit_stats s JOIN commits c ON c.rehash = s.commit_rehash WHERE c.author_email = $1 GROUP BY s.tech ORDER BY lines DESC LIMIT 10`, email)
		} else {
			rows, err = db.Query("SELECT tech, SUM(num_lines_added) as lines FROM commit_stats GROUP BY tech ORDER BY lines DESC LIMIT 10")
		}

		if err != nil {
			slog.Error("Error querying languages", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type LangStat struct {
			Tech       string
			Lines      int
			Percentage int
		}
		var stats []LangStat
		totalLines := 0
		for rows.Next() {
			var s LangStat
			if err := rows.Scan(&s.Tech, &s.Lines); err == nil {
				if s.Tech != "" {
					stats = append(stats, s)
					totalLines += s.Lines
				}
			}
		}
		if totalLines > 0 {
			for i := range stats {
				stats[i].Percentage = int((float64(stats[i].Lines) / float64(totalLines)) * 100.0)
				if stats[i].Percentage == 0 && stats[i].Lines > 0 {
					stats[i].Percentage = 1
				}
			}
		}
		renderTemplate(w, "languages.html", stats)
	})

	r.Get("/dashboard/repos", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		type RepoItem struct {
			Rehash       string
			CommitCount  int
			LinesAdded   int
			LinesDeleted int
		}
		var repos []RepoItem
		var rows *sql.Rows
		var err error

		if email != "" {
			rows, err = db.Query(`
				SELECT r.rehash, COUNT(c.rehash) as commit_count, 
				       COALESCE(SUM(c.num_lines_added), 0) as lines_added, 
				       COALESCE(SUM(c.num_lines_deleted), 0) as lines_deleted
				FROM repos r
				JOIN commits c ON c.repo_rehash = r.rehash
				WHERE c.author_email = $1
				GROUP BY r.rehash
				ORDER BY commit_count DESC
				LIMIT 10`, email)
		} else {
			rows, err = db.Query(`
				SELECT r.rehash, COUNT(c.rehash) as commit_count, 
				       COALESCE(SUM(c.num_lines_added), 0) as lines_added, 
				       COALESCE(SUM(c.num_lines_deleted), 0) as lines_deleted
				FROM repos r
				LEFT JOIN commits c ON c.repo_rehash = r.rehash
				GROUP BY r.rehash
				ORDER BY commit_count DESC
				LIMIT 10`)
		}

		if err != nil {
			slog.Error("Error querying repos", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var repo RepoItem
			if err := rows.Scan(&repo.Rehash, &repo.CommitCount, &repo.LinesAdded, &repo.LinesDeleted); err == nil {
				repos = append(repos, repo)
			}
		}
		renderTemplate(w, "repos.html", repos)
	})

	r.Get("/dashboard/facts", handleDashboardFacts)

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
		os.Exit(1)
	}

	slog.Info("Server exited cleanly")
}

func handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	stateBytes := make([]byte, 32)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		HttpOnly: true,
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

	// Set a signed session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    signCookie(email),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})

	// Fetch repos in background to avoid blocking request
	go func(email string, token *oauth2.Token) {
		client := github.NewClient(oauthConfig.Client(context.Background(), token))
		opt := &github.RepositoryListByAuthenticatedUserOptions{
			Type:        "owner",
			ListOptions: github.ListOptions{PerPage: 100},
		}
		repos, _, err := client.Repositories.ListByAuthenticatedUser(context.Background(), opt)
		if err != nil {
			slog.Error("Failed to fetch repositories for user", "email", email, "error", err)
			return
		}

		slog.Info("Discovered repositories for user", "count", len(repos), "email", email)
		for _, repo := range repos {
			EnqueueJob(IngestionJob{
				RepoURL:   repo.GetCloneURL(),
				UserEmail: email,
			})
		}
	}(email, token)

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
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

func getLanguagesForEmail(email string) []LangStat {
	var rows *sql.Rows
	var err error

	if email != "" {
		rows, err = db.Query(`SELECT s.tech, SUM(s.num_lines_added) as lines FROM commit_stats s JOIN commits c ON c.rehash = s.commit_rehash WHERE c.author_email = $1 GROUP BY s.tech ORDER BY lines DESC LIMIT 10`, email)
	} else {
		rows, err = db.Query("SELECT tech, SUM(num_lines_added) as lines FROM commit_stats GROUP BY tech ORDER BY lines DESC LIMIT 10")
	}

	if err != nil {
		slog.Error("Error querying languages", "error", err)
		return nil
	}
	defer rows.Close()

	var stats []LangStat
	totalLines := 0
	for rows.Next() {
		var s LangStat
		if err := rows.Scan(&s.Tech, &s.Lines); err == nil {
			if s.Tech != "" {
				stats = append(stats, s)
				totalLines += s.Lines
			}
		}
	}
	if totalLines > 0 {
		for i := range stats {
			stats[i].Percentage = int((float64(stats[i].Lines) / float64(totalLines)) * 100.0)
			if stats[i].Percentage == 0 && stats[i].Lines > 0 {
				stats[i].Percentage = 1
			}
		}
	}
	return stats
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
	email := r.URL.Query().Get("email")
	facts := computeFacts(email)
	renderTemplate(w, "facts.html", facts)
}

func handlePublicProfile(w http.ResponseWriter, r *http.Request) {
	identifier := chi.URLParam(r, "email")
	if identifier == "" {
		identifier = chi.URLParam(r, "username")
	}

	type RepoInfo struct {
		Rehash       string
		CommitCount  int
		LinesAdded   int
		LinesDeleted int
	}

	type ProfileData struct {
		Name         string
		Email        string
		TotalCommits int
		LinesAdded   int
		LinesDeleted int
		Languages    []LangStat
		Facts        FactsData
		Repos        []RepoInfo
	}

	var data ProfileData
	data.Email = identifier
	data.Name = identifier

	// Look up author name if available
	_ = db.QueryRow("SELECT name FROM authors WHERE email = $1 LIMIT 1", identifier).Scan(&data.Name)
	if data.Name == "" {
		data.Name = identifier
	}

	// Total commits, lines
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", identifier).Scan(&data.TotalCommits, &data.LinesAdded, &data.LinesDeleted)

	// Languages
	data.Languages = getLanguagesForEmail(identifier)

	// Facts
	data.Facts = computeFacts(identifier)

	// Repos
	rows, err := db.Query(`
		SELECT r.rehash, COUNT(c.rehash) as commit_count, 
		       COALESCE(SUM(c.num_lines_added), 0) as lines_added, 
		       COALESCE(SUM(c.num_lines_deleted), 0) as lines_deleted
		FROM repos r
		JOIN commits c ON c.repo_rehash = r.rehash
		WHERE c.author_email = $1
		GROUP BY r.rehash
		ORDER BY commit_count DESC
		LIMIT 10`, identifier)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var repo RepoInfo
			if err := rows.Scan(&repo.Rehash, &repo.CommitCount, &repo.LinesAdded, &repo.LinesDeleted); err == nil {
				data.Repos = append(data.Repos, repo)
			}
		}
	}

	renderTemplate(w, "profile.html", data)
}

func generateBadgeSVG(name string, totalCommits int, linesAdded int, linesDeleted int, languages []LangStat) []byte {
	if name == "" {
		name = "Sourcerer Engineer"
	}

	palette := []string{"#8b5cf6", "#a855f7", "#06b6d4", "#10b981", "#f59e0b"}

	var langBars strings.Builder
	var langLegend strings.Builder

	currentX := 25.0
	barWidthTotal := 445.0

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

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="495" height="195" viewBox="0 0 495 195" fill="none">
	<style>
		.header { font: 700 16px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #f8fafc; }
		.sub { font: 500 12px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #94a3b8; }
		.stat-val { font: 700 18px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #f8fafc; }
		.stat-label { font: 600 10px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #64748b; text-transform: uppercase; letter-spacing: 0.5px; }
		.added { fill: #10b981; }
		.deleted { fill: #f43f5e; }
		.brand { font: 800 12px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; fill: #a855f7; }
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

func handleBadgeSVG(w http.ResponseWriter, r *http.Request) {
	identifier := chi.URLParam(r, "identifier")
	identifier = strings.TrimSuffix(identifier, ".svg")

	var name string
	_ = db.QueryRow("SELECT name FROM authors WHERE email = $1 LIMIT 1", identifier).Scan(&name)
	if name == "" {
		name = identifier
	}

	var totalCommits, linesAdded, linesDeleted int
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", identifier).Scan(&totalCommits, &linesAdded, &linesDeleted)

	languages := getLanguagesForEmail(identifier)

	svgBytes := generateBadgeSVG(name, totalCommits, linesAdded, linesDeleted, languages)

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	w.WriteHeader(http.StatusOK)
	w.Write(svgBytes)
}

// Hall of Fame Data Structures & Handlers (Feature: hall-of-fame)

type ContributorStat struct {
	Email        string `json:"email"`
	Name         string `json:"name"`
	Commits      int    `json:"commits"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesDeleted int    `json:"lines_deleted,omitempty"`
	IsSourcerer  bool   `json:"is_sourcerer"`
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
		       bool_or(u.email IS NOT NULL) as is_sourcerer
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		ORDER BY commit_count DESC
		LIMIT 6`, repoRehash)
	if err == nil {
		defer topRows.Close()
		for topRows.Next() {
			var cs ContributorStat
			if err := topRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer); err == nil {
				data.TopContributors = append(data.TopContributors, cs)
			}
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
		       bool_or(u.email IS NOT NULL) as is_sourcerer
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.date >= $2 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		ORDER BY commit_count DESC
		LIMIT 6`, repoRehash, cutoff)
	if err == nil {
		defer trendRows.Close()
		for trendRows.Next() {
			var cs ContributorStat
			if err := trendRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer); err == nil {
				data.TrendingContributors = append(data.TrendingContributors, cs)
			}
		}
	}

	// 5. New Contributors (initial commit in this repo within cutoff)
	newRows, err := db.Query(`
		SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name, 
		       COUNT(*) as commit_count,
		       bool_or(u.email IS NOT NULL) as is_sourcerer
		FROM commits c
		LEFT JOIN authors a ON a.email = c.author_email
		LEFT JOIN users u ON u.email = c.author_email
		WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
		GROUP BY c.author_email
		HAVING MIN(c.date) >= $2
		ORDER BY MIN(c.date) DESC
		LIMIT 6`, repoRehash, cutoff)
	if err == nil {
		defer newRows.Close()
		for newRows.Next() {
			var cs ContributorStat
			if err := newRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.IsSourcerer); err == nil {
				data.NewContributors = append(data.NewContributors, cs)
			}
		}
	}
	// Fallback if no new contributors within cutoff: fetch latest first-time joiners
	if len(data.NewContributors) == 0 {
		fallbackNewRows, err := db.Query(`
			SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name, 
			       COUNT(*) as commit_count,
			       bool_or(u.email IS NOT NULL) as is_sourcerer
			FROM commits c
			LEFT JOIN authors a ON a.email = c.author_email
			LEFT JOIN users u ON u.email = c.author_email
			WHERE c.repo_rehash = $1 AND c.author_email IS NOT NULL AND c.author_email != ''
			GROUP BY c.author_email
			ORDER BY MIN(c.date) DESC
			LIMIT 6`, repoRehash)
		if err == nil {
			defer fallbackNewRows.Close()
			for fallbackNewRows.Next() {
				var cs ContributorStat
				if err := fallbackNewRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.IsSourcerer); err == nil {
					data.NewContributors = append(data.NewContributors, cs)
				}
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
		totalLines := 0
		for langRows.Next() {
			var s LangStat
			if err := langRows.Scan(&s.Tech, &s.Lines); err == nil && s.Tech != "" {
				data.Languages = append(data.Languages, s)
				totalLines += s.Lines
			}
		}
		if totalLines > 0 {
			for i := range data.Languages {
				data.Languages[i].Percentage = int((float64(data.Languages[i].Lines) / float64(totalLines)) * 100.0)
				if data.Languages[i].Percentage == 0 && data.Languages[i].Lines > 0 {
					data.Languages[i].Percentage = 1
				}
			}
		}
	}

	return data
}

func generateHallOfFameSVG(data HallOfFameData) []byte {
	palette := []string{"#8b5cf6", "#a855f7", "#06b6d4", "#10b981", "#f59e0b"}

	var langBars strings.Builder
	var langLegend strings.Builder

	currentX := 25.0
	barWidthTotal := 790.0

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
			if len(displayName) > 16 {
				displayName = displayName[:14] + "…"
			}

			haloAttr := ""
			if c.IsSourcerer {
				haloAttr = fmt.Sprintf(`<circle cx="%d" cy="%d" r="16" fill="none" stroke="#a855f7" stroke-width="1.5" stroke-dasharray="2 2" />`, startX+16, rowY+12)
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
	if len(repoDisplay) > 28 {
		repoDisplay = repoDisplay[:26] + "…"
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
	<text x="815" y="329" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10" font-weight="600" text-anchor="end">sourcerer.io/r/%s</text>
</svg>`,
		template.HTMLEscapeString(repoDisplay),
		data.TotalCommits, data.TotalContributors,
		topListSVG, trendingListSVG, newListSVG,
		langBars.String(),
		langLegend.String(),
		template.HTMLEscapeString(repoDisplay),
	)

	return []byte(svg)
}

func handleHallOfFameSVG(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)
	svgBytes := generateHallOfFameSVG(data)

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	w.WriteHeader(http.StatusOK)
	w.Write(svgBytes)
}

func handleHallOfFameHTML(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)
	renderTemplate(w, "repo.html", data)
}

func handleHallOfFameAPI(w http.ResponseWriter, r *http.Request) {
	repoRehash := chi.URLParam(r, "repo")
	repoRehash = strings.TrimSuffix(repoRehash, ".svg")

	data := getHallOfFameData(repoRehash)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode Hall of Fame API response", "error", err)
	}
}




