package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSyncTrackerLifecycle(t *testing.T) {
	st := &SyncTracker{
		status: make(map[string]*UserSyncStatus),
	}

	testUser := "developer@sourcerer.io"

	// 1. Initial status is not syncing
	s0 := st.GetStatus(testUser)
	if s0.IsSyncing {
		t.Errorf("Expected IsSyncing=false initially")
	}

	// 2. Start batch with 3 repos
	st.StartBatch(testUser, 3)
	s1 := st.GetStatus(testUser)
	if !s1.IsSyncing || s1.TotalCount != 3 || s1.CompletedCount != 0 {
		t.Errorf("Unexpected status after StartBatch: %+v", s1)
	}

	// 3. Start first job
	st.StartJob(testUser, "sourcerer-io/sourcerer-app")
	s2 := st.GetStatus(testUser)
	if s2.CurrentRepo != "sourcerer-io/sourcerer-app" {
		t.Errorf("Expected CurrentRepo sourcerer-app, got %s", s2.CurrentRepo)
	}

	// 4. Finish first job
	st.FinishJob(testUser, nil)
	s3 := st.GetStatus(testUser)
	if !s3.IsSyncing || s3.CompletedCount != 1 {
		t.Errorf("Expected IsSyncing=true and CompletedCount=1, got %+v", s3)
	}

	// 5. Finish remaining jobs
	st.StartJob(testUser, "sourcerer-io/awesome-libraries")
	st.FinishJob(testUser, nil)
	st.StartJob(testUser, "sourcerer-io/hall-of-fame")
	st.FinishJob(testUser, nil)

	sFinal := st.GetStatus(testUser)
	if sFinal.IsSyncing {
		t.Errorf("Expected IsSyncing=false after all jobs finished, got true")
	}
	if sFinal.CompletedCount != 3 {
		t.Errorf("Expected CompletedCount=3, got %d", sFinal.CompletedCount)
	}
	if sFinal.LastSyncedAt == 0 {
		t.Errorf("Expected LastSyncedAt timestamp to be set")
	}
}

func TestAPISyncStatus(t *testing.T) {
	testEmail := "test-sync@sourcerer.io"
	globalSyncTracker.StartBatch(testEmail, 2)
	globalSyncTracker.StartJob(testEmail, "my-repo")

	req := httptest.NewRequest("GET", "/api/sync-status?email="+testEmail, nil)
	w := httptest.NewRecorder()

	handleAPISyncStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}

	var status UserSyncStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if !status.IsSyncing || status.CurrentRepo != "my-repo" {
		t.Errorf("Unexpected sync status: %+v", status)
	}

	// Clean up
	globalSyncTracker.FinishJob(testEmail, nil)
	globalSyncTracker.FinishJob(testEmail, nil)
}
