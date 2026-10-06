// Package workspace isolates team workers from the user's checkout and combines
// only changes within each worker's declared ownership roots.
package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Manager struct{ Project, RunDir, Base string }
type Result struct {
	Dir, Patch string
	Changed    map[string][]string
}
type metadata struct {
	Project, RunDir, Base string
	Version               int
}

var roles = []string{"frontend", "backend"}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = 2 * time.Second
	// Do not let the caller's Git environment redirect a generated worktree's index.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args[6:], " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func destination(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for p := abs; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			root, err := canonical(p)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				root = filepath.Join(root, tail[i])
			}
			return root, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		tail = append(tail, filepath.Base(p))
	}
}

// noLinks rejects symlinks in any existing component, including the final path.
func noLinks(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path is not allowed: %s", p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}

func projectRoot(ctx context.Context, project string) (string, error) {
	p, err := canonical(project)
	if err != nil {
		return "", err
	}
	out, err := git(ctx, p, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root, err := canonical(strings.TrimSpace(string(out)))
	if err != nil {
		return "", err
	}
	if p != root {
		return "", errors.New("project must be the Git checkout root")
	}
	return root, nil
}

func New(ctx context.Context, project, runDir string) (*Manager, error) {
	p, err := projectRoot(ctx, project)
	if err != nil {
		return nil, err
	}
	status, err := git(ctx, p, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if len(status) > 0 {
		return nil, errors.New("project has tracked or untracked changes; commit or move them before starting team work")
	}
	base, err := git(ctx, p, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	r, err := destination(runDir)
	if err != nil {
		return nil, err
	}
	if r == p {
		return nil, errors.New("run directory cannot be the project root")
	}
	if strings.HasPrefix(r, p+string(filepath.Separator)) {
		if _, err := git(ctx, p, "check-ignore", "-q", "--", r+string(filepath.Separator)); err != nil {
			return nil, errors.New("run directory inside the project must be Git-ignored")
		}
	}
	// Canonicalize existing ancestors (macOS /tmp is itself a symlink).
	if err := os.MkdirAll(r, 0700); err != nil {
		return nil, err
	}
	r, err = canonical(r)
	if err != nil {
		return nil, err
	}
	if r == p {
		return nil, errors.New("run directory cannot be the project root")
	}
	m := &Manager{p, r, strings.TrimSpace(string(base))}
	data, _ := json.Marshal(metadata{p, r, m.Base, 1})
	path := filepath.Join(r, "workspace-owner.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create workspace ownership: %w", err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return m, nil
}

func Restore(ctx context.Context, project, runDir, base string) (*Manager, error) {
	p, err := projectRoot(ctx, project)
	if err != nil {
		return nil, err
	}
	r, err := canonical(runDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{p, r, base}
	if err := m.validate(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Base == "" || len(m.Base) < 40 || strings.Trim(m.Base, "0123456789abcdef") != "" {
		return errors.New("invalid base commit")
	}
	if !filepath.IsAbs(m.Project) || !filepath.IsAbs(m.RunDir) || filepath.Clean(m.RunDir) != m.RunDir || m.Project == m.RunDir {
		return errors.New("invalid workspace paths")
	}
	if err := noLinks(m.RunDir); err != nil {
		return err
	}
	path := filepath.Join(m.RunDir, "workspace-owner.json")
	if err := noLinks(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var owner metadata
	if err := json.Unmarshal(data, &owner); err != nil {
		return err
	}
	if owner != (metadata{m.Project, m.RunDir, m.Base, 1}) {
		return errors.New("workspace ownership metadata mismatch")
	}
	_, err = git(ctx, m.Project, "cat-file", "-e", m.Base+"^{commit}")
	return err
}

func (m *Manager) verifyTree(ctx context.Context, path string) error {
	if err := noLinks(path); err != nil {
		return err
	}
	root, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(root)) != path {
		return errors.New("generated directory is not its own worktree")
	}
	want, err := git(ctx, m.Project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	got, err := git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		return errors.New("worktree belongs to a different repository")
	}
	// A copied .git pointer must not grant ownership of an arbitrary directory.
	gitDir, err := git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	backlink, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(gitDir)), "gitdir"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(backlink)) != filepath.Join(path, ".git") {
		return errors.New("worktree registration path mismatch")
	}
	return nil
}

func (m *Manager) create(ctx context.Context, name string, reuse bool) (string, error) {
	if err := m.validate(ctx); err != nil {
		return "", err
	}
	path := filepath.Join(m.RunDir, "workspaces", name)
	if err := noLinks(path); err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		if !reuse {
			return "", errors.New("integration attempt already exists; use a new attempt number")
		}
		if err := m.verifyTree(ctx, path); err != nil {
			return "", err
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	if _, err := git(ctx, m.Project, "worktree", "add", "--detach", path, m.Base); err != nil {
		return "", err
	}
	if err := m.verifyTree(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

func (m *Manager) Worker(ctx context.Context, role string) (string, error) {
	if role != "frontend" && role != "backend" {
		return "", errors.New("unknown worker role")
	}
	return m.create(ctx, role, true)
}

func validPath(path string) bool {
	return path != "" && path != "." && !strings.ContainsAny(path, "\\\x00\r\n") && !strings.HasPrefix(path, "/") && filepath.ToSlash(filepath.Clean(path)) == path && path != ".." && !strings.HasPrefix(path, "../") && path != ".git" && !strings.HasPrefix(path, ".git/")
}
func contains(root, path string) bool { return path == root || strings.HasPrefix(path, root+"/") }

func ownership(owners map[string][]string) (map[string][]string, error) {
	if len(owners) != 2 {
		return nil, errors.New("ownership must specify exactly frontend and backend")
	}
	normal := map[string][]string{}
	for _, role := range roles {
		if len(owners[role]) == 0 {
			return nil, fmt.Errorf("missing ownership for %s", role)
		}
		for _, root := range owners[role] {
			root = strings.TrimSuffix(root, "/")
			if !validPath(root) {
				return nil, fmt.Errorf("invalid ownership root %q", root)
			}
			normal[role] = append(normal[role], root)
		}
	}
	for _, a := range normal["frontend"] {
		for _, b := range normal["backend"] {
			if contains(a, b) || contains(b, a) {
				return nil, fmt.Errorf("ownership overlap: %s and %s", a, b)
			}
		}
	}
	return normal, nil
}

func (m *Manager) patch(ctx context.Context, role string, roots []string) ([]byte, []string, error) {
	dir, err := m.Worker(ctx, role)
	if err != nil {
		return nil, nil, err
	}
	if _, err := git(ctx, dir, "add", "--all", "--", "."); err != nil {
		return nil, nil, err
	}
	out, err := git(ctx, dir, "diff", "--cached", "--name-only", "--no-renames", "-z", m.Base, "--")
	if err != nil {
		return nil, nil, err
	}
	var changed []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := string(raw)
		if !validPath(path) {
			return nil, nil, fmt.Errorf("unsafe changed path %q", path)
		}
		allowed := false
		for _, root := range roots {
			if contains(root, path) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, fmt.Errorf("%s changed unowned path %s", role, path)
		}
		if err := noLinks(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			return nil, nil, err
		}
		entry, err := git(ctx, dir, "ls-files", "--stage", "-z", "--", path)
		if err != nil {
			return nil, nil, err
		}
		if bytes.HasPrefix(entry, []byte("160000 ")) || bytes.HasPrefix(entry, []byte("120000 ")) {
			return nil, nil, fmt.Errorf("gitlink or symlink change is not allowed: %s", path)
		}
		previous, err := git(ctx, dir, "ls-tree", "-z", m.Base, "--", path)
		if err != nil {
			return nil, nil, err
		}
		if bytes.HasPrefix(previous, []byte("160000 ")) || bytes.HasPrefix(previous, []byte("120000 ")) {
			return nil, nil, fmt.Errorf("gitlink or symlink change is not allowed: %s", path)
		}
		changed = append(changed, path)
	}
	patch, err := git(ctx, dir, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames", m.Base, "--")
	return patch, changed, err
}

func (m *Manager) Integrate(ctx context.Context, attempt int, owners map[string][]string) (Result, error) {
	var result Result
	if attempt < 1 {
		return result, errors.New("attempt must be positive")
	}
	if err := m.validate(ctx); err != nil {
		return result, err
	}
	roots, err := ownership(owners)
	if err != nil {
		return result, err
	}
	patches := map[string][]byte{}
	result.Changed = map[string][]string{}
	for _, role := range roles {
		patch, changed, err := m.patch(ctx, role, roots[role])
		if err != nil {
			return result, err
		}
		patches[role], result.Changed[role] = patch, changed
	}
	dir, err := m.create(ctx, "integration-"+strconv.Itoa(attempt), false)
	if err != nil {
		return result, err
	}
	result.Dir = dir
	patchDir := filepath.Join(m.RunDir, "patches")
	if err := noLinks(patchDir); err != nil {
		return result, err
	}
	if err := os.MkdirAll(patchDir, 0700); err != nil {
		return result, err
	}
	for _, role := range roles {
		if len(patches[role]) == 0 {
			continue
		}
		path := filepath.Join(patchDir, fmt.Sprintf("%s-%d.patch", role, attempt))
		if err := writeExclusive(path, patches[role]); err != nil {
			return result, err
		}
		if _, err := git(ctx, dir, "apply", "--index", "--binary", "--", path); err != nil {
			return result, err
		}
	}
	patch, err := git(ctx, dir, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", m.Base, "--")
	if err != nil {
		return result, err
	}
	result.Patch = filepath.Join(patchDir, fmt.Sprintf("integration-%d.patch", attempt))
	if err := writeExclusive(result.Patch, patch); err != nil {
		return result, err
	}
	return result, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
