// Package experience stores evidence-backed experience candidates and their revisions.
package experience

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	Draft    = "draft"
	Approved = "approved"
	Retired  = "retired"
)

type Entry struct {
	ID              string    `json:"id"`
	Project         string    `json:"project"`
	Problem         string    `json:"problem"`
	RootCause       string    `json:"root_cause,omitempty"`
	Solution        string    `json:"solution"`
	Applicability   string    `json:"applicability"`
	Risks           []string  `json:"risks,omitempty"`
	Modules         []string  `json:"modules,omitempty"`
	Sources         []string  `json:"sources"`
	ContractVersion int       `json:"contract_version,omitempty"`
	ContractHash    string    `json:"contract_hash,omitempty"`
	Status          string    `json:"status"`
	Revision        int       `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Store struct{ Root string }
type snapshot struct {
	Version int                `json:"version"`
	History map[string][]Entry `json:"history"`
}

func (s Store) path() (string, error) {
	if strings.TrimSpace(s.Root) == "" {
		return "", errors.New("experience root is required")
	}
	return filepath.Join(s.Root, "entries.json"), nil
}
func (s Store) read() (snapshot, error) {
	path, err := s.path()
	if err != nil {
		return snapshot{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot{Version: 1, History: map[string][]Entry{}}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	var state snapshot
	if err = json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("read experience store: %w", err)
	}
	if state.Version != 1 || state.History == nil {
		return state, errors.New("unsupported or invalid experience store")
	}
	for id, history := range state.History {
		if len(history) == 0 {
			return state, fmt.Errorf("empty history for %s", id)
		}
		for i, e := range history {
			if e.ID != id || e.Revision != i+1 || (e.Status != Draft && e.Status != Approved && e.Status != Retired) {
				return state, fmt.Errorf("invalid history for %s", id)
			}
		}
	}
	return state, nil
}
func (s Store) mutate(fn func(*snapshot) (Entry, error)) (Entry, error) {
	path, err := s.path()
	if err != nil {
		return Entry{}, err
	}
	if err = os.MkdirAll(s.Root, 0700); err != nil {
		return Entry{}, err
	}
	lock := filepath.Join(s.Root, ".lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = os.Mkdir(lock, 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return Entry{}, err
		}
		if time.Now().After(deadline) {
			return Entry{}, errors.New("experience store locked; if a previous writer crashed, confirm it has stopped before removing .lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer os.Remove(lock)
	state, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	entry, err := fn(&state)
	if err != nil {
		return Entry{}, err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return Entry{}, err
	}
	f, err := os.CreateTemp(s.Root, ".entries-*")
	if err != nil {
		return Entry{}, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return Entry{}, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return Entry{}, err
	}
	if err = f.Close(); err != nil {
		return Entry{}, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return Entry{}, err
	}
	return entry, nil
}
func normalized(values []string) []string {
	var result []string
	seen := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			result = append(result, v)
			seen[v] = true
		}
	}
	sort.Strings(result)
	return result
}
func clean(e Entry) (Entry, error) {
	e.Project = strings.TrimSpace(e.Project)
	e.Problem = strings.TrimSpace(e.Problem)
	e.RootCause = strings.TrimSpace(e.RootCause)
	e.Solution = strings.TrimSpace(e.Solution)
	e.Applicability = strings.TrimSpace(e.Applicability)
	e.Modules = normalized(e.Modules)
	e.Sources = normalized(e.Sources)
	e.Risks = normalized(e.Risks)
	if e.Project == "" || e.Problem == "" || e.Solution == "" || e.Applicability == "" || len(e.Sources) == 0 {
		return e, errors.New("project, problem, solution, applicability and at least one source are required")
	}
	if e.ContractVersion < 0 {
		return e, errors.New("contract_version cannot be negative")
	}
	return e, nil
}
func sameContent(a, b Entry) bool {
	return a.Project == b.Project && a.Problem == b.Problem && a.RootCause == b.RootCause && a.Solution == b.Solution && a.Applicability == b.Applicability && a.ContractVersion == b.ContractVersion && a.ContractHash == b.ContractHash && reflect.DeepEqual(a.Modules, b.Modules) && reflect.DeepEqual(a.Sources, b.Sources) && reflect.DeepEqual(a.Risks, b.Risks)
}
func (s Store) Propose(e Entry) (Entry, error) {
	var err error
	e, err = clean(e)
	if err != nil {
		return Entry{}, err
	}
	return s.mutate(func(state *snapshot) (Entry, error) {
		for _, h := range state.History {
			last := h[len(h)-1]
			if sameContent(last, e) {
				return last, nil
			}
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Entry{}, err
		}
		now := time.Now().UTC()
		e.ID = hex.EncodeToString(id[:])
		e.Status = Draft
		e.Revision = 1
		e.CreatedAt = now
		e.UpdatedAt = now
		state.History[e.ID] = []Entry{e}
		return e, nil
	})
}
func (s Store) List(project string) ([]Entry, error) {
	if strings.TrimSpace(project) == "" {
		return nil, errors.New("project is required")
	}
	state, err := s.read()
	if err != nil {
		return nil, err
	}
	result := []Entry{}
	for _, h := range state.History {
		e := h[len(h)-1]
		if e.Project == project {
			result = append(result, e)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}
func (s Store) Get(id string) (Entry, error) {
	state, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	h := state.History[id]
	if len(h) == 0 {
		return Entry{}, fmt.Errorf("experience %q not found", id)
	}
	return h[len(h)-1], nil
}

// History returns chronological immutable revisions, including approval and retirement.
func (s Store) History(id string) ([]Entry, error) {
	state, err := s.read()
	if err != nil {
		return nil, err
	}
	h := state.History[id]
	if len(h) == 0 {
		return nil, fmt.Errorf("experience %q not found", id)
	}
	return h, nil
}
func (s Store) update(id string, fn func(Entry) (Entry, error)) (Entry, error) {
	return s.mutate(func(state *snapshot) (Entry, error) {
		h := state.History[id]
		if len(h) == 0 {
			return Entry{}, fmt.Errorf("experience %q not found", id)
		}
		old := h[len(h)-1]
		next, err := fn(old)
		if err != nil {
			return Entry{}, err
		}
		if reflect.DeepEqual(old, next) {
			return old, nil
		}
		next.ID = old.ID
		next.CreatedAt = old.CreatedAt
		next.UpdatedAt = time.Now().UTC()
		next.Revision = old.Revision + 1
		state.History[id] = append(h, next)
		return next, nil
	})
}
func (s Store) Approve(id string) (Entry, error) {
	return s.update(id, func(e Entry) (Entry, error) {
		if e.Status == Retired {
			return e, errors.New("revise retired experience before approving")
		}
		e.Status = Approved
		return e, nil
	})
}
func (s Store) Retire(id string) (Entry, error) {
	return s.update(id, func(e Entry) (Entry, error) { e.Status = Retired; return e, nil })
}

// Revise replaces content and returns it to draft; approval never carries over.
func (s Store) Revise(id string, e Entry) (Entry, error) {
	next, err := clean(e)
	if err != nil {
		return Entry{}, err
	}
	return s.update(id, func(old Entry) (Entry, error) {
		if old.Project != next.Project {
			return old, errors.New("revision cannot change project")
		}
		if next.Revision != 0 && next.Revision != old.Revision {
			return old, fmt.Errorf("stale revision: supplied %d, current %d", next.Revision, old.Revision)
		}
		next.Status = Draft
		return next, nil
	})
}
func (s Store) Search(project, query string, modules []string, limit int) ([]Entry, error) {
	return s.SearchFiltered(project, query, modules, limit, nil)
}

// SearchFiltered applies the host's applicability check before limiting hits,
// so newer but incompatible advice cannot crowd out a relevant approved entry.
func (s Store) SearchFiltered(project, query string, modules []string, limit int, accept func(Entry) bool) ([]Entry, error) {
	entries, err := s.List(project)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	type hit struct {
		entry Entry
		score int
	}
	hits := []hit{}
	terms := strings.Fields(strings.ToLower(query))
	modules = normalized(modules)
	for _, e := range entries {
		if e.Status != Approved {
			continue
		}
		if len(modules) > 0 && len(e.Modules) > 0 {
			matched := false
			for _, a := range modules {
				for _, b := range e.Modules {
					if a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/") {
						matched = true
					}
				}
			}
			if !matched {
				continue
			}
		}
		body := strings.ToLower(e.Problem + " " + e.RootCause + " " + e.Solution + " " + e.Applicability + " " + strings.Join(e.Risks, " ") + " " + strings.Join(e.Modules, " "))
		score := 0
		for _, term := range terms {
			if strings.Contains(body, term) {
				score++
			}
		}
		if len(terms) > 0 && score == 0 {
			continue
		}
		if accept != nil && !accept(e) {
			continue
		}
		hits = append(hits, hit{e, score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	result := []Entry{}
	for _, h := range hits {
		if len(result) == limit {
			break
		}
		result = append(result, h.entry)
	}
	return result, nil
}
