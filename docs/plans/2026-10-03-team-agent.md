# Frontend/backend collaboration and experience plugin implementation plan

**Goal:** Extend the existing Go pigo runtime with a local, observable frontend/backend workflow: explicit acceptance criteria and versioned API contracts, independent workers, integration checks and bounded repair, and an evidence-backed reusable experience plugin.

**Architecture:** A Go coordinator owns persistent run state and contract revisions. Workers use the existing runtime with separate contexts and Git workspaces; their shared inputs contain the requirement, accepted contract and targeted verification evidence, never the other worker's transcript. The coordinator integrates owned-path patches and runs configured checks. Experience drafts are proposed from run evidence, confirmed explicitly and queried by project/module. Human-readable stderr and append-only JSONL events cover all stages.

**Tech Stack:** Existing Go runtime/provider/tools, standard library, existing JSON-RPC plugin transport, local JSON/JSONL and Git. No database/server/UI dependency is required.

## Acceptance and evidence

- A CLI command runs or resumes a full team task from a versioned JSON specification. Missing acceptance/checks/ownership and overlapping role paths fail before model calls.
- Each role has an independent context and working directory; no process-global `os.Chdir` is used for concurrent runs.
- The controller validates contract versions, records blocked/change requests and accepts revisions explicitly. Old-version results cannot pass integration.
- Integration applies only each role's declared path changes to a dedicated workspace; the source checkout is never reset or overwritten. Final patch and integration workspace remain available.
- Actual check exit codes gate success. Failure reports trigger bounded targeted repair; cancellation/timeouts and exhausted retries preserve evidence and a resumable status.
- Every phase logs timestamp, run id, phase, role where relevant, contract revision, attempt and result. State writes are atomic; one controller holds the run lock.
- Experience drafts retain source run/check/patch evidence and applicability; only approved active entries are recalled. Revisions preserve history and retired entries are excluded.
- A standalone executable exposes experience operations using the pigo plugin protocol, with portable CLI operations for other host adapters.
- Unit tests exercise state/contract errors, scoped integration, cancellation, failed checks then repair, persistence, experience lifecycle and protocol errors. A deterministic offline fixture demonstrates the real workflow without LLM credentials; the real runtime adapter remains the production default.

## Work sequence

1. **Baseline:** locate/download Go into a temporary directory, build and run existing relevant tests. Record environmental blockers without weakening the module requirement.
2. **Models and logs:** create `internal/team/spec.go`, `state.go`, `log.go` and tests. Validate role ownership, acceptance, contract and commands. Implement atomic persistence and exclusive controller lock. Test rejected stale transitions and concurrent logging (`go test -race ./internal/team`).
3. **Workspaces and checks:** create `internal/team/workspace.go`, `checks.go` and tests using temporary Git repositories. Create isolated detached worktrees, collect scoped binary patches, integrate, run bounded commands and retain logs. Test conflicts, unowned writes, failed checks and cancellation.
4. **Coordinator and runtime adapter:** create `internal/team/coordinator.go`, `worker.go`; reuse `runtime.StartRun`, provider resolution and tools explicitly rooted in the worker directory. Persist private transcripts, bounded turns, structured worker reports and contract revision proposals. Test with deterministic worker doubles, including retry/resume and version changes.
5. **Experience core:** create `internal/experience` with local versioned store, draft/approve/revise/retire/search/export operations and tests. Validate evidence and project scope; deduplicate identical source content. Reuse the runtime's model access for proposal extraction without granting shell/write tools to the summarizer.
6. **CLI and plugin:** create `internal/cli/teamcmd`, `cmd/pigo-experience`; wire `pigo team` early in `cmd/pigo/main.go`. Implement `run`, `status`, `resume`, `contract` and `experience` commands; use separate stdout machine results and stderr phase logs. Test flags/exit codes and JSON-RPC handshake/calls.
7. **Integration and documentation:** create `examples/team` specification and offline workflow test/demo, document actual commands and scope in `docs/team-agent.md`. Validate real runtime serialization with a local mock provider plus the genuine tool loop. Run build/vet/focused race tests, then repository suite; distinguish existing failures from new regressions.

## Completion tracking

- [x] Environment and existing baseline verified
- [x] Specification, contract and persistent lifecycle
- [x] Stage logs in stderr and JSONL
- [x] Independent workers and workspaces
- [x] Integration checks, repair, cancellation and resume
- [x] Experience lifecycle and scoped recall
- [x] Plugin and CLI integration
- [x] End-to-end fixtures, documentation and full verification

Completion evidence is recorded in `docs/team-validation.md`. The portable plugin also supports standard MCP stdio through a separate `mcp` mode; existing pigo `serve` behavior is preserved. This implementation does not include a web service, multi-user tenancy, PostgreSQL/vector retrieval, automatic approval, or measured real-model speed/quality claims.

This task is already authorized for implementation. Keep user files (including `.idea/`) intact. Do not publish, push or merge as part of local delivery.
