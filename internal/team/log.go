package team

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Time    time.Time `json:"time"`
	RunID   string    `json:"run_id"`
	Phase   string    `json:"phase"`
	Role    string    `json:"role,omitempty"`
	Status  string    `json:"status"`
	Version int       `json:"contract_version"`
	Attempt int       `json:"attempt"`
	Message string    `json:"message"`
}

// Logger serializes human stderr and machine JSONL; it never writes model
// credentials, tool arguments or full private transcripts to the shared log.
type Logger struct {
	mu    sync.Mutex
	file  *os.File
	out   io.Writer
	runID string
	err   error
}

func NewLogger(file, runID string, out io.Writer) (*Logger, error) {
	f, e := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	if out == nil {
		out = io.Discard
	}
	return &Logger{file: f, out: out, runID: runID}, nil
}

func (l *Logger) Emit(phase, role, status string, version, attempt int, message string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	message = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, message)
	ev := Event{Time: time.Now().UTC(), RunID: l.runID, Phase: phase, Role: role, Status: status, Version: version, Attempt: attempt, Message: message}
	if e := json.NewEncoder(l.file).Encode(ev); e != nil && l.err == nil {
		l.err = e
	}
	if e := l.file.Sync(); e != nil && l.err == nil {
		l.err = e
	}
	label := phase
	if role != "" {
		label += "/" + role
	}
	_, e := fmt.Fprintf(l.out, "%s [%s] [%s] %-9s v%d round=%d %s\n", ev.Time.Format("15:04:05"), l.runID, label, status, version, attempt, message)
	if e != nil && l.err == nil {
		l.err = e
	}
}
func (l *Logger) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.file.Close()
	if l.err != nil {
		return l.err
	}
	return e
}
