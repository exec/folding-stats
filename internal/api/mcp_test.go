package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mcpDo(t *testing.T, srv *Server, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d for %s", rec.Code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unparseable response: %v\n%s", err, rec.Body.String())
	}
	return out
}

// mcpText returns a successful tool call's text, failing if the call errored.
func mcpText(t *testing.T, srv *Server, tool, args string) string {
	t.Helper()
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+
		tool+`","arguments":`+args+`}}`)
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", tool, out)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s: empty content", tool)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		t.Fatalf("%s(%s) errored: %s", tool, args, text)
	}
	return text
}

func TestMCPHandshake(t *testing.T) {
	srv := fixture(t)
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], mcpProtocolVersion)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("no tools capability advertised, so a client will never call tools/list")
	}
	// The instructions are how a model learns the data's shape before its first
	// call. Losing them is silent: every tool still works, slightly wrongly used.
	if s, _ := res["instructions"].(string); !strings.Contains(s, "hourly") {
		t.Error("instructions do not mention the refresh cadence")
	}

	// A notification has no id and must get no body back.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("notification got %d with %d bytes; want 202 and nothing",
			rec.Code, rec.Body.Len())
	}
}

// TestMCPToolsAreWellFormed guards the contract a model reads before it can call
// anything. A tool with no description or a malformed schema is not a broken call —
// it is a tool the model declines to use, or uses wrongly, with no error anywhere.
func TestMCPToolsAreWellFormed(t *testing.T) {
	srv := fixture(t)
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("only %d tools", len(tools))
	}
	seen := map[string]bool{}
	for _, raw := range tools {
		tl := raw.(map[string]any)
		name, _ := tl["name"].(string)
		if name == "" || seen[name] {
			t.Errorf("missing or duplicate tool name %q", name)
		}
		seen[name] = true
		if d, _ := tl["description"].(string); len(d) < 40 {
			t.Errorf("%s: description is too thin for a model to choose on: %q", name, d)
		}
		sch, ok := tl["inputSchema"].(map[string]any)
		if !ok || sch["type"] != "object" {
			t.Errorf("%s: inputSchema is not an object schema", name)
			continue
		}
		props, _ := sch["properties"].(map[string]any)
		// Every declared required field must exist in properties, or a client
		// generating arguments from the schema produces something unusable.
		for _, r := range toStrings(sch["required"]) {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: required field %q is not described in properties", name, r)
			}
		}
		for pn, p := range props {
			if d, _ := p.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s.%s has no description", name, pn)
			}
		}
	}
	for _, want := range []string{"search", "get_donor", "get_team", "leaderboard", "compare"} {
		if !seen[want] {
			t.Errorf("tool %q is missing", want)
		}
	}
}

func TestMCPToolsAnswerFromTheFixture(t *testing.T) {
	srv := fixture(t)

	if s := mcpText(t, srv, "search", `{"query":"overclockers"}`); !strings.Contains(s, "overclockers") {
		t.Errorf("search did not find the fixture team:\n%s", s)
	}
	if s := mcpText(t, srv, "get_donor", `{"name":"DH"}`); !strings.Contains(s, "DH") ||
		!strings.Contains(s, "Lifetime") {
		t.Errorf("get_donor output is not a profile:\n%s", s)
	}
	if s := mcpText(t, srv, "get_team", `{"team_id":32}`); !strings.Contains(s, "Members") {
		t.Errorf("get_team output is not a profile:\n%s", s)
	}
	if s := mcpText(t, srv, "leaderboard", `{"kind":"donors","limit":3}`); !strings.Contains(s, "Top") {
		t.Errorf("leaderboard produced nothing:\n%s", s)
	}
	if s := mcpText(t, srv, "compare", `{"kind":"teams","a":"32","b":"51"}`); !strings.Contains(s, "ahead by") {
		t.Errorf("compare did not state a gap:\n%s", s)
	}
	if s := mcpText(t, srv, "project_status", `{}`); !strings.Contains(s, "Donors") {
		t.Errorf("project_status produced nothing:\n%s", s)
	}

	// Every answer carries its own age. A model quoting a figure without knowing
	// how old it is is the specific failure this endpoint would otherwise invite,
	// and it cannot be caught downstream.
	for _, c := range []struct{ tool, args string }{
		{"search", `{"query":"overclockers"}`}, {"get_donor", `{"name":"DH"}`},
		{"get_team", `{"team_id":32}`}, {"leaderboard", `{"kind":"teams"}`},
		{"compare", `{"kind":"teams","a":"32","b":"51"}`}, {"project_status", `{}`},
	} {
		if !strings.Contains(mcpText(t, srv, c.tool, c.args), "Data as of") {
			t.Errorf("%s does not say how fresh its answer is", c.tool)
		}
	}
}

// TestMCPFailuresAreResultsNotProtocolErrors keeps a bad argument recoverable. A
// JSON-RPC error makes a client report that the server is broken; a tool result with
// isError lets the model read what went wrong and try again.
func TestMCPFailuresAreResultsNotProtocolErrors(t *testing.T) {
	srv := fixture(t)
	for _, c := range []struct{ tool, args, want string }{
		{"get_donor", `{"name":"nobody-by-that-name"}`, "search"},
		{"get_team", `{"team_id":999999}`, "no team"},
		{"leaderboard", `{"kind":"sideways"}`, "teams"},
		{"production_history", `{"scope":"team"}`, "team_id"},
		{"compare", `{"kind":"teams","a":"notanumber","b":"32"}`, "team number"},
		{"no_such_tool", `{}`, "tools/list"},
	} {
		out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+
			c.tool+`","arguments":`+c.args+`}}`)
		if _, isErr := out["error"]; isErr {
			t.Errorf("%s(%s) returned a protocol error; a client will call the server broken",
				c.tool, c.args)
			continue
		}
		res := out["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("%s(%s) reported success", c.tool, c.args)
			continue
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, c.want) {
			t.Errorf("%s(%s): message does not suggest a way forward (want %q):\n  %s",
				c.tool, c.args, c.want, text)
		}
	}

	// An unknown method is a genuine protocol error and should say so.
	if _, ok := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"no/such"}`)["error"]; !ok {
		t.Error("unknown method did not produce a JSON-RPC error")
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestMCPGetIsNegotiated covers the two very different callers of GET on this
// endpoint. A conforming MCP client asks for a stream by Accept and must be refused,
// because there is nothing to stream. A person who pasted the URL into a browser is
// not an error and should be sent somewhere that explains what they found.
func TestMCPGetIsNegotiated(t *testing.T) {
	srv := fixture(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("stream request got %d, want 405 — the spec requires refusal when "+
			"there are no server-initiated messages", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/agents" {
		t.Errorf("browser request got %d to %q, want 303 to /agents",
			rec.Code, rec.Header().Get("Location"))
	}
}
