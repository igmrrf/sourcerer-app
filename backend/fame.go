package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

// FameEntry represents an individual contributor in the Hall of Fame widget.
type FameEntry struct {
	Index       int    `json:"index"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Commits     int    `json:"commits"`
	Badge       string `json:"badge"` // "new", "trending", "top", "legend", "empty"
	BadgeColor  string `json:"badge_color"`
	IsSourcerer bool   `json:"is_sourcerer"`
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileURL  string `json:"profile_url"`
}

const (
	MaxFameNew      = 3
	MaxFameTrending = 4
	MaxFameTotal    = 7
	FameSlotCount   = 8 // 7 contributors + 1 legend (indices 0..7)

	BadgeColorNew      = "#4CB04F"
	BadgeColorTrending = "#2B95CF"
	BadgeColorTop      = "#F28F56"
)

// getRepoFameEntries computes the exact 8 slots (0..7) according to the Hall of Fame algorithm.
func getRepoFameEntries(repoRehash string, baseURL string) ([]FameEntry, HallOfFameData) {
	data := getHallOfFameData(repoRehash)
	data.PublicBaseURL = baseURL

	var everyone []FameEntry
	excluded := make(map[string]bool)

	// 1. Assign New Contributors (up to 3)
	for i, c := range data.NewContributors {
		if i >= MaxFameNew {
			break
		}
		profileURL := baseURL + "/r/" + repoRehash
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		everyone = append(everyone, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "new",
			BadgeColor:  BadgeColorNew,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
		excluded[c.Email] = true
	}

	// 2. Assign Trending Contributors (up to 4)
	var trendingCandidates []FameEntry
	for _, c := range data.TrendingContributors {
		if excluded[c.Email] {
			continue
		}
		profileURL := baseURL + "/r/" + repoRehash
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		trendingCandidates = append(trendingCandidates, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "trending",
			BadgeColor:  BadgeColorTrending,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
	}

	// If more trending than spots, drop top all-time contributors from trending
	if len(trendingCandidates) > MaxFameTrending {
		topEmails := make(map[string]bool)
		for i, c := range data.TopContributors {
			if i < 3 {
				topEmails[c.Email] = true
			}
		}
		var filteredTrending []FameEntry
		for _, tc := range trendingCandidates {
			if !topEmails[tc.Email] {
				filteredTrending = append(filteredTrending, tc)
			}
		}
		if len(filteredTrending) >= MaxFameTrending {
			trendingCandidates = filteredTrending
		}
	}

	for i, tc := range trendingCandidates {
		if i >= MaxFameTrending || len(everyone) >= MaxFameTotal {
			break
		}
		everyone = append(everyone, tc)
		excluded[tc.Email] = true
	}

	// 3. Assign Top Contributors (fill remaining spots up to 7)
	for _, c := range data.TopContributors {
		if len(everyone) >= MaxFameTotal {
			break
		}
		if excluded[c.Email] {
			continue
		}
		profileURL := baseURL + "/r/" + repoRehash
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		everyone = append(everyone, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "top",
			BadgeColor:  BadgeColorTop,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
		excluded[c.Email] = true
	}

	// Build full 8-slot array (0..7)
	slots := make([]FameEntry, FameSlotCount)
	for i := 0; i < FameSlotCount; i++ {
		if i < len(everyone) {
			slots[i] = everyone[i]
			slots[i].Index = i
		} else if i == len(everyone) {
			// Legend slot
			slots[i] = FameEntry{
				Index:      i,
				Name:       "Hall of Fame",
				Badge:      "legend",
				ProfileURL: baseURL + "/r/" + repoRehash,
			}
		} else {
			// Empty slot
			slots[i] = FameEntry{
				Index:      i,
				Name:       "Sourcerer",
				Badge:      "empty",
				ProfileURL: baseURL + "/r/" + repoRehash,
			}
		}
	}

	return slots, data
}

// generateIndividualFameSVG creates the standalone SVG for a contributor slot (height: 90px).
func generateIndividualFameSVG(entry FameEntry) []byte {
	switch entry.Badge {
	case "legend":
		return generateFameLegendSVG()
	case "empty":
		return generateFameEmptySVG()
	}

	initial := "?"
	if len(entry.Name) > 0 {
		initial = strings.ToUpper(string([]rune(entry.Name)[0]))
	}

	// Measure badge width roughly based on label & commit length
	label := entry.Badge
	valStr := strconv.Itoa(entry.Commits)
	labelWidth := len(label)*18 + 24
	valWidth := len(valStr)*18 + 24
	totalBadgeW := labelWidth + valWidth
	totalBadgeH := 44

	avatarD := 160.0
	viewW := math.Max(avatarD+20.0, float64(totalBadgeW)+20.0)
	viewH := avatarD + float64(totalBadgeH) + 20.0

	avatarCX := viewW / 2.0
	avatarCY := avatarD/2.0 + 10.0

	badgeX := (viewW - float64(totalBadgeW)) / 2.0
	badgeY := avatarD + 8.0

	haloSVG := ""
	if entry.IsSourcerer {
		haloSVG = fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="#a78bfa" stroke-width="3" stroke-dasharray="6 4" opacity="0.9"/>`,
			avatarCX, avatarCY, (avatarD/2.0)+6.0)
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" version="1.1" width="%.1f" height="90" viewBox="0 0 %.1f %.1f">
	<defs>
		<clipPath id="avatar-clip-%d">
			<circle cx="%.1f" cy="%.1f" r="%.1f"/>
		</clipPath>
		<clipPath id="badge-clip-%d">
			<rect width="%d" height="%d" rx="7"/>
		</clipPath>
	</defs>

	<!-- Halo for Sourcerer Members -->
	%s

	<!-- Avatar Circle -->
	<circle cx="%.1f" cy="%.1f" r="%.1f" fill="#1c1538" stroke="#332463" stroke-width="2"/>
	<text x="%.1f" y="%.1f" fill="#f8fafc" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="64" font-weight="700" text-anchor="middle" dominant-baseline="central">%s</text>

	<!-- Badge -->
	<g transform="translate(%.1f, %.1f)">
		<g clip-path="url(#badge-clip-%d)">
			<rect width="%d" height="%d" fill="#475569"/>
			<rect x="%d" width="%d" height="%d" fill="%s"/>
		</g>
		<g text-anchor="middle" font-size="22" font-weight="700" fill="#ffffff" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif">
			<text x="%.1f" y="30">%s</text>
			<text x="%.1f" y="30">%s</text>
		</g>
	</g>
</svg>`,
		viewW*(90.0/viewH), viewW, viewH,
		entry.Index,
		avatarCX, avatarCY, avatarD/2.0,
		entry.Index,
		totalBadgeW, totalBadgeH,
		haloSVG,
		avatarCX, avatarCY, avatarD/2.0,
		avatarCX, avatarCY, template.HTMLEscapeString(initial),
		badgeX, badgeY,
		entry.Index,
		totalBadgeW, totalBadgeH,
		labelWidth, valWidth, totalBadgeH, entry.BadgeColor,
		float64(labelWidth)/2.0, template.HTMLEscapeString(label),
		float64(labelWidth)+float64(valWidth)/2.0, template.HTMLEscapeString(valStr),
	)

	return []byte(svg)
}

func generateFameLegendSVG() []byte {
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" version="1.1" width="130" height="90" viewBox="0 0 260 180">
	<!-- Weekly Dots -->
	<circle cx="36" cy="45" r="16" fill="%s"/>
	<circle cx="78" cy="45" r="16" fill="%s"/>
	<text x="110" y="53" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="26" font-weight="600" fill="#cbd5e1">weekly</text>

	<!-- All Time Dot -->
	<circle cx="78" cy="115" r="16" fill="%s"/>
	<text x="110" y="123" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="26" font-weight="600" fill="#cbd5e1">all time</text>
</svg>`, BadgeColorNew, BadgeColorTrending, BadgeColorTop)
	return []byte(svg)
}

func generateFameEmptySVG() []byte {
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" version="1.1" width="1" height="90" viewBox="0 0 1 180"></svg>`)
}

// resolveRepoParam parses repo parameter from URL which might be a rehash, "owner/repo", or "user/owner/repo"
func resolveRepoParam(r *http.Request) string {
	repo := chi.URLParam(r, "repo")
	owner := chi.URLParam(r, "owner")
	user := chi.URLParam(r, "user")

	if user != "" && owner != "" && repo != "" {
		// e.g. /fame/{user}/{owner}/{repo}
		var rehash string
		err := db.QueryRow("SELECT rehash FROM repos WHERE repo_name = $1 OR repo_name = $2 LIMIT 1", owner+"/"+repo, user+"/"+owner+"/"+repo).Scan(&rehash)
		if err == nil && rehash != "" {
			return rehash
		}
		return owner + "/" + repo
	}

	if owner != "" && repo != "" {
		var rehash string
		err := db.QueryRow("SELECT rehash FROM repos WHERE repo_name = $1 LIMIT 1", owner+"/"+repo).Scan(&rehash)
		if err == nil && rehash != "" {
			return rehash
		}
		return owner + "/" + repo
	}

	return repo
}

func handleFameImage(w http.ResponseWriter, r *http.Request) {
	repoParam := resolveRepoParam(r)
	numStr := chi.URLParam(r, "num")
	numStr = strings.TrimSuffix(numStr, ".svg")
	num, err := strconv.Atoi(numStr)
	if err != nil || num < 0 || num >= FameSlotCount {
		http.Error(w, "Invalid image slot", http.StatusBadRequest)
		return
	}

	baseURL := getPublicBaseURL(r)
	slots, _ := getRepoFameEntries(repoParam, baseURL)

	var entry FameEntry
	if num < len(slots) {
		entry = slots[num]
	} else {
		entry = FameEntry{Index: num, Badge: "empty"}
	}

	svgBytes := generateIndividualFameSVG(entry)
	writeSVG(w, r, fmt.Sprintf("fame-%s-%d", repoParam, num), svgBytes)
}

func handleFameLink(w http.ResponseWriter, r *http.Request) {
	repoParam := resolveRepoParam(r)
	numStr := chi.URLParam(r, "num")
	num, err := strconv.Atoi(numStr)
	baseURL := getPublicBaseURL(r)

	if err != nil || num < 0 || num >= FameSlotCount {
		http.Redirect(w, r, baseURL+"/r/"+repoParam, http.StatusFound)
		return
	}

	slots, _ := getRepoFameEntries(repoParam, baseURL)
	if num < len(slots) && slots[num].ProfileURL != "" {
		http.Redirect(w, r, slots[num].ProfileURL, http.StatusFound)
		return
	}

	http.Redirect(w, r, baseURL+"/r/"+repoParam, http.StatusFound)
}

func handleFaceHofMatch(w http.ResponseWriter, r *http.Request) {
	namesParam := r.URL.Query().Get("names")
	if namesParam == "" {
		namesParam = r.URL.Query().Get("usernames")
	}

	names := strings.Split(namesParam, ",")
	mapping := make(map[string]string)

	if db != nil && len(names) > 0 {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			var profileID string
			err := db.QueryRow(`
				SELECT p.profile_id
				FROM public_profiles p
				LEFT JOIN authors a ON a.email = p.email
				WHERE p.email = $1 OR a.name ILIKE $1 OR p.email ILIKE $1 || '@%'
				LIMIT 1`, name).Scan(&profileID)
			if err == nil && profileID != "" {
				mapping[name] = profileID
			} else {
				mapping[name] = ""
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mapping)
}

// getLibraryFameEntries computes the exact 8 slots (0..7) according to the Hall of Fame algorithm for a library/framework.
func getLibraryFameEntries(techID string, baseURL string) ([]FameEntry, TechnologyMeta, []ContributorStat, []ContributorStat, []ContributorStat) {
	var tm TechnologyMeta
	if db != nil {
		err := db.QueryRow(`
			SELECT id, name, lang, category, COALESCE(icon, ''), COALESCE(description, ''), COALESCE(import_tokens, '{}')
			FROM technologies
			WHERE id = $1 OR name ILIKE $1 OR id LIKE '%.' || $1 OR SPLIT_PART(id, '.', 2) = $1 LIMIT 1`, techID).
			Scan(&tm.ID, &tm.Name, &tm.Lang, &tm.Category, &tm.Icon, &tm.Description, pq.Array(&tm.ImportTokens))
		if err != nil {
			tm = TechnologyMeta{
				ID:       techID,
				Name:     techID,
				Lang:     "framework",
				Category: "General Library",
			}
		}
	} else {
		tm = TechnologyMeta{
			ID:       techID,
			Name:     techID,
			Lang:     "framework",
			Category: "General Library",
		}
	}

	strippedID := strings.TrimPrefix(tm.ID, tm.Lang+".")
	cutoff := time.Now().AddDate(0, 0, -30).Unix() // 30 days recent activity

	var topContributors []ContributorStat
	var trendingContributors []ContributorStat
	var newContributors []ContributorStat

	if db != nil {
		// 1. Top all-time contributors for this library
		topRows, err := db.Query(`
			SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
			       COUNT(DISTINCT c.rehash) as commit_count,
			       COALESCE(SUM(s.num_lines_added), 0) as added,
			       COALESCE(SUM(s.num_lines_deleted), 0) as deleted,
			       bool_or(u.email IS NOT NULL) as is_sourcerer,
			       COALESCE(MAX(p.profile_id), '') as profile_id
			FROM commit_stats s
			JOIN commits c ON c.rehash = s.commit_rehash
			LEFT JOIN authors a ON a.email = c.author_email
			LEFT JOIN users u ON u.email = c.author_email
			LEFT JOIN public_profiles p ON p.email = c.author_email
			WHERE (s.tech = $1 OR s.tech = $2 OR s.tech = $3) AND c.author_email IS NOT NULL AND c.author_email != ''
			GROUP BY c.author_email
			ORDER BY added DESC, commit_count DESC
			LIMIT 10`, tm.ID, tm.Name, strippedID)
		if err == nil {
			defer topRows.Close()
			for topRows.Next() {
				var cs ContributorStat
				if err := topRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer, &cs.ProfileID); err == nil {
					cs.sanitize()
					topContributors = append(topContributors, cs)
				}
			}
		}

		// 2. Trending contributors for this library (last 30 days)
		trendRows, err := db.Query(`
			SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
			       COUNT(DISTINCT c.rehash) as commit_count,
			       COALESCE(SUM(s.num_lines_added), 0) as added,
			       COALESCE(SUM(s.num_lines_deleted), 0) as deleted,
			       bool_or(u.email IS NOT NULL) as is_sourcerer,
			       COALESCE(MAX(p.profile_id), '') as profile_id
			FROM commit_stats s
			JOIN commits c ON c.rehash = s.commit_rehash
			LEFT JOIN authors a ON a.email = c.author_email
			LEFT JOIN users u ON u.email = c.author_email
			LEFT JOIN public_profiles p ON p.email = c.author_email
			WHERE (s.tech = $1 OR s.tech = $2 OR s.tech = $3) AND c.date >= $4 AND c.author_email IS NOT NULL AND c.author_email != ''
			GROUP BY c.author_email
			ORDER BY commit_count DESC, added DESC
			LIMIT 6`, tm.ID, tm.Name, strippedID, cutoff)
		if err == nil {
			defer trendRows.Close()
			for trendRows.Next() {
				var cs ContributorStat
				if err := trendRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.LinesAdded, &cs.LinesDeleted, &cs.IsSourcerer, &cs.ProfileID); err == nil {
					cs.sanitize()
					trendingContributors = append(trendingContributors, cs)
				}
			}
		}

		// 3. New contributors for this library (joined within 30 days)
		newRows, err := db.Query(`
			SELECT c.author_email, COALESCE(MAX(a.name), MAX(c.author_name), c.author_email) as name,
			       COUNT(DISTINCT c.rehash) as commit_count,
			       bool_or(u.email IS NOT NULL) as is_sourcerer,
			       COALESCE(MAX(p.profile_id), '') as profile_id
			FROM commit_stats s
			JOIN commits c ON c.rehash = s.commit_rehash
			LEFT JOIN authors a ON a.email = c.author_email
			LEFT JOIN users u ON u.email = c.author_email
			LEFT JOIN public_profiles p ON p.email = c.author_email
			WHERE (s.tech = $1 OR s.tech = $2 OR s.tech = $3) AND c.author_email IS NOT NULL AND c.author_email != ''
			GROUP BY c.author_email
			HAVING MIN(c.date) >= $4
			ORDER BY MIN(c.date) DESC
			LIMIT 6`, tm.ID, tm.Name, strippedID, cutoff)
		if err == nil {
			defer newRows.Close()
			for newRows.Next() {
				var cs ContributorStat
				if err := newRows.Scan(&cs.Email, &cs.Name, &cs.Commits, &cs.IsSourcerer, &cs.ProfileID); err == nil {
					cs.sanitize()
					newContributors = append(newContributors, cs)
				}
			}
		}
	}

	var everyone []FameEntry
	excluded := make(map[string]bool)

	// 1. Assign New Contributors (up to 3)
	for i, c := range newContributors {
		if i >= MaxFameNew {
			break
		}
		profileURL := baseURL + "/libraries/" + tm.ID
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		everyone = append(everyone, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "new",
			BadgeColor:  BadgeColorNew,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
		excluded[c.Email] = true
	}

	// 2. Assign Trending Contributors (up to 4)
	var trendingCandidates []FameEntry
	for _, c := range trendingContributors {
		if excluded[c.Email] {
			continue
		}
		profileURL := baseURL + "/libraries/" + tm.ID
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		trendingCandidates = append(trendingCandidates, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "trending",
			BadgeColor:  BadgeColorTrending,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
	}

	if len(trendingCandidates) > MaxFameTrending {
		topEmails := make(map[string]bool)
		for i, c := range topContributors {
			if i < 3 {
				topEmails[c.Email] = true
			}
		}
		var filteredTrending []FameEntry
		for _, tc := range trendingCandidates {
			if !topEmails[tc.Email] {
				filteredTrending = append(filteredTrending, tc)
			}
		}
		if len(filteredTrending) >= MaxFameTrending {
			trendingCandidates = filteredTrending
		}
	}

	for i, tc := range trendingCandidates {
		if i >= MaxFameTrending || len(everyone) >= MaxFameTotal {
			break
		}
		everyone = append(everyone, tc)
		excluded[tc.Email] = true
	}

	// 3. Assign Top Contributors (fill remaining up to 7)
	for _, c := range topContributors {
		if len(everyone) >= MaxFameTotal {
			break
		}
		if excluded[c.Email] {
			continue
		}
		profileURL := baseURL + "/libraries/" + tm.ID
		if c.ProfileID != "" {
			profileURL = baseURL + "/p/" + c.ProfileID
		}
		everyone = append(everyone, FameEntry{
			Username:    c.Name,
			Name:        c.Name,
			Email:       c.Email,
			Commits:     c.Commits,
			Badge:       "top",
			BadgeColor:  BadgeColorTop,
			IsSourcerer: c.IsSourcerer,
			ProfileID:   c.ProfileID,
			ProfileURL:  profileURL,
		})
		excluded[c.Email] = true
	}

	// Build 8-slot array (0..7)
	slots := make([]FameEntry, FameSlotCount)
	for i := 0; i < FameSlotCount; i++ {
		if i < len(everyone) {
			slots[i] = everyone[i]
			slots[i].Index = i
		} else if i == len(everyone) {
			slots[i] = FameEntry{
				Index:      i,
				Name:       "Hall of Fame",
				Badge:      "legend",
				ProfileURL: baseURL + "/libraries/" + tm.ID,
			}
		} else {
			slots[i] = FameEntry{
				Index:      i,
				Name:       "Sourcerer",
				Badge:      "empty",
				ProfileURL: baseURL + "/libraries/" + tm.ID,
			}
		}
	}

	return slots, tm, topContributors, trendingContributors, newContributors
}

func handleLibraryFameImage(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	numStr := chi.URLParam(r, "num")
	numStr = strings.TrimSuffix(numStr, ".svg")
	num, err := strconv.Atoi(numStr)
	if err != nil || num < 0 || num >= FameSlotCount {
		http.Error(w, "Invalid image slot", http.StatusBadRequest)
		return
	}

	baseURL := getPublicBaseURL(r)
	slots, _, _, _, _ := getLibraryFameEntries(techID, baseURL)

	var entry FameEntry
	if num < len(slots) {
		entry = slots[num]
	} else {
		entry = FameEntry{Index: num, Badge: "empty"}
	}

	svgBytes := generateIndividualFameSVG(entry)
	writeSVG(w, r, fmt.Sprintf("fame-lib-%s-%d", techID, num), svgBytes)
}

func handleLibraryFameLink(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	numStr := chi.URLParam(r, "num")
	num, err := strconv.Atoi(numStr)
	baseURL := getPublicBaseURL(r)

	if err != nil || num < 0 || num >= FameSlotCount {
		http.Redirect(w, r, baseURL+"/libraries/"+techID, http.StatusFound)
		return
	}

	slots, _, _, _, _ := getLibraryFameEntries(techID, baseURL)
	if num < len(slots) && slots[num].ProfileURL != "" {
		http.Redirect(w, r, slots[num].ProfileURL, http.StatusFound)
		return
	}

	http.Redirect(w, r, baseURL+"/libraries/"+techID, http.StatusFound)
}

func handleLibraryFameSVG(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	techID = strings.TrimSuffix(techID, ".svg")
	baseURL := getPublicBaseURL(r)

	_, tm, top, _, _ := getLibraryFameEntries(techID, baseURL)

	var linesTotal int
	if db != nil {
		_ = db.QueryRow(`
			SELECT COALESCE(SUM(s.num_lines_added), 0)
			FROM commit_stats s
			WHERE s.tech = $1 OR s.tech = $2 OR s.tech = REPLACE($1, $3 || '.', '')`,
			tm.ID, tm.Name, tm.Lang).Scan(&linesTotal)
	}

	var topListSVG strings.Builder
	for i, c := range top {
		if i >= 4 {
			break
		}
		rowY := 90 + (i * 38)
		initial := "?"
		if len(c.Name) > 0 {
			initial = strings.ToUpper(string([]rune(c.Name)[0]))
		}
		displayName := c.Name
		if len([]rune(displayName)) > 16 {
			displayName = string([]rune(displayName)[:14]) + "…"
		}
		topListSVG.WriteString(fmt.Sprintf(`
			<g transform="translate(25, %d)">
				<rect width="360" height="32" rx="4" fill="#0f0c24" stroke="#251b47" stroke-width="1"/>
				<circle cx="16" cy="16" r="10" fill="#1c1538"/>
				<text x="16" y="20" fill="#a78bfa" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10" font-weight="700" text-anchor="middle">%s</text>
				<text x="34" y="20" fill="#f8fafc" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-weight="600">%s</text>
				<text x="345" y="20" fill="#38bdf8" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="11" font-weight="600" text-anchor="end">+%d lines</text>
			</g>`, rowY, template.HTMLEscapeString(initial), template.HTMLEscapeString(displayName), c.LinesAdded))
	}
	if len(top) == 0 {
		topListSVG.WriteString(`<text x="25" y="110" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="12" font-style="italic">No contributors indexed yet</text>`)
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" version="1.1" width="410" height="260" viewBox="0 0 410 260">
	<defs>
		<linearGradient id="bg-lib-grad" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
			<stop offset="0%%" stop-color="#0b081a" />
			<stop offset="100%%" stop-color="#140e2e" />
		</linearGradient>
	</defs>
	<rect width="410" height="260" rx="10" fill="url(#bg-lib-grad)" stroke="#251b47" stroke-width="1.5" />
	
	<!-- Header -->
	<g transform="translate(25, 25)">
		<text x="0" y="18" fill="#f8fafc" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="18" font-weight="700">%s</text>
		<rect x="0" y="28" width="60" height="18" rx="3" fill="#251b47"/>
		<text x="30" y="41" fill="#a78bfa" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10" font-weight="600" text-anchor="middle">%s</text>
		<text x="360" y="18" fill="#94a3b8" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="12" font-weight="500" text-anchor="end">%d lines &middot; %d contributors</text>
	</g>

	<!-- Top Contributors List -->
	%s

	<!-- Footer -->
	<g transform="translate(25, 245)">
		<text x="0" y="0" fill="#64748b" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10">Sourcerer &middot; library hall of fame</text>
		<text x="360" y="0" fill="#a78bfa" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-size="10" font-weight="600" text-anchor="end">%s</text>
	</g>
</svg>`,
		template.HTMLEscapeString(tm.Name),
		template.HTMLEscapeString(tm.Lang),
		linesTotal, len(top),
		topListSVG.String(),
		template.HTMLEscapeString(displayHost(baseURL)),
	)

	writeSVG(w, r, "sourcerer-lib-fame-"+techID, []byte(svg))
}

func handleLibraryFameAPI(w http.ResponseWriter, r *http.Request) {
	techID := chi.URLParam(r, "tech")
	techID = strings.TrimSuffix(techID, ".svg")
	baseURL := getPublicBaseURL(r)

	slots, tm, top, trending, newContribs := getLibraryFameEntries(techID, baseURL)

	data := struct {
		Library      TechnologyMeta    `json:"library"`
		Slots        []FameEntry       `json:"slots"`
		Top          []ContributorStat `json:"top_contributors"`
		Trending     []ContributorStat `json:"trending_contributors"`
		New          []ContributorStat `json:"new_contributors"`
	}{
		Library:  tm,
		Slots:    slots,
		Top:      top,
		Trending: trending,
		New:      newContribs,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode Library Hall of Fame API response", "error", err)
	}
}

type HofManageRequest struct {
	Command string `json:"command"`
	User    string `json:"user,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

type HofRepoItem struct {
	User         string `json:"user"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Status       string `json:"status"`
	LastModified string `json:"last_modified"`
	Message      string `json:"message,omitempty"`
}

func handleFaceHofManage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req HofManageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid JSON: " + err.Error()})
		return
	}

	if req.Command == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Command is required"})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Command {
	case "add":
		if req.Owner == "" || req.Repo == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Owner and repo are required"})
			return
		}
		repoName := req.Owner + "/" + req.Repo
		repoURL := "https://github.com/" + repoName
		rehash := computeCommitRehash(repoName)

		if db != nil {
			_, _ = db.Exec(`
				INSERT INTO repos (rehash, repo_name, repo_url, last_synced_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (rehash) DO UPDATE SET repo_name = EXCLUDED.repo_name, repo_url = EXCLUDED.repo_url`,
				rehash, repoName, repoURL, time.Now().Unix())
		}

		userEmail := req.User
		if userEmail == "" {
			userEmail = getSessionEmail(r)
		}
		EnqueueJob(IngestionJob{
			RepoURL:   repoURL,
			RepoName:  repoName,
			UserEmail: userEmail,
		})

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "Repo added to tracking"})

	case "remove":
		if req.Owner == "" || req.Repo == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Owner and repo are required"})
			return
		}
		repoName := req.Owner + "/" + req.Repo
		if db != nil {
			_, _ = db.Exec("DELETE FROM repos WHERE repo_name = $1 OR repo_url LIKE '%' || $1", repoName)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "message": "Repo removed from tracking"})

	case "list":
		var items []HofRepoItem
		if db != nil {
			rows, err := db.Query("SELECT repo_name, repo_url, COALESCE(last_synced_at, 0) FROM repos WHERE repo_name IS NOT NULL AND repo_name != '' ORDER BY last_synced_at DESC LIMIT 100")
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var rName, rURL string
					var lastSync int64
					if err := rows.Scan(&rName, &rURL, &lastSync); err == nil {
						parts := strings.Split(rName, "/")
						owner := ""
						repo := rName
						if len(parts) >= 2 {
							owner = parts[0]
							repo = parts[1]
						}
						modTime := time.Unix(lastSync, 0).UTC().Format(time.RFC3339)
						if lastSync == 0 {
							modTime = time.Now().UTC().Format(time.RFC3339)
						}
						items = append(items, HofRepoItem{
							User:         req.User,
							Owner:        owner,
							Repo:         repo,
							Status:       "SUCCESS",
							LastModified: modTime,
						})
					}
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": items})

	default:
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Invalid command: " + req.Command})
	}
}

func handleFaceHofToken(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("API_INTERNAL_TOKEN")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"token":  token,
	})
}

