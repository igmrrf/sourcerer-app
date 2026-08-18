package main

import (
	"html/template"
	"strings"
	"testing"
	"time"
)

func unixAt(year int, month time.Month, day, hour int) int64 {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC).Unix()
}

func TestBuildRepoCardsReadsAHabitPerRepository(t *testing.T) {
	repos := []RepoInfo{
		{Rehash: "acme/night", CommitCount: 30, LinesAdded: 900, LinesDeleted: 300},
		{Rehash: "acme/day", CommitCount: 10, LinesAdded: 100, LinesDeleted: 100},
	}
	rhythms := map[string]repoRhythm{
		// Mostly after hours, and a quarter of it on weekends.
		"acme/night": {
			Commits: 30, Morning: 3, Afternoon: 6, Night: 21, Weekend: 9, Lines: 1200,
			First: unixAt(2024, time.March, 4, 23), Last: unixAt(2026, time.August, 1, 2),
		},
		// Squarely a working-hours project inside one month.
		"acme/day": {
			Commits: 10, Morning: 2, Afternoon: 8, Night: 0, Weekend: 0, Lines: 200,
			First: unixAt(2026, time.July, 6, 14), Last: unixAt(2026, time.July, 28, 11),
		},
	}

	dir := buildRepoCards(repos, rhythms)

	if !dir.HasData || dir.Count != 2 || len(dir.Cards) != 2 {
		t.Fatalf("want 2 cards with data, got HasData=%v Count=%d cards=%d", dir.HasData, dir.Count, len(dir.Cards))
	}
	if dir.TotalCommits != 40 || dir.TotalAdded != 1000 || dir.TotalDeleted != 400 {
		t.Errorf("totals wrong: %d commits, +%d, -%d", dir.TotalCommits, dir.TotalAdded, dir.TotalDeleted)
	}
	// Cards keep the order they arrive in, which is commit count descending.
	if dir.Cards[0].Rehash != "acme/night" {
		t.Errorf("want the busiest repository first, got %q", dir.Cards[0].Rehash)
	}

	night := dir.Cards[0]
	if night.CommitPct != 75 {
		t.Errorf("want 75%% commit share, got %d", night.CommitPct)
	}
	if night.WorkStyle != "Night Owl" {
		t.Errorf("want Night Owl, got %q", night.WorkStyle)
	}
	if !strings.Contains(night.WorkDesc, "70%") {
		t.Errorf("want the night share in the description, got %q", night.WorkDesc)
	}
	if night.Schedule != "Weekend Warrior" {
		t.Errorf("30%% weekend commits should read as a Weekend Warrior, got %q", night.Schedule)
	}
	if night.AvgCommit != "40" { // 1200 lines of churn over 30 commits
		t.Errorf("want an average commit of 40 lines, got %q", night.AvgCommit)
	}
	if night.Span != "Mar 2024 – Aug 2026" {
		t.Errorf("want the full commit window, got %q", night.Span)
	}

	day := dir.Cards[1]
	if day.WorkStyle != "Daytime Builder" {
		t.Errorf("want Daytime Builder, got %q", day.WorkStyle)
	}
	if day.Schedule != "Weekday Master" {
		t.Errorf("want Weekday Master with no weekend commits, got %q", day.Schedule)
	}
	// A repository worked in a single month reads as one label, not "Jul 2026 – Jul 2026".
	if day.Span != "Jul 2026" {
		t.Errorf("want a single month, got %q", day.Span)
	}
}

func TestBuildRepoCardsSurvivesMissingRhythms(t *testing.T) {
	// A repository whose commits carry no usable timestamps still has to render:
	// the name and the hall of fame link are the point of the card.
	dir := buildRepoCards([]RepoInfo{{Rehash: "acme/quiet", CommitCount: 3}}, nil)

	if len(dir.Cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(dir.Cards))
	}
	card := dir.Cards[0]
	if card.HasHabit {
		t.Error("no rhythm should mean no habit")
	}
	if card.WorkStyle != "" || card.Schedule != "" || card.AvgCommit != "" || card.Span != "" {
		t.Errorf("habit fields should stay empty, got %+v", card)
	}
	if card.CommitPct != 100 {
		t.Errorf("a lone repository is 100%% of the author's commits, got %d", card.CommitPct)
	}
}

func TestBuildRepoCardsWithNoRepositories(t *testing.T) {
	dir := buildRepoCards(nil, nil)
	if dir.HasData || dir.Count != 0 || len(dir.Cards) != 0 {
		t.Errorf("want an empty directory, got %+v", dir)
	}
}

func TestMonthSpanHandlesUnsetTimestamps(t *testing.T) {
	if got := monthSpan(0, 0); got != "" {
		t.Errorf("want an empty span for unset timestamps, got %q", got)
	}
	if got := monthSpan(unixAt(2026, time.January, 5, 9), 0); got != "" {
		t.Errorf("want an empty span when either end is unset, got %q", got)
	}
}

func TestRepoCardsTemplateShowsNameHallOfFameAndHabit(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("Failed to parse embedded templates: %v", err)
	}

	dir := buildRepoCards(
		[]RepoInfo{{Rehash: "acme/night", CommitCount: 30, LinesAdded: 900, LinesDeleted: 300}},
		map[string]repoRhythm{"acme/night": {
			Commits: 30, Morning: 3, Afternoon: 6, Night: 21, Weekend: 9, Lines: 1200,
			First: unixAt(2024, time.March, 4, 23), Last: unixAt(2026, time.August, 1, 2),
		}},
	)

	var out strings.Builder
	if err := tmpl.ExecuteTemplate(&out, "repo_cards.html", dir); err != nil {
		t.Fatalf("Failed to render repo_cards.html: %v", err)
	}
	html := out.String()

	for _, want := range []string{
		"acme/night",           // the repository name
		`href="/r/acme/night"`, // the hall of fame button
		"Hall of fame",
		"Night Owl", // the habit read from this repository alone
		"Weekend Warrior",
		"40 lines",
		"Mar 2024 – Aug 2026",
		"30 commits",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("repo_cards.html output missing %q", want)
		}
	}
}
