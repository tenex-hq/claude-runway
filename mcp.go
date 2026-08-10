package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A minimal MCP server over stdio, stdlib only. Kept even though AXI argues the CLI is
// the cheaper integration, because a host application that already speaks MCP should not
// have to shell out, and the whole surface here is one read-only tool.
//
// stdio framing is newline-delimited JSON-RPC 2.0. One hard rule: stdout carries protocol
// traffic and nothing else, so diagnostics go to stderr.

const (
	protocolVersion = "2025-06-18"
	serverName      = "claude-runway"
)

var supportedProtocols = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// The wording is deliberate and load-bearing: it is what makes a model treat the number as
// a budget to pace against rather than a statistic to mention.
const checkUsageDescription = "Return how much of the user's Claude subscription allowance is LEFT for the session (5h) and weekly windows: percent left (100 = untouched, 0 = exhausted), time until each window resets, and a pace verdict. Pace compares budget left against time left in the window: positive headroom means the remaining budget comfortably covers the time to reset (you are ahead and safe), negative means you are burning faster than the clock and may run dry early. The top-level verdict (safe, caution, stop) already combines the windows for you. Use it to gate budget-limited work."

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func serveMCP(stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	reader := bufio.NewReader(stdin)
	enc := json.NewEncoder(stdout)
	// Go's encoder appends the newline the framing needs, and escaping HTML in tool text
	// would be pointless noise here.
	enc.SetEscapeHTML(false)

	send := func(v any) {
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(stderr, "claude-runway: write failed: %v\n", err)
		}
	}
	reply := func(id json.RawMessage, result any) { send(rpcResponse{JSONRPC: "2.0", ID: id, Result: result}) }
	fail := func(id json.RawMessage, code int, msg string) {
		send(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	}

	for {
		// ReadString rather than a Scanner: a Scanner has a fixed max token size and an
		// oversized initialize payload would silently end the loop.
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			// Requests are handled synchronously in arrival order, so responses come back
			// in order and an in-flight request can never be cut short by EOF.
			handleRPC(trimmed, reply, fail, send)
		}
		if err != nil {
			// io.EOF or a broken pipe: the client is gone, and everything read has already
			// been answered.
			return 0
		}
	}
}

func handleRPC(line string, reply func(json.RawMessage, any), fail func(json.RawMessage, int, string), send func(any)) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		// No id can be known, so per JSON-RPC this is a parse error against a null id.
		send(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	// A notification carries no id and must never be answered.
	isRequest := len(req.ID) > 0 && string(req.ID) != "null"

	switch req.Method {
	case "initialize":
		if !isRequest {
			return
		}
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersion
		if supportedProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		reply(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": binVersion},
		})
	case "ping":
		if isRequest {
			reply(req.ID, map[string]any{})
		}
	case "tools/list":
		if !isRequest {
			return
		}
		reply(req.ID, map[string]any{"tools": []any{map[string]any{
			"name":        "check_usage",
			"description": checkUsageDescription,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		}}})
	case "tools/call":
		if !isRequest {
			return
		}
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Name != "check_usage" {
			fail(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		r := getUsage()
		// brief: a model calling this in a loop should not pay for the discovery preamble
		// on every call.
		text := renderTOON(r, time.Now(), renderOpts{brief: true})
		result := map[string]any{"content": []toolContent{{Type: "text", Text: strings.TrimRight(text, "\n")}}}
		// A reading that could not be taken is not a protocol error: the model should see
		// the explanation and decide, so it comes back as tool content flagged isError.
		if !r.ok && r.reason != failProvider {
			result["isError"] = true
		}
		reply(req.ID, result)
	default:
		// notifications/initialized and cancellations land here and are correctly ignored.
		if isRequest {
			fail(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}
