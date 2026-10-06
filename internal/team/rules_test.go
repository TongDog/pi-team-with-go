package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ruleFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func rulesRequest(root string, paths ...string) WorkRequest {
	return WorkRequest{Role: "frontend", Workdir: root, Spec: Spec{Frontend: RoleSpec{Paths: paths}, Backend: RoleSpec{Paths: []string{"api"}}}}
}
func TestRoleInstructionsLoadOwnedAncestorsInOrder(t *testing.T) {
	root := t.TempDir()
	for path, text := range map[string]string{"AGENTS.md": "ROOT_ALREADY_LOADED", "web/AGENTS.md": "FRONTEND_PARENT", "web/users/AGENTS.md": "FRONTEND_CHILD", "web/users/deep/AGENTS.md": "UNLOCATED_DESCENDANT", "web/orders/AGENTS.md": "UNOWNED_SIBLING", "api/AGENTS.md": "BACKEND_PRIVATE"} {
		ruleFile(t, root, path, text)
	}
	got, err := roleInstructions(rulesRequest(root, "web/users"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want ancestor and owned module, got %v", got)
	}
	if !strings.Contains(got[0], "FRONTEND_PARENT") || !strings.Contains(got[1], "FRONTEND_CHILD") {
		t.Fatalf("wrong rule order: %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, excluded := range []string{"ROOT_ALREADY_LOADED", "UNLOCATED_DESCENDANT", "UNOWNED_SIBLING", "BACKEND_PRIVATE"} {
		if strings.Contains(joined, excluded) {
			t.Fatalf("injected unrelated rules: %s", excluded)
		}
	}
	if !strings.Contains(got[0], "web/AGENTS.md") || !strings.Contains(got[0], "Scope: web/") || !strings.Contains(got[0], filepath.Join(root, "web", "AGENTS.md")) {
		t.Fatalf("rule source/scope missing: %s", got[0])
	}
}
func TestRoleInstructionsFileOwnershipAndDedup(t *testing.T) {
	root := t.TempDir()
	ruleFile(t, root, "web/AGENTS.md", "PARENT_RULE")
	ruleFile(t, root, "web/users/AGENTS.md", "CHILD_RULE")
	ruleFile(t, root, "web/users/view.ts", "export {}")
	got, err := roleInstructions(rulesRequest(root, "web/users/view.ts", "web/users", "web", "web/users/new.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("duplicate scopes injected duplicate rules: %v", got)
	}
	if !strings.Contains(got[0], "PARENT_RULE") || !strings.Contains(got[1], "CHILD_RULE") {
		t.Fatalf("unexpected rule ordering: %v", got)
	}
	fileOnly, err := roleInstructions(rulesRequest(root, "web/users/view.ts"))
	if err != nil || len(fileOnly) != 2 {
		t.Fatalf("file scope did not inherit parent rules: %v %v", fileOnly, err)
	}
}
func TestRoleInstructionsExperienceAndMissingRules(t *testing.T) {
	got, err := roleInstructions(WorkRequest{Role: "experience", Workdir: "/nonexistent"})
	if err != nil || len(got) != 0 {
		t.Fatalf("experience unexpectedly read project rules: %v %v", got, err)
	}
	root := t.TempDir()
	got, err = roleInstructions(rulesRequest(root, "future/module/file.go"))
	if err != nil || len(got) != 0 {
		t.Fatalf("missing directories must not invent rules: %v %v", got, err)
	}
	ruleFile(t, root, "web/AGENTS.md", "WEB_RULE")
	got, err = roleInstructions(rulesRequest(root, "web/new/feature.go"))
	if err != nil || len(got) != 1 {
		t.Fatalf("new path lost existing ancestor rules: %v %v", got, err)
	}
}
func TestRoleInstructionsReadFailuresAreNotSilenced(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web", "AGENTS.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := roleInstructions(rulesRequest(root, "web")); err == nil {
		t.Fatal("existing unreadable rule path ignored")
	}
	if _, err := roleInstructions(rulesRequest(root, "../outside")); err == nil {
		t.Fatal("unsafe scope accepted")
	}
}
func TestRoleInstructionsRejectOversizeWithoutPartialRules(t *testing.T) {
	root := t.TempDir()
	ruleFile(t, root, "web/AGENTS.md", strings.Repeat("a", maxRoleRuleBytes/2))
	ruleFile(t, root, "web/users/AGENTS.md", strings.Repeat("b", maxRoleRuleBytes/2+1))
	got, err := roleInstructions(rulesRequest(root, "web/users"))
	if err == nil || !strings.Contains(err.Error(), "refusing to truncate") {
		t.Fatalf("oversized mandatory rules not rejected: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("returned partial mandatory rules after size failure")
	}
}
func TestRoleInstructionsRejectSymlinkedSiblingRules(t *testing.T) {
	root := t.TempDir()
	ruleFile(t, root, "api/AGENTS.md", "PEER_RULE")
	if err := os.Symlink(filepath.Join(root, "api"), filepath.Join(root, "web")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := roleInstructions(rulesRequest(root, "web/child")); err == nil {
		t.Fatal("symlinked module could inject peer rules")
	}
}
