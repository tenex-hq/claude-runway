package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// isolateCache points the cache at a scratch directory. Both variables are set because
// os.UserCacheDir consults XDG_CACHE_HOME on Linux and HOME on macOS.
func isolateCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("CLAUDE_RUNWAY_NO_CACHE", "")
}

func TestCacheRoundTrip(t *testing.T) {
	isolateCache(t)
	now := time.Now()
	live := reading{
		ok: true,
		buckets: []bucket{
			{key: "five_hour", utilization: 40, hasUtil: true, resetsAt: now.Add(2 * time.Hour), severity: "normal"},
			{key: "seven_day", utilization: 27, hasUtil: true, resetsAt: now.Add(5 * 24 * time.Hour), severity: "warning"},
		},
		transport: "http", credSource: "file", plan: "team", fetchedAt: now,
	}
	writeCache(live)

	got, age, ok := readCache()
	if !ok {
		t.Fatal("cache did not read back")
	}
	if age > time.Minute {
		t.Errorf("age = %v, want near zero", age)
	}
	if len(got.buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(got.buckets))
	}
	if got.buckets[0].utilization != 40 || !got.buckets[0].hasUtil {
		t.Errorf("utilization lost: %+v", got.buckets[0])
	}
	if !got.buckets[0].resetsAt.Equal(live.buckets[0].resetsAt) {
		t.Errorf("resetsAt lost: got %v want %v", got.buckets[0].resetsAt, live.buckets[0].resetsAt)
	}
	if got.buckets[1].severity != "warning" || got.plan != "team" {
		t.Errorf("metadata lost: %+v", got)
	}
}

// The cache must never become a place a token can leak to.
func TestCacheHoldsNoSecret(t *testing.T) {
	isolateCache(t)
	writeCache(reading{
		ok:      true,
		buckets: []bucket{{key: "five_hour", utilization: 10, hasUtil: true, resetsAt: time.Now().Add(time.Hour)}},
		plan:    "team", credSource: "file", transport: "http", fetchedAt: time.Now(),
	})
	// Whatever the file contains, it is built from a fixed field list; assert the shape
	// rather than trusting that, so a future field cannot smuggle one in.
	raw, err := readFileForTest(cachePath())
	if err != nil {
		t.Fatalf("cache file unreadable: %v", err)
	}
	for _, forbidden := range []string{"token", "accessToken", "Bearer", "refresh"} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(forbidden)) {
			t.Errorf("cache file mentions %q:\n%s", forbidden, raw)
		}
	}
}

func TestCacheDisabledByEnv(t *testing.T) {
	isolateCache(t)
	t.Setenv("CLAUDE_RUNWAY_NO_CACHE", "1")
	writeCache(reading{ok: true, buckets: []bucket{{key: "five_hour", utilization: 1, hasUtil: true}}, fetchedAt: time.Now()})
	if _, _, ok := readCache(); ok {
		t.Error("cache should be inert when CLAUDE_RUNWAY_NO_CACHE=1")
	}
}

func TestCacheRejectsAncientAndFutureReadings(t *testing.T) {
	isolateCache(t)
	mk := func(fetchedAt time.Time) {
		writeCache(reading{
			ok:        true,
			buckets:   []bucket{{key: "five_hour", utilization: 1, hasUtil: true, resetsAt: time.Now()}},
			fetchedAt: fetchedAt,
		})
	}
	mk(time.Now().Add(-cacheMaxStale - time.Hour))
	if _, _, ok := readCache(); ok {
		t.Error("a reading older than the stale ceiling should be refused, not labelled")
	}
	mk(time.Now().Add(2 * time.Hour)) // clock moved backwards since it was written
	if _, _, ok := readCache(); ok {
		t.Error("a reading from the future should be refused")
	}
}

// The degradation path: a failed live read serves the last reading, flagged, rather than
// nothing. This mirrors what autodev does in-process, and the flag is what keeps it honest.
func TestStaleFallbackIsLabelledNotSilent(t *testing.T) {
	isolateCache(t)
	past := time.Now().Add(-10 * time.Minute)
	writeCache(reading{
		ok:        true,
		buckets:   []bucket{{key: "seven_day", utilization: 27, hasUtil: true, resetsAt: time.Now().Add(3 * 24 * time.Hour), severity: "normal"}},
		transport: "http", credSource: "file", fetchedAt: past,
	})

	got := staleOr(reading{reason: failHTTP, status: 429, detail: "rate_limit_error"})
	if !got.ok || !got.stale || !got.fromCache {
		t.Fatalf("expected a stale-but-usable reading, got %+v", got)
	}
	if got.reason != failHTTP || got.status != 429 {
		t.Errorf("the original failure must be preserved so the output can explain itself, got %v/%d", got.reason, got.status)
	}

	out := renderTOON(got, time.Now(), renderOpts{brief: true})
	if !strings.Contains(out, "warning: NOT LIVE") {
		t.Errorf("stale reading rendered without a warning:\n%s", out)
	}
	if !strings.Contains(out, "source: cache-stale") || !strings.Contains(out, "age: 10m") {
		t.Errorf("stale reading must state its source and age:\n%s", out)
	}
	if !strings.Contains(out, "rate-limited") {
		t.Errorf("stale reading should say why it is not live:\n%s", out)
	}
	// It still has to be usable: the numbers and the verdict are the point.
	if !strings.Contains(out, "windows[1]") || !strings.Contains(out, "verdict: ") {
		t.Errorf("stale reading lost its payload:\n%s", out)
	}
}

// A 429 from the meter must never read as an exhausted subscription.
func TestRateLimitOnTheMeterIsNotBudgetExhaustion(t *testing.T) {
	f := describeFailure(reading{reason: failHTTP, status: 429, detail: "rate_limit_error"})
	if !strings.Contains(f.message, "NOT your subscription allowance") {
		t.Errorf("429 message must distinguish the two limits, got: %s", f.message)
	}
	if strings.Contains(f.message, "0%") || strings.Contains(f.message, "exhausted your") {
		t.Errorf("429 message must not imply the budget is gone, got: %s", f.message)
	}
	if f.help == "" {
		t.Error("429 should tell the caller what to do")
	}
}

func TestFmtAgeHasSecondResolution(t *testing.T) {
	// "0m" for a 3-second-old reading would read as live when it is not.
	if got := fmtAge(3 * time.Second); got != "3s" {
		t.Errorf("fmtAge(3s) = %q, want 3s", got)
	}
	if got := fmtAge(90 * time.Second); got != "2m" {
		t.Errorf("fmtAge(90s) = %q, want 2m", got)
	}
}

func readFileForTest(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
