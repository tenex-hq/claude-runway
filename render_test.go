package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleReading(now time.Time) reading {
	return reading{
		ok: true,
		buckets: []bucket{
			{key: "five_hour", utilization: 40, hasUtil: true, resetsAt: now.Add(2 * time.Hour), severity: "normal"},
			{key: "seven_day", utilization: 27, hasUtil: true, resetsAt: now.Add(5 * 24 * time.Hour), severity: "normal"},
		},
		transport: "http", credSource: "file", plan: "team", fetchedAt: now,
	}
}

func TestRenderTOONShape(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	out := renderTOON(sampleReading(now), now, renderOpts{bin: "/usr/local/bin/claude-runway"})

	for _, want := range []string{
		"bin: /usr/local/bin/claude-runway",
		"description: ",
		"verdict: ",
		"tightest: ",
		"windows[2]{window,left_pct,resets_in,pace,headroom_pts}:",
		"  session_5h,60,2h,",
		"  weekly,73,5d,",
		"help[2]:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// Remaining, never consumed: 40 utilization must surface as 60 left and 40 must not
	// appear as this window's percentage.
	if strings.Contains(out, "session_5h,40,") {
		t.Errorf("rendered utilization instead of remaining percent\n---\n%s", out)
	}
}

func TestRenderTOONBriefDropsPreamble(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	out := renderTOON(sampleReading(now), now, renderOpts{brief: true, bin: "/x"})
	for _, unwanted := range []string{"bin:", "description:", "help["} {
		if strings.Contains(out, unwanted) {
			t.Errorf("--brief output should not contain %q\n---\n%s", unwanted, out)
		}
	}
	// It must still carry the decision and the data.
	if !strings.Contains(out, "verdict: ") || !strings.Contains(out, "windows[2]") {
		t.Errorf("--brief dropped the payload\n---\n%s", out)
	}
}

func TestRenderTOONCustomFields(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	out := renderTOON(sampleReading(now), now, renderOpts{brief: true, fields: []string{"window", "left_pct", "resets_at"}})
	if !strings.Contains(out, "windows[2]{window,left_pct,resets_at}:") {
		t.Errorf("header does not reflect requested fields\n---\n%s", out)
	}
	if !strings.Contains(out, "session_5h,60,2026-08-10T14:00:00Z") {
		t.Errorf("resets_at not rendered as an absolute timestamp\n---\n%s", out)
	}
}

// The single most important property: a reading that could not be taken must never render
// anything a caller could mistake for a percentage, and must never be silent.
func TestFailuresNeverRenderNumbers(t *testing.T) {
	now := time.Now()
	for _, r := range []reading{
		{reason: failNoCreds},
		{reason: failExpired, detail: "2026-08-10T14:55:20Z"},
		{reason: failHTTP, status: 500, detail: "boom"},
		{reason: failTransport, detail: "dial tcp: no route to host"},
		{reason: failBadPayload, detail: "response was not JSON"},
	} {
		out := renderTOON(r, now, renderOpts{brief: true})
		if strings.TrimSpace(out) == "" {
			t.Fatalf("%s rendered nothing: an agent cannot tell that from a crash", r.reason)
		}
		if !strings.HasPrefix(out, "error: ") {
			t.Errorf("%s should render as a structured error, got:\n%s", r.reason, out)
		}
		if strings.Contains(out, "left_pct") || strings.Contains(out, "windows[") {
			t.Errorf("%s leaked a data shape into a failure:\n%s", r.reason, out)
		}
		if describeFailure(r).exit != exitError {
			t.Errorf("%s should exit %d", r.reason, exitError)
		}
	}
}

// A custom provider is a definite answer, not a failure: it must not look like an error and
// must not make a script retry.
func TestCustomProviderIsADefiniteAnswer(t *testing.T) {
	r := reading{reason: failProvider, detail: "https://example.invalid"}
	out := renderTOON(r, time.Now(), renderOpts{brief: true})
	if !strings.Contains(out, "verdict: not-applicable") {
		t.Errorf("custom provider should render a verdict, got:\n%s", out)
	}
	if strings.Contains(out, "error:") {
		t.Errorf("custom provider should not render as an error, got:\n%s", out)
	}
	if got := describeFailure(r).exit; got != exitOK {
		t.Errorf("custom provider exit = %d, want %d so callers proceed instead of retrying", got, exitOK)
	}
}

func TestRenderJSONIsValidAndCarriesRemaining(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	var out jsonOut
	if err := json.Unmarshal([]byte(renderJSON(sampleReading(now), now)), &out); err != nil {
		t.Fatalf("JSON output does not parse: %v", err)
	}
	if out.Status != "ok" || out.Verdict == "" || len(out.Windows) != 2 {
		t.Fatalf("unexpected JSON: %+v", out)
	}
	if out.Windows[0].PercentLeft == nil || *out.Windows[0].PercentLeft != 60 {
		t.Errorf("first window percent_left = %v, want 60", out.Windows[0].PercentLeft)
	}
	if out.Transport != "http" || out.CredFrom != "file" {
		t.Errorf("provenance lost: transport=%q credential_source=%q", out.Transport, out.CredFrom)
	}

	var errOut jsonOut
	if err := json.Unmarshal([]byte(renderJSON(reading{reason: failNoCreds}, now)), &errOut); err != nil {
		t.Fatalf("error JSON does not parse: %v", err)
	}
	if errOut.Status != "error" || errOut.Reason != string(failNoCreds) || len(errOut.Windows) != 0 {
		t.Errorf("unexpected error JSON: %+v", errOut)
	}
}

// --- CLI contract (AXI exit codes, errors on stdout) --------------------------------

func TestUsageErrorsGoToStdoutWithExit2(t *testing.T) {
	for _, args := range [][]string{{"--nope"}, {"bogus"}, {"--fields=window,not_a_field"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, code, exitUsage)
		}
		if !strings.HasPrefix(stdout.String(), "error: ") {
			t.Errorf("run(%v) stdout should start with `error: `, got %q", args, stdout.String())
		}
		if !strings.Contains(stdout.String(), "help: ") {
			t.Errorf("run(%v) should suggest a fix, got %q", args, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("run(%v) wrote to stderr: %q", args, stderr.String())
		}
	}
}

func TestHelpAndVersionExitZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"version"}, {"--version"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
		if stdout.Len() == 0 {
			t.Errorf("run(%v) printed nothing", args)
		}
	}
}

// --- MCP protocol -------------------------------------------------------------------
// Covers everything that does not need the network. tools/call is exercised by hand
// against the live endpoint, since faking it would only test the fake.

func TestMCPHandshake(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"no/such/method"}`,
		`garbage`,
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("serveMCP = %d, want 0", code)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	// One response per request, and nothing for the notification.
	if len(lines) != 6 {
		t.Fatalf("got %d responses, want 6 (the notification must not be answered):\n%s", len(lines), stdout.String())
	}

	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatalf("initialize response does not parse: %v", err)
	}
	if init.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want the client's own 2024-11-05 echoed back", init.Result.ProtocolVersion)
	}
	if init.Result.ServerInfo.Name != serverName {
		t.Errorf("serverInfo.name = %q", init.Result.ServerInfo.Name)
	}

	var tools struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &tools); err != nil {
		t.Fatalf("tools/list does not parse: %v", err)
	}
	if len(tools.Result.Tools) != 1 || tools.Result.Tools[0].Name != "check_usage" {
		t.Fatalf("unexpected tool list: %+v", tools.Result.Tools)
	}
	if !strings.Contains(tools.Result.Tools[0].Description, "LEFT") {
		t.Error("the tool description must say the number is remaining, not consumed")
	}

	// Unknown tool, unknown method, and unparseable input each get the right JSON-RPC code.
	for i, wantCode := range map[int]int{3: -32602, 4: -32601, 5: -32700} {
		var e struct {
			Error *rpcError `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			t.Fatalf("line %d does not parse: %v", i, err)
		}
		if e.Error == nil || e.Error.Code != wantCode {
			t.Errorf("line %d error = %+v, want code %d", i, e.Error, wantCode)
		}
	}
}

// Regression guard for the bug the Node prototype had: a request still in flight when
// stdin closes must be answered before the process exits. Here stdin ends immediately
// after the request, with no trailing newline.
func TestMCPAnswersRequestWithoutTrailingNewline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if code := serveMCP(strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("serveMCP = %d", code)
	}
	if !strings.Contains(stdout.String(), `"id":1`) {
		t.Errorf("request on an unterminated final line went unanswered, got %q", stdout.String())
	}
}
