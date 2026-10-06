package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/experience"
)

type coordinatorWorker func(context.Context, WorkRequest, func(string, string)) (WorkResult, error)

func (f coordinatorWorker) Run(ctx context.Context, r WorkRequest, emit func(string, string)) (WorkResult, error) {
	return f(ctx, r, emit)
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []WorkRequest
}

func (r *requestRecorder) record(req WorkRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
}
func (r *requestRecorder) snapshot() []WorkRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WorkRequest(nil), r.requests...)
}

func coordinatorGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func coordinatorWrite(root, path, value string) error {
	file := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(value), 0600)
}

type coordinatorFixture struct {
	project, run string
	state        *State
	memory       *experience.Store
	logger       *Logger
	output       *bytes.Buffer
}

func makeCoordinatorFixture(t *testing.T, budget int) coordinatorFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinatorGit(t, project, "init")
	coordinatorGit(t, project, "config", "user.name", "Fixture")
	coordinatorGit(t, project, "config", "user.email", "fixture@example.com")
	for name, value := range map[string]string{
		".gitignore":     ".pigo/\n",
		"web/client.txt": "initial-client\n",
		"api/result.txt": "initial-result\n",
		// Both checks read real files in the integration tree. The joint backend
		// check cannot pass on either independent worker tree alone.
		"acceptance.sh": "set -eu\ncase \"$1\" in\nfrontend) test \"$(cat web/client.txt)\" = client ;;\nbackend) test \"$(cat web/client.txt)\" = client; test \"$(cat api/result.txt)\" = answer ;;\nesac\nprintf 'validated %s against contract %s\\n' \"$1\" \"$PIGO_TEAM_CONTRACT_VERSION\"\n",
	} {
		if err := coordinatorWrite(project, name, value); err != nil {
			t.Fatal(err)
		}
	}
	coordinatorGit(t, project, "add", ".")
	coordinatorGit(t, project, "commit", "-m", "fixture")
	spec := Spec{Version: 1, Project: "fixture-project", Requirement: "pagination feature", Acceptance: []string{"client and server agree"}, Contract: Contract{Version: 1, Summary: "pagination API", Endpoints: []Endpoint{{Method: "GET", Path: "/items", Response: json.RawMessage(`{"items":[]}`)}}}, Frontend: RoleSpec{Task: "pagination client", Paths: []string{"web"}}, Backend: RoleSpec{Task: "pagination server", Paths: []string{"api"}}, Checks: []Check{{Name: "frontend check", Owner: "frontend", Command: []string{"sh", "acceptance.sh", "frontend"}}, {Name: "joint API check", Owner: "backend", Command: []string{"sh", "acceptance.sh", "backend"}}}, MaxRepairRounds: budget}
	run := filepath.Join(project, ".pigo", "runs", "fixture")
	state, err := Create(context.Background(), project, run, spec)
	if err != nil {
		t.Fatal(err)
	}
	output := new(bytes.Buffer)
	logger, err := NewLogger(filepath.Join(run, "events.jsonl"), state.ID, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logger.Close(); err != nil {
			t.Error(err)
		}
	})
	return coordinatorFixture{project, run, state, &experience.Store{Root: filepath.Join(project, ".pigo", "experience")}, logger, output}
}

func successfulCoordinatorWork(req WorkRequest) (WorkResult, error) {
	result := WorkResult{Status: "ready", ContractVersion: req.Spec.Contract.Version, Summary: "implementation ready for verification"}
	if req.Role == "experience" {
		return result, nil
	}
	path, value := "web/client.txt", "client\n"
	if req.Role == "backend" {
		path, value = "api/result.txt", "answer\n"
	}
	return result, coordinatorWrite(req.Workdir, path, value)
}

func TestCoordinatorRealIntegrationRepairRecallAndLogs(t *testing.T) {
	f := makeCoordinatorFixture(t, 1)
	base := experience.Entry{Project: f.state.Spec.Project, Problem: "pagination contract mismatch", Solution: "use the accepted fields", Applicability: "pagination endpoint", Sources: []string{"fixture evidence"}, ContractVersion: 1, ContractHash: f.state.Spec.Contract.Fingerprint()}
	approved, err := f.memory.Propose(base)
	if err != nil {
		t.Fatal(err)
	}
	approved, err = f.memory.Approve(approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	base.Problem = "pagination draft must stay private"
	draft, err := f.memory.Propose(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Problem = "pagination obsolete contract"
	base.ContractVersion = 99
	base.ContractHash = ""
	obsolete, err := f.memory.Propose(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.memory.Approve(obsolete.ID); err != nil {
		t.Fatal(err)
	}
	var recorded requestRecorder
	var barrierMu sync.Mutex
	arrived := 0
	barrier := make(chan struct{})
	worker := coordinatorWorker(func(ctx context.Context, req WorkRequest, emit func(string, string)) (WorkResult, error) {
		recorded.record(req)
		emit("fixture", "actual fixture execution")
		if req.Role == "experience" {
			if len(req.Evidence) < 2 || !strings.Contains(req.Evidence[0].Text, "+answer") || !strings.Contains(req.Evidence[1].Text, "pagination API") {
				return WorkResult{}, fmt.Errorf("experience extraction did not receive the actual patch and contract: %+v", req.Evidence)
			}
			return WorkResult{Status: "ready", ContractVersion: req.Spec.Contract.Version, Experiences: []experience.Entry{{Project: req.Spec.Project, Problem: "pagination result did not match accepted answer", Solution: "replace incorrect server output and rerun joint check", Applicability: "fixture pagination protocol", Modules: []string{"api"}, Sources: append([]string(nil), req.Sources...), ContractVersion: req.Spec.Contract.Version}}}, nil
		}
		if req.Attempt == 1 {
			barrierMu.Lock()
			arrived++
			if arrived == 2 {
				close(barrier)
			}
			barrierMu.Unlock()
			select {
			case <-barrier:
			case <-ctx.Done():
				return WorkResult{}, ctx.Err()
			}
		}
		res, err := successfulCoordinatorWork(req)
		if err != nil {
			return res, err
		}
		if req.Role == "backend" && req.Attempt == 1 {
			err = coordinatorWrite(req.Workdir, "api/result.txt", "incorrect\n")
		}
		return res, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := Execute(ctx, f.run, worker, f.logger, f.memory)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "completed" || s.Attempt != 2 || s.Repairs != 1 || !allPassed(s.Checks) {
		t.Fatalf("unexpected final state: %+v", s)
	}
	if len(s.CheckHistory) != 4 || s.CheckHistory[1].Passed || !s.CheckHistory[3].Passed {
		t.Fatalf("missing failure/success evidence: %+v", s.CheckHistory)
	}
	counts := map[string]int{}
	dirs := map[string]string{}
	for _, req := range recorded.snapshot() {
		counts[req.Role]++
		dirs[req.Role] = req.Workdir
		if req.Role == "experience" {
			if len(req.Feedback) != 4 || len(req.Sources) != 6 {
				t.Fatalf("incomplete experience evidence: %+v", req)
			}
			continue
		}
		if len(req.Experiences) != 1 || req.Experiences[0].ID != approved.ID {
			t.Fatalf("draft or obsolete recall leaked (draft %s obsolete %s): %+v", draft.ID, obsolete.ID, req.Experiences)
		}
		if req.Attempt == 2 {
			if req.Role != "backend" || len(req.Feedback) != 1 || req.Feedback[0].Owner != "backend" || req.Feedback[0].Passed {
				t.Fatalf("repair not targeted: %+v", req)
			}
		}
	}
	if counts["frontend"] != 1 || counts["backend"] != 2 || counts["experience"] != 1 || dirs["frontend"] == dirs["backend"] || dirs["frontend"] == f.project {
		t.Fatalf("bad isolation or calls: %v %v", counts, dirs)
	}
	if got := coordinatorGit(t, f.project, "status", "--porcelain"); got != "" {
		t.Fatalf("source modified: %s", got)
	}
	if got := coordinatorGit(t, f.project, "rev-parse", "HEAD"); got != s.Base {
		t.Fatal("source HEAD changed")
	}
	patch, err := os.ReadFile(s.Patch)
	if err != nil || !bytes.Contains(patch, []byte("+answer")) || !bytes.Contains(patch, []byte("+client")) {
		t.Fatalf("bad delivered patch: %s %v", patch, err)
	}
	if len(s.ExperienceIDs) != 1 {
		t.Fatalf("draft missing: %v", s.ExperienceIDs)
	}
	candidate, err := f.memory.Get(s.ExperienceIDs[0])
	if err != nil || candidate.Status != experience.Draft {
		t.Fatalf("candidate not draft: %+v %v", candidate, err)
	}
	if _, err = f.memory.Approve(candidate.ID); err != nil {
		t.Fatal(err)
	}
	hits, err := f.memory.Search(s.Spec.Project, "pagination", []string{"api"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hits {
		found = found || hit.ID == candidate.ID
		if hit.ID == draft.ID {
			t.Fatal("unapproved draft recalled")
		}
	}
	if !found {
		t.Fatal("approved candidate not recalled")
	}
	file, err := os.Open(filepath.Join(f.run, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	phases := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.RunID != s.ID || event.Time.IsZero() {
			t.Fatalf("invalid event %+v", event)
		}
		phases[event.Phase] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"alignment", "recall", "development", "integration", "verification", "repair", "experience", "delivery"} {
		if !phases[phase] {
			t.Errorf("missing JSONL phase %s", phase)
		}
		if !strings.Contains(f.output.String(), "["+phase) {
			t.Errorf("missing CLI phase %s", phase)
		}
	}
}

func TestCoordinatorRepairBudgetAndManualResume(t *testing.T) {
	f := makeCoordinatorFixture(t, 0)
	calls := 0
	var mu sync.Mutex
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		res, err := successfulCoordinatorWork(req)
		if req.Role == "backend" && err == nil {
			err = coordinatorWrite(req.Workdir, "api/result.txt", "bad\n")
		}
		return res, err
	})
	s, err := Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err == nil || s.Status != "blocked" || !strings.Contains(err.Error(), "budget") || s.Attempt != 1 {
		t.Fatalf("budget not enforced: %+v %v", s, err)
	}
	// An explicit manual correction can be checked without asking already-ready
	// workers to regenerate their code or overwriting the previous integration.
	if err := coordinatorWrite(s.Roles["backend"].Workdir, "api/result.txt", "answer\n"); err != nil {
		t.Fatal(err)
	}
	oldPatch := s.Patch
	s, err = Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "completed" || s.Attempt != 2 || s.Patch == oldPatch || calls != 3 {
		t.Fatalf("bad resume: %+v calls=%d", s, calls)
	}
	if _, err := os.Stat(oldPatch); err != nil {
		t.Fatal("prior evidence lost")
	}
}

func TestCoordinatorContractChangeRequiresExplicitAcceptance(t *testing.T) {
	f := makeCoordinatorFixture(t, 1)
	var rec requestRecorder
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		rec.record(req)
		if req.Role == "backend" && req.Spec.Contract.Version == 1 {
			next := req.Spec.Contract
			next.Version = 2
			next.Summary = "explicit revision"
			return WorkResult{Status: "change_requested", ContractVersion: 1, Summary: "response contract requires clarification", Proposal: &next}, nil
		}
		return successfulCoordinatorWork(req)
	})
	s, err := Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err == nil || s.Status != "blocked" || s.Proposal == nil {
		t.Fatalf("change not blocked: %+v %v", s, err)
	}
	before := len(rec.snapshot())
	if _, err = Execute(context.Background(), f.run, worker, f.logger, f.memory); err == nil {
		t.Fatal("unaccepted proposal resumed")
	}
	if len(rec.snapshot()) != before {
		t.Fatal("worker ran before acceptance")
	}
	if err := AcceptContract(f.run, *s.Proposal); err != nil {
		t.Fatal(err)
	}
	s, err = Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "completed" || s.Spec.Contract.Version != 2 {
		t.Fatalf("revision not completed: %+v", s)
	}
	seen := map[string]bool{}
	for _, req := range rec.snapshot()[before:] {
		seen[req.Role] = true
		if req.Spec.Contract.Version != 2 {
			t.Fatalf("stale request: %+v", req)
		}
	}
	if !seen["frontend"] || !seen["backend"] {
		t.Fatal("both workers did not revalidate")
	}
	if _, err := os.Stat(filepath.Join(f.run, "contract-v2.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorCancellationAndRetry(t *testing.T) {
	f := makeCoordinatorFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 2)
	worker := coordinatorWorker(func(ctx context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		if _, err := successfulCoordinatorWork(req); err != nil {
			return WorkResult{}, err
		}
		started <- req.Role
		<-ctx.Done()
		return WorkResult{}, ctx.Err()
	})
	type outcome struct {
		s   *State
		err error
	}
	done := make(chan outcome, 1)
	go func() { s, err := Execute(ctx, f.run, worker, f.logger, f.memory); done <- outcome{s, err} }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(15 * time.Second):
			t.Fatal("workers failed to start")
		}
	}
	cancel()
	var first outcome
	select {
	case first = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop workers")
	}
	if first.err == nil || first.s.Status != "cancelled" {
		t.Fatalf("bad cancellation: %+v", first)
	}
	good := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		return successfulCoordinatorWork(req)
	})
	s, err := Execute(context.Background(), f.run, good, f.logger, f.memory)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "completed" || s.Attempt != 2 {
		t.Fatalf("retry failed: %+v", s)
	}
}

func TestCoordinatorRejectsStaleWorkerVersion(t *testing.T) {
	f := makeCoordinatorFixture(t, 1)
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		res, err := successfulCoordinatorWork(req)
		if req.Role == "backend" {
			res.ContractVersion = 0
		}
		return res, err
	})
	s, err := Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err == nil || !strings.Contains(err.Error(), "stale backend result") || s.Status != "failed" || s.IntegrationDir != "" {
		t.Fatalf("stale result accepted: %+v %v", s, err)
	}
}

func TestCoordinatorRejectsInventedExperienceSource(t *testing.T) {
	f := makeCoordinatorFixture(t, 0)
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		res, err := successfulCoordinatorWork(req)
		if req.Role == "experience" {
			res.Experiences = []experience.Entry{{Project: req.Spec.Project, Problem: "pagination issue", Solution: "fix", Applicability: "fixture", Sources: []string{"invented source"}}}
		}
		return res, err
	})
	s, err := Execute(context.Background(), f.run, worker, f.logger, f.memory)
	if err == nil || !strings.Contains(err.Error(), "unknown source") || s.Status != "failed" {
		t.Fatalf("invented evidence accepted: %+v %v", s, err)
	}
	if entries, err := f.memory.List(f.state.Spec.Project); err != nil || len(entries) != 0 {
		t.Fatalf("invalid candidate saved: %v %v", entries, err)
	}
}
