package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The installer writes into a home directory or a project. Every test here redirects both, so
// a bug can never touch the real ~/.claude.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

func skillPath(root string) string {
	return filepath.Join(root, ".claude", "skills", "claude-runway", "SKILL.md")
}

func TestInstallSkillsUserScope(t *testing.T) {
	home := isolateHome(t)
	var out bytes.Buffer
	if code := run([]string{"install-skills", "--user"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("exit = %d, want 0. output:\n%s", code, out.String())
	}
	body, err := os.ReadFile(skillPath(home))
	if err != nil {
		t.Fatalf("skill not written: %v\noutput:\n%s", err, out.String())
	}
	// It has to be a usable skill file, not just any bytes.
	for _, want := range []string{"---", "name: claude-runway", "description:", "user-invocable: true"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("installed skill missing %q", want)
		}
	}
	if !strings.Contains(out.String(), "action") || !strings.Contains(out.String(), "installed") {
		t.Errorf("output should report what it did:\n%s", out.String())
	}
}

func TestInstallSkillsProjectScope(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	t.Chdir(project)

	var out bytes.Buffer
	if code := run([]string{"install-skills", "--project"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("exit = %d, want 0. output:\n%s", code, out.String())
	}
	if _, err := os.Stat(skillPath(project)); err != nil {
		t.Fatalf("skill not written into the project: %v", err)
	}
}

// Re-running must be a no-op, not a duplicate or a rewrite.
func TestInstallSkillsIsIdempotent(t *testing.T) {
	home := isolateHome(t)
	if code := run([]string{"install-skills", "--user"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != exitOK {
		t.Fatal("first install failed")
	}
	before, _ := os.Stat(skillPath(home))

	var out bytes.Buffer
	if code := run([]string{"install-skills", "--user"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("second install exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("second run should report unchanged, got:\n%s", out.String())
	}
	after, _ := os.Stat(skillPath(home))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("second run rewrote a file that was already correct")
	}
}

// The important safety property: someone may have adapted the skill. Silently reverting their
// edit is worse than refusing to act.
func TestInstallSkillsRefusesToClobberEdits(t *testing.T) {
	home := isolateHome(t)
	if code := run([]string{"install-skills", "--user"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != exitOK {
		t.Fatal("first install failed")
	}
	edited := "---\nname: claude-runway\n---\n\nMy own version.\n"
	if err := os.WriteFile(skillPath(home), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := run([]string{"install-skills", "--user"}, strings.NewReader(""), &out, &bytes.Buffer{})
	if code != exitError {
		t.Errorf("exit = %d, want %d when a modified file is present", code, exitError)
	}
	if !strings.Contains(out.String(), "conflict") || !strings.Contains(out.String(), "--force") {
		t.Errorf("output should flag the conflict and mention --force:\n%s", out.String())
	}
	body, _ := os.ReadFile(skillPath(home))
	if string(body) != edited {
		t.Error("the local edit was overwritten without --force")
	}

	// --force is the escape hatch, and it must actually replace the file.
	if code := run([]string{"install-skills", "--user", "--force"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("--force exit = %d, want 0", code)
	}
	body, _ = os.ReadFile(skillPath(home))
	if string(body) == edited {
		t.Error("--force did not overwrite")
	}
}

func TestInstallSkillsDryRunWritesNothing(t *testing.T) {
	home := isolateHome(t)
	var out bytes.Buffer
	if code := run([]string{"install-skills", "--user", "--dry-run"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(skillPath(home)); err == nil {
		t.Error("--dry-run created a file")
	}
	if !strings.Contains(out.String(), "would-install") || !strings.Contains(out.String(), "nothing was written") {
		t.Errorf("dry run should say what it would do and that it did nothing:\n%s", out.String())
	}
}

// A missing scope must fail loudly. Guessing between the home directory and the project is
// exactly the surprise an installer should not spring.
func TestInstallSkillsRequiresAScope(t *testing.T) {
	home := isolateHome(t)
	var out bytes.Buffer
	if code := run([]string{"install-skills"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitUsage {
		t.Errorf("exit = %d, want %d with no scope", code, exitUsage)
	}
	if !strings.Contains(out.String(), "error: ") || !strings.Contains(out.String(), "--user") {
		t.Errorf("should name the missing flag:\n%s", out.String())
	}
	if _, err := os.Stat(skillPath(home)); err == nil {
		t.Error("wrote something despite having no scope")
	}
}

func TestInstallSkillsRejectsUnknownFlag(t *testing.T) {
	isolateHome(t)
	var out bytes.Buffer
	if code := run([]string{"install-skills", "--user", "--nope"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.HasPrefix(out.String(), "error: unknown flag") {
		t.Errorf("output: %s", out.String())
	}
}

// The skill documents a shell watcher. A snippet an agent copies has to at least parse, and
// its dangerous-to-omit branches have to be present: a watcher that cannot distinguish "cannot
// read the budget" from "still above the floor" waits forever and looks like success.
func TestEmbeddedWatcherSnippetIsSound(t *testing.T) {
	body, err := skillFS.ReadFile("skills/claude-runway/SKILL.md")
	if err != nil {
		t.Fatalf("skill not embedded: %v", err)
	}
	var snippet string
	for _, b := range strings.Split(string(body), "```bash\n") {
		if i := strings.Index(b, "```"); i >= 0 && strings.Contains(b[:i], "while :;") {
			snippet = b[:i]
		}
	}
	if snippet == "" {
		t.Fatal("the watcher snippet is gone from the skill")
	}
	for _, want := range []string{"FLOOR=", "INTERVAL=", "not-applicable", "cannot read the budget", "window reset", "exit 0"} {
		if !strings.Contains(snippet, want) {
			t.Errorf("watcher snippet no longer handles %q", want)
		}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash to syntax-check with")
	}
	f := filepath.Join(t.TempDir(), "snip.sh")
	if err := os.WriteFile(f, []byte(snippet), 0o644); err != nil {
		t.Fatal(err)
	}
	// -n parses without executing, so this never polls the endpoint.
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Errorf("documented watcher does not parse: %v\n%s", err, out)
	}
}

// The shipped skill has to teach the one thing that inverts every decision if misread.
func TestEmbeddedSkillStatesRemainingNotConsumed(t *testing.T) {
	body, err := skillFS.ReadFile("skills/claude-runway/SKILL.md")
	if err != nil {
		t.Fatalf("skill not embedded: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "REMAINING") {
		t.Error("the skill must say percentages are remaining, not consumed")
	}
	for _, want := range []string{"429", "cache-stale", "--brief", "verdict"} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill should cover %q", want)
		}
	}
}
