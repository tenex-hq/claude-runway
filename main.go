// Package main implements claude-runway: how much of a Claude subscription allowance is
// left, and whether it will last to the window reset.
//
// Everything here is unexported on purpose. This is a command, not a library, and nothing
// outside it can import `package main`, so an exported identifier would only imply an API
// that does not exist.
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
//
// Left as the sentinel rather than a plausible-looking number: an un-stamped build falls back
// to the module version and VCS revision instead (see version.go), which cannot go stale.
var binVersion = devVersion

// Resolved once. Reported by `version`, `--version`, and MCP serverInfo, so all three agree.
var reportedVersion = resolveVersion()

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
  claude-runway install-skills Install the Claude Code skill (--user or --project)
  claude-runway version        Print the version

install-skills flags:
  --user                      Install to ~/.claude/skills (every project)
  --project                   Install to ./.claude/skills (this project only)
  --dry-run                   Report what would change, write nothing
  --force                     Overwrite a file that exists with different content

  Additive: it only writes under <scope>/.claude/skills/claude-runway/ and never
  touches settings.json or any existing hook configuration.

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
	// Environment first, before any entry point can read the cache with a TTL nobody asked
	// for. Checked here rather than per subcommand so `mcp` is covered too: it consults the
	// cache on every tools/call, and failing at startup means nothing is ever written into a
	// live JSON-RPC stream. The cost is that a malformed value also refuses --help, which is
	// why the message states the variable, the accepted values and the default, instead of
	// pointing at help the caller cannot currently reach.
	if problem := cacheTTLEnvError(); problem != "" {
		// stdout everywhere except `mcp`, where stdout is the JSON-RPC channel and mcp.go's
		// one hard rule is that nothing but protocol traffic goes there. Exiting before the
		// stream starts means nothing is corrupted mid-conversation, but a client still reads
		// whatever was written, and one bare `error:` line is a parse error rather than an
		// explanation. On that path the message belongs on stderr, where the MCP server sends
		// every other diagnostic.
		w := stdout
		if len(args) > 0 && args[0] == "mcp" {
			w = stderr
		}
		fmt.Fprintf(w, "error: %s\nhelp: set it to a whole number of seconds (%d is the default, 0 always reads live), or unset it\n",
			problem, int(cacheTTL.Seconds()))
		return exitUsage
	}

	// Subcommands first: anything not starting with "-".
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "mcp":
			return serveMCP(stdin, stdout, stderr)
		case "doctor":
			// doctor and version take no flags at all, so any argument is a mistake. Both used
			// to accept and ignore one: `doctor --totally-bogus` printed the normal output and
			// exited 0, while install-skills and the root command already exited 2 for the
			// same typo. Three behaviours for one user error.
			if len(args) > 1 {
				return rejectSubcommandArgs(stdout, "doctor", args[1])
			}
			return runDoctor(stdout)
		case "install-skills":
			return runInstallSkills(args[1:], stdout)
		case "version":
			if len(args) > 1 {
				return rejectSubcommandArgs(stdout, "version", args[1])
			}
			fmt.Fprintf(stdout, "%s\n", reportedVersion)
			return exitOK
		case "help":
			// Left permissive on purpose, unlike doctor and version: `claude-runway help
			// --fields` is someone asking what a flag does, and printing the help text answers
			// that, where exiting 2 would answer nothing.
			fmt.Fprintln(stdout, helpText)
			return exitOK
		default:
			fmt.Fprintf(stdout, "error: unknown command %q\nhelp: valid commands are doctor, mcp, install-skills, version, help. Run `claude-runway --help`.\n", args[0])
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
			fmt.Fprintf(stdout, "%s\n", reportedVersion)
			return exitOK
		case a == "--brief":
			opts.brief = true
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--fields="):
			fields, err := parseFields(strings.TrimPrefix(a, "--fields="))
			if err != nil {
				// Fail loud on an unknown field rather than silently dropping a column the
				// caller asked for and expects to parse.
				fmt.Fprintf(stdout, "error: %s\nhelp: available fields: %s\n", err, strings.Join(knownFields(), ", "))
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

// parseFields resolves a --fields spec, or reports why it cannot. The two failures are worded
// apart because they call for different fixes: an unknown name is a typo in a column, while an
// empty list is a caller that built the flag out of a variable that turned out to be empty.
// "unknown field" would be the wrong thing to tell the second one.
//
// The empty case was previously the one hole in the fail-loud rule: `--fields=` returned no
// fields and no complaint, so it fell through to the default set, silently ignoring what the
// caller asked for. (`--fields=,,,` did already error, just under the unknown-field wording.)
func parseFields(spec string) ([]string, error) {
	var fields []string
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := fieldRenderers[f]; !ok {
			return nil, fmt.Errorf("unknown field %q", f)
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("--fields= names no fields")
	}
	return fields, nil
}

// rejectSubcommandArgs is the shared refusal for subcommands that take nothing, so the wording
// and the exit code cannot drift between them.
func rejectSubcommandArgs(stdout io.Writer, cmd, arg string) int {
	fmt.Fprintf(stdout, "error: unknown argument %q for `%s`\nhelp: `claude-runway %s` takes no flags. Run `claude-runway --help` for the flags the root command accepts.\n", arg, cmd, cmd)
	return exitUsage
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
