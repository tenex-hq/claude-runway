package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The CLI contracts an agent depends on: exit 2 for a usage error, the explanation on stdout
// where a caller capturing stdout will see it, and stderr left empty.

// runCLI drives run() with an empty stdin and returns the exit code and both streams.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// A --fields spec that names nothing and one that names something unknown are both refused, and
// refused differently: an unknown name is a typo in a column, an empty list is a caller that
// built the flag out of a variable that turned out to be empty. Silently falling back to the
// default set would hand back columns the caller did not ask for and will not parse.
func TestBadFieldSpecIsRefusedWithTheFieldList(t *testing.T) {
	cases := []struct {
		arg      string
		wantText string
		notText  string
	}{
		{"--fields=", "--fields= names no fields", "unknown field"},
		{"--fields=,,,", "--fields= names no fields", "unknown field"},
		{"--fields=window,nope", `unknown field "nope"`, "names no fields"},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "")
			code, stdout, stderr := runCLI(t, c.arg)
			if code != exitUsage {
				t.Errorf("exit = %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(stdout, "error: ") {
				t.Errorf("stdout should start with `error: `, got %q", stdout)
			}
			if !strings.Contains(stdout, c.wantText) {
				t.Errorf("stdout should explain the problem with %q, got %q", c.wantText, stdout)
			}
			if strings.Contains(stdout, c.notText) {
				t.Errorf("stdout uses the wrong wording (%q) for %s: %q", c.notText, c.arg, stdout)
			}
			// The fix is a field name, so the message has to carry the list of them.
			if !strings.Contains(stdout, "help: available fields: ") {
				t.Errorf("stdout should list the available fields, got %q", stdout)
			}
			for _, f := range knownFields() {
				if !strings.Contains(stdout, f) {
					t.Errorf("the field list omits %q: %q", f, stdout)
				}
			}
			if stderr != "" {
				t.Errorf("wrote to stderr: %q", stderr)
			}
			// A refused spec must not also print a reading.
			if strings.Contains(stdout, "windows[") {
				t.Errorf("a refused --fields spec still produced output: %q", stdout)
			}
		})
	}
}

// A valid spec still has to work, or the guard above would be indistinguishable from a broken
// flag. Checked through parseFields so this needs no network.
func TestValidFieldSpecIsAccepted(t *testing.T) {
	got, err := parseFields("window, left_pct ,resets_at")
	if err != nil {
		t.Fatalf("a valid spec was refused: %v", err)
	}
	want := []string{"window", "left_pct", "resets_at"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q: the caller's order is the column order", i, got[i], want[i])
		}
	}
}

// doctor and version take nothing, so any argument is a mistake. They used to accept and ignore
// one, printing normal output and exiting 0, while the root command exited 2 for the same typo:
// three behaviours for one user error.
func TestSubcommandsThatTakeNoFlagsRefuseArguments(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "--anything"},
		{"doctor", "--json"},
		{"version", "--anything"},
		{"version", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "")
			code, stdout, stderr := runCLI(t, args...)
			if code != exitUsage {
				t.Errorf("exit = %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(stdout, "error: unknown argument ") {
				t.Errorf("stdout should name the offending argument, got %q", stdout)
			}
			if !strings.Contains(stdout, "help: ") || !strings.Contains(stdout, "takes no flags") {
				t.Errorf("stdout should say the subcommand takes no flags, got %q", stdout)
			}
			if !strings.Contains(stdout, args[1]) {
				t.Errorf("stdout should quote the argument the caller passed, got %q", stdout)
			}
			if stderr != "" {
				t.Errorf("wrote to stderr: %q", stderr)
			}
			// Refused before doing the work: no diagnosis and no version number came out.
			if strings.Contains(stdout, "using: ") || strings.Contains(stdout, "sources[") {
				t.Errorf("the subcommand ran anyway: %q", stdout)
			}
		})
	}
}

// help stays permissive on purpose, unlike doctor and version: `claude-runway help --fields` is
// someone asking what a flag does, and printing the help text answers that, where exiting 2
// would answer nothing.
func TestHelpStaysPermissiveWithArguments(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"help", "--anything"}, {"help", "--fields"}} {
		t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "")
		code, stdout, stderr := runCLI(t, args...)
		if code != exitOK {
			t.Errorf("run(%v) = %d, want 0: help is deliberately permissive", args, code)
		}
		if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "--fields=a,b,c") {
			t.Errorf("run(%v) did not print the help text: %q", args, stdout)
		}
		if stderr != "" {
			t.Errorf("run(%v) wrote to stderr: %q", args, stderr)
		}
	}
}

// A TTL the caller asked for and did not get is a silent lie about how fresh the numbers are.
// Refusing at startup is deliberate, and so is the cost: it also refuses --help, which is why
// the message has to state the variable, what is accepted and the default, rather than pointing
// at help the caller cannot currently reach.
func TestMalformedCacheSecondsIsRefusedBeforeAnythingElse(t *testing.T) {
	for _, value := range []string{"banana", "-5", "5.5", " ", "1e3"} {
		t.Run(value, func(t *testing.T) {
			isolateCache(t)
			t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", value)
			// Even --help, and even a subcommand that never reads the cache itself.
			for _, args := range [][]string{{}, {"--help"}, {"doctor"}, {"version"}} {
				code, stdout, stderr := runCLI(t, args...)
				if code != exitUsage {
					t.Fatalf("run(%v) = %d, want %d with CLAUDE_RUNWAY_CACHE_SECONDS=%q", args, code, exitUsage, value)
				}
				if !strings.Contains(stdout, "CLAUDE_RUNWAY_CACHE_SECONDS") {
					t.Errorf("run(%v) does not name the variable: %q", args, stdout)
				}
				if !strings.Contains(stdout, "300") {
					t.Errorf("run(%v) does not state the default: %q", args, stdout)
				}
				if !strings.Contains(stdout, "help: ") || !strings.Contains(stdout, "unset it") {
					t.Errorf("run(%v) does not say how to fix it: %q", args, stdout)
				}
				if stderr != "" {
					t.Errorf("run(%v) wrote to stderr: %q", args, stderr)
				}
			}
		})
	}
	// The negative case gets its own wording, because "not a whole number" would be wrong.
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "-5")
	if _, stdout, _ := runCLI(t, "--help"); !strings.Contains(stdout, "negative") {
		t.Errorf("a negative TTL should be described as negative: %q", stdout)
	}
}

// The same refusal, on the one path where it cannot use stdout: for `mcp`, stdout is the
// JSON-RPC channel, and a bare `error:` line there is a parse error to the client rather than an
// explanation. So the message goes to stderr, where the MCP server sends every other diagnostic,
// and stdout stays byte-for-byte empty.
func TestMalformedCacheSecondsKeepsTheMCPChannelClean(t *testing.T) {
	isolateCache(t)
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "banana")
	code, stdout, stderr := runCLI(t, "mcp")
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout carries JSON-RPC traffic only, but got %q", stdout)
	}
	if !strings.Contains(stderr, "CLAUDE_RUNWAY_CACHE_SECONDS") || !strings.Contains(stderr, "help: ") {
		t.Errorf("stderr should carry the explanation, got %q", stderr)
	}
}

// And a usable value has to keep working, or the guard above would be indistinguishable from a
// tool that refuses the variable outright. `--help` is used because it exercises the same
// startup check without touching the network.
func TestUsableCacheSecondsIsAccepted(t *testing.T) {
	for _, value := range []string{"", "0", "30", "3600"} {
		isolateCache(t)
		t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", value)
		if problem := cacheTTLEnvError(); problem != "" {
			t.Errorf("CLAUDE_RUNWAY_CACHE_SECONDS=%q was refused: %s", value, problem)
		}
		code, stdout, stderr := runCLI(t, "--help")
		if code != exitOK {
			t.Errorf("run(--help) = %d with CLAUDE_RUNWAY_CACHE_SECONDS=%q: %q", code, value, stdout)
		}
		if stderr != "" {
			t.Errorf("wrote to stderr: %q", stderr)
		}
	}
	// 0 means "always read live", so it must survive as 0 and not fall back to the default.
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "0")
	if got := cacheTTLFromEnv(); got != 0 {
		t.Errorf("cacheTTLFromEnv() = %v with the variable set to 0, want 0", got)
	}
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "30")
	if got := cacheTTLFromEnv().Seconds(); got != 30 {
		t.Errorf("cacheTTLFromEnv() = %vs, want 30s", got)
	}
	// A value the check refuses still has a defined fallback, since describeFailure reads it to
	// build a help string and cannot report an error from there.
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "banana")
	if got := cacheTTLFromEnv(); got != cacheTTL {
		t.Errorf("cacheTTLFromEnv() = %v for a malformed value, want the %v default", got, cacheTTL)
	}
}

// --- MCP tools/call, against a stubbed endpoint ---------------------------------------
// This is the one MCP path that reads the network, so it used to be exercised by hand against
// the live endpoint. With the transport pointable at a test server it can be asserted here.

func TestMCPToolCallReturnsTheReadingAsContent(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	now := time.Now()
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, usagePayloadJSON(now))
	})

	var stdout, stderr bytes.Buffer
	in := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"check_usage"}}` + "\n"
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("serveMCP = %d", code)
	}
	var res struct {
		Result struct {
			Content []toolContent `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("tools/call response does not parse: %v\n%s", err, stdout.String())
	}
	if res.Error != nil {
		t.Fatalf("tools/call failed: %+v", res.Error)
	}
	if len(res.Result.Content) != 1 || res.Result.Content[0].Type != "text" {
		t.Fatalf("unexpected content: %+v", res.Result.Content)
	}
	text := res.Result.Content[0].Text
	for _, want := range []string{"verdict: ", "windows[2]{", "session_5h,60,"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool content missing %q:\n%s", want, text)
		}
	}
	// brief: a model calling this in a loop should not pay for the discovery preamble every time.
	for _, unwanted := range []string{"description: ", "help[2]:"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("tool content carries the preamble %q:\n%s", unwanted, text)
		}
	}
	if res.Result.IsError {
		t.Error("a successful reading must not be flagged as an error")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be quiet on a successful call: %q", stderr.String())
	}
}

// A reading that could not be taken is not a protocol error: the model has to see the
// explanation and decide, so it comes back as tool content flagged isError rather than as a
// JSON-RPC failure the host would swallow.
func TestMCPToolCallFlagsAFailedReadingWithoutBreakingTheProtocol(t *testing.T) {
	netTestEnv(t)
	plantCredentialFile(t, plantedAuth, time.Now().Add(time.Hour).UnixMilli())
	startUsageServer(t, func(_ *recorder, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	var stdout, stderr bytes.Buffer
	in := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"check_usage"}}` + "\n"
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("serveMCP = %d", code)
	}
	var res struct {
		Result struct {
			Content []toolContent `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("response does not parse: %v\n%s", err, stdout.String())
	}
	if res.Error != nil {
		t.Fatalf("a failed reading became a protocol error: %+v", res.Error)
	}
	if !res.Result.IsError {
		t.Error("a failed reading must be flagged isError so the model does not read it as data")
	}
	if len(res.Result.Content) != 1 {
		t.Fatalf("unexpected content: %+v", res.Result.Content)
	}
	text := res.Result.Content[0].Text
	if !strings.Contains(text, "NOT your subscription allowance") {
		t.Errorf("the model must be told a 429 is not an exhausted budget:\n%s", text)
	}
	if strings.Contains(text, "windows[") {
		t.Errorf("a failure must not carry a data shape:\n%s", text)
	}
}

// An unknown subcommand is a usage error, not an attempt to read the meters.
func TestUnknownSubcommandNamesTheValidOnes(t *testing.T) {
	t.Setenv("CLAUDE_RUNWAY_CACHE_SECONDS", "")
	code, stdout, stderr := runCLI(t, "doctr")
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stdout, `unknown command "doctr"`) {
		t.Errorf("stdout should quote the command, got %q", stdout)
	}
	for _, cmd := range []string{"doctor", "mcp", "install-skills", "version", "help"} {
		if !strings.Contains(stdout, cmd) {
			t.Errorf("the help line omits the valid command %q: %q", cmd, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("wrote to stderr: %q", stderr)
	}
}
