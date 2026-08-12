## What this changes, and why

<!-- The why matters more than the what. If a choice came from a measurement or from a
     bug you actually hit, say so with the number or the symptom: that is the house style
     for comments here, and it applies to pull requests too. -->

## Checks

- [ ] `go test -race ./...` passes
- [ ] `gofmt -l .` is empty and `go vet ./...` is clean
- [ ] `golangci-lint run ./...` exits 0
- [ ] Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `build:`). The prefix is not cosmetic: `.goreleaser.yaml` filters `docs:`, `test:` and `chore:` out of the generated release notes.

## Invariants

These are contracts, each with a test asserting it. Confirm your change keeps them, or say which one you are deliberately changing and why.

- [ ] No failure path renders anything a caller could mistake for a percentage. "I could not find out" must never look like "nothing left".
- [ ] An HTTP 429 from the usage endpoint reads as the meter refusing to be read, never as an exhausted subscription.
- [ ] The cache holds only derived numbers. No token, ever.
- [ ] The OAuth token is never logged and never placed in a process argument list.
- [ ] Errors go to stdout as structured `error:` and `help:` lines. stderr stays empty.
- [ ] Exit codes hold: 0 for a reading or a definite non-answer, 1 for a runtime failure, 2 for a usage error.
- [ ] `go.mod` still has zero dependencies. Adding one needs an argument in this description.

## Authorship

- [ ] This change is substantially agent-authored.

Most of this repository is, and that is fine. Ticking the box is not a confession, it is
metadata for the reviewer. What is expected either way: you ran it, you read the diff, and
you can explain why it is correct.
