package main

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"os"

	"backend/app"

	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/proto"
)

type API struct {
	db *sql.DB
}

func apiAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("API_INTERNAL_TOKEN")
		if token == "" {
			// No token configured, allow all (dev mode)
			next.ServeHTTP(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+token {
			cookie, err := r.Cookie("Token")
			if err != nil || cookie.Value != token {
				slog.Warn("Unauthorized API request rejected", "remote_addr", r.RemoteAddr, "path", r.URL.Path)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(apiAuthMiddleware)

	r.Post("/auth", a.HandleAuth)
	r.Get("/user", a.HandleGetUser)
	r.Post("/user", a.HandlePostUser)
	r.Post("/repo", a.HandlePostRepo)
	r.Post("/commits", a.HandlePostCommits)
	r.Delete("/commits", a.HandleDeleteCommits)
	r.Post("/facts", a.HandlePostFacts)
	r.Post("/authors", a.HandlePostAuthors)
	r.Post("/distances", a.HandlePostDistances)
	r.Post("/process/create", a.HandleProcessCreate)
	r.Post("/process", a.HandleProcess)

	return r
}

func (a *API) HandleAuth(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("API_INTERNAL_TOKEN")
	if token == "" {
		token = "dummy_token"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "Token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
	})
	w.WriteHeader(http.StatusOK)
}


func (a *API) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	u := &app.User{}
	b, err := proto.Marshal(u)
	if err != nil {
		slog.Error("Failed to marshal user protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (a *API) HandlePostUser(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandlePostRepo(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
	if err != nil {
		slog.Error("Failed to read repo request body", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := &app.Repo{}
	if err := proto.Unmarshal(b, repo); err != nil {
		slog.Error("Failed to unmarshal repo protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = a.db.Exec("INSERT INTO repos (rehash, initial_commit_rehash) VALUES ($1, $2) ON CONFLICT DO NOTHING", repo.Rehash, repo.InitialCommitRehash)
	if err != nil {
		slog.Error("Failed to insert repo", "rehash", repo.Rehash, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(b)
}

func (a *API) HandlePostCommits(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		slog.Error("Failed to read commits request body", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cg := &app.CommitGroup{}
	if err := proto.Unmarshal(b, cg); err != nil {
		slog.Error("Failed to unmarshal commits protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		slog.Error("Failed to begin transaction for commits", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtCommit, err := tx.Prepare(`INSERT INTO commits (rehash, repo_rehash, author_name, author_email, date, is_qommit, num_lines_added, num_lines_deleted) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`)
	if err != nil {
		slog.Error("Failed to prepare commit insert statement", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtCommit.Close()

	stmtStats, err := tx.Prepare(`INSERT INTO commit_stats (commit_rehash, num_lines_added, num_lines_deleted, type, tech) 
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`)
	if err != nil {
		slog.Error("Failed to prepare commit_stats insert statement", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtStats.Close()

	for _, c := range cg.Commits {
		_, err := stmtCommit.Exec(c.Rehash, c.RepoRehash, c.AuthorName, c.AuthorEmail, c.Date, c.IsQommit, c.NumLinesAdded, c.NumLinesDeleted)
		if err != nil {
			slog.Error("Error inserting commit", "rehash", c.Rehash, "error", err)
			continue
		}

		for _, s := range c.Stats {
			_, err := stmtStats.Exec(c.Rehash, s.NumLinesAdded, s.NumLinesDeleted, s.Type, s.Tech)
			if err != nil {
				slog.Error("Error inserting commit_stats", "commit_rehash", c.Rehash, "tech", s.Tech, "error", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Error("Failed to commit transaction for commits", "error", err)
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandleDeleteCommits(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		slog.Error("Failed to read delete commits request body", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cg := &app.CommitGroup{}
	if err := proto.Unmarshal(b, cg); err != nil {
		slog.Error("Failed to unmarshal delete commits protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		slog.Error("Failed to begin transaction for delete commits", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	for _, c := range cg.Commits {
		_, _ = tx.Exec("DELETE FROM commit_stats WHERE commit_rehash = $1", c.Rehash)
		_, _ = tx.Exec("DELETE FROM commits WHERE rehash = $1", c.Rehash)
	}

	if err := tx.Commit(); err != nil {
		slog.Error("Failed to commit delete commits transaction", "error", err)
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandlePostDistances(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}


func (a *API) HandlePostFacts(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		slog.Error("Failed to read facts request body", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fg := &app.FactGroup{}
	if err := proto.Unmarshal(b, fg); err != nil {
		slog.Error("Failed to unmarshal facts protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		slog.Error("Failed to begin transaction for facts", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtFacts, err := tx.Prepare(`INSERT INTO facts (repo_rehash, email, code, key, value1, value2, value3, value4) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`)
	if err != nil {
		slog.Error("Failed to prepare facts insert statement", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtFacts.Close()

	for _, f := range fg.Facts {
		_, err := stmtFacts.Exec(f.RepoRehash, f.Email, f.Code, f.Key, f.Value1, f.Value2, f.Value3, f.Value4)
		if err != nil {
			slog.Error("Error inserting fact", "repo_rehash", f.RepoRehash, "email", f.Email, "code", f.Code, "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Error("Failed to commit transaction for facts", "error", err)
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandlePostAuthors(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		slog.Error("Failed to read authors request body", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ag := &app.AuthorGroup{}
	if err := proto.Unmarshal(b, ag); err != nil {
		slog.Error("Failed to unmarshal authors protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		slog.Error("Failed to begin transaction for authors", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtAuthors, err := tx.Prepare("INSERT INTO authors (email, name, repo_rehash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING")
	if err != nil {
		slog.Error("Failed to prepare authors insert statement", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtAuthors.Close()

	for _, author := range ag.Authors {
		_, err := stmtAuthors.Exec(author.Email, author.Name, author.RepoRehash)
		if err != nil {
			slog.Error("Error inserting author", "email", author.Email, "repo_rehash", author.RepoRehash, "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Error("Failed to commit transaction for authors", "error", err)
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandleProcessCreate(w http.ResponseWriter, r *http.Request) {
	p := &app.Process{Id: 1}
	b, err := proto.Marshal(p)
	if err != nil {
		slog.Error("Failed to marshal process protobuf", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (a *API) HandleProcess(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

