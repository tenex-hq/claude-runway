package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// resolveVersion reads real build info, so its branches cannot all be driven from a unit test.
// What is testable, and what actually regressed, is the composition: a doubled dirty marker
// ("0.3.0+dirty+abc1234.dirty") shipped once because Go appends its own "+dirty" to
// Main.Version and this code appended another.
func TestReportedVersionHasNoDoubledMarkers(t *testing.T) {
	v := reportedVersion
	if v == "" {
		t.Fatal("version is empty")
	}
	if strings.Count(v, "+dirty") > 1 || strings.Count(v, "dirty") > 1 {
		t.Errorf("version %q repeats the dirty marker", v)
	}
	if strings.Count(v, "+") > 1 {
		t.Errorf("version %q has more than one build-metadata separator", v)
	}
	if strings.HasPrefix(v, "v") {
		t.Errorf("version %q keeps the tag's leading v; it should be normalised away to match the release stamp", v)
	}
	// Either a plain version, or a version plus one metadata segment.
	if !regexp.MustCompile(`^[0-9a-zA-Z.\-]+(\+[0-9a-z]+(\.dirty)?|\+dirty)?$`).MatchString(v) {
		t.Errorf("version %q is not a shape we intend to emit", v)
	}
}

// An explicit stamp must always win, because that is what a release build relies on.
func TestExplicitStampWinsOverBuildInfo(t *testing.T) {
	saved := binVersion
	t.Cleanup(func() { binVersion = saved })

	binVersion = "9.9.9"
	if got := resolveVersion(); got != "9.9.9" {
		t.Errorf("resolveVersion() = %q, want the stamped 9.9.9", got)
	}

	// With the sentinel in place the fallback runs. It cannot be asserted to produce a real
	// version here: `go test` builds its binary without VCS stamping, so "dev" is the correct
	// and only available answer inside a test. What matters is that the previous stamp is gone
	// and the result is still a usable string. The real fallback is covered by
	// TestUnstampedBuildReportsSomethingReal, which builds an actual binary.
	binVersion = devVersion
	got := resolveVersion()
	if got == "9.9.9" {
		t.Error("the previous stamp leaked through after being cleared")
	}
	if got == "" {
		t.Error("resolveVersion() returned nothing; every build must report some identity")
	}
}

// The point of the whole exercise: `go build` with no ldflags must not report a hardcoded
// literal that goes stale the moment a new version is tagged. It must instead derive identity
// from whatever the build actually offers.
//
// What it can offer depends on where the build happens, which is the subtlety: a Nix build
// unpacks the source from a store path with no .git and no module version, so "dev" is the
// correct and only available answer there. An earlier version of this test asserted "dev" was
// never acceptable and failed inside the Nix sandbox, contradicting the documented behaviour.
func TestUnstampedBuildReportsSomethingReal(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	bin := t.TempDir() + "/claude-runway"
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	got := strings.TrimSpace(string(out))

	if got == "" {
		t.Fatal("unstamped build reported nothing at all")
	}
	// The regression that matters: a stale hardcoded number. "dev" is honest; "0.2.0-dev"
	// baked into the source while the repo has moved on is not.
	if regexp.MustCompile(`^\d+\.\d+\.\d+-dev$`).MatchString(got) {
		t.Errorf("unstamped build reported %q, which looks like a hardcoded literal", got)
	}
	if strings.Count(got, "dirty") > 1 {
		t.Errorf("unstamped build reported %q with a doubled marker", got)
	}

	// Only demand real identity when the build had VCS metadata to draw on. `git rev-parse` is
	// not sufficient to establish that on its own, which is the second time this test has had
	// to learn where the toolchain draws the line (the first was the Nix sandbox, above).
	//
	// In a linked worktree (`git worktree add`) .git is a file pointing at the real gitdir
	// rather than a directory, and the Go toolchain does not stamp vcs.revision in that
	// layout, while rev-parse still answers "true". Measured, not assumed: the same commit
	// stamps a revision in the main checkout and reports the bare sentinel in a linked
	// worktree of it. So the probe requires a real .git directory, otherwise this test failed
	// on unmodified code for anyone developing in a worktree, which is exactly where an agent
	// working in isolation runs.
	inVCS := false
	if st, err := os.Stat(".git"); err == nil && st.IsDir() {
		inVCS = exec.Command("git", "rev-parse", "--is-inside-work-tree").Run() == nil
	}
	if inVCS && got == devVersion {
		t.Errorf("built inside a git work tree but reported the bare sentinel %q; VCS info was available and unused", got)
	}
}
