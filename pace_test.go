package main

import (
	"strings"
	"testing"
	"time"
)

const fiveH = 5 * time.Hour

func TestPercentLeft(t *testing.T) {
	cases := []struct {
		util    float64
		present bool
		want    int
		wantOK  bool
	}{
		{0, true, 100, true},
		{73.4, true, 27, true},
		{100, true, 0, true},
		{120, true, 0, true}, // over-utilization cannot go negative
		{0, false, 0, false}, // absent means unknown, not 100
	}
	for _, c := range cases {
		got, ok := percentLeft(c.util, c.present)
		if got != c.want || ok != c.wantOK {
			t.Errorf("percentLeft(%v, %v) = (%d, %v), want (%d, %v)", c.util, c.present, got, ok, c.want, c.wantOK)
		}
	}
}

func TestFmtRelative(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "now"},
		{45 * time.Minute, "45m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
		{30 * time.Hour, "1d6h"},
		{7 * 24 * time.Hour, "7d"},
	}
	for _, c := range cases {
		if got := fmtRelative(c.in); got != c.want {
			t.Errorf("fmtRelative(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPace(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	// Half the window gone, budget untouched: far ahead.
	if p, ok := pace(100, true, now.Add(fiveH/2), fiveH, now); !ok || p.label != "well-ahead" {
		t.Errorf("untouched budget at half window = %+v, %v", p, ok)
	}
	// Budget and clock exactly aligned: on pace, zero headroom.
	p, ok := pace(50, true, now.Add(fiveH/2), fiveH, now)
	if !ok || p.label != "on-pace" || p.headroomPts != 0 {
		t.Errorf("aligned budget and clock = %+v, %v; want on-pace at 0pts", p, ok)
	}
	// Most of the budget gone with most of the window left: burning fast.
	if p, ok := pace(10, true, now.Add(time.Duration(float64(fiveH)*0.9)), fiveH, now); !ok || p.label != "burning-fast" {
		t.Errorf("10%% left early in window = %+v, %v", p, ok)
	}
	// Not enough information to judge.
	if _, ok := pace(0, false, now.Add(fiveH), fiveH, now); ok {
		t.Error("pace with unknown percent left should not produce a verdict")
	}
	if _, ok := pace(50, true, time.Time{}, fiveH, now); ok {
		t.Error("pace with no reset time should not produce a verdict")
	}
}

// The aggregate is the part a caller trusts to make a decision, so its tie-breaking is
// pinned: the window that runs dry first wins, which is not always the lowest percentage.
func TestSummarizePicksWindowThatRunsDryFirst(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	mk := func(label string, left int, resetsIn time.Duration, window time.Duration) row {
		p, ok := pace(left, true, now.Add(resetsIn), window, now)
		return row{label: label, left: left, leftOK: true, pace: p, paceOK: ok, resetsAt: now.Add(resetsIn)}
	}

	// weekly has more percent left but far less headroom against its clock, so it is the
	// binding constraint even though session_5h shows a smaller number.
	rows := []row{
		mk("session_5h", 40, 30*time.Minute, fiveH),
		mk("weekly", 50, 6*24*time.Hour, 7*24*time.Hour),
	}
	got, ok := summarize(rows)
	if !ok {
		t.Fatal("summarize returned nothing for two usable windows")
	}
	if got.tightest != "weekly" {
		t.Errorf("tightest = %q, want weekly (lower headroom despite higher percent left)", got.tightest)
	}

	// Exhaustion dominates everything else.
	stop := []row{mk("session_5h", 3, 2*time.Hour, fiveH)}
	if got, _ := summarize(stop); got.verdict != "stop" {
		t.Errorf("3%% left = %q, want stop", got.verdict)
	}
	// Low but not gone.
	low := []row{mk("weekly", 15, 3*24*time.Hour, 7*24*time.Hour)}
	if got, _ := summarize(low); got.verdict != "caution" {
		t.Errorf("15%% left = %q, want caution", got.verdict)
	}
	// Healthy.
	fine := []row{mk("weekly", 80, 2*24*time.Hour, 7*24*time.Hour)}
	if got, _ := summarize(fine); got.verdict != "safe" {
		t.Errorf("80%% left with most of the window gone = %q, want safe", got.verdict)
	}
	// Nothing usable at all.
	if _, ok := summarize([]row{{label: "weekly"}}); ok {
		t.Error("summarize should report nothing when no window has a usable number")
	}
}

func TestSummarizePrefersJudgeableWindow(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	p, ok := pace(60, true, now.Add(time.Hour), fiveH, now)
	rows := []row{
		{label: "no_reset", left: 10, leftOK: true},                                                  // lower percent, but unjudgeable
		{label: "weekly", left: 60, leftOK: true, pace: p, paceOK: ok, resetsAt: now.Add(time.Hour)}, // judgeable
	}
	got, _ := summarize(rows)
	if got.tightest != "weekly" {
		t.Errorf("tightest = %q, want weekly: a window we can judge beats one we cannot", got.tightest)
	}
}

// A data-less window must be dropped, not rendered as a row of "unknown": on plans that
// do not meter Opus separately the endpoint returns seven_day_opus with nothing in it,
// and a row of unknowns reads like a failed lookup of a limit that applies.
func TestParsePayloadDropsEmptyWindows(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 6, "resets_at": "2026-08-10T16:00:00Z"},
		"seven_day": {"utilization": 27, "resets_at": "2026-08-15T13:00:00Z"},
		"seven_day_opus": null,
		"limits": [{"kind": "session", "severity": "normal"}]
	}`)
	buckets, _, ok := parsePayload(body)
	if !ok {
		t.Fatal("parsePayload rejected a well-formed payload")
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d windows, want 2 (the empty seven_day_opus must be dropped)", len(buckets))
	}
	for _, b := range buckets {
		if b.key == "seven_day_opus" {
			t.Error("seven_day_opus was kept despite carrying no data")
		}
	}
}

func TestParsePayloadRejectsUnknownShape(t *testing.T) {
	if _, _, ok := parsePayload([]byte(`not json`)); ok {
		t.Error("non-JSON should not parse")
	}
	_, keys, ok := parsePayload([]byte(`{"something_else": {"utilization": 5}}`))
	if ok {
		t.Error("a payload with no known windows should fail rather than report nothing")
	}
	if !strings.Contains(keys, "something_else") {
		t.Errorf("failure detail should name the keys it did see, got %q", keys)
	}
}
