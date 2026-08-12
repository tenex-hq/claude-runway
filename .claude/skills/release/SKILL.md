---
name: release
description: Cut a release of claude-runway. Bumps the version, dry-runs GoReleaser, tags, pushes, and verifies the GitHub Release and the Homebrew cask landed. Use when the user says "release", "cut a release", "ship v0.3.0", "tag a version", or wants to publish a new version.
user-invocable: true
---

# Release claude-runway

**User's arguments:** $ARGUMENTS (usually the target version, e.g. `v0.3.0` or `0.3.0`)

Releasing is tag-driven: pushing a `v*` tag fires `.github/workflows/release.yml`, which vets
and tests, checks `flake.nix` against the tag, then runs GoReleaser to build six targets,
generate an SPDX SBOM per archive, sign `checksums.txt` with keyless cosign, publish a GitHub
Release, and push an updated cask to `tenex-hq/homebrew-tap`. A final step attests build
provenance for the archives.

Two ordering decisions in that list are load-bearing and are documented in the files
themselves, so read the comment before moving anything:

- **`go vet` and `go test` are workflow steps, not GoReleaser `before.hooks`.** Before-hooks
  execute inside the goreleaser step, whose env holds `HOMEBREW_TAP_TOKEN`, a PAT with write
  access to another repository. Test code could read it out of the environment. Moving them
  back re-opens that.
- **The `flake.nix` guard runs before anything publishes**, so a version mismatch costs one
  red run and no public artifacts.

Every action is pinned to a full commit SHA with the version in a trailing comment, since a
mutable tag next to a cross-repo PAT is the classic supply-chain path. Do not "simplify" one
back to `@v7`. `.github/dependabot.yml` opens a weekly grouped PR that rewrites the SHAs and
the comments together.

The pipeline has failure modes that are silent or half-destructive. Work through preflight
rather than skipping to the tag.

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

   The workflow now guards this: its first step after checkout compares that line against
   `${GITHUB_REF_NAME#v}` and fails before publishing anything. The guard reads the config
   from the **tagged commit**, so the bump has to be in a commit the tag points at or
   contains, not merely on `main`.

2. **Verify everything locally**, in this order:
   ```bash
   gofmt -l . ; go vet ./... && go test -race ./...
   golangci-lint run ./...
   goreleaser check
   nix build --no-link --print-out-paths && nix flake check
   ```
   `golangci-lint` reads `.golangci.yml`, which pins the schema version and the two
   deliberate `errcheck` exclusions. CI pins the linter version too, so a green run here
   means a green run there.
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

   The `sboms` and `signs` stages shell out to `syft` and `cosign`, which the release runner
   installs but a laptop may not have. Without them the dry run dies **after** the builds
   with `exec: "syft": executable file not found in $PATH`, which is the tool missing locally
   and not a config error. Either install them, or skip those two stages and accept that the
   dry run no longer exercises them:
   ```bash
   HOMEBREW_TAP_TOKEN=dryrun goreleaser release --snapshot --clean --skip=publish,sbom,sign
   ```
   `cosign` signing is keyless and needs a GitHub OIDC token, so it only ever really runs in
   Actions. A local run of that stage would prompt for a browser login.

4. **Land the version bump on `main` through a pull request**, then tag what landed.

   `main` is protected: it requires a pull request and a green `check` status. Admins are not
   enforced, so a direct `git push origin main` would succeed, and that is exactly why this
   says not to. CONTRIBUTING.md tells contributors that changes never land by direct push, and
   the release path is the worst place for the maintainer to be seen doing otherwise.

   ```bash
   git checkout -b chore/vX.Y.Z
   git add flake.nix && git commit -m "chore: v X.Y.Z"   # no space in the real message
   git push -u origin chore/vX.Y.Z
   gh pr create --title "chore: vX.Y.Z" --body "Version bump for vX.Y.Z." --base main
   gh pr merge --rebase --delete-branch    # waits for the required check
   ```

   Then tag the commit that actually landed, not your local one. `main` is rebase-merge only
   (linear history is required), so the merged commit has a **different SHA** than the one you
   pushed. Tagging the local commit would tag an object that is not on `main`, and the flake
   guard reads `flake.nix` from the tagged commit:

   ```bash
   git checkout main && git fetch origin && git reset --hard origin/main
   grep -n 'version = ' flake.nix        # confirm it says X.Y.Z before tagging
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

5. **Watch the run**, do not assume it passed:
   ```bash
   gh run list --repo tenex-hq/claude-runway --workflow release --limit 1
   gh run watch <id> --repo tenex-hq/claude-runway
   ```

6. **Verify the outcome**, all of it:
   ```bash
   # 14 artifacts: 4 tar.gz, 2 zip, 6 .sbom.json, checksums.txt, checksums.txt.cosign.bundle
   gh release view vX.Y.Z --repo tenex-hq/claude-runway
   gh api repos/tenex-hq/homebrew-tap/contents/Casks/claude-runway.rb --jq .name
   ```
   The cask is the half that fails independently of the release. Check it explicitly.

   Then verify the release the way a user would, because a signature nobody checks is
   decoration and this is the only time anyone will notice it is wrong:
   ```bash
   gh release download vX.Y.Z --repo tenex-hq/claude-runway \
     -p 'checksums.txt' -p 'checksums.txt.cosign.bundle' -p '*.tar.gz'
   cosign verify-blob \
     --bundle checksums.txt.cosign.bundle \
     --certificate-identity "https://github.com/tenex-hq/claude-runway/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     checksums.txt
   sha256sum --check --ignore-missing checksums.txt
   gh attestation verify claude-runway_*.tar.gz --repo tenex-hq/claude-runway
   ```
   `--certificate-identity` and `--certificate-oidc-issuer` are not optional. Omit them and
   cosign accepts a signature from any GitHub identity, so the check passes while proving
   nothing. The identity is the workflow path at the tag, which is why it changes every
   release. The same command with the tag substituted is in the release body.

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
  (`git tag -d`, `git push origin :refs/tags/vX.Y.Z`), fix, start over. Everything up to and
  including the goreleaser step is in this category: the flake guard, vet, test, the tool
  installs, the builds, the SBOMs and the signature all run before any asset is uploaded.
- **Flake version guard failed.** Cheapest failure in the pipeline, and the error text spells
  out the fix. Delete the tag, bump `flake.nix`, commit, re-tag. Do not reach for
  `--no-verify` equivalents; the guard exists because this drifted twice.
- **`exec: "syft"` or `"cosign"`: executable file not found in $PATH.** The install step was
  removed, reordered after the goreleaser step, or its action pin broke. Both must be
  installed before goreleaser runs. This fails late, after the cross-compiles, and nothing has
  been published at that point.
- **`cosign: unknown flag: --output-certificate`.** The `signs` block was reverted to the
  pre-v3 two-file form. `sigstore/cosign-installer` installs cosign v3, which dropped
  `--output-signature`/`--output-certificate` from `sign-blob` in favour of `--bundle`. The
  config in `.goreleaser.yaml` is the v3 form on purpose.
- **`cosign` failed with an OIDC or token error.** The job lost `id-token: write`. Same for
  the attestation step, which additionally needs `attestations: write`. All three permissions
  are listed on the job with a comment each; removing one breaks exactly one stage.
- **Wrong version reported by the shipped binary.** `binVersion` was a const, or the tag
  points at a commit whose `flake.nix` was not bumped. Both need a new patch release; a
  published tag is not worth rewriting.

## Report back

State plainly: the tag, the release URL, artifact count, whether the cask landed in the tap,
whether `cosign verify-blob` and `gh attestation verify` passed, and the verified
`claude-runway version` output. If any half did not land, say so first.
