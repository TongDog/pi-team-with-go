package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/experience"
)

func recallEntry(t *testing.T, store experience.Store, c Contract, label, project, module, status string) experience.Entry {
	t.Helper()
	e := experience.Entry{Project: project, Problem: "pagination " + label, Solution: "Follow verified pagination mapping", Applicability: "Pagination list API", Sources: []string{"fixture:" + label}, Modules: []string{module}, ContractVersion: c.Version, ContractHash: c.Fingerprint()}
	e, err := store.Propose(e)
	if err != nil {
		t.Fatal(err)
	}
	if status != experience.Draft {
		e, err = store.Approve(e.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status == experience.Retired {
		e, err = store.Retire(e.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

type recallFixture struct {
	store                      experience.Store
	contract                   Contract
	front, back, wrong, legacy string
}

func newRecallFixture(t *testing.T) recallFixture {
	t.Helper()
	s := experience.Store{Root: t.TempDir()}
	c := validSpecForTest().Contract
	front := recallEntry(t, s, c, "frontend", "project", "web", experience.Approved)
	back := recallEntry(t, s, c, "backend", "project", "api", experience.Approved)
	other := validSpecForTest().Contract
	other.Endpoints[0].Response = json.RawMessage(`{"data":[]}`)
	wrong := recallEntry(t, s, other, "other API same v1", "project", "web", experience.Approved)
	recallEntry(t, s, c, "draft", "project", "web", experience.Draft)
	recallEntry(t, s, c, "retired", "project", "web", experience.Retired)
	recallEntry(t, s, c, "different project", "elsewhere", "web", experience.Approved)
	recallEntry(t, s, c, "different module", "project", "ops", experience.Approved)
	legacy, err := s.Propose(experience.Entry{Project: "project", Problem: "pagination legacy entry", Solution: "Legacy mapping", Applicability: "Identity unknown", Sources: []string{"legacy:record"}, Modules: []string{"web"}, ContractVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(legacy.ID); err != nil {
		t.Fatal(err)
	}
	return recallFixture{s, c, front.ID, back.ID, wrong.ID, legacy.ID}
}

type recallRecorder chan WorkRequest

func (r recallRecorder) Run(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
	r <- req
	return WorkResult{Status: "ready", ContractVersion: req.Spec.Contract.Version, Summary: "ready"}, nil
}
func TestInitialRecallFiltersIdentityApprovalAndOwnership(t *testing.T) {
	f := newRecallFixture(t)
	dir := t.TempDir()
	state := &State{Version: 1, ID: "recall", Spec: validSpecForTest(), Attempt: 1, Roles: map[string]RoleState{"frontend": {Status: "pending", ContractVersion: 1, Workdir: t.TempDir()}, "backend": {Status: "pending", ContractVersion: 1, Workdir: t.TempDir()}}}
	requests := make(recallRecorder, 2)
	if err := develop(context.Background(), dir, state, requests, nil, &f.store); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := <-requests
		want := f.front
		if req.Role == "backend" {
			want = f.back
		}
		if len(req.Experiences) != 1 || req.Experiences[0].ID != want {
			t.Fatalf("%s got unapproved/unrelated knowledge: %+v", req.Role, req.Experiences)
		}
		if req.KnowledgeRoot != f.store.Root {
			t.Fatal("second-stage search store not passed to worker")
		}
	}
}
func TestSecondRecallFiltersAndReportsContractMismatch(t *testing.T) {
	f := newRecallFixture(t)
	tool := &recallTool{store: f.store, project: "project", contract: f.contract, paths: []string{"web"}}
	result, err := tool.Execute(context.Background(), "search", json.RawMessage(`{"query":"pagination","modules":["web/users.ts"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Advice []experience.Entry `json:"advice"`
		Review []string           `json:"requires_version_review"`
	}
	if err = json.Unmarshal([]byte(agentcore.ContentToText(result.Content)), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Advice) != 1 || response.Advice[0].ID != f.front {
		t.Fatalf("second recall leaked unrelated entries: %+v", response)
	}
	review := map[string]bool{}
	for _, id := range response.Review {
		review[id] = true
	}
	if len(response.Review) != 2 || !review[f.wrong] || !review[f.legacy] {
		t.Fatalf("contract mismatch not reported for review: %+v", response)
	}
	if len(response.Advice[0].Sources) == 0 || response.Advice[0].Applicability == "" {
		t.Fatal("recall dropped evidence/applicability")
	}
}
func TestSecondRecallRequiresLocatedOwnedModules(t *testing.T) {
	f := newRecallFixture(t)
	tool := &recallTool{store: f.store, project: "project", contract: f.contract, paths: []string{"web"}}
	for _, modules := range [][]string{nil, {"../web"}, {"web/../api"}, {"api"}, {"web-other"}, {"web/users.ts", "api/users.go"}, {"/web"}} {
		t.Run(fmt.Sprint(modules), func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{"query": "pagination", "modules": modules})
			if _, err := tool.Execute(context.Background(), "search", args, nil); err == nil {
				t.Fatal("unlocated or unowned query accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, "search", json.RawMessage(`{"query":"pagination","modules":["web"]}`), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
func TestVersionedExperienceNeedsContractIdentity(t *testing.T) {
	c := validSpecForTest().Contract
	if applicable(experience.Entry{ContractVersion: 1}, c) {
		t.Fatal("version-only v1 experience cannot establish identity with this task's v1; must require review")
	}
	if !applicable(experience.Entry{ContractVersion: 0}, c) {
		t.Fatal("explicitly unversioned approved general advice rejected")
	}
	if !applicable(experience.Entry{ContractVersion: 99, ContractHash: c.Fingerprint()}, c) {
		t.Fatal("same actual interface rejected solely due to task-local revision label")
	}
	other := validSpecForTest().Contract
	other.Endpoints[0].Path = "/unrelated"
	if applicable(experience.Entry{ContractVersion: 1, ContractHash: other.Fingerprint()}, c) {
		t.Fatal("same revision label allowed different interface")
	}
}

func TestRecallCompatibleEvidenceSurvivesCandidateCrowding(t *testing.T) {
	f := newRecallFixture(t)
	for i := 0; i < 12; i++ {
		incompatible := validSpecForTest().Contract
		incompatible.Endpoints[0].Path = fmt.Sprintf("/other-task-%d", i)
		recallEntry(t, f.store, incompatible, fmt.Sprintf("incompatible-%d", i), "project", "web", experience.Approved)
	}
	state := &State{Version: 1, ID: "crowded-recall", Spec: validSpecForTest(), Attempt: 1, Roles: map[string]RoleState{"frontend": {Status: "pending", ContractVersion: 1, Workdir: t.TempDir()}, "backend": {Status: "pending", ContractVersion: 1, Workdir: t.TempDir()}}}
	requests := make(recallRecorder, 2)
	if err := develop(context.Background(), t.TempDir(), state, requests, nil, &f.store); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := <-requests
		if req.Role == "frontend" && (len(req.Experiences) != 1 || req.Experiences[0].ID != f.front) {
			t.Errorf("initial recall truncated before applicability: %+v", req.Experiences)
		}
	}
	tool := &recallTool{store: f.store, project: "project", contract: f.contract, paths: []string{"web"}}
	result, err := tool.Execute(context.Background(), "search", json.RawMessage(`{"query":"pagination","modules":["web/users.ts"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Advice []experience.Entry `json:"advice"`
	}
	if err = json.Unmarshal([]byte(agentcore.ContentToText(result.Content)), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Advice) != 1 || response.Advice[0].ID != f.front {
		t.Fatalf("second recall truncated before applicability: %+v", response.Advice)
	}
}
