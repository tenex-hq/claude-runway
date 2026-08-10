---
name: release
description: Cut a release of claude-runway. Bumps the version, dry-runs GoReleaser, tags, pushes, and verifies the GitHub Release and the Homebrew cask landed. Use when the user says "release", "cut a release", "ship v0.3.0", "tag a version", or wants to publish a new version.
user-invocable: true
---

# Release claude-runway

**User's arguments:** $ARGUMENTS (usually the target version, e.g. `v0.3.0` or `0.3.0`)

Releasing is tag-driven: pushing a `v*` tag fires `.github/workflows/release.yml`, which runs
GoReleaser to build five targets, publish a GitHub Release, and push an updated cask to
`tenex-hq/homebrew-tap`.

The pipeline has three failure modes that are silent or half-destructive. Work through
preflight rather than skipping to the tag.

## Preflight, all blocking

Run these and stop on the first failure. Do not "fix it after tagging": a pushed tag is
public, and re-tagging the same version is the one thing to avoid.

1. **Version argument.** Normalise to both forms: tag `vX.Y.Z`, bare `X.Y.Z`. If the user
   gave no version, look at `git tag --list 'v*' | sort -V | tail -1` and propose the next
   patch, but confirm before using it.

2. **Tag is not already taken**, locally or on the remote:
   ```bash
   git tag --list 'vX.Y.Z' ; git ls-remote --tags origin 'refs/tags/vX.Y.Z'
   ```
   Both must be empty. If the tag exists, stop and ask. Never force-push a tag.

3. **Clean tree, on `main`, level with origin:**
   ```bash
   git status --porcelain   # must be empty
   git rev-parse --abbrev-ref HEAD   # main
   git fetch origin && git status -sb | head -1   # no ahead/behind
   ```

4. **`binVersion` is still a `var`.** This is the trap that produces a technically successful
   release reporting the wrong version:
   ```bash
   grep -n 'binVersion' main.go
   ```
   It must be `var binVersion = ...`, never `const`. `-ldflags -X` cannot write to a const,
   and nothing fails loudly if it is one; the binary just reports the in-repo literal
   forever. If it is a const, fix it and commit before releasing.

5. **`HOMEBREW_TAP_TOKEN` exists** as a repo or org secret:
   ```bash
   gh secret list --repo tenex-hq/claude-runway
   gh secret list --org tenex-hq 2>/dev/null
   ```
   Either location works; the workflow resolves both identically. If it is missing, **stop
   and tell the user** rather than releasing. Without it the run creates the GitHub Release
   and *then* fails pushing the cask, which is the annoying half-published state. It must be
   a fine-grained PAT scoped to `tenex-hq/homebrew-tap` with `contents:write`. A classic PAT
   cannot be scoped that narrowly.

6. **Remote is SSH.** `git remote -v` should show `git@github.com:...`. Over HTTPS, the `gh`
   OAuth token lacks the `workflow` scope and GitHub rejects any push that touches
   `.github/workflows/`. Fix with:
   ```bash
   git remote set-url origin git@github.com:tenex-hq/claude-runway.git
   ```
   Pushes need to run **outside the sandbox** so the SSH agent is reachable.

## Steps

1. **Bump `flake.nix`.** Its `version` is hand-maintained and *not* derived from the tag. If
   you skip this, `nix` installs report a stale version while Homebrew and `go install`
   report the right one. Edit the `version = "X.Y.Z";` line in the `let` block.

2. **Verify everything locally**, in this order:
   ```bash
   gofmt -l . ; go vet ./... && go test ./...
   goreleaser check
   nix build --no-link --print-out-paths && nix flake check
   ```
   Then confirm the Nix build reports the new version:
   ```bash
   <out-path>/bin/claude-runway version   # must print X.Y.Z, not 0.2.0-dev
   ```

3. **Full dry run.** This builds every target and generates the cask without publishing:
   ```bash
   HOMEBREW_TAP_TOKEN=dryrun goreleaser release --snapshot --clean --skip=publish
   ```
   Inspect `dist/homebrew/Casks/claude-runway.rb`: it should carry four
   `sha256`/`url` pairs (macOS and Linux, intel and arm), a `binary "claude-runway"` stanza,
   and the `postflight` quarantine hook. `dist/` is gitignored.

4. **Commit the version bump**, then tag and push:
   ```bash
   git add flake.nix && git commit -m "chore: v X.Y.Z"   # no space in the real message
   git push origin main
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```
   Push the branch before the tag, so the tag points at a commit that exists on the remote.

5. **Watch the run**, do not assume it passed:
   ```bash
   gh run list --repo tenex-hq/claude-runway --workflow release --limit 1
   gh run watch <id> --repo tenex-hq/claude-runway
   ```

6. **Verify the outcome**, both halves:
   ```bash
   gh release view vX.Y.Z --repo tenex-hq/claude-runway   # 7 artifacts: 4 archives, 2 zips, checksums
   gh api repos/tenex-hq/homebrew-tap/contents/Casks/claude-runway.rb --jq .name
   ```
   The cask is the half that fails independently of the release. Check it explicitly.

## Recovery

- **Homebrew step failed, release exists.** Do not delete the tag. Fix the cause, then re-run
  the failed job: `gh run rerun <id> --repo tenex-hq/claude-runway --failed`.

  A re-run only works because **`release.replace_existing_artifacts: true`** is set in
  `.goreleaser.yaml`. Uploads are not idempotent by default: run 1 uploads the assets, and the
  re-run then collides with them, `422 Validation Failed ... already_exists`, failing the
  whole release even though every artifact is already present and correct. This happened on
  v0.2.0. If that option is ever removed, every re-run breaks.

  Note the config comes from the **tagged commit**, not from `main`. Adding that option to
  `main` does not change the behaviour of a re-run of an existing tag. To recover a tag cut
  before the fix: delete the release assets
  (`gh release delete-asset vX.Y.Z <name> --repo ...`) and then re-run, so the uploads have
  nothing to collide with.

- **Don't trust `gh secret list --repo` to prove a secret is missing.** It does not show
  **organisation** secrets, and listing those needs org admin (`403` otherwise). An empty repo
  list is not evidence. Check the workflow log for what the Homebrew step actually did.

- **A fine-grained PAT against an org needs approving by an org owner** after it is created.
  Until it is approved it exists and looks correct but is not authorised, and the Homebrew
  step fails on it. This was the original v0.2.0 failure.
- **Build failed before publishing.** Nothing is public. Delete the local and remote tag
  (`git tag -d`, `git push origin :refs/tags/vX.Y.Z`), fix, start over.
- **Wrong version reported by the shipped binary.** `binVersion` was a const, or `flake.nix`
  was not bumped. Both need a new patch release; a published tag is not worth rewriting.

## Report back

State plainly: the tag, the release URL, artifact count, whether the cask landed in the tap,
and the verified `claude-runway version` output. If any half did not land, say so first.
