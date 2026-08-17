package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionCookieExpires(t *testing.T) {
	sessionSecret = []byte("test-secret-key-32-bytes-long!!")
	email := "developer@example.com"

	// A cookie whose signed expiry has passed must be rejected even though its
	// MAC is valid, so a stolen session cannot be replayed indefinitely.
	expired := signCookieAt(email, time.Now().Add(-time.Minute))
	if _, ok := verifyCookie(expired); ok {
		t.Fatal("verifyCookie accepted an expired cookie")
	}

	valid := signCookieAt(email, time.Now().Add(time.Minute))
	got, ok := verifyCookie(valid)
	if !ok || got != email {
		t.Fatalf("verifyCookie rejected a live cookie: got %q ok=%v", got, ok)
	}

	// Legacy cookies signed over the bare email carry no expiry and must not
	// be honoured.
	mac := hmac.New(sha256.New, sessionSecret)
	mac.Write([]byte(email))
	legacy := base64.URLEncoding.EncodeToString([]byte(email)) + "." + hex.EncodeToString(mac.Sum(nil))
	if _, ok := verifyCookie(legacy); ok {
		t.Fatal("verifyCookie accepted a legacy cookie with no expiry")
	}
}

func TestApiAuthAcceptsCLIHashedToken(t *testing.T) {
	const token = "secret-token-123"
	t.Setenv("API_INTERNAL_TOKEN", token)

	handler := apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// The Kotlin CLI SHA-256 hashes every password before sending it
	// (PasswordHelper.hashPassword), so this is what real ingestion presents.
	req := httptest.NewRequest("POST", "/api/commits", nil)
	req.SetBasicAuth("cli", hashedInternalToken(token))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200 for CLI hashed token, got %d", rr.Code)
	}

	bad := httptest.NewRequest("POST", "/api/commits", nil)
	bad.SetBasicAuth("cli", hashedInternalToken("other-token"))
	rrBad := httptest.NewRecorder()
	handler.ServeHTTP(rrBad, bad)
	if rrBad.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for hash of a different token, got %d", rrBad.Code)
	}
}

func TestHandleAuthRoundTripsCLICredential(t *testing.T) {
	const token = "secret-token-123"
	t.Setenv("API_INTERNAL_TOKEN", token)

	api := &API{}
	req := httptest.NewRequest("POST", "/api/auth", nil)
	req.SetBasicAuth("cli", hashedInternalToken(token))
	rr := httptest.NewRecorder()
	api.HandleAuth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200 from /api/auth, got %d", rr.Code)
	}

	var tokenCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "Token" {
			tokenCookie = c
		}
	}
	if tokenCookie == nil {
		t.Fatal("/api/auth did not issue a Token cookie")
	}
	if tokenCookie.Value == token {
		t.Fatal("/api/auth echoed the raw internal token back to the caller")
	}
	if !tokenCookie.HttpOnly || tokenCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("Token cookie missing hardening flags: %+v", tokenCookie)
	}

	// The issued cookie must authenticate subsequent ingestion calls.
	handler := apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	follow := httptest.NewRequest("POST", "/api/commits", nil)
	follow.AddCookie(tokenCookie)
	rrFollow := httptest.NewRecorder()
	handler.ServeHTTP(rrFollow, follow)
	if rrFollow.Code != http.StatusOK {
		t.Fatalf("Token cookie from /api/auth was rejected: %d", rrFollow.Code)
	}
}

func TestApiAuthFailsClosedWithoutToken(t *testing.T) {
	t.Setenv("API_INTERNAL_TOKEN", "")
	t.Setenv("ENV", "production")

	handler := apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("POST", "/api/commits", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 when API_INTERNAL_TOKEN is unset in production, got %d", rr.Code)
	}
}

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "al***@example.com",
		"ab@example.com":    "***@example.com",
		"a@example.com":     "***@example.com",
		"nodomain":          "no***",
		"":                  "***",
	}
	for in, want := range cases {
		if got := maskEmail(in); got != want {
			t.Fatalf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContributorStatSanitize(t *testing.T) {
	// A commit whose author name is just the email must not leak it via Name.
	cs := ContributorStat{Email: "alice@example.com", Name: "alice@example.com"}
	cs.sanitize()
	if strings.Contains(cs.Email, "alice@") || strings.Contains(cs.Name, "alice@") {
		t.Fatalf("sanitize leaked the raw address: %+v", cs)
	}

	named := ContributorStat{Email: "bob@example.com", Name: "Bob Architect"}
	named.sanitize()
	if named.Name != "Bob Architect" {
		t.Fatalf("sanitize should preserve a real display name, got %q", named.Name)
	}
	if named.Email != "bo***@example.com" {
		t.Fatalf("sanitize should mask the address, got %q", named.Email)
	}
}

func TestContributorProfileURL(t *testing.T) {
	linked := ContributorStat{ProfileID: "abc123"}
	if got := linked.ProfileURL(); got != "/p/abc123" {
		t.Fatalf("ProfileURL() = %q, want /p/abc123", got)
	}
	// Contributors who never signed in have no published profile to link to.
	if got := (ContributorStat{}).ProfileURL(); got != "#" {
		t.Fatalf("ProfileURL() = %q, want inert anchor", got)
	}
}

func TestHallOfFameSVGNeverRendersRawEmail(t *testing.T) {
	stat := ContributorStat{Name: "alice@example.com", Email: "alice@example.com", Commits: 10}
	stat.sanitize()
	svg := string(generateHallOfFameSVG(HallOfFameData{
		RepoRehash:      "example/repo",
		TopContributors: []ContributorStat{stat},
	}))
	if strings.Contains(svg, "alice@example.com") {
		t.Fatal("Hall of Fame SVG rendered a raw email address")
	}
}

func TestResolveJarPathRejectsEmptyJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "sourcerer-app.jar")
	if err := os.WriteFile(jar, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOURCERER_JAR_PATH", jar)

	// A zero-byte jar satisfies a naive existence check but makes every
	// `java -jar` call fail at runtime, so it must never be selected. It may
	// legitimately fall through to a real jar built into the workspace, so
	// assert on what was chosen rather than on failure.
	if got, err := resolveJarPath(); err == nil && got == jar {
		t.Fatal("resolveJarPath selected a zero-byte jar")
	}

	// A directory is what Docker leaves behind when a bind-mount source is
	// missing; it must be rejected the same way.
	asDir := filepath.Join(dir, "dir-jar")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCERER_JAR_PATH", asDir)
	if got, err := resolveJarPath(); err == nil && got == asDir {
		t.Fatal("resolveJarPath selected a directory")
	}

	if err := os.WriteFile(jar, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCERER_JAR_PATH", jar)
	got, err := resolveJarPath()
	if err != nil || got != jar {
		t.Fatalf("resolveJarPath() = %q, %v; want %q", got, err, jar)
	}
}

func TestCORSDoesNotExposeSessionRoutes(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Dashboard partials are session-backed and must carry no CORS grant.
	req := httptest.NewRequest("GET", "/dashboard/stats", nil)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Session route advertised CORS origin %q", got)
	}

	// Public read-only JSON is meant to be embeddable.
	pub := httptest.NewRequest("GET", "/api/hall-of-fame/example", nil)
	pub.Header.Set("Origin", "https://embed.example")
	rrPub := httptest.NewRecorder()
	handler.ServeHTTP(rrPub, pub)
	if got := rrPub.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Public API should allow any origin, got %q", got)
	}
	if got := rrPub.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Wildcard CORS must never be credentialed, got %q", got)
	}
}

func TestSecurityHeadersApplied(t *testing.T) {
	handler := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if rr.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
}

func TestGetPublicBaseURL(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://sourcerer.example/")
	if got := getPublicBaseURL(nil); got != "https://sourcerer.example" {
		t.Fatalf("PUBLIC_BASE_URL not honoured: %q", got)
	}

	t.Setenv("PUBLIC_BASE_URL", "")
	req := httptest.NewRequest("GET", "/r/example", nil)
	req.Host = "sourcerer.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := getPublicBaseURL(req); got != "https://sourcerer.example" {
		t.Fatalf("Forwarded scheme not honoured: %q", got)
	}
}

// TestTemplatesRender guards the public surfaces that previously linked to
// /p/<email> and /badge/<email>.svg — routes keyed on profile_id, which
// therefore 404'd. Executing the templates against the real structs also fails
// on any field that no longer exists.
func TestTemplatesRender(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("Failed to parse embedded templates: %v", err)
	}

	contributor := ContributorStat{
		Email: "alice@example.com", Name: "Alice Developer",
		ProfileID: "abc123", Commits: 10, IsSourcerer: true,
	}
	contributor.sanitize()

	type userInfo struct {
		Name            string
		Email           string
		PublicProfileID string
	}
	type author struct {
		Email string
		Name  string
	}

	// Every page carries page metadata; the sidebar is driven from it.
	pageSEO := func(path, title string) SEOMeta {
		seo := newSEO("https://sourcerer.example", path, title, "A description.")
		seo.Nav = NavMeta{Active: "overview", SignedIn: true, ProfileID: "abc123"}
		return seo
	}

	cases := []struct {
		name string
		data any
		want []string
	}{
		{
			name: "profile.html",
			data: ProfileData{
				ProfileID: "abc123", Name: "Alice Developer", Email: "al***@example.com",
				PublicBaseURL: "https://sourcerer.example",
				Languages:     []LangStat{{Tech: "Go", Lines: 10, Percentage: 100}},
				SEO:           pageSEO("/p/abc123", "Alice Developer — engineering profile"),
			},
			want: []string{"/badge/abc123.svg", "https://sourcerer.example/p/abc123"},
		},
		{
			name: "landing.html",
			data: struct {
				SEO          SEOMeta
				LibraryCount int
			}{SEO: pageSEO("/", "Sourcerer"), LibraryCount: 412},
			want: []string{"/auth/github/login", "412 libraries and frameworks"},
		},
		{
			name: "repo.html",
			data: HallOfFameData{
				RepoRehash: "example/repo", RepoName: "example/repo",
				PublicBaseURL:   "https://sourcerer.example",
				TopContributors: []ContributorStat{contributor},
				SEO:             pageSEO("/r/example/repo", "example/repo contributors"),
			},
			want: []string{"/p/abc123", "https://sourcerer.example/r/example/repo"},
		},
		{
			name: "index.html",
			data: struct {
				Authors []author
				User    *userInfo
				SEO     SEOMeta
			}{
				Authors: []author{{Email: "al***@example.com", Name: "Alice"}},
				User:    &userInfo{Name: "Alice", Email: "al***@example.com", PublicProfileID: "abc123"},
				SEO:     pageSEO("/", "Overview"),
			},
			want: []string{"/p/abc123", "/badge/abc123.svg"},
		},
		{
			name: "libraries.html",
			data: struct {
				Groups []LibraryCategoryGroup
				SEO    SEOMeta
			}{
				Groups: []LibraryCategoryGroup{{
					Category: "Web frameworks",
					Items:    []TechnologyMeta{{ID: "go.chi", Name: "Chi", Lang: "Go", Description: "HTTP router", ImportTokens: []string{"go-chi/chi"}}},
				}},
				SEO: pageSEO("/libraries", "Library catalog"),
			},
			want: []string{"/libraries/go.chi", "go-chi/chi", "Web frameworks"},
		},
		{
			name: "library_detail.html",
			data: struct {
				Library      *TechnologyMeta
				Contributors []ContributorStat
				SEO          SEOMeta
			}{
				Library:      &TechnologyMeta{ID: "go.chi", Name: "Chi", Lang: "Go", Category: "Web frameworks", Description: "HTTP router", ImportTokens: []string{"go-chi/chi"}},
				Contributors: []ContributorStat{contributor},
				SEO:          pageSEO("/libraries/go.chi", "Chi contributors"),
			},
			want: []string{"/api/libraries/go.chi", "/p/abc123", "Chi"},
		},
		{
			name: "stats.html",
			data: struct {
				TotalCommits int
				LinesAdded   int
				LinesDeleted int
			}{TotalCommits: 42, LinesAdded: 300, LinesDeleted: 100},
			want: []string{"42", "+300", "net <b>200</b>", "75.00%"},
		},
		{
			name: "languages.html",
			data: []LangStat{{Tech: "Go", Lines: 10, Percentage: 100}},
			want: []string{"Go", "100%", "10 lines"},
		},
		{
			name: "repos.html",
			data: []RepoInfo{{Rehash: "example/repo", CommitCount: 7, LinesAdded: 90, LinesDeleted: 10}},
			want: []string{"example/repo", "7 commits", "+90", "/r/example/repo"},
		},
		{
			name: "facts.html",
			data: FactsData{HasData: true, WorkStyle: "Night owl", WorkStyleIcon: "N", WorkStyleDesc: "Commits after hours", AvgCommitSize: "38"},
			want: []string{"Night owl", "Commits after hours", "38 lines"},
		},
		{
			name: "libraries_partial.html",
			data: []TechnologyMeta{{ID: "go.chi", Name: "Chi", Category: "Web frameworks", Lines: 120}},
			want: []string{"/libraries/go.chi", "+120 lines"},
		},
	}

	// Full pages must link the fingerprinted stylesheet; partials are swapped
	// into an existing page and must not.
	fullPages := map[string]bool{
		"profile.html": true, "repo.html": true, "index.html": true,
		"libraries.html": true, "library_detail.html": true, "landing.html": true,
	}

	for _, tc := range cases {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, tc.name, tc.data); err != nil {
			t.Fatalf("%s failed to execute: %v", tc.name, err)
		}
		out := buf.String()
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Fatalf("%s output missing %q", tc.name, want)
			}
		}
		if strings.Contains(out, "localhost:8080") {
			t.Fatalf("%s still hardcodes localhost:8080", tc.name)
		}
		if strings.Contains(out, "alice@example.com") {
			t.Fatalf("%s rendered a raw email address", tc.name)
		}
		wantSheet := "/static/app.css?v=" + assetVersion
		if fullPages[tc.name] && !strings.Contains(out, wantSheet) {
			t.Fatalf("%s does not link %s", tc.name, wantSheet)
		}
		if !fullPages[tc.name] && strings.Contains(out, "<link") {
			t.Fatalf("%s is a partial but emits a <link> tag", tc.name)
		}
	}
}

// The stylesheet ships embedded, so a missing or renamed file must fail the
// build rather than 404 in production.
func TestStaticStylesheetIsServed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/static/app.css?v="+assetVersion, nil)
	rec := httptest.NewRecorder()

	staticHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stylesheet request returned %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "--accent") {
		t.Fatal("stylesheet response does not contain the design tokens")
	}
	if assetVersion == "dev" {
		t.Fatal("asset fingerprint fell back to \"dev\"; the embedded stylesheet was not found")
	}
}
