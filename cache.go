package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A tiny on-disk cache of the last reading.
//
// This exists because of something measured, not anticipated: the usage endpoint itself
// returns 429 when called repeatedly in quick succession. The intended caller is an agent
// checking its budget inside a work loop, so without a cache the tool would poison the
// very thing it reports on. Two behaviours follow:
//
//  1. A reading younger than the TTL is served straight from cache, so a loop is free.
//  2. When a live fetch fails, a cached reading is served with its age stated loudly
//     instead of failing outright. A number labelled "4m old" is far more useful than
//     "unavailable", and labelling it is what keeps that honest.
//
// The cache holds only derived numbers. The token is never written here.

// The TTL is set from measurement, not taste. Probing the endpoint: a second call 250ms
// after the first is refused once the burst allowance is drained, and recovery took 120s
// of polling to clear. Against that, the data being reported moves over a 5h and a 7d
// window, so a 5-minute-old reading is barely different from a live one. Anything finer
// buys resolution the data does not have and pays for it in 429s.
const (
	cacheTTL      = 5 * time.Minute // serve without touching the network
	cacheMaxStale = 6 * time.Hour   // beyond this, a stale reading is misleading, not useful
)

type cachedBucket struct {
	Key         string     `json:"key"`
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
	Severity    string     `json:"severity"`
}

type cacheFile struct {
	Version   int            `json:"version"`
	FetchedAt time.Time      `json:"fetched_at"`
	Transport string         `json:"transport"`
	CredFrom  string         `json:"credential_source"`
	Plan      string         `json:"plan"`
	Buckets   []cachedBucket `json:"buckets"`
}

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "claude-runway", "last.json")
}

func cacheDisabled() bool { return os.Getenv("CLAUDE_RUNWAY_NO_CACHE") == "1" }

func cacheTTLFromEnv() time.Duration {
	if v := os.Getenv("CLAUDE_RUNWAY_CACHE_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return cacheTTL
}

// writeCache is best effort: a machine with no writable cache directory should still get a
// working tool, just without the 429 protection.
func writeCache(r reading) {
	if cacheDisabled() || !r.ok {
		return
	}
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f := cacheFile{Version: 1, FetchedAt: r.fetchedAt, Transport: r.transport, CredFrom: r.credSource, Plan: r.plan}
	for _, b := range r.buckets {
		cb := cachedBucket{Key: b.key, Severity: b.severity}
		if b.hasUtil {
			u := b.utilization
			cb.Utilization = &u
		}
		if !b.resetsAt.IsZero() {
			t := b.resetsAt
			cb.ResetsAt = &t
		}
		f.Buckets = append(f.Buckets, cb)
	}
	buf, err := json.Marshal(f)
	if err != nil {
		return
	}
	// Written 0600 and replaced atomically, so a concurrent reader never sees half a file.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// readCache returns the last reading and its age. ok is false when there is nothing usable.
func readCache() (reading, time.Duration, bool) {
	if cacheDisabled() {
		return reading{}, 0, false
	}
	p := cachePath()
	if p == "" {
		return reading{}, 0, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return reading{}, 0, false
	}
	var f cacheFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) == 0 {
		return reading{}, 0, false
	}
	age := time.Since(f.FetchedAt)
	if age < 0 || age > cacheMaxStale {
		// A negative age means the clock moved; either way the file is not trustworthy.
		return reading{}, 0, false
	}
	r := reading{ok: true, transport: f.Transport, credSource: f.CredFrom, plan: f.Plan, fetchedAt: f.FetchedAt}
	for _, cb := range f.Buckets {
		b := bucket{key: cb.Key, severity: cb.Severity}
		if cb.Utilization != nil {
			b.utilization, b.hasUtil = *cb.Utilization, true
		}
		if cb.ResetsAt != nil {
			b.resetsAt = *cb.ResetsAt
		}
		r.buckets = append(r.buckets, b)
	}
	return r, age, true
}
