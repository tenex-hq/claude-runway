package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Output follows the AXI principles (https://axi.md): TOON on stdout, a pre-computed
// aggregate so the caller does not have to combine windows itself, a minimal default
// field set, definitive non-empty answers, and errors as structured stdout rather than
// stderr noise. One renderer serves the CLI and the MCP tool so the two cannot drift.

// row is a bucket with the derived numbers already worked out, which is what every
// renderer and the aggregate want.
type row struct {
	key      string
	label    string
	left     int
	leftOK   bool
	pace     paceVerdict
	paceOK   bool
	resetsAt time.Time
	severity string
	util     float64
	utilOK   bool
}

func buildRows(r reading, now time.Time) []row {
	rows := make([]row, 0, len(r.buckets))
	for _, b := range r.buckets {
		label := windowLabel[b.key]
		if label == "" {
			label = b.key
		}
		left, leftOK := percentLeft(b.utilization, b.hasUtil)
		window := windowLen[b.key]
		if window == 0 {
			window = windowLen["seven_day"]
		}
		p, pOK := pace(left, leftOK, b.resetsAt, window, now)
		rows = append(rows, row{
			key: b.key, label: label, left: left, leftOK: leftOK,
			pace: p, paceOK: pOK, resetsAt: b.resetsAt,
			severity: b.severity, util: b.utilization, utilOK: b.hasUtil,
		})
	}
	return rows
}

// --- fields (AXI principle 2: minimal default schema, more on request) -------------

var defaultFields = []string{"window", "left_pct", "resets_in", "pace", "headroom_pts"}

// Everything a caller can ask for via --fields. Kept small and flat; there is no deep
// structure here worth exposing.
var fieldRenderers = map[string]func(row, time.Time) string{
	"window":   func(r row, _ time.Time) string { return r.label },
	"left_pct": func(r row, _ time.Time) string { return intOrUnknown(r.left, r.leftOK) },
	"resets_in": func(r row, now time.Time) string {
		if r.resetsAt.IsZero() {
			return "unknown"
		}
		return fmtRelative(r.resetsAt.Sub(now))
	},
	"resets_at": func(r row, _ time.Time) string {
		if r.resetsAt.IsZero() {
			return "unknown"
		}
		return r.resetsAt.UTC().Format(time.RFC3339)
	},
	"pace": func(r row, _ time.Time) string {
		if !r.paceOK {
			return "unknown"
		}
		return r.pace.label
	},
	"headroom_pts": func(r row, _ time.Time) string {
		if !r.paceOK {
			return "unknown"
		}
		return fmt.Sprintf("%+d", r.pace.headroomPts)
	},
	"severity": func(r row, _ time.Time) string {
		if r.severity == "" {
			return "unknown"
		}
		return r.severity
	},
	"utilization_pct": func(r row, _ time.Time) string {
		if !r.utilOK {
			return "unknown"
		}
		return fmt.Sprintf("%g", r.util)
	},
}

func knownFields() []string {
	out := make([]string, 0, len(fieldRenderers))
	for k := range fieldRenderers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intOrUnknown(v int, ok bool) string {
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d", v)
}

// --- TOON --------------------------------------------------------------------------

type renderOpts struct {
	fields []string
	brief  bool   // drop the discovery preamble and help lines: for repeated calls in a loop
	bin    string // executable path, for the home view
}

// renderTOON never returns an empty string: an agent cannot tell "no output" apart from
// "the command died", so every path says something definite.
func renderTOON(r reading, now time.Time, o renderOpts) string {
	var b strings.Builder
	if !o.brief {
		if o.bin != "" {
			fmt.Fprintf(&b, "bin: %s\n", o.bin)
		}
		fmt.Fprintf(&b, "description: %s\n", description)
	}

	if !r.ok {
		f := describeFailure(r)
		fmt.Fprintf(&b, "%s: %s\n", f.key, f.message)
		if f.help != "" {
			fmt.Fprintf(&b, "help: %s\n", f.help)
		}
		return b.String()
	}

	// Provenance before the numbers. A reading served from cache after a failed live read is
	// still useful, but only if nobody can mistake it for current, so the warning goes first
	// where it cannot be skimmed past.
	if r.stale {
		fmt.Fprintf(&b, "warning: NOT LIVE - the live read failed (%s). Showing the last known reading, taken %s ago.\n",
			shortReason(r), fmtAge(r.age))
	}
	switch {
	case r.stale:
		fmt.Fprintf(&b, "source: cache-stale\nage: %s\n", fmtAge(r.age))
	case r.fromCache:
		fmt.Fprintf(&b, "source: cache\nage: %s\n", fmtAge(r.age))
	default:
		b.WriteString("source: live\n")
	}

	rows := buildRows(r, now)
	if sum, ok := summarize(rows); ok {
		fmt.Fprintf(&b, "verdict: %s\ntightest: %s\nbecause: %s\n", sum.verdict, sum.tightest, sum.reason)
	} else {
		// Windows came back but none carried a number we could use. Say so plainly rather
		// than printing a table of "unknown" with no verdict.
		b.WriteString("verdict: unknown\nbecause: the endpoint returned windows without usable numbers\n")
	}

	fields := o.fields
	if len(fields) == 0 {
		fields = defaultFields
	}
	fmt.Fprintf(&b, "windows[%d]{%s}:\n", len(rows), strings.Join(fields, ","))
	for _, r := range rows {
		cells := make([]string, 0, len(fields))
		for _, f := range fields {
			cells = append(cells, fieldRenderers[f](r, now))
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(cells, ","))
	}

	if !o.brief {
		b.WriteString("help[2]:\n")
		b.WriteString("  Run `claude-runway --brief` for just the verdict and windows, for repeated calls in a loop\n")
		b.WriteString("  Run `claude-runway --fields=window,left_pct,resets_at` to pick columns; see --help for the full list\n")
	}
	return b.String()
}

// --- failures ----------------------------------------------------------------------

type failureText struct {
	key     string // "error" for something that went wrong, "verdict" for a definite answer
	message string
	help    string
	exit    int
}

// shortReason is the one-clause version used inside the stale-cache warning.
func shortReason(r reading) string {
	switch r.reason {
	case failNoCreds:
		return "no credentials found"
	case failExpired:
		return "the stored token has expired"
	case failHTTP:
		if r.status == 429 {
			return "the usage endpoint rate-limited this tool, HTTP 429"
		}
		return fmt.Sprintf("HTTP %d from the usage endpoint", r.status)
	case failTransport:
		return "the endpoint was unreachable"
	case failBadPayload:
		return "the endpoint returned an unexpected shape"
	default:
		return "unknown reason"
	}
}

// Each failure says what is and is not known. A caller gating spend must never mistake
// "could not read" for "nothing left", so no path here ever emits a percentage.
func describeFailure(r reading) failureText {
	// The endpoint rate-limits this tool independently of your subscription, and the two are
	// dangerously easy to confuse: a caller that reads "429" as "budget exhausted" would stop
	// working for no reason. So it gets its own wording that says which limit was hit.
	if r.reason == failHTTP && r.status == 429 {
		return failureText{
			key:     "error",
			message: "the usage endpoint rate-limited this tool (HTTP 429). This is a limit on reading the meter, NOT your subscription allowance: your actual budget is unchanged and currently unknown.",
			help:    "wait a moment and re-run. Do not poll in a tight loop; readings are cached for " + cacheTTLFromEnv().String() + " so a per-iteration check is already cheap.",
			exit:    exitError,
		}
	}
	switch r.reason {
	case failProvider:
		// Not a failure at all: it is the correct, final answer. Exit 0 so a script
		// proceeds instead of retrying a number that will never exist.
		return failureText{
			key:     "verdict",
			message: "not-applicable",
			help:    fmt.Sprintf("ANTHROPIC_BASE_URL=%s is set, so agents here run through a custom provider rather than a Claude subscription. There is no allowance window to report; proceed without a budget gate.", r.detail),
			exit:    0,
		}
	case failNoCreds:
		return failureText{key: "error", message: "no Claude Code credentials found on this machine",
			help: "sign in with `claude`, then re-run. `claude-runway doctor` shows exactly where it looked.", exit: 1}
	case failExpired:
		return failureText{key: "error", message: "the stored Claude Code OAuth token expired at " + r.detail,
			help: "run any `claude` command to refresh it, then re-run. This tool never writes credentials itself.", exit: 1}
	case failHTTP:
		msg := fmt.Sprintf("the usage endpoint returned HTTP %d", r.status)
		if r.detail != "" {
			msg += " (" + r.detail + ")"
		}
		return failureText{key: "error", message: msg,
			help: "if this persists, the undocumented endpoint may have changed. Try CLAUDE_RUNWAY_FORCE_CURL=1.", exit: 1}
	case failTransport:
		return failureText{key: "error", message: "could not reach the usage endpoint (" + r.detail + ")",
			help: "check network access to api.anthropic.com, then re-run.", exit: 1}
	case failBadPayload:
		return failureText{key: "error", message: "the usage endpoint returned something unexpected (" + r.detail + ")",
			help: "the endpoint is undocumented and may have changed shape.", exit: 1}
	default:
		return failureText{key: "error", message: "usage could not be read", exit: 1}
	}
}

// --- JSON ---------------------------------------------------------------------------
// For hosts that embed this rather than read it: autodev, scripts, anything that wants
// to keep its own state on top.

type jsonWindow struct {
	Window      string  `json:"window"`
	PercentLeft *int    `json:"percent_left"`
	ResetsAt    *string `json:"resets_at"`
	ResetsIn    *string `json:"resets_in"`
	Severity    string  `json:"severity,omitempty"`
	Pace        *struct {
		Label       string `json:"label"`
		HeadroomPts int    `json:"headroom_pts"`
	} `json:"pace"`
}

type jsonOut struct {
	Status    string       `json:"status"`
	Reason    string       `json:"reason,omitempty"`
	Message   string       `json:"message,omitempty"`
	Help      string       `json:"help,omitempty"`
	Verdict   string       `json:"verdict,omitempty"`
	Tightest  string       `json:"tightest,omitempty"`
	Because   string       `json:"because,omitempty"`
	Source    string       `json:"source,omitempty"` // live | cache | cache-stale
	AgeSecs   int          `json:"age_seconds,omitempty"`
	Warning   string       `json:"warning,omitempty"`
	FetchedAt string       `json:"fetched_at,omitempty"`
	Transport string       `json:"transport,omitempty"`
	CredFrom  string       `json:"credential_source,omitempty"`
	Plan      string       `json:"plan,omitempty"`
	Windows   []jsonWindow `json:"windows,omitempty"`
}

func renderJSON(r reading, now time.Time) string {
	var out jsonOut
	if !r.ok {
		f := describeFailure(r)
		out = jsonOut{Status: "error", Reason: string(r.reason), Message: f.message, Help: f.help}
		if r.reason == failProvider {
			out.Status, out.Verdict = "not-applicable", "not-applicable"
		}
	} else {
		out = jsonOut{
			Status:    "ok",
			FetchedAt: r.fetchedAt.UTC().Format(time.RFC3339),
			Transport: r.transport,
			CredFrom:  r.credSource,
			Plan:      r.plan,
			Source:    "live",
		}
		if r.fromCache {
			out.Source, out.AgeSecs = "cache", int(r.age.Seconds())
		}
		if r.stale {
			out.Source = "cache-stale"
			out.Warning = fmt.Sprintf("not live: the live read failed (%s); this reading is %s old", shortReason(r), fmtAge(r.age))
		}
		rows := buildRows(r, now)
		if sum, ok := summarize(rows); ok {
			out.Verdict, out.Tightest, out.Because = sum.verdict, sum.tightest, sum.reason
		} else {
			out.Verdict = "unknown"
		}
		for _, w := range rows {
			jw := jsonWindow{Window: w.label, Severity: w.severity}
			if w.leftOK {
				v := w.left
				jw.PercentLeft = &v
			}
			if !w.resetsAt.IsZero() {
				s := w.resetsAt.UTC().Format(time.RFC3339)
				jw.ResetsAt = &s
				rel := fmtRelative(w.resetsAt.Sub(now))
				jw.ResetsIn = &rel
			}
			if w.paceOK {
				jw.Pace = &struct {
					Label       string `json:"label"`
					HeadroomPts int    `json:"headroom_pts"`
				}{w.pace.label, w.pace.headroomPts}
			}
			out.Windows = append(out.Windows, jw)
		}
	}
	// Indented: this is read by humans debugging and by hosts parsing, and the size
	// difference is irrelevant next to a network round trip.
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return `{"status":"error","reason":"internal","message":"could not encode output"}`
	}
	return string(buf)
}
