package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// The repositories page is the overview's list panel grown into a surface of
// its own: one card per repository, carrying the same things the public profile
// shows — the name and a hall of fame link — plus the coding habit that
// repository pulled out of the author. The habit is computed per repository
// rather than reused from the profile: the same person commits at midnight on
// one project and at lunchtime on another, and that difference is the point.
//
// Commit timestamps are Unix seconds and every bucket below is computed in UTC,
// matching the charts on the overview.

// repoRhythm is one repository's commit shape for a single author.
type repoRhythm struct {
	Commits   int
	Morning   int // 05:00–11:59
	Afternoon int // 12:00–17:59
	Night     int // everything else
	Weekend   int
	Lines     int
	First     int64
	Last      int64
}

// RepoCard is one repository as the template renders it.
type RepoCard struct {
	Rehash string
	// Name is what the card leads with: the owner/repository name when
	// ingestion recorded one, and the rehash when it did not.
	Name         string
	CommitCount  int
	LinesAdded   int
	LinesDeleted int
	// CommitPct is this repository's share of the author's commits.
	CommitPct int

	// Habit is the per-repository read of when and how the author works here.
	// Empty strings mean there was not enough history to say anything.
	HasHabit     bool
	WorkStyle    string
	WorkIcon     string
	WorkDesc     string
	Schedule     string
	ScheduleIcon string
	ScheduleDesc string
	AvgCommit    string
	Span         string
}

// RepoDirectory is everything repo_cards.html needs.
type RepoDirectory struct {
	HasData      bool
	Count        int
	Cards        []RepoCard
	TotalCommits int
	TotalAdded   int
	TotalDeleted int
}

// queryRepoRhythms groups an author's commits per repository. Postgres does the
// bucketing so a long history never has to come back row by row.
func queryRepoRhythms(email string) map[string]repoRhythm {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT repo_rehash,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE hour BETWEEN 5 AND 11),
		       COUNT(*) FILTER (WHERE hour BETWEEN 12 AND 17),
		       COUNT(*) FILTER (WHERE hour < 5 OR hour > 17),
		       COUNT(*) FILTER (WHERE dow >= 6),
		       COALESCE(SUM(churn), 0),
		       MIN(ts), MAX(ts)
		FROM (
			SELECT repo_rehash,
			       EXTRACT(HOUR FROM to_timestamp(date) AT TIME ZONE 'UTC')::int AS hour,
			       EXTRACT(ISODOW FROM to_timestamp(date) AT TIME ZONE 'UTC')::int AS dow,
			       num_lines_added + num_lines_deleted AS churn,
			       date AS ts
			FROM commits
			WHERE author_email = $1
		) c
		GROUP BY repo_rehash`, email)
	if err != nil {
		slog.Error("Error querying repository rhythms", "email", email, "error", err)
		return nil
	}
	defer rows.Close()

	out := map[string]repoRhythm{}
	for rows.Next() {
		var repo string
		var r repoRhythm
		if err := rows.Scan(&repo, &r.Commits, &r.Morning, &r.Afternoon, &r.Night,
			&r.Weekend, &r.Lines, &r.First, &r.Last); err != nil {
			slog.Error("Error scanning repository rhythm row", "error", err)
			continue
		}
		out[repo] = r
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating repository rhythms", "email", email, "error", err)
	}
	return out
}

// monthSpan describes the window a repository was worked in. A single month
// reads as one label rather than as a range with the same value on both sides.
func monthSpan(first, last int64) string {
	if first <= 0 || last <= 0 {
		return ""
	}
	from := time.Unix(first, 0).UTC().Format("Jan 2006")
	to := time.Unix(last, 0).UTC().Format("Jan 2006")
	if from == to {
		return from
	}
	return from + " – " + to
}

// buildRepoCards turns the repository totals and their rhythms into cards. It
// takes both as arguments rather than querying, so the shape of a card is
// testable without a database.
func buildRepoCards(repos []RepoInfo, rhythms map[string]repoRhythm) RepoDirectory {
	dir := RepoDirectory{Count: len(repos)}
	if len(repos) == 0 {
		return dir
	}
	dir.HasData = true

	for _, repo := range repos {
		dir.TotalCommits += repo.CommitCount
		dir.TotalAdded += repo.LinesAdded
		dir.TotalDeleted += repo.LinesDeleted
	}

	for _, repo := range repos {
		card := RepoCard{
			Rehash:       repo.Rehash,
			Name:         repo.Name,
			CommitCount:  repo.CommitCount,
			LinesAdded:   repo.LinesAdded,
			LinesDeleted: repo.LinesDeleted,
		}
		if card.Name == "" {
			card.Name = repo.Rehash
		}
		if dir.TotalCommits > 0 {
			card.CommitPct = int(float64(repo.CommitCount)*100/float64(dir.TotalCommits) + 0.5)
		}
		applyRepoHabit(&card, rhythms[repo.Rehash])
		dir.Cards = append(dir.Cards, card)
	}
	return dir
}

// applyRepoHabit writes the habit lines for one repository. The vocabulary
// deliberately matches the profile's coding habits panel, so a reader who has
// seen "Night Owl" there recognises it here without relearning anything.
func applyRepoHabit(card *RepoCard, r repoRhythm) {
	if r.Commits == 0 {
		return
	}
	card.HasHabit = true

	total := float64(r.Morning + r.Afternoon + r.Night)
	if total > 0 {
		switch {
		case r.Night >= r.Morning && r.Night >= r.Afternoon:
			card.WorkStyle, card.WorkIcon = "Night Owl", "🌙"
			card.WorkDesc = fmt.Sprintf("%d%% of commits here land late", pct(r.Night, total))
		case r.Morning >= r.Afternoon:
			card.WorkStyle, card.WorkIcon = "Early Bird", "🌅"
			card.WorkDesc = fmt.Sprintf("%d%% of commits here land before noon", pct(r.Morning, total))
		default:
			card.WorkStyle, card.WorkIcon = "Daytime Builder", "⚡"
			card.WorkDesc = fmt.Sprintf("%d%% of commits here land midday", pct(r.Afternoon, total))
		}
	}

	if r.Weekend*4 >= r.Commits { // 25% or more, same threshold as the profile
		card.Schedule, card.ScheduleIcon = "Weekend Warrior", "⚔️"
		card.ScheduleDesc = fmt.Sprintf("%d%% of commits on weekends", pct(r.Weekend, float64(r.Commits)))
	} else {
		card.Schedule, card.ScheduleIcon = "Weekday Master", "💼"
		card.ScheduleDesc = fmt.Sprintf("%d%% of commits Mon–Fri", pct(r.Commits-r.Weekend, float64(r.Commits)))
	}

	card.AvgCommit = fmt.Sprintf("%d", int(float64(r.Lines)/float64(r.Commits)+0.5))
	card.Span = monthSpan(r.First, r.Last)
}

func pct(part int, total float64) int {
	if total <= 0 {
		return 0
	}
	return int(float64(part)*100/total + 0.5)
}

func buildRepoDirectory(email string) RepoDirectory {
	return buildRepoCards(getReposForEmail(email), queryRepoRhythms(email))
}

func handleDashboardRepoCards(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	renderTemplate(w, "repo_cards.html", buildRepoDirectory(email))
}

// handleRepositoriesPage serves the repositories surface itself. It is behind
// the session and marked noindex: the public, crawlable view of a repository is
// its hall of fame page.
func handleRepositoriesPage(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	seo := newSEO(getPublicBaseURL(r), "/repositories",
		"Your repositories | "+siteName,
		"Every repository Sourcerer has read for you: churn, commit share, hall of fame links and the coding habit each project pulls out of you.")
	seo.NoIndex = true
	seo.Nav = navFor(r, "repositories")

	renderTemplate(w, "repositories.html", struct{ SEO SEOMeta }{SEO: seo})
}
