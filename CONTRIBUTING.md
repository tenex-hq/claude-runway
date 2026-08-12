# Contributing

Issues and pull requests are welcome. This file exists so a first PR does not fail CI on
something nobody wrote down.

## Build, test, lint

These are the checks `.github/workflows/ci.yml` runs, in order. Run them locally and CI holds no
surprises. The first four are literally the workflow's commands; the last two run there through
pinned actions, so these are the local equivalents.

```bash
test -z "$(gofmt -l .)"                       # must print nothing
go vet ./...
go build ./...
go test -race ./...                           # CI adds -coverprofile=coverage.out
golangci-lint run ./...
govulncheck ./...
```

Plus the check that keeps the zero-dependency promise honest:

```bash
go mod tidy
git diff --exit-code go.mod                   # must be unchanged
test ! -f go.sum                              # must not exist
```

Two things about the linter. `.golangci.yml` declares `version: "2"`, and v1 and v2 have
incompatible layouts, so a v1 binary will refuse the file. CI pins **golangci-lint v2.12.2**
exactly, deliberately, so that a release of the linter cannot turn a green `main` red on its
own. Match that version locally or expect disagreements. The config enables `gosec`,
`bodyclose`, `errorlint`, `misspell`, `revive` and `unconvert` on top of the standard set, and
every exclusion in it has a comment explaining the specific line it excuses. Read those before
adding another.

The Go toolchain version comes from `go.mod` (CI uses `go-version-file`), so there is one place
to change it.

Every `uses:` in both workflows is pinned to a full commit SHA with the version in a trailing
comment, and the repository has GitHub's "require SHA pinning" setting enabled, so a workflow
that references an action by tag is rejected before it runs. This is not stylistic: `release.yml`
holds a PAT with write access to another repository, and a mutable tag means whoever controls it
controls what executes next to that token. Dependabot rewrites the SHA and the comment together
in a weekly grouped PR, which is the only way pinning survives contact with reality. If you add
an action, resolve its SHA with
`gh api repos/OWNER/REPO/git/ref/tags/vX.Y.Z --jq .object.sha` (dereference a second time if the
tag object is annotated rather than a commit).

There is a Nix flake with a devShell:

```bash
nix develop      # go, gopls, goreleaser on PATH
nix build        # builds the package, runs the test suite in the sandbox
nix flake check
```

Note what the devShell does not provide: `golangci-lint` and `govulncheck` are not in it, so
install those yourself or let CI be the check for them. The Nix build sets `doCheck = true`, and
the suite is hermetic (no network, `HOME` and `XDG_CACHE_HOME` redirected to a scratch dir),
which is what lets it run inside the sandbox at all. A test that reaches the network or the real
home directory breaks `nix build`, not just good taste.

CI runs on `ubuntu-latest` only. That is a cost decision, explained in the workflow: the only
OS-specific behaviour is path construction, and GoReleaser cross-compiles all six targets at
release time, so a build break on another `GOOS` still gets caught before anything ships.

## Zero dependencies

`go.mod` has no `require` block, and that is a property to preserve rather than an accident of
being small. It is why there is no `go.sum`, why `vendorHash = null` works in `flake.nix`, why
module caching is switched off in both workflows with nothing lost, why `govulncheck` can only
ever report a standard library or toolchain issue, and why the binary is a 5.7 MB static
artifact with no third-party CVE surface. CI enforces it: `go mod tidy` must leave `go.mod`
byte-identical and must not produce a `go.sum`.

A PR that adds a dependency has to argue for it in the description, against the alternative of
writing the twenty lines by hand. That argument can be won. It usually is not. If a dependency
does land, the comments in `ci.yml`, `release.yml` and `flake.nix` that reason from the absence of
`go.sum` all become wrong and need updating in the same PR.

## Agent-authored code

Most of the code in this repository was written by AI agents, including most of its comments and
most of this file. That is deliberate and it is not going to change. Stating it plainly is
better than letting someone work it out from the commit log.

What makes that acceptable is the gate, not the disclosure. CI must pass. Changes land through a
pull request against a protected `main`, never a direct push, and a human maintainer reviews the
diff before merge (see `.github/CODEOWNERS`). Contributors are asked to tick the authorship box
in the pull request template when a change is substantially agent-authored, and, whatever wrote
it, to have actually run it and read the diff. An agent-authored PR that the author cannot
explain will be sent back.

## Commits

Conventional Commits, matching the existing history:

```
feat: install-skills, shipping the skill inside the binary
fix: derive the version from build info when nothing stamps it
docs(skill): document working to a budget target
build: add distribution via GoReleaser, Homebrew tap, and a Nix flake
chore: bump flake version to 0.3.0
```

`feat:`, `fix:`, `docs:`, `chore:` and `build:` are the prefixes in use so far, with scoped forms
like `docs(skill):` where a scope adds something. `test:` is not in the history yet but is
recognised by the release config, so use it for a test-only change.

The prefix has a consequence beyond tidiness: `.goreleaser.yaml` filters `^docs:`, `^test:` and
`^chore:` out of the generated changelog. A user-visible change committed as `chore:` will not
appear in the release notes. Pick the prefix for what the change is, not for how small it felt.

## Comment style

This is the most distinctive convention in the codebase and the easiest one to get wrong.

Comments explain **why**, never what. Where a choice came from a measurement or from a bug that
actually happened, the comment says so, with the number or the symptom. From `version.go`:

```go
// Go already appends its own "+dirty" to a version derived from a modified tree. Take that
// as a second source of truth for dirtiness, then strip it, so the marker is not emitted
// twice ("0.3.0+dirty+abc1234.dirty" was a real bug).
```

The parenthetical is the part that earns the comment its place. Same pattern in `cache.go`,
where the 5-minute TTL is justified by the 120s endpoint recovery that was measured against the
live API, and in `.goreleaser.yaml`, where `replace_existing_artifacts: true` carries the
`422 already_exists` failure that made it necessary. A future reader can disagree with a number.
They cannot disagree with taste.

A comment that restates the code is worse than no comment, because it is one more thing that can
go out of date. Delete those rather than updating them.

## Testing expectations

Tests here assert behavioural contracts, not implementation. Good examples to imitate, by name:

| Test | What it pins |
|---|---|
| `TestFailuresNeverRenderNumbers` | Every failure reason renders a structured error and never a data shape |
| `TestCacheHoldsNoSecret` | The cache file mentions nothing token-shaped |
| `TestRateLimitOnTheMeterIsNotBudgetExhaustion` | A 429 is worded as the meter refusing to be read, and the retry advice matches the measurement |

Each of those would still pass if the code underneath were rewritten, and would fail if the
behaviour changed. That is the bar.

The invariants a change must not break, each with a test behind it:

- No failure path renders anything a caller could mistake for a percentage, and no failure path
  is silent.
- An HTTP 429 from the usage endpoint never reads as an exhausted subscription.
- The cache holds only derived numbers. No token, ever.
- Errors go to **stdout** as structured `error:` and `help:` lines. stderr stays empty.
- Exit codes: `0` for a reading or a definite non-answer such as a custom provider, `1` for a
  runtime failure, `2` for a usage error (unknown flag, command or field).

Percentages are remaining, not consumed, everywhere: output, JSON, the MCP tool description and
the embedded skill. A change that flips the direction in one of them and not the others is a
correctness bug, and `TestEmbeddedSkillStatesRemainingNotConsumed` and `TestMCPHandshake` both
exist to catch it.

The transport is the one place to be careful. `usageURL` is a package variable so a test can
point it at an `httptest.Server`, which covers status handling, body limits, the 403 curl
fallback and the paths that must send no request at all. It does **not** cover the 15s deadline,
because observing that means waiting for it, and a fake of an undocumented endpoint can only
ever confirm what the fake was told to do anyway. Real behaviour, a genuine 429 and recovery
from it, has been checked by hand against the live API. If your change touches the transport,
say in the PR what you actually ran and what came back.

One thing `usageURL` is not: configurable. It must never be wired to an environment variable,
because that would be a route for sending a subscription OAuth token to a third-party host.
`ANTHROPIC_BASE_URL` already means the opposite (send the token nowhere at all), and
[`SECURITY.md`](SECURITY.md) explains why.

## Releasing

Maintainer only, and tag-driven. The procedure and its failure modes are written down in
`.claude/skills/release/SKILL.md`, and summarised in the README's
[Releasing](README.md#releasing) section. Do not duplicate them here, and do not push tags in a
pull request.

## License

Contributions are accepted under the MIT license in `LICENSE`. Security reports have their own
route: see [`SECURITY.md`](SECURITY.md), and use private vulnerability reporting rather than a
public issue for anything touching credential handling.
