# Security Policy

claude-runway reads a live Claude Code OAuth access token off the local machine and sends it
to one HTTPS endpoint. That is the whole of what it does, and it is why this file is specific
rather than boilerplate.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: the **Security** tab on
[tenex-hq/claude-runway](https://github.com/tenex-hq/claude-runway/security), then **Report a
vulnerability**. That opens an advisory visible only to you and the maintainer.

**Do not open a public issue for anything involving credential handling.** For this tool a
public report of a token-leak path is close to a working exploit, and every user running the
released binary is exposed until they upgrade. Functional bugs, wrong numbers, bad wording and
packaging problems are fine as normal public issues.

What to expect, stated honestly: this is maintained by one person in their own time. Aim for an
acknowledgement within five working days and an assessment within two weeks. A confirmed
credential-handling issue gets a release as soon as a fix exists and is tested; anything less
severe goes out with the next release. There is no bounty programme and none is planned.

Useful in a report: the output of `claude-runway version`, your OS and architecture, whether
credentials come from the file or the macOS Keychain (`claude-runway doctor` says which), and
the steps to reproduce. **Never paste a token, an `Authorization` header, or the contents of
`~/.claude/.credentials.json` into a report.** Redact it. If a proof of concept needs a real
token to be convincing, describe the mechanism instead.

## Supported versions

| Version | Supported |
|---|---|
| Latest release | Yes |
| Everything older | No |

Fixes land on `main` and ship in the next tag. There are no maintenance branches and no
backports, because there is one maintainer and pretending otherwise would be a promise this
project cannot keep. Upgrading is `brew upgrade claude-runway`,
`go install github.com/tenex-hq/claude-runway@latest`, or updating the flake input.

## Threat model

The tool holds exactly one thing worth attacking: read access to your Claude Code OAuth token,
which is a bearer credential for your Anthropic account. Everything below follows from that.

### In scope

- **Any path that exfiltrates or leaks the token.** Writing it to the cache, to a log, to
  stdout or stderr, into a process argument list, into an environment variable inherited by a
  child, or to any host other than the usage endpoint. `doctor` output is included: it reports
  where credentials were found and must never reveal anything about the token itself.
- **Any write to the credentials file or the Keychain item.** This tool is read-only on
  credentials by design, so that it cannot lock you out of Claude Code. A path that writes,
  truncates, corrupts or re-permissions either source is a vulnerability even if no data
  escapes the machine.
- **Weakening the cache's isolation.** The cache must stay 0600 inside a 0700 directory, must
  be replaced atomically rather than written in place, and must never contain a secret. A
  change that widens the mode, follows a symlink, writes through a predictable path in a
  world-writable directory, or lets a stale reading be served without its age being stated is
  in scope. Serving a stale number silently is a security bug here, not a cosmetic one: the
  label is what stops a caller acting on data it thinks is current.
- **Anything in a response from the endpoint that causes code execution, a panic, a crash, an
  unbounded allocation or a hang.** The response is untrusted input. So is `curl`'s output.
- **Misreporting that changes a caller's behaviour dangerously**, specifically a failure
  rendering something a caller could parse as a percentage, or a 429 on the meter reading as an
  exhausted subscription. Both are behavioural contracts with tests behind them, and an agent
  gating real work on the output is the reason they matter.
- **Supply chain on the release pipeline**: a way to get a different artifact than the tagged
  source builds, or to influence the published Homebrew cask.

### Out of scope

- **The endpoint being undocumented.** `GET https://api.anthropic.com/api/oauth/usage` is not a
  published API. It can change shape, start refusing requests, or disappear, and any of those
  will break this tool. That is a known and documented cost of the approach, not a
  vulnerability. Reports that the endpoint changed are welcome as ordinary issues.
- **The unsigned macOS binaries and the Homebrew cask's `postflight` quarantine strip.** This
  is a deliberate, documented trade, and the reasoning and what it gives up are set out in
  [README.md, "A note on unsigned macOS binaries"](README.md#a-note-on-unsigned-macos-binaries)
  and in the comment on the `homebrew_casks` hook in `.goreleaser.yaml`. Read those before
  filing. A new report needs to identify something those two do not already account for. If you
  do not want to accept the trade, `go install` and the Nix flake both compile locally from
  source and never involve quarantine.
- **Anything that requires an attacker who already has read access to your home directory.**
  At that point `~/.claude/.credentials.json` is directly readable and the token is already
  gone. This tool adds no privilege and removes no protection in that scenario, so "a local
  process could read the cache file" or "a local process could read the credentials file" is
  not a finding against claude-runway. The exception is a path where this tool grants strictly
  more than the attacker already had, for example writing a secret somewhere with wider
  permissions than the credentials file itself.
- **Compromise of the machine, of Claude Code's own credential storage, or of the Anthropic
  account.** Those are upstream of this tool entirely.
- **The token being sent to Anthropic.** That is the function.

## Security properties, and where they are enforced

Each of these is enforced in the source. The ones with a named test are also pinned against
regression; the rest are code facts, and stating the difference is the point of this table.

| Property | Enforced in | Test |
|---|---|---|
| The cache file contains no token, and is built from a fixed field list so a future field cannot smuggle one in | `cache.go`, `writeCache` | `TestCacheHoldsNoSecret` asserts the file mentions none of `token`, `accessToken`, `Bearer`, `refresh` |
| Credentials are never written. The only two write paths in the whole program are the cache and `install-skills` | `credentials.go` (reads only), `cache.go`, `install.go` | none directly; `install-skills` write scope is covered by `TestInstallSkillsDryRunWritesNothing` and `TestInstallSkillsRefusesToClobberEdits` |
| The token reaches `curl` through a config file on **stdin**, never in argv, so it cannot be read out of the process table | `usage.go`, `viaCurl` | none |
| The cache is written 0600 into a 0700 directory, through a temp file with a name unique per process, and replaced by an atomic rename, so a concurrent reader never sees half a file and two concurrent writers cannot tear each other's output | `cache.go`, `writeCache` | none asserts the mode |
| The usage URL is a package variable only so a test can point it at an `httptest.Server`. It is not user configurable and must never be wired to an environment variable, because that would be a way to send a subscription OAuth token to a third-party host | `usage.go`, `usageURL` | none |
| A cached reading with a negative age (clock moved) or older than the 6h ceiling is refused rather than served | `cache.go`, `readCache` | `TestCacheRejectsAncientAndFutureReadings` |
| A stale reading is always labelled with its source and age and says why it is not live | `render.go`, `cache.go` | `TestStaleFallbackIsLabelledNotSilent` |
| No failure path renders anything mistakable for a percentage, and no failure path is silent | `render.go`, `describeFailure` | `TestFailuresNeverRenderNumbers` |
| A 429 on the meter is never worded as an exhausted subscription | `render.go`, `describeFailure` | `TestRateLimitOnTheMeterIsNotBudgetExhaustion` |
| Errors go to stdout as structured `error:` and `help:` lines. stderr stays empty | `main.go`, `render.go` | `TestUsageErrorsGoToStdoutWithExit2` asserts stderr is empty |
| Both transports cap the response body at the same 4 MiB constant, so a hostile or runaway response cannot exhaust memory. An oversized body is reported as a failure rather than parsed as a truncated document | `usage.go`, `maxResponseBytes`, `viaHTTP`, `viaCurl` | none |
| Neither transport can block forever. Go's client has a 15s timeout; the `curl` path has a context deadline, `--max-time` and `--connect-timeout` | `usage.go`, `viaHTTP`, `viaCurl` | none |
| Free-form text from the endpoint is clipped before it reaches an error message, and the key list from a wrong-shape response is sorted and bounded | `usage.go`, `clipDetail`, `parsePayload` | none |
| An unparseable or unrecognised payload becomes a typed failure, not a crash | `usage.go`, `parsePayload` | `TestParsePayloadRejectsUnknownShape`, `TestParsePayloadDropsEmptyWindows` |
| `doctor` reports only platform, paths and expiry. The struct it prints from carries no token field | `credentials.go`, `diagnose`; `main.go`, `runDoctor` | none |
| Nothing is logged anywhere. The program imports no logging package at all | all of `*.go` | none |

`gosec` runs over every pull request as part of the lint job. It is enabled in `.golangci.yml`
for the two things it is actually good at here: credential handling and shelling out.
`govulncheck` also runs on every pull request. With zero dependencies the only thing it can
report is a vulnerability in the Go standard library or toolchain, which is precisely the reason
it is worth running. Both are in `.github/workflows/ci.yml`, and the linter version there is
pinned exactly so a linter release cannot quietly stop enforcing this.

The `security find-generic-password` call on macOS discards the subprocess's stderr, so a
locked-Keychain prompt cannot leak into this tool's own output.

## `ANTHROPIC_BASE_URL`

Security-relevant enough to state on its own. When `ANTHROPIC_BASE_URL` is set, the tool
concludes a custom provider is in use, and **the token is sent nowhere at all**. The check runs
before credentials are read, so on that path the credential sources are never even opened. The
tool reports `verdict: not-applicable` and exits 0, so a caller proceeds without a budget gate
instead of polling a number that will never exist.

This means claude-runway will not send your Claude subscription token to a third-party gateway
you have pointed Claude Code at. If you find a path where a token leaves the machine while
`ANTHROPIC_BASE_URL` is set, that is a vulnerability, and it is the one worth reporting first.
