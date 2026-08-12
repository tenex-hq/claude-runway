package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The transport layer, driven against an httptest server on loopback.
//
// usageURL is a package-level var for exactly this reason, so none of these tests may run in
// parallel with each other: they mutate shared package state and shared environment. Every one
// of them saves and restores usageURL through t.Cleanup, redirects HOME and the cache
// directory, and plants its own credentials file, so nothing here reads the real machine's
// token, cache, keychain or network.

// plantedAuth is the fake access token these tests plant on disk. Distinctive on purpose: the
// security assertions work by grepping for it in argv, in output and in files, so it must not
// look like anything else that could appear there by accident.
const plantedAuth = "runway-fixture-0123456789-not-a-real-grant"

// netTestEnv isolates everything getUsage can read from the environment. The proxy variables
// are cleared as well: Go's transport skips a proxy for loopback but curl does not necessarily,
// and a developer machine with http_proxy set would otherwise send a test request off-box.
func netTestEnv(t *testing.T) {
	t.Helper()
	isolateCache(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "")
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "")
	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(v, "")
	}
	t.Setenv("no_proxy", "*")
	t.Setenv("NO_PROXY", "*")
}

// withUsageURL points the transport at a test server and puts the real endpoint back
// afterwards, so a failing test cannot leave the rest of the suite aimed at loopback.
func withUsageURL(t *testing.T, url string) {
	t.Helper()
	saved := usageURL
	t.Cleanup(func() { usageURL = saved })
	usageURL = url
}

// plantCredentialFile writes a credentials file under the redirected HOME. Call netTestEnv or
// isolateCache first, otherwise this would write into the real home directory.
func plantCredentialFile(t *testing.T, accessToken string, expiresAtMillis int64) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home directory to plant credentials in: %v", err)
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"subscriptionType":"max"}}`,
		accessToken, expiresAtMillis)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// recorder observes what the endpoint was actually asked. Guarded by a mutex because the
// handler runs on the server's goroutine and the assertions run on the test's; without it the
// race detector is entitled to complain, and would be right to.
type recorder struct {
	mu     sync.Mutex
	hits   int
	header http.Header
}

func (rec *recorder) note(r *http.Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.hits++
	rec.header = r.Header.Clone()
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.hits
}

func (rec *recorder) get(key string) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.header.Get(key)
}

func startUsageServer(t *testing.T, h func(*recorder, http.ResponseWriter, *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.note(r)
		h(rec, w, r)
	}))
	t.Cleanup(srv.Close)
	withUsageURL(t, srv.URL)
	return rec
}

func usagePayloadJSON(now time.Time) string {
	return fmt.Sprintf(`{
		"five_hour": {"utilization": 40, "resets_at": %q},
		"seven_day": {"utilization": 27, "resets_at": %q},
		"seven_day_opus": null,
		"limits": [{"kind": "session", "severity": "normal"}, {"kind": "weekly_all", "severity": "warning"}]
	}`, now.Add(2*time.Hour).UTC().Format(time.RFC3339), now.Add(5*24*time.Hour).UTC().Format(time.RFC3339))
}

// The whole point of the tool, end to end over a real socket: a 200 becomes a live reading
// with its provenance intact, and the reading is cached so a second call inside the TTL costs
// no request at all. That second property is what makes a per-iteration budget check safe, and
// until now nothing asserted it against an actual endpoint.
func TestLiveReadingCrossesTheWireAndIsThenServedFromCache(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})

	r := getUsage()
	if !r.ok {
		t.Fatalf("live read failed: reason=%q detail=%q", r.reason, r.detail)
	}
	if r.transport != "http" {
		t.Errorf("transport = %q, want http", r.transport)
	}
	if r.fromCache || r.stale {
		t.Errorf("a fresh read must not claim to come from cache: %+v", r)
	}
	if r.credSource != "file" || r.plan != "max" {
		t.Errorf("provenance lost: credSource=%q plan=%q", r.credSource, r.plan)
	}
	if len(r.buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (the empty seven_day_opus is dropped): %+v", len(r.buckets), r.buckets)
	}
	if r.buckets[0].key != "five_hour" || !r.buckets[0].hasUtil || r.buckets[0].utilization != 40 {
		t.Errorf("five_hour bucket = %+v", r.buckets[0])
	}
	if r.buckets[0].resetsAt.IsZero() {
		t.Error("five_hour reset time was not parsed")
	}
	// Severity arrives in a separate array keyed by `kind`, so the key-to-kind mapping is part
	// of the wire contract.
	if r.buckets[1].severity != "warning" {
		t.Errorf("seven_day severity = %q, want warning from limits[kind=weekly_all]", r.buckets[1].severity)
	}

	if _, err := os.Stat(cachePath()); err != nil {
		t.Fatalf("a successful live read must be cached, but %s is not there: %v", cachePath(), err)
	}
	second := getUsage()
	if !second.fromCache || second.stale {
		t.Errorf("second call inside the TTL should be a plain cache hit, got %+v", second)
	}
	if got := rec.count(); got != 1 {
		t.Errorf("endpoint was called %d times; a cached reading must cost zero requests", got)
	}
}

// The request has to carry the OAuth bearer and the beta opt-in, or the endpoint answers 401.
// Asserted against the constant rather than a copy of the literal, so the two cannot drift.
func TestRequestCarriesTheBearerAndTheBetaOptIn(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})
	if r := getUsage(); !r.ok {
		t.Fatalf("live read failed: reason=%q detail=%q", r.reason, r.detail)
	}
	if got, want := rec.get("Authorization"), "Bearer "+plantedAuth; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := rec.get("anthropic-beta"); got != betaHeader {
		t.Errorf("anthropic-beta = %q, want %q", got, betaHeader)
	}
}

// An explicit User-Agent, not Go's default "Go-http-client/2.x": whoever operates an
// undocumented endpoint should be able to tell this tool apart from a scraper. The exact string
// is deliberately not pinned, because it embeds the build's version (a dev build appends
// "+dirty"). The prefix is the part that identifies the tool.
func TestRequestIdentifiesTheToolInItsUserAgent(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})
	if r := getUsage(); !r.ok {
		t.Fatalf("live read failed: reason=%q detail=%q", r.reason, r.detail)
	}
	ua := rec.get("User-Agent")
	if ua == "" {
		t.Fatal("no User-Agent was sent")
	}
	if !strings.HasPrefix(ua, "claude-runway/") {
		t.Errorf("User-Agent = %q, want it to start with claude-runway/", ua)
	}
	if strings.Contains(ua, "Go-http-client") {
		t.Errorf("User-Agent = %q, which is Go's default rather than ours", ua)
	}
}

// A 429 from the meter has to arrive labelled as a 429 all the way through, because the wording
// keyed on it is the one thing standing between "I could not read the meter" and a caller
// concluding its subscription is gone. The wording itself is unit-tested; what was missing was
// that a real 429 response reaches it.
func TestRealRateLimitResponseReachesTheNonAlarmingWording(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
	})

	r := getUsage()
	if r.ok {
		t.Fatal("a 429 must not produce a reading")
	}
	if r.reason != failHTTP || r.status != http.StatusTooManyRequests {
		t.Fatalf("reason=%q status=%d, want %q/429", r.reason, r.status, failHTTP)
	}
	f := describeFailure(r)
	if !strings.Contains(f.message, "NOT your subscription allowance") {
		t.Errorf("429 reached the generic HTTP wording instead of its own: %s", f.message)
	}
	out := renderTOON(r, time.Now(), renderOpts{brief: true})
	for _, wrong := range []string{"0%", "exhausted your", "left_pct", "windows["} {
		if strings.Contains(out, wrong) {
			t.Errorf("429 output contains %q, which reads as an exhausted budget:\n%s", wrong, out)
		}
	}
}

// A stuck proxy answering with a page of HTML would otherwise produce a multi-kilobyte
// single-line error: unreadable in a terminal, useless in a log.
func TestServerErrorBodyIsClippedIntoTheMessage(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	long := strings.Repeat("gateway blew up. ", 400) // ~6.8 KB, far past maxDetailChars
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, long)
	})

	r := getUsage()
	if r.reason != failHTTP || r.status != http.StatusInternalServerError {
		t.Fatalf("reason=%q status=%d, want %q/500", r.reason, r.status, failHTTP)
	}
	// maxDetailChars plus the three dots that mark the truncation.
	if len(r.detail) > maxDetailChars+3 {
		t.Errorf("detail is %d bytes, want at most %d", len(r.detail), maxDetailChars+3)
	}
	if !strings.HasSuffix(r.detail, "...") {
		t.Errorf("a truncated detail must say it was truncated, got %q", r.detail)
	}
	if !strings.Contains(describeFailure(r).message, "gateway blew up") {
		t.Errorf("the clipped detail should still show what the endpoint said: %q", r.detail)
	}
}

// Two ways a body can be unusable, worded apart because they need different fixes.
func TestUnusableBodyIsReportedAsABadPayloadNotAsZero(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json at all", `<html>503 from a proxy</html>`, "response was not JSON"},
		// Deliberately out of alphabetical order: map iteration is randomised, so an unsorted
		// implementation produced different text on every run.
		{"json without any known window", `{"zeta":1,"alpha":2,"mid":3}`, "no known windows in response (keys: alpha mid zeta)"},
		// A known window whose value is the wrong type is dropped, not guessed at: a string
		// where an object belongs cannot yield a number, and inventing one would be worse.
		{"known window of the wrong type", `{"five_hour":"nope"}`, "no known windows in response (keys: five_hour)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			netTestEnv(t)
			plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
			startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, c.body)
			})
			r := getUsage()
			if r.ok {
				t.Fatal("an unusable body must not produce a reading")
			}
			if r.reason != failBadPayload {
				t.Fatalf("reason = %q, want %q", r.reason, failBadPayload)
			}
			if r.detail != c.want {
				t.Errorf("detail = %q, want %q", r.detail, c.want)
			}
			if _, _, ok := readCache(); ok {
				t.Error("a failed read must not write a cache entry")
			}
		})
	}
}

// Regression guard for map-iteration nondeterminism: the key list in the message has to be the
// same text every time, sorted, and bounded, because it is read out of logs and diffed.
func TestUnknownKeyListIsSortedBoundedAndStable(t *testing.T) {
	// 15 keys, inserted out of order, so both the sort and the cap are exercised.
	order := []string{"a09", "a15", "a01", "a12", "a04", "a13", "a02", "a11", "a06", "a03", "a14", "a08", "a05", "a10", "a07"}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%q:%d", k, 1))
	}
	body := []byte("{" + strings.Join(parts, ",") + "}")

	want := "a01 a02 a03 a04 a05 a06 a07 a08 a09 a10 a11 a12 (+3 more)"
	for i := 0; i < 50; i++ {
		_, keys, ok := parsePayload(body)
		if ok {
			t.Fatal("a payload with no known windows must fail")
		}
		if keys != want {
			t.Fatalf("iteration %d: keys = %q, want %q", i, keys, want)
		}
	}
	// The expectation above is 12 names plus the two words of "(+3 more)". Pinned against the
	// constant so a change to maxDetailKeys forces a look here instead of silently passing.
	if named := len(strings.Fields(want)) - 2; named != maxDetailKeys {
		t.Errorf("expectation lists %d key names but maxDetailKeys = %d", named, maxDetailKeys)
	}
}

// A body past the ceiling must be reported as too big, on both transports, rather than
// truncated at the limit and then parsed as though it were the whole document. Streamed from
// the handler rather than built as a literal, so the test costs no source and little memory.
//
// The padded-JSON case is the one that matters and the one a bare LimitReader(maxResponseBytes)
// got wrong: a small valid object followed by megabytes of whitespace still parses after the
// cut, so a truncating read accepts an oversized response in silence and reports numbers from
// a document it never saw the end of.
func TestOversizedBodyIsReportedAsTooBigOnBothTransports(t *testing.T) {
	// pad streams the given filler until the ceiling is comfortably passed.
	pad := func(w http.ResponseWriter, prefix, filler string) {
		if prefix != "" {
			_, _ = io.WriteString(w, prefix)
		}
		chunk := strings.Repeat(filler, (64<<10)/len(filler))
		for sent := 0; sent < maxResponseBytes+len(chunk); sent += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}
	cases := []struct {
		name    string
		prefix  func() string
		filler  string
		curl    bool
		explain string
	}{
		{"garbage", func() string { return "" }, "a", false, ""},
		{"valid json padded with whitespace", func() string { return usagePayloadJSON(time.Now()) }, " ", false,
			"a truncating read would have parsed the valid prefix and reported its numbers"},
		{"over curl", func() string { return usagePayloadJSON(time.Now()) }, " ", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.curl {
				requireCurl(t)
			}
			netTestEnv(t)
			plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
			prefix := c.prefix()
			startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
				pad(w, prefix, c.filler)
			})
			if c.curl {
				t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")
			}

			r := getUsage()
			if r.ok {
				t.Fatalf("a body past the %d byte ceiling produced a reading (%d buckets). %s",
					maxResponseBytes, len(r.buckets), c.explain)
			}
			if r.reason != failTransport {
				t.Errorf("reason = %q, want %q: too much data is a transport problem, not a shape problem", r.reason, failTransport)
			}
			if !strings.Contains(r.detail, fmt.Sprintf("exceeded %d bytes", maxResponseBytes)) {
				t.Errorf("detail = %q, want it to say the response was too big", r.detail)
			}
			if _, _, ok := readCache(); ok {
				t.Error("an oversized body must not be cached")
			}
			if len(r.detail) > maxDetailChars+3 {
				t.Errorf("the failure detail carried %d bytes of the oversized body", len(r.detail))
			}
		})
	}
}

// An endpoint that is not there, and one that hangs up mid-conversation, are both transport
// failures and both have to return rather than block: this tool exists to live inside a work
// loop, so a wedged read is worse than a reported one.
//
// The 15s deadline itself is NOT exercised here. Waiting for it would add 15s to a 3s suite,
// and it cannot be shortened from a test without editing non-test code. These two cases cover
// the error path it shares.
func TestUnreachableEndpointIsATransportFailure(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		netTestEnv(t)
		plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()
		withUsageURL(t, url)

		done := make(chan reading, 1)
		go func() { done <- getUsage() }()
		var r reading
		select {
		case r = <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("getUsage never returned against a closed port")
		}
		if r.ok || r.reason != failTransport {
			t.Fatalf("reason = %q, want %q; reading was %+v", r.reason, failTransport, r)
		}
		if r.detail == "" {
			t.Error("a transport failure must say what went wrong")
		}
	})

	t.Run("connection closed mid-response", func(t *testing.T) {
		netTestEnv(t)
		plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
		startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
			// Hijack and drop the connection: the client sees the peer go away rather than a
			// status code, which is the same shape of failure as a network that black-holes.
			// No t.Skip or t.Fatal here, both are illegal off the test's own goroutine; if
			// hijacking were unavailable the assertions below would fail loudly instead.
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		})
		r := getUsage()
		if r.ok || r.reason != failTransport {
			t.Fatalf("reason = %q, want %q; reading was %+v", r.reason, failTransport, r)
		}
	})
}

// A security property, so it is asserted by counting requests and not by reading the return
// value: with a custom provider configured there is no subscription window to report, and the
// subscription OAuth token must not be sent to api.anthropic.com or anywhere else.
func TestCustomProviderSendsNoRequestAtAll(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(time.Now()))
	})
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.invalid")

	r := getUsage()
	if got := rec.count(); got != 0 {
		t.Fatalf("the endpoint was contacted %d times with a custom provider configured", got)
	}
	if r.reason != failProvider {
		t.Errorf("reason = %q, want %q", r.reason, failProvider)
	}
	if describeFailure(r).exit != exitOK {
		t.Error("a custom provider is a definite answer, so callers must not be told to retry")
	}
}

// The same guarantee for an expired token: there is nothing to gain from sending a grant the
// endpoint will refuse, and the fix is local (refresh it), so no request goes out.
func TestExpiredTokenIsRefusedBeforeAnyRequest(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(-time.Hour).UnixMilli())
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(time.Now()))
	})

	r := getUsage()
	if got := rec.count(); got != 0 {
		t.Errorf("an expired token was still sent to the endpoint (%d requests)", got)
	}
	if r.reason != failExpired {
		t.Fatalf("reason = %q, want %q", r.reason, failExpired)
	}
	if !strings.Contains(describeFailure(r).help, "claude") {
		t.Error("an expired token should tell the caller how to refresh it")
	}
}

// --- curl -----------------------------------------------------------------------------

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this machine")
	}
}

// The fallback transport has to produce the same answer as the primary one, or switching to it
// would change the numbers a caller acts on.
func TestForcedCurlProducesTheSameReading(t *testing.T) {
	requireCurl(t)
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})
	t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

	r := getUsage()
	if !r.ok {
		t.Fatalf("curl read failed: reason=%q detail=%q", r.reason, r.detail)
	}
	if r.transport != "curl" {
		t.Errorf("transport = %q, want curl", r.transport)
	}
	if len(r.buckets) != 2 || !r.buckets[0].hasUtil || r.buckets[0].utilization != 40 {
		t.Errorf("curl produced different numbers than the payload carried: %+v", r.buckets)
	}
	// curl sends the headers from the config file, so the same wire contract applies.
	if got, want := rec.get("Authorization"), "Bearer "+plantedAuth; got != want {
		t.Errorf("curl Authorization = %q, want %q", got, want)
	}
	if got := rec.get("anthropic-beta"); got != betaHeader {
		t.Errorf("curl anthropic-beta = %q, want %q", got, betaHeader)
	}
	if !strings.HasPrefix(rec.get("User-Agent"), "claude-runway/") {
		t.Errorf("curl User-Agent = %q", rec.get("User-Agent"))
	}
}

// The reason the fallback exists: some networks refuse Go's TLS stack with a 403 that curl gets
// through. The retry is only worth having if it actually replaces the answer.
func TestForbiddenFallsBackToCurl(t *testing.T) {
	requireCurl(t)
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	rec := startUsageServer(t, func(rec *recorder, w http.ResponseWriter, _ *http.Request) {
		if rec.count() == 1 { // the first attempt, over net/http
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"message":"Request not allowed"}}`)
			return
		}
		fmt.Fprint(w, usagePayloadJSON(now))
	})

	r := getUsage()
	if !r.ok {
		t.Fatalf("the curl fallback did not rescue a 403: reason=%q detail=%q", r.reason, r.detail)
	}
	if r.transport != "curl" {
		t.Errorf("transport = %q, want curl: the answer came from the fallback and should say so", r.transport)
	}
	if rec.count() != 2 {
		t.Errorf("expected exactly two attempts, got %d", rec.count())
	}
}

// And the other half of the same decision: curl's answer is only kept when it did better.
// Otherwise the reported failure stays the honest HTTP 403 rather than becoming a confusing
// story about a subprocess the caller never asked for.
func TestForbiddenStaysForbiddenWhenCurlAlsoFails(t *testing.T) {
	requireCurl(t)
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"Request not allowed"}}`)
	})

	r := getUsage()
	if r.ok {
		t.Fatal("two 403s must not produce a reading")
	}
	if r.reason != failHTTP || r.status != http.StatusForbidden {
		t.Fatalf("reason=%q status=%d, want %q/403: the honest failure is the HTTP one", r.reason, r.status, failHTTP)
	}
	if !strings.Contains(r.detail, "Request not allowed") {
		t.Errorf("detail = %q, want the endpoint's own explanation", r.detail)
	}
	if strings.Contains(strings.ToLower(r.detail), "curl") {
		t.Errorf("detail = %q, which blames curl for a refusal that came from the endpoint", r.detail)
	}
}

// fakeCurl puts a script named curl first on PATH, so a test can decide exactly how the
// subprocess behaves. Hermetic: nothing here opens a socket.
func fakeCurl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubCurl installs a fake curl that records its own argv and its stdin, then answers with a
// canned body and the "\n<status>" trailer the real curl is asked to append.
//
// It is the only way to observe the argv of a subprocess this package spawns internally.
func stubCurl(t *testing.T, body string) (argvPath, stdinPath string) {
	t.Helper()
	dir := t.TempDir()
	argvPath = filepath.Join(dir, "argv")
	stdinPath = filepath.Join(dir, "stdin")
	t.Setenv("RUNWAY_TEST_ARGV", argvPath)
	t.Setenv("RUNWAY_TEST_STDIN", stdinPath)
	t.Setenv("RUNWAY_TEST_BODY", body)
	fakeCurl(t,
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$RUNWAY_TEST_ARGV\"; done\n"+
			"while IFS= read -r line; do printf '%s\\n' \"$line\" >> \"$RUNWAY_TEST_STDIN\"; done\n"+
			"printf '%s' \"$RUNWAY_TEST_BODY\"\n"+
			"printf '\\n200'\n")
	return argvPath, stdinPath
}

// A stated security property with nothing behind it until now: anyone on the machine can read
// another process's argv, so the token goes in on stdin as a curl config file instead.
//
// This asserts it structurally, from the far side of the exec: the token is present in what the
// process received on stdin and absent from every element of its argv. What it cannot prove is
// that the real curl treats the config the same way a stub does, which is why
// TestForcedCurlProducesTheSameReading exists alongside it.
func TestTokenReachesCurlOnStdinAndNeverInArgv(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	withUsageURL(t, "http://127.0.0.1:1/api/oauth/usage") // never dialled: the stub does not connect
	argvPath, stdinPath := stubCurl(t, usagePayloadJSON(time.Now()))
	t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

	r := getUsage()
	if !r.ok {
		t.Fatalf("stubbed curl read failed: reason=%q detail=%q", r.reason, r.detail)
	}
	if r.transport != "curl" {
		t.Errorf("transport = %q, want curl", r.transport)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("curl was never executed: %v", err)
	}
	if strings.Contains(string(argv), plantedAuth) {
		t.Errorf("the token is in curl's argv, where any local process can read it:\n%s", argv)
	}
	for _, want := range []string{"-K", "-"} { // config read from stdin
		if !strings.Contains(string(argv), want+"\n") {
			t.Errorf("argv does not contain %q, so the config is not coming from stdin:\n%s", want, argv)
		}
	}

	config, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("curl received nothing on stdin: %v", err)
	}
	for _, want := range []string{
		"Authorization: Bearer " + plantedAuth,
		"anthropic-beta: " + betaHeader,
		`user-agent = "claude-runway/`,
		fmt.Sprintf("max-time = %d", int(curlMaxTime.Seconds())),
		fmt.Sprintf("connect-timeout = %d", int(curlConnectTimeout.Seconds())),
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("curl config missing %q:\n%s", want, config)
		}
	}
	// The deadlines are what stop a black-holed network from wedging a work loop, so they must
	// land inside the context's own timeout rather than outside it.
	if curlMaxTime >= httpTimeout || curlConnectTimeout >= httpTimeout {
		t.Errorf("curl's deadlines (%v, %v) must be shorter than the context's %v, or the ordinary timeout arrives as a bare signal kill",
			curlMaxTime, curlConnectTimeout, httpTimeout)
	}
}

// curl writes its diagnostic to stderr, and throwing that away is what turned "could not
// resolve host" into a bare "exit status 6" that told the caller nothing actionable. A curl that
// says nothing still has to produce a definite failure, just a shorter one.
func TestCurlDiagnosticSurvivesIntoTheFailure(t *testing.T) {
	t.Run("with a diagnostic on stderr", func(t *testing.T) {
		netTestEnv(t)
		plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
		withUsageURL(t, "http://127.0.0.1:1/api/oauth/usage")
		fakeCurl(t, "printf 'curl: (6) Could not resolve host: api.anthropic.com\\n' >&2\nexit 6\n")
		t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

		r := getUsage()
		if r.reason != failTransport {
			t.Fatalf("reason = %q, want %q", r.reason, failTransport)
		}
		if !strings.Contains(r.detail, "Could not resolve host") {
			t.Errorf("detail = %q, want curl's own diagnostic in it", r.detail)
		}
		if strings.TrimSpace(r.detail) == "curl failed: exit status 6" {
			t.Error("the failure is a bare exit status again, with the diagnostic discarded")
		}
		if !strings.Contains(describeFailure(r).message, "Could not resolve host") {
			t.Errorf("the diagnostic did not reach the user-facing message: %s", describeFailure(r).message)
		}
	})

	t.Run("silent non-zero exit", func(t *testing.T) {
		netTestEnv(t)
		plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
		withUsageURL(t, "http://127.0.0.1:1/api/oauth/usage")
		fakeCurl(t, "exit 7\n")
		t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

		r := getUsage()
		if r.reason != failTransport {
			t.Fatalf("reason = %q, want %q", r.reason, failTransport)
		}
		if !strings.Contains(r.detail, "exit status 7") {
			t.Errorf("detail = %q, want the exit status when there is nothing else to report", r.detail)
		}
		if r.detail == "" || describeFailure(r).message == "" {
			t.Error("a silent curl failure still has to produce a definite message")
		}
	})
}

// curl is not guaranteed to be installed, and CLAUDE_RUNWAY_FORCE_CURL=1 on a machine without it
// has to fail as a plain transport error naming the problem, not as a panic or an empty reading.
func TestMissingCurlIsAReportedFailure(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	withUsageURL(t, "http://127.0.0.1:1/api/oauth/usage")
	t.Setenv("PATH", t.TempDir()) // an empty PATH: nothing at all is executable
	t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

	r := getUsage()
	if r.ok {
		t.Fatal("a reading appeared without any transport to fetch it")
	}
	if r.reason != failTransport {
		t.Errorf("reason = %q, want %q", r.reason, failTransport)
	}
	if !strings.Contains(r.detail, "curl") {
		t.Errorf("detail = %q, want it to name curl as the thing that could not run", r.detail)
	}
}

// No token means no request. There is nothing to send, and the fix is local (sign in), so the
// endpoint is never contacted.
func TestMissingCredentialsMeanNoRequest(t *testing.T) {
	netTestEnv(t) // an empty HOME: no credentials file
	stubKeychain(t, "", 1)
	rec := startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(time.Now()))
	})

	r := getUsage()
	if got := rec.count(); got != 0 {
		t.Errorf("the endpoint was contacted %d times with no credentials to send", got)
	}
	if r.reason != failNoCreds {
		t.Fatalf("reason = %q, want %q", r.reason, failNoCreds)
	}
	if describeFailure(r).exit != exitError {
		t.Error("no credentials is a runtime failure a human has to act on")
	}
	if !strings.Contains(describeFailure(r).help, "doctor") {
		t.Error("the fix should point at `claude-runway doctor`, which shows where it looked")
	}
}

// The status code arrives on the last line of curl's output, because -w appends it. Output that
// does not carry one cannot be interpreted, and guessing 200 would turn a failed request into a
// confident reading.
func TestCurlOutputWithoutAStatusLineIsAFailure(t *testing.T) {
	cases := []struct{ name, script string }{
		{"no trailing line at all", "printf 'body with no status'\n"},
		{"status line is not a number", "printf 'body\\nnot-a-status'\n"},
		{"nothing on stdout", "exit 0\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			netTestEnv(t)
			plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
			withUsageURL(t, "http://127.0.0.1:1/api/oauth/usage")
			fakeCurl(t, c.script)
			t.Setenv("CLAUDE_RUNWAY_FORCE_CURL", "1")

			r := getUsage()
			if r.ok {
				t.Fatalf("output with no usable status became a reading: %+v", r.buckets)
			}
			if r.reason != failTransport {
				t.Errorf("reason = %q, want %q", r.reason, failTransport)
			}
			if !strings.Contains(r.detail, "no status line") {
				t.Errorf("detail = %q, want it to name the missing status line", r.detail)
			}
		})
	}
}

// A window can arrive with a reset time and no number. It must render as unknown: silently
// becoming 0 would read as an exhausted budget, and becoming 100 would read as an untouched one.
// Driven from the wire, because that is where such a window comes from.
func TestWindowWithoutANumberRendersUnknownNotZero(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now().UTC().Truncate(time.Second)
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"five_hour": {"resets_at": %q}}`, now.Add(2*time.Hour).Format(time.RFC3339))
	})

	r := getUsage()
	if !r.ok {
		t.Fatalf("a window with a reset time but no number is still a reading: reason=%q detail=%q", r.reason, r.detail)
	}
	if len(r.buckets) != 1 || r.buckets[0].hasUtil {
		t.Fatalf("expected one bucket carrying no utilization, got %+v", r.buckets)
	}
	out := renderTOON(r, now, renderOpts{brief: true})
	if !strings.Contains(out, "session_5h,unknown,2h,unknown,unknown") {
		t.Errorf("a numberless window must render as unknown in every derived column:\n%s", out)
	}
	if !strings.Contains(out, "verdict: unknown") {
		t.Errorf("with nothing to judge, the verdict must say so rather than guess:\n%s", out)
	}
	if !strings.Contains(out, "windows without usable numbers") {
		t.Errorf("the reason for an unknown verdict must be stated:\n%s", out)
	}
}

// --- detail clipping ------------------------------------------------------------------

// Cutting at a byte offset lands inside a multi-byte rune sooner or later, and emitting a broken
// sequence into an error message corrupts whatever reads it. Nothing asserted the repair or the
// truncation marker.
func TestClipDetailNeverEmitsABrokenRune(t *testing.T) {
	// One ASCII byte then two-byte runes, so the cut at maxDetailChars falls on an odd offset
	// and therefore in the middle of a rune.
	s := "x" + strings.Repeat("é", 149)
	if len(s) <= maxDetailChars {
		t.Fatalf("fixture is only %d bytes; it must exceed maxDetailChars to truncate", len(s))
	}
	if utf8.ValidString(s[:maxDetailChars]) {
		t.Fatal("fixture does not actually cut mid-rune, so it proves nothing")
	}
	got := clipDetail(s)
	if !utf8.ValidString(got) {
		t.Errorf("clipDetail produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("clipDetail truncated without saying so: %q", got)
	}
	if len(got) > maxDetailChars+3 {
		t.Errorf("clipDetail returned %d bytes, want at most %d", len(got), maxDetailChars+3)
	}
}

func TestClipDetailLeavesShortDetailsAlone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  rate_limit_error\n", "rate_limit_error"},
		{"dial tcp 1.2.3.4:443: connect: no route to host", "dial tcp 1.2.3.4:443: connect: no route to host"},
	}
	for _, c := range cases {
		if got := clipDetail(c.in); got != c.want {
			t.Errorf("clipDetail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A detail that fits must not gain a truncation marker it did not earn.
	if got := clipDetail(strings.Repeat("a", maxDetailChars)); strings.HasSuffix(got, "...") {
		t.Errorf("a detail of exactly maxDetailChars was marked as truncated: %q", got)
	}
}
