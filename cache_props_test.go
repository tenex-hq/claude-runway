package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cache properties SECURITY.md states. They were documented and unasserted, which is the
// combination that quietly stops being true.

func cachedSample() reading {
	now := time.Now()
	return reading{
		ok: true,
		buckets: []bucket{
			{key: "five_hour", utilization: 40, hasUtil: true, resetsAt: now.Add(2 * time.Hour), severity: "normal"},
			{key: "seven_day", utilization: 27, hasUtil: true, resetsAt: now.Add(5 * 24 * time.Hour), severity: "normal"},
		},
		transport: "http", credSource: "file", plan: "max", fetchedAt: now,
	}
}

// Nobody else on a shared machine gets to read the reading, and nobody gets to list the
// directory it lives in.
//
// The file mode is asserted as exact equality: writeCache sets it with Chmod, which a umask
// cannot influence, so 0600 means 0600. The directory comes from MkdirAll(0700), whose mode IS
// filtered by the caller's umask, and a umask can only clear bits, never set them. So the
// directory is asserted as the security property instead: no group and no other bits.
func TestCacheFileAndDirectoryAreOwnerOnly(t *testing.T) {
	isolateCache(t)
	writeCache(cachedSample())

	fi, err := os.Stat(cachePath())
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("cache file mode = %#o, want 0600: it is written with an explicit Chmod", got)
	}

	di, err := os.Stat(filepath.Dir(cachePath()))
	if err != nil {
		t.Fatalf("cache directory not created: %v", err)
	}
	if !di.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Dir(cachePath()))
	}
	if got := di.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("cache directory mode = %#o, want no group or other bits (0700 filtered by umask)", got)
	}
	if got := di.Mode().Perm(); got&0o700 != 0o700 {
		t.Errorf("cache directory mode = %#o, which the owner cannot use", got)
	}
}

// The rename is what publishes the file. Two things follow, and only one of them is provable
// here: the temp file must never be left behind (a cache directory slowly filling with
// half-written last-*.json files is worse than the race the rename prevents), and the published
// file must be whole.
//
// True atomicity is NOT proven by this test. Observing a torn read would need a reader racing a
// writer at the right instant, which a unit test cannot arrange reliably. What is asserted is
// that no temp file survives any path out of writeCache, and that what lands is complete.
func TestWriteCacheLeavesNoTempFileBehind(t *testing.T) {
	isolateCache(t)
	// Written repeatedly, because the temp name is unique per call and a leak would only show
	// up as an accumulation.
	for i := 0; i < 3; i++ {
		writeCache(cachedSample())
	}
	dir := filepath.Dir(cachePath())

	leftovers, err := filepath.Glob(filepath.Join(dir, "last-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("writeCache left temp files behind: %v", leftovers)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "last.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache directory holds %v, want only last.json", names)
	}

	// Complete, not half a document: it parses, it is the current version, and the numbers
	// survived the round trip.
	raw, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatal(err)
	}
	var f cacheFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("published cache file does not parse, so it was published torn: %v\n%s", err, raw)
	}
	if f.Version != 1 || len(f.Buckets) != 2 {
		t.Errorf("published cache file is incomplete: %+v", f)
	}
	if f.Buckets[0].Utilization == nil || *f.Buckets[0].Utilization != 40 {
		t.Errorf("published cache file lost its numbers: %+v", f.Buckets[0])
	}
}

// The cache is best effort: a machine with no writable cache directory still gets a working
// tool, just without the 429 protection. Simulated portably by putting a regular file where the
// cache directory needs to be, so MkdirAll cannot succeed on any platform.
func TestUnwritableCacheDirectoryIsNotFatal(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})

	blocked := filepath.Dir(cachePath())
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := getUsage()
	if !r.ok {
		t.Fatalf("an unwritable cache directory broke the reading: reason=%q detail=%q", r.reason, r.detail)
	}
	if r.fromCache {
		t.Error("the reading claims to come from a cache that cannot exist")
	}
	if _, _, ok := readCache(); ok {
		t.Error("readCache returned something from an unwritable location")
	}
}

// A cache file we cannot make sense of is ignored, not repaired and not trusted. It is written
// by a previous version of this tool or by nothing at all, and acting on half-understood contents
// would mean reporting numbers whose provenance is unknown.
func TestUnusableCacheFileIsIgnored(t *testing.T) {
	cases := []struct{ name, body string }{
		{"not json", `{ this is not json`},
		{"a different schema version", `{"version":99,"fetched_at":"2026-08-10T12:00:00Z","buckets":[{"key":"five_hour"}]}`},
		{"no buckets", `{"version":1,"fetched_at":"2026-08-10T12:00:00Z","buckets":[]}`},
		{"empty file", ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateCache(t)
			if err := os.MkdirAll(filepath.Dir(cachePath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cachePath(), []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if r, age, ok := readCache(); ok {
				t.Errorf("readCache accepted %s: %+v (age %v)", c.name, r, age)
			}
			// And it must not be promoted into a stale fallback either, which is the path where
			// a bad cache entry would reach a caller labelled as real numbers.
			if got := staleOr(reading{reason: failTransport, detail: "x"}); got.ok || got.stale {
				t.Errorf("staleOr served an unusable cache entry: %+v", got)
			}
		})
	}
}

// With no home directory at all there is nowhere to look for either the cache or the
// credentials. Both have to degrade to "not found" rather than panicking or reading a relative
// path in the working directory.
func TestNoHomeDirectoryDegradesToNothingFound(t *testing.T) {
	isolateCache(t)
	stubKeychain(t, "", 1)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	if got := cachePath(); got != "" {
		t.Errorf("cachePath() = %q with no home directory, want empty", got)
	}
	if got := credentialsPath(); got != "" {
		t.Errorf("credentialsPath() = %q with no home directory, want empty", got)
	}
	writeCache(cachedSample()) // must be a silent no-op, not a panic
	if _, _, ok := readCache(); ok {
		t.Error("readCache found something with no home directory")
	}
	if _, ok := fromFile(); ok {
		t.Error("fromFile found something with no home directory")
	}

	d := diagnose()
	if d.file != "absent" || d.using != "none" {
		t.Errorf("diagnosis = %+v, want file absent and using none", d)
	}
	var out bytes.Buffer
	if code := runDoctor(&out); code != exitError {
		t.Errorf("runDoctor = %d, want %d", code, exitError)
	}
	// The location column cannot be empty: this output is comma-separated, so a blank cell
	// would shift the row.
	if !strings.Contains(out.String(), "file,unknown,absent") {
		t.Errorf("doctor should name the unknown location:\n%s", out.String())
	}
}

// A failed reading must not overwrite a good one. The stale-fallback path only has something to
// serve because of this.
func TestWriteCacheIgnoresFailedReadings(t *testing.T) {
	isolateCache(t)
	writeCache(cachedSample())
	before, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatal(err)
	}

	writeCache(reading{reason: failHTTP, status: 429})
	writeCache(reading{reason: failTransport, detail: "no route to host"})

	after, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatalf("a failed reading removed the cache file: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a failed reading rewrote the cache:\nbefore %s\nafter  %s", before, after)
	}
}
