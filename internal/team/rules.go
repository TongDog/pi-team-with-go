package team

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxRoleRuleBytes bounds additional module rules without silently truncating
// mandatory instructions. The runtime separately loads the root AGENTS.md.
const maxRoleRuleBytes = 128 << 10

// roleInstructions loads AGENTS.md at the owned directories and their ancestors,
// in general-to-specific order. It does not recursively inspect unlocated child
// modules or peer-owned directories. Existing file ownership uses its parent;
// missing paths still inherit rules from every existing ancestor directory.
func roleInstructions(req WorkRequest) ([]string, error) {
	if req.Role == "experience" {
		return nil, nil
	}
	if req.Role != "frontend" && req.Role != "backend" {
		return nil, fmt.Errorf("unknown rule-loading role %q", req.Role)
	}
	if strings.TrimSpace(req.Workdir) == "" {
		return nil, fmt.Errorf("working directory is required for module rules")
	}
	root, err := filepath.Abs(req.Workdir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("module rule root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("module rule root is not a directory")
	}
	dirs := map[string]bool{}
	for _, owned := range req.Spec.Role(req.Role).Paths {
		if !SafeRelative(owned) {
			return nil, fmt.Errorf("unsafe module rule scope %q", owned)
		}
		rel := filepath.FromSlash(owned)
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect rule scope %s: %w", owned, err)
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("module rule scope %s is a symlink; explicit directory ownership required", owned)
			}
			if !info.IsDir() {
				rel = filepath.Dir(rel)
			}
		}
		for rel != "." {
			dirs[rel] = true
			rel = filepath.Dir(rel)
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := strings.Count(filepath.ToSlash(ordered[i]), "/"), strings.Count(filepath.ToSlash(ordered[j]), "/")
		if left != right {
			return left < right
		}
		return ordered[i] < ordered[j]
	})
	var instructions []string
	total := 0
	for _, dir := range ordered {
		full := filepath.Join(root, dir)
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect module rules directory %s: %w", dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("module rules directory %s is a symlink", dir)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("module rules scope %s is not a directory", dir)
		}
		file := filepath.Join(full, "AGENTS.md")
		info, err = os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect module rules %s: %w", file, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("module rules %s must be a regular file", file)
		}
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("read module rules %s: %w", file, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(f, int64(maxRoleRuleBytes-total+1)))
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read module rules %s: %w", file, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close module rules %s: %w", file, closeErr)
		}
		total += len(content)
		if total > maxRoleRuleBytes {
			return nil, fmt.Errorf("module rules exceed %d bytes in total; refusing to truncate mandatory rules (at %s)", maxRoleRuleBytes, file)
		}
		if len(strings.TrimSpace(string(content))) == 0 {
			continue
		}
		source := filepath.ToSlash(filepath.Join(dir, "AGENTS.md"))
		scope := filepath.ToSlash(dir) + "/"
		instructions = append(instructions, fmt.Sprintf("Module rules from %s (source: %s). Scope: %s and descendants, only within this role's owned paths. More specific module rules follow; report unresolved rule conflicts.\n\n%s", source, file, scope, string(content)))
	}
	return instructions, nil
}
