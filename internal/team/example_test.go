package team

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A scripted worker verifies the shipped example's real Go/JS acceptance suite.
// Model behavior is tested independently through the HTTP/SSE CLI fixture.
func TestUsersExampleJointHTTPAcceptanceAndRepair(t *testing.T) {
	for _, program := range []string{"git", "node"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skip(program + " unavailable")
		}
	}
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.CopyFS(project, os.DirFS("../../examples/team/project")); err != nil {
		t.Fatal(err)
	}
	coordinatorGit(t, project, "init")
	coordinatorGit(t, project, "config", "user.name", "Fixture")
	coordinatorGit(t, project, "config", "user.email", "fixture@example.invalid")
	coordinatorGit(t, project, "-c", "commit.gpgsign=false", "add", ".")
	coordinatorGit(t, project, "-c", "commit.gpgsign=false", "commit", "-m", "sample starter")
	spec, err := LoadSpec("../../examples/team/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := filepath.Join(root, "run")
	state, err := Create(ctx, project, run, spec)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log, err := NewLogger(filepath.Join(run, "events.jsonl"), state.ID, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		result := WorkResult{ContractVersion: 1, Status: "ready", Summary: "scripted example implementation"}
		switch req.Role {
		case "frontend":
			return result, coordinatorWrite(req.Workdir, "frontend/users.mjs", exampleFrontend)
		case "backend":
			total := 0 // Deliberate first-pass contract violation, detected by actual HTTP.
			if req.Attempt > 1 {
				total = 2
			}
			return result, coordinatorWrite(req.Workdir, "backend/main.go", fmt.Sprintf(exampleBackend, total))
		default:
			return result, nil // No fabricated reusable knowledge from a scripted demo.
		}
	})
	state, err = Execute(ctx, run, worker, log, nil)
	if err != nil {
		t.Fatalf("sample failed: %v\n%s", err, output.String())
	}
	if state.Status != "completed" || state.Repairs != 1 || len(state.CheckHistory) != 6 {
		t.Fatalf("unexpected outcome: %+v", state)
	}
	if state.CheckHistory[2].Passed || !state.CheckHistory[5].Passed {
		t.Fatalf("HTTP contract violation was not repaired: %+v", state.CheckHistory)
	}
	if got := coordinatorGit(t, project, "status", "--porcelain"); got != "" {
		t.Fatalf("starter modified: %s", got)
	}
	starter, err := os.ReadFile(filepath.Join(project, "frontend/users.mjs"))
	if err != nil || !strings.Contains(string(starter), "not implemented") {
		t.Fatal("starter overwritten")
	}
	t.Logf("Real Go/JS example stage log (scripted worker):\n%s", output.String())
}

const exampleFrontend = `export async function loadUsers(baseURL) {
  const response = await fetch(baseURL + '/api/users');
  if (!response.ok) throw new Error('HTTP ' + response.status);
  const data = await response.json();
  if (!Array.isArray(data.items) || data.total !== data.items.length) throw new Error('Invalid users envelope');
  return data;
}
export function renderUsers(users) {
  const escape = name => String(name).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  return '<ul>' + users.map(user => '<li>' + escape(user.name) + '</li>').join('') + '</ul>';
}
`

const exampleBackend = `package main
import ("encoding/json"; "log"; "net/http"; "os")
func main() {
  addr := os.Getenv("TEAM_DEMO_ADDR"); if addr == "" { addr = "127.0.0.1:8080" }
  mux := http.NewServeMux()
  mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
  mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}},"total":%d})
  })
  mux.Handle("/", http.FileServer(http.Dir("../frontend")))
  log.Fatal(http.ListenAndServe(addr,mux))
}
`
