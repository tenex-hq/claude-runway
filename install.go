package main

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Skills ship inside the binary, so installing one needs no network, no checkout, and no
// second artifact to keep in sync with the release.
//
//go:embed skills
var skillFS embed.FS

// Deliberately a skill rather than a SessionStart hook. A hook means merging into a shared
// settings.json that likely already has hooks in it, and it pays context cost every session
// whether the budget is relevant or not. A skill is a new directory, touches nothing that
// exists, and loads only when it is needed.

type installScope struct {
	name string // "user" | "project"
	dir  string // .../skills
}

func resolveScope(scope string) (installScope, error) {
	switch scope {
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return installScope{}, fmt.Errorf("cannot locate your home directory: %w", err)
		}
		return installScope{"user", filepath.Join(home, ".claude", "skills")}, nil
	case "project":
		cwd, err := os.Getwd()
		if err != nil {
			return installScope{}, fmt.Errorf("cannot determine the working directory: %w", err)
		}
		return installScope{"project", filepath.Join(cwd, ".claude", "skills")}, nil
	default:
		return installScope{}, fmt.Errorf("unknown scope %q", scope)
	}
}

type installAction struct {
	skill  string
	path   string // relative to the skills dir, for display
	action string // installed | updated | unchanged | would-install | would-update | conflict
}

// Install is additive: it only ever writes files under <scope>/skills/<skill-name>/. It never
// edits an existing file that differs from what we ship without --force, because a user may
// have adapted it and silently reverting that would be worse than refusing.
func installSkills(scope installScope, dryRun, force bool) ([]installAction, error) {
	var out []installAction
	err := fs.WalkDir(skillFS, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := skillFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "skills/")
		skill := strings.SplitN(rel, "/", 2)[0]
		dest := filepath.Join(scope.dir, rel)

		act := installAction{skill: skill, path: rel}
		existing, readErr := os.ReadFile(dest)
		switch {
		case readErr == nil && bytes.Equal(existing, want):
			// Idempotent: re-running changes nothing and says so.
			act.action = "unchanged"
			out = append(out, act)
			return nil
		case readErr == nil && !force:
			act.action = "conflict"
			out = append(out, act)
			return nil
		case readErr == nil:
			act.action = "updated"
		default:
			act.action = "installed"
		}
		if dryRun {
			if act.action == "updated" {
				act.action = "would-update"
			} else {
				act.action = "would-install"
			}
			out = append(out, act)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, want, 0o644); err != nil {
			return err
		}
		out = append(out, act)
		return nil
	})
	return out, err
}

func runInstallSkills(args []string, stdout io.Writer) int {
	scopeName, dryRun, force := "", false, false
	for _, a := range args {
		switch a {
		case "--user":
			scopeName = "user"
		case "--project":
			scopeName = "project"
		case "--dry-run":
			dryRun = true
		case "--force":
			force = true
		default:
			fmt.Fprintf(stdout, "error: unknown flag %q for install-skills\nhelp: valid flags are --user, --project, --dry-run, --force\n", a)
			return exitUsage
		}
	}
	// No default scope on purpose. Writing into a home directory when the caller meant the
	// project (or the reverse) is exactly the kind of surprise an installer should not spring.
	if scopeName == "" {
		fmt.Fprintf(stdout, "error: install-skills needs a scope\nhelp: --user installs for every project (~/.claude/skills), --project installs into ./.claude/skills. Add --dry-run to see what would change.\n")
		return exitUsage
	}

	scope, err := resolveScope(scopeName)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return exitError
	}
	actions, err := installSkills(scope, dryRun, force)
	if err != nil {
		fmt.Fprintf(stdout, "error: could not install into %s (%v)\n", scope.dir, err)
		return exitError
	}
	if len(actions) == 0 {
		// Cannot happen with an embedded skill present, but an empty report would be
		// indistinguishable from a crash, so it gets a definite answer too.
		fmt.Fprintf(stdout, "error: this build embeds no skills\n")
		return exitError
	}

	fmt.Fprintf(stdout, "bin: %s\ndescription: Install the claude-runway skill so an agent knows how to read the budget.\n", binPath())
	fmt.Fprintf(stdout, "scope: %s\ntarget: %s\n", scope.name, scope.dir)
	fmt.Fprintf(stdout, "skills[%d]{skill,path,action}:\n", len(actions))
	conflicts := 0
	for _, a := range actions {
		if a.action == "conflict" {
			conflicts++
		}
		fmt.Fprintf(stdout, "  %s,%s,%s\n", a.skill, a.path, a.action)
	}
	if conflicts > 0 {
		fmt.Fprintf(stdout, "error: %d file(s) already exist with different content and were left alone\n", conflicts)
		fmt.Fprintf(stdout, "help: inspect them, then re-run with --force to overwrite. Local edits will be lost.\n")
		return exitError
	}
	if dryRun {
		fmt.Fprintf(stdout, "help: nothing was written. Re-run without --dry-run to apply.\n")
		return exitOK
	}
	fmt.Fprintf(stdout, "help: invoke it with `/claude-runway`, or let the model load it when budget comes up. Remove it with `rm -rf %s`\n", filepath.Join(scope.dir, actions[0].skill))
	return exitOK
}
