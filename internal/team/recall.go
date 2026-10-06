package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/experience"
)

func applicable(e experience.Entry, c Contract) bool {
	if e.ContractHash != "" {
		return e.ContractHash == c.Fingerprint()
	}
	// Unversioned entries can represent explicitly approved general advice.
	// A task-local revision alone cannot identify an API in another task.
	// Legacy versioned entries need review and a fingerprint before reuse.
	return e.ContractVersion == 0
}

type recallTool struct {
	store    experience.Store
	project  string
	contract Contract
	paths    []string
}

func (t *recallTool) Name() string { return "experience_search" }
func (t *recallTool) Description() string {
	return "Retrieve approved project experience after locating target files. Advice includes evidence and applicability; it never overrides the current contract."
}
func (t *recallTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"modules":{"type":"array","items":{"type":"string"}}},"required":["query","modules"],"additionalProperties":false}`)
}
func (t *recallTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t *recallTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if e := ctx.Err(); e != nil {
		return agentcore.AgentToolResult{}, e
	}
	var a struct {
		Query   string   `json:"query"`
		Modules []string `json:"modules"`
	}
	if e := json.Unmarshal(args, &a); e != nil {
		return agentcore.AgentToolResult{}, e
	}
	if len(a.Modules) == 0 {
		return agentcore.AgentToolResult{}, fmt.Errorf("provide located target files/modules")
	}
	for _, m := range a.Modules {
		if !SafeRelative(m) {
			return agentcore.AgentToolResult{}, fmt.Errorf("invalid module path")
		}
		owned := false
		for _, p := range t.paths {
			owned = owned || m == p || strings.HasPrefix(m, p+"/")
		}
		if !owned {
			return agentcore.AgentToolResult{}, fmt.Errorf("module is outside this role's scope")
		}
	}
	var skipped []string
	active, e := t.store.SearchFiltered(t.project, a.Query, a.Modules, 5, func(hit experience.Entry) bool {
		if applicable(hit, t.contract) {
			return true
		}
		skipped = append(skipped, hit.ID)
		return false
	})
	if e != nil {
		return agentcore.AgentToolResult{}, e
	}
	b, e := json.Marshal(map[string]any{"advice": active, "requires_version_review": skipped})
	if e != nil {
		return agentcore.AgentToolResult{}, e
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(string(b))}}, nil
}
