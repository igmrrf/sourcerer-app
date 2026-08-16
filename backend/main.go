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
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
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
var tmpl *template.Template

func main() {
	secretStr := os.Getenv("SESSION_SECRET")
	if secretStr != "" {
		sessionSecret = []byte(secretStr)
	} else {
		sessionSecret = make([]byte, 32)
		if _, err := rand.Read(sessionSecret); err != nil {
			log.Fatalf("Failed to generate session secret: %v", err)
		}
		log.Println("WARNING: SESSION_SECRET not set. Generated a random one. Sessions will not persist across restarts.")
	}

	// Parse templates
	var err error
	tmpl, err = template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatalf("Error parsing templates: %v", err)
	}

	// Connect to Postgres
	// You may need to change these credentials to match your local setup
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "host=localhost user=postgres password=postgres dbname=postgres sslmode=disable"
	}
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Printf("Warning: Could not ping db. Ensure Postgres is running: %v", err)
	}

	api := &API{db: db}

	// Start background worker
	ctxWorker, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go StartWorker(ctxWorker)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

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
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("db: unhealthy"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// OAuth Routes
	r.Get("/auth/github/login", handleGitHubLogin)
	r.Get("/auth/github/callback", handleGitHubCallback)

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
			log.Printf("Error querying authors: %v", err)
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
		tmpl.ExecuteTemplate(w, "index.html", data)
	})

	r.Get("/dashboard/stats", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		var totalCommits int
		var totalLinesAdded int
		var totalLinesDeleted int

		if email != "" {
			if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits WHERE author_email = $1", email).Scan(&totalCommits, &totalLinesAdded, &totalLinesDeleted); err != nil && err != sql.ErrNoRows {
				log.Printf("Error querying stats for %s: %v", email, err)
			}
		} else {
			if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(num_lines_added), 0), COALESCE(SUM(num_lines_deleted), 0) FROM commits").Scan(&totalCommits, &totalLinesAdded, &totalLinesDeleted); err != nil && err != sql.ErrNoRows {
				log.Printf("Error querying overall stats: %v", err)
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
		tmpl.ExecuteTemplate(w, "stats.html", data)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type LangStat struct {
			Tech  string
			Lines int
		}
		var stats []LangStat
		for rows.Next() {
			var s LangStat
			if err := rows.Scan(&s.Tech, &s.Lines); err == nil {
				if s.Tech != "" {
					stats = append(stats, s)
				}
			}
		}
		tmpl.ExecuteTemplate(w, "languages.html", stats)
	})

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
		log.Printf("Server starting on :%s...\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Listen error: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exiting")
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
		http.Error(w, "State cookie not found", http.StatusBadRequest)
		return
	}
	queryState := r.URL.Query().Get("state")
	if !hmac.Equal([]byte(stateCookie.Value), []byte(queryState)) {
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
		http.Error(w, "Code not found", http.StatusBadRequest)
		return
	}

	token, err := oauthConfig.Exchange(context.Background(), code)
	if err != nil {
		http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
		return
	}

	client := github.NewClient(oauthConfig.Client(context.Background(), token))

	user, _, err := client.Users.Get(context.Background(), "")
	if err != nil {
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		return
	}

	email := ""
	if user.Email != nil {
		email = *user.Email
	} else {
		email = user.GetLogin() + "@users.noreply.github.com"
	}

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
			log.Printf("Failed to fetch repos for %s: %v\n", email, err)
			return
		}

		for _, repo := range repos {
			EnqueueJob(IngestionJob{
				RepoURL:   repo.GetCloneURL(),
				UserEmail: email,
			})
		}
	}(email, token)

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}
