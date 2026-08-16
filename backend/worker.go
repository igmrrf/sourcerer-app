package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

type IngestionJob struct {
	RepoURL   string
	UserEmail string
}

var JobQueue = make(chan IngestionJob, 100)

func EnqueueJob(job IngestionJob) bool {
	select {
	case JobQueue <- job:
		slog.Info("Enqueued repository ingestion job", "repo_url", job.RepoURL, "user_email", job.UserEmail)
		return true
	default:
		slog.Warn("Job queue full, dropping repository ingestion job", "repo_url", job.RepoURL, "user_email", job.UserEmail)
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

func processJob(job IngestionJob) {
	slog.Info("Processing repository ingestion job", "repo_url", job.RepoURL, "user_email", job.UserEmail)

	// Create temporary directory for cloning
	tmpDir, err := os.MkdirTemp("", "sourcerer-*")
	if err != nil {
		slog.Error("Failed to create temporary directory for clone", "error", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// Clone repository (full clone without --depth 1 to preserve full commit history)
	slog.Info("Cloning repository", "repo_url", job.RepoURL, "dir", tmpDir)
	cmdClone := exec.Command("git", "clone", job.RepoURL, tmpDir)
	cmdClone.Stdout = os.Stdout
	cmdClone.Stderr = os.Stderr
	if err := cmdClone.Run(); err != nil {
		slog.Error("Failed to clone repository", "repo_url", job.RepoURL, "error", err)
		return
	}

	// Execute Java CLI
	jarPath := "/root/sourcerer-app.jar"
	// Fallback for local development if not running inside Docker
	if _, err := os.Stat(jarPath); os.IsNotExist(err) {
		jarPath = filepath.Join("..", "cli", "build", "libs", "sourcerer-app.jar")
	}

	slog.Info("Running headless ingestion CLI", "path", tmpDir, "user_email", job.UserEmail)
	cmdJava := exec.Command("java", "-jar", jarPath,
		"--headless",
		"--path", tmpDir,
		"--username", job.UserEmail)

	cmdJava.Stdout = os.Stdout
	cmdJava.Stderr = os.Stderr
	if err := cmdJava.Run(); err != nil {
		slog.Error("Failed to run Java CLI ingestion", "repo_url", job.RepoURL, "error", err)
		return
	}

	slog.Info("Successfully ingested repository", "repo_url", job.RepoURL, "user_email", job.UserEmail)
}

