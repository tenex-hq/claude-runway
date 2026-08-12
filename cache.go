package main

import (
	"encoding/json"
	"fmt"
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

// cacheTTLEnvError names a malformed CLAUDE_RUNWAY_CACHE_SECONDS, or returns "" when the
// variable is absent or usable.
//
// CLAUDE_RUNWAY_CACHE_SECONDS=banana used to become the 300s default in silence, so a caller
// who asked for a TTL and got a different one had no way to notice. That contradicts the
// fail-loud rule the --fields path states outright. Of the two honest fixes, refusing at
// startup beats printing a warning next to the numbers, because the numbers are consumed by
// machines and a warning line they do not parse changes nothing.
//
// The check lives apart from cacheTTLFromEnv on purpose. cacheTTLFromEnv keeps its fallback
// and its no-error signature because render.go's describeFailure calls it to build a help
// string, where an error return would have to be invented and then ignored, and every other
// call site would grow a branch it cannot act on. The tradeoff is that the fallback still
// exists and must stay in agreement with this function about what "valid" means: both go
// through strconv.Atoi with the same secs >= 0 rule. run() refuses first, so in practice
// nothing reaches the fallback without having been told.
func cacheTTLEnvError() string {
	v := os.Getenv("CLAUDE_RUNWAY_CACHE_SECONDS")
	if v == "" {
		return ""
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Sprintf("CLAUDE_RUNWAY_CACHE_SECONDS=%q is not a whole number of seconds", v)
	}
	if secs < 0 {
		return fmt.Sprintf("CLAUDE_RUNWAY_CACHE_SECONDS=%q is negative", v)
	}
	return ""
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
	// Written 0600 and replaced by rename, so a reader never sees half a file. The temp name
	// has to be unique per process to make that true: it used to be a fixed p+".tmp", which
	// meant two concurrent invocations (an agent loop plus a human in a terminal is an
	// ordinary combination for this tool) shared one temp path, and either could rename a
	// file the other was halfway through writing. That is exactly the tear the rename was
	// there to prevent.
	tmp, err := os.CreateTemp(filepath.Dir(p), "last-*.json")
	if err != nil {
		return
	}
	// Every path out of here from now on either renames the file or removes it: a cache
	// directory slowly filling with half-written last-*.json files is a worse bug than the
	// race this fixes.
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	// os.CreateTemp already opens 0600 and a umask can only clear bits, never add them, so
	// this cannot loosen the mode. Set anyway so the file the tool leaves behind has exactly
	// one documented mode rather than one that depends on the caller's umask.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return
	}
	renamed = true
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
