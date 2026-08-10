package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A var, not a const, so a release build can stamp the real version in with
// `-ldflags "-X main.binVersion=..."`. As a const it would silently report this
// literal forever, no matter what was tagged.
var binVersion = "0.2.0-dev"

// One sentence, printed in the home view so an agent that runs the bare command learns
// what this is without a second call (AXI principle 8).
const description = "How much of your Claude subscription allowance is left, and whether it will last to the reset."

// Exit codes follow AXI: 0 success or a definite non-answer, 1 runtime failure, 2 usage
// error. Errors go to stdout as structured text, not stderr, so an agent capturing stdout
// always sees why.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const helpText = `claude-runway - ` + description + `

Usage:
  claude-runway [flags]        Read the meters: verdict, then one row per window
  claude-runway doctor         Where credentials were found (never prints the token)
  claude-runway mcp            Serve MCP over stdio; one tool, check_usage
  claude-runway version        Print the version

Flags:
  --brief                     Drop the preamble and help lines. Use this in loops.
  --json                      JSON instead of TOON, for hosts and scripts
  --fields=a,b,c              Pick columns. Default: window,left_pct,resets_in,pace,headroom_pts
  -h, --help                  This text

Fields available to --fields:
  headroom_pts, left_pct, pace, resets_at, resets_in, severity, utilization_pct, window

Percentages are REMAINING: 100 means untouched, 0 means exhausted. Pace compares budget
left against time left in the window, so positive headroom means you are safe to push.
The verdict (safe, caution, stop) already combines the windows.

Readings are cached for 5 minutes, because the usage endpoint rate-limits repeated calls
with HTTP 429 and took 120s to recover when measured. That makes a per-iteration budget
check safe. When a live read fails, the last reading is served instead, labelled with its
age, rather than nothing.

Environment:
  ANTHROPIC_BASE_URL           If set, reports not-applicable and sends no token anywhere
  CLAUDE_RUNWAY_FORCE_CURL=1   Skip Go's HTTP client, use the system curl
  CLAUDE_RUNWAY_CACHE_SECONDS  Cache TTL in seconds (default 300)
  CLAUDE_RUNWAY_NO_CACHE=1     Never read or write the cache

Examples:
  claude-runway --brief
  claude-runway --fields=window,left_pct,resets_at
  claude mcp add runway -- claude-runway mcp

Reads the OAuth token Claude Code already stores locally and calls Anthropic's
undocumented usage endpoint. No API key. The token is sent nowhere else.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Subcommands first: anything not starting with "-".
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "mcp":
			return serveMCP(stdin, stdout, stderr)
		case "doctor":
			return runDoctor(stdout)
		case "version":
			fmt.Fprintf(stdout, "%s\n", binVersion)
			return exitOK
		case "help":
			fmt.Fprintln(stdout, helpText)
			return exitOK
		default:
			fmt.Fprintf(stdout, "error: unknown command %q\nhelp: valid commands are doctor, mcp, version, help. Run `claude-runway --help`.\n", args[0])
			return exitUsage
		}
	}

	var opts renderOpts
	asJSON := false
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(stdout, helpText)
			return exitOK
		case a == "--version":
			fmt.Fprintf(stdout, "%s\n", binVersion)
			return exitOK
		case a == "--brief":
			opts.brief = true
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--fields="):
			fields, bad := parseFields(strings.TrimPrefix(a, "--fields="))
			if bad != "" {
				// Fail loud on an unknown field rather than silently dropping a column the
				// caller asked for and expects to parse.
				fmt.Fprintf(stdout, "error: unknown field %q\nhelp: available fields: %s\n", bad, strings.Join(knownFields(), ", "))
				return exitUsage
			}
			opts.fields = fields
		default:
			fmt.Fprintf(stdout, "error: unknown flag %q\nhelp: run `claude-runway --help` for the flag list\n", a)
			return exitUsage
		}
	}

	r := getUsage()
	now := time.Now()
	if asJSON {
		fmt.Fprintln(stdout, renderJSON(r, now))
	} else {
		if !opts.brief {
			opts.bin = binPath()
		}
		fmt.Fprint(stdout, renderTOON(r, now, opts))
	}
	if !r.ok {
		return describeFailure(r).exit
	}
	return exitOK
}

func parseFields(spec string) (fields []string, unknown string) {
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := fieldRenderers[f]; !ok {
			return nil, f
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return nil, spec
	}
	return fields, ""
}

// binPath is best-effort: the home view is more useful with it, and not worth failing over.
func binPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func runDoctor(stdout io.Writer) int {
	d := diagnose()
	// TOON, like every other output. Nothing here can reveal the token itself.
	fmt.Fprintf(stdout, "bin: %s\ndescription: Where claude-runway looks for Claude Code credentials.\n", binPath())
	fmt.Fprintf(stdout, "platform: %s\nusing: %s\n", d.platform, d.using)
	fmt.Fprintf(stdout, "sources[2]{source,location,state}:\n")
	fmt.Fprintf(stdout, "  file,%s,%s\n", orUnknown(d.path), d.file)
	fmt.Fprintf(stdout, "  keychain,%s,%s\n", keychainService, d.keychain)
	if !d.expiresAt.IsZero() {
		fmt.Fprintf(stdout, "token_expires_at: %s\ntoken_expires_in: %s\n",
			d.expiresAt.UTC().Format(time.RFC3339), fmtRelative(time.Until(d.expiresAt)))
	}
	if d.using == "none" {
		fmt.Fprintln(stdout, "error: no credentials found in either source")
		fmt.Fprintln(stdout, "help: sign in with `claude`, then re-run `claude-runway doctor`")
		return exitError
	}
	fmt.Fprintln(stdout, "help: Run `claude-runway` to read the meters")
	return exitOK
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
