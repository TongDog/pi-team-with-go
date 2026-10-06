package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

func runtimeTestRequest(t *testing.T, role string) WorkRequest {
	t.Helper()
	return WorkRequest{RunID: "test-run", Role: role, Phase: "development", Workdir: t.TempDir(), PrivateDir: t.TempDir(), Attempt: 1, Spec: Spec{Version: 1, Project: "test-project", Requirement: "Add user listing", Acceptance: []string{"List users through API"}, Contract: Contract{Version: 1, Summary: "User API"}, Frontend: RoleSpec{Task: "Implement list view", Paths: []string{"web"}}, Backend: RoleSpec{Task: "Implement list endpoint", Paths: []string{"api"}}, MaxTurns: 4, WorkerTimeoutSeconds: 5}}
}
func runtimeText(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}
func runtimeReady() agentcore.AssistantMessage {
	return runtimeText(`{"contract_version":1,"status":"ready","summary":"implemented"}`)
}
func runtimeWrite(path, content string) agentcore.AssistantMessage {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{agentcore.NewToolCallContent("write-1", "write", args)}}
}
func runtimeEvent(ctx context.Context, event provider.AssistantMessageEvent) *provider.AssistantMessageEventStream {
	s := provider.NewAssistantMessageEventStream(1)
	_ = s.Emit(ctx, event)
	s.Close()
	return s
}
func runtimeRun(ctx context.Context, req WorkRequest, stream provider.StreamFn, emit func(string, string)) (WorkResult, error) {
	if emit == nil {
		emit = func(string, string) {}
	}
	return (&RuntimeWorker{}).runWithStream(ctx, req, emit, "fake", "fake", "", stream, func(context.Context, string) string { return "" })
}
func transcriptBytes(t *testing.T, req WorkRequest) []byte {
	t.Helper()
	path := filepath.Join(req.PrivateDir, fmt.Sprintf("%s-%03d.json", req.Role, req.Attempt))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid transcript %s", data)
	}
	return data
}

func TestRuntimeWorkerIsolatedRealToolsAndTranscripts(t *testing.T) {
	requests := []WorkRequest{runtimeTestRequest(t, "frontend"), runtimeTestRequest(t, "backend")}
	for i := range requests {
		marker := requests[i].Role + "-private-guidance"
		if err := os.WriteFile(filepath.Join(requests[i].Workdir, "AGENTS.md"), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		for _, role := range Roles {
			module := filepath.Join(requests[i].Workdir, requests[i].Spec.Role(role).Paths[0])
			if err := os.MkdirAll(module, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(module, "AGENTS.md"), []byte(role+"-module-guidance"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	for _, req := range requests {
		wg.Add(1)
		go func(req WorkRequest) {
			defer wg.Done()
			other := "frontend"
			if req.Role == "frontend" {
				other = "backend"
			}
			calls := 0
			var events []string
			stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
				calls++
				if !strings.Contains(llm.SystemPrompt, req.Role+"-private-guidance") || strings.Contains(llm.SystemPrompt, other+"-private-guidance") {
					t.Errorf("%s received wrong instructions: %s", req.Role, llm.SystemPrompt)
				}
				if !strings.Contains(llm.SystemPrompt, req.Role+"-module-guidance") || strings.Contains(llm.SystemPrompt, other+"-module-guidance") {
					t.Errorf("%s received wrong module rules: %s", req.Role, llm.SystemPrompt)
				}
				b, err := json.Marshal(llm.Messages)
				if err != nil {
					return nil, err
				}
				if strings.Contains(string(b), other+"-private-output") {
					t.Errorf("%s received peer transcript", req.Role)
				}
				if calls == 1 {
					if len(llm.Messages) != 1 {
						t.Errorf("initial role context has %d messages", len(llm.Messages))
					}
					var input WorkRequest
					user, ok := llm.Messages[0].(agentcore.UserMessage)
					if !ok {
						return nil, errors.New("initial request not a user message")
					}
					if err = json.Unmarshal([]byte(agentcore.ContentToText(user.Content)), &input); err != nil {
						return nil, err
					}
					if input.Role != req.Role || input.Spec.Contract.Version != 1 {
						t.Errorf("wrong worker input: %+v", input)
					}
					return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeWrite("owned.txt", req.Role+"-private-output")}), nil
				}
				found := false
				for _, message := range llm.Messages {
					if result, ok := message.(agentcore.ToolResultMessage); ok {
						found = true
						if result.IsError {
							t.Errorf("write failed: %+v", result)
						}
					}
				}
				if !found {
					t.Error("tool result was not fed into real loop")
				}
				return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeReady()}), nil
			}
			result, err := runtimeRun(context.Background(), req, stream, func(kind, message string) { events = append(events, kind+":"+message) })
			if err != nil {
				t.Errorf("%s: %v", req.Role, err)
				return
			}
			if result.Status != "ready" || calls != 2 {
				t.Errorf("%s result=%+v calls=%d", req.Role, result, calls)
			}
			data, err := os.ReadFile(filepath.Join(req.Workdir, "owned.txt"))
			if err != nil || string(data) != req.Role+"-private-output" {
				t.Errorf("%s tool wrote wrong root: %s %v", req.Role, data, err)
			}
			if !strings.Contains(strings.Join(events, ","), "tool:write") {
				t.Errorf("missing tool event: %v", events)
			}
		}(req)
	}
	wg.Wait()
	for _, req := range requests {
		data := transcriptBytes(t, req)
		other := "frontend"
		if req.Role == "frontend" {
			other = "backend"
		}
		if strings.Contains(string(data), other+"-private-output") {
			t.Errorf("%s persisted peer output", req.Role)
		}
		if !strings.Contains(string(data), req.Role+"-private-output") {
			t.Errorf("%s omitted own tool transcript", req.Role)
		}
	}
}

func TestRuntimeWorkerProviderFailuresAndInvalidReport(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		early      bool
		message    agentcore.AssistantMessage
	}{
		{name: "early provider error", want: "provider unavailable", early: true},
		{name: "terminal provider error", want: "provider terminal error", message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonError, ErrorMessage: "provider terminal error"}},
		{name: "invalid report", want: "required JSON report", message: runtimeText("I finished without JSON")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := runtimeTestRequest(t, "frontend")
			stream := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
				if tc.early {
					return nil, errors.New("provider unavailable")
				}
				if tc.message.StopReason == agentcore.StopReasonError {
					return runtimeEvent(ctx, provider.StreamErrorEvent{Message: tc.message, Err: errors.New(tc.want)}), nil
				}
				return runtimeEvent(ctx, provider.StreamDoneEvent{Message: tc.message}), nil
			}
			_, err := runtimeRun(context.Background(), req, stream, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			transcriptBytes(t, req)
		})
	}
}

func TestRuntimeWorkerTurnLimit(t *testing.T) {
	req := runtimeTestRequest(t, "frontend")
	req.Spec.MaxTurns = 1
	var calls atomic.Int32
	stream := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		calls.Add(1)
		return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeWrite("limit.txt", "one turn")}), nil
	}
	_, err := runtimeRun(context.Background(), req, stream, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded 1 model turns") {
		t.Fatalf("expected bounded run, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times", calls.Load())
	}
	transcriptBytes(t, req)
}
func TestRuntimeWorkerCancellation(t *testing.T) {
	req := runtimeTestRequest(t, "backend")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	stream := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() { _, err := runtimeRun(ctx, req, stream, nil); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider not started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop worker")
	}
	transcriptBytes(t, req)
}
func TestRuntimeWorkerExperienceCannotExecuteTools(t *testing.T) {
	req := runtimeTestRequest(t, "experience")
	req.KnowledgeRoot = t.TempDir()
	calls := 0
	stream := func(ctx context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		calls++
		if len(llm.Tools) != 0 {
			t.Errorf("experience worker has %d tools", len(llm.Tools))
		}
		if !strings.Contains(llm.SystemPrompt, "cannot modify code or approve knowledge") {
			t.Error("wrong extraction prompt")
		}
		if calls == 1 {
			return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeWrite("forbidden.txt", "must not exist")}), nil
		}
		foundError := false
		for _, m := range llm.Messages {
			if result, ok := m.(agentcore.ToolResultMessage); ok && result.IsError {
				foundError = true
			}
		}
		if !foundError {
			t.Error("unknown tool call did not produce error evidence")
		}
		return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeText(`{"contract_version":1,"status":"ready","summary":"no supported experience","experiences":[]}`)}), nil
	}
	result, err := runtimeRun(context.Background(), req, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" {
		t.Fatalf("result: %+v", result)
	}
	if _, err = os.Stat(filepath.Join(req.Workdir, "forbidden.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("experience tool unexpectedly wrote file: %v", err)
	}
	transcriptBytes(t, req)
}
func TestRuntimeWorkerTranscriptFailureIsReported(t *testing.T) {
	req := runtimeTestRequest(t, "frontend")
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	req.PrivateDir = blocked
	stream := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeReady()}), nil
	}
	if _, err := runtimeRun(context.Background(), req, stream, nil); err == nil {
		t.Fatal("successful report masked transcript failure")
	}
	data, err := os.ReadFile(blocked)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing file altered: %s %v", data, err)
	}
}

func TestRuntimeWorkerRejectsBackgroundShell(t *testing.T) {
	req := runtimeTestRequest(t, "backend")
	calls := 0
	var events []string
	stream := func(ctx context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		calls++
		if calls == 1 {
			return runtimeEvent(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{agentcore.NewToolCallContent("background", "bash", json.RawMessage(`{"command":"echo leaked > background.txt","run_in_background":true}`))}}}), nil
		}
		rejected := false
		for _, m := range llm.Messages {
			if result, ok := m.(agentcore.ToolResultMessage); ok && result.IsError && strings.Contains(agentcore.ContentToText(result.Content), "not available") {
				rejected = true
			}
		}
		if !rejected {
			t.Error("background shell was not explicitly rejected")
		}
		return runtimeEvent(ctx, provider.StreamDoneEvent{Message: runtimeReady()}), nil
	}
	if _, err := runtimeRun(context.Background(), req, stream, func(kind, message string) { events = append(events, kind+":"+message) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(req.Workdir, "background.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background command executed: %v", err)
	}
	if !strings.Contains(strings.Join(events, ","), "tool_error:bash") {
		t.Fatalf("missing tool failure log: %v", events)
	}
}
func TestRuntimeWorkerCancelsActiveShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	req := runtimeTestRequest(t, "backend")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	stream := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return runtimeEvent(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{agentcore.NewToolCallContent("long-shell", "bash", json.RawMessage(`{"command":"echo ready > started.txt; sleep 30; echo leaked > survived.txt"}`))}}}), nil
	}
	go func() { _, err := runtimeRun(ctx, req, stream, nil); done <- err }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case <-ticker.C:
			_, err := os.Stat(filepath.Join(req.Workdir, "started.txt"))
			ready = err == nil
		case err := <-done:
			t.Fatalf("worker returned before shell started: %v", err)
		case <-deadline.C:
			t.Fatal("shell never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active shell prevented cancellation")
	}
	if _, err := os.Stat(filepath.Join(req.Workdir, "survived.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled shell completed: %v", err)
	}
	transcriptBytes(t, req)
}
