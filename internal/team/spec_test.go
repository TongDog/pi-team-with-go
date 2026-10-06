package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSpecForTest() Spec {
	return Spec{Version: 1, Project: "project", Requirement: "pagination feature", Acceptance: []string{"Both roles use the declared API"}, Contract: Contract{Version: 1, Summary: "pagination API", Endpoints: []Endpoint{{Method: "GET", Path: "/items", Response: json.RawMessage(`{"items":[]}`)}}}, Frontend: RoleSpec{Task: "pagination client", Paths: []string{"web"}}, Backend: RoleSpec{Task: "pagination server", Paths: []string{"api"}}, Checks: []Check{{Name: "joint acceptance", Owner: "both", Command: []string{"go", "test", "./..."}}}, MaxRepairRounds: 1}
}
func TestSpecRejectsUnsafeAndIncompleteInputs(t *testing.T) {
	if err := validSpecForTest().Validate(); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Spec)
	}{
		{"parent traversal", func(s *Spec) { s.Frontend.Paths = []string{"../web"} }},
		{"normalized traversal", func(s *Spec) { s.Frontend.Paths = []string{"web/../api"} }},
		{"absolute path", func(s *Spec) { s.Frontend.Paths = []string{"/tmp/web"} }},
		{"Windows volume", func(s *Spec) { s.Frontend.Paths = []string{"C:/web"} }},
		{"backslash traversal", func(s *Spec) { s.Frontend.Paths = []string{`..\web`} }},
		{"git metadata", func(s *Spec) { s.Frontend.Paths = []string{".git/config"} }},
		{"pigo state", func(s *Spec) { s.Frontend.Paths = []string{".pigo/team"} }},
		{"same ownership", func(s *Spec) { s.Backend.Paths = []string{"web"} }},
		{"child ownership", func(s *Spec) { s.Backend.Paths = []string{"web/api"} }},
		{"parent ownership", func(s *Spec) { s.Frontend.Paths = []string{"api/subdir"} }},
		{"missing acceptance", func(s *Spec) { s.Acceptance = nil }},
		{"blank acceptance", func(s *Spec) { s.Acceptance = []string{"  "} }},
		{"missing checks", func(s *Spec) { s.Checks = nil }},
		{"duplicate check", func(s *Spec) { s.Checks = append(s.Checks, s.Checks[0]) }},
		{"missing command", func(s *Spec) { s.Checks[0].Command = nil }},
		{"invalid check owner", func(s *Spec) { s.Checks[0].Owner = "unknown" }},
		{"unsafe check directory", func(s *Spec) { s.Checks[0].Dir = "../outside" }},
		{"missing role task", func(s *Spec) { s.Frontend.Task = "" }},
		{"missing owned paths", func(s *Spec) { s.Backend.Paths = nil }},
		{"invalid timeout", func(s *Spec) { s.WorkerTimeoutSeconds = -1 }},
		{"invalid turn limit", func(s *Spec) { s.MaxTurns = 201 }},
		{"invalid retry budget", func(s *Spec) { s.MaxRepairRounds = 11 }},
		{"invalid HTTP method", func(s *Spec) { s.Contract.Endpoints[0].Method = "UNKNOWN" }},
		{"duplicate endpoint", func(s *Spec) { s.Contract.Endpoints = append(s.Contract.Endpoints, s.Contract.Endpoints[0]) }},
		{"invalid response JSON", func(s *Spec) { s.Contract.Endpoints[0].Response = json.RawMessage(`{bad`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpecForTest()
			tc.change(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("invalid specification accepted")
			}
		})
	}
	// Adjacent name prefixes are independent paths rather than parent/child ownership.
	s := validSpecForTest()
	s.Frontend.Paths = []string{"app"}
	s.Backend.Paths = []string{"application"}
	if err := s.Validate(); err != nil {
		t.Fatalf("nonoverlapping prefixes rejected: %v", err)
	}
}
func TestLoadSpecStrictJSON(t *testing.T) {
	data, err := json.Marshal(validSpecForTest())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", string(data), true},
		{"trailing whitespace", string(data) + " \n\t", true},
		{"unknown top-level field", strings.TrimSuffix(string(data), "}") + `,"typo":true}`, false},
		{"unknown nested field", strings.Replace(string(data), `"method":"GET"`, `"method":"GET","typo":true`, 1), false},
		{"second object", string(data) + ` {"version":1}`, false},
		{"trailing garbage", string(data) + " garbage", false},
		{"trailing null", string(data) + " null", false},
		{"hidden trailing object past size limit", string(data) + strings.Repeat(" ", (2<<20)-len(data)) + ` {"version":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "spec.json")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadSpec(file)
			if tc.valid {
				if err != nil || got.Project != "project" {
					t.Fatalf("valid JSON rejected: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid/trailing JSON accepted")
			}
		})
	}
}
func TestContractFingerprintTracksInterfaceRatherThanVersionLabel(t *testing.T) {
	base := validSpecForTest().Contract
	same := validSpecForTest().Contract
	same.Version = 2
	same.Summary = "Reworded summary"
	if base.Fingerprint() != same.Fingerprint() {
		t.Fatal("metadata-only change altered API identity")
	}
	for _, change := range []func(*Contract){
		func(c *Contract) { c.Endpoints[0].Path = "/other" },
		func(c *Contract) { c.Endpoints[0].Response = json.RawMessage(`{"data":[]}`) },
		func(c *Contract) { c.Endpoints[0].Request = json.RawMessage(`{"page":1}`) },
		func(c *Contract) { c.Endpoints[0].Errors = map[string]string{"400": "invalid page"} },
	} {
		different := validSpecForTest().Contract
		change(&different)
		if different.Version != base.Version || different.Fingerprint() == base.Fingerprint() {
			t.Fatal("different APIs sharing v1 got same fingerprint")
		}
	}
}
