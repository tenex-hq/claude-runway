package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Reads current subscription usage from the OAuth usage endpoint: the same numbers
// behind claude.ai's "Current session" and "Weekly" meters and Claude Code's own /usage.
//
// The endpoint is UNDOCUMENTED. Every failure is returned as a typed reason instead of
// being thrown or logged, because a usage reading is advisory: a caller gating work on it
// must be able to tell "40% left" apart from "I could not find out", and the difference
// must never look like an exhausted budget.

const (
	betaHeader  = "oauth-2025-04-20"
	httpTimeout = 15 * time.Second
	// curl gets a deadline just inside the context's, so the ordinary timeout failure is
	// curl's own "(28) Operation timed out" (which arrives on stderr with a diagnostic)
	// rather than a context kill, which reports nothing but "signal: killed".
	curlMaxTime        = 13 * time.Second
	curlConnectTimeout = 5 * time.Second
	// Both transports cap the body at the same number, as a named constant so they cannot
	// drift apart again: viaCurl used to read an undocumented endpoint's response into
	// memory unbounded while viaHTTP was already capped. The real payload is a few KB.
	maxResponseBytes = 4 << 20
	// Every free-form detail that reaches an error message is clipped to this. A stuck proxy
	// answering with a page of HTML would otherwise produce a multi-kilobyte single-line
	// error: unreadable in a terminal, useless in a log.
	maxDetailChars = 200
	// And a wrong-shape response gets at most this many of its key names listed.
	maxDetailKeys = 12
)

// usageURL is a var and not a const FOR TESTABILITY. As a const the transport layer was
// structurally unreachable from a test, which is why viaHTTP, viaCurl and getUsage sat at 0%
// coverage while the pure logic was at 80 to 97%; a test needs to point this at an
// httptest.Server.
//
// It is NOT user configurable and must never be wired to an environment variable.
// ANTHROPIC_BASE_URL already means something different and incompatible ("a custom provider
// is in use, so send the token nowhere and report not-applicable"), and repurposing it to
// move this request to another host would ship a subscription OAuth token to a third party.
var usageURL = "https://api.anthropic.com/api/oauth/usage"

// An explicit User-Agent on both transports, in place of Go's default "Go-http-client/2.x".
// It is polite, and it lets whoever operates an undocumented endpoint tell this tool's
// traffic apart from a scraper if they ever want to. Package variables initialise in
// dependency order, so reportedVersion (main.go) is resolved before this reads it.
var userAgent = "claude-runway/" + reportedVersion

// clipDetail bounds anything we did not author before it goes into an error message, and
// trims it, so failures stay one readable line. Cutting at a byte offset can land inside a
// multi-byte rune, so the tail is repaired rather than emitted as a broken sequence.
func clipDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetailChars {
		return strings.TrimSpace(strings.ToValidUTF8(s[:maxDetailChars], "")) + "..."
	}
	return s
}

// Windows on the response, in the order we report them.
var bucketKeys = []string{"five_hour", "seven_day", "seven_day_opus"}

// Where severity lives: the response's normalized `limits` array, keyed by `kind`.
var kindByKey = map[string]string{
	"five_hour":      "session",
	"seven_day":      "weekly_all",
	"seven_day_opus": "weekly_opus",
}

type failure string

const (
	failNone       failure = ""
	failProvider   failure = "custom-provider"
	failNoCreds    failure = "no-credentials"
	failExpired    failure = "expired-credentials"
	failHTTP       failure = "http-error"
	failTransport  failure = "transport-error"
	failBadPayload failure = "bad-response"
)

type bucket struct {
	key         string
	utilization float64
	hasUtil     bool
	resetsAt    time.Time
	severity    string
}

type reading struct {
	ok      bool
	buckets []bucket

	transport  string // "http" | "curl"
	credSource string
	plan       string
	fetchedAt  time.Time

	// Set when the numbers came from the on-disk cache rather than the network. `stale`
	// distinguishes the two reasons that happens: a deliberate hit inside the TTL, versus a
	// live fetch that failed and left us serving the last thing we knew.
	fromCache bool
	age       time.Duration
	stale     bool

	reason failure
	detail string
	status int
}

// customProviderBaseURL: a third-party provider has no subscription window, so the whole
// question is moot and the OAuth token must never be sent anywhere. Detected the way the
// Claude Code CLI itself picks up a custom endpoint.
func customProviderBaseURL() string {
	return strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
}

// --- transport -------------------------------------------------------------------
// Go's TLS stack is not undici's, so the 403 "Request not allowed" that Node-based
// clients hit on some networks may or may not apply here. Verified working over Go's
// net/http on the machine this was written on; the curl fallback stays as a cheap
// insurance policy for networks or edge behaviour we have not seen.

type httpResult struct {
	status int
	body   []byte
	err    error
}

func viaHTTP(token string) httpResult {
	req, err := http.NewRequest(http.MethodGet, usageURL, nil)
	if err != nil {
		return httpResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: httpTimeout}
	res, err := client.Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer res.Body.Close()
	// One byte past the ceiling, so an oversized body is detectable instead of being silently
	// truncated at exactly the limit and then reported downstream as "response was not JSON".
	// viaCurl does the same and the two have to agree: one condition surfacing under two
	// different reasons is how a caller ends up debugging the wrong thing. A bare
	// LimitReader(maxResponseBytes) here was that bug.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return httpResult{err: err}
	}
	if len(body) > maxResponseBytes {
		return httpResult{err: fmt.Errorf("response exceeded %d bytes", maxResponseBytes)}
	}
	return httpResult{status: res.StatusCode, body: body}
}

func viaCurl(token string) httpResult {
	// Two independent deadlines, because they fail in different ways and neither covers the
	// other: the context kill catches a curl that hangs before or beyond its own limit (a
	// wedged DNS resolver, a process stopped in the middle of a TLS handshake), and curl's
	// --max-time catches the same wall-clock overrun while still exiting normally with a
	// readable diagnostic. Without either, CLAUDE_RUNWAY_FORCE_CURL=1 against a black-holed
	// network blocked forever, inside a tool whose whole job is to not block a work loop.
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	// The token goes in on stdin as a curl config file, never in argv, so it cannot be
	// read out of the process table by anyone else on the machine.
	config := fmt.Sprintf("url = %q\nheader = %q\nheader = %q\nuser-agent = %q\nmax-time = %d\nconnect-timeout = %d\n",
		usageURL, "Authorization: Bearer "+token, "anthropic-beta: "+betaHeader, userAgent,
		int(curlMaxTime.Seconds()), int(curlConnectTimeout.Seconds()))
	cmd := exec.CommandContext(ctx, "curl", "-sS", "-K", "-", "-w", "\n%{http_code}")
	cmd.Stdin = strings.NewReader(config)

	// Piped rather than cmd.Output(), because Output() reads the whole body into memory with
	// no ceiling and this path has to honour maxResponseBytes like viaHTTP does. curl's own
	// --max-filesize is not a substitute: it only fires when the server declares a
	// Content-Length. Cost of the switch is that (*exec.ExitError).Stderr stays empty, so
	// stderr is captured here instead. -sS was passed precisely so curl would write a
	// diagnostic there, and throwing it away is what turned "could not resolve host" into a
	// bare "exit status 6".
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return httpResult{err: fmt.Errorf("curl failed: %w", err)}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return httpResult{err: fmt.Errorf("curl failed to start: %w", err)}
	}
	// One byte past the ceiling, so an oversized body is detectable rather than silently
	// parsed as a truncated document.
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxResponseBytes+1))
	oversize := len(out) > maxResponseBytes
	if oversize {
		// Kill instead of draining a body already decided against. Leaving the pipe unread
		// would block Wait until the context fired.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	switch {
	case ctx.Err() != nil:
		// Named explicitly: the process was killed, so waitErr alone would read "signal:
		// killed" and say nothing about why.
		return httpResult{err: fmt.Errorf("curl timed out after %s", httpTimeout)}
	case oversize:
		return httpResult{err: fmt.Errorf("curl response exceeded %d bytes", maxResponseBytes)}
	case readErr != nil:
		return httpResult{err: fmt.Errorf("curl output unreadable: %w", readErr)}
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if d := clipDetail(stderr.String()); d != "" {
				return httpResult{err: fmt.Errorf("curl failed: %w (%s)", waitErr, d)}
			}
		}
		return httpResult{err: fmt.Errorf("curl failed: %w", waitErr)}
	}

	cut := strings.LastIndexByte(string(out), '\n')
	if cut < 0 {
		return httpResult{err: fmt.Errorf("curl produced no status line")}
	}
	var status int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out[cut+1:])), "%d", &status); err != nil {
		return httpResult{err: fmt.Errorf("curl produced no status line")}
	}
	return httpResult{status: status, body: out[:cut]}
}

// --- parsing ---------------------------------------------------------------------

type usagePayload struct {
	Limits []struct {
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
	} `json:"limits"`
	// Windows are decoded generically so an added window does not require a code change
	// to be visible, and so a changed shape degrades to "unknown" instead of a crash.
	Raw map[string]json.RawMessage `json:"-"`
}

type windowPayload struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

func parsePayload(body []byte) ([]bucket, string, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false
	}
	var meta usagePayload
	_ = json.Unmarshal(body, &meta) // limits are a bonus; absence is not an error
	severity := func(kind string) string {
		for _, l := range meta.Limits {
			if l.Kind == kind {
				return l.Severity
			}
		}
		return "normal"
	}

	var out []bucket
	for _, key := range bucketKeys {
		rawWindow, present := raw[key]
		if !present {
			continue
		}
		var w windowPayload
		if err := json.Unmarshal(rawWindow, &w); err != nil {
			continue
		}
		// A window can be present but carry no data at all: seven_day_opus comes back this
		// way on plans that do not meter Opus separately. Reporting it as a row of
		// "unknown" is worse than omitting it, because it reads like a failed lookup of a
		// limit that actually applies.
		if w.Utilization == nil && w.ResetsAt == "" {
			continue
		}
		b := bucket{key: key, severity: severity(kindByKey[key])}
		if w.Utilization != nil {
			b.utilization, b.hasUtil = *w.Utilization, true
		}
		if w.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
				b.resetsAt = t
			}
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		// Sorted because these key names end up in an error message: map order is randomised,
		// so the same broken response produced different text on every run, which is
		// undiffable in a log and unassertable in a test. Counted and clipped because the
		// body is whatever an undocumented endpoint decided to send.
		sort.Strings(keys)
		more := ""
		if len(keys) > maxDetailKeys {
			more = fmt.Sprintf(" (+%d more)", len(keys)-maxDetailKeys)
			keys = keys[:maxDetailKeys]
		}
		return nil, clipDetail(strings.Join(keys, " ")) + more, false
	}
	return out, "", true
}

// --- public ----------------------------------------------------------------------

func getUsage() reading {
	if base := customProviderBaseURL(); base != "" {
		return reading{reason: failProvider, detail: base}
	}
	// A recent reading is served without touching the network at all. This is what makes a
	// per-iteration budget check safe: without it, a loop earns a 429 from the endpoint.
	if cached, age, ok := readCache(); ok && age <= cacheTTLFromEnv() {
		cached.fromCache, cached.age = true, age
		return cached
	}
	creds, ok := readCredentials()
	if !ok {
		return staleOr(reading{reason: failNoCreds})
	}
	if creds.expired {
		return staleOr(reading{reason: failExpired, detail: creds.expiresAt.UTC().Format(time.RFC3339)})
	}

	forceCurl := os.Getenv("CLAUDE_RUNWAY_FORCE_CURL") == "1"
	transport := "http"
	r := httpResult{}
	if forceCurl {
		transport, r = "curl", viaCurl(creds.token)
	} else {
		r = viaHTTP(creds.token)
		// Retry over curl only when the first attempt was refused outright or never got
		// a response, and only keep curl's answer if it actually did better. Otherwise
		// the reported failure stays the honest one.
		if r.err != nil || r.status == http.StatusForbidden {
			if alt := viaCurl(creds.token); alt.status == http.StatusOK {
				transport, r = "curl", alt
			}
		}
	}

	switch {
	case r.err != nil:
		return staleOr(reading{reason: failTransport, detail: r.err.Error()})
	case r.status != http.StatusOK:
		return staleOr(reading{reason: failHTTP, status: r.status, detail: clipDetail(string(r.body))})
	}

	buckets, keys, ok := parsePayload(r.body)
	if !ok {
		detail := "response was not JSON"
		if keys != "" {
			detail = "no known windows in response (keys: " + keys + ")"
		}
		return staleOr(reading{reason: failBadPayload, detail: detail})
	}
	live := reading{
		ok:         true,
		buckets:    buckets,
		transport:  transport,
		credSource: creds.source,
		plan:       creds.plan,
		fetchedAt:  time.Now(),
	}
	writeCache(live)
	return live
}

// staleOr degrades a failed live read to the last cached reading, labelled with its age,
// because a number a caller can judge the freshness of beats no number at all. The original
// failure is kept in `reason` so the output can say why it is not live.
func staleOr(f reading) reading {
	cached, age, ok := readCache()
	if !ok {
		return f
	}
	cached.fromCache, cached.age, cached.stale = true, age, true
	cached.reason, cached.detail, cached.status = f.reason, f.detail, f.status
	return cached
}
