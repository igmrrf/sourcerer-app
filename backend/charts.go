package main

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The overview dashboard renders charts rather than the summary rows the public
// profile uses. All the geometry is computed here and handed to the template as
// plain numbers: the browser gets an SVG it can display before any script runs,
// and the inline JS on the page only adds hover, keyboard and metric switching
// on top of it.
//
// Commit timestamps are stored as Unix seconds. Every bucket boundary below is
// computed in UTC so the same history produces the same chart wherever it is
// viewed.

const (
	chartWidth   = 1000.0
	chartHeight  = 280.0
	chartPadTop  = 18.0
	chartPadRght = 14.0
	chartPadBot  = 30.0
	chartPadLeft = 52.0
)

// activityRange is one entry in the range switcher above the activity chart.
type activityRange struct {
	Key   string
	Label string
	// Days is how far back the range reaches; 0 means all recorded history.
	Days int
	// Unit is the bucket width: "day", "week" or "month".
	Unit string
	// Layout formats a bucket's tooltip title.
	Layout string
	// TickLayout formats the x axis labels, which are shorter.
	TickLayout string
}

var activityRanges = []activityRange{
	{Key: "30d", Label: "30 days", Days: 30, Unit: "day", Layout: "Mon, 2 Jan 2006", TickLayout: "2 Jan"},
	{Key: "90d", Label: "90 days", Days: 90, Unit: "week", Layout: "Week of 2 Jan 2006", TickLayout: "2 Jan"},
	{Key: "12m", Label: "12 months", Days: 365, Unit: "month", Layout: "January 2006", TickLayout: "Jan"},
	{Key: "all", Label: "All time", Days: 0, Unit: "month", Layout: "January 2006", TickLayout: "Jan 06"},
}

func lookupActivityRange(key string) activityRange {
	for _, rng := range activityRanges {
		if rng.Key == key {
			return rng
		}
	}
	return activityRanges[0]
}

// RangeOption is the template's view of a range switcher button.
type RangeOption struct {
	Key      string
	Label    string
	IsActive bool
}

// AxisTick is one horizontal gridline: a value and where it sits.
type AxisTick struct {
	Label string
	Y     float64
}

// ChartMetric is one switchable series. Every metric is rendered into the same
// SVG and hidden with CSS, so switching between them costs no round trip.
type ChartMetric struct {
	Key   string
	Label string
	// Unit is appended to the tooltip figure ("commits", "lines").
	Unit string
	// Line and Area are SVG path data. Area is the line closed to the baseline.
	Line string
	Area string
	// Ticks are this metric's own y axis, since the scales differ wildly.
	Ticks []AxisTick
	// Zero is the baseline y, which only differs from the bottom for net lines.
	Zero float64
	// Peak and Total describe the series in the caption line.
	Peak  int
	Total int
}

// ActivityBucket is one point in time. The template renders an invisible hit
// rectangle per bucket so hover and keyboard focus have something to land on.
type ActivityBucket struct {
	// Title is the human-readable period, used in the tooltip.
	Title string
	// TickLabel is non-empty only for buckets that carry an x axis label.
	TickLabel string
	Commits   int
	Added     int
	Deleted   int
	Net       int
	// CenterX is the bucket's x position; HitX and HitW bound its hit area.
	CenterX float64
	HitX    float64
	HitW    float64
	// Y positions of this bucket on each metric's curve, so the crosshair dot
	// can follow whichever series is showing.
	YCommits float64
	YAdded   float64
	YDeleted float64
	YNet     float64
}

// ActivityChart is everything activity.html needs.
type ActivityChart struct {
	HasData bool
	Range   string
	Ranges  []RangeOption
	Unit    string
	Buckets []ActivityBucket
	Metrics []ChartMetric

	Width, Height             float64
	PlotLeft, PlotRight       float64
	PlotTop, PlotBottom       float64
	PlotHeight                float64
	TotalCommits              int
	TotalAdded, TotalDeleted  int
	BusiestLabel              string
	BusiestCommits            int
	ActiveBuckets, AllBuckets int
	// Cadence is the average commits per active bucket, already formatted.
	Cadence string
}

type activityRow struct {
	bucket  time.Time
	commits int
	added   int
	deleted int
}

// truncateToUnit snaps a UTC instant back to the start of its bucket. Weeks
// start on Monday, matching date_trunc('week') in Postgres.
func truncateToUnit(t time.Time, unit string) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch unit {
	case "week":
		offset := (int(day.Weekday()) + 6) % 7 // Monday = 0
		return day.AddDate(0, 0, -offset)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return day
	}
}

func advanceUnit(t time.Time, unit string) time.Time {
	switch unit {
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	default:
		return t.AddDate(0, 0, 1)
	}
}

// queryActivity groups an author's commits into calendar buckets. Postgres does
// the bucketing so a long history never has to be pulled row by row.
func queryActivity(email, unit string, since int64) []activityRow {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT date_trunc($2, to_timestamp(date) AT TIME ZONE 'UTC') AS bucket,
		       COUNT(*),
		       COALESCE(SUM(num_lines_added), 0),
		       COALESCE(SUM(num_lines_deleted), 0)
		FROM commits
		WHERE author_email = $1 AND date >= $3
		GROUP BY bucket
		ORDER BY bucket`, email, unit, since)
	if err != nil {
		slog.Error("Error querying commit activity", "email", email, "error", err)
		return nil
	}
	defer rows.Close()

	var out []activityRow
	for rows.Next() {
		var r activityRow
		if err := rows.Scan(&r.bucket, &r.commits, &r.added, &r.deleted); err != nil {
			slog.Error("Error scanning commit activity row", "error", err)
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating commit activity", "error", err)
	}
	return out
}

// earliestCommit is used by the all-time range to decide where the axis starts.
func earliestCommit(email string) (time.Time, bool) {
	if db == nil {
		return time.Time{}, false
	}
	var epoch int64
	if err := db.QueryRow("SELECT COALESCE(MIN(date), 0) FROM commits WHERE author_email = $1", email).Scan(&epoch); err != nil {
		slog.Error("Error querying earliest commit", "email", email, "error", err)
		return time.Time{}, false
	}
	if epoch <= 0 {
		return time.Time{}, false
	}
	return time.Unix(epoch, 0).UTC(), true
}

// niceCeiling rounds an axis maximum up to a round number so the gridline
// labels read as 200 / 400 / 600 rather than 187 / 374 / 561.
func niceCeiling(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	switch f := v / exp; {
	case f <= 1:
		return exp
	case f <= 2:
		return 2 * exp
	case f <= 2.5:
		return 2.5 * exp
	case f <= 5:
		return 5 * exp
	default:
		return 10 * exp
	}
}

// compactNumber keeps axis labels narrow: 12400 becomes 12.4k.
func compactNumber(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1_000_000:
		return trimZero(v/1_000_000) + "M"
	case abs >= 1_000:
		return trimZero(v/1_000) + "k"
	default:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
}

func trimZero(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

func fmtCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// buildMetric turns one series of values into paths, ticks and per-bucket y
// positions. lo and hi bound the axis; lo is below zero only for net lines.
func buildMetric(key, label, unit string, values []int, xs []float64, lo, hi float64) (ChartMetric, []float64) {
	plotTop := chartPadTop
	plotBottom := chartHeight - chartPadBot

	if hi <= lo {
		hi = lo + 1
	}
	// Snap the top of the axis to a whole number of gridline steps so the last
	// label sits exactly on the top of the plot.
	step := math.Max(1, niceCeiling((hi-lo)/5))
	hi = lo + step*math.Ceil((hi-lo)/step)

	scale := func(v float64) float64 {
		return plotBottom - (v-lo)/(hi-lo)*(plotBottom-plotTop)
	}

	ys := make([]float64, len(values))
	var line strings.Builder
	for i, v := range values {
		ys[i] = scale(float64(v))
		if i == 0 {
			line.WriteString("M")
		} else {
			line.WriteString(" L")
		}
		line.WriteString(fmtCoord(xs[i]) + "," + fmtCoord(ys[i]))
	}

	zero := scale(math.Max(lo, 0))
	area := ""
	if len(xs) > 0 {
		area = "M" + fmtCoord(xs[0]) + "," + fmtCoord(zero) +
			" L" + strings.TrimPrefix(line.String(), "M") +
			" L" + fmtCoord(xs[len(xs)-1]) + "," + fmtCoord(zero) + " Z"
	}

	// Gridlines step by a round amount rather than by an even fraction of the
	// range, so the labels read 0 / 5 / 10 / 15 instead of 0 / 6.25 / 12.5.
	var ticks []AxisTick
	for v := lo; v <= hi+step/2; v += step {
		ticks = append(ticks, AxisTick{Label: compactNumber(v), Y: scale(v)})
	}

	total, peak := 0, 0
	for _, v := range values {
		total += v
		if v > peak {
			peak = v
		}
	}

	return ChartMetric{
		Key: key, Label: label, Unit: unit,
		Line: line.String(), Area: area, Ticks: ticks, Zero: zero,
		Peak: peak, Total: total,
	}, ys
}

// buildActivityChart assembles the commit activity chart for one author. The
// database work lives here; the geometry lives in assembleActivityChart, which
// is a pure function of the rows it is given.
func buildActivityChart(email, rangeKey string) ActivityChart {
	rng := lookupActivityRange(rangeKey)
	now := time.Now().UTC()

	start := truncateToUnit(now.AddDate(0, 0, -rng.Days+1), rng.Unit)
	if rng.Days == 0 {
		first, ok := earliestCommit(email)
		if !ok {
			return assembleActivityChart(rng, now, start, nil)
		}
		start = truncateToUnit(first, rng.Unit)
	}

	return assembleActivityChart(rng, now, start, queryActivity(email, rng.Unit, start.Unix()))
}

// assembleActivityChart turns bucketed rows into the paths, ticks and hit areas
// activity.html renders.
func assembleActivityChart(rng activityRange, now, start time.Time, rows []activityRow) ActivityChart {
	chart := ActivityChart{
		Range:      rng.Key,
		Unit:       rng.Unit,
		Width:      chartWidth,
		Height:     chartHeight,
		PlotLeft:   chartPadLeft,
		PlotRight:  chartWidth - chartPadRght,
		PlotTop:    chartPadTop,
		PlotBottom: chartHeight - chartPadBot,
		PlotHeight: chartHeight - chartPadBot - chartPadTop,
	}
	for _, r := range activityRanges {
		chart.Ranges = append(chart.Ranges, RangeOption{Key: r.Key, Label: r.Label, IsActive: r.Key == rng.Key})
	}

	if len(rows) == 0 {
		return chart
	}
	byBucket := make(map[int64]activityRow, len(rows))
	for _, r := range rows {
		byBucket[r.bucket.UTC().Unix()] = r
	}

	// Walk the calendar rather than the result set, so quiet periods stay
	// visible as gaps in the line instead of being collapsed away.
	var (
		labels   []string
		commits  []int
		added    []int
		deleted  []int
		net      []int
		titles   []string
		maxCount = 0
	)
	for cursor := start; !cursor.After(now); cursor = advanceUnit(cursor, rng.Unit) {
		row := byBucket[cursor.Unix()]
		commits = append(commits, row.commits)
		added = append(added, row.added)
		deleted = append(deleted, row.deleted)
		net = append(net, row.added-row.deleted)
		titles = append(titles, cursor.Format(rng.Layout))
		labels = append(labels, cursor.Format(rng.TickLayout))
		if row.commits > maxCount {
			maxCount = row.commits
		}
		if len(commits) > 400 {
			break // guard against a pathological date in the data
		}
	}
	if len(commits) == 0 {
		return chart
	}
	chart.HasData = true

	// x positions, evenly spaced across the plot area.
	xs := make([]float64, len(commits))
	span := chart.PlotRight - chart.PlotLeft
	step := span
	if len(commits) > 1 {
		step = span / float64(len(commits)-1)
	}
	for i := range commits {
		xs[i] = chart.PlotLeft + step*float64(i)
		if len(commits) == 1 {
			xs[i] = chart.PlotLeft + span/2
		}
	}

	maxLines := 0
	minNet, maxNet := 0, 0
	for i := range commits {
		if added[i] > maxLines {
			maxLines = added[i]
		}
		if deleted[i] > maxLines {
			maxLines = deleted[i]
		}
		if net[i] > maxNet {
			maxNet = net[i]
		}
		if net[i] < minNet {
			minNet = net[i]
		}
	}

	countHi := niceCeiling(float64(maxCount))
	lineHi := niceCeiling(float64(maxLines))
	netHi := niceCeiling(float64(maxNet))
	netLo := -niceCeiling(math.Abs(float64(minNet)))
	if minNet == 0 {
		netLo = 0
	}

	commitMetric, yCommits := buildMetric("commits", "Commits", "commits", commits, xs, 0, countHi)
	addedMetric, yAdded := buildMetric("added", "Lines added", "lines", added, xs, 0, lineHi)
	deletedMetric, yDeleted := buildMetric("deleted", "Lines deleted", "lines", deleted, xs, 0, lineHi)
	netMetric, yNet := buildMetric("net", "Net lines", "lines", net, xs, netLo, netHi)
	chart.Metrics = []ChartMetric{commitMetric, addedMetric, deletedMetric, netMetric}

	// Label at most a dozen ticks, whatever the bucket count.
	labelEvery := 1
	if len(commits) > 12 {
		labelEvery = int(math.Ceil(float64(len(commits)) / 12))
	}

	busiest := 0
	for i := range commits {
		tick := ""
		if i%labelEvery == 0 || i == len(commits)-1 {
			tick = labels[i]
		}
		hitW := step
		if hitW <= 0 {
			hitW = span
		}
		chart.Buckets = append(chart.Buckets, ActivityBucket{
			Title: titles[i], TickLabel: tick,
			Commits: commits[i], Added: added[i], Deleted: deleted[i], Net: net[i],
			CenterX:  xs[i],
			HitX:     math.Max(chart.PlotLeft, xs[i]-hitW/2),
			HitW:     hitW,
			YCommits: yCommits[i], YAdded: yAdded[i],
			YDeleted: yDeleted[i], YNet: yNet[i],
		})

		chart.TotalCommits += commits[i]
		chart.TotalAdded += added[i]
		chart.TotalDeleted += deleted[i]
		if commits[i] > 0 {
			chart.ActiveBuckets++
		}
		if commits[i] > busiest {
			busiest = commits[i]
			chart.BusiestCommits = commits[i]
			chart.BusiestLabel = titles[i]
		}
	}
	chart.AllBuckets = len(commits)
	if chart.ActiveBuckets > 0 {
		chart.Cadence = strconv.FormatFloat(float64(chart.TotalCommits)/float64(chart.ActiveBuckets), 'f', 1, 64)
	}

	return chart
}

// PunchCell is one weekday/hour square in the punchcard.
type PunchCell struct {
	Commits int
	// Level buckets the count into 0-4 so the CSS can shade it.
	Level int
	Label string
}

// PunchRow is a weekday's worth of cells.
type PunchRow struct {
	Day     string
	Short   string
	Cells   []PunchCell
	Commits int
}

// PunchCard is the weekday x hour commit heatmap. It answers a question the
// profile page never asks: when does this person actually commit?
type PunchCard struct {
	HasData bool
	Rows    []PunchRow
	// Hours are the column headers, labelled every six hours.
	Hours []struct {
		Hour  int
		Label string
	}
	Max          int
	Total        int
	PeakLabel    string
	PeakCommits  int
	MorningShare int
	NightShare   int
}

var weekdayNames = [7]struct{ Long, Short string }{
	{"Monday", "Mon"}, {"Tuesday", "Tue"}, {"Wednesday", "Wed"}, {"Thursday", "Thu"},
	{"Friday", "Fri"}, {"Saturday", "Sat"}, {"Sunday", "Sun"},
}

// hourLabel renders an hour of the day the way a person would say it.
func hourLabel(h int) string {
	switch {
	case h == 0:
		return "12am"
	case h < 12:
		return fmt.Sprintf("%dam", h)
	case h == 12:
		return "12pm"
	default:
		return fmt.Sprintf("%dpm", h-12)
	}
}

// buildPunchCard reads an author's commit times; assemblePunchCard shapes them.
func buildPunchCard(email string) PunchCard {
	return assemblePunchCard(queryPunchCounts(email))
}

// queryPunchCounts returns commit counts indexed by [weekday][hour], Monday
// first, in UTC.
func queryPunchCounts(email string) [7][24]int {
	counts := [7][24]int{}
	if db != nil {
		rows, err := db.Query(`
			SELECT EXTRACT(ISODOW FROM to_timestamp(date) AT TIME ZONE 'UTC')::int AS dow,
			       EXTRACT(HOUR FROM to_timestamp(date) AT TIME ZONE 'UTC')::int AS hr,
			       COUNT(*)
			FROM commits
			WHERE author_email = $1
			GROUP BY dow, hr`, email)
		if err != nil {
			slog.Error("Error querying commit punchcard", "email", email, "error", err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var dow, hr, n int
				if err := rows.Scan(&dow, &hr, &n); err != nil {
					slog.Error("Error scanning punchcard row", "error", err)
					continue
				}
				if dow < 1 || dow > 7 || hr < 0 || hr > 23 {
					continue
				}
				counts[dow-1][hr] = n
			}
			if err := rows.Err(); err != nil {
				slog.Error("Error iterating punchcard", "error", err)
			}
		}
	}
	return counts
}

// assemblePunchCard shades the grid and pulls out the headline figures.
func assemblePunchCard(counts [7][24]int) PunchCard {
	card := PunchCard{}
	for h := 0; h < 24; h++ {
		label := ""
		if h%6 == 0 {
			label = hourLabel(h)
		}
		card.Hours = append(card.Hours, struct {
			Hour  int
			Label string
		}{Hour: h, Label: label})
	}

	for _, hours := range counts {
		for _, n := range hours {
			if n > card.Max {
				card.Max = n
			}
			card.Total += n
		}
	}
	card.HasData = card.Total > 0

	var morning, night int
	for d := 0; d < 7; d++ {
		row := PunchRow{Day: weekdayNames[d].Long, Short: weekdayNames[d].Short}
		for h := 0; h < 24; h++ {
			n := counts[d][h]
			row.Commits += n
			switch {
			case h >= 6 && h < 12:
				morning += n
			case h >= 22 || h < 6:
				night += n
			}
			cell := PunchCell{
				Commits: n,
				Level:   punchLevel(n, card.Max),
				Label:   fmt.Sprintf("%s %s — %s", weekdayNames[d].Short, hourLabel(h), plural(n, "commit")),
			}
			row.Cells = append(row.Cells, cell)
			if n > card.PeakCommits {
				card.PeakCommits = n
				card.PeakLabel = fmt.Sprintf("%s around %s", weekdayNames[d].Long, hourLabel(h))
			}
		}
		card.Rows = append(card.Rows, row)
	}

	if card.Total > 0 {
		card.MorningShare = int(math.Round(float64(morning) * 100 / float64(card.Total)))
		card.NightShare = int(math.Round(float64(night) * 100 / float64(card.Total)))
	}
	return card
}

// punchLevel maps a raw count onto the five shades the heatmap uses.
func punchLevel(n, max int) int {
	if n <= 0 || max <= 0 {
		return 0
	}
	level := int(math.Ceil(float64(n) * 4 / float64(max)))
	if level > 4 {
		level = 4
	}
	return level
}

// DonutSegment is one arc of the language ring. The arc is drawn with a
// stroked circle and a dash pattern, which needs no path arithmetic in the
// template and animates for free.
type DonutSegment struct {
	Tech       string
	Lines      int
	Percentage int
	Index      int
	// Dash is the visible arc length and DashGap the rest of the circumference.
	Dash float64
	Gap  float64
	// Offset rotates this arc to start where the previous one ended.
	Offset float64
}

// LanguageDonut replaces the profile page's stacked bar with a ring the
// overview can highlight segment by segment.
type LanguageDonut struct {
	HasData    bool
	Segments   []DonutSegment
	TotalLines int
	TopTech    string
	TopPct     int
	// Circumference is the ring length the dash values are computed against.
	Circumference float64
	Radius        float64
}

const donutRadius = 52.0

func buildLanguageDonut(stats []LangStat) LanguageDonut {
	donut := LanguageDonut{Radius: donutRadius, Circumference: 2 * math.Pi * donutRadius}
	if len(stats) == 0 {
		return donut
	}

	total := 0
	for _, s := range stats {
		total += s.Lines
	}
	if total <= 0 {
		return donut
	}
	donut.HasData = true
	donut.TotalLines = total
	donut.TopTech = stats[0].Tech
	donut.TopPct = stats[0].Percentage

	// A hairline gap between arcs keeps adjacent languages readable.
	const gapArc = 1.5
	offset := 0.0
	for i, s := range stats {
		share := float64(s.Lines) / float64(total)
		arc := math.Max(share*donut.Circumference-gapArc, 0.5)
		donut.Segments = append(donut.Segments, DonutSegment{
			Tech: s.Tech, Lines: s.Lines, Percentage: s.Percentage, Index: i,
			Dash: arc, Gap: donut.Circumference - arc, Offset: -offset,
		})
		offset += share * donut.Circumference
	}
	return donut
}

func handleDashboardActivity(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	renderTemplate(w, "activity.html", buildActivityChart(email, r.URL.Query().Get("range")))
}

func handleDashboardPunchcard(w http.ResponseWriter, r *http.Request) {
	email := getSessionEmail(r)
	if email == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	renderTemplate(w, "punchcard.html", buildPunchCard(email))
}
