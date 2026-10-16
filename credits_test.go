package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Per-model weekly windows from limits[kind=weekly_scoped], and usage credits from spend or
// extra_usage. Fixtures follow the payload shape in issue #6, trimmed to the keys that matter.

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func bucketByKey(bs []bucket, key string) (bucket, bool) {
	for _, b := range bs {
		if b.key == key {
			return b, true
		}
	}
	return bucket{}, false
}

// The Fable window has no top-level key of its own. It has to become a row anyway, named from
// the model, with remaining percent, its own reset, a pace against seven days, and severity.
func TestScopedWeeklyWindowBecomesARow(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
		"five_hour": {"utilization": 20, "resets_at": %q},
		"seven_day": {"utilization": 73, "resets_at": %q},
		"seven_day_opus": null,
		"limits": [
			{"kind": "session", "percent": 20, "severity": "normal", "resets_at": %q, "scope": null},
			{"kind": "weekly_all", "percent": 73, "severity": "normal", "resets_at": %q, "scope": null},
			{"kind": "weekly_scoped", "percent": 30, "severity": "warning", "resets_at": %q,
			 "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
		]
	}`, ts(now.Add(2*time.Hour)), ts(now.Add(4*24*time.Hour)), ts(now.Add(2*time.Hour)),
		ts(now.Add(4*24*time.Hour)), ts(now.Add(3*24*time.Hour)))

	buckets, _, ok := parsePayload([]byte(body))
	if !ok {
		t.Fatal("parsePayload rejected a payload with a scoped window")
	}
	if len(buckets) != 3 {
		t.Fatalf("got %d windows, want session, weekly and weekly_fable: %+v", len(buckets), buckets)
	}
	rows := buildRows(reading{ok: true, buckets: buckets}, now)
	fable := rows[2]
	if fable.label != "weekly_fable" {
		t.Fatalf("scoped window label = %q, want weekly_fable", fable.label)
	}
	if !fable.leftOK || fable.left != 70 {
		t.Errorf("weekly_fable left = %d (ok %v), want 70: percent is consumed, the row is remaining", fable.left, fable.leftOK)
	}
	if fable.severity != "warning" {
		t.Errorf("weekly_fable severity = %q, want the entry's own warning", fable.severity)
	}
	if !fable.resetsAt.Equal(now.Add(3 * 24 * time.Hour)) {
		t.Errorf("weekly_fable resets_at = %v, want its own reset", fable.resetsAt)
	}
	// 70% budget left against 3 of 7 days left is +27 points. Against a 5h window, or with no
	// period at all, this would be unknown or wildly different.
	if !fable.paceOK || fable.pace.headroomPts != 27 {
		t.Errorf("weekly_fable pace = %+v (ok %v), want +27pts judged against seven days", fable.pace, fable.paceOK)
	}
}

// Running a model's weekly window out stops work on that model, so the scoped row takes part
// in verdict and tightest like any other window.
func TestScopedWindowCanDecideTheVerdict(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
		"five_hour": {"utilization": 10, "resets_at": %q},
		"seven_day": {"utilization": 40, "resets_at": %q},
		"limits": [{"kind": "weekly_scoped", "percent": 97, "severity": "critical", "resets_at": %q,
			"scope": {"model": {"display_name": "Fable"}}}]
	}`, ts(now.Add(2*time.Hour)), ts(now.Add(4*24*time.Hour)), ts(now.Add(2*24*time.Hour)))
	buckets, _, ok := parsePayload([]byte(body))
	if !ok {
		t.Fatal("parse failed")
	}
	sum, ok := summarize(buildRows(reading{ok: true, buckets: buckets}, now))
	if !ok || sum.verdict != "stop" || sum.tightest != "weekly_fable" {
		t.Errorf("summary = %+v, want stop on weekly_fable at 3%% left", sum)
	}
}

// seven_day_opus already reports Opus as weekly_opus. A weekly_scoped entry for Opus is the same
// meter, and two rows for it would count one limit twice.
func TestScopedOpusNextToSevenDayOpusIsNotDuplicated(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	scoped := fmt.Sprintf(`{"kind": "weekly_scoped", "percent": 55, "severity": "warning", "resets_at": %q,
		"scope": {"model": {"display_name": "Opus"}}}`, ts(now.Add(3*24*time.Hour)))

	t.Run("top-level window present", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"seven_day": {"utilization": 40, "resets_at": %q},
			"seven_day_opus": {"utilization": 50, "resets_at": %q},
			"limits": [%s]
		}`, ts(now.Add(3*24*time.Hour)), ts(now.Add(3*24*time.Hour)), scoped)
		buckets, _, ok := parsePayload([]byte(body))
		if !ok {
			t.Fatal("parse failed")
		}
		rows := buildRows(reading{ok: true, buckets: buckets}, now)
		var opus []row
		for _, r := range rows {
			if r.label == "weekly_opus" {
				opus = append(opus, r)
			}
		}
		if len(opus) != 1 {
			t.Fatalf("weekly_opus appears %d times, want once: %+v", len(opus), rows)
		}
		if opus[0].key != "seven_day_opus" || opus[0].left != 50 {
			t.Errorf("weekly_opus = %+v, want the top-level window's numbers (50 left)", opus[0])
		}
		// No limits entry of kind weekly_opus exists here, so the scoped entry is the only
		// severity the endpoint gave for this model.
		if opus[0].severity != "warning" {
			t.Errorf("weekly_opus severity = %q, want warning taken from the scoped entry", opus[0].severity)
		}
	})

	t.Run("top-level window null", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"seven_day": {"utilization": 40, "resets_at": %q},
			"seven_day_opus": null,
			"limits": [%s]
		}`, ts(now.Add(3*24*time.Hour)), scoped)
		buckets, _, ok := parsePayload([]byte(body))
		if !ok {
			t.Fatal("parse failed")
		}
		b, found := bucketByKey(buckets, "weekly_opus")
		if !found || !b.hasUtil || b.utilization != 55 {
			t.Errorf("with seven_day_opus null the scoped entry must report weekly_opus itself, got %+v", buckets)
		}
	})
}

func TestScopedWindowNameIsSafeInATOONRow(t *testing.T) {
	for in, want := range map[string]string{
		"Fable":            "fable",
		"  Opus ":          "opus",
		"Claude Fable 5.1": "claude_fable_5_1",
		"a,b":              "a_b",
		"Mödel":            "m_del",
		"":                 "",
	} {
		if got := scopedWindowName(in); got != want {
			t.Errorf("scopedWindowName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A scoped entry with no model name cannot be labelled, and one with neither a number nor a
// reset carries nothing. Both are dropped rather than shown as a row of unknowns.
func TestUnusableScopedEntriesAreDropped(t *testing.T) {
	body := `{
		"seven_day": {"utilization": 40, "resets_at": "2026-10-20T00:00:00Z"},
		"limits": [
			{"kind": "weekly_scoped", "percent": 10, "scope": null},
			{"kind": "weekly_scoped", "percent": 10, "scope": {"model": {"display_name": null}}},
			{"kind": "weekly_scoped", "percent": null, "resets_at": null, "scope": {"model": {"display_name": "Fable"}}}
		]
	}`
	buckets, _, ok := parsePayload([]byte(body))
	if !ok {
		t.Fatal("parse failed")
	}
	if len(buckets) != 1 {
		t.Errorf("got %d windows, want only weekly: %+v", len(buckets), buckets)
	}
}

// --- credits --------------------------------------------------------------------

const spendOverLimit = `"spend": {
	"used":  {"amount_minor": 2150, "currency": "EUR", "exponent": 2},
	"limit": {"amount_minor": 2000, "currency": "EUR", "exponent": 2},
	"percent": 100, "severity": "critical", "enabled": true
}`

const extraUsageEnabled = `"extra_usage": {
	"is_enabled": true, "monthly_limit": 2000, "used_credits": 512.0,
	"utilization": 25.6, "currency": "USD", "decimal_places": 2, "spend_limit_reached": false
}`

func payloadWith(parts ...string) []byte {
	all := append([]string{`"five_hour": {"utilization": 20, "resets_at": "2026-10-16T17:00:00Z"}`}, parts...)
	return []byte("{" + strings.Join(all, ",") + "}")
}

func TestCreditsOverTheLimit(t *testing.T) {
	// spend_limit_reached reads false here, as it did on the real payload with used > limit.
	c := parseCredits(payloadWith(spendOverLimit, `"extra_usage": {"is_enabled": true, "spend_limit_reached": false}`))
	if c == nil {
		t.Fatal("credits over the limit were dropped")
	}
	if c.used != 21.5 || c.limit != 20 || c.currency != "EUR" || c.decimals != 2 {
		t.Errorf("credits = %+v, want 21.50 of 20.00 EUR", c)
	}
	if c.percentLeft != 0 {
		t.Errorf("percent_left = %d, want 0: over the limit clamps, it does not go negative", c.percentLeft)
	}
	if c.severity != "critical" {
		t.Errorf("severity = %q, want spend's critical", c.severity)
	}
}

func TestCreditsDisabled(t *testing.T) {
	cases := map[string][]byte{
		// spend is the newer shape, so its "off" is not overruled by an older extra_usage.
		"spend disabled": payloadWith(`"spend": {"used": {"amount_minor": 0, "currency": "EUR", "exponent": 2},
			"limit": {"amount_minor": 2000, "currency": "EUR", "exponent": 2}, "enabled": false}`, extraUsageEnabled),
		"extra_usage disabled": payloadWith(`"extra_usage": {"is_enabled": false, "monthly_limit": 2000,
			"used_credits": 0, "currency": "EUR", "decimal_places": 2}`),
	}
	for name, body := range cases {
		if c := parseCredits(body); c != nil {
			t.Errorf("%s: credits reported as %+v, want none", name, c)
		}
	}
}

func TestCreditsFallBackToExtraUsageWhenSpendIsMissing(t *testing.T) {
	c := parseCredits(payloadWith(extraUsageEnabled))
	if c == nil {
		t.Fatal("extra_usage was not used as a fallback")
	}
	if c.used != 5.12 || c.limit != 20 || c.currency != "USD" {
		t.Errorf("credits = %+v, want 5.12 of 20.00 USD (minor units over decimal_places)", c)
	}
	if c.percentLeft != 74 {
		t.Errorf("percent_left = %d, want 74", c.percentLeft)
	}
	// extra_usage has no severity field; the tool does not make one up.
	if c.severity != "unknown" {
		t.Errorf("severity = %q, want unknown", c.severity)
	}

	// A spend object that cannot settle the question is treated as missing, not as zero.
	incomplete := `"spend": {"used": {"amount_minor": 100, "currency": "EUR"}, "limit": {"amount_minor": 2000, "currency": "EUR", "exponent": 2}, "enabled": true}`
	if c := parseCredits(payloadWith(incomplete, extraUsageEnabled)); c == nil || c.currency != "USD" {
		t.Errorf("spend without an exponent should fall back to extra_usage, got %+v", c)
	}
}

func TestCreditsAbsentWhenNeitherSourceIsPresent(t *testing.T) {
	if c := parseCredits(payloadWith()); c != nil {
		t.Errorf("credits invented from nothing: %+v", c)
	}
	// Enabled but missing a number, a currency or a usable limit is the same as absent: any
	// of them would otherwise have to be emitted as a zero that means "unknown".
	for _, part := range []string{
		`"extra_usage": {"is_enabled": true, "monthly_limit": 2000, "currency": "EUR", "decimal_places": 2}`,
		`"extra_usage": {"is_enabled": true, "monthly_limit": 2000, "used_credits": 5, "decimal_places": 2}`,
		`"extra_usage": {"is_enabled": true, "monthly_limit": 0, "used_credits": 5, "currency": "EUR", "decimal_places": 2}`,
		`"extra_usage": {"is_enabled": true, "monthly_limit": 2000, "used_credits": 5, "currency": "EUR"}`,
		`"spend": {"used": {"amount_minor": 1, "currency": "EUR", "exponent": 2}, "limit": {"amount_minor": 2000, "currency": "USD", "exponent": 2}, "enabled": true}`,
	} {
		if c := parseCredits(payloadWith(part)); c != nil {
			t.Errorf("incomplete credits reported as %+v from %s", c, part)
		}
	}
}

// --- rendering ------------------------------------------------------------------

func readingWithCredits(now time.Time, c *credits) reading {
	r := sampleReading(now)
	r.credits = c
	return r
}

var overLimit = &credits{used: 21.5, limit: 20, decimals: 2, currency: "EUR", percentLeft: 0, severity: "critical"}

// The key names are a contract for callers parsing --json, so the exact set is pinned, and the
// amounts keep the currency's decimal places.
func TestJSONCreditsObjectShape(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	out := renderJSON(readingWithCredits(now, overLimit), now)

	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &generic); err != nil {
		t.Fatalf("JSON does not parse: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(generic["credits"], &obj); err != nil {
		t.Fatalf("credits is not an object: %v\n%s", err, out)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"currency", "enabled", "limit", "percent_left", "severity", "used"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("credits keys = %v, want exactly %v", keys, want)
	}
	if obj["enabled"] != true || obj["used"] != 21.5 || obj["limit"] != 20.0 ||
		obj["currency"] != "EUR" || obj["percent_left"] != 0.0 || obj["severity"] != "critical" {
		t.Errorf("credits values = %v", obj)
	}
	if !strings.Contains(out, `"used": 21.50`) || !strings.Contains(out, `"limit": 20.00`) {
		t.Errorf("amounts lost their decimal places:\n%s", out)
	}
}

func TestNoCreditsMeansNoCreditsKeyAndNoLine(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if out := renderJSON(sampleReading(now), now); strings.Contains(out, `"credits"`) {
		t.Errorf("credits key emitted with nothing to report:\n%s", out)
	}
	if out := renderTOON(sampleReading(now), now, renderOpts{brief: true}); strings.Contains(out, "credits") {
		t.Errorf("credits line emitted with nothing to report:\n%s", out)
	}
}

// The brief line is read by agents that never saw the docs, so it has to say on its own that
// credits are a reserve and not part of the verdict.
func TestBriefCreditsLineStatesItIsAReserve(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	out := renderTOON(readingWithCredits(now, overLimit), now, renderOpts{brief: true})
	want := "credits: reserve, 0% left (21.50 of 20.00 EUR used), not budget and not part of the verdict\n"
	if !strings.Contains(out, want) {
		t.Fatalf("brief output missing %q\n---\n%s", want, out)
	}
	if strings.Index(out, "because: ") > strings.Index(out, "credits: ") {
		t.Errorf("credits line must follow because:\n%s", out)
	}
	if strings.Count(out, "credits:") != 1 {
		t.Errorf("want exactly one credits line\n---\n%s", out)
	}
}

// Credits are a reserve the user keeps for time-sensitive work. They must never move the
// verdict, so a window at stop is a stop with the reserve full, spent, or absent.
func TestVerdictIgnoresCredits(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	base := reading{
		ok: true,
		buckets: []bucket{
			{key: "five_hour", utilization: 30, hasUtil: true, resetsAt: now.Add(2 * time.Hour), severity: "normal"},
			{key: "seven_day", utilization: 98, hasUtil: true, resetsAt: now.Add(2 * 24 * time.Hour), severity: "critical"},
		},
		fetchedAt: now,
	}
	full := &credits{used: 0, limit: 20, decimals: 2, currency: "EUR", percentLeft: 100, severity: "normal"}

	var verdicts []jsonOut
	for _, c := range []*credits{nil, full, overLimit} {
		r := base
		r.credits = c
		var j jsonOut
		if err := json.Unmarshal([]byte(renderJSON(r, now)), &j); err != nil {
			t.Fatal(err)
		}
		verdicts = append(verdicts, j)

		toon := renderTOON(r, now, renderOpts{brief: true})
		if !strings.Contains(toon, "verdict: stop\ntightest: weekly\nbecause: weekly has 2% left\n") {
			t.Errorf("credits %+v changed the TOON verdict:\n%s", c, toon)
		}
	}
	for i, j := range verdicts {
		if j.Verdict != "stop" || j.Tightest != "weekly" {
			t.Errorf("case %d: verdict %q tightest %q, want stop on weekly", i, j.Verdict, j.Tightest)
		}
		if j.Verdict != verdicts[0].Verdict || j.Tightest != verdicts[0].Tightest || j.Because != verdicts[0].Because {
			t.Errorf("case %d: verdict differs from the no-credits case: %+v vs %+v", i, j, verdicts[0])
		}
		if strings.Contains(strings.ToLower(j.Because), "credit") {
			t.Errorf("because mentions credits: %q", j.Because)
		}
	}
}

// --- cache ----------------------------------------------------------------------

func TestCacheRoundTripsCreditsAndScopedWindows(t *testing.T) {
	isolateCache(t)
	now := time.Now()
	live := reading{
		ok: true,
		buckets: []bucket{
			{key: "seven_day", utilization: 40, hasUtil: true, resetsAt: now.Add(3 * 24 * time.Hour), severity: "normal"},
			{key: "weekly_fable", utilization: 30, hasUtil: true, resetsAt: now.Add(3 * 24 * time.Hour), severity: "warning"},
		},
		fetchedAt: now,
		credits:   overLimit,
	}
	writeCache(live)
	got, _, ok := readCache()
	if !ok {
		t.Fatal("cache did not read back")
	}
	if got.credits == nil || *got.credits != *overLimit {
		t.Errorf("credits after round trip = %+v, want %+v", got.credits, overLimit)
	}
	// The scoped window survives as its label, and is still paced against seven days, which
	// is only possible because the period resolves from the key alone.
	rows := buildRows(got, now)
	if len(rows) != 2 || rows[1].label != "weekly_fable" || !rows[1].paceOK || rows[1].pace.headroomPts != 27 {
		t.Errorf("scoped window after round trip = %+v", rows)
	}

	raw, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version":2`) {
		t.Errorf("cache written without the bumped version:\n%s", raw)
	}
}

// A version 1 file has no credits. Read as current, it would report "no credits" for up to
// six hours on a plan that has them, so it is ignored, even when fresh.
func TestVersionOneCacheIsIgnored(t *testing.T) {
	isolateCache(t)
	body := fmt.Sprintf(`{"version":1,"fetched_at":%q,"buckets":[{"key":"five_hour","utilization":10,"resets_at":%q,"severity":"normal"}]}`,
		time.Now().Add(-time.Minute).Format(time.RFC3339Nano), time.Now().Add(time.Hour).Format(time.RFC3339Nano))
	if err := os.MkdirAll(filepath.Dir(cachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _, ok := readCache(); ok {
		t.Errorf("a fresh version 1 cache file was accepted: %+v", r)
	}
}

// --- end to end -----------------------------------------------------------------

func fullPayloadJSON(now time.Time) string {
	return fmt.Sprintf(`{
		"five_hour": {"utilization": 20, "resets_at": %q},
		"seven_day": {"utilization": 73, "resets_at": %q},
		"seven_day_opus": null,
		"limits": [
			{"kind": "session", "percent": 20, "severity": "normal", "resets_at": %q, "scope": null},
			{"kind": "weekly_all", "percent": 73, "severity": "normal", "resets_at": %q, "scope": null},
			{"kind": "weekly_scoped", "percent": 0, "severity": "normal", "resets_at": %q,
			 "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
		],
		%s,
		%s
	}`, ts(now.Add(2*time.Hour)), ts(now.Add(4*24*time.Hour)), ts(now.Add(2*time.Hour)),
		ts(now.Add(4*24*time.Hour)), ts(now.Add(4*24*time.Hour)), extraUsageEnabled, spendOverLimit)
}

// Over the wire and through the CLI: --json carries the credits object and the scoped window,
// and the second call, served from cache, carries the same.
func TestJSONOverTheWireCarriesCreditsAndScopedWindow(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fullPayloadJSON(now))
	})
	for _, pass := range []string{"live", "cache"} {
		code, stdout, stderr := runCLI(t, "--json")
		if code != exitOK || stderr != "" {
			t.Fatalf("%s: exit %d stderr %q", pass, code, stderr)
		}
		var j jsonOut
		if err := json.Unmarshal([]byte(stdout), &j); err != nil {
			t.Fatalf("%s: %v\n%s", pass, err, stdout)
		}
		if j.Source != pass {
			t.Errorf("source = %q, want %s", j.Source, pass)
		}
		if j.Credits == nil || j.Credits.Used != "21.50" || j.Credits.PercentLeft != 0 || j.Credits.Currency != "EUR" {
			t.Errorf("%s: credits = %+v, want spend's 21.50 of 20.00 EUR", pass, j.Credits)
		}
		var names []string
		for _, w := range j.Windows {
			names = append(names, w.Window)
		}
		if !reflect.DeepEqual(names, []string{"session_5h", "weekly", "weekly_fable"}) {
			t.Errorf("%s: windows = %v", pass, names)
		}
	}
}

func TestMCPToolCallCarriesTheCreditsLine(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fullPayloadJSON(now))
	})
	var stdout, stderr bytes.Buffer
	in := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"check_usage"}}` + "\n"
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("serveMCP = %d", code)
	}
	var res struct {
		Result struct {
			Content []toolContent `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || len(res.Result.Content) != 1 {
		t.Fatalf("bad response: %v\n%s", err, stdout.String())
	}
	text := res.Result.Content[0].Text
	for _, want := range []string{
		"credits: reserve, 0% left (21.50 of 20.00 EUR used), not budget and not part of the verdict",
		"weekly_fable,100,",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("tool content missing %q:\n%s", want, text)
		}
	}
}

// Nothing an agent reads before deciding may present credits as somewhere to go once a window
// is out. The tool description is read on every MCP session, the help text on discovery.
func TestNoSurfaceSuggestsFallingBackToCredits(t *testing.T) {
	for name, text := range map[string]string{"mcp description": checkUsageDescription, "help": helpText} {
		// Whitespace folded, because the help text is wrapped at a fixed width.
		lower := strings.Join(strings.Fields(strings.ToLower(text)), " ")
		if !strings.Contains(lower, "not part of the verdict") || !strings.Contains(lower, "stop means stop") {
			t.Errorf("%s does not say credits are not part of the verdict and stop means stop", name)
		}
		for _, wrong := range []string{"continue on credits", "fall back to credits", "falls back to credits", "use credits"} {
			if strings.Contains(lower, wrong) {
				t.Errorf("%s suggests %q", name, wrong)
			}
		}
	}
}

// The shipped skill is where an agent learns how to read the credits line next to a stop, so
// the rule has to be in it, not only in the README.
func TestEmbeddedSkillSaysStopMeansStopWhateverCreditsShow(t *testing.T) {
	body, err := skillFS.ReadFile("skills/claude-runway/SKILL.md")
	if err != nil {
		t.Fatalf("skill not embedded: %v", err)
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	for _, want := range []string{"may not spend them on your own", "`stop` means stop", "explicitly authorized"} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill should say %q about credits", want)
		}
	}
}
