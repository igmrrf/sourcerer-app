package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bounds on the external tools the worker shells out to. Without these, a
// single unresponsive remote or a pathological repository stalls ingestion
// permanently, because the queue has one consumer.
const (
	lsRemoteTimeout = 15 * time.Second
	cloneTimeout    = 15 * time.Minute
	extractTimeout  = 30 * time.Minute
)

// cliConfigMutex serializes CLI invocations. The Kotlin CLI keeps its
// configuration in a single directory beside the jar and calls resetAndSave()
// on every headless run (cli/src/main/kotlin/app/Main.kt), so two concurrent
// runs would clobber each other's credentials and repo list.
var cliConfigMutex sync.Mutex

type IngestionJob struct {
	RepoURL        string
	RepoName       string
	UserEmail      string
	GitHubPushedAt string
}

var JobQueue = make(chan IngestionJob, 200)

func EnqueueJob(job IngestionJob) bool {
	select {
	case JobQueue <- job:
		slog.Info("Enqueued repository ingestion job", "repo_url", job.RepoURL, "repo_name", job.RepoName, "user_email", job.UserEmail)
		return true
	default:
		slog.Warn("Job queue full, dropping repository ingestion job", "repo_url", job.RepoURL, "repo_name", job.RepoName, "user_email", job.UserEmail)
		return false
	}
}

func StartWorker(ctx context.Context) {
	slog.Info("Starting background ingestion worker")
	for {
		select {
		case <-ctx.Done():
			slog.Info("Background ingestion worker shutting down")
			return
		case job := <-JobQueue:
			// Deliberately detached from ctx: on shutdown the loop stops
			// accepting new jobs, but the job already in flight is allowed to
			// finish (bounded by its own timeouts) rather than leaving a
			// half-written clone and a partial ingest. main() waits for it.
			processJob(context.Background(), job)
		}
	}
}

// resolveJarPath locates the Kotlin extractor jar and rejects placeholder
// files. A zero-length jar passes a naive existence check but makes every
// `java -jar` invocation fail at runtime, which is how a missing build step
// used to surface — one failed job at a time instead of at startup.
func resolveJarPath() (string, error) {
	candidates := []string{
		"/app/sourcerer-app.jar",
		filepath.Join("..", "cli", "build", "libs", "sourcerer-app.jar"),
	}
	if env := os.Getenv("SOURCERER_JAR_PATH"); env != "" {
		candidates = append([]string{env}, candidates...)
	}

	var problems []string
	for _, path := range candidates {
		stat, err := os.Stat(path)
		switch {
		case err != nil:
			problems = append(problems, err.Error())
		case stat.IsDir():
			problems = append(problems, path+": is a directory, not a jar (Docker creates one when a bind-mount source is missing)")
		case stat.Size() == 0:
			problems = append(problems, path+": empty file; build the CLI jar first")
		default:
			return path, nil
		}
	}
	// Report every candidate: the first one failing is the interesting case in
	// Docker, and reporting only the last hides it behind the dev-mode path.
	return "", fmt.Errorf("no usable sourcerer-app.jar: %s", strings.Join(problems, "; "))
}

func computeCommitRehash(gitSHA string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(gitSHA)))
	return hex.EncodeToString(hash[:])
}

func getRemoteHeadSHA(parent context.Context, repoURL string) string {
	ctx, cancel := context.WithTimeout(parent, lsRemoteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-remote", repoURL, "HEAD")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("git ls-remote failed", "repo_url", repoURL, "error", err)
		return ""
	}
	parts := strings.Fields(string(out))
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func processJob(ctx context.Context, job IngestionJob) {
	slog.Info("Processing repository ingestion job", "repo_url", job.RepoURL, "repo_name", job.RepoName, "user_email", job.UserEmail)

	// Step 1: Lightweight remote HEAD check (git ls-remote takes ~100ms and transfers a few bytes)
	remoteHeadSHA := getRemoteHeadSHA(ctx, job.RepoURL)
	var headRehash string
	if remoteHeadSHA != "" {
		headRehash = computeCommitRehash(remoteHeadSHA)

		if db != nil {
			// Check if this latest commit is already indexed in commits or recorded as last_commit_rehash
			var exists bool
			err := db.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM commits WHERE rehash = $1
				) OR EXISTS (
					SELECT 1 FROM repos WHERE (repo_url = $2 OR repo_name = $3) AND last_commit_rehash = $1
				)`, headRehash, job.RepoURL, job.RepoName).Scan(&exists)

			if err == nil && exists {
				slog.Info("Skipping repository ingestion: latest remote HEAD commit already indexed",
					"repo", job.RepoName,
					"head_sha", remoteHeadSHA,
					"head_rehash", headRehash)

				// Update sync timestamps without cloning or running JVM extractor
				_, _ = db.Exec(`
					UPDATE repos 
					SET last_commit_rehash = $1, 
					    last_synced_at = $2, 
					    github_pushed_at = COALESCE(NULLIF($3, ''), github_pushed_at),
					    repo_url = COALESCE(NULLIF($4, ''), repo_url),
					    repo_name = COALESCE(NULLIF($5, ''), repo_name)
					WHERE repo_url = $4 OR repo_name = $5`,
					headRehash, time.Now().Unix(), job.GitHubPushedAt, job.RepoURL, job.RepoName)
				return
			}
		}
	}

	// Step 2: Full ingestion needed — Create temporary directory for cloning
	tmpDir, err := os.MkdirTemp("", "sourcerer-*")
	if err != nil {
		slog.Error("Failed to create temporary directory for clone", "error", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// Clone repository (full clone to preserve complete commit telemetry).
	// git is told never to prompt: a repository that has become private would
	// otherwise block on a credential prompt until the timeout expires.
	slog.Info("Cloning repository", "repo_url", job.RepoURL, "dir", tmpDir)
	cloneCtx, cancelClone := context.WithTimeout(ctx, cloneTimeout)
	defer cancelClone()
	cmdClone := exec.CommandContext(cloneCtx, "git", "clone", job.RepoURL, tmpDir)
	cmdClone.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never")
	cmdClone.Stdout = os.Stdout
	cmdClone.Stderr = os.Stderr
	if err := cmdClone.Run(); err != nil {
		if cloneCtx.Err() == context.DeadlineExceeded {
			slog.Error("Clone timed out", "repo_url", job.RepoURL, "timeout", cloneTimeout)
		} else {
			slog.Error("Failed to clone repository", "repo_url", job.RepoURL, "error", err)
		}
		return
	}

	jarPath, err := resolveJarPath()
	if err != nil {
		slog.Error("Cannot run ingestion CLI", "error", err)
		return
	}

	internalToken := os.Getenv("API_INTERNAL_TOKEN")
	if internalToken == "" {
		internalToken = "dummy_token"
	}

	slog.Info("Running headless ingestion CLI", "path", tmpDir, "user_email", job.UserEmail)
	extractCtx, cancelExtract := context.WithTimeout(ctx, extractTimeout)
	defer cancelExtract()

	// The CLI rewrites its shared on-disk config on every run, so only one may
	// execute at a time.
	cliConfigMutex.Lock()
	cmdJava := exec.CommandContext(extractCtx, "java", "-jar", jarPath,
		"--headless",
		"--path", tmpDir,
		"--username", job.UserEmail,
		"--password", internalToken)

	cmdJava.Stdout = os.Stdout
	cmdJava.Stderr = os.Stderr
	runErr := cmdJava.Run()
	cliConfigMutex.Unlock()

	if runErr != nil {
		if extractCtx.Err() == context.DeadlineExceeded {
			slog.Error("Ingestion CLI timed out", "repo_url", job.RepoURL, "timeout", extractTimeout)
		} else {
			slog.Error("Failed to run Java CLI ingestion", "repo_url", job.RepoURL, "error", runErr)
		}
		return
	}

	// Step 3: Record sync completion in PostgreSQL
	if db != nil && headRehash != "" {
		var repoRehash string
		err := db.QueryRow("SELECT repo_rehash FROM commits WHERE rehash = $1 LIMIT 1", headRehash).Scan(&repoRehash)
		if err == nil && repoRehash != "" {
			_, err = db.Exec(`
				UPDATE repos 
				SET last_commit_rehash = $1, 
				    last_synced_at = $2, 
				    github_pushed_at = COALESCE(NULLIF($3, ''), github_pushed_at),
				    repo_url = COALESCE(NULLIF($4, ''), repo_url),
				    repo_name = COALESCE(NULLIF($5, ''), repo_name)
				WHERE rehash = $6`,
				headRehash, time.Now().Unix(), job.GitHubPushedAt, job.RepoURL, job.RepoName, repoRehash)
			if err != nil {
				slog.Warn("Failed to update repo sync metadata", "repo_url", job.RepoURL, "error", err)
			}
		} else {
			slog.Warn("Could not find repo_rehash for commit", "head_rehash", headRehash, "repo_url", job.RepoURL, "error", err)
		}
	}

	slog.Info("Successfully ingested repository", "repo_url", job.RepoURL, "user_email", job.UserEmail)
}
