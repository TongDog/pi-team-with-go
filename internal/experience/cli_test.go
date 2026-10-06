package experience

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCLI(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	data, _ := json.Marshal(candidate())
	if code := Run([]string{"--root", root, "propose", "-"}, bytes.NewReader(data), &out, &errOut); code != 0 {
		t.Fatalf("propose: %d %s", code, errOut.String())
	}
	var e Entry
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "result=completed") {
		t.Fatal("missing stage log")
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"approve", e.ID, "--root", root}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("approve: %s", errOut.String())
	}
	out.Reset()
	if code := Run([]string{"--root", root, "search", "--project", "repo", "--query", "pagination"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatal(code)
	}
	var entries []Entry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil || len(entries) != 1 {
		t.Fatalf("search: %s %v", out.String(), err)
	}
	out.Reset()
	if code := Run([]string{"--root", root, "list"}, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("unscoped list succeeded")
	}
}
func TestProtocol(t *testing.T) {
	s := Store{Root: t.TempDir()}
	data, _ := json.Marshal(candidate())
	var input strings.Builder
	input.WriteString("{bad\n")
	input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"experience_propose","arguments":` + string(data) + "}}\n")
	input.WriteString(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"experience_search","arguments":{"project":"repo"}}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"experience_approve","arguments":{}}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","method":"event","params":{}}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","method":"shutdown"}` + "\n")
	var out, logs bytes.Buffer
	if err := Serve(s, strings.NewReader(input.String()), &out, &logs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("unexpected protocol output: %s", out.String())
	}
	var parse rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &parse); err != nil || parse.Error == nil || parse.Error.Code != -32700 {
		t.Fatalf("parse error: %s", lines[0])
	}
	if !strings.Contains(lines[1], "experience_propose") || strings.Contains(lines[1], "experience_approve") {
		t.Fatal("unsafe manifest")
	}
	var response struct {
		Result callResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.Content != "[]" {
		t.Fatalf("draft visible in recall: %s", lines[3])
	}
	if err := json.Unmarshal([]byte(lines[4]), &response); err != nil || !response.Result.IsError {
		t.Fatalf("approval tool accessible: %s", lines[4])
	}
	if !strings.Contains(logs.String(), "operation=experience_propose") {
		t.Fatal("missing stderr logs")
	}
}
