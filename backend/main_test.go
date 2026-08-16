package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

)

func TestCookieSigningAndVerification(t *testing.T) {
	sessionSecret = []byte("test-secret-key-32-bytes-long!!")
	email := "developer@example.com"

	signed := signCookie(email)
	if signed == "" {
		t.Fatal("signCookie returned empty string")
	}

	verifiedEmail, ok := verifyCookie(signed)
	if !ok {
		t.Fatalf("verifyCookie failed to verify signed cookie: %s", signed)
	}

	if verifiedEmail != email {
		t.Fatalf("verifyCookie returned %s, expected %s", verifiedEmail, email)
	}

	// Tampered cookie should fail verification
	tampered := signed + "tampered"
	if _, ok := verifyCookie(tampered); ok {
		t.Fatal("verifyCookie succeeded on tampered cookie")
	}
}

func TestGetSessionEmail(t *testing.T) {
	sessionSecret = []byte("test-secret-key-32-bytes-long!!")
	email := "developer@example.com"
	signed := signCookie(email)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  "session",
		Value: signed,
	})

	extracted := getSessionEmail(req)
	if extracted != email {
		t.Fatalf("getSessionEmail returned %s, expected %s", extracted, email)
	}

	// Missing cookie
	reqNoCookie := httptest.NewRequest("GET", "/", nil)
	if getSessionEmail(reqNoCookie) != "" {
		t.Fatal("getSessionEmail should return empty string for request without cookie")
	}
}

func TestApiAuthMiddleware(t *testing.T) {
	os.Setenv("API_INTERNAL_TOKEN", "secret-token-123")
	defer os.Unsetenv("API_INTERNAL_TOKEN")

	handler := apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authorized"))
	}))

	// 1. Missing auth -> 401
	req1 := httptest.NewRequest("POST", "/api/commits", nil)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 Unauthorized, got %d", rr1.Code)
	}

	// 2. Valid Bearer Token -> 200
	req2 := httptest.NewRequest("POST", "/api/commits", nil)
	req2.Header.Set("Authorization", "Bearer secret-token-123")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", rr2.Code)
	}

	// 3. Valid Cookie Token -> 200
	req3 := httptest.NewRequest("POST", "/api/commits", nil)
	req3.AddCookie(&http.Cookie{
		Name:  "Token",
		Value: "secret-token-123",
	})
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK with cookie, got %d", rr3.Code)
	}

	// 4. Invalid Token -> 401
	req4 := httptest.NewRequest("POST", "/api/commits", nil)
	req4.Header.Set("Authorization", "Bearer wrong-token")
	rr4 := httptest.NewRecorder()
	handler.ServeHTTP(rr4, req4)
	if rr4.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 Unauthorized for wrong token, got %d", rr4.Code)
	}
}

func TestHandleLogout(t *testing.T) {
	req := httptest.NewRequest("GET", "/auth/logout", nil)
	rr := httptest.NewRecorder()

	handleLogout(rr, req)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("Expected 307 Redirect, got %d", rr.Code)
	}

	cookies := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "session" {
			sessionCookie = c
			break
		}
	}

	if sessionCookie == nil {
		t.Fatal("Session cookie was not cleared in logout response")
	}

	if sessionCookie.MaxAge != -1 {
		t.Fatalf("Expected MaxAge -1 for cleared session cookie, got %d", sessionCookie.MaxAge)
	}
}

func TestGenerateBadgeSVG(t *testing.T) {
	langs := []LangStat{
		{Tech: "Go", Lines: 5000, Percentage: 50},
		{Tech: "Kotlin", Lines: 3000, Percentage: 30},
		{Tech: "Python", Lines: 2000, Percentage: 20},
	}

	svg := generateBadgeSVG("John Developer", 120, 10000, 2500, langs)
	if len(svg) == 0 {
		t.Fatal("generateBadgeSVG returned empty byte slice")
	}

	svgStr := string(svg)
	if !strings.Contains(svgStr, "<svg") || !strings.Contains(svgStr, "</svg>") {
		t.Fatal("generateBadgeSVG output does not contain valid svg tags")
	}

	if !strings.Contains(svgStr, "John Developer") {
		t.Fatal("generateBadgeSVG output missing developer name")
	}

	if !strings.Contains(svgStr, "120") || !strings.Contains(svgStr, "+10000") || !strings.Contains(svgStr, "-2500") {
		t.Fatal("generateBadgeSVG output missing commit or line statistics")
	}

	if !strings.Contains(svgStr, "Go") || !strings.Contains(svgStr, "Kotlin") {
		t.Fatal("generateBadgeSVG output missing language stats")
	}
}

func TestGenerateHallOfFameSVG(t *testing.T) {
	data := HallOfFameData{
		RepoRehash:        "sourcerer-io/sourcerer-app",
		RepoName:          "sourcerer-io/sourcerer-app",
		TotalCommits:      1420,
		TotalLinesAdded:   250000,
		TotalLinesDeleted: 45000,
		TotalContributors: 18,
		TopContributors: []ContributorStat{
			{Name: "Alice Developer", Email: "alice@example.com", Commits: 450, LinesAdded: 90000, IsSourcerer: true},
			{Name: "Bob Architect", Email: "bob@example.com", Commits: 280, LinesAdded: 50000, IsSourcerer: false},
		},
		TrendingContributors: []ContributorStat{
			{Name: "Charlie Coder", Email: "charlie@example.com", Commits: 42, IsSourcerer: true},
		},
		NewContributors: []ContributorStat{
			{Name: "Dave Newbie", Email: "dave@example.com", Commits: 5, IsSourcerer: false},
		},
		Languages: []LangStat{
			{Tech: "Go", Lines: 120000, Percentage: 60},
			{Tech: "TypeScript", Lines: 60000, Percentage: 30},
			{Tech: "Kotlin", Lines: 20000, Percentage: 10},
		},
	}

	svg := generateHallOfFameSVG(data)
	if len(svg) == 0 {
		t.Fatal("generateHallOfFameSVG returned empty byte slice")
	}

	svgStr := string(svg)
	if !strings.Contains(svgStr, "<svg") || !strings.Contains(svgStr, "</svg>") {
		t.Fatal("generateHallOfFameSVG output does not contain valid svg tags")
	}

	if !strings.Contains(svgStr, "SOURCERER HALL OF FAME") {
		t.Fatal("generateHallOfFameSVG output missing Hall of Fame title")
	}

	if !strings.Contains(svgStr, "sourcerer-io/sourcerer-app") {
		t.Fatal("generateHallOfFameSVG output missing repo name")
	}

	if !strings.Contains(svgStr, "Alice Developer") || !strings.Contains(svgStr, "Bob Architect") {
		t.Fatal("generateHallOfFameSVG output missing top contributors")
	}

	if !strings.Contains(svgStr, "Charlie Coder") {
		t.Fatal("generateHallOfFameSVG output missing trending contributor")
	}

	if !strings.Contains(svgStr, "Dave Newbie") {
		t.Fatal("generateHallOfFameSVG output missing new contributor")
	}

	if !strings.Contains(svgStr, "Go") || !strings.Contains(svgStr, "TypeScript") {
		t.Fatal("generateHallOfFameSVG output missing language stats")
	}
}


