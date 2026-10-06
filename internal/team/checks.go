package team

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type CheckResult struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	Log        string `json:"log"`
	DurationMS int64  `json:"duration_ms"`
}

// cappedOutput bounds both log size and prompt feedback. Once full, writes are
// consumed but discarded so a noisy child cannot exhaust disk or deadlock.
type cappedOutput struct {
	mu    sync.Mutex
	file  io.Writer
	text  []byte
	total int
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += n
	if len(w.text) < 16384 {
		k := min(n, 16384-len(w.text))
		w.text = append(w.text, p[:k]...)
	}
	if w.total <= 4<<20 {
		if _, e := w.file.Write(p); e != nil {
			return 0, e
		}
	}
	return n, nil
}

func runChecks(ctx context.Context, dir, runDir string, spec Spec, attempt int, log *Logger) ([]CheckResult, error) {
	results := make([]CheckResult, 0, len(spec.Checks))
	if e := os.MkdirAll(filepath.Join(runDir, "checks"), 0700); e != nil {
		return nil, e
	}
	for i, c := range spec.Checks {
		if e := ctx.Err(); e != nil {
			return results, e
		}
		log.Emit("verification", c.Owner, "started", spec.Contract.Version, attempt, c.Name)
		wd := dir
		if c.Dir != "" {
			wd = filepath.Join(dir, c.Dir)
		}
		resolved, e := filepath.EvalSymlinks(wd)
		if e != nil {
			return results, fmt.Errorf("check %s directory: %w", c.Name, e)
		}
		root, e := filepath.EvalSymlinks(dir)
		if e != nil {
			return results, e
		}
		rel, e := filepath.Rel(root, resolved)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return results, fmt.Errorf("check directory escapes integration workspace")
		}
		seconds := c.TimeoutSeconds
		if seconds == 0 {
			seconds = 120
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		file := filepath.Join(runDir, "checks", fmt.Sprintf("%03d-%02d.log", attempt, i))
		f, e := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			cancel()
			return results, e
		}
		capture := &cappedOutput{file: f}
		cmd := exec.CommandContext(checkCtx, c.Command[0], c.Command[1:]...)
		cmd.Dir = resolved
		cmd.Stdout = capture
		cmd.Stderr = capture
		prepareCommand(cmd)
		cmd.Env = append(os.Environ(), "PIGO_TEAM_RUN_DIR="+runDir, fmt.Sprintf("PIGO_TEAM_CONTRACT_VERSION=%d", spec.Contract.Version))
		start := time.Now()
		runErr := cmd.Run()
		contextErr := checkCtx.Err()
		cancel()
		closeErr := f.Close()
		if closeErr != nil {
			return results, closeErr
		}
		code := 0
		if runErr != nil {
			code = -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
		}
		output := string(capture.text)
		if runErr != nil {
			output += "\n" + runErr.Error()
		}
		if contextErr != nil {
			output += "\n" + contextErr.Error()
		}
		if capture.total > len(capture.text) {
			output += "\n[output preview truncated; see log]"
		}
		r := CheckResult{Name: c.Name, Owner: c.Owner, Passed: runErr == nil && contextErr == nil, ExitCode: code, Output: output, Log: file, DurationMS: time.Since(start).Milliseconds()}
		results = append(results, r)
		status := "passed"
		if !r.Passed {
			status = "failed"
		}
		log.Emit("verification", c.Owner, status, spec.Contract.Version, attempt, fmt.Sprintf("%s exit=%d duration=%dms log=%s", c.Name, r.ExitCode, r.DurationMS, file))
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
	}
	return results, nil
}
