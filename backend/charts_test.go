package main

import (
	"html/template"
	"math"
	"strings"
	"testing"
	"time"
)

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("bad test date %q: %v", s, err)
	}
	return parsed.UTC()
}

// Weeks have to start where Postgres date_trunc('week') puts them, or the
// bucket the query returns will not match the bucket the axis walks to.
func TestTruncateToUnitStartsWeeksOnMonday(t *testing.T) {
	sunday := mustDay(t, "2026-08-16") // a Sunday
	if got := truncateToUnit(sunday, "week"); got.Weekday() != time.Monday {
		t.Fatalf("week bucket for Sunday is %s (%s), want a Monday", got.Format("2006-01-02"), got.Weekday())
	}
	if got := truncateToUnit(sunday, "week"); !got.Equal(mustDay(t, "2026-08-10")) {
		t.Fatalf("week bucket = %s, want 2026-08-10", got.Format("2006-01-02"))
	}
	if got := truncateToUnit(sunday, "month"); !got.Equal(mustDay(t, "2026-08-01")) {
		t.Fatalf("month bucket = %s, want 2026-08-01", got.Format("2006-01-02"))
	}
	if got := truncateToUnit(sunday, "day"); !got.Equal(sunday) {
		t.Fatalf("day bucket = %s, want the same day", got.Format("2006-01-02"))
	}
}

func TestNiceCeilingRoundsAxisMaxima(t *testing.T) {
	cases := map[float64]float64{
		0: 1, 1: 1, 7: 10, 12: 20, 23: 25, 47: 50, 187: 200, 1400: 2000,
	}
	for in, want := range cases {
		if got := niceCeiling(in); got != want {
			t.Fatalf("niceCeiling(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCompactNumberKeepsAxisLabelsShort(t *testing.T) {
	cases := map[float64]string{0: "0", 250: "250", 1200: "1.2k", 12000: "12k", 2_400_000: "2.4M"}
	for in, want := range cases {
		if got := compactNumber(in); got != want {
			t.Fatalf("compactNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

// The chart walks the calendar rather than the result set, so a month with no
// commits still has to occupy a slot on the axis.
func TestAssembleActivityChartFillsQuietPeriods(t *testing.T) {
	rng := lookupActivityRange("30d")
	now := mustDay(t, "2026-08-10")
	start := mustDay(t, "2026-08-01")

	rows := []activityRow{
		{bucket: mustDay(t, "2026-08-01"), commits: 3, added: 120, deleted: 20},
		{bucket: mustDay(t, "2026-08-10"), commits: 7, added: 40, deleted: 900},
	}

	chart := assembleActivityChart(rng, now, start, rows)

	if !chart.HasData {
		t.Fatal("chart with rows reports no data")
	}
	if len(chart.Buckets) != 10 {
		t.Fatalf("chart has %d buckets, want 10 days from the 1st to the 10th", len(chart.Buckets))
	}
	if chart.Buckets[0].Commits != 3 || chart.Buckets[9].Commits != 7 {
		t.Fatalf("commits landed in the wrong buckets: %d and %d", chart.Buckets[0].Commits, chart.Buckets[9].Commits)
	}
	for i := 1; i < 9; i++ {
		if chart.Buckets[i].Commits != 0 {
			t.Fatalf("bucket %d should be quiet, got %d commits", i, chart.Buckets[i].Commits)
		}
	}
	if chart.TotalCommits != 10 || chart.TotalAdded != 160 || chart.TotalDeleted != 920 {
		t.Fatalf("totals wrong: %d commits, +%d, -%d", chart.TotalCommits, chart.TotalAdded, chart.TotalDeleted)
	}
	if chart.ActiveBuckets != 2 || chart.AllBuckets != 10 {
		t.Fatalf("active buckets = %d/%d, want 2/10", chart.ActiveBuckets, chart.AllBuckets)
	}
	if chart.BusiestCommits != 7 {
		t.Fatalf("busiest bucket = %d commits, want 7", chart.BusiestCommits)
	}

	// Buckets must be laid out left to right inside the plot area.
	for i, b := range chart.Buckets {
		if b.CenterX < chart.PlotLeft-0.01 || b.CenterX > chart.PlotRight+0.01 {
			t.Fatalf("bucket %d sits at x=%v, outside the plot [%v, %v]", i, b.CenterX, chart.PlotLeft, chart.PlotRight)
		}
		if i > 0 && b.CenterX <= chart.Buckets[i-1].CenterX {
			t.Fatalf("bucket %d is not to the right of its predecessor", i)
		}
	}

	// A bigger value has to sit higher, which in SVG means a smaller y.
	if chart.Buckets[9].YCommits >= chart.Buckets[1].YCommits {
		t.Fatal("the busiest bucket is not drawn above a quiet one")
	}
	// Deleted dwarfs added here, so the two share a scale for comparison.
	if chart.Buckets[9].YDeleted >= chart.Buckets[9].YAdded {
		t.Fatal("added and deleted are not on a shared scale")
	}
}

func TestAssembleActivityChartRendersEveryMetric(t *testing.T) {
	rng := lookupActivityRange("30d")
	chart := assembleActivityChart(rng, mustDay(t, "2026-08-05"), mustDay(t, "2026-08-01"), []activityRow{
		{bucket: mustDay(t, "2026-08-02"), commits: 4, added: 300, deleted: 60},
	})

	if len(chart.Metrics) != 4 {
		t.Fatalf("chart has %d metrics, want commits, added, deleted and net", len(chart.Metrics))
	}
	keys := map[string]bool{}
	for _, m := range chart.Metrics {
		keys[m.Key] = true
		if m.Line == "" || !strings.HasPrefix(m.Line, "M") {
			t.Fatalf("metric %q has no line path: %q", m.Key, m.Line)
		}
		if !strings.HasSuffix(m.Area, "Z") {
			t.Fatalf("metric %q area path is not closed: %q", m.Key, m.Area)
		}
		if len(m.Ticks) < 3 || len(m.Ticks) > 8 {
			t.Fatalf("metric %q has %d gridlines, want a handful", m.Key, len(m.Ticks))
		}
		// Labels are counts, so they must be whole numbers, and they must climb
		// the plot rather than wander.
		for i, tick := range m.Ticks {
			if strings.ContainsAny(tick.Label, ".") && !strings.HasSuffix(tick.Label, "k") && !strings.HasSuffix(tick.Label, "M") {
				t.Fatalf("metric %q has a fractional gridline label %q", m.Key, tick.Label)
			}
			if i > 0 && tick.Y >= m.Ticks[i-1].Y {
				t.Fatalf("metric %q gridlines are not ascending", m.Key)
			}
		}
	}
	for _, want := range []string{"commits", "added", "deleted", "net"} {
		if !keys[want] {
			t.Fatalf("metric %q is missing", want)
		}
	}

	// Only one range button is active, and it is the one that was asked for.
	active := 0
	for _, r := range chart.Ranges {
		if r.IsActive {
			active++
			if r.Key != "30d" {
				t.Fatalf("active range is %q, want 30d", r.Key)
			}
		}
	}
	if active != 1 {
		t.Fatalf("%d ranges are active, want exactly 1", active)
	}
}

// Net lines go below zero, which means the axis has to as well.
func TestNetMetricHandlesNegativeBuckets(t *testing.T) {
	rng := lookupActivityRange("30d")
	chart := assembleActivityChart(rng, mustDay(t, "2026-08-03"), mustDay(t, "2026-08-01"), []activityRow{
		{bucket: mustDay(t, "2026-08-01"), commits: 1, added: 10, deleted: 500},
		{bucket: mustDay(t, "2026-08-03"), commits: 1, added: 400, deleted: 10},
	})

	var net ChartMetric
	for _, m := range chart.Metrics {
		if m.Key == "net" {
			net = m
		}
	}
	if net.Zero <= chart.PlotTop || net.Zero >= chart.PlotBottom {
		t.Fatalf("net baseline at %v is not inside the plot [%v, %v]", net.Zero, chart.PlotTop, chart.PlotBottom)
	}
	if chart.Buckets[0].YNet <= net.Zero {
		t.Fatal("a negative net bucket should be drawn below the zero line")
	}
	if chart.Buckets[2].YNet >= net.Zero {
		t.Fatal("a positive net bucket should be drawn above the zero line")
	}
}

func TestPunchLevelBuckets(t *testing.T) {
	cases := []struct{ n, max, want int }{
		{0, 10, 0}, {1, 10, 1}, {3, 10, 2}, {6, 10, 3}, {10, 10, 4}, {5, 0, 0},
	}
	for _, c := range cases {
		if got := punchLevel(c.n, c.max); got != c.want {
			t.Fatalf("punchLevel(%d, %d) = %d, want %d", c.n, c.max, got, c.want)
		}
	}
}

func TestHourLabelReadsLikeAClock(t *testing.T) {
	cases := map[int]string{0: "12am", 9: "9am", 12: "12pm", 13: "1pm", 23: "11pm"}
	for in, want := range cases {
		if got := hourLabel(in); got != want {
			t.Fatalf("hourLabel(%d) = %q, want %q", in, got, want)
		}
	}
}

// Every language's arc plus its gap has to add up to the ring, or the segments
// drift out of place as they accumulate.
func TestLanguageDonutArcsCoverTheRing(t *testing.T) {
	donut := buildLanguageDonut([]LangStat{
		{Tech: "Go", Lines: 600, Percentage: 60},
		{Tech: "Kotlin", Lines: 300, Percentage: 30},
		{Tech: "CSS", Lines: 100, Percentage: 10},
	})

	if !donut.HasData || len(donut.Segments) != 3 {
		t.Fatalf("donut has %d segments, want 3", len(donut.Segments))
	}
	if donut.TotalLines != 1000 || donut.TopTech != "Go" {
		t.Fatalf("donut summary wrong: %d lines, top %q", donut.TotalLines, donut.TopTech)
	}

	for _, seg := range donut.Segments {
		if math.Abs(seg.Dash+seg.Gap-donut.Circumference) > 0.01 {
			t.Fatalf("segment %q dash+gap = %v, want the circumference %v", seg.Tech, seg.Dash+seg.Gap, donut.Circumference)
		}
	}
	// Offsets run backwards around the ring, each starting where the last ended.
	if donut.Segments[0].Offset != 0 {
		t.Fatalf("first segment starts at offset %v, want 0", donut.Segments[0].Offset)
	}
	if donut.Segments[1].Offset >= 0 || donut.Segments[2].Offset >= donut.Segments[1].Offset {
		t.Fatalf("segment offsets do not accumulate: %v then %v", donut.Segments[1].Offset, donut.Segments[2].Offset)
	}

	if empty := buildLanguageDonut(nil); empty.HasData {
		t.Fatal("an empty language list should report no data")
	}
	if zeroed := buildLanguageDonut([]LangStat{{Tech: "Go", Lines: 0}}); zeroed.HasData {
		t.Fatal("a language with no lines should not divide by zero into a chart")
	}
}

// The chart is server-rendered SVG: it has to be complete in the response,
// before any script has run.
func TestActivityTemplateRendersChartWithoutScript(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("Failed to parse embedded templates: %v", err)
	}

	rng := lookupActivityRange("30d")
	chart := assembleActivityChart(rng, mustDay(t, "2026-08-04"), mustDay(t, "2026-08-01"), []activityRow{
		{bucket: mustDay(t, "2026-08-02"), commits: 5, added: 210, deleted: 30},
	})

	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "activity.html", chart); err != nil {
		t.Fatalf("activity.html failed to execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`<svg class="chart-svg"`,
		`data-metric="commits"`,
		`data-metric="added"`,
		`data-metric="deleted"`,
		`data-metric="net"`,
		`class="chart-line"`,
		`class="chart-hit"`,
		`data-range="90d"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("activity.html output missing %q", want)
		}
	}
	if strings.Contains(out, "<script") {
		t.Fatal("the swapped partial should not carry script tags")
	}
	// One hit rectangle per bucket, each reachable by keyboard.
	if got := strings.Count(out, `class="chart-hit"`); got != len(chart.Buckets) {
		t.Fatalf("%d hit areas for %d buckets", got, len(chart.Buckets))
	}
	if got := strings.Count(out, `tabindex="0"`); got != len(chart.Buckets) {
		t.Fatalf("%d focusable buckets, want %d", got, len(chart.Buckets))
	}
}
