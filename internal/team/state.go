package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type RoleState struct {
	Status          string `json:"status"`
	ContractVersion int    `json:"contract_version"`
	Summary         string `json:"summary,omitempty"`
	Workdir         string `json:"workdir,omitempty"`
}

type State struct {
	Version            int                  `json:"version"`
	ID                 string               `json:"id"`
	ProjectDir         string               `json:"project_dir"`
	Base               string               `json:"base"`
	Spec               Spec                 `json:"spec"`
	Phase              string               `json:"phase"`
	Status             string               `json:"status"`
	Attempt            int                  `json:"attempt"`
	Repairs            int                  `json:"repairs"`
	Roles              map[string]RoleState `json:"roles"`
	Checks             []CheckResult        `json:"checks,omitempty"`
	CheckHistory       []CheckResult        `json:"check_history,omitempty"`
	Proposal           *Contract            `json:"proposal,omitempty"`
	Error              string               `json:"error,omitempty"`
	IntegrationDir     string               `json:"integration_dir,omitempty"`
	Patch              string               `json:"patch,omitempty"`
	ExperienceIDs      []string             `json:"experience_ids,omitempty"`
	ExperienceComplete bool                 `json:"experience_complete"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

func WriteJSON(file string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(file), ".write-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, file)
}

func (s *State) Save(dir string) error {
	s.UpdatedAt = time.Now().UTC()
	return WriteJSON(filepath.Join(dir, "state.json"), s)
}

func LoadState(dir string) (*State, error) {
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil {
		return nil, e
	}
	var s State
	if e = json.Unmarshal(b, &s); e != nil {
		return nil, e
	}
	if s.Version != 1 || s.ID == "" || s.Roles == nil {
		return nil, fmt.Errorf("invalid run state")
	}
	if e = s.Spec.Validate(); e != nil {
		return nil, e
	}
	return &s, nil
}

func (s *State) SetRole(role, status string, version int, summary string) error {
	if role != "frontend" && role != "backend" {
		return fmt.Errorf("unknown role %q", role)
	}
	if version != s.Spec.Contract.Version {
		return fmt.Errorf("stale %s result: v%d, expected v%d", role, version, s.Spec.Contract.Version)
	}
	switch status {
	case "pending", "running", "ready", "blocked", "failed", "change_requested":
	default:
		return fmt.Errorf("invalid role status %q", status)
	}
	r := s.Roles[role]
	r.Status = status
	r.ContractVersion = version
	r.Summary = summary
	s.Roles[role] = r
	return nil
}

func (s *State) AcceptContract(c Contract) error {
	if s.Status == "completed" {
		return fmt.Errorf("completed run cannot change its contract")
	}
	if e := c.Validate(); e != nil {
		return e
	}
	if c.Version != s.Spec.Contract.Version+1 {
		return fmt.Errorf("next contract version must be %d", s.Spec.Contract.Version+1)
	}
	s.Spec.Contract = c
	s.Proposal = nil
	s.Checks = nil
	s.Status = "pending"
	s.Phase = "alignment"
	s.Error = ""
	s.Repairs = 0
	for _, role := range Roles {
		r := s.Roles[role]
		r.Status = "pending"
		r.ContractVersion = c.Version
		r.Summary = "Contract changed; revalidate implementation"
		s.Roles[role] = r
	}
	return nil
}

func AcceptContract(dir string, c Contract) error {
	release, e := acquireLock(dir)
	if e != nil {
		return fmt.Errorf("run is in use: %w", e)
	}
	defer release()
	s, e := LoadState(dir)
	if e != nil {
		return e
	}
	if e = s.AcceptContract(c); e != nil {
		return e
	}
	if e = WriteJSON(filepath.Join(dir, fmt.Sprintf("contract-v%d.json", c.Version)), c); e != nil {
		return e
	}
	if e = s.Save(dir); e != nil {
		return e
	}
	l, e := NewLogger(filepath.Join(dir, "events.jsonl"), s.ID, nil)
	if e != nil {
		return e
	}
	l.Emit("alignment", "", "accepted", c.Version, s.Attempt, "Contract revision accepted explicitly; both workers must revalidate")
	return l.Close()
}
