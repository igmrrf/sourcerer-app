package main

import (
	"context"
	"log"
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
		return true
	default:
		log.Printf("WARNING: Job queue full, dropping job for %s: %s\n", job.UserEmail, job.RepoURL)
		return false
	}
}

func StartWorker(ctx context.Context) {
	log.Println("Starting background ingestion worker...")
	for {
		select {
		case <-ctx.Done():
			log.Println("Worker shutting down...")
			return
		case job := <-JobQueue:
			processJob(job)
		}
	}
}

func processJob(job IngestionJob) {
	log.Printf("Processing job for %s: %s\n", job.UserEmail, job.RepoURL)

	// Create temporary directory for cloning
	tmpDir, err := os.MkdirTemp("", "sourcerer-*")
	if err != nil {
		log.Printf("Failed to create temp dir: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// Clone repository
	log.Printf("Cloning %s into %s...\n", job.RepoURL, tmpDir)
	cmdClone := exec.Command("git", "clone", "--depth", "1", job.RepoURL, tmpDir)
	cmdClone.Stdout = os.Stdout
	cmdClone.Stderr = os.Stderr
	if err := cmdClone.Run(); err != nil {
		log.Printf("Failed to clone repository %s: %v\n", job.RepoURL, err)
		return
	}

	// Execute Java CLI
	jarPath := "/root/sourcerer-app.jar"
	// if we are running locally (not in docker), we might use a different path
	if _, err := os.Stat(jarPath); os.IsNotExist(err) {
		// Fallback for local testing
		jarPath = filepath.Join("..", "cli", "build", "libs", "sourcerer-app.jar")
	}

	log.Printf("Running headless ingestion on %s...\n", tmpDir)
	cmdJava := exec.Command("java", "-jar", jarPath,
		"--headless",
		"--path", tmpDir,
		"--username", job.UserEmail)
	
	cmdJava.Stdout = os.Stdout
	cmdJava.Stderr = os.Stderr
	if err := cmdJava.Run(); err != nil {
		log.Printf("Failed to run Java CLI for %s: %v\n", job.RepoURL, err)
		return
	}

	log.Printf("Successfully ingested %s for %s\n", job.RepoURL, job.UserEmail)
}
