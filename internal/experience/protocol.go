package experience

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}
type callResult struct {
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

func schema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str() map[string]string        { return map[string]string{"type": "string"} }
func stringsSchema() map[string]any { return map[string]any{"type": "array", "items": str()} }
func manifest() any {
	return struct {
		Name    string     `json:"name"`
		Version string     `json:"version"`
		Tools   []toolSpec `json:"tools"`
	}{"experience", "1.0.0", experienceTools()}
}

func experienceTools() []toolSpec {
	return []toolSpec{
		{"experience_propose", "Propose an evidence-backed draft. Requires explicit human approval through the CLI before recall.", schema(map[string]any{"project": str(), "problem": str(), "root_cause": str(), "solution": str(), "applicability": str(), "risks": stringsSchema(), "sources": stringsSchema(), "modules": stringsSchema(), "contract_version": map[string]string{"type": "integer"}, "contract_hash": str()}, "project", "problem", "solution", "applicability", "sources")},
		{"experience_search", "Search only approved experiences in the requested project. Historical advice never overrides the current contract.", schema(map[string]any{"project": str(), "query": str(), "modules": stringsSchema(), "limit": map[string]string{"type": "integer"}}, "project")},
		{"experience_list", "List entries and lifecycle status in one project; drafts and retired entries are not validated advice.", schema(map[string]any{"project": str()}, "project")},
		{"experience_get", "Get one experience by ID, including approval status and source evidence.", schema(map[string]any{"id": str()}, "id")},
	}
}

// Serve implements pigo's line-delimited JSON-RPC plugin protocol. It exposes no
// approval tool: generated candidates require an explicit user CLI operation.
func Serve(store Store, in io.Reader, out, errOut io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		var request rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			if err := encoder.Encode(rpcResponse{Version: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "invalid JSON"}}); err != nil {
				return err
			}
			continue
		}
		response := rpcResponse{Version: "2.0", ID: request.ID}
		if len(response.ID) == 0 {
			response.ID = json.RawMessage("null")
		}
		if request.Version != "2.0" || request.Method == "" {
			response.Error = &rpcError{-32600, "invalid JSON-RPC request"}
		} else if request.Method == "shutdown" {
			if len(request.ID) > 0 {
				response.Result = map[string]bool{"ok": true}
				return encoder.Encode(response)
			}
			return nil
		} else if len(request.ID) == 0 {
			continue
		} else {
			switch request.Method {
			case "initialize":
				response.Result = manifest()
			case "tools/call":
				var p struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(request.Params, &p); err != nil || p.Name == "" {
					response.Error = &rpcError{-32602, "invalid tool parameters"}
					break
				}
				fmt.Fprintf(errOut, "%s phase=experience operation=%s result=started\n", time.Now().UTC().Format(time.RFC3339), p.Name)
				value, err := callTool(store, p.Name, p.Arguments)
				status := "completed"
				if err != nil {
					status = "failed"
					response.Result = callResult{Content: err.Error(), IsError: true}
				} else {
					data, merr := json.Marshal(value)
					if merr != nil {
						return merr
					}
					response.Result = callResult{Content: string(data)}
				}
				fmt.Fprintf(errOut, "%s phase=experience operation=%s result=%s\n", time.Now().UTC().Format(time.RFC3339), p.Name, status)
			default:
				response.Error = &rpcError{-32601, "method not found"}
			}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func callTool(store Store, name string, args json.RawMessage) (any, error) {
	switch name {
	case "experience_propose":
		var e Entry
		if err := json.Unmarshal(args, &e); err != nil {
			return nil, err
		}
		return store.Propose(e)
	case "experience_search":
		var p struct {
			Project string   `json:"project"`
			Query   string   `json:"query"`
			Modules []string `json:"modules"`
			Limit   int      `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		return store.Search(p.Project, p.Query, p.Modules, p.Limit)
	case "experience_list":
		var p struct {
			Project string `json:"project"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		return store.List(p.Project)
	case "experience_get":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		return store.Get(p.ID)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
