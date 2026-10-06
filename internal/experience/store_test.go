package experience

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func candidate() Entry {
	return Entry{Project: "repo", Problem: "Pagination mismatch", Solution: "Convert page indices in adapter", Applicability: "List endpoints using v1 pagination", Modules: []string{"web/users"}, Sources: []string{"run:123/check:pagination"}, ContractVersion: 1}
}
func TestLifecycleAndScope(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Propose(candidate())
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != Draft || e.Revision != 1 {
		t.Fatalf("bad draft: %+v", e)
	}
	hits, err := s.Search("repo", "pagination", nil, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("draft recalled: %v %v", hits, err)
	}
	same, err := s.Propose(candidate())
	if err != nil || same.ID != e.ID {
		t.Fatalf("dedup: %+v %v", same, err)
	}
	e, err = s.Approve(e.ID)
	if err != nil || e.Revision != 2 {
		t.Fatalf("approve: %+v %v", e, err)
	}
	for _, tc := range []struct {
		project, query string
		modules        []string
		want           int
	}{{"repo", "pagination", nil, 1}, {"other", "pagination", nil, 0}, {"repo", "unrelated", nil, 0}, {"repo", "pagination", []string{"backend"}, 0}, {"repo", "pagination", []string{"web"}, 1}, {"repo", "pagination", []string{"web/users/list.ts"}, 1}} {
		hits, err = s.Search(tc.project, tc.query, tc.modules, 10)
		if err != nil || len(hits) != tc.want {
			t.Fatalf("search %+v: %+v %v", tc, hits, err)
		}
	}
	updated := candidate()
	updated.Solution = "Use new contract's cursor"
	revised, err := s.Revise(e.ID, updated)
	if err != nil || revised.Status != Draft || revised.Revision != 3 {
		t.Fatalf("revise: %+v %v", revised, err)
	}
	hits, _ = s.Search("repo", "pagination", nil, 10)
	if len(hits) != 0 {
		t.Fatal("revised content retained old approval")
	}
	if _, err = s.Approve(e.ID); err != nil {
		t.Fatal(err)
	}
	retired, err := s.Retire(e.ID)
	if err != nil || retired.Status != Retired {
		t.Fatalf("retire: %+v %v", retired, err)
	}
	if _, err = s.Approve(e.ID); err == nil {
		t.Fatal("retired entry approved without revision")
	}
	hits, _ = s.Search("repo", "pagination", nil, 10)
	if len(hits) != 0 {
		t.Fatal("retired entry recalled")
	}
	history, err := s.History(e.ID)
	if err != nil || len(history) != 5 {
		t.Fatalf("history: %+v %v", history, err)
	}
	if history[0].Solution != candidate().Solution || history[2].Solution != updated.Solution {
		t.Fatal("history mutated")
	}
	reloaded := Store{Root: s.Root}
	if got, err := reloaded.Get(e.ID); err != nil || got.Revision != 5 {
		t.Fatalf("reload: %+v %v", got, err)
	}
}
func TestConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	const count = 24
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := candidate()
			e.Problem = fmt.Sprintf("problem %d", i)
			_, err := (Store{Root: root}).Propose(e)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := (Store{Root: root}).List("repo")
	if err != nil || len(entries) != count {
		t.Fatalf("lost writes: %d %v", len(entries), err)
	}
}
func TestConcurrentDedup(t *testing.T) {
	s := Store{Root: t.TempDir()}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := s.Propose(candidate())
			if err != nil {
				t.Error(err)
			}
			ids <- e.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && id != next {
			t.Fatal("duplicate proposal IDs")
		}
		id = next
	}
}
func TestValidationAndCorruption(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e := candidate()
	e.Sources = nil
	if _, err := s.Propose(e); err == nil {
		t.Fatal("accepted no evidence")
	}
	if _, err := s.List(""); err == nil {
		t.Fatal("accepted empty scope")
	}
	if _, err := (Store{}).Propose(candidate()); err == nil {
		t.Fatal("accepted empty root")
	}
	if err := os.WriteFile(filepath.Join(s.Root, "entries.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(candidate()); err == nil {
		t.Fatal("overwrote corrupted store")
	}
	bytes, _ := os.ReadFile(filepath.Join(s.Root, "entries.json"))
	if string(bytes) != "broken" {
		t.Fatal("corrupted data overwritten")
	}
}

func TestDedupWithoutModules(t *testing.T) {
	s := Store{Root: t.TempDir()}
	candidate := candidate()
	candidate.Modules = nil
	first, err := s.Propose(candidate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Propose(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatal("optional omitted modules broke persisted deduplication")
	}
}

func TestRejectStaleRevision(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Propose(candidate())
	if err != nil {
		t.Fatal(err)
	}
	stale := e
	if _, err = s.Approve(e.ID); err != nil {
		t.Fatal(err)
	}
	stale.Solution = "must not overwrite newer approval"
	if _, err = s.Revise(e.ID, stale); err == nil {
		t.Fatal("accepted stale revision")
	}
	current, err := s.Get(e.ID)
	if err != nil || current.Status != Approved || current.Revision != 2 {
		t.Fatalf("stale revision modified store: %+v %v", current, err)
	}
}
