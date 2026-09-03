package main

// ─────────────────────────────────────────────────────────────────────────────
// Connector System
//
// Provides a minimal but extensible connector framework for tinyRAG.
// Supported connector types: http, rpc (JSON-RPC 2.0), sql (read-only,
// parameterised).
// Each connector exposes one or more Capabilities (tool / resource / ingest).
// The system can generate an LLM-compatible function-calling array and an
// MCP-compatible tool list from the registered connectors.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Enumerations
// ─────────────────────────────────────────────────────────────────────────────

// ConnectorType identifies the transport layer of a Connector.
type ConnectorType string

const (
	ConnectorTypeHTTP ConnectorType = "http"
	ConnectorTypeRPC  ConnectorType = "rpc"
	ConnectorTypeSQL  ConnectorType = "sql"
)

// CapabilityType classifies how a Capability is intended to be used.
type CapabilityType string

const (
	CapabilityTypeTool     CapabilityType = "tool"
	CapabilityTypeResource CapabilityType = "resource"
	CapabilityTypeIngest   CapabilityType = "ingest"
)

// ─────────────────────────────────────────────────────────────────────────────
// JSON Schema helpers
// ─────────────────────────────────────────────────────────────────────────────

// JSONSchemaProperty describes a single property in an input schema.
type JSONSchemaProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// JSONSchema is a simplified JSON Schema object (object type only).
type JSONSchema struct {
	Type       string                        `json:"type"`
	Properties map[string]JSONSchemaProperty `json:"properties,omitempty"`
	Required   []string                      `json:"required,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Output Mapping
// ─────────────────────────────────────────────────────────────────────────────

// OutputMapping maps a field from the raw connector response to a named result
// key. For HTTP connectors, Field uses dot-notation (e.g. "items.0.name").
// For SQL connectors, Field matches a column name from the tabular output.
type OutputMapping struct {
	Field string `json:"field"` // source field (dot-notation for JSON, column name for SQL)
	As    string `json:"as"`    // target key in the result map
}

// ─────────────────────────────────────────────────────────────────────────────
// Capability
// ─────────────────────────────────────────────────────────────────────────────

// Capability describes a single action a Connector can perform.
type Capability struct {
	// Semantic name — used as the tool function name.
	Name string `json:"name"`
	// Human-readable description for the LLM (when to use this capability).
	Description string `json:"description"`
	// Type classifies how the capability should be used.
	Type CapabilityType `json:"type"`
	// InputSchema describes and validates the parameters accepted by this capability.
	InputSchema JSONSchema `json:"input_schema"`
	// OutputMap defines how to extract fields from the raw response.
	OutputMap []OutputMapping `json:"output_map,omitempty"`

	// ── HTTP-specific ──────────────────────────────────────────────────────
	// Method is the HTTP verb (GET, POST, …). Defaults to GET.
	Method string `json:"method,omitempty"`
	// PathTemplate is appended to the connector BaseURL.
	// Named parameters use {param_name} syntax and are URL-escaped.
	PathTemplate string `json:"path_template,omitempty"`
	// BodyTemplate is a JSON string template for the request body.
	// Named parameters use {param_name} syntax and are JSON-escaped.
	BodyTemplate string `json:"body_template,omitempty"`

	// ── JSON-RPC-specific ───────────────────────────────────────────────────
	// RPCMethod is the JSON-RPC 2.0 method name. The validated input object is
	// passed as `params` without string templating.
	RPCMethod string `json:"rpc_method,omitempty"`
	// ReadOnly is an explicit operator assertion used only for autonomous
	// execution policy. JSON-RPC is POST-based, so it is never inferred from
	// the transport method.
	ReadOnly bool `json:"read_only,omitempty"`

	// ── SQL-specific ───────────────────────────────────────────────────────
	// Query is a SELECT-only SQL template. Named parameters use {param_name}
	// syntax. String values are single-quote escaped; numerics are unquoted.
	Query string `json:"query,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Connector
// ─────────────────────────────────────────────────────────────────────────────

// Connector is the top-level configuration object for an external data source
// or service. It holds connection details and a set of Capabilities.
//
// Secret references: any string value that begins with "secret:" is resolved
// at runtime by reading the named environment variable.
// Example: "secret:DB_PASSWORD" → os.Getenv("DB_PASSWORD").
type Connector struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Type        ConnectorType `json:"type"`
	Description string        `json:"description"`
	Enabled     bool          `json:"enabled"`

	// ── HTTP connector fields ───────────────────────────────────────────────
	// BaseURL is the root URL (may start with "secret:").
	BaseURL string `json:"base_url,omitempty"`
	// Headers are additional HTTP headers. Values may start with "secret:".
	Headers map[string]string `json:"headers,omitempty"`

	// ── SQL connector fields ────────────────────────────────────────────────
	// Driver selects the SQL backend: "sqlite", "mariadb", or "mssql".
	Driver string `json:"driver,omitempty"`
	// Config holds driver-specific connection parameters.
	// Supported keys mirror the existing SQL module config:
	//   sqlite: sqlite_bin, sqlite_path
	//   mariadb: mariadb_bin, mariadb_host, mariadb_port, mariadb_user, mariadb_pass, mariadb_db
	//   mssql:   mssql_bin, mssql_host, mssql_port, mssql_user, mssql_pass, mssql_db
	// Any value may start with "secret:" to pull from an env var.
	Config map[string]string `json:"config,omitempty"`

	// ── Resource limits ─────────────────────────────────────────────────────
	// TimeoutSec is the maximum execution time in seconds (default: 10).
	TimeoutSec int `json:"timeout_sec"`
	// MaxBodyBytes limits the HTTP response body size in bytes (default: 1 MiB).
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
	// MaxRows limits SQL result rows (informational; enforced by appending LIMIT).
	MaxRows int `json:"max_rows,omitempty"`

	// Capabilities lists the actions this connector exposes.
	Capabilities []Capability `json:"capabilities"`
}

// timeout returns the connector's timeout duration, applying a floor of 1 s
// and a ceiling of 120 s.
func (c *Connector) timeout() time.Duration {
	t := c.TimeoutSec
	if t <= 0 {
		t = 10
	}
	if t > 120 {
		t = 120
	}
	return time.Duration(t) * time.Second
}

// maxBody returns the maximum HTTP response body size with a sane default.
func (c *Connector) maxBody() int64 {
	if c.MaxBodyBytes <= 0 {
		return 1 << 20 // 1 MiB
	}
	return c.MaxBodyBytes
}

// ─────────────────────────────────────────────────────────────────────────────
// Secret resolution
// ─────────────────────────────────────────────────────────────────────────────

// resolveSecret resolves a secret reference of the form "secret:ENV_VAR_NAME"
// to the corresponding environment variable value. If the value does not start
// with "secret:", it is returned unchanged.
func resolveSecret(val string) string {
	if !strings.HasPrefix(val, "secret:") {
		return val
	}
	envKey := strings.TrimPrefix(val, "secret:")
	return os.Getenv(envKey)
}

// resolveConfigSecret resolves all values in a config map that start with "secret:".
func resolveConfigSecret(cfg map[string]string) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = resolveSecret(v)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// connectorStore — persistence
// ─────────────────────────────────────────────────────────────────────────────

// connectorStore manages a set of Connectors, persisted as JSON.
type connectorStore struct {
	mu   sync.Mutex
	path string
	data []Connector
}

// newConnectorStore creates (or loads) a connectorStore backed by path.
func newConnectorStore(path string) (*connectorStore, error) {
	s := &connectorStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the persisted connectors file. Missing file is not an error.
func (s *connectorStore) load() error {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		s.data = []Connector{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("connectorStore: open %s: %w", s.path, err)
	}
	defer f.Close()
	var list []Connector
	if err := json.NewDecoder(f).Decode(&list); err != nil {
		return fmt.Errorf("connectorStore: decode %s: %w", s.path, err)
	}
	s.data = list
	return nil
}

// saveLocked writes the current state to disk. Caller must hold s.mu.
func (s *connectorStore) saveLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("connectorStore: create tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(s.data); encErr != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("connectorStore: encode: %w", encErr)
	}
	f.Close()
	return os.Rename(tmp, s.path)
}

// list returns a copy of all connectors.
func (s *connectorStore) list() []Connector {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Connector, len(s.data))
	copy(out, s.data)
	return out
}

// get returns a Connector by ID.
func (s *connectorStore) get(id string) (Connector, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.data {
		if c.ID == id {
			return c, true
		}
	}
	return Connector{}, false
}

// upsert creates or updates a Connector.
func (s *connectorStore) upsert(c Connector) (Connector, error) {
	if strings.TrimSpace(c.ID) == "" {
		c.ID = fmt.Sprintf("conn-%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(c.Name) == "" {
		return Connector{}, fmt.Errorf("connector name required")
	}
	if c.Type != ConnectorTypeHTTP && c.Type != ConnectorTypeRPC && c.Type != ConnectorTypeSQL {
		return Connector{}, fmt.Errorf("connector type must be %q, %q, or %q", ConnectorTypeHTTP, ConnectorTypeRPC, ConnectorTypeSQL)
	}
	if c.Type == ConnectorTypeRPC {
		if strings.TrimSpace(c.BaseURL) == "" {
			return Connector{}, fmt.Errorf("rpc connector base_url required")
		}
		for _, cap := range c.Capabilities {
			if strings.TrimSpace(cap.RPCMethod) == "" {
				return Connector{}, fmt.Errorf("rpc capability %q requires rpc_method", cap.Name)
			}
		}
	}
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.data {
		if existing.ID == c.ID {
			s.data[i] = c
			return c, s.saveLocked()
		}
	}
	s.data = append(s.data, c)
	return c, s.saveLocked()
}

// remove deletes a Connector by ID.
func (s *connectorStore) remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.data {
		if c.ID == id {
			s.data = append(s.data[:i], s.data[i+1:]...)
			return true, s.saveLocked()
		}
	}
	return false, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool registry
// ─────────────────────────────────────────────────────────────────────────────

// connectorRegistryEntry links a Capability back to its parent Connector.
type connectorRegistryEntry struct {
	Connector  Connector
	Capability Capability
}

// registry returns a map from capability name to its registry entry for all
// enabled connectors and their capabilities.
func (s *connectorStore) registry() map[string]connectorRegistryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]connectorRegistryEntry)
	for _, c := range s.data {
		if !c.Enabled {
			continue
		}
		for _, cap := range c.Capabilities {
			out[cap.Name] = connectorRegistryEntry{Connector: c, Capability: cap}
		}
	}
	return out
}

// registryEntry resolves a capability name case-insensitively. XML tool calls
// are canonicalized before execution, while persisted connector definitions
// retain the operator's original spelling.
func (s *connectorStore) registryEntry(name string) (connectorRegistryEntry, bool) {
	needle := strings.TrimSpace(name)
	for registeredName, entry := range s.registry() {
		if strings.EqualFold(registeredName, needle) {
			return entry, true
		}
	}
	return connectorRegistryEntry{}, false
}

// ─────────────────────────────────────────────────────────────────────────────
// LLM function-calling format
// ─────────────────────────────────────────────────────────────────────────────

// LLMFunctionParameters is the JSON Schema passed in the function definition.
type LLMFunctionParameters struct {
	Type       string                        `json:"type"`
	Properties map[string]JSONSchemaProperty `json:"properties,omitempty"`
	Required   []string                      `json:"required,omitempty"`
}

// LLMFunction describes a callable function for LLMs that support function
// calling (OpenAI-compatible format).
type LLMFunction struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Parameters  LLMFunctionParameters `json:"parameters"`
}

// LLMTool wraps an LLMFunction in the tool envelope expected by OpenAI-
// compatible APIs.
type LLMTool struct {
	Type     string      `json:"type"` // always "function"
	Function LLMFunction `json:"function"`
}

// llmTools generates the LLM-compatible tool array from all enabled connectors.
func (s *connectorStore) llmTools() []LLMTool {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tools []LLMTool
	for _, c := range s.data {
		if !c.Enabled {
			continue
		}
		for _, cap := range c.Capabilities {
			tools = append(tools, LLMTool{
				Type: "function",
				Function: LLMFunction{
					Name:        cap.Name,
					Description: cap.Description,
					Parameters: LLMFunctionParameters{
						Type:       cap.InputSchema.Type,
						Properties: cap.InputSchema.Properties,
						Required:   cap.InputSchema.Required,
					},
				},
			})
		}
	}
	if tools == nil {
		tools = []LLMTool{}
	}
	return tools
}

// enabledToolDefs exposes enabled connector capabilities as tinyRAG toolDef
// entries so they can be used through the XML tool protocol.
func (s *connectorStore) enabledToolDefs() []toolDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []toolDef
	for _, c := range s.data {
		if !c.Enabled {
			continue
		}
		for _, cap := range c.Capabilities {
			paramHint := "JSON input matching connector schema"
			if len(cap.InputSchema.Required) == 1 {
				paramHint = fmt.Sprintf("Either plain text or JSON with required field %q", cap.InputSchema.Required[0])
			}
			schema := cap.InputSchema
			out = append(out, toolDef{
				Name:        cap.Name,
				Description: fmt.Sprintf("%s (connector: %s)", cap.Description, c.Name),
				ParamHint:   paramHint,
				InputSchema: &schema,
			})
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// MCP-compatible interface (stretch goal)
// ─────────────────────────────────────────────────────────────────────────────

// MCPTool represents a tool in the Model Context Protocol format.
type MCPTool struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema JSONSchema `json:"inputSchema"`
}

// MCPToolsResponse is the MCP list-tools response envelope.
type MCPToolsResponse struct {
	Tools []MCPTool `json:"tools"`
}

// MCPCallRequest is the MCP call-tool request body.
type MCPCallRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// MCPContentItem is a single content item in an MCP tool result.
type MCPContentItem struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// MCPCallResult is the MCP call-tool response envelope.
type MCPCallResult struct {
	Content []MCPContentItem `json:"content"`
	IsError bool             `json:"isError"`
}

// mcpTools returns the MCP-compatible tool list.
func (s *connectorStore) mcpTools() MCPToolsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tools []MCPTool
	for _, c := range s.data {
		if !c.Enabled {
			continue
		}
		for _, cap := range c.Capabilities {
			tools = append(tools, MCPTool{
				Name:        cap.Name,
				Description: cap.Description,
				InputSchema: cap.InputSchema,
			})
		}
	}
	if tools == nil {
		tools = []MCPTool{}
	}
	return MCPToolsResponse{Tools: tools}
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor
// ─────────────────────────────────────────────────────────────────────────────

// ConnectorExecRequest carries the information needed to execute a capability.
type ConnectorExecRequest struct {
	ConnectorID string         `json:"connector_id"`
	Capability  string         `json:"capability"`
	Input       map[string]any `json:"input"`
}

// ConnectorExecResult holds the outcome of a capability execution.
type ConnectorExecResult struct {
	ConnectorID string         `json:"connector_id"`
	Capability  string         `json:"capability"`
	Output      map[string]any `json:"output"`
	Raw         string         `json:"raw,omitempty"`
	Source      string         `json:"source"`
	DurationMs  int64          `json:"duration_ms"`
	TraceID     string         `json:"trace_id"`
}

// connectorExecutor runs capabilities and handles validation, timeouts, and
// logging.
type connectorExecutor struct {
	store *connectorStore
}

func newConnectorExecutor(store *connectorStore) *connectorExecutor {
	return &connectorExecutor{store: store}
}

// newTraceID generates a short random hex trace identifier.
func newTraceID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Execute finds the requested capability and runs it. It is the compatibility
// entry point for manually triggered connector calls.
func (e *connectorExecutor) Execute(req ConnectorExecRequest) (ConnectorExecResult, error) {
	return e.ExecuteContext(context.Background(), req)
}

// ExecuteContext runs a capability under the caller's cancellation boundary.
// The connector-specific timeout remains an upper bound, while a cancelled
// HTTP request or agent request stops sooner.
func (e *connectorExecutor) ExecuteContext(parent context.Context, req ConnectorExecRequest) (ConnectorExecResult, error) {
	traceID := newTraceID()
	start := time.Now()
	log.Printf("[connector] trace=%s connector=%s capability=%s — starting", traceID, req.ConnectorID, req.Capability)

	conn, ok := e.store.get(req.ConnectorID)
	if !ok {
		return ConnectorExecResult{}, fmt.Errorf("connector %q not found", req.ConnectorID)
	}
	if !conn.Enabled {
		return ConnectorExecResult{}, fmt.Errorf("connector %q is disabled", req.ConnectorID)
	}

	var cap *Capability
	for i := range conn.Capabilities {
		if conn.Capabilities[i].Name == req.Capability {
			cap = &conn.Capabilities[i]
			break
		}
	}
	if cap == nil {
		return ConnectorExecResult{}, fmt.Errorf("capability %q not found on connector %q", req.Capability, req.ConnectorID)
	}

	// Validate input against the capability's JSON schema.
	if err := validateInput(req.Input, cap.InputSchema); err != nil {
		return ConnectorExecResult{}, fmt.Errorf("input validation failed: %w", err)
	}

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, conn.timeout())
	defer cancel()

	var (
		raw     string
		output  map[string]any
		source  string
		execErr error
	)

	switch conn.Type {
	case ConnectorTypeHTTP:
		raw, output, source, execErr = executeHTTPCapability(ctx, conn, *cap, req.Input)
	case ConnectorTypeRPC:
		raw, output, source, execErr = executeRPCCapability(ctx, conn, *cap, req.Input)
	case ConnectorTypeSQL:
		raw, output, source, execErr = executeSQLCapability(ctx, conn, *cap, req.Input)
	default:
		execErr = fmt.Errorf("unsupported connector type %q", conn.Type)
	}

	dur := time.Since(start).Milliseconds()
	if execErr != nil {
		log.Printf("[connector] trace=%s connector=%s capability=%s error=%v duration_ms=%d",
			traceID, req.ConnectorID, req.Capability, execErr, dur)
		return ConnectorExecResult{}, execErr
	}

	log.Printf("[connector] trace=%s connector=%s capability=%s source=%s duration_ms=%d — ok",
		traceID, req.ConnectorID, req.Capability, source, dur)

	return ConnectorExecResult{
		ConnectorID: req.ConnectorID,
		Capability:  req.Capability,
		Output:      output,
		Raw:         raw,
		Source:      source,
		DurationMs:  dur,
		TraceID:     traceID,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Input validation
// ─────────────────────────────────────────────────────────────────────────────

// validateInput checks that input satisfies the required fields of schema.
// It also verifies that no unknown required fields are missing.
func validateInput(input map[string]any, schema JSONSchema) error {
	if input == nil {
		input = map[string]any{}
	}
	for _, req := range schema.Required {
		v, ok := input[req]
		if !ok || v == nil {
			return fmt.Errorf("required parameter %q is missing", req)
		}
		// For string values, also reject empty strings.
		if s, isStr := v.(string); isStr && s == "" {
			return fmt.Errorf("required parameter %q must not be empty", req)
		}
	}
	// Type-check present properties.
	for key, val := range input {
		prop, defined := schema.Properties[key]
		if !defined {
			continue // unknown extra params are allowed
		}
		if err := checkType(key, val, prop.Type); err != nil {
			return err
		}
		// Enum check
		if len(prop.Enum) > 0 {
			strVal := fmt.Sprintf("%v", val)
			found := false
			for _, e := range prop.Enum {
				if e == strVal {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("parameter %q value %q is not in allowed enum %v", key, strVal, prop.Enum)
			}
		}
	}
	return nil
}

func checkType(key string, val any, typ string) error {
	switch typ {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %q must be a string, got %T", key, val)
		}
	case "number", "integer":
		switch val.(type) {
		case float64, float32, int, int64, int32, int16, int8, uint, uint64, uint32:
			// ok
		default:
			return fmt.Errorf("parameter %q must be a number, got %T", key, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean, got %T", key, val)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Template parameter substitution helpers
// ─────────────────────────────────────────────────────────────────────────────

// limitRE detects an existing LIMIT clause in a SQL query (case-insensitive).
var limitRE = regexp.MustCompile(`(?i)\bLIMIT\s+\d+`)
var templateParamRE = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// expandTemplate replaces all {key} occurrences in tmpl with the corresponding
// value from params (converted to string). Unknown placeholders remain.
func expandTemplate(tmpl string, params map[string]any) string {
	return templateParamRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := m[1 : len(m)-1]
		if v, ok := params[key]; ok {
			return fmt.Sprintf("%v", v)
		}
		return m
	})
}

// expandSQLTemplate replaces {key} placeholders in a SQL template.
// String values (schema type "string") are single-quote escaped.
// Numeric/boolean values are inserted without quoting.
func expandSQLTemplate(query string, params map[string]any, schema JSONSchema) string {
	return templateParamRE.ReplaceAllStringFunc(query, func(m string) string {
		key := m[1 : len(m)-1]
		v, ok := params[key]
		if !ok {
			return m
		}
		// Determine the schema type for this param.
		prop, hasProp := schema.Properties[key]
		strVal := fmt.Sprintf("%v", v)
		if hasProp && (prop.Type == "number" || prop.Type == "integer" || prop.Type == "boolean") {
			return strVal
		}
		// Default to string quoting: escape single quotes.
		return "'" + strings.ReplaceAll(strVal, "'", "''") + "'"
	})
}

// isSelectQuery returns true if q begins with SELECT or WITH (for CTEs).
// It normalises whitespace and is case-insensitive.
var selectRE = regexp.MustCompile(`(?i)^\s*(SELECT|WITH)\s`)

func isSelectQuery(q string) bool {
	return selectRE.MatchString(q)
}

// ─────────────────────────────────────────────────────────────────────────────
// Output mapping helpers
// ─────────────────────────────────────────────────────────────────────────────

// applyOutputMap extracts fields from rawJSON according to the OutputMap.
// If the mapping is empty, the full decoded JSON is returned as output.
func applyOutputMap(rawJSON string, mappings []OutputMapping) (map[string]any, error) {
	if rawJSON == "" {
		return map[string]any{}, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(rawJSON), &decoded); err != nil {
		// Not JSON — return raw text under "text" key.
		return map[string]any{"text": rawJSON}, nil
	}

	if len(mappings) == 0 {
		switch v := decoded.(type) {
		case map[string]any:
			return v, nil
		default:
			return map[string]any{"value": decoded}, nil
		}
	}

	out := make(map[string]any, len(mappings))
	for _, m := range mappings {
		val := dotGet(decoded, m.Field)
		targetKey := m.As
		if targetKey == "" {
			targetKey = m.Field
		}
		out[targetKey] = val
	}
	return out, nil
}

// dotGet navigates a dot-separated path in a decoded JSON value.
func dotGet(v any, path string) any {
	if path == "" {
		return v
	}
	parts := strings.SplitN(path, ".", 2)
	key := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}

	switch node := v.(type) {
	case map[string]any:
		child, ok := node[key]
		if !ok {
			return nil
		}
		return dotGet(child, rest)
	case []any:
		idx := 0
		if _, err := fmt.Sscan(key, &idx); err != nil {
			return nil
		}
		if idx < 0 || idx >= len(node) {
			return nil
		}
		return dotGet(node[idx], rest)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP connector executor
// ─────────────────────────────────────────────────────────────────────────────

// executeHTTPCapability performs the HTTP request for a capability.
func executeHTTPCapability(
	ctx context.Context,
	conn Connector,
	cap Capability,
	input map[string]any,
) (raw string, output map[string]any, source string, err error) {

	baseURL := resolveSecret(conn.BaseURL)
	baseURL = strings.TrimRight(baseURL, "/")

	// Build URL.
	path := expandTemplate(cap.PathTemplate, input)
	fullURL := baseURL + path
	if err := isSafeFetchURL(fullURL); err != nil {
		return "", nil, "http:" + fullURL, err
	}
	source = "http:" + fullURL

	// Build body (for POST/PUT/PATCH).
	var bodyReader io.Reader
	method := strings.ToUpper(cap.Method)
	if method == "" {
		method = http.MethodGet
	}
	if cap.BodyTemplate != "" {
		expanded := expandTemplate(cap.BodyTemplate, input)
		bodyReader = strings.NewReader(expanded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return "", nil, source, fmt.Errorf("http: build request: %w", err)
	}

	// Apply headers with secret resolution.
	if cap.BodyTemplate != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range conn.Headers {
		req.Header.Set(k, resolveSecret(v))
	}

	client := newExternalHTTPClient(conn.timeout())
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, source, fmt.Errorf("http: do request: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, conn.maxBody())
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		return "", nil, source, fmt.Errorf("http: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, source, fmt.Errorf("http: unexpected status %d", resp.StatusCode)
	}

	raw = string(bodyBytes)
	output, err = applyOutputMap(raw, cap.OutputMap)
	if err != nil {
		return raw, nil, source, fmt.Errorf("http: output mapping: %w", err)
	}
	return raw, output, source, nil
}

// buildJSONRPCRequest creates a JSON-RPC 2.0 request from a capability and
// its already schema-validated input. Keeping params structured avoids the
// quoting and injection hazards of string templates.
func buildJSONRPCRequest(cap Capability, input map[string]any) ([]byte, error) {
	method := strings.TrimSpace(cap.RPCMethod)
	if method == "" {
		return nil, fmt.Errorf("rpc: rpc_method is required for capability %q", cap.Name)
	}
	return json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      string         `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      cap.Name,
		Method:  method,
		Params:  input,
	})
}

// executeRPCCapability invokes a JSON-RPC 2.0 endpoint. The response's
// `result` is the tool output; protocol errors are returned as regular tool
// errors and never presented as evidence.
func executeRPCCapability(
	ctx context.Context,
	conn Connector,
	cap Capability,
	input map[string]any,
) (raw string, output map[string]any, source string, err error) {
	endpoint := strings.TrimSpace(resolveSecret(conn.BaseURL))
	if err := isSafeFetchURL(endpoint); err != nil {
		return "", nil, "rpc:" + endpoint, err
	}
	payload, err := buildJSONRPCRequest(cap, input)
	if err != nil {
		return "", nil, "rpc:" + endpoint, err
	}
	source = "rpc:" + endpoint + "#" + strings.TrimSpace(cap.RPCMethod)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", nil, source, fmt.Errorf("rpc: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range conn.Headers {
		req.Header.Set(key, resolveSecret(value))
	}

	resp, err := newExternalHTTPClient(conn.timeout()).Do(req)
	if err != nil {
		return "", nil, source, fmt.Errorf("rpc: do request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, conn.maxBody()))
	if err != nil {
		return "", nil, source, fmt.Errorf("rpc: read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", nil, source, fmt.Errorf("rpc: unexpected status %d", resp.StatusCode)
	}

	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", nil, source, fmt.Errorf("rpc: invalid JSON-RPC response: %w", err)
	}
	if envelope.JSONRPC != "2.0" {
		return "", nil, source, fmt.Errorf("rpc: response is not JSON-RPC 2.0")
	}
	if envelope.Error != nil {
		return "", nil, source, fmt.Errorf("rpc: remote error %d: %s", envelope.Error.Code, strings.TrimSpace(envelope.Error.Message))
	}
	if len(envelope.Result) == 0 {
		return "", nil, source, fmt.Errorf("rpc: response has no result")
	}
	raw = string(envelope.Result)
	output, err = applyOutputMap(raw, cap.OutputMap)
	if err != nil {
		return raw, nil, source, fmt.Errorf("rpc: output mapping: %w", err)
	}
	return raw, output, source, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SQL connector executor
// ─────────────────────────────────────────────────────────────────────────────

// executeSQLCapability runs a read-only parameterised SQL query via the CLI
// tool appropriate for the configured driver.
func executeSQLCapability(
	ctx context.Context,
	conn Connector,
	cap Capability,
	input map[string]any,
) (raw string, output map[string]any, source string, err error) {

	if cap.Query == "" {
		return "", nil, "", fmt.Errorf("sql: no query defined for capability %q", cap.Name)
	}
	if !isSelectQuery(cap.Query) {
		return "", nil, "", fmt.Errorf("sql: only SELECT queries are allowed (read-only)")
	}

	// Expand template parameters.
	query := expandSQLTemplate(cap.Query, input, cap.InputSchema)

	// Enforce row limit (only when the query does not already specify one).
	if conn.MaxRows > 0 && !limitRE.MatchString(query) {
		query = fmt.Sprintf("%s LIMIT %d", query, conn.MaxRows)
	}

	cfg := resolveConfigSecret(conn.Config)
	driver := strings.ToLower(strings.TrimSpace(conn.Driver))

	var out string
	switch driver {
	case "sqlite", "":
		out, source, err = runSQLiteConnector(ctx, cfg, query, conn.ID)
	case "mariadb", "mysql":
		out, source, err = runMariaDBConnector(ctx, cfg, query, conn.ID)
	case "mssql":
		out, source, err = runMSSQLConnector(ctx, cfg, query, conn.ID)
	default:
		err = fmt.Errorf("sql: unsupported driver %q", driver)
	}
	if err != nil {
		return "", nil, source, err
	}

	raw = strings.TrimSpace(out)

	// Try to parse tabular output to extract mapped columns.
	output = parseSQLOutput(raw, cap.OutputMap)
	return raw, output, source, nil
}

// runSQLiteConnector runs a query against an SQLite database via the sqlite3 CLI.
func runSQLiteConnector(ctx context.Context, cfg map[string]string, query, connID string) (string, string, error) {
	bin := strings.TrimSpace(cfg["sqlite_bin"])
	if bin == "" {
		bin = "sqlite3"
	}
	dbPath := strings.TrimSpace(cfg["sqlite_path"])
	if dbPath == "" {
		return "", "", fmt.Errorf("sql: sqlite_path not configured")
	}
	source := fmt.Sprintf("connector:%s:sqlite:%s", connID, filepath.Base(dbPath))
	out, err := runCommandContext(ctx, bin, []string{"-header", "-column", dbPath, query}, nil)
	return out, source, err
}

// runMariaDBConnector runs a query against a MariaDB/MySQL server via the mysql CLI.
func runMariaDBConnector(ctx context.Context, cfg map[string]string, query, connID string) (string, string, error) {
	bin := strings.TrimSpace(cfg["mariadb_bin"])
	if bin == "" {
		bin = "mysql"
	}
	host := strings.TrimSpace(cfg["mariadb_host"])
	port := strings.TrimSpace(cfg["mariadb_port"])
	user := strings.TrimSpace(cfg["mariadb_user"])
	pass := cfg["mariadb_pass"]
	db := strings.TrimSpace(cfg["mariadb_db"])

	args := []string{"-B", "-r", "-e", query}
	if host != "" {
		args = append(args, "-h", host)
	}
	if port != "" {
		args = append(args, "-P", port)
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	if db != "" {
		args = append(args, "-D", db)
	}
	var env []string
	if pass != "" {
		env = append(env, "MYSQL_PWD="+pass)
	}
	source := fmt.Sprintf("connector:%s:mariadb:%s", connID, db)
	out, err := runCommandContext(ctx, bin, args, env)
	return out, source, err
}

// runMSSQLConnector runs a query against an MSSQL server via the sqlcmd CLI.
func runMSSQLConnector(ctx context.Context, cfg map[string]string, query, connID string) (string, string, error) {
	bin := strings.TrimSpace(cfg["mssql_bin"])
	if bin == "" {
		bin = "sqlcmd"
	}
	host := strings.TrimSpace(cfg["mssql_host"])
	port := strings.TrimSpace(cfg["mssql_port"])
	user := strings.TrimSpace(cfg["mssql_user"])
	pass := cfg["mssql_pass"]
	db := strings.TrimSpace(cfg["mssql_db"])

	server := host
	if port != "" {
		server = host + "," + port
	}
	args := []string{"-S", server, "-Q", query, "-W"}
	if user != "" {
		args = append(args, "-U", user)
	}
	if pass != "" {
		args = append(args, "-P", pass)
	}
	if db != "" {
		args = append(args, "-d", db)
	}
	source := fmt.Sprintf("connector:%s:mssql:%s", connID, db)
	out, err := runCommandContext(ctx, bin, args, nil)
	return out, source, err
}

// runCommandContext runs an external command with the provided context for
// timeout cancellation. Supplementary env vars are merged with os.Environ().
func runCommandContext(ctx context.Context, name string, args []string, env []string) (string, error) {
	// Resolve the binary to an absolute path.
	binPath, lookErr := lookPath(name)
	if lookErr != nil {
		return "", fmt.Errorf("required binary %q not found in PATH", name)
	}

	cmd := newExecCommandContext(ctx, binPath, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("%s: command timed out", name)
		}
		return buf.String(), fmt.Errorf("%s failed: %w\n%s", name, runErr, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// parseSQLOutput converts tabular CLI output (header + rows separated by
// whitespace) into a structured map using the OutputMap. If no OutputMap is
// given, the raw text is placed under "text".
func parseSQLOutput(raw string, mappings []OutputMapping) map[string]any {
	if len(mappings) == 0 {
		return map[string]any{"text": raw}
	}

	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) < 2 {
		return map[string]any{"text": raw}
	}

	// First line is the header row.
	headers := strings.Fields(lines[0])
	colIdx := make(map[string]int, len(headers))
	for i, h := range headers {
		colIdx[strings.ToLower(h)] = i
	}

	// Collect first data row for now (simple single-row extraction).
	if len(lines) < 2 {
		return map[string]any{"text": raw}
	}
	firstRow := strings.Fields(lines[1])

	out := make(map[string]any, len(mappings))
	for _, m := range mappings {
		col := strings.ToLower(m.Field)
		idx, ok := colIdx[col]
		if !ok || idx >= len(firstRow) {
			out[m.As] = nil
			continue
		}
		out[m.As] = firstRow[idx]
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Connector health test
// ─────────────────────────────────────────────────────────────────────────────

// ConnectorTestResult is the result of a health-check ping on a connector.
type ConnectorTestResult struct {
	ConnectorID string `json:"connector_id"`
	OK          bool   `json:"ok"`
	Message     string `json:"message"`
	DurationMs  int64  `json:"duration_ms"`
	TraceID     string `json:"trace_id"`
}

// TestConnector performs a basic connectivity check on the connector:
//   - HTTP/RPC: issues a HEAD request (or GET if HEAD is not supported) to BaseURL.
//   - SQL:  verifies that the CLI binary is in PATH and the db path is accessible.
func (e *connectorExecutor) TestConnector(id string) ConnectorTestResult {
	traceID := newTraceID()
	start := time.Now()

	conn, ok := e.store.get(id)
	if !ok {
		return ConnectorTestResult{ConnectorID: id, OK: false, Message: "connector not found", TraceID: traceID}
	}

	ctx, cancel := context.WithTimeout(context.Background(), conn.timeout())
	defer cancel()

	var msg string
	switch conn.Type {
	case ConnectorTypeHTTP, ConnectorTypeRPC:
		msg = testHTTPConnector(ctx, conn)
	case ConnectorTypeSQL:
		msg = testSQLConnector(ctx, conn)
	default:
		msg = fmt.Sprintf("unsupported connector type %q", conn.Type)
	}

	dur := time.Since(start).Milliseconds()
	ok2 := msg == ""
	if ok2 {
		msg = "ok"
	}
	log.Printf("[connector] trace=%s connector=%s test ok=%v duration_ms=%d message=%s",
		traceID, id, ok2, dur, msg)
	return ConnectorTestResult{
		ConnectorID: id,
		OK:          ok2,
		Message:     msg,
		DurationMs:  dur,
		TraceID:     traceID,
	}
}

func testHTTPConnector(ctx context.Context, conn Connector) string {
	baseURL := resolveSecret(conn.BaseURL)
	if baseURL == "" {
		return "base_url is empty"
	}
	if err := isSafeFetchURL(baseURL); err != nil {
		return err.Error()
	}
	client := newExternalHTTPClient(conn.timeout())
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, baseURL, nil)
	if err != nil {
		return fmt.Sprintf("build request: %v", err)
	}
	for k, v := range conn.Headers {
		req.Header.Set(k, resolveSecret(v))
	}
	resp, err := client.Do(req)
	if err != nil {
		// Retry with GET in case the server doesn't support HEAD.
		req2, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
		if reqErr != nil {
			return fmt.Sprintf("request failed: %v (retry build: %v)", err, reqErr)
		}
		resp2, err2 := client.Do(req2)
		if err2 != nil {
			return fmt.Sprintf("request failed: %v", err)
		}
		resp2.Body.Close()
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Sprintf("server returned status %d", resp.StatusCode)
	}
	return ""
}

func testSQLConnector(ctx context.Context, conn Connector) string {
	cfg := resolveConfigSecret(conn.Config)
	driver := strings.ToLower(strings.TrimSpace(conn.Driver))
	switch driver {
	case "sqlite", "":
		bin := cfg["sqlite_bin"]
		if bin == "" {
			bin = "sqlite3"
		}
		if _, err := lookPath(bin); err != nil {
			return fmt.Sprintf("sqlite3 binary %q not found in PATH", bin)
		}
		dbPath := cfg["sqlite_path"]
		if dbPath == "" {
			return "sqlite_path not configured"
		}
		out, err := runCommandContext(ctx, bin, []string{"-header", "-column", dbPath, "SELECT 1;"}, nil)
		if err != nil {
			return fmt.Sprintf("sqlite test query failed: %v", err)
		}
		_ = out
	case "mariadb", "mysql":
		bin := cfg["mariadb_bin"]
		if bin == "" {
			bin = "mysql"
		}
		if _, err := lookPath(bin); err != nil {
			return fmt.Sprintf("mysql binary %q not found in PATH", bin)
		}
	case "mssql":
		bin := cfg["mssql_bin"]
		if bin == "" {
			bin = "sqlcmd"
		}
		if _, err := lookPath(bin); err != nil {
			return fmt.Sprintf("sqlcmd binary %q not found in PATH", bin)
		}
	default:
		return fmt.Sprintf("unsupported driver %q", driver)
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// Seams for testing — thin wrappers around os/exec that tests can replace.
// ─────────────────────────────────────────────────────────────────────────────

// lookPath is an overridable wrapper around exec.LookPath.
var lookPath = func(file string) (string, error) {
	return exec.LookPath(file)
}

// newExecCommandContext is an overridable wrapper around exec.CommandContext.
var newExecCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handler helpers — registered in runWebServer
// ─────────────────────────────────────────────────────────────────────────────

// registerConnectorRoutes attaches all connector-related HTTP endpoints to mux.
func registerConnectorRoutes(mux *http.ServeMux, store *connectorStore, executor *connectorExecutor, guard func(http.HandlerFunc) http.HandlerFunc) {
	if guard == nil {
		guard = func(h http.HandlerFunc) http.HandlerFunc { return h }
	}
	// GET  /api/connectors         — list all connectors
	// POST /api/connectors         — upsert a connector
	mux.HandleFunc("/api/connectors", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(store.list())
		case http.MethodPost:
			var c Connector
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			saved, err := store.upsert(c)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(saved)
		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
	}))

	// POST /api/connectors/delete  — delete connector by id
	mux.HandleFunc("/api/connectors/delete", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		ok, err := store.remove(req.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	// GET /api/connectors/tools    — LLM function-calling tool array
	mux.HandleFunc("/api/connectors/tools", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.llmTools())
	}))

	// POST /api/connectors/execute — execute a capability
	mux.HandleFunc("/api/connectors/execute", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req ConnectorExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		result, err := executor.Execute(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}))

	// POST /api/connectors/test    — test connector health
	mux.HandleFunc("/api/connectors/test", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		result := executor.TestConnector(req.ID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}))

	// ── MCP-compatible endpoints ───────────────────────────────────────────

	// GET /api/mcp/tools           — MCP tool list
	mux.HandleFunc("/api/mcp/tools", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.mcpTools())
	}))

	// POST /api/mcp/execute        — MCP call-tool
	mux.HandleFunc("/api/mcp/execute", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var mcpReq MCPCallRequest
		if err := json.NewDecoder(r.Body).Decode(&mcpReq); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Resolve the capability name to its connector via the registry.
		reg := store.registry()
		entry, ok := reg[mcpReq.Name]
		if !ok {
			mcpErr := MCPCallResult{
				Content: []MCPContentItem{{Type: "text", Text: fmt.Sprintf("tool %q not found", mcpReq.Name)}},
				IsError: true,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mcpErr)
			return
		}
		if strings.TrimSpace(entry.Connector.Config["mcp_approved"]) != "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(MCPCallResult{
				Content: []MCPContentItem{{Type: "text", Text: "tool is not approved for MCP execution"}},
				IsError: true,
			})
			return
		}

		result, err := executor.Execute(ConnectorExecRequest{
			ConnectorID: entry.Connector.ID,
			Capability:  mcpReq.Name,
			Input:       mcpReq.Arguments,
		})

		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(MCPCallResult{
				Content: []MCPContentItem{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
			return
		}

		// Encode the output map as the text content.
		outBytes, _ := json.Marshal(result.Output)
		json.NewEncoder(w).Encode(MCPCallResult{
			Content: []MCPContentItem{{Type: "text", Text: string(outBytes)}},
			IsError: false,
		})
	}))
}
