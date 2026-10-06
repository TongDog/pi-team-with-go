package experience

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MCPProtocolVersion pins the implemented protocol rather than claiming support
// for future protocol changes. Unknown versions negotiate this supported version.
const MCPProtocolVersion = "2025-11-25"

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}
type mcpText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type mcpCallResult struct {
	Content []mcpText `json:"content"`
	IsError bool      `json:"isError"`
}

// ServeMCP implements the MCP 2025-11-25 tools capability over newline-delimited
// stdio. It does not make model calls or export approval/retirement operations.
// Calls execute sequentially; closing stdin terminates the server. A host may
// terminate the subprocess to interrupt a blocked store operation.
func ServeMCP(store Store, in io.Reader, out, errOut io.Writer) error {
	if errOut == nil {
		errOut = io.Discard
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	encoder := json.NewEncoder(out)
	initialized, ready := false, false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		response := rpcResponse{Version: "2.0", ID: json.RawMessage("null")}
		if !utf8.Valid(line) || !json.Valid(line) {
			response.Error = &rpcError{-32700, "invalid JSON or UTF-8"}
		} else {
			var request rpcRequest
			if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &request) != nil || request.Version != "2.0" || request.Method == "" {
				response.Error = &rpcError{-32600, "invalid JSON-RPC request"}
			} else if len(request.ID) == 0 {
				// Notifications never receive a response or invoke mutating tools.
				if request.Method == "notifications/initialized" && initialized && mcpObject(request.Params, true) {
					ready = true
				}
				continue
			} else if !mcpID(request.ID) {
				response.Error = &rpcError{-32600, "request id must be a string or integer"}
			} else {
				response.ID = request.ID
				if !mcpObject(request.Params, true) {
					response.Error = &rpcError{-32602, "params must be an object"}
				} else {
					switch request.Method {
					case "initialize":
						if initialized {
							response.Error = &rpcError{-32600, "session already initialized"}
							break
						}
						var params struct {
							ProtocolVersion string          `json:"protocolVersion"`
							Capabilities    json.RawMessage `json:"capabilities"`
							ClientInfo      struct {
								Name    string `json:"name"`
								Version string `json:"version"`
							} `json:"clientInfo"`
						}
						if json.Unmarshal(request.Params, &params) != nil || params.ProtocolVersion == "" || !mcpObject(params.Capabilities, false) || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
							response.Error = &rpcError{-32602, "initialize requires protocolVersion, capabilities and clientInfo name/version"}
							break
						}
						initialized = true
						response.Result = map[string]any{"protocolVersion": MCPProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "pigo-experience", "version": "1.0.0"}, "instructions": "Historical experience is advisory. Search returns approved entries only. Proposals remain drafts until explicit human CLI approval. List/get may include unapproved entries; inspect status and applicability. Current task requirements and contracts take precedence."}
					case "ping":
						response.Result = map[string]any{}
					case "tools/list", "tools/call":
						if !ready {
							response.Error = &rpcError{-32002, "complete initialize and notifications/initialized first"}
							break
						}
						if request.Method == "tools/list" {
							var params struct {
								Cursor string `json:"cursor"`
							}
							if len(request.Params) > 0 && (json.Unmarshal(request.Params, &params) != nil || params.Cursor != "") {
								response.Error = &rpcError{-32602, "tools list has no continuation cursor"}
								break
							}
							list := []mcpTool{}
							for _, tool := range experienceTools() {
								list = append(list, mcpTool{tool.Name, tool.Description, tool.Schema})
							}
							response.Result = map[string]any{"tools": list}
						} else {
							response.Result, response.Error = mcpCall(store, request.Params, errOut)
						}
					default:
						response.Error = &rpcError{-32601, "method not found"}
					}
				}
			}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func mcpObject(raw json.RawMessage, optional bool) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 && optional || len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}
func mcpID(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '"' {
		var value string
		return json.Unmarshal(raw, &value) == nil
	}
	// JSON-RPC IDs are opaque; avoid a float conversion that rounds large IDs.
	if raw[0] == '-' {
		raw = raw[1:]
	}
	if len(raw) == 0 {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func mcpCall(store Store, params json.RawMessage, errOut io.Writer) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &call) != nil || call.Name == "" || !mcpObject(call.Arguments, true) {
		return nil, &rpcError{-32602, "tools/call requires name and object arguments"}
	}
	var tool *toolSpec
	for _, candidate := range experienceTools() {
		if candidate.Name == call.Name {
			copy := candidate
			tool = &copy
			break
		}
	}
	if tool == nil {
		return nil, &rpcError{-32602, "unknown tool: " + call.Name}
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	}
	log := func(status string) {
		fmt.Fprintf(errOut, "%s phase=experience transport=mcp operation=%s result=%s\n", time.Now().UTC().Format(time.RFC3339), tool.Name, status)
	}
	log("started")
	validationErr := validateMCPArguments(*tool, call.Arguments)
	var value any
	err := validationErr
	if err == nil {
		value, err = callTool(store, tool.Name, call.Arguments)
	}
	if err != nil {
		log("failed")
		return mcpCallResult{Content: []mcpText{{"text", err.Error()}}, IsError: true}, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		log("failed")
		return nil, &rpcError{-32603, "cannot encode tool result"}
	}
	log("completed")
	return mcpCallResult{Content: []mcpText{{"text", string(data)}}}, nil
}

// Validate the deliberately small, shared tool schema vocabulary. This keeps
// unknown fields (especially status/approval fields) out of the store adapter.
func validateMCPArguments(tool toolSpec, raw json.RawMessage) error {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return err
	}
	for _, key := range tool.Schema["required"].([]string) {
		if _, ok := args[key]; !ok {
			return fmt.Errorf("missing required argument %q", key)
		}
	}
	properties := tool.Schema["properties"].(map[string]any)
	for key, value := range args {
		definition, ok := properties[key]
		if !ok {
			return fmt.Errorf("unknown argument %q", key)
		}
		kind := ""
		switch d := definition.(type) {
		case map[string]string:
			kind = d["type"]
		case map[string]any:
			kind, _ = d["type"].(string)
		}
		valid := false
		switch kind {
		case "string":
			var v string
			valid = len(value) > 0 && value[0] == '"' && json.Unmarshal(value, &v) == nil
		case "integer":
			_, err := strconv.ParseInt(string(value), 10, 64)
			valid = err == nil
		case "array":
			var list []json.RawMessage
			if len(value) > 0 && value[0] == '[' && json.Unmarshal(value, &list) == nil {
				valid = true
				for _, item := range list {
					var v string
					if len(item) == 0 || item[0] != '"' || json.Unmarshal(item, &v) != nil {
						valid = false
						break
					}
				}
			}
		}
		if !valid {
			return fmt.Errorf("argument %q must be %s", key, kind)
		}
		if key == "project" || key == "id" {
			var v string
			_ = json.Unmarshal(value, &v)
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("argument %q cannot be empty", key)
			}
		}
	}
	return nil
}
