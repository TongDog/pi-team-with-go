package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// shellTool keeps commands inside the worker lifecycle. Independent working
// directories are not a security sandbox, but cancellation must reap children.
type shellTool struct{ dir string }

func (*shellTool) Name() string { return "bash" }
func (*shellTool) Description() string {
	return "Run a shell command in this worker's directory. Commands have a bounded timeout; background jobs are unavailable. Nonzero exits are errors."
}
func (*shellTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer","minimum":0,"maximum":600000},"run_in_background":{"type":"boolean","description":"Must be false; background jobs are not available."}},"required":["command"],"additionalProperties":false}`)
}
func (*shellTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}
func (t *shellTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var a struct {
		Command    string `json:"command"`
		Timeout    int    `json:"timeout_ms"`
		Background bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agentcore.AgentToolResult{}, err
	}
	if a.Background {
		return agentcore.AgentToolResult{}, fmt.Errorf("bash: background jobs are not available in team workers")
	}
	if strings.TrimSpace(a.Command) == "" || a.Timeout < 0 {
		return agentcore.AgentToolResult{}, fmt.Errorf("bash: command is required and timeout must be nonnegative")
	}
	timeout := 2 * time.Minute
	if a.Timeout > 0 {
		timeout = time.Duration(min(a.Timeout, 600000)) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		if runtime.GOOS == "windows" {
			shell, flag = "powershell", "-Command"
			if _, err := exec.LookPath(shell); err != nil {
				shell, flag = "cmd", "/c"
			}
		} else {
			shell = "sh"
		}
	}
	cmd := exec.CommandContext(runCtx, shell, flag, a.Command)
	cmd.Dir = t.dir
	prepareCommand(cmd)
	capture := &cappedOutput{file: io.Discard}
	cmd.Stdout, cmd.Stderr = capture, capture
	err := cmd.Run()
	// A shell can exit while children remain. Reap its process group as well.
	if cmd.Process != nil {
		_ = cmd.Cancel()
	}
	output := string(capture.text)
	if capture.total > len(capture.text) {
		output += "\n[output truncated]"
	}
	result := agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(output)}}
	if runCtx.Err() != nil {
		return result, fmt.Errorf("bash: %w\n%s", runCtx.Err(), output)
	}
	if err != nil {
		return result, fmt.Errorf("bash: %w\n%s", err, output)
	}
	return result, nil
}
