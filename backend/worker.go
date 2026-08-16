package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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
			processJob(job)
		}
	}
}

func computeCommitRehash(gitSHA string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(gitSHA)))
	return hex.EncodeToString(hash[:])
}

func getRemoteHeadSHA(repoURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func processJob(job IngestionJob) {
	slog.Info("Processing repository ingestion job", "repo_url", job.RepoURL, "repo_name", job.RepoName, "user_email", job.UserEmail)

	// Step 1: Lightweight remote HEAD check (git ls-remote takes ~100ms and transfers a few bytes)
	remoteHeadSHA := getRemoteHeadSHA(job.RepoURL)
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

	// Clone repository (full clone to preserve complete commit telemetry)
	slog.Info("Cloning repository", "repo_url", job.RepoURL, "dir", tmpDir)
	cmdClone := exec.Command("git", "clone", job.RepoURL, tmpDir)
	cmdClone.Stdout = os.Stdout
	cmdClone.Stderr = os.Stderr
	if err := cmdClone.Run(); err != nil {
		slog.Error("Failed to clone repository", "repo_url", job.RepoURL, "error", err)
		return
	}

	// Execute Java CLI headless pipeline
	jarPath := "/root/sourcerer-app.jar"
	// Fallback for local development if not running inside Docker
	if _, err := os.Stat(jarPath); os.IsNotExist(err) {
		jarPath = filepath.Join("..", "cli", "build", "libs", "sourcerer-app.jar")
	}

	internalToken := os.Getenv("API_INTERNAL_TOKEN")
	if internalToken == "" {
		internalToken = "dummy_token"
	}

	slog.Info("Running headless ingestion CLI", "path", tmpDir, "user_email", job.UserEmail)
	cmdJava := exec.Command("java", "-jar", jarPath,
		"--headless",
		"--path", tmpDir,
		"--username", job.UserEmail,
		"--password", internalToken)

	cmdJava.Stdout = os.Stdout
	cmdJava.Stderr = os.Stderr
	if err := cmdJava.Run(); err != nil {
		slog.Error("Failed to run Java CLI ingestion", "repo_url", job.RepoURL, "error", err)
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
