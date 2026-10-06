package team

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/experience"
	"github.com/smallnest/pigo/internal/team/workspace"
)

func Create(ctx context.Context, projectDir, runDir string, spec Spec) (*State, error) {
	if e := spec.Validate(); e != nil {
		return nil, e
	}
	if _, e := os.Stat(runDir); e == nil {
		return nil, fmt.Errorf("run directory already exists; use resume or choose a new directory")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	m, e := workspace.New(ctx, projectDir, runDir)
	if e != nil {
		return nil, e
	}
	var b [6]byte
	if _, e = rand.Read(b[:]); e != nil {
		return nil, e
	}
	s := &State{Version: 1, ID: hex.EncodeToString(b[:]), ProjectDir: m.Project, Base: m.Base, Spec: spec, Phase: "alignment", Status: "pending", CreatedAt: time.Now().UTC(), Roles: map[string]RoleState{}}
	for _, role := range Roles {
		s.Roles[role] = RoleState{Status: "pending", ContractVersion: spec.Contract.Version}
	}
	if e = WriteJSON(filepath.Join(m.RunDir, fmt.Sprintf("contract-v%d.json", spec.Contract.Version)), spec.Contract); e != nil {
		return nil, e
	}
	if e = WriteJSON(filepath.Join(m.RunDir, "spec.json"), spec); e != nil {
		return nil, e
	}
	return s, s.Save(m.RunDir)
}

// Execute holds a single controller lock for the complete run. Worker goroutines
// send immutable results through a channel; only this goroutine mutates State.
func Execute(ctx context.Context, runDir string, worker Worker, log *Logger, memory *experience.Store) (state *State, err error) {
	if worker == nil {
		return nil, fmt.Errorf("worker is required")
	}
	release, e := acquireLock(runDir)
	if e != nil {
		return nil, fmt.Errorf("run is already controlled: %w", e)
	}
	defer release()
	s, e := LoadState(runDir)
	if e != nil {
		return nil, e
	}
	state = s
	if s.Status == "completed" {
		log.Emit("delivery", "", "completed", s.Spec.Contract.Version, s.Attempt, "Run already completed; artifacts preserved")
		return s, log.Err()
	}
	defer func() {
		if err != nil {
			if ctx.Err() != nil {
				s.Status = "cancelled"
			} else if s.Status != "blocked" {
				s.Status = "failed"
			}
			s.Error = err.Error()
			log.Emit(s.Phase, "", s.Status, s.Spec.Contract.Version, s.Attempt, s.Error)
			if saveErr := s.Save(runDir); saveErr != nil {
				err = errors.Join(err, saveErr)
			}
		}
	}()
	m, e := workspace.Restore(ctx, s.ProjectDir, runDir, s.Base)
	if e != nil {
		return s, e
	}
	runDir = m.RunDir
	if s.Proposal != nil {
		s.Status = "blocked"
		return s, fmt.Errorf("contract change awaits explicit acceptance; use team contract")
	}
	log.Emit("alignment", "", "started", s.Spec.Contract.Version, s.Attempt, "Validate accepted contract, ownership and acceptance checks")
	if e = s.Spec.Validate(); e != nil {
		return s, e
	}
	for _, role := range Roles {
		dir, e := m.Worker(ctx, role)
		if e != nil {
			return s, e
		}
		rs := s.Roles[role]
		rs.Workdir = dir
		if rs.Status != "ready" || rs.ContractVersion != s.Spec.Contract.Version {
			rs.Status = "pending"
			rs.ContractVersion = s.Spec.Contract.Version
		}
		s.Roles[role] = rs
	}
	log.Emit("alignment", "", "completed", s.Spec.Contract.Version, s.Attempt, "Separate contexts/workspaces ready; only contract and evidence are shared")
	// A verified implementation whose extraction was interrupted resumes at the
	// experience stage instead of re-running workers and invalidating evidence.
	if s.Phase == "experience" && allPassed(s.Checks) && s.Patch != "" {
		return finishExperience(ctx, runDir, s, worker, log, memory)
	}
	s.Status = "running"
	s.Error = ""
	for {
		if e = ctx.Err(); e != nil {
			return s, e
		}
		s.Attempt++
		s.Phase = "development"
		if e = s.Save(runDir); e != nil {
			return s, e
		}
		if e = develop(ctx, runDir, s, worker, log, memory); e != nil {
			return s, e
		}
		s.Phase = "integration"
		if e = s.Save(runDir); e != nil {
			return s, e
		}
		log.Emit("integration", "", "started", s.Spec.Contract.Version, s.Attempt, "Collect ownership-checked changes and integrate patches")
		integrated, e := m.Integrate(ctx, s.Attempt, s.Spec.Ownership())
		if e != nil {
			return s, e
		}
		s.IntegrationDir = integrated.Dir
		s.Patch = integrated.Patch
		if e = WriteJSON(filepath.Join(integrated.Dir, ".pigo-team-contract.json"), s.Spec.Contract); e != nil {
			return s, e
		}
		log.Emit("integration", "", "completed", s.Spec.Contract.Version, s.Attempt, "Integration workspace: "+integrated.Dir)
		s.Phase = "verification"
		if e = s.Save(runDir); e != nil {
			return s, e
		}
		checks, checkErr := runChecks(ctx, integrated.Dir, runDir, s.Spec, s.Attempt, log)
		s.Checks = checks
		s.CheckHistory = append(s.CheckHistory, checks...)
		if e = s.Save(runDir); e != nil {
			return s, e
		}
		if checkErr != nil {
			return s, checkErr
		}
		if allPassed(checks) {
			break
		}
		if s.Repairs >= s.Spec.MaxRepairRounds {
			s.Status = "blocked"
			return s, fmt.Errorf("acceptance failed; repair budget %d exhausted, inspect checks and workspaces before resuming", s.Spec.MaxRepairRounds)
		}
		s.Repairs++
		s.Phase = "repair"
		for _, role := range Roles {
			if len(failedFor(checks, role)) > 0 {
				_ = s.SetRole(role, "pending", s.Spec.Contract.Version, "Assigned failing acceptance evidence")
			}
		}
		if e = s.Save(runDir); e != nil {
			return s, e
		}
		log.Emit("repair", "", "scheduled", s.Spec.Contract.Version, s.Attempt, fmt.Sprintf("Repair %d/%d; only affected workers receive failures", s.Repairs, s.Spec.MaxRepairRounds))
	}
	return finishExperience(ctx, runDir, s, worker, log, memory)
}

func allPassed(checks []CheckResult) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if !c.Passed {
			return false
		}
	}
	return true
}
func failedFor(checks []CheckResult, role string) []CheckResult {
	var out []CheckResult
	for _, c := range checks {
		if !c.Passed && (c.Owner == role || c.Owner == "both") {
			out = append(out, c)
		}
	}
	return out
}

func develop(ctx context.Context, dir string, s *State, worker Worker, log *Logger, memory *experience.Store) error {
	type outcome struct {
		role   string
		result WorkResult
		err    error
	}
	ch := make(chan outcome, 2)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	requests := []WorkRequest{}
	for _, role := range Roles {
		rs := s.Roles[role]
		if rs.Status == "ready" && rs.ContractVersion == s.Spec.Contract.Version {
			log.Emit("development", role, "reused", s.Spec.Contract.Version, s.Attempt, "Retain ready implementation for current contract")
			continue
		}
		var memories []experience.Entry
		if memory != nil {
			hits, e := memory.SearchFiltered(s.Spec.Project, s.Spec.Requirement+" "+s.Spec.Role(role).Task, s.Spec.Role(role).Paths, 5, func(entry experience.Entry) bool {
				if applicable(entry, s.Spec.Contract) {
					return true
				}
				log.Emit("recall", role, "skipped", s.Spec.Contract.Version, s.Attempt, "Experience "+entry.ID+" has an incompatible or unverified contract fingerprint; requires review")
				return false
			})
			if e != nil {
				return e
			}
			memories = hits
		}
		log.Emit("recall", role, "completed", s.Spec.Contract.Version, s.Attempt, fmt.Sprintf("%d approved applicable experiences", len(memories)))
		if e := s.SetRole(role, "running", s.Spec.Contract.Version, ""); e != nil {
			return e
		}
		knowledgeRoot := ""
		if memory != nil {
			knowledgeRoot = memory.Root
		}
		requests = append(requests, WorkRequest{RunID: s.ID, Role: role, Phase: "development", Workdir: rs.Workdir, PrivateDir: filepath.Join(dir, "private"), KnowledgeRoot: knowledgeRoot, Spec: s.Spec, Attempt: s.Attempt, Feedback: failedFor(s.Checks, role), Experiences: memories})
	}
	if e := s.Save(dir); e != nil {
		return e
	}
	for _, req := range requests {
		log.Emit("development", req.Role, "started", s.Spec.Contract.Version, s.Attempt, req.Workdir)
		go func(req WorkRequest) {
			result, e := worker.Run(runCtx, req, func(status, message string) {
				log.Emit("development", req.Role, status, req.Spec.Contract.Version, req.Attempt, message)
			})
			ch <- outcome{role: req.Role, result: result, err: e}
		}(req)
	}
	var failures []error
	for range requests {
		o := <-ch
		if o.err != nil {
			_ = s.SetRole(o.role, "failed", s.Spec.Contract.Version, o.err.Error())
			failures = append(failures, fmt.Errorf("%s: %w", o.role, o.err))
			cancel()
		} else {
			r := o.result
			if r.Status != "ready" && r.Status != "blocked" && r.Status != "change_requested" {
				o.err = fmt.Errorf("invalid worker outcome %q", r.Status)
			} else {
				o.err = s.SetRole(o.role, r.Status, r.ContractVersion, r.Summary)
			}
			if o.err != nil {
				failures = append(failures, o.err)
				_ = s.SetRole(o.role, "failed", s.Spec.Contract.Version, o.err.Error())
				cancel()
			}
			if o.err == nil && r.Status == "change_requested" {
				if r.Proposal == nil || r.Proposal.Version != s.Spec.Contract.Version+1 {
					o.err = fmt.Errorf("%s proposed no valid next contract", o.role)
				} else {
					o.err = r.Proposal.Validate()
				}
				if o.err == nil {
					o.err = WriteJSON(filepath.Join(dir, "contract-proposal-"+o.role+".json"), r.Proposal)
				}
				if o.err != nil {
					failures = append(failures, o.err)
				} else {
					if s.Proposal == nil {
						s.Proposal = r.Proposal
					}
					s.Status = "blocked"
					failures = append(failures, fmt.Errorf("%s requested contract revision; explicitly accept the proposal with team contract", o.role))
				}
			}
			if o.err == nil && r.Status == "blocked" {
				s.Status = "blocked"
				failures = append(failures, fmt.Errorf("%s blocked: %s", o.role, r.Summary))
			}
		}
		log.Emit("development", o.role, s.Roles[o.role].Status, s.Spec.Contract.Version, s.Attempt, s.Roles[o.role].Summary)
		if e := s.Save(dir); e != nil {
			failures = append(failures, e)
			cancel()
		}
	}
	return errors.Join(failures...)
}

func finishExperience(ctx context.Context, dir string, s *State, worker Worker, log *Logger, memory *experience.Store) (*State, error) {
	s.Phase = "experience"
	s.Status = "running"
	if e := s.Save(dir); e != nil {
		return s, e
	}
	if !s.ExperienceComplete {
		log.Emit("experience", "", "started", s.Spec.Contract.Version, s.Attempt, "Extract draft experience from actual verification evidence")
		sources := []string{s.Patch, filepath.Join(dir, fmt.Sprintf("contract-v%d.json", s.Spec.Contract.Version))}
		for _, c := range s.CheckHistory {
			sources = append(sources, c.Log)
		}
		var evidence []Evidence
		// The extractor has no filesystem tools: provide actual bounded patch and
		// contract contents, not just paths it cannot read.
		for _, source := range sources[:2] {
			f, e := os.Open(source)
			if e != nil {
				return s, e
			}
			b, e := io.ReadAll(io.LimitReader(f, 48<<10))
			ce := f.Close()
			if e != nil {
				return s, e
			}
			if ce != nil {
				return s, ce
			}
			evidence = append(evidence, Evidence{Source: source, Text: string(b)})
		}
		res, e := worker.Run(ctx, WorkRequest{RunID: s.ID, Role: "experience", Phase: "experience", Workdir: s.IntegrationDir, PrivateDir: filepath.Join(dir, "private"), Spec: s.Spec, Attempt: s.Attempt, Feedback: s.CheckHistory, Sources: sources, Evidence: evidence}, func(status, message string) {
			log.Emit("experience", "", status, s.Spec.Contract.Version, s.Attempt, message)
		})
		if e != nil {
			return s, e
		}
		if res.Status != "ready" || res.ContractVersion != s.Spec.Contract.Version {
			return s, fmt.Errorf("experience worker returned invalid status/version")
		}
		if len(res.Experiences) > 10 {
			return s, fmt.Errorf("experience worker returned too many candidates (limit 10)")
		}
		allowed := map[string]bool{}
		for _, src := range sources {
			allowed[src] = true
		}
		for _, entry := range res.Experiences {
			if memory == nil {
				return s, fmt.Errorf("experience store is required to save candidates")
			}
			if entry.Project != s.Spec.Project || (entry.ContractVersion != 0 && entry.ContractVersion != s.Spec.Contract.Version) {
				return s, fmt.Errorf("experience scope/version does not match run")
			}
			for _, src := range entry.Sources {
				if !allowed[src] {
					return s, fmt.Errorf("experience cited unknown source %q", src)
				}
			}
			for _, module := range entry.Modules {
				if !SafeRelative(module) {
					return s, fmt.Errorf("unsafe experience module")
				}
			}
			entry.ContractVersion = s.Spec.Contract.Version
			entry.ContractHash = s.Spec.Contract.Fingerprint()
			saved, e := memory.Propose(entry)
			if e != nil {
				return s, e
			}
			found := false
			for _, id := range s.ExperienceIDs {
				found = found || id == saved.ID
			}
			if !found {
				s.ExperienceIDs = append(s.ExperienceIDs, saved.ID)
			}
			log.Emit("experience", "", saved.Status, s.Spec.Contract.Version, s.Attempt, "Candidate "+saved.ID+" stored or reused; status="+saved.Status+" (approval is never automatic)")
			if e = s.Save(dir); e != nil {
				return s, e
			}
		}
		s.ExperienceComplete = true
		log.Emit("experience", "", "completed", s.Spec.Contract.Version, s.Attempt, fmt.Sprintf("%d candidate entries; none automatically approved", len(s.ExperienceIDs)))
	}
	s.Phase = "delivery"
	s.Status = "completed"
	s.Error = ""
	if e := log.Err(); e != nil {
		return s, e
	}
	if e := s.Save(dir); e != nil {
		return s, e
	}
	log.Emit("delivery", "", "completed", s.Spec.Contract.Version, s.Attempt, "Verified workspace "+s.IntegrationDir+"; patch "+s.Patch)
	return s, log.Err()
}

// CompactSummary is suitable for adapters which need a handoff without private
// transcripts. It intentionally includes no model credentials or chat history.
func (s *State) CompactSummary() string {
	return strings.Join([]string{s.ID, s.Status, s.Phase, s.Patch}, " ")
}
