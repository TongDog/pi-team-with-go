package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func command(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func put(t *testing.T, root, path, data string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) (*Manager, string, string) {
	t.Helper()
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command(t, root, "init")
	command(t, root, "config", "user.email", "test@example.com")
	command(t, root, "config", "user.name", "Test")
	put(t, root, ".gitignore", ".pigo/\n")
	put(t, root, "web/app.txt", "base frontend\n")
	put(t, root, "api/main.txt", "base backend\n")
	command(t, root, "add", ".")
	command(t, root, "commit", "-m", "fixture")
	m, err := New(context.Background(), root, filepath.Join(root, ".pigo", "run"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Worker(context.Background(), "frontend")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Worker(context.Background(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	return m, f, b
}
func scopes() map[string][]string {
	return map[string][]string{"frontend": {"web/"}, "backend": {"api/"}}
}

func TestIndependentWorktreesAndScopedIntegration(t *testing.T) {
	m, f, b := setup(t)
	if f == b || f == m.Project || b == m.Project {
		t.Fatal("not independent")
	}
	put(t, f, "web/app.txt", "frontend changed\n")
	put(t, f, "web/new.bin", string([]byte{0, 1, 2, 3}))
	// A worker may commit; integration still collects everything since Base.
	command(t, b, "config", "user.email", "test@example.com")
	put(t, b, "api/main.txt", "backend changed\n")
	command(t, b, "add", ".")
	command(t, b, "commit", "-m", "worker change")
	result, err := m.Integrate(context.Background(), 1, scopes())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changed["frontend"]) != 2 || len(result.Changed["backend"]) != 1 {
		t.Fatalf("changes: %#v", result.Changed)
	}
	for path, want := range map[string]string{"web/app.txt": "frontend changed\n", "api/main.txt": "backend changed\n", "web/new.bin": string([]byte{0, 1, 2, 3})} {
		got, err := os.ReadFile(filepath.Join(result.Dir, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", path, got, err)
		}
	}
	if command(t, m.Project, "status", "--porcelain") != "" {
		t.Fatal("original checkout modified")
	}
	if command(t, m.Project, "rev-parse", "HEAD") != m.Base {
		t.Fatal("original HEAD changed")
	}
	if data, err := os.ReadFile(result.Patch); err != nil || len(data) == 0 {
		t.Fatalf("patch: %v", err)
	}
	if _, err := m.Integrate(context.Background(), 1, scopes()); err == nil {
		t.Fatal("reused integration attempt")
	}
}

func TestRejectUnownedChanges(t *testing.T) {
	for _, path := range []string{"api/main.txt", "unknown.txt", "web/../../escape"} {
		t.Run(path, func(t *testing.T) {
			m, f, _ := setup(t)
			if strings.Contains(path, "..") {
				o := scopes()
				o["frontend"] = []string{path}
				if _, err := m.Integrate(context.Background(), 1, o); err == nil {
					t.Fatal("accepted traversal ownership")
				}
				return
			}
			put(t, f, path, "bad\n")
			if _, err := m.Integrate(context.Background(), 1, scopes()); err == nil {
				t.Fatal("accepted unowned path")
			}
		})
	}
}

func TestRejectOverlappingOwnership(t *testing.T) {
	m, _, _ := setup(t)
	o := scopes()
	o["backend"] = []string{"web/nested"}
	if _, err := m.Integrate(context.Background(), 1, o); err == nil {
		t.Fatal("accepted overlap")
	}
}

func TestRejectSymlinkAndGitlink(t *testing.T) {
	t.Run("escape symlink", func(t *testing.T) {
		m, f, _ := setup(t)
		if err := os.Symlink(m.Project, filepath.Join(f, "web", "escape")); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Integrate(context.Background(), 1, scopes()); err == nil {
			t.Fatal("accepted symlink")
		}
	})
	t.Run("gitlink", func(t *testing.T) {
		m, f, _ := setup(t)
		command(t, f, "update-index", "--add", "--cacheinfo", "160000,"+m.Base+",web/submodule")
		if err := os.MkdirAll(filepath.Join(f, "web", "submodule"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Integrate(context.Background(), 1, scopes()); err == nil {
			t.Fatal("accepted gitlink")
		}
	})
}

func TestRestoreAndTamperedMetadata(t *testing.T) {
	m, f, _ := setup(t)
	r, err := Restore(context.Background(), m.Project, m.RunDir, m.Base)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Worker(context.Background(), "frontend")
	if err != nil || got != f {
		t.Fatalf("reopen: %s %v", got, err)
	}
	if _, err := Restore(context.Background(), m.Project, m.RunDir, strings.Repeat("a", 40)); err == nil {
		t.Fatal("accepted mismatched base")
	}
	put(t, m.RunDir, "workspace-owner.json", `{"Project":"/","Version":1}`)
	if _, err := Restore(context.Background(), m.Project, m.RunDir, m.Base); err == nil {
		t.Fatal("accepted tampered metadata")
	}
}

func TestRejectCopiedGitPointer(t *testing.T) {
	m, f, _ := setup(t)
	// Copying a real worktree's pointer does not establish ownership.
	other := filepath.Join(m.RunDir, "workspaces", "integration-8")
	data, err := os.ReadFile(filepath.Join(f, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, other, ".git", string(data))
	if err := m.verifyTree(context.Background(), other); err == nil {
		t.Fatal("accepted copied git pointer")
	}
}

func TestCancellationAndInvalidRole(t *testing.T) {
	m, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Worker(ctx, "frontend"); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := m.Integrate(ctx, 1, scopes()); err != context.Canceled {
		t.Fatalf("cancel integrate: %v", err)
	}
	if _, err := m.Worker(context.Background(), "../../elsewhere"); err == nil {
		t.Fatal("accepted invalid role")
	}
}

func TestNewRefusesDirtySource(t *testing.T) {
	m, _, _ := setup(t)
	put(t, m.Project, "untracked.txt", "dirty")
	if _, err := New(context.Background(), m.Project, filepath.Join(m.Project, ".pigo", "other")); err == nil {
		t.Fatal("accepted dirty checkout")
	}
}

func TestNewRefusesNonIgnoredRunDirectory(t *testing.T) {
	m, _, _ := setup(t)
	run := filepath.Join(m.Project, "unignored-run")
	if _, err := New(context.Background(), m.Project, run); err == nil {
		t.Fatal("accepted a run directory that changes the source checkout")
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatal("created refused run directory")
	}
}

func TestWorktreePathCannotEscapeThroughSymlink(t *testing.T) {
	m, f, _ := setup(t)
	// Move the actual worktree, then replace its expected location with a link.
	moved := filepath.Join(m.RunDir, "moved-frontend")
	if err := os.Rename(f, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, f); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Worker(context.Background(), "frontend"); err == nil {
		t.Fatal("accepted symlink worktree")
	}
}

func TestTrackedDeletionIsCollected(t *testing.T) {
	m, f, _ := setup(t)
	if err := os.Remove(filepath.Join(f, "web", "app.txt")); err != nil {
		t.Fatal(err)
	}
	result, err := m.Integrate(context.Background(), 1, scopes())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changed["frontend"]) != 1 {
		t.Fatal("deletion missing")
	}
	if _, err := os.Stat(filepath.Join(result.Dir, "web", "app.txt")); !os.IsNotExist(err) {
		t.Fatal("deletion not integrated")
	}
	if _, err := os.Stat(filepath.Join(m.Project, "web", "app.txt")); err != nil {
		t.Fatal("source file removed")
	}
}
