package main

import (
	"fmt"
	"math"
	"time"
)

// Pure logic: turning the endpoint's "percent consumed" into the two things a caller
// actually wants, which are "how much is left" and "am I going to run dry early".

// Window lengths, so "how much time does this budget still have to cover" is well
// defined. Keyed by the endpoint's own field names.
var windowLen = map[string]time.Duration{
	"five_hour":      5 * time.Hour,
	"seven_day":      7 * 24 * time.Hour,
	"seven_day_opus": 7 * 24 * time.Hour,
}

// Short names used in output.
var windowLabel = map[string]string{
	"five_hour":      "session_5h",
	"seven_day":      "weekly",
	"seven_day_opus": "weekly_opus",
}

// percentLeft inverts utilization (a percent, 0..100) into percent remaining, clamped.
// ok is false when the endpoint gave us no number, which is reported as unknown rather
// than silently becoming 0 or 100.
func percentLeft(utilization float64, present bool) (int, bool) {
	if !present || math.IsNaN(utilization) {
		return 0, false
	}
	v := math.Round(100 - utilization)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return int(v), true
}

// Pace tones, ordered worst to best so they can be compared for the overall verdict.
type tone int

const (
	toneBad tone = iota
	toneWarn
	toneOK
	toneGood
)

type paceVerdict struct {
	headroomPts int
	label       string
	tone        tone
	timeLeft    time.Duration
}

// pace compares the fraction of budget left against the fraction of the window left.
// Positive headroom means more budget remains than time, so the budget comfortably
// covers the time to reset. Negative means burning faster than the clock.
//
// Labels are hyphenated single tokens on purpose: they sit in comma-separated TOON rows
// where a multi-word value costs tokens and invites quoting.
func pace(left int, leftOK bool, resetsAt time.Time, window time.Duration, now time.Time) (paceVerdict, bool) {
	if !leftOK || resetsAt.IsZero() || window <= 0 {
		return paceVerdict{}, false
	}
	timeLeft := resetsAt.Sub(now)
	if timeLeft < 0 {
		timeLeft = 0
	}
	timeLeftFrac := math.Min(1, float64(timeLeft)/float64(window))
	budgetLeftFrac := math.Min(1, math.Max(0, float64(left)/100))
	headroom := budgetLeftFrac - timeLeftFrac
	pts := int(math.Round(headroom * 100))
	switch {
	case headroom >= 0.15:
		return paceVerdict{pts, "well-ahead", toneGood, timeLeft}, true
	case headroom >= 0.03:
		return paceVerdict{pts, "ahead", toneGood, timeLeft}, true
	case headroom > -0.05:
		return paceVerdict{pts, "on-pace", toneOK, timeLeft}, true
	case headroom > -0.15:
		return paceVerdict{pts, "behind", toneWarn, timeLeft}, true
	default:
		return paceVerdict{pts, "burning-fast", toneBad, timeLeft}, true
	}
}

// fmtRelative renders a compact "time until": "3h34m", "45m", "5d4h", "now". Coarse on
// purpose (two units at most) and unspaced, which reads fine and costs fewer tokens.
func fmtRelative(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	min := int(math.Round(d.Minutes()))
	if min < 60 {
		return fmt.Sprintf("%dm", min)
	}
	h, m := min/60, min%60
	if h < 24 {
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	d2, rh := h/24, h%24
	if rh > 0 {
		return fmt.Sprintf("%dd%dh", d2, rh)
	}
	return fmt.Sprintf("%dd", d2)
}

// fmtAge is fmtRelative's counterpart for elapsed time. It needs second resolution
// because a cache age of "0m" would read as "live" when it is not.
func fmtAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmtRelative(d)
}

// --- the pre-computed aggregate ---------------------------------------------------
// An agent gating work has to combine two or three windows into one decision. Doing
// that here removes a round of reasoning (and the chance of getting it wrong) from
// every caller.

// Thresholds for the overall verdict, in percent remaining.
const (
	stopBelow    = 5
	cautionBelow = 20
)

type overall struct {
	verdict  string // stop | caution | safe
	tightest string // label of the window that decided it
	reason   string
}

// summarize picks the window that will bite first and turns it into a single verdict.
// "Tightest" is by pace headroom when known, because a window with plenty of percent
// left but very little time left is the one that runs dry: raw percentage alone hides
// that. Falls back to lowest percent remaining when no pace could be computed.
func summarize(rows []row) (overall, bool) {
	best := -1
	for i, r := range rows {
		if !r.leftOK {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := rows[best]
		if r.paceOK && b.paceOK {
			if r.pace.headroomPts < b.pace.headroomPts {
				best = i
			}
			continue
		}
		if r.paceOK != b.paceOK {
			// A window we can judge beats one we cannot.
			if r.paceOK {
				best = i
			}
			continue
		}
		if r.left < b.left {
			best = i
		}
	}
	if best < 0 {
		return overall{}, false
	}
	r := rows[best]
	o := overall{tightest: r.label}
	switch {
	case r.left <= stopBelow:
		o.verdict, o.reason = "stop", fmt.Sprintf("%s has %d%% left", r.label, r.left)
	case r.left <= cautionBelow:
		o.verdict, o.reason = "caution", fmt.Sprintf("%s has %d%% left", r.label, r.left)
	case r.paceOK && r.pace.tone == toneBad:
		o.verdict, o.reason = "caution", fmt.Sprintf("%s is %s (%+dpts budget-vs-clock)", r.label, r.pace.label, r.pace.headroomPts)
	default:
		o.verdict = "safe"
		if r.paceOK {
			o.reason = fmt.Sprintf("%s is %s with %d%% left", r.label, r.pace.label, r.left)
		} else {
			o.reason = fmt.Sprintf("%s has %d%% left", r.label, r.left)
		}
	}
	return o, true
}
