package teamcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/team"
)

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}
func TestArguments(t *testing.T) {
	cases := []struct {
		args []string
		code int
	}{
		{nil, 0}, {[]string{"--help"}, 0}, {[]string{"run", "--help"}, 0}, {[]string{"resume", "-h"}, 0},
		{[]string{"wat"}, 2}, {[]string{"run"}, 2}, {[]string{"status"}, 2}, {[]string{"contract", "--run-dir", "unused"}, 2},
		{[]string{"status", "--run-dir", "unused", "stray"}, 2}, {[]string{"run", "--unknown"}, 2},
		{[]string{"run", "--project", "unused", "--spec", "unused"}, 2},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, _, _ := invoke(t, tc.args...)
			if code != tc.code {
				t.Fatalf("code=%d want %d", code, tc.code)
			}
		})
	}
}
func TestInvalidSpecDoesNotCreateRun(t *testing.T) {
	root := t.TempDir()
	spec := filepath.Join(root, "bad.json")
	run := filepath.Join(root, "run")
	if err := os.WriteFile(spec, []byte(`{"version":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := invoke(t, "run", "--spec", spec, "--project", root, "--run-dir", run, "--model", "unused")
	if code != 2 || out != "" {
		t.Fatalf("code=%d output=%q", code, out)
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatalf("invalid spec created run: %v", err)
	}
}
func TestStatusIsReadOnlyJSON(t *testing.T) {
	spec, err := team.LoadSpec("../../../examples/team/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := &team.State{Version: 1, ID: "test-run", ProjectDir: root, Spec: spec, Status: "pending", Phase: "alignment", Roles: map[string]team.RoleState{}}
	if err = state.Save(root); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := invoke(t, "status", "--run-dir", root)
	if code != 0 || diagnostic != "" {
		t.Fatalf("code=%d stderr=%q", code, diagnostic)
	}
	var result team.State
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != "test-run" || result.Status != "pending" {
		t.Fatalf("unexpected state: %+v", result)
	}
	after, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("status mutated state")
	}
}
func TestMissingCredentialsAreNotPrinted(t *testing.T) {
	const variable = "PIGO_TEAM_TEST_EMPTY_KEY"
	t.Setenv(variable, "")
	code, out, diagnostic := invoke(t, "resume", "--run-dir", t.TempDir(), "--model", "test", "--api-key-env", variable)
	if code != 2 || out != "" || strings.Contains(diagnostic, variable) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out, diagnostic)
	}
}
func TestContractRejectsTrailingJSON(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "contract.json")
	spec, err := team.LoadSpec("../../../examples/team/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(spec.Contract)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, append(data, []byte(" {}")...), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic := invoke(t, "contract", "--run-dir", root, "--file", file)
	if code != 2 || !strings.Contains(diagnostic, "exactly one") {
		t.Fatalf("code=%d error=%s", code, diagnostic)
	}
}
func TestExperienceDelegation(t *testing.T) {
	code, out, diagnostic := invoke(t, "experience", "--root", t.TempDir(), "list", "--project", "team-users-demo")
	if code != 0 || !json.Valid([]byte(out)) || !strings.Contains(diagnostic, "phase=experience") {
		t.Fatalf("code=%d out=%s err=%s", code, out, diagnostic)
	}
}
func TestMissingStateFails(t *testing.T) {
	code, out, diagnostic := invoke(t, "status", "--run-dir", t.TempDir())
	if code != 1 || out != "" || diagnostic == "" {
		t.Fatalf("code=%d out=%s err=%s", code, out, diagnostic)
	}
}

func TestContractAcceptanceRecordsVersionAndRequiresNoModel(t *testing.T) {
	spec, err := team.LoadSpec("../../../examples/team/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := &team.State{Version: 1, ID: "contract-run", ProjectDir: root, Spec: spec, Status: "blocked", Phase: "development", Roles: map[string]team.RoleState{
		"frontend": {Status: "ready", ContractVersion: 1}, "backend": {Status: "change_requested", ContractVersion: 1},
	}}
	if err = state.Save(root); err != nil {
		t.Fatal(err)
	}
	contract := spec.Contract
	contract.Version = 2
	contract.Summary = "Explicitly accepted second version"
	file := filepath.Join(root, "accepted.json")
	if err = team.WriteJSON(file, contract); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := invoke(t, "contract", "--run-dir", root, "--file", file)
	if code != 0 || !json.Valid([]byte(out)) || !strings.Contains(diagnostic, "contract_version=2") {
		t.Fatalf("code=%d out=%s err=%s", code, out, diagnostic)
	}
	got, err := team.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Contract.Version != 2 || got.Roles["frontend"].Status != "pending" || got.Roles["backend"].Status != "pending" {
		t.Fatalf("unexpected accepted state: %+v", got)
	}
	events, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if err != nil || !bytes.Contains(events, []byte("accepted")) {
		t.Fatalf("missing acceptance event: %s %v", events, err)
	}
	code, _, _ = invoke(t, "contract", "--run-dir", root, "--file", file)
	if code != 1 {
		t.Fatalf("duplicate contract version must fail; code=%d", code)
	}
}
