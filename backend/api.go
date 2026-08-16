package main

import (
	"database/sql"
	"io"
	"log"
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
	r.Post("/facts", a.HandlePostFacts)
	r.Post("/authors", a.HandlePostAuthors)
	r.Post("/process/create", a.HandleProcessCreate)
	r.Post("/process", a.HandleProcess)

	return r
}

func (a *API) HandleAuth(w http.ResponseWriter, r *http.Request) {
	// Dummy auth
	w.Header().Set("Set-Cookie", "Token=dummy_token; Path=/")
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	u := &app.User{}
	b, err := proto.Marshal(u)
	if err != nil {
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := &app.Repo{}
	if err := proto.Unmarshal(b, repo); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Just insert ignore
	_, err = a.db.Exec("INSERT INTO repos (rehash, initial_commit_rehash) VALUES ($1, $2) ON CONFLICT DO NOTHING", repo.Rehash, repo.InitialCommitRehash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(b)
}

func (a *API) HandlePostCommits(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cg := &app.CommitGroup{}
	if err := proto.Unmarshal(b, cg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtCommit, err := tx.Prepare(`INSERT INTO commits (rehash, repo_rehash, author_name, author_email, date, is_qommit, num_lines_added, num_lines_deleted) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtCommit.Close()

	stmtStats, err := tx.Prepare(`INSERT INTO commit_stats (commit_rehash, num_lines_added, num_lines_deleted, type, tech) 
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtStats.Close()

	for _, c := range cg.Commits {
		_, err := stmtCommit.Exec(c.Rehash, c.RepoRehash, c.AuthorName, c.AuthorEmail, c.Date, c.IsQommit, c.NumLinesAdded, c.NumLinesDeleted)
		if err != nil {
			log.Println("Error inserting commit:", err)
			continue
		}

		for _, s := range c.Stats {
			_, err := stmtStats.Exec(c.Rehash, s.NumLinesAdded, s.NumLinesDeleted, s.Type, s.Tech)
			if err != nil {
				log.Println("Error inserting commit_stats:", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandlePostFacts(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fg := &app.FactGroup{}
	if err := proto.Unmarshal(b, fg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtFacts, err := tx.Prepare(`INSERT INTO facts (repo_rehash, email, code, key, value1, value2, value3, value4) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtFacts.Close()

	for _, f := range fg.Facts {
		_, err := stmtFacts.Exec(f.RepoRehash, f.Email, f.Code, f.Key, f.Value1, f.Value2, f.Value3, f.Value4)
		if err != nil {
			log.Println("Error inserting fact:", err)
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandlePostAuthors(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 50<<20)) // 50MB limit
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ag := &app.AuthorGroup{}
	if err := proto.Unmarshal(b, ag); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmtAuthors, err := tx.Prepare("INSERT INTO authors (email, name, repo_rehash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmtAuthors.Close()

	for _, author := range ag.Authors {
		_, err := stmtAuthors.Exec(author.Email, author.Name, author.RepoRehash)
		if err != nil {
			log.Println("Error inserting author:", err)
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) HandleProcessCreate(w http.ResponseWriter, r *http.Request) {
	p := &app.Process{Id: 1}
	b, err := proto.Marshal(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (a *API) HandleProcess(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
