package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
)

// A deliberately small, read-only MCP server.  It exposes the two things R3
// can safely share with another AI host: ACL-filtered retrieval and an
// ACL-filtered source read.  Administrative, connector, mailbox, and live
// write tools remain outside MCP until they have their own confirmation and
// authorization story.
//
// It implements the stateless JSON-response subset of Streamable HTTP.  The
// MCP 2025-11-25 transport explicitly permits application/json responses and
// a server may reply 405 to a GET when it does not offer an SSE stream.
const mcpProtocolVersion = "2025-11-25"

const (
	mcpRequestMaxBytes       = 256 << 10
	mcpSourceIDMaxChars      = 1024
	mcpSourceContentMaxBytes = 100000
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func handleMCP(rag *ragSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !mcpOriginAllowed(r) {
			writeMCPResponse(w, http.StatusForbidden, nil, nil, &mcpError{Code: -32001, Message: "origin is not allowed"})
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Allow", http.MethodPost)
			writeMCPResponse(w, http.StatusMethodNotAllowed, nil, nil, &mcpError{Code: -32600, Message: "this MCP endpoint does not provide an SSE stream"})
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeMCPResponse(w, http.StatusMethodNotAllowed, nil, nil, &mcpError{Code: -32600, Message: "method not allowed"})
			return
		}
		if !mcpAuthenticated(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="R3 MCP"`)
			writeMCPResponse(w, http.StatusUnauthorized, nil, nil, &mcpError{Code: -32000, Message: "valid API key required"})
			return
		}
		if contentType := strings.TrimSpace(r.Header.Get("Content-Type")); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
			writeMCPResponse(w, http.StatusUnsupportedMediaType, nil, nil, &mcpError{Code: -32600, Message: "Content-Type must be application/json"})
			return
		}

		var req mcpRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, mcpRequestMaxBytes))
		if err := dec.Decode(&req); err != nil || req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
			writeMCPResponse(w, http.StatusBadRequest, nil, nil, &mcpError{Code: -32600, Message: "invalid JSON-RPC request"})
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF || !validMCPRequestID(req.ID) {
			writeMCPResponse(w, http.StatusBadRequest, nil, nil, &mcpError{Code: -32600, Message: "invalid JSON-RPC request"})
			return
		}
		// Notifications never receive a JSON-RPC response. The standard
		// initialized/cancelled notifications are accepted for stateless
		// clients; unknown notifications are harmlessly ignored too.
		if len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null")) {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		result, rpcErr := executeMCPRequest(r, rag, req)
		if rpcErr != nil {
			writeMCPResponse(w, http.StatusOK, req.ID, nil, rpcErr)
			return
		}
		writeMCPResponse(w, http.StatusOK, req.ID, result, nil)
	}
}

// validMCPRequestID accepts only the JSON-RPC request-id scalar types. It
// keeps response correlation simple and prevents arbitrary nested JSON from
// being reflected verbatim into the response's id field.
func validMCPRequestID(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return false
	}
	switch id.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}

func mcpOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" { // non-browser MCP hosts normally do not send Origin
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func mcpAuthenticated(r *http.Request) bool {
	presented := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if presented == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			presented = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if presented == "" {
		return false
	}
	rec, ok := findAPIKey(settings.get().API.Keys, presented)
	if !ok {
		return false
	}
	touchAPIKeyLastUsed(rec.ID)
	return true
}

func executeMCPRequest(r *http.Request, rag *ragSystem, req mcpRequest) (any, *mcpError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "Rubix Ranked RAG", "version": "r3"},
			"instructions":    "Read-only, ACL-filtered retrieval. Use search_knowledge_base before get_source_content.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var call mcpToolCall
		if err := json.Unmarshal(req.Params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
			return nil, &mcpError{Code: -32602, Message: "tools/call requires name and optional arguments"}
		}
		return callMCPTool(r, rag, call), nil
	default:
		return nil, &mcpError{Code: -32601, Message: "method not found"}
	}
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "search_knowledge_base", "title": "Search R3 knowledge base",
			"description": "Searches R3's imported knowledge base. Results respect the caller's source-kind and document-level ACLs.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Question or search terms."},
				"k":     map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Maximum results; default 5."},
			}, "required": []string{"query"}},
		},
		{
			"name": "get_source_content", "title": "Read an R3 source",
			"description": "Returns an imported source's extracted text when it is accessible to this caller. Source IDs must come from a previous search.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"source_id": map[string]any{"type": "string"},
			}, "required": []string{"source_id"}},
		},
		{
			"name": "get_r3_status", "title": "Get R3 status",
			"description": "Returns non-sensitive R3 knowledge-base status.",
			"inputSchema": map[string]any{"type": "object"},
		},
	}
}

func callMCPTool(r *http.Request, rag *ragSystem, call mcpToolCall) map[string]any {
	toolError := func(err error) map[string]any {
		return mcpToolResult(nil, true, err.Error())
	}
	s := settings.get()
	dept, user := mcpRequestIdentity(r)
	switch call.Name {
	case "search_knowledge_base":
		if err := mcpOnlyArguments(call.Arguments, "query", "k"); err != nil {
			return toolError(err)
		}
		query, _ := call.Arguments["query"].(string)
		query = strings.TrimSpace(query)
		if query == "" {
			return toolError(fmt.Errorf("query is required"))
		}
		if len([]rune(query)) > effectiveMaxPromptChars(s.Upload) {
			return toolError(fmt.Errorf("query too long"))
		}
		k := s.K
		if raw, ok := call.Arguments["k"].(float64); ok {
			if math.Trunc(raw) != raw {
				return toolError(fmt.Errorf("k must be an integer"))
			}
			k = int(raw)
		} else if _, ok := call.Arguments["k"]; ok {
			return toolError(fmt.Errorf("k must be an integer"))
		}
		if k < 1 || k > 20 {
			return toolError(fmt.Errorf("k must be between 1 and 20"))
		}
		hits, err := rag.rankedSearchForIdentity(query, k, s.Ranking, s.activeEmbedModel(), s.SourceAccess, dept, user, nil)
		if err != nil {
			return toolError(fmt.Errorf("search failed: %w", err))
		}
		logAudit(r, "mcp_search", fmt.Sprintf("k=%d %s", k, truncateRunesNote(query, 300)))
		return mcpToolResult(map[string]any{"hits": hits}, false, "")
	case "get_source_content":
		if err := mcpOnlyArguments(call.Arguments, "source_id"); err != nil {
			return toolError(err)
		}
		sourceID, _ := call.Arguments["source_id"].(string)
		sourceID = strings.TrimSpace(sourceID)
		if sourceID == "" {
			return toolError(fmt.Errorf("source_id is required"))
		}
		if len([]rune(sourceID)) > mcpSourceIDMaxChars {
			return toolError(fmt.Errorf("source_id too long"))
		}
		kind, ok := rag.fetchSourceKind(sourceID)
		if !ok || !rag.sourceAccessAllowed(s.SourceAccess, sourceID, kind, dept, user) {
			return toolError(fmt.Errorf("source not found"))
		}
		content, ok := rag.fetchSourceContent(sourceID)
		if !ok {
			return toolError(fmt.Errorf("source not found"))
		}
		logAudit(r, "mcp_get_source", "source_id="+sourceID)
		return mcpToolResult(map[string]any{"source_id": sourceID, "source_kind": kind, "content": truncateRunesNote(content, mcpSourceContentMaxBytes)}, false, "")
	case "get_r3_status":
		if err := mcpOnlyArguments(call.Arguments); err != nil {
			return toolError(err)
		}
		return mcpToolResult(map[string]any{"chunks": rag.docCount(), "storage_backend": s.Storage.Backend}, false, "")
	default:
		return toolError(fmt.Errorf("unknown tool: %s", call.Name))
	}
}

func mcpOnlyArguments(args map[string]any, allowed ...string) error {
	for key := range args {
		ok := false
		for _, name := range allowed {
			if key == name {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unexpected argument %q", key)
		}
	}
	return nil
}

func mcpRequestIdentity(r *http.Request) (deptCode, user string) {
	claims, ok := currentSession(r)
	if !ok {
		return "", ""
	}
	deptCode = resolveDeptCode(claims.IsAdmin, claims.DeptCode)
	user = sessionActor(claims)
	return deptCode, user
}

func mcpToolResult(value any, isError bool, errText string) map[string]any {
	if isError {
		return map[string]any{"content": []map[string]string{{"type": "text", "text": errText}}, "isError": true}
	}
	b, _ := json.Marshal(value)
	return map[string]any{
		"content":           []map[string]string{{"type": "text", "text": string(b)}},
		"structuredContent": value,
	}
}

func writeMCPResponse(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr *mcpError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusAccepted {
		return
	}
	_ = json.NewEncoder(w).Encode(mcpResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}
