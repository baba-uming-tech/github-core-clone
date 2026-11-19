package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const (
	mcpJSONType    = "application/json"
	mcpSSEType     = "text/event-stream"
	mcpVersionNew  = "2026-07-28"
	mcpVersionOld  = "2025-11-25"
	mcpVersion2025 = "2025-06-18"
)

var mcpServerInfo = struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}{Name: "github-core-clone-01", Version: "1.0.0"}

type mcpCapabilities struct {
	Tools struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools"`
}

type mcpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`

	required  []string
	propTypes map[string]string
}

var (
	mcpToolDefs     []mcpToolDef
	mcpToolsResult  []byte
	mcpDiscoverJSON []byte
)

func init() {
	mcpCapabilitiesJSON := mcpCapabilities{}
	mcpCapabilitiesJSON.Tools.ListChanged = false

	mcpToolDefs = []mcpToolDef{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the authenticated login.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"}},"required":["name"]}`),
			required:    []string{"name"},
			propTypes:   map[string]string{"name": "string", "description": "string", "private": "boolean"},
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}`),
			required:    []string{"owner", "repo"},
			propTypes:   map[string]string{"owner": "string", "repo": "string"},
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title"]}`),
			required:    []string{"owner", "repo", "title"},
			propTypes:   map[string]string{"owner": "string", "repo": "string", "title": "string", "body": "string"},
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository, optionally filtered by state.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string"}},"required":["owner","repo"]}`),
			required:    []string{"owner", "repo"},
			propTypes:   map[string]string{"owner": "string", "repo": "string", "state": "string"},
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request in a repository.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title","head","base"]}`),
			required:    []string{"owner", "repo", "title", "head", "base"},
			propTypes:   map[string]string{"owner": "string", "repo": "string", "title": "string", "head": "string", "base": "string", "body": "string"},
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request into its base branch.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pull_number":{"type":"integer"}},"required":["owner","repo","pull_number"]}`),
			required:    []string{"owner", "repo", "pull_number"},
			propTypes:   map[string]string{"owner": "string", "repo": "string", "pull_number": "integer"},
		},
	}

	mcpToolsResult = encodeJSON(struct {
		Tools []mcpToolDef `json:"tools"`
	}{Tools: mcpToolDefs})

	mcpDiscoverJSON = encodeJSON(struct {
		ProtocolVersion   string                 `json:"protocolVersion"`
		SupportedVersions []string               `json:"supportedVersions"`
		ServerInfo        interface{}            `json:"serverInfo"`
		Capabilities      mcpCapabilities        `json:"capabilities"`
		Instructions      string                 `json:"instructions"`
		Meta              map[string]interface{} `json:"_meta"`
	}{
		ProtocolVersion:   mcpVersionNew,
		SupportedVersions: []string{mcpVersionNew, mcpVersionOld, mcpVersion2025},
		ServerInfo:        mcpServerInfo,
		Capabilities:      mcpCapabilitiesJSON,
		Instructions:      "A GitHub-compatible forge.",
		Meta: map[string]interface{}{
			"io.modelcontextprotocol/serverInfo": mcpServerInfo,
		},
	})
}

type mcpInitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      interface{}     `json:"serverInfo"`
}

type mcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpTextContent `json:"content"`
	StructuredContent json.RawMessage  `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
}

func mcpToolSuccess(structured []byte) []byte {
	return encodeJSON(mcpToolResult{
		Content:           []mcpTextContent{{Type: "text", Text: string(structured)}},
		StructuredContent: json.RawMessage(structured),
		IsError:           false,
	})
}

func mcpToolError(message string) []byte {
	return encodeJSON(mcpToolResult{
		Content: []mcpTextContent{{Type: "text", Text: message}},
		IsError: true,
	})
}

func mcpRPCResult(id json.RawMessage, result []byte) []byte {
	out := make([]byte, 0, len(id)+len(result)+48)
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	out = append(out, id...)
	out = append(out, `,"result":`...)
	out = append(out, result...)
	out = append(out, '}')
	return out
}

func mcpRPCError(id json.RawMessage, code int, message string) []byte {
	errJSON := encodeJSON(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
	out := make([]byte, 0, len(id)+len(errJSON)+48)
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	out = append(out, id...)
	out = append(out, `,"error":`...)
	out = append(out, errJSON...)
	out = append(out, '}')
	return out
}

func mcpNullID() json.RawMessage {
	return json.RawMessage("null")
}

func mcpAsObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	obj, _ := value.(map[string]any)
	return obj
}

func mcpString(params map[string]any, key string) string {
	if value, ok := params[key].(string); ok {
		return value
	}
	return ""
}

func mcpProtocolVersion(params map[string]any) string {
	if version := mcpString(params, "protocolVersion"); version != "" {
		return version
	}
	if meta, ok := params["_meta"].(map[string]any); ok {
		if version := mcpString(meta, "protocolVersion"); version != "" {
			return version
		}
	}
	return ""
}

func mcpNormalizeToolName(name string) string {
	lowered := strings.ToLower(name)
	if i := strings.LastIndex(lowered, "__"); i >= 0 {
		lowered = lowered[i+2:]
	}
	return lowered
}

func mcpFindTool(name string) *mcpToolDef {
	for i := range mcpToolDefs {
		if mcpToolDefs[i].Name == name {
			return &mcpToolDefs[i]
		}
	}
	return nil
}

func mcpValidateArgs(def *mcpToolDef, args map[string]any) string {
	for _, key := range def.required {
		value, exists := args[key]
		if !exists || value == nil {
			return "Missing required argument: " + key
		}
		if text, ok := value.(string); ok && text == "" {
			return "Missing required argument: " + key
		}
	}
	for key, value := range args {
		propType, known := def.propTypes[key]
		if !known || value == nil {
			continue
		}
		switch propType {
		case "integer":
			number, ok := value.(float64)
			if !ok || number != math.Trunc(number) {
				return "Invalid argument: " + key
			}
		case "string":
			if _, ok := value.(string); !ok {
				return "Invalid argument: " + key
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return "Invalid argument: " + key
			}
		}
	}
	return ""
}

type mcpRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newMCPRecorder() *mcpRecorder {
	return &mcpRecorder{header: make(http.Header)}
}

func (rec *mcpRecorder) Header() http.Header {
	return rec.header
}

func (rec *mcpRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
}

func (rec *mcpRecorder) Write(p []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.body.Write(p)
}

func mcpSuccessStatus(status int) bool {
	return status >= 200 && status < 300
}

func mcpRESTError(body []byte, fallback string) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		return parsed.Message
	}
	return fallback
}

func mcpIntArg(args map[string]any, key string) int {
	if number, ok := args[key].(float64); ok {
		return int(number)
	}
	return 0
}

func (a *App) mcpCallTool(r *http.Request, def *mcpToolDef, args map[string]any) []byte {
	if invalid := mcpValidateArgs(def, args); invalid != "" {
		return mcpToolError(invalid)
	}

	owner := mcpString(args, "owner")
	repoName := mcpString(args, "repo")
	store := a.Store

	switch def.Name {
	case "create_repo":
		body := map[string]any{"name": args["name"]}
		if value, exists := args["description"]; exists && value != nil {
			body["description"] = value
		}
		if value, exists := args["private"]; exists && value != nil {
			body["private"] = value
		}
		rec := newMCPRecorder()
		a.createUserRepo(rec, body)
		if mcpSuccessStatus(rec.status) {
			return mcpToolSuccess(rec.body.Bytes())
		}
		return mcpToolError(mcpRESTError(rec.body.Bytes(), "Repository creation failed"))

	case "get_repo":
		rec := newMCPRecorder()
		segments := []string{"api", "v3", "repos", owner, repoName}
		a.RouteAPIV3(rec, r, http.MethodGet, "/api/v3/repos/"+owner+"/"+repoName, segments, "", nil)
		if mcpSuccessStatus(rec.status) {
			return mcpToolSuccess(rec.body.Bytes())
		}
		return mcpToolError("Not Found: " + owner + "/" + repoName)

	case "create_issue":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Not Found: " + owner + "/" + repoName)
		}
		body := map[string]any{"title": args["title"]}
		if value, exists := args["body"]; exists && value != nil {
			body["body"] = value
		}
		rec := newMCPRecorder()
		a.routeIssuesPulls(rec, r, http.MethodPost, []string{"issues"}, "", body, repo)
		if mcpSuccessStatus(rec.status) {
			return mcpToolSuccess(rec.body.Bytes())
		}
		return mcpToolError(mcpRESTError(rec.body.Bytes(), "Validation Failed"))

	case "list_issues":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Not Found: " + owner + "/" + repoName)
		}
		state := mcpString(args, "state")
		if state == "" {
			state = "all"
		}
		items := make([]json.RawMessage, 0, len(repo.Issues))
		for i := range repo.Issues {
			if state == "all" || repo.Issues[i].State == state {
				items = append(items, IssueJSON(repo, store, &repo.Issues[i]))
			}
		}
		structured := encodeJSON(struct {
			Issues []json.RawMessage `json:"issues"`
		}{Issues: items})
		return mcpToolSuccess(structured)

	case "create_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Not Found: " + owner + "/" + repoName)
		}
		body := map[string]any{
			"title": args["title"],
			"head":  args["head"],
			"base":  args["base"],
		}
		if value, exists := args["body"]; exists && value != nil {
			body["body"] = value
		}
		rec := newMCPRecorder()
		a.routeIssuesPulls(rec, r, http.MethodPost, []string{"pulls"}, "", body, repo)
		if mcpSuccessStatus(rec.status) {
			return mcpToolSuccess(rec.body.Bytes())
		}
		return mcpToolError(mcpRESTError(rec.body.Bytes(), "Validation Failed"))

	case "merge_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Not Found: " + owner + "/" + repoName)
		}
		number := mcpIntArg(args, "pull_number")
		pull := repo.FindPull(number)
		if pull == nil {
			return mcpToolError("Pull request not found: " + owner + "/" + repoName + "#" + strconv.Itoa(number))
		}
		result := MergePull(store, repo, pull, "merge")
		if !result.OK {
			if result.Conflict {
				return mcpToolError("Pull Request is not mergeable")
			}
			return mcpToolError("Validation Failed")
		}
		structured := encodeJSON(struct {
			Merged  bool   `json:"merged"`
			Number  int    `json:"number"`
			SHA     string `json:"sha"`
			Message string `json:"message"`
		}{Merged: true, Number: pull.Number, SHA: result.SHA, Message: "Pull Request successfully merged"})
		return mcpToolSuccess(structured)
	}

	return mcpToolError("Unknown tool: " + def.Name)
}

func (a *App) mcpDispatch(r *http.Request, request map[string]json.RawMessage) (int, []byte) {
	idRaw, hasID := request["id"]
	if !hasID || bytes.Equal(bytes.TrimSpace(idRaw), []byte("null")) {
		return http.StatusAccepted, nil
	}
	id := json.RawMessage(idRaw)

	var method string
	if raw, exists := request["method"]; exists {
		_ = json.Unmarshal(raw, &method)
	}

	params := mcpAsObject(request["params"])
	if params == nil {
		params = map[string]any{}
	}

	switch method {
	case "server/discover":
		return http.StatusOK, mcpRPCResult(id, mcpDiscoverJSON)
	case "initialize":
		version := mcpProtocolVersion(params)
		if version == "" {
			version = mcpVersionOld
		}
		result := encodeJSON(mcpInitializeResult{
			ProtocolVersion: version,
			Capabilities:    mcpCapabilities{},
			ServerInfo:      mcpServerInfo,
		})
		return http.StatusOK, mcpRPCResult(id, result)
	case "tools/list":
		return http.StatusOK, mcpRPCResult(id, mcpToolsResult)
	case "tools/call":
		rawName := mcpString(params, "name")
		def := mcpFindTool(mcpNormalizeToolName(rawName))
		if def == nil {
			return http.StatusOK, mcpRPCError(id, -32602, "Unknown tool: "+rawName)
		}
		arguments, _ := params["arguments"].(map[string]any)
		if arguments == nil {
			arguments = map[string]any{}
		}
		return http.StatusOK, mcpRPCResult(id, a.mcpCallTool(r, def, arguments))
	default:
		return http.StatusOK, mcpRPCError(id, -32601, "Method not found: "+method)
	}
}

func (a *App) sendMCP(w http.ResponseWriter, r *http.Request, status int, responses [][]byte, batch bool) {
	w.Header().Set("X-GitHub-Request-Id", a.nextRequestID())
	if sessionID := r.Header.Get("Mcp-Session-Id"); sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}

	if len(responses) == 0 {
		w.WriteHeader(status)
		return
	}

	hasBody := false
	for _, response := range responses {
		if response != nil {
			hasBody = true
			break
		}
	}
	if !hasBody {
		w.WriteHeader(status)
		return
	}

	if status < 400 && strings.Contains(strings.ToLower(r.Header.Get("Accept")), mcpSSEType) {
		w.Header().Set("Content-Type", mcpSSEType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		for _, response := range responses {
			if response == nil {
				continue
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(response)
			_, _ = w.Write([]byte("\n\n"))
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", mcpJSONType)
	w.WriteHeader(status)
	if !batch && len(responses) == 1 {
		_, _ = w.Write(responses[0])
		return
	}
	_, _ = w.Write([]byte("["))
	for i, response := range responses {
		if i > 0 {
			_, _ = w.Write([]byte(","))
		}
		if response != nil {
			_, _ = w.Write(response)
		} else {
			_, _ = w.Write([]byte("null"))
		}
	}
	_, _ = w.Write([]byte("]"))
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request, method string, body []byte) {
	if r == nil {
		r = &http.Request{Header: make(http.Header), Host: "localhost"}
	}
	method = strings.ToUpper(method)

	if method == http.MethodDelete {
		a.sendMCP(w, r, http.StatusNoContent, nil, false)
		return
	}
	if method != http.MethodPost {
		a.sendMCP(w, r, http.StatusMethodNotAllowed, [][]byte{[]byte(`{"message":"Method Not Allowed"}`)}, false)
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		a.sendMCP(w, r, http.StatusBadRequest, [][]byte{mcpRPCError(mcpNullID(), -32700, "Parse error")}, false)
		return
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			a.sendMCP(w, r, http.StatusBadRequest, [][]byte{mcpRPCError(mcpNullID(), -32700, "Parse error")}, false)
			return
		}
		responses := make([][]byte, 0, len(items))
		for _, item := range items {
			var request map[string]json.RawMessage
			if err := json.Unmarshal(item, &request); err != nil || request == nil {
				responses = append(responses, mcpRPCError(mcpNullID(), -32600, "Invalid Request"))
				continue
			}
			_, response := a.mcpDispatch(r, request)
			if response != nil {
				responses = append(responses, response)
			}
		}
		a.sendMCP(w, r, http.StatusOK, responses, true)
		return
	}

	if trimmed[0] != '{' {
		a.sendMCP(w, r, http.StatusBadRequest, [][]byte{mcpRPCError(mcpNullID(), -32600, "Invalid Request")}, false)
		return
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &request); err != nil || request == nil {
		a.sendMCP(w, r, http.StatusBadRequest, [][]byte{mcpRPCError(mcpNullID(), -32600, "Invalid Request")}, false)
		return
	}
	status, response := a.mcpDispatch(r, request)
	if response == nil {
		a.sendMCP(w, r, status, nil, false)
		return
	}
	a.sendMCP(w, r, status, [][]byte{response}, false)
}
