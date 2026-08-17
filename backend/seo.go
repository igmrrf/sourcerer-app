package main

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// siteName is the brand string used in <title> suffixes and og:site_name.
const siteName = "Sourcerer"

// sitemapEntryLimit bounds each section of the sitemap. A single sitemap file
// may hold 50,000 URLs; staying well under that keeps one file valid without
// needing an index. Truncation is logged rather than silent.
const sitemapEntryLimit = 10000

// SEOMeta is the per-page metadata block rendered by the "seo" template. Every
// page builds one; nothing about it is optional except JSONLD and NoIndex.
type SEOMeta struct {
	Title       string
	Description string
	// Canonical is the absolute URL this page should be indexed under.
	Canonical string
	// Image is an absolute URL to the social share image.
	Image    string
	ImageAlt string
	// Type is the Open Graph object type: "website" or "profile".
	Type string
	// NoIndex keeps a page out of search results while leaving its links
	// crawlable.
	NoIndex bool
	// JSONLD is a rendered schema.org document. It is emitted verbatim inside
	// a script tag, so it must be built by newJSONLD and never from user input
	// that has not been through encoding/json.
	JSONLD template.JS
	// Nav drives the sidebar, which is identical on every page.
	Nav NavMeta
}

// NavMeta is what the shared sidebar needs to know. Every page builds one so
// the navigation never diverges between the dashboard and the public pages.
type NavMeta struct {
	// Active is the highlighted entry: "overview", "libraries", "profile" or
	// "repository".
	Active string
	// SignedIn switches the sidebar footer between log out and sign in.
	SignedIn bool
	// ProfileID is the viewer's own public profile, when they have one. Empty
	// for anonymous visitors, which hides the "Yours" group.
	ProfileID string
}

// navFor builds the sidebar state for a request: whether the visitor is signed
// in, and which profile is theirs. The profile id is read, never created — a
// visitor browsing someone else's page should not mint rows.
func navFor(r *http.Request, active string) NavMeta {
	email := getSessionEmail(r)
	if email == "" {
		return NavMeta{Active: active}
	}

	nav := NavMeta{Active: active, SignedIn: true}
	if db == nil {
		return nav
	}
	var profileID sql.NullString
	if err := db.QueryRow("SELECT profile_id FROM public_profiles WHERE email = $1", email).Scan(&profileID); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("Failed to look up viewer profile id for navigation", "error", err)
		}
		return nav
	}
	nav.ProfileID = profileID.String
	return nav
}

// newSEO fills in the defaults every page shares. Callers override what differs.
func newSEO(baseURL, path, title, description string) SEOMeta {
	return SEOMeta{
		Title:       title,
		Description: description,
		Canonical:   baseURL + path,
		Image:       baseURL + "/static/og.png",
		ImageAlt:    "Sourcerer — engineering profiles from git history",
		Type:        "website",
	}
}

// newJSONLD serializes a schema.org document for embedding in a script tag.
// encoding/json escapes <, > and & as <, > and & by default, so
// no marshalled value can close the surrounding <script> element — which is
// what makes the template.JS conversion safe here.
func newJSONLD(doc any) template.JS {
	encoded, err := json.Marshal(doc)
	if err != nil {
		slog.Error("Failed to encode JSON-LD", "error", err)
		return ""
	}
	return template.JS(encoded)
}

// truncateDescription keeps meta descriptions inside the ~160 characters search
// engines display, cutting on a word boundary.
func truncateDescription(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndex(cut, " "); i > limit/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:") + "…"
}

// robotsTxt keeps crawlers on the public surfaces. The dashboard and auth
// routes are session-gated and the API is data, not content.
func handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	base := getPublicBaseURL(r)

	var b strings.Builder
	b.WriteString("User-agent: *\n")
	b.WriteString("Allow: /\n")
	b.WriteString("Disallow: /dashboard/\n")
	b.WriteString("Disallow: /auth/\n")
	b.WriteString("Disallow: /api/\n")
	b.WriteString("Disallow: /healthz\n")
	b.WriteString("Disallow: /badge/\n")
	b.WriteString("Disallow: /hall-of-fame/\n")
	b.WriteString("\n")
	b.WriteString("Sitemap: " + base + "/sitemap.xml\n")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := w.Write([]byte(b.String())); err != nil {
		slog.Error("Failed to write robots.txt", "error", err)
	}
}

type sitemapURL struct {
	Location   string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
}

type sitemapSet struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// handleSitemapXML lists every page a crawler should know about: the two
// evergreen pages, the library catalog entries, each indexed repository's hall
// of fame, and each public profile.
func handleSitemapXML(w http.ResponseWriter, r *http.Request) {
	base := getPublicBaseURL(r)
	today := time.Now().UTC().Format("2006-01-02")

	set := sitemapSet{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs,
		sitemapURL{Location: base + "/", ChangeFreq: "daily", Priority: "1.0", LastMod: today},
		sitemapURL{Location: base + "/libraries", ChangeFreq: "weekly", Priority: "0.8", LastMod: today},
	)

	for _, group := range getLibrariesCatalog() {
		for _, item := range group.Items {
			set.URLs = append(set.URLs, sitemapURL{
				Location:   base + "/libraries/" + item.ID,
				ChangeFreq: "weekly",
				Priority:   "0.6",
			})
		}
	}

	for _, rehash := range sitemapRepoRehashes() {
		set.URLs = append(set.URLs, sitemapURL{
			Location:   base + "/r/" + rehash,
			ChangeFreq: "daily",
			Priority:   "0.7",
		})
	}

	for _, profileID := range sitemapProfileIDs() {
		set.URLs = append(set.URLs, sitemapURL{
			Location:   base + "/p/" + profileID,
			ChangeFreq: "daily",
			Priority:   "0.7",
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		slog.Error("Failed to write sitemap header", "error", err)
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		slog.Error("Failed to encode sitemap", "error", err)
	}
}

// sitemapRepoRehashes returns the repositories that actually have commits, so
// the sitemap never advertises an empty hall of fame.
func sitemapRepoRehashes() []string {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT r.rehash
		FROM repos r
		WHERE EXISTS (SELECT 1 FROM commits c WHERE c.repo_rehash = r.rehash)
		ORDER BY r.rehash
		LIMIT $1`, sitemapEntryLimit+1)
	if err != nil {
		slog.Error("Error querying repositories for sitemap", "error", err)
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var rehash string
		if err := rows.Scan(&rehash); err == nil && rehash != "" {
			out = append(out, rehash)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating repositories for sitemap", "error", err)
	}
	return capSitemapSection(out, "repositories")
}

// sitemapProfileIDs returns the public profiles that have something to show.
func sitemapProfileIDs() []string {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT p.profile_id
		FROM public_profiles p
		WHERE p.profile_id IS NOT NULL
		  AND p.profile_id <> ''
		  AND EXISTS (SELECT 1 FROM commits c WHERE c.author_email = p.email)
		ORDER BY p.profile_id
		LIMIT $1`, sitemapEntryLimit+1)
	if err != nil {
		slog.Error("Error querying profiles for sitemap", "error", err)
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating profiles for sitemap", "error", err)
	}
	return capSitemapSection(out, "profiles")
}

func capSitemapSection(items []string, section string) []string {
	if len(items) <= sitemapEntryLimit {
		return items
	}
	slog.Warn("Sitemap section truncated",
		"section", section, "limit", sitemapEntryLimit, "dropped", len(items)-sitemapEntryLimit)
	return items[:sitemapEntryLimit]
}

// noIndexPrefixes are the paths that return data or images rather than pages.
// They stay reachable — badges are embedded in READMEs and must load — but they
// should never be a search result themselves.
var noIndexPrefixes = []string{"/api/", "/badge/", "/hall-of-fame/", "/dashboard/", "/auth/", "/static/"}

func robotsHeaderMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range noIndexPrefixes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("X-Robots-Tag", "noindex")
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleLanding renders the public front door for signed-out visitors. It is
// the only page a crawler can reach at "/", so it carries the site-level
// structured data.
func handleLanding(w http.ResponseWriter, r *http.Request) {
	base := getPublicBaseURL(r)

	seo := newSEO(base, "/",
		"Sourcerer — turn your git history into an engineering profile",
		"Sourcerer reads a repository's git history and builds a profile from it: commit volume, language split, coding habits, recognized libraries, a contributor hall of fame and an embeddable README badge.")
	seo.JSONLD = newJSONLD([]any{
		map[string]any{
			"@context":    "https://schema.org",
			"@type":       "WebSite",
			"name":        siteName,
			"url":         base,
			"description": seo.Description,
		},
		map[string]any{
			"@context":            "https://schema.org",
			"@type":               "SoftwareApplication",
			"name":                siteName,
			"url":                 base,
			"applicationCategory": "DeveloperApplication",
			"operatingSystem":     "Web",
			"description":         seo.Description,
			"offers":              map[string]any{"@type": "Offer", "price": "0", "priceCurrency": "USD"},
		},
	})

	libraryCount := 0
	for _, group := range getLibrariesCatalog() {
		libraryCount += len(group.Items)
	}

	data := struct {
		SEO          SEOMeta
		LibraryCount int
	}{SEO: seo, LibraryCount: libraryCount}
	data.SEO.Nav = navFor(r, "overview")

	renderTemplate(w, "landing.html", data)
}

// plural renders "1 commit" / "2 commits" for description copy.
func plural(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}
