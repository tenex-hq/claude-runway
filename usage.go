package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// nil when credits are absent, disabled or unreadable. Never a zero-valued stand-in.
	credits *credits

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

type limitEntry struct {
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type usagePayload struct {
	Limits []limitEntry `json:"limits"`
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
	severity := func(kind string) (string, bool) {
		for _, l := range meta.Limits {
			if l.Kind == kind {
				return l.Severity, true
			}
		}
		return "normal", false
	}

	var out []bucket
	// Label to index in out, plus whether that window's severity came from a limits entry of
	// its own, so a scoped entry for the same model can be merged rather than repeated.
	type seen struct {
		i        int
		sevFound bool
	}
	byLabel := map[string]seen{}
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
		sev, found := severity(kindByKey[key])
		b := bucket{key: key, severity: sev}
		if w.Utilization != nil {
			b.utilization, b.hasUtil = *w.Utilization, true
		}
		b.resetsAt = parseResetsAt(w.ResetsAt)
		byLabel[windowLabel[key]] = seen{len(out), found}
		out = append(out, b)
	}

	// Per-model weekly windows. The Fable window on /usage has no top-level key at all: it
	// exists only as a weekly_scoped entry in limits, so reading bucketKeys alone reported
	// session_5h and weekly while /usage showed a third meter that can stop work on its own.
	for _, l := range meta.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		name := scopedWindowName(l.Scope.Model.DisplayName)
		if name == "" {
			continue
		}
		label := "weekly_" + name
		if prev, dup := byLabel[label]; dup {
			p := &out[prev.i]
			if _, topLevel := windowLabel[p.key]; topLevel {
				// seven_day_opus already reports this model as weekly_opus, so a second row
				// would count one meter twice. Its severity is still worth taking: the
				// top-level key looks for a limits kind of weekly_opus, and when the only
				// entry for that model is weekly_scoped it would otherwise report the
				// "normal" fallback whatever the entry said.
				if !prev.sevFound && l.Severity != "" {
					p.severity = l.Severity
				}
				continue
			}
			// Two scoped entries resolving to one name (scope also has a surface field that
			// nothing here distinguishes). Keep the more consumed one: a gate shown the
			// looser of two windows with the same name would say safe while the other stops.
			if l.Percent != nil && (!p.hasUtil || *l.Percent > p.utilization) {
				*p = scopedBucket(label, l)
			}
			continue
		}
		// Same rule as an empty top-level window: no number and no reset is a row of
		// "unknown" for a limit that may not apply, which reads like a failed lookup.
		if l.Percent == nil && l.ResetsAt == "" {
			continue
		}
		byLabel[label] = seen{len(out), true}
		out = append(out, scopedBucket(label, l))
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

func parseResetsAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// scopedBucket keys the window by its output label. A scoped window has no endpoint field
// name to key it by, and the cache stores only the key, so the label is what survives a
// round trip through it.
func scopedBucket(label string, l limitEntry) bucket {
	b := bucket{key: label, severity: l.Severity, resetsAt: parseResetsAt(l.ResetsAt)}
	if l.Percent != nil {
		b.utilization, b.hasUtil = *l.Percent, true
	}
	return b
}

// scopedWindowName turns a model display name into the suffix of a window label: lowercase,
// with everything outside ASCII a-z and 0-9 replaced by "_" ("Fable" becomes "fable"). ASCII
// only on purpose, because the label lands unquoted in a comma-separated TOON row, where a
// comma or a space from a display name would shift every column after it.
func scopedWindowName(display string) string {
	display = strings.ToLower(strings.TrimSpace(display))
	var b strings.Builder
	for _, r := range display {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// --- credits ---------------------------------------------------------------------
// Usage credits are a paid reserve the user keeps for time-sensitive work, not budget an agent
// may spend. They are read so the user can see them, and kept out of the windows on purpose:
// as a row they would take part in verdict and tightest, and a reserve with 100% left would
// then be able to make an exhausted plan look safe.

// credits holds major-unit amounts. decimals is the currency exponent, kept so 21.50 renders
// as 21.50 and not 21.5.
type credits struct {
	used        float64
	limit       float64
	decimals    int
	currency    string
	percentLeft int
	severity    string
}

type moneyPayload struct {
	AmountMinor *float64 `json:"amount_minor"`
	Currency    string   `json:"currency"`
	Exponent    *int     `json:"exponent"`
}

type spendPayload struct {
	Used     *moneyPayload `json:"used"`
	Limit    *moneyPayload `json:"limit"`
	Severity string        `json:"severity"`
	Enabled  *bool         `json:"enabled"`
}

type extraUsagePayload struct {
	IsEnabled     *bool    `json:"is_enabled"`
	MonthlyLimit  *float64 `json:"monthly_limit"`
	UsedCredits   *float64 `json:"used_credits"`
	Currency      string   `json:"currency"`
	DecimalPlaces *int     `json:"decimal_places"`
}

// A currency exponent outside this range is not a currency, and 10^n of a garbage n would
// turn a minor amount into a number nobody should act on.
const maxCurrencyExponent = 6

// parseCredits returns nil when credits are absent, disabled, or not readable as a complete
// set of numbers. There is deliberately no partial result: a used amount without a limit, or
// either without a currency, would have to be filled in with a zero, and a zero here reads as
// "credits spent" when it means "could not tell".
//
// spend_limit_reached is ignored on purpose. It read false on a payload where used (21.50)
// already exceeded limit (20.00), so it is not an exhaustion signal. The used and limit
// amounts are.
func parseCredits(body []byte) *credits {
	var p struct {
		Spend      *spendPayload      `json:"spend"`
		ExtraUsage *extraUsagePayload `json:"extra_usage"`
	}
	// A field of the wrong type fails only that field, and the rest still decodes. Anything
	// unusable ends up nil and is handled below.
	_ = json.Unmarshal(body, &p)
	if c, definite := creditsFromSpend(p.Spend); definite {
		return c
	}
	return creditsFromExtraUsage(p.ExtraUsage)
}

// creditsFromSpend reports definite=false when spend cannot settle the question, so the
// caller falls back to extra_usage. An explicit enabled=false is definite: spend is the newer
// of the two shapes, and reviving credits it says are off from the older one would be wrong.
func creditsFromSpend(s *spendPayload) (*credits, bool) {
	if s == nil || s.Enabled == nil {
		return nil, false
	}
	if !*s.Enabled {
		return nil, true
	}
	if s.Used == nil || s.Limit == nil {
		return nil, false
	}
	used, usedDec, ok := minorToMajor(s.Used.AmountMinor, s.Used.Exponent)
	if !ok {
		return nil, false
	}
	limit, limitDec, ok := minorToMajor(s.Limit.AmountMinor, s.Limit.Exponent)
	if !ok {
		return nil, false
	}
	currency := s.Used.Currency
	if currency == "" {
		currency = s.Limit.Currency
	}
	if s.Limit.Currency != "" && s.Limit.Currency != currency {
		// A ratio of two different currencies is not a percentage of anything.
		return nil, false
	}
	c, ok := newCredits(used, limit, max(usedDec, limitDec), currency, s.Severity)
	return c, ok
}

func creditsFromExtraUsage(e *extraUsagePayload) *credits {
	if e == nil || e.IsEnabled == nil || !*e.IsEnabled {
		return nil
	}
	used, dec, ok := minorToMajor(e.UsedCredits, e.DecimalPlaces)
	if !ok {
		return nil
	}
	limit, _, ok := minorToMajor(e.MonthlyLimit, e.DecimalPlaces)
	if !ok {
		return nil
	}
	// extra_usage carries no severity, and deriving one here would be this tool's guess
	// presented as the endpoint's verdict.
	c, _ := newCredits(used, limit, dec, e.Currency, "")
	return c
}

func minorToMajor(amount *float64, exponent *int) (float64, int, bool) {
	if amount == nil || exponent == nil || *exponent < 0 || *exponent > maxCurrencyExponent {
		return 0, 0, false
	}
	return *amount / math.Pow10(*exponent), *exponent, true
}

func newCredits(used, limit float64, decimals int, currency, severity string) (*credits, bool) {
	// A zero limit could mean "no credits allowed" or "no cap", and nothing in the payload
	// says which, so it gets no percentage at all rather than a guessed one.
	if currency == "" || limit <= 0 || used < 0 {
		return nil, false
	}
	left, _ := percentLeft(used/limit*100, true)
	if severity == "" {
		severity = "unknown"
	}
	return &credits{used: used, limit: limit, decimals: decimals, currency: currency, percentLeft: left, severity: severity}, true
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
		credits:    parseCredits(r.body),
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
