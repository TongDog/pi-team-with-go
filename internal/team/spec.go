// Package team coordinates isolated frontend/backend coding runs. Shared state
// is owned by the coordinator; workers exchange contracts and evidence, not chat.
package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

var Roles = []string{"frontend", "backend"}

type Endpoint struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Request  json.RawMessage   `json:"request,omitempty"`
	Response json.RawMessage   `json:"response"`
	Errors   map[string]string `json:"errors,omitempty"`
}

type Contract struct {
	Version   int        `json:"version"`
	Summary   string     `json:"summary"`
	Endpoints []Endpoint `json:"endpoints"`
}

// Fingerprint prevents unrelated task-local "v1" contracts from being treated
// as the same API. Summary and revision labels do not change endpoint identity.
func (c Contract) Fingerprint() string {
	b, _ := json.Marshal(c.Endpoints)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (c Contract) Validate() error {
	if c.Version < 1 || strings.TrimSpace(c.Summary) == "" || len(c.Endpoints) == 0 {
		return fmt.Errorf("contract requires a positive version, summary and endpoints")
	}
	seen := map[string]bool{}
	for _, e := range c.Endpoints {
		switch e.Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			return fmt.Errorf("invalid HTTP method %q", e.Method)
		}
		key := e.Method + " " + e.Path
		if !strings.HasPrefix(e.Path, "/") || seen[key] || len(e.Response) == 0 || !json.Valid(e.Response) {
			return fmt.Errorf("invalid or duplicate endpoint %q (response schema/example is required)", key)
		}
		if len(e.Request) > 0 && !json.Valid(e.Request) {
			return fmt.Errorf("invalid request for %s", key)
		}
		seen[key] = true
	}
	return nil
}

type RoleSpec struct {
	Task  string   `json:"task"`
	Paths []string `json:"paths"`
}

type Check struct {
	Name           string   `json:"name"`
	Owner          string   `json:"owner"`   // frontend, backend or both
	Command        []string `json:"command"` // argv, never implicitly interpreted as shell
	Dir            string   `json:"dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type Spec struct {
	Version              int      `json:"version"`
	Project              string   `json:"project"` // stable, explicitly selected knowledge scope
	Requirement          string   `json:"requirement"`
	Acceptance           []string `json:"acceptance"`
	Contract             Contract `json:"contract"`
	Frontend             RoleSpec `json:"frontend"`
	Backend              RoleSpec `json:"backend"`
	Checks               []Check  `json:"checks"`
	MaxRepairRounds      int      `json:"max_repair_rounds"`
	WorkerTimeoutSeconds int      `json:"worker_timeout_seconds,omitempty"`
	MaxTurns             int      `json:"max_turns,omitempty"`
}

func LoadSpec(file string) (Spec, error) {
	f, e := os.Open(file)
	if e != nil {
		return Spec{}, e
	}
	defer f.Close()
	var s Spec
	b, e := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if e != nil {
		return s, e
	}
	if len(b) > 2<<20 {
		return s, fmt.Errorf("spec exceeds 2 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&s); e != nil {
		return s, e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return s, fmt.Errorf("spec must contain exactly one JSON object")
	}
	return s, s.Validate()
}

func SafeRelative(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\:") && !strings.ContainsRune(p, 0) && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/") && p != ".pigo" && !strings.HasPrefix(p, ".pigo/")
}

func (s Spec) Role(role string) RoleSpec {
	if role == "frontend" {
		return s.Frontend
	}
	return s.Backend
}
func (s Spec) Ownership() map[string][]string {
	return map[string][]string{"frontend": s.Frontend.Paths, "backend": s.Backend.Paths}
}

func (s Spec) Validate() error {
	if s.Version != 1 || strings.TrimSpace(s.Project) == "" || strings.TrimSpace(s.Requirement) == "" || len(s.Acceptance) == 0 {
		return fmt.Errorf("spec requires version=1, project, requirement and acceptance criteria")
	}
	for _, a := range s.Acceptance {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("empty acceptance criterion")
		}
	}
	if e := s.Contract.Validate(); e != nil {
		return e
	}
	for _, role := range Roles {
		r := s.Role(role)
		if strings.TrimSpace(r.Task) == "" || len(r.Paths) == 0 {
			return fmt.Errorf("%s requires task and owned paths", role)
		}
		for _, p := range r.Paths {
			if !SafeRelative(p) {
				return fmt.Errorf("unsafe owned path %q", p)
			}
		}
	}
	for _, a := range s.Frontend.Paths {
		for _, b := range s.Backend.Paths {
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return fmt.Errorf("overlapping ownership: %s and %s", a, b)
			}
		}
	}
	if len(s.Checks) == 0 {
		return fmt.Errorf("at least one executable acceptance check is required")
	}
	seen := map[string]bool{}
	for _, c := range s.Checks {
		if c.Name == "" || seen[c.Name] || len(c.Command) == 0 || c.Command[0] == "" {
			return fmt.Errorf("checks require unique names and command argv")
		}
		seen[c.Name] = true
		if c.Owner != "frontend" && c.Owner != "backend" && c.Owner != "both" {
			return fmt.Errorf("check %s needs owner frontend/backend/both", c.Name)
		}
		if c.Dir != "" && c.Dir != "." && !SafeRelative(c.Dir) {
			return fmt.Errorf("unsafe check directory %q", c.Dir)
		}
		if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 3600 {
			return fmt.Errorf("check timeout must be 0..3600 seconds")
		}
	}
	if s.MaxRepairRounds < 0 || s.MaxRepairRounds > 10 {
		return fmt.Errorf("max_repair_rounds must be 0..10")
	}
	if s.WorkerTimeoutSeconds < 0 || s.WorkerTimeoutSeconds > 7200 || s.MaxTurns < 0 || s.MaxTurns > 200 {
		return fmt.Errorf("worker timeout/turn limit out of range")
	}
	return nil
}
