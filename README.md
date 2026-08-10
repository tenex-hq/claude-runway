# claude-runway

How much of your Claude subscription allowance is **left**, and whether it will last to the
reset. A single static Go binary, no dependencies, no runtime to install.

Usable three ways: as a CLI, as an MCP server, and as a Go package.

```console
$ claude-runway --brief
source: live
verdict: safe
tightest: weekly
because: weekly is on-pace with 73% left
windows[2]{window,left_pct,resets_in,pace,headroom_pts}:
  session_5h,92,3h20m,well-ahead,+25
  weekly,73,5d4h,on-pace,-1
```

Percentages are **remaining**: 100 means untouched, 0 means exhausted. That direction is
deliberate, because "45%" left and "45%" used are the same string and opposite instructions.

## Why it exists

Claude Code's own `/usage` renders for the human. An agent cannot call it. This exposes the
same numbers so a model can pace its own spending, which is the whole point.

It also answers the question the raw numbers do not. "73% left" and "resets in 5 days" are
two facts; whether you are about to run dry is a third, and that is what `verdict` is.

## Install

```bash
go install github.com/tenex-hq/claude-runway@latest
```

Or build for wherever you need it. No cgo, so the binary is static:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w"
```

As an MCP server in Claude Code:

```bash
claude mcp add runway -- claude-runway mcp
```

The CLI is the cheaper integration and the recommended one. An MCP server's tool schema sits
in the context window of every turn, whether or not it gets used; a CLI costs tokens only
when called. The `mcp` subcommand exists for hosts that already speak MCP and would otherwise
have to shell out.

## Commands

| Command | What it does |
|---|---|
| `claude-runway` | Home view: provenance, verdict, windows, then next-step hints |
| `claude-runway --brief` | Just provenance, verdict and windows. Use this in loops |
| `claude-runway --json` | JSON, for hosts and scripts |
| `claude-runway --fields=a,b,c` | Pick columns |
| `claude-runway doctor` | Where credentials were found. Never prints the token |
| `claude-runway mcp` | Serve MCP over stdio. One tool: `check_usage` |

Fields: `window`, `left_pct`, `resets_in`, `resets_at`, `pace`, `headroom_pts`, `severity`,
`utilization_pct`. The default five are the ones a decision actually needs.

## The verdict

- **pace** per window compares the fraction of budget left against the fraction of the
  window left. Positive headroom means the remaining budget comfortably covers the remaining
  time, so you are safe to push. Negative means you are burning faster than the clock.
- **verdict** (`safe`, `caution`, `stop`) combines the windows so the caller does not have to.
  `tightest` names the window that decided it.

`tightest` is chosen by pace headroom, not by lowest percentage. A window with plenty of
percent left but very little time left is the one that runs dry, and a raw percentage hides
that.

## Caching, and why it is not optional

The usage endpoint **rate-limits reads with HTTP 429**. This was measured, not guessed: the
tool earned one while being built. Since the intended caller is an agent checking its budget
inside a work loop, an uncached tool would poison the thing it reports on. So:

- A reading younger than 60s is served from cache without touching the network.
- When a live read fails, the last reading is served with its age stated, rather than
  nothing. A number labelled "16s old" beats "unavailable"; the label is what keeps it honest.

```console
$ claude-runway --brief          # after a 429, with a cached reading available
warning: NOT LIVE - the live read failed (the usage endpoint rate-limited this tool, HTTP 429). Showing the last known reading, taken 16s ago.
source: cache-stale
age: 16s
verdict: safe
...
```

A 429 also gets its own wording, because "429" read as "budget exhausted" would make an agent
stop working for no reason. The message says explicitly that the limit hit was on reading the
meter, not on the subscription.

The cache lives in the user cache dir (`~/Library/Caches/claude-runway` on macOS), mode 0600,
holds only derived numbers, and is written atomically. **No token is ever written to it**, and
a test asserts that.

## How it works

1. Reads the OAuth access token Claude Code already stores, from
   `~/.claude/.credentials.json` or, on macOS, the Keychain item `Claude Code-credentials`.
   No API key. Read-only: this never writes credentials back, so it cannot lock you out of
   Claude Code.
2. Calls `GET https://api.anthropic.com/api/oauth/usage`. The response carries `five_hour`,
   `seven_day` and sometimes `seven_day_opus`, each with `utilization` (0..100) and
   `resets_at`.
3. Uses Go's `net/http`, with a fallback to the system `curl`. Node-based clients are
   reported to hit `403 "Request not allowed"` from TLS fingerprinting; Go's client is not
   affected on the networks this was tested on, and both paths were verified to return 200.
   The fallback is cheap insurance for edges we have not seen. `CLAUDE_RUNWAY_FORCE_CURL=1`
   pins it to curl.

The token reaches curl through a config file on **stdin**, never in argv, so it cannot be read
out of the process table.

### Caveats

- **The endpoint is undocumented.** It can change or vanish. Every failure returns an
  explanation instead of a number, so a caller can always tell "40% left" from "I could not
  find out". Nothing panics.
- **Custom providers.** If `ANTHROPIC_BASE_URL` is set there is no subscription window, so the
  tool reports `verdict: not-applicable`, sends no token anywhere, and exits 0. A caller is
  told to proceed without a budget gate rather than to keep polling a number that will never
  exist.
- **Expired tokens** are reported, not refreshed. Refreshing would mean writing to the
  credentials Claude Code itself depends on.
- **A data-less window is dropped.** On plans that do not meter Opus separately,
  `seven_day_opus` comes back empty; a row of `unknown` reads like a failed lookup of a limit
  that actually applies.

## Environment

| Variable | Effect |
|---|---|
| `ANTHROPIC_BASE_URL` | Report `not-applicable`, send no token |
| `CLAUDE_RUNWAY_FORCE_CURL=1` | Skip Go's HTTP client, use system `curl` |
| `CLAUDE_RUNWAY_CACHE_SECONDS` | Cache TTL in seconds (default 60) |
| `CLAUDE_RUNWAY_NO_CACHE=1` | Never read or write the cache |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | A reading, or a definite non-answer such as a custom provider |
| 1 | The reading could not be taken |
| 2 | Usage error: unknown flag, unknown command, unknown field |

Errors go to **stdout** as structured `error:` and `help:` lines, not to stderr, so an agent
capturing stdout always sees why. stderr stays empty.

## Output design

Output follows [AXI](https://axi.md), a set of design principles for agent-ergonomic CLIs:
TOON on stdout rather than JSON, a pre-computed aggregate so the caller does not combine
windows itself, a minimal default field set with `--fields` to widen it, definitive answers
instead of silence, and structured errors with exit codes.

Two places where AXI was applied with judgement rather than followed literally:

- **The discovery preamble is opt-out.** AXI's home view leads with `bin:` and `description:`,
  which is right for a first call and pure waste on the hundredth. `--brief` drops it, and MCP
  `tools/call` uses brief automatically.
- **Not every principle applies.** There is no large text to truncate, no pagination to report
  a total for, and nothing to make idempotent: this is one read-only query.

## Measurements

On an M-series Mac, `darwin/arm64`:

| | |
|---|---|
| Binary size | 5.7 MB, static, no cgo |
| Startup + cached read | 8.5 ms |
| Cross-compiles cleanly | darwin/{arm64,amd64}, linux/{amd64,arm64}, windows/amd64 (5.6-6.3 MB) |

## Tests

```bash
go test ./...
```

Covers the pace math, the verdict's tie-breaking, payload parsing including the empty-window
case, the cache round trip and its refusal of ancient or future readings, the guarantee that
no failure ever renders something mistakable for a percentage, the stale-fallback labelling,
that the cache holds no secret, CLI exit codes and stderr silence, and the MCP protocol
(handshake, version echo, notification handling, error codes, and a request on an unterminated
final line). Network paths were verified by hand against the live endpoint on both transports,
including a real 429 and recovery from it.

## Prior art

- [`NG-Bullseye/claude-usage-mcp`](https://github.com/NG-Bullseye/claude-usage-mcp): same
  endpoint and credential sources, plus a forecast and a velocity recommendation. Ships no
  license, so its code cannot be reused.
- [`ccusage`](https://github.com/ccusage/ccusage): reads local JSONL transcripts to compute
  token counts and costs. Estimated spend, not official utilization. Different question.
- [`AlbeeDev/claude-usage`](https://github.com/AlbeeDev/claude-usage): CLI plus MCP tool, but
  scrapes claude.ai through a browser you stay logged into. Also surfaces credits.

## License

MIT
