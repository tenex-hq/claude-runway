# claude-runway

How much of your Claude subscription allowance is **left**, and whether it will last to the
reset. A single static Go binary, no dependencies, no runtime to install.

Usable two ways: as a CLI and as an MCP server.

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
# Homebrew
brew install tenex-hq/tap/claude-runway

# Go
go install github.com/tenex-hq/claude-runway@latest

# Nix
nix profile install github:tenex-hq/claude-runway
nix run github:tenex-hq/claude-runway            # without installing
```

In a nix-darwin or NixOS config, add the flake as an input:

```nix
inputs.claude-runway.url = "github:tenex-hq/claude-runway";
# then, in your packages list:
inputs.claude-runway.packages.${pkgs.system}.default
```

Or grab a prebuilt binary from [Releases](https://github.com/tenex-hq/claude-runway/releases),
or build for wherever you need it. No cgo, so every target is static:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w"
```

### A note on unsigned macOS binaries

The release archives are not code-signed, because signing requires a paid Apple Developer
account. macOS quarantines unsigned downloads and refuses to run them, so the Homebrew cask
clears the quarantine flag on the installed binary in a `postflight` hook.

Be clear about what that trades away. Gatekeeper's check is "was this signed by a developer
Apple recognises", which no unsigned open-source binary can pass. Integrity is still
enforced: Homebrew verifies the downloaded archive against the sha256 recorded in the cask.
What is given up is Apple's opinion of the publisher, not tamper detection, and not publisher
verification in general: releases carry a cosign signature and a build provenance
attestation, which answer the same question Gatekeeper does without involving Apple. See
[Verifying a release](#verifying-a-release).

If you would rather not accept that at all, `go install` and the Nix flake both compile
locally from source and never involve quarantine.

### Verifying a release

`checksums.txt` covers every uploaded artifact, and it is signed keyless with
[cosign](https://docs.sigstore.dev/), so verifying it verifies the whole release. There is no
public key to fetch: the signature is bound to the workflow that produced it.

```bash
cosign verify-blob \
  --bundle checksums.txt.cosign.bundle \
  --certificate-identity "https://github.com/tenex-hq/claude-runway/.github/workflows/release.yml@refs/tags/v0.4.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

Both `--certificate-identity` and `--certificate-oidc-issuer` are required. Without them
cosign accepts a signature from any identity Sigstore will issue to, which is anyone with a
GitHub account, and the check proves nothing.

Every archive also carries a GitHub build provenance attestation, which is a stronger claim
than a signature: it records which workflow, which commit and which runner produced that exact
file. It needs no certificate flags to get wrong.

```bash
gh attestation verify claude-runway_0.4.0_darwin_arm64.tar.gz --repo tenex-hq/claude-runway
```

Each archive additionally ships an SPDX SBOM (`<archive>.sbom.json`). For a zero-dependency
binary the interesting part is how short it is.

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
| `claude-runway install-skills` | Install the Claude Code skill. `--user` or `--project` |

Fields: `window`, `left_pct`, `resets_in`, `resets_at`, `pace`, `headroom_pts`, `severity`,
`utilization_pct`. The default five are the ones a decision actually needs.

## Teaching an agent to use it

```bash
claude-runway install-skills --user      # ~/.claude/skills, every project
claude-runway install-skills --project   # ./.claude/skills, this project only
claude-runway install-skills --user --dry-run
```

This installs a skill that explains how to read the output and what to do about it: that
percentages are remaining, what each verdict means, that a 429 is the meter refusing to be
read rather than an exhausted budget, and how to gate a loop without polling. The skill is
embedded in the binary, so installing needs no network and no second artifact to keep in sync.

**Deliberately a skill, not a `SessionStart` hook.** A hook would inject the budget into every
session before the first prompt, which sounds better than it is: it means merging into a
`settings.json` that probably already has hooks in it, and it pays context cost every session
whether or not the budget turns out to be relevant. A skill is purely additive, writes only
under `<scope>/.claude/skills/claude-runway/`, touches no existing configuration, and loads
only when it is needed.

Safety properties, both covered by tests: re-running is a no-op rather than a rewrite, and a
file you have edited yourself is never silently overwritten. That case reports `conflict`,
exits 1, and tells you to pass `--force` if losing the edit is what you want. A missing scope
is a usage error rather than a guess between your home directory and the project.

### Why it cannot be passive

Worth recording, because it is the obvious next idea: a hook or skill **cannot** read live
window utilisation the way an application driving the Agent SDK can. The SDK streams
`rate_limit_event` messages carrying utilisation directly, but hook input contains no usage
fields at all, and the session transcript records only `message.usage` token counts, with no
utilisation, quota or window data. So the only two ways to learn the real numbers are calling
the endpoint, which is what this does, or estimating spend from token counts, which is a
different question. There is no free lunch here.

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

The usage endpoint **rate-limits reads with HTTP 429**, and it is strict. Measured against
the live endpoint:

| | |
|---|---|
| Calls before refusal | a second call 250ms after the first, once the burst allowance is drained |
| Recovery | **120s**, while polling every 15s |
| `Retry-After` header | present on every 429, value always `0`, therefore useless |

Two caveats on those numbers. There is real burst allowance: several consecutive calls do
get through from a rested bucket, and measuring its true size needs a long idle period
first. And the 120s figure cannot be separated from the polling that measured it, so treat
it as an upper bound on quiet recovery rather than a window length.

Since the intended caller is an agent checking its budget inside a work loop, an uncached
tool would poison the thing it reports on. So:

- A reading younger than 5 minutes is served from cache without touching the network. The
  TTL is set from those measurements: the data moves over a 5h and a 7d window, so a
  5-minute-old reading is barely different from a live one, and anything finer buys
  resolution the data does not have while paying for it in 429s.
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
| `CLAUDE_RUNWAY_CACHE_SECONDS` | Cache TTL in seconds (default 300). `0` always reads live. A value that is not a whole number of seconds, or is negative, is a usage error and exits 2 rather than quietly falling back to the default |
| `CLAUDE_RUNWAY_NO_CACHE=1` | Never read or write the cache |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | A reading, or a definite non-answer such as a custom provider |
| 1 | The reading could not be taken |
| 2 | Usage error: unknown flag, unknown command, unknown field, an argument passed to a subcommand that takes none, or a malformed `CLAUDE_RUNWAY_CACHE_SECONDS` |

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
| Cross-compiles cleanly | darwin/{arm64,amd64}, linux/{arm64,amd64}, windows/{arm64,amd64} (5.6-6.3 MB) |

## Tests

```bash
go test -race ./...
```

90 tests, 89% statement coverage, about 3 seconds. Tests assert behavioural contracts rather
than implementation, so the names read as guarantees:
`TestFailuresNeverRenderNumbers`, `TestCacheHoldsNoSecret`,
`TestRateLimitOnTheMeterIsNotBudgetExhaustion`, `TestDoctorNeverPrintsTheToken`,
`TestCustomProviderSendsNoRequestAtAll`, `TestTokenReachesCurlOnStdinAndNeverInArgv`.

Covered: the pace math and the verdict's tie-breaking; payload parsing including the
empty-window case; the cache round trip, its 0600 mode, its refusal of ancient or future
readings, and that it leaves no temp file behind; the guarantee that no failure ever renders
something mistakable for a percentage; the stale-fallback labelling; credential parsing,
including the subtlety that a missing expiry means *unknown* rather than expired; CLI exit
codes and stderr silence; and the MCP protocol.

Both transports run against an `httptest.Server`, which is why `usageURL` is a package
variable: 200, a real 429, a clipped 500 body, malformed and unrecognised payloads, an
oversized body, a refused connection, the 403 Go-to-curl fallback (and that a curl failure
does not overwrite the honest 403), and that a set `ANTHROPIC_BASE_URL` or an expired token
results in **zero** requests reaching the server.

The suite is hermetic: no network beyond loopback, `HOME` and `XDG_CACHE_HOME` redirected to a
scratch dir, and the macOS Keychain faked rather than touched. That is what lets `nix build`
run it inside the sandbox with `doCheck = true`.

Three things the tests do not prove, worth naming rather than implying: the 15s transport
deadline is not exercised (observing it means waiting for it, so the shared error path is
covered by refused and dropped connections instead); cache atomicity is not proven, only that
no temp file survives and the published file parses; and the Keychain tests exercise the
exec-and-parse plumbing against a stub, not the real Keychain. Live behaviour, including a
genuine 429 and recovery from it, was checked by hand against the real endpoint on both
transports.

## Releasing

Tagging is the whole procedure. `.github/workflows/release.yml` fires on `v*`, and
GoReleaser builds the six targets, publishes a GitHub Release, and pushes the updated cask
to [`tenex-hq/homebrew-tap`](https://github.com/tenex-hq/homebrew-tap).

```bash
goreleaser check                              # validate the config (CI runs this on every PR too)
goreleaser release --snapshot --clean         # full dry run, no publishing
git tag -a v0.2.0 -m "v0.2.0" && git push origin v0.2.0
```

Before it publishes anything, the job checks that `flake.nix` agrees with the tag, then runs
`go vet` and the test suite as their own steps. Those two used to be GoReleaser `before` hooks,
which put them inside the step holding `HOMEBREW_TAP_TOKEN`, a PAT with write access to another
repository. Test code is the least reviewed code in the repo and the worst place to hand a
cross-repo token, so they were moved out.

Two prerequisites, both easy to trip over:

- **`HOMEBREW_TAP_TOKEN`** must exist as a repository secret: a PAT with `contents:write` on
  the tap repo. The default `GITHUB_TOKEN` is scoped to this repository and cannot push to
  another one. Without it the release fails at the Homebrew step, *after* the GitHub Release
  has already been created.
- **`binVersion` is a `var`, not a `const`** (`main.go`), because `-ldflags -X` cannot write
  to a const. If it ever becomes a const again, every release will silently report the
  in-repo development version.

The `version` in `flake.nix` is set by hand and is not derived from the tag, because a flake has
to evaluate from a plain checkout with no tags present. Bump it in the commit the tag points at,
or `nix` installs report a stale version. This drifted twice before the release job started
refusing a mismatch, and the symptom was quiet: Homebrew and `go install` reported the right
version while `nix profile install` reported an older one, on a release that looked entirely
successful.

## Prior art

- [`NG-Bullseye/claude-usage-mcp`](https://github.com/NG-Bullseye/claude-usage-mcp): same
  endpoint and credential sources, plus a forecast and a velocity recommendation. Ships no
  license, so its code cannot be reused.
- [`ccusage`](https://github.com/ccusage/ccusage): reads local JSONL transcripts to compute
  token counts and costs. Estimated spend, not official utilization. Different question.
- [`AlbeeDev/claude-usage`](https://github.com/AlbeeDev/claude-usage): CLI plus MCP tool, but
  scrapes claude.ai through a browser you stay logged into. Also surfaces credits.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) has the build, test and lint commands CI actually runs,
the zero-dependency rule and why it is one, the comment convention, and the behavioural
invariants a change must not break. Most of this repository is written by AI agents, which it
says out loud and pairs with the actual gate: CI green, a pull request against a protected
`main`, and a human review before merge.

[`SECURITY.md`](SECURITY.md) has the threat model and the disclosure route. This tool reads a
live OAuth token off your machine, so anything touching credential handling goes through
private vulnerability reporting rather than a public issue.

## License

MIT
