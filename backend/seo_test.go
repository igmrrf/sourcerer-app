package main

import (
	"encoding/json"
	"encoding/xml"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// robots.txt is the only thing standing between a crawler and the session-gated
// dashboard, so the disallow list is asserted rather than assumed.
func TestRobotsTxtKeepsCrawlersOffPrivatePaths(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://sourcerer.example")

	rec := httptest.NewRecorder()
	handleRobotsTxt(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("robots.txt returned %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Disallow: /dashboard/",
		"Disallow: /auth/",
		"Disallow: /api/",
		"Sitemap: https://sourcerer.example/sitemap.xml",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("robots.txt missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Disallow: /\n") {
		t.Fatalf("robots.txt disallows the whole site:\n%s", body)
	}
}

// The sitemap must be parseable and must never advertise a private path.
func TestSitemapListsPublicPagesOnly(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://sourcerer.example")

	rec := httptest.NewRecorder()
	handleSitemapXML(rec, httptest.NewRequest(http.MethodGet, "/sitemap.xml", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("sitemap returned %d, want 200", rec.Code)
	}

	var set sitemapSet
	if err := xml.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("sitemap is not valid XML: %v", err)
	}
	if len(set.URLs) < 2 {
		t.Fatalf("sitemap has %d urls, want at least the home and catalog pages", len(set.URLs))
	}

	seen := map[string]bool{}
	for _, u := range set.URLs {
		seen[u.Location] = true
		if !strings.HasPrefix(u.Location, "https://sourcerer.example/") {
			t.Fatalf("sitemap entry is not absolute: %q", u.Location)
		}
		for _, private := range []string{"/dashboard/", "/auth/", "/api/", "/badge/", "/healthz"} {
			if strings.Contains(u.Location, private) {
				t.Fatalf("sitemap advertises a private path: %q", u.Location)
			}
		}
	}
	for _, want := range []string{"https://sourcerer.example/", "https://sourcerer.example/libraries"} {
		if !seen[want] {
			t.Fatalf("sitemap missing %q", want)
		}
	}
}

// Data and image routes stay reachable — README badges must load — but they
// should never turn up as a search result.
func TestNoIndexHeaderOnDataRoutes(t *testing.T) {
	handler := robotsHeaderMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := map[string]string{
		"/api/libraries":            "noindex",
		"/badge/abc123.svg":         "noindex",
		"/hall-of-fame/example.svg": "noindex",
		"/dashboard/stats":          "noindex",
		"/libraries":                "",
		"/p/abc123":                 "",
		"/":                         "",
	}

	for path, want := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Robots-Tag"); got != want {
			t.Fatalf("%s: X-Robots-Tag = %q, want %q", path, got, want)
		}
	}
}

// A page with no canonical, or with two, is worse than one with neither — this
// pins the tags every indexable page has to emit exactly once.
func TestPagesEmitCanonicalAndSocialTags(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("Failed to parse embedded templates: %v", err)
	}

	seo := newSEO("https://sourcerer.example", "/libraries", "Library catalog | Sourcerer", "A catalog of libraries.")
	seo.JSONLD = newJSONLD(map[string]any{"@context": "https://schema.org", "@type": "CollectionPage"})

	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "seo", seo); err != nil {
		t.Fatalf("seo block failed to execute: %v", err)
	}
	out := buf.String()

	for tag, want := range map[string]int{
		`<link rel="canonical"`:               1,
		`property="og:image"`:                 1,
		`property="og:title"`:                 1,
		`name="twitter:card"`:                 1,
		`<meta name="description"`:            1,
		`<script type="application/ld+json">`: 1,
	} {
		if got := strings.Count(out, tag); got != want {
			t.Fatalf("seo block has %d of %q, want %d:\n%s", got, tag, want, out)
		}
	}

	if !strings.Contains(out, `content="index, follow, max-image-preview:large"`) {
		t.Fatalf("indexable page is not marked indexable:\n%s", out)
	}

	seo.NoIndex = true
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "seo", seo); err != nil {
		t.Fatalf("seo block failed to execute: %v", err)
	}
	if !strings.Contains(buf.String(), `content="noindex, follow"`) {
		t.Fatalf("NoIndex page is missing its robots meta:\n%s", buf.String())
	}
}

// JSON-LD is written into a <script> element as template.JS, which skips
// escaping. encoding/json must be the thing that keeps a name from closing the
// element.
func TestJSONLDCannotEscapeItsScriptTag(t *testing.T) {
	out := string(newJSONLD(map[string]any{"name": `</script><img src=x onerror=alert(1)>`}))

	if strings.Contains(out, "</script") {
		t.Fatalf("JSON-LD can close its own script tag: %s", out)
	}
	var round map[string]string
	if err := json.Unmarshal([]byte(out), &round); err != nil {
		t.Fatalf("JSON-LD is not valid JSON: %v", err)
	}
	if round["name"] != `</script><img src=x onerror=alert(1)>` {
		t.Fatalf("JSON-LD did not round-trip: %q", round["name"])
	}
}

// The icons and share card are committed build artifacts; a missing one is a
// silent 404 in someone's browser tab or social preview.
func TestGeneratedAssetsArePresent(t *testing.T) {
	assets := []string{
		"static/favicon.svg",
		"static/favicon.ico",
		"static/apple-touch-icon.png",
		"static/icon-192.png",
		"static/icon-512.png",
		"static/og.png",
		"static/site.webmanifest",
	}

	for _, name := range assets {
		data, err := staticFS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s is not embedded: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}

	var manifest struct {
		Name      string `json:"name"`
		StartURL  string `json:"start_url"`
		ThemeCol  string `json:"theme_color"`
		IconCount []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	raw, err := staticFS.ReadFile("static/site.webmanifest")
	if err != nil {
		t.Fatalf("manifest unreadable: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("site.webmanifest is not valid JSON: %v", err)
	}
	if manifest.Name == "" || manifest.StartURL == "" || len(manifest.IconCount) == 0 {
		t.Fatalf("site.webmanifest is missing required fields: %+v", manifest)
	}

	// The generator script is what makes the icons reproducible.
	if _, err := os.Stat("static/generate_assets.sh"); err != nil {
		t.Fatalf("asset generator script is missing: %v", err)
	}
}

// The badge is a file, not a page: ?download=1 has to come back as an
// attachment, and a rehash containing slashes must not leak into the filename.
func TestSVGDownloadDisposition(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)

	rec := httptest.NewRecorder()
	writeSVG(rec, httptest.NewRequest(http.MethodGet, "/hall-of-fame/a.svg", nil), "sourcerer-hall-of-fame-owner/repo", svg)
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("plain request should render inline, got Content-Disposition %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Fatalf("Content-Type = %q, want image/svg+xml", got)
	}

	rec = httptest.NewRecorder()
	writeSVG(rec, httptest.NewRequest(http.MethodGet, "/hall-of-fame/a.svg?download=1", nil), "sourcerer-hall-of-fame-owner/repo", svg)
	want := `attachment; filename="sourcerer-hall-of-fame-owner-repo.svg"`
	if got := rec.Header().Get("Content-Disposition"); got != want {
		t.Fatalf("Content-Disposition = %q, want %q", got, want)
	}
	if rec.Body.String() != string(svg) {
		t.Fatalf("body was altered: %q", rec.Body.String())
	}
}

func TestSVGDownloadNameIsSafe(t *testing.T) {
	cases := map[string]string{
		"sourcerer-profile-abc123": "sourcerer-profile-abc123.svg",
		`../../etc/passwd`:         "etc-passwd.svg",
		`a"b;c`:                    "a-b-c.svg",
		"":                         "badge.svg",
		"///":                      "badge.svg",
	}
	for in, want := range cases {
		if got := svgDownloadName(in); got != want {
			t.Fatalf("svgDownloadName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateDescription(t *testing.T) {
	long := strings.Repeat("commit ", 60)
	got := truncateDescription(long, 100)

	if len([]rune(got)) > 101 {
		t.Fatalf("description not truncated: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated description should be elided, got %q", got)
	}
	if short := truncateDescription("Short one.", 100); short != "Short one." {
		t.Fatalf("short description was altered: %q", short)
	}
}
