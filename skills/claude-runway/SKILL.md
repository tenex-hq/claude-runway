---
name: claude-runway
description: Check how much of the Claude subscription allowance is left and whether it will last until the window resets, then decide whether to keep going. Use before starting long or expensive work, when running a loop that must stop before the budget runs out, and when the user asks about usage, quota, budget, remaining tokens, rate limits, or "am I about to run out".
user-invocable: true
---

# claude-runway

One command, and the answer is in the first three lines:

```bash
claude-runway --brief
```

```
source: live
verdict: safe
tightest: weekly
because: weekly is on-pace with 73% left
windows[2]{window,left_pct,resets_in,pace,headroom_pts}:
  session_5h,92,3h20m,well-ahead,+25
  weekly,73,5d4h,on-pace,-1
```

Use `--brief` when calling it programmatically. Plain `claude-runway` adds a discovery
preamble and next-step hints that are useful once and wasteful thereafter.

## Reading it correctly

**Percentages are REMAINING, not consumed.** `left_pct=73` means 73% of the allowance is
still available. This is the single easiest thing to get backwards, and getting it backwards
inverts every decision.

**Act on `verdict` first.** It already combines the windows, so you do not have to:

| verdict | Meaning | What to do |
|---|---|---|
| `safe` | Budget comfortably covers the time to reset | Proceed |
| `caution` | Under 20% left, or burning much faster than the clock | Finish current work, commit it, avoid starting anything large |
| `stop` | 5% or less left on some window | Stop, commit, and tell the user which window and when it resets |
| `not-applicable` | A custom provider is configured; there is no subscription window | Proceed with no budget gate at all |
| `unknown` | Windows returned without usable numbers | Do not guess. Treat as "no reading" |

`tightest` names the window that decided the verdict, and it is chosen by pace headroom
rather than by lowest percentage: a window with plenty of percent left but very little time
left is the one that actually runs dry.

**`pace` and `headroom_pts`** compare budget left against time left in the window. Positive
headroom means the remaining budget more than covers the remaining time, so you are safe to
push. Negative means you are burning faster than the clock.

## Gating long work

Check every few units of work, not every iteration. Readings are cached for 5 minutes, so a
frequent check is cheap, but there is no value in more resolution than that: the windows are
5 hours and 7 days.

When told to work "until the weekly budget is down to 45%", that means keep going while more
than 45% is **left**. Commit progress as you go, so nothing is lost if the budget or the
session ends mid-task.

## Three traps

**A 429 is not an exhausted budget.** The usage endpoint rate-limits reads independently of
the subscription. If you see `error:` mentioning HTTP 429, the meter refused to be read; the
allowance itself is unchanged and currently unknown. Wait about 2 minutes. Never treat it as
"out of budget", and never conclude the user is out of quota from a failed reading.

**A stale reading announces itself.** `source: cache-stale` plus `age:` and a
`warning: NOT LIVE` line means the live read failed and this is the last known value. Judge
whether an age of that size still supports the decision you are about to make. An age of
seconds usually does; an age of hours usually does not.

**Failure is never a number.** Any failed reading renders as `error:` with an explanation and
never as a percentage, so there is no way to mistake "could not find out" for "nothing left".
If you get an `error:`, you have no reading. Say so rather than assuming the worst or the
best.

## Other invocations

| Command | Use |
|---|---|
| `claude-runway --brief` | The gating check. Start here |
| `claude-runway --json` | When you need to parse it |
| `claude-runway --fields=window,left_pct,resets_at` | Absolute reset timestamps |
| `claude-runway doctor` | The reading looks wrong or credentials may be missing |

Exit codes: `0` a reading or a definite non-answer, `1` no reading could be taken, `2` a usage
error such as an unknown flag.

## Reporting to the user

Lead with the decision, not the table. "Weekly is at 73% with 5 days left, on pace, so I will
keep going" is useful. Pasting the raw output is not, unless they asked for the numbers.
