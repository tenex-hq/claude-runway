package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A stale reading is only honest if it says why it is not live. Every failure reason therefore
// needs its own clause: "NOT LIVE" on its own tells a caller nothing about whether to retry, and
// a missing clause would leave the warning reading as a bug in the tool.
func TestStaleWarningNamesTheReasonItIsNotLive(t *testing.T) {
	now := time.Now()
	base := func(reason failure, status int) reading {
		return reading{
			ok:        true,
			fromCache: true,
			stale:     true,
			age:       10 * time.Minute,
			buckets:   []bucket{{key: "five_hour", utilization: 40, hasUtil: true, resetsAt: now.Add(2 * time.Hour), severity: "normal"}},
			fetchedAt: now.Add(-10 * time.Minute),
			reason:    reason,
			status:    status,
		}
	}
	cases := []struct {
		name   string
		reason failure
		status int
		want   string
	}{
		{"no credentials", failNoCreds, 0, "no credentials found"},
		{"expired token", failExpired, 0, "the stored token has expired"},
		{"rate limited", failHTTP, 429, "rate-limited"},
		{"other http status", failHTTP, 503, "HTTP 503"},
		{"unreachable", failTransport, 0, "unreachable"},
		{"unexpected shape", failBadPayload, 0, "unexpected shape"},
		// Nothing sets this today, but a future reason with no clause of its own must still
		// produce a sentence rather than an empty parenthesis.
		{"unnamed reason", failNone, 0, "unknown reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base(c.reason, c.status)
			out := renderTOON(r, now, renderOpts{brief: true})
			if !strings.HasPrefix(out, "warning: NOT LIVE") {
				t.Fatalf("a stale reading must lead with its warning:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("warning does not say %q:\n%s", c.want, out)
			}
			if !strings.Contains(out, "taken 10m ago") {
				t.Errorf("warning must state the age so nobody mistakes it for current:\n%s", out)
			}
			// It still has to be usable: the numbers are why serving a stale reading beats
			// serving nothing.
			if !strings.Contains(out, "windows[1]") || !strings.Contains(out, "session_5h,60,") {
				t.Errorf("stale reading lost its payload:\n%s", out)
			}

			// The JSON form has to carry the same explanation, in its own field, so a host
			// application is not left parsing prose out of the TOON.
			var j jsonOut
			if err := json.Unmarshal([]byte(renderJSON(r, now)), &j); err != nil {
				t.Fatalf("stale JSON does not parse: %v", err)
			}
			if j.Source != "cache-stale" {
				t.Errorf("source = %q, want cache-stale", j.Source)
			}
			if !strings.Contains(j.Warning, c.want) {
				t.Errorf("JSON warning does not say %q: %q", c.want, j.Warning)
			}
			if j.Status != "ok" {
				t.Errorf("status = %q: a stale reading is still a reading, and the warning is what qualifies it", j.Status)
			}
			if j.AgeSecs != 600 {
				t.Errorf("age_seconds = %d, want 600", j.AgeSecs)
			}
		})
	}
}

// A deliberate cache hit inside the TTL is not the same event as a stale fallback, and the two
// must not be labelled the same way: one is expected and fine, the other is a degraded answer.
func TestCacheHitAndStaleFallbackAreLabelledApart(t *testing.T) {
	now := time.Now()
	hit := reading{
		ok:        true,
		fromCache: true,
		age:       42 * time.Second,
		buckets:   []bucket{{key: "five_hour", utilization: 40, hasUtil: true, resetsAt: now.Add(2 * time.Hour)}},
		fetchedAt: now.Add(-42 * time.Second),
	}
	out := renderTOON(hit, now, renderOpts{brief: true})
	if !strings.Contains(out, "source: cache\nage: 42s") {
		t.Errorf("a fresh cache hit must state its source and age:\n%s", out)
	}
	if strings.Contains(out, "NOT LIVE") || strings.Contains(out, "cache-stale") {
		t.Errorf("a cache hit inside the TTL is not a degraded answer:\n%s", out)
	}

	var j jsonOut
	if err := json.Unmarshal([]byte(renderJSON(hit, now)), &j); err != nil {
		t.Fatal(err)
	}
	if j.Source != "cache" || j.Warning != "" {
		t.Errorf("JSON for a cache hit = source %q warning %q, want cache and no warning", j.Source, j.Warning)
	}
}
