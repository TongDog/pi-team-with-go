package experience

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const mcpInit = `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`
const mcpReady = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

type mcpTestReply struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// This subprocess runs the same CLI entry function as cmd/pigo-experience and
// exchanges actual stdin/stdout messages; no external host or API is involved.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("PIGO_TEST_MCP_CHILD") != "1" {
		return
	}
	os.Exit(Run([]string{"--root", os.Getenv("PIGO_TEST_MCP_ROOT"), "mcp"}, os.Stdin, os.Stdout, os.Stderr))
}

func TestMCPStdioLifecycleAndApprovedOnlySearch(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	e, err := store.Propose(Entry{Project: "demo", Problem: "pagination index mismatch", Solution: "use agreed index", Applicability: "list endpoints", Sources: []string{"test evidence"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Approve(e.ID); err != nil {
		t.Fatal(err)
	}
	draft, err := store.Propose(Entry{Project: "demo", Problem: "pagination draft", Solution: "unverified change", Applicability: "test", Sources: []string{"draft evidence"}})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMCPStdioHelper$")
	cmd.Env = append(os.Environ(), "PIGO_TEST_MCP_CHILD=1", "PIGO_TEST_MCP_ROOT="+root)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	decoder := json.NewDecoder(output)
	send := func(message string) {
		t.Helper()
		if _, err := fmt.Fprintln(input, message); err != nil {
			t.Fatal(err)
		}
	}
	read := func() mcpTestReply {
		t.Helper()
		var r mcpTestReply
		if err := decoder.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Version != "2.0" {
			t.Fatalf("nonprotocol stdout: %+v", r)
		}
		return r
	}
	send(mcpInit)
	reply := read()
	if reply.Error != nil || string(reply.ID) != `"init"` {
		t.Fatalf("initialize: %+v", reply)
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      map[string]string          `json:"serverInfo"`
	}
	if err := json.Unmarshal(reply.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != MCPProtocolVersion || init.ServerInfo["name"] != "pigo-experience" || len(init.Capabilities) != 1 || init.Capabilities["tools"] == nil {
		t.Fatalf("bad negotiation %+v", init)
	}
	send(mcpReady)
	send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	reply = read()
	if string(reply.ID) != "2" || string(reply.Result) != "{}" {
		t.Fatalf("notification response or bad ping: %+v", reply)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	reply = read()
	var listing struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Tools) != 4 {
		t.Fatalf("bad tools: %+v", listing)
	}
	for _, tool := range listing.Tools {
		if strings.Contains(tool.Name, "approve") || strings.Contains(tool.Name, "retire") || tool.InputSchema["type"] != "object" {
			t.Fatalf("unsafe/invalid tool %+v", tool)
		}
	}
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"experience_search","arguments":{"project":"demo","query":"pagination"}}}`)
	reply = read()
	var result mcpCallResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("bad call result %+v", result)
	}
	var entries []Entry
	if err := json.Unmarshal([]byte(result.Content[0].Text), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != e.ID {
		t.Fatalf("unapproved recall: %+v", entries)
	}
	send(fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"experience_approve","arguments":{"id":%q}}}`, draft.ID))
	reply = read()
	if reply.Error == nil || reply.Error.Code != -32602 {
		t.Fatalf("approval tool callable: %+v", reply)
	}
	send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"experience_propose","arguments":{"project":"demo","problem":"pagination candidate","solution":"adjust conversion","applicability":"fixture","sources":["fixture log"]}}}`)
	reply = read()
	if err := json.Unmarshal(reply.Result, &result); err != nil || result.IsError {
		t.Fatalf("propose: %+v %v", reply, err)
	}
	var proposed Entry
	if err := json.Unmarshal([]byte(result.Content[0].Text), &proposed); err != nil || proposed.Status != Draft {
		t.Fatalf("proposal not draft: %+v %v", proposed, err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("stdio shutdown: %v %s", err, logs.String())
	}
	if !strings.Contains(logs.String(), "transport=mcp operation=experience_search result=completed") {
		t.Fatalf("missing stderr phases: %s", logs.String())
	}
	if got, err := store.Get(draft.ID); err != nil || got.Status != Draft {
		t.Fatalf("draft approved through MCP: %+v %v", got, err)
	}
}

func mcpExchange(t *testing.T, lines ...string) []mcpTestReply {
	t.Helper()
	var output, logs bytes.Buffer
	if err := ServeMCP(Store{Root: t.TempDir()}, strings.NewReader(strings.Join(lines, "\n")+"\n"), &output, &logs); err != nil {
		t.Fatal(err)
	}
	var replies []mcpTestReply
	decoder := json.NewDecoder(&output)
	for {
		var reply mcpTestReply
		err := decoder.Decode(&reply)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, reply)
	}
	return replies
}

func TestMCPProtocolErrorsAndRecovery(t *testing.T) {
	cases := []struct {
		name, line string
		code       int
	}{
		{"malformed", `{`, -32700},
		{"array", `[]`, -32600},
		{"null", `null`, -32600},
		{"missing version", `{"id":1,"method":"ping"}`, -32600},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, -32600},
		{"fractional id", `{"jsonrpc":"2.0","id":1.2,"method":"ping"}`, -32600},
		{"boolean id", `{"jsonrpc":"2.0","id":true,"method":"ping"}`, -32600},
		{"params array", `{"jsonrpc":"2.0","id":1,"method":"ping","params":[]}`, -32602},
		{"premature tools", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, -32002},
		{"incomplete initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`, -32602},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"unknown"}`, -32601},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replies := mcpExchange(t, tc.line, `{"jsonrpc":"2.0","id":"recovery","method":"ping"}`)
			if len(replies) != 2 || replies[0].Error == nil || replies[0].Error.Code != tc.code || replies[1].Error != nil || string(replies[1].ID) != `"recovery"` {
				t.Fatalf("bad protocol handling: %+v", replies)
			}
		})
	}
}

func TestMCPLifecycleAndNotifications(t *testing.T) {
	replies := mcpExchange(t, mcpReady, mcpInit, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, mcpReady, `{"jsonrpc":"2.0","method":"notifications/unknown"}`, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, mcpInit)
	if len(replies) != 4 || replies[1].Error == nil || replies[1].Error.Code != -32002 || replies[2].Error != nil || replies[3].Error == nil {
		t.Fatalf("lifecycle failed: %+v", replies)
	}
	future := strings.Replace(mcpInit, MCPProtocolVersion, "2099-01-01", 1)
	replies = mcpExchange(t, future)
	if len(replies) != 1 || replies[0].Error != nil || !bytes.Contains(replies[0].Result, []byte(MCPProtocolVersion)) {
		t.Fatalf("version negotiation: %+v", replies)
	}
}

func TestMCPToolValidation(t *testing.T) {
	cases := []struct {
		params   string
		protocol bool
	}{
		{`{"name":"experience_search","arguments":[]}`, true},
		{`{"name":"experience_search","arguments":null}`, true},
		{`{"name":"experience_search","arguments":{}}`, false},
		{`{"name":"experience_search","arguments":{"project":"demo","limit":"five"}}`, false},
		{`{"name":"experience_search","arguments":{"project":"demo","modules":[null]}}`, false},
		{`{"name":"experience_propose","arguments":{"project":"demo","problem":"p","solution":"s","applicability":"a","sources":["e"],"status":"approved"}}`, false},
		{`{"name":"experience_get","arguments":{"id":"missing"}}`, false},
		{`{"name":"experience_approve","arguments":{"id":"missing"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.params, func(t *testing.T) {
			replies := mcpExchange(t, mcpInit, mcpReady, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+tc.params+`}`)
			r := replies[len(replies)-1]
			if tc.protocol {
				if r.Error == nil || r.Error.Code != -32602 {
					t.Fatalf("expected protocol error: %+v", r)
				}
			} else {
				var result mcpCallResult
				if err := json.Unmarshal(r.Result, &result); err != nil || r.Error != nil || !result.IsError {
					t.Fatalf("expected tool failure: %+v %v", r, err)
				}
			}
		})
	}
}

func TestMCPNotificationCannotPropose(t *testing.T) {
	store := Store{Root: t.TempDir()}
	var output bytes.Buffer
	input := mcpInit + "\n" + mcpReady + "\n" + `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"experience_propose","arguments":{"project":"demo","problem":"p","solution":"s","applicability":"a","sources":["e"]}}}` + "\n"
	if err := ServeMCP(store, strings.NewReader(input), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("demo")
	if err != nil || len(entries) != 0 {
		t.Fatalf("notification mutated store: %+v %v", entries, err)
	}
	if bytes.Count(output.Bytes(), []byte("\n")) != 1 {
		t.Fatal("notification generated a response")
	}
}

func TestMCPCLIRejectsArgumentsAndOversizedInput(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"--root", t.TempDir(), "mcp", "unexpected"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("unexpected positional accepted: %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--root", t.TempDir(), "mcp"}, strings.NewReader(strings.Repeat("x", (4<<20)+1)), &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatalf("oversized input: %d %s", code, out.String())
	}
}
