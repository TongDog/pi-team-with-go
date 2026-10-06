package teamcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/experience"
	"github.com/smallnest/pigo/internal/team"
)

// Exercises the actual CLI/provider/SSE/tool/workspace pipeline. The local model
// deliberately produces one faulty backend version: this proves deterministic
// wiring and repair behavior, not real-model coding quality.
func TestRunCLIProviderSSEDevelopmentRepairExperience(t *testing.T) {
	for _, binary := range []string{"git", "sh", "grep"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s unavailable", binary)
		}
	}
	root := t.TempDir()
	project, runDir := filepath.Join(root, "project"), filepath.Join(root, "run")
	for _, role := range []string{"frontend", "backend"} {
		if err := os.MkdirAll(filepath.Join(project, role), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, role, "value.txt"), []byte("original-"+role+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(".pigo/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-C", project}, args...)...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init")
	git("add", ".")
	git("commit", "-m", "fixture base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	spec := team.Spec{
		Version: 1, Project: "offline-e2e", Requirement: "Implement matching frontend and backend contract values.",
		Acceptance: []string{"Frontend value is ready-front", "Backend value is ready-back"},
		Contract: team.Contract{Version: 1, Summary: "Both roles use the accepted value contract", Endpoints: []team.Endpoint{
			{Method: "GET", Path: "/value", Response: json.RawMessage("{\"value\":\"ready-back\"}")},
		}},
		Frontend: team.RoleSpec{Task: "Write ready-front in frontend/value.txt", Paths: []string{"frontend"}},
		Backend:  team.RoleSpec{Task: "Write ready-back in backend/value.txt", Paths: []string{"backend"}},
		Checks: []team.Check{
			{Name: "frontend-contract", Owner: "frontend", Command: []string{"sh", "-c", "if grep -qx ready-front frontend/value.txt; then echo frontend-contract-ok; else echo frontend-contract-failed; exit 1; fi"}, TimeoutSeconds: 5},
			{Name: "backend-contract", Owner: "backend", Command: []string{"sh", "-c", "if grep -qx ready-back backend/value.txt; then echo backend-contract-ok; else echo backend-contract-failed; exit 1; fi"}, TimeoutSeconds: 5},
		},
		MaxRepairRounds: 1, MaxTurns: 4, WorkerTimeoutSeconds: 15,
	}
	specPath := filepath.Join(root, "spec.json")
	if err := team.WriteJSON(specPath, spec); err != nil {
		t.Fatal(err)
	}
	const key = "offline-fixture-key-not-a-real-credential"
	const keyEnv = "PIGO_TEAM_E2E_FIXTURE_KEY"
	t.Setenv(keyEnv, key)
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(message string) {
			t.Error(message)
			http.Error(w, message, http.StatusBadRequest)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			fail("unexpected provider route: " + r.Method + " " + r.URL.Path)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			fail("missing fixture authorization")
			return
		}
		var body struct {
			Model    string
			Stream   bool
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
			Tools []json.RawMessage
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
			fail(err.Error())
			return
		}
		if body.Model != "fixture" || !body.Stream {
			fail("CLI model/stream flags were not propagated")
			return
		}
		var req team.WorkRequest
		found, toolResults := false, 0
		for _, message := range body.Messages {
			if message.Role == "user" && !found {
				var text string
				if err := json.Unmarshal(message.Content, &text); err != nil {
					fail("request content is not text: " + err.Error())
					return
				}
				if err := json.Unmarshal([]byte(text), &req); err != nil {
					fail("initial user message is not WorkRequest: " + err.Error())
					return
				}
				found = true
			}
			if message.Role == "tool" {
				toolResults++
			}
		}
		if !found || req.Spec.Project != spec.Project || req.Spec.Contract.Version != 1 {
			fail("wrong or missing shared contract")
			return
		}
		mu.Lock()
		calls[fmt.Sprintf("%s-%d", req.Role, req.Attempt)]++
		mu.Unlock()
		var delta map[string]any
		finish := "stop"
		result := team.WorkResult{ContractVersion: 1, Status: "ready", Summary: req.Role + " completed"}
		switch req.Role {
		case "frontend", "backend":
			if len(body.Tools) == 0 {
				fail("coding worker has no tools")
				return
			}
			if req.Role == "frontend" && req.Attempt != 1 {
				fail("frontend reran despite passing acceptance")
				return
			}
			if req.Role == "backend" && req.Attempt == 2 {
				if len(req.Feedback) != 1 || req.Feedback[0].Name != "backend-contract" || req.Feedback[0].Passed || !strings.Contains(req.Feedback[0].Output, "backend-contract-failed") {
					fail("repair lacks targeted actual failure evidence")
					return
				}
			}
			if toolResults == 0 {
				content := "ready-front\n"
				if req.Role == "backend" {
					content = "wrong-back\n"
					if req.Attempt == 2 {
						content = "ready-back\n"
					}
				}
				args, _ := json.Marshal(map[string]string{"path": req.Role + "/value.txt", "content": content})
				delta = map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": req.Role + "-write", "type": "function",
					"function": map[string]string{"name": "write", "arguments": string(args)},
				}}}
				finish = "tool_calls"
			} else {
				if toolResults != 1 {
					fail("unexpected tool result count")
					return
				}
				encoded, _ := json.Marshal(result)
				delta = map[string]any{"content": string(encoded)}
			}
		case "experience":
			if len(body.Tools) != 0 {
				fail("experience worker unexpectedly has mutation tools")
				return
			}
			failed, passed, sawPatch := false, false, false
			for _, feedback := range req.Feedback {
				if feedback.Name == "backend-contract" {
					failed = failed || !feedback.Passed
					passed = passed || feedback.Passed
				}
			}
			for _, evidence := range req.Evidence {
				sawPatch = sawPatch || (strings.HasSuffix(evidence.Source, ".patch") && strings.Contains(evidence.Text, "+ready-back"))
			}
			if !failed || !passed || !sawPatch || len(req.Sources) < 3 {
				fail("extraction lacks failed+passed checks and actual patch evidence")
				return
			}
			for _, source := range req.Sources {
				if _, err := os.Stat(source); err != nil {
					fail("missing evidence source: " + source)
					return
				}
			}
			result.Experiences = []experience.Entry{{
				Project: spec.Project, Problem: "Backend used wrong-back and failed its contract check",
				Solution:      "Use ready-back; the subsequent backend contract check passed",
				Applicability: "This fixture's accepted GET /value response", Modules: []string{"backend"},
				Sources: req.Sources, ContractVersion: 1,
			}}
			encoded, _ := json.Marshal(result)
			delta = map[string]any{"content": string(encoded)}
		default:
			fail("unexpected role " + req.Role)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Separate events exercise the real transport/decoder, not a StreamFn stub.
		for _, choice := range []map[string]any{
			{"index": 0, "delta": map[string]string{"role": "assistant"}},
			{"index": 0, "delta": delta},
			{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
		} {
			chunk, _ := json.Marshal(map[string]any{"id": "fixture-completion", "model": "fixture", "choices": []any{choice}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, logs bytes.Buffer
	code := Run(ctx, []string{"run", "--spec", specPath, "--project", project, "--run-dir", runDir, "--model", "fixture", "--base-url", server.URL + "/v1", "--protocol", "openai", "--api-key-env", keyEnv}, &out, &logs)
	if code != 0 {
		t.Fatalf("CLI failed: %d\nstdout=%s\nstderr=%s", code, out.String(), logs.String())
	}
	var state team.State
	if err := json.Unmarshal(out.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "completed" || state.Phase != "delivery" || state.Attempt != 2 || state.Repairs != 1 {
		t.Fatalf("wrong terminal state: %+v", state)
	}
	if len(state.CheckHistory) != 4 || state.CheckHistory[1].Passed || !state.CheckHistory[3].Passed {
		t.Fatalf("missing real failure/repair: %+v", state.CheckHistory)
	}
	for _, check := range state.CheckHistory {
		data, err := os.ReadFile(check.Log)
		if err != nil || len(data) == 0 {
			t.Fatalf("missing check output: %s %v", check.Log, err)
		}
	}
	for _, fragment := range []string{"[setup]", "[alignment]", "[development/frontend]", "[development/backend]", "[integration]", "[verification/backend] failed", "[verification/backend] passed", "[repair]", "[experience]", "[delivery]"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Errorf("missing stage log %q in %s", fragment, logs.String())
		}
	}
	eventData, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var failedEvent, passedEvent bool
	for _, line := range bytes.Split(bytes.TrimSpace(eventData), []byte("\n")) {
		var event team.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Phase == "verification" && event.Role == "backend" {
			failedEvent = failedEvent || event.Status == "failed"
			passedEvent = passedEvent || event.Status == "passed"
		}
	}
	if !failedEvent || !passedEvent {
		t.Fatal("JSONL lacks failed and passed backend checks")
	}
	patch, err := os.ReadFile(state.Patch)
	if err != nil || !bytes.Contains(patch, []byte("+ready-front")) || !bytes.Contains(patch, []byte("+ready-back")) {
		t.Fatalf("wrong patch: %s %v", patch, err)
	}
	for role, want := range map[string]string{"frontend": "ready-front\n", "backend": "ready-back\n"} {
		value, err := os.ReadFile(filepath.Join(state.IntegrationDir, role, "value.txt"))
		if err != nil || string(value) != want {
			t.Fatalf("integration %s=%q: %v", role, value, err)
		}
		original, err := os.ReadFile(filepath.Join(project, role, "value.txt"))
		if err != nil || string(original) != "original-"+role+"\n" {
			t.Fatalf("source checkout changed: %s=%q %v", role, original, err)
		}
	}
	if got := strings.TrimSpace(git("rev-parse", "HEAD")); got != base {
		t.Fatalf("source HEAD changed to %s", got)
	}
	if status := git("status", "--porcelain"); status != "" {
		t.Fatalf("source checkout dirty: %s", status)
	}
	store := experience.Store{Root: filepath.Join(state.ProjectDir, ".pigo", "team", "experience")}
	entries, err := store.List(spec.Project)
	if err != nil || len(entries) != 1 || entries[0].Status != experience.Draft {
		t.Fatalf("expected one draft, got %+v %v", entries, err)
	}
	if len(state.ExperienceIDs) != 1 || state.ExperienceIDs[0] != entries[0].ID {
		t.Fatal("state does not reference saved draft")
	}
	hits, err := store.Search(spec.Project, "backend", []string{"backend"}, 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("unapproved experience recalled: %+v %v", hits, err)
	}
	code, statusJSON, diagnostic := invoke(t, "status", "--run-dir", runDir)
	if code != 0 || diagnostic != "" {
		t.Fatalf("status failed: %d %s", code, diagnostic)
	}
	var status team.State
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil || status.ID != state.ID || status.Status != "completed" {
		t.Fatalf("status mismatch: %+v %v", status, err)
	}
	if strings.Contains(out.String()+logs.String()+string(eventData), key) {
		t.Fatal("credential leaked into state or logs")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["frontend-1"] != 2 || calls["backend-1"] != 2 || calls["backend-2"] != 2 || calls["experience-2"] != 1 || len(calls) != 4 {
		t.Fatalf("unexpected model/tool loops: %+v", calls)
	}
	t.Logf("CLI stage log (offline model fixture):\n%s", logs.String())
}
