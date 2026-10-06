package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentPhaseLogsRemainReadable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "events.jsonl")
	var human bytes.Buffer
	log, err := NewLogger(file, "run", &human)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) { defer wg.Done(); log.Emit("development", Roles[i%2], "tool", 2, 3, "one\nline\x1b") }(i)
	}
	wg.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 40 || strings.Count(human.String(), "\n") != 40 {
		t.Fatal("lost or interleaved events")
	}
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.RunID != "run" || event.Version != 2 || event.Attempt != 3 || event.Time.IsZero() || event.Message != "one line " {
			t.Fatalf("incomplete event: %+v", event)
		}
	}
}

type brokenLogOutput struct{}

func (brokenLogOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLoggingFailurePreventsSuccessfulDelivery(t *testing.T) {
	f := makeCoordinatorFixture(t, 0)
	log, err := NewLogger(filepath.Join(f.run, "broken.jsonl"), f.state.ID, brokenLogOutput{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	worker := coordinatorWorker(func(_ context.Context, req WorkRequest, _ func(string, string)) (WorkResult, error) {
		return successfulCoordinatorWork(req)
	})
	s, err := Execute(context.Background(), f.run, worker, log, f.memory)
	if !errors.Is(err, io.ErrClosedPipe) || s.Status == "completed" {
		t.Fatalf("logging failure reported success: %+v %v", s, err)
	}
}

func TestControllerLockRejectsConcurrentExecution(t *testing.T) {
	f := makeCoordinatorFixture(t, 0)
	release, err := acquireLock(f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	worker := coordinatorWorker(func(context.Context, WorkRequest, func(string, string)) (WorkResult, error) {
		t.Error("second controller started worker")
		return WorkResult{}, nil
	})
	if _, err := Execute(context.Background(), f.run, worker, f.logger, f.memory); err == nil || !strings.Contains(err.Error(), "already controlled") {
		t.Fatalf("lock not enforced: %v", err)
	}
}

func TestShellTimeoutAndExitPreserveEvidence(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell unavailable")
	}
	tool := shellTool{dir: t.TempDir()}
	for _, tc := range []struct{ name, args, want string }{
		{"nonzero", `{"command":"echo actual-failure; exit 7"}`, "actual-failure"},
		{"timeout", `{"command":"echo timed-start; sleep 30","timeout_ms":50}`, "timed-start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := tool.Execute(context.Background(), "id", json.RawMessage(tc.args), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing failure evidence: %v", err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("shell descendants prevented timeout")
			}
			if tc.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("not a timeout: %v", err)
			}
		})
	}
}
