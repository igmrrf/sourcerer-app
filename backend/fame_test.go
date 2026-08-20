package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestIndividualFameSVGGeneration(t *testing.T) {
	entry := FameEntry{
		Index:       0,
		Username:    "alice",
		Name:        "Alice Developer",
		Email:       "alice@example.com",
		Commits:     42,
		Badge:       "top",
		BadgeColor:  BadgeColorTop,
		IsSourcerer: true,
		ProfileID:   "alice-profile",
		ProfileURL:  "https://sourcerer.io/p/alice-profile",
	}

	svgBytes := generateIndividualFameSVG(entry)
	svg := string(svgBytes)

	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "</svg>") {
		t.Fatalf("Generated Fame badge is not a valid SVG: %s", svg)
	}

	if !strings.Contains(svg, "top") {
		t.Errorf("Expected 'top' tier tag in SVG, got: %s", svg)
	}

	if !strings.Contains(svg, "42") {
		t.Errorf("Expected '42' in SVG, got: %s", svg)
	}

	// Verify Legend SVG
	legendSVG := string(generateFameLegendSVG())
	if !strings.Contains(legendSVG, "weekly") || !strings.Contains(legendSVG, "all time") {
		t.Errorf("Legend SVG missing key text: %s", legendSVG)
	}

	// Verify Empty Spacer SVG
	emptySVG := string(generateFameEmptySVG())
	if !strings.Contains(emptySVG, "<svg") {
		t.Errorf("Empty slot SVG invalid")
	}
}

func TestFaceHofMatchHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/face/hof/match?names=torvalds,alice", nil)
	w := httptest.NewRecorder()

	handleFaceHofMatch(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK from match API, got %d", resp.StatusCode)
	}

	var match map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&match); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}
}

func TestFameImageAndLinkRoutes(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/fame/{repo}/images/{num}", handleFameImage)
	r.Get("/fame/{repo}/links/{num}", handleFameLink)

	// Test image endpoint for slot 7 (legend)
	reqImg := httptest.NewRequest("GET", "/fame/testrepo/images/7", nil)
	wImg := httptest.NewRecorder()
	r.ServeHTTP(wImg, reqImg)

	if wImg.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for fame image slot 7, got %d", wImg.Code)
	}
	if !strings.Contains(wImg.Header().Get("Content-Type"), "image/svg+xml") {
		t.Errorf("Expected Content-Type image/svg+xml, got %s", wImg.Header().Get("Content-Type"))
	}

	// Test link endpoint
	reqLink := httptest.NewRequest("GET", "/fame/testrepo/links/7", nil)
	wLink := httptest.NewRecorder()
	r.ServeHTTP(wLink, reqLink)

	if wLink.Code != http.StatusFound && wLink.Code != http.StatusTemporaryRedirect {
		t.Errorf("Expected redirect status for fame link, got %d", wLink.Code)
	}
}

func TestLibraryFameSlotsAndRoutes(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/fame/lib/{tech}/images/{num}", handleLibraryFameImage)
	r.Get("/fame/lib/{tech}/links/{num}", handleLibraryFameLink)
	r.Get("/libraries/{tech}", handleLibraryDetailHTML)
	r.Get("/api/hall-of-fame/lib/{tech}", handleLibraryFameAPI)

	// Test library fame image
	reqImg := httptest.NewRequest("GET", "/fame/lib/py.numpy/images/0", nil)
	wImg := httptest.NewRecorder()
	r.ServeHTTP(wImg, reqImg)

	if wImg.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for library fame image slot 0, got %d", wImg.Code)
	}
	if !strings.Contains(wImg.Header().Get("Content-Type"), "image/svg+xml") {
		t.Errorf("Expected Content-Type image/svg+xml, got %s", wImg.Header().Get("Content-Type"))
	}

	// Test library fame link redirect
	reqLink := httptest.NewRequest("GET", "/fame/lib/py.numpy/links/0", nil)
	wLink := httptest.NewRecorder()
	r.ServeHTTP(wLink, reqLink)

	if wLink.Code != http.StatusFound && wLink.Code != http.StatusTemporaryRedirect {
		t.Errorf("Expected redirect for library fame link, got %d", wLink.Code)
	}

	// Test library SVG banner
	reqSVG := httptest.NewRequest("GET", "/libraries/py.numpy.svg", nil)
	wSVG := httptest.NewRecorder()
	r.ServeHTTP(wSVG, reqSVG)

	if wSVG.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for library SVG banner, got %d", wSVG.Code)
	}
	if !strings.Contains(wSVG.Body.String(), "<svg") {
		t.Errorf("Expected valid SVG for library banner, got %s", wSVG.Body.String())
	}

	// Test library fame API
	reqAPI := httptest.NewRequest("GET", "/api/hall-of-fame/lib/py.numpy", nil)
	wAPI := httptest.NewRecorder()
	r.ServeHTTP(wAPI, reqAPI)

	if wAPI.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for library fame API, got %d", wAPI.Code)
	}
}

func TestFaceHofManageAndTokenHandlers(t *testing.T) {
	// Drain any preexisting jobs from queue
	for len(JobQueue) > 0 {
		<-JobQueue
	}

	// Test token endpoint
	reqToken := httptest.NewRequest("GET", "/api/face/hof/token?username=alice&provider=github", nil)
	wToken := httptest.NewRecorder()
	handleFaceHofToken(wToken, reqToken)

	if wToken.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for token handler, got %d", wToken.Code)
	}
	var tokenResp map[string]string
	if err := json.NewDecoder(wToken.Body).Decode(&tokenResp); err != nil || tokenResp["status"] != "ok" {
		t.Errorf("Invalid token response: %v", tokenResp)
	}

	// Test manage add
	addBody := strings.NewReader(`{"command":"add","user":"alice","owner":"sourcerer-io","repo":"awesome-libraries"}`)
	reqAdd := httptest.NewRequest("POST", "/api/face/hof/manage", addBody)
	wAdd := httptest.NewRecorder()
	handleFaceHofManage(wAdd, reqAdd)

	if wAdd.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for manage add, got %d", wAdd.Code)
	}

	// Drain enqueued job so it doesn't leak into subsequent tests
	for len(JobQueue) > 0 {
		<-JobQueue
	}

	// Test manage list
	listBody := strings.NewReader(`{"command":"list","user":"alice"}`)
	reqList := httptest.NewRequest("POST", "/api/face/hof/manage", listBody)
	wList := httptest.NewRecorder()
	handleFaceHofManage(wList, reqList)

	if wList.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for manage list, got %d", wList.Code)
	}

	// Test manage remove
	removeBody := strings.NewReader(`{"command":"remove","user":"alice","owner":"sourcerer-io","repo":"awesome-libraries"}`)
	reqRemove := httptest.NewRequest("POST", "/api/face/hof/manage", removeBody)
	wRemove := httptest.NewRecorder()
	handleFaceHofManage(wRemove, reqRemove)

	if wRemove.Code != http.StatusOK {
		t.Errorf("Expected 200 OK for manage remove, got %d", wRemove.Code)
	}
}

func TestTokenLimitingFunctions(t *testing.T) {
	tokens := make([]string, 50)
	for i := 0; i < 50; i++ {
		tokens[i] = "token"
	}

	limitFn := templateFuncs["limitTokens"].(func([]string, int) []string)
	hasMoreFn := templateFuncs["hasMoreTokens"].(func([]string, int) bool)
	moreCountFn := templateFuncs["moreTokenCount"].(func([]string, int) int)

	limited := limitFn(tokens, 25)
	if len(limited) != 25 {
		t.Errorf("Expected 25 tokens, got %d", len(limited))
	}

	if !hasMoreFn(tokens, 25) {
		t.Errorf("Expected hasMoreTokens to be true for 50 tokens with limit 25")
	}

	if moreCountFn(tokens, 25) != 25 {
		t.Errorf("Expected 25 more tokens, got %d", moreCountFn(tokens, 25))
	}

	shortTokens := []string{"a", "b", "c"}
	if hasMoreFn(shortTokens, 25) {
		t.Errorf("Expected hasMoreTokens to be false for 3 tokens")
	}
	if moreCountFn(shortTokens, 25) != 0 {
		t.Errorf("Expected 0 more tokens for short slice")
	}
}

