package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI-compatible chat completions server — one or more named endpoints
// (settings.OpenAIAPI.Endpoints, each an openAIEndpointConfig — see its doc
// comment in settings.go for the full capability model), all sharing one
// dedicated TCP port (settings.OpenAIAPI.Port), separate from the main
// UI/API port. Point any OpenAI-client-shaped tool (Open WebUI, an IDE
// assistant, another agent) at http://host:port[/<endpoint-name>]/v1/... and
// it gets exactly the mix of R3 capabilities that endpoint was configured
// with — anywhere from a bare LLM passthrough (R3's configured default
// backend, e.g. Azure, nothing else) up to full RAG + live tools — entirely
// transparently: the caller only ever sees a normal chat completion
// response, never which internal capability produced it.
//
// Routing: each endpoint mounts at "/"+Name+"/v1/chat/completions" and
// "/"+Name+"/v1/models"; Name "" mounts at the bare, unprefixed "/v1/..."
// paths (at most one endpoint may use it — validateOpenAIEndpoints enforces
// this), so a single-endpoint deployment needs no path prefix at all.
//
// Tool building deliberately reuses agent.go's buildLiveTools — the exact
// same function Chat/Agent/Mail already call — rather than a second,
// hand-rolled copy of the MSSQL/Shop/HTTP-template loop: a live tool
// enabled once in Settings is then automatically available to every
// surface (bundled UI or external API) whose capability flags ask for it,
// with no risk of the two copies drifting apart.
//
// Auth: unlike /api/ask/api/search (gated only when settings.api.
// require_api_key is explicitly turned on), this server ALWAYS requires a
// valid key from settings.api.keys, for every endpoint — it's a distinct,
// deliberately-exposed integration surface for other tools, not the same
// trust level as the bundled browser UI's same-origin requests, so it
// defaults secure rather than defaulting open. Create a key in Settings →
// "API-Zugriff" (the same key list works for both surfaces). A tool
// executor's audit-log entry (agent.go's auditExecutor) is attributed to
// the presented key's Name, same as every other identity-aware feature.
//
// The listener itself is fully torn down and rebuilt whenever settings are
// saved (see reconcileOpenAIAPIServer, called from handleSettings) — no
// restart needed to turn it on/off, move it to a different port, add/
// remove/reconfigure an endpoint, since every endpoint's capability flags
// are baked into its route closures at registration time and would
// otherwise go stale.
// ─────────────────────────────────────────────────────────────────────────────

// ---- Request/response shapes (OpenAI chat completions API) ----------------

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// UnmarshalJSON accepts both the original string form and the current
// OpenAI-compatible array-of-content-parts form. R3 is text-only at this
// boundary, so image/audio/file parts fail explicitly instead of quietly
// disappearing from a question and producing a misleading answer.
func (m *openAIChatMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = ""
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw.Content, &text); err == nil {
		m.Content = text
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return fmt.Errorf("content must be a string or an array of text parts")
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "text" {
			return fmt.Errorf("unsupported content part type %q; R3 accepts text only", part.Type)
		}
		texts = append(texts, part.Text)
	}
	m.Content = strings.Join(texts, "\n")
	return nil
}

type openAIChatCompletionRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
	Stream   bool                `json:"stream,omitempty"`
	N        int                 `json:"n,omitempty"`
}

type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

type openAIErrorResponse struct {
	Error openAIErrorBody `json:"error"`
}

type openAIChatCompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIUsage        `json:"usage"`
}

type openAIChatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIChatChunkChoice struct {
	Index        int             `json:"index"`
	Delta        openAIChatDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason,omitempty"`
}

type openAIChatCompletionChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAIChatChunkChoice `json:"choices"`
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

// openAICompletionID produces an id in OpenAI's own "chatcmpl-..." shape —
// cosmetic (nothing looks it up later), but a client that pattern-matches
// on the prefix shouldn't be surprised.
func openAICompletionID() string {
	return fmt.Sprintf("chatcmpl-r3-%d", time.Now().UnixNano())
}

// writeOpenAIError uses the error envelope OpenAI SDKs expect, rather than
// the bundled UI API's simpler {error: "..."} convention.
func writeOpenAIError(w http.ResponseWriter, message, errorType, param, code string, status int) {
	var paramPtr, codePtr *string
	if param != "" {
		paramPtr = &param
	}
	if code != "" {
		codePtr = &code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorResponse{Error: openAIErrorBody{
		Message: message, Type: errorType, Param: paramPtr, Code: codePtr,
	}})
}

// estimateOpenAITokens is intentionally a transparent approximation for
// OpenAI-compatible clients: R3 can route to different local/Azure models
// and therefore has no one tokenizer to report exact counts for. A
// rune-based 4-characters-per-token estimate is still more useful than the
// former permanent zeroes for dashboards and context-budget guards.
func estimateOpenAITokens(text string) int {
	if text == "" {
		return 0
	}
	return (len([]rune(text)) + 3) / 4
}

// ---- Auth -------------------------------------------------------------------

// requireOpenAIAPIKey always demands a valid key from settings.api.keys —
// see the package comment for why this, unlike requireAPIKey
// (handlers.go), is never optional.
func requireOpenAIAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := settings.get()
		presented := r.Header.Get("X-API-Key")
		if presented == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				presented = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		rec, ok := findAPIKey(s.API.Keys, presented)
		if !ok {
			writeOpenAIError(w, "missing or invalid API key", "authentication_error", "", "invalid_api_key", http.StatusUnauthorized)
			return
		}
		touchAPIKeyLastUsed(rec.ID)
		// See requireAPIKey's identical stash (handlers.go) — lets
		// tokenUsageActor (tokenusage.go) attribute this caller's usage to
		// the key's Name instead of "anonym".
		r = r.WithContext(withAPIKeyName(r.Context(), rec.Name))
		next(w, r)
	}
}

// ---- Handlers ---------------------------------------------------------------

// handleOpenAIModels lists every configured chat profile as a model id. A
// caller can set "model" to one of these stable profile names in a chat
// completion request; the actual provider/model remains server-side —
// unless ep.Profile pins this endpoint to exactly one backend already, in
// which case that's the only entry listed (an accurate reflection of what
// the endpoint will actually use, regardless of what "model" a caller
// sends — see openAIEndpointConfig.Profile's doc comment).
func handleOpenAIModels(ep openAIEndpointConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		s := settings.get()
		profiles := []struct {
			name    string
			profile llmProfile
		}{
			{"local", s.Profiles.Local},
			{"azure", s.Profiles.Azure},
			{"openai", s.Profiles.OpenAI},
			{"openrouter", s.Profiles.OpenRouter},
			{"claude", s.Profiles.Claude},
			{"gemini", s.Profiles.Gemini},
		}
		pinned := strings.ToLower(strings.TrimSpace(ep.Profile))
		data := make([]openAIModel, 0, len(profiles))
		for _, entry := range profiles {
			if pinned != "" && entry.name != pinned {
				continue
			}
			if entry.name != "local" && strings.TrimSpace(entry.profile.ChatModel) == "" {
				continue
			}
			if entry.name == "azure" && strings.TrimSpace(entry.profile.BaseURL) == "" {
				continue
			}
			data = append(data, openAIModel{ID: entry.name, Object: "model", Created: now, OwnedBy: "r3"})
		}
		writeJSON(w, openAIModelList{
			Object: "list",
			Data:   data,
		})
	}
}

// passthroughSystemFromMessages joins every system/developer message's
// content (in order) into one string — used instead of R3's own system
// prompt when ep.EnableRAG is false (a bare LLM passthrough has no R3
// context to ground, so the caller's own instructions are honored rather
// than silently discarded, unlike the EnableRAG branch where R3's own
// prompt+context deliberately replaces whatever the caller sent).
func passthroughSystemFromMessages(messages []openAIChatMessage) string {
	var parts []string
	for _, m := range messages {
		if (m.Role == "system" || m.Role == "developer") && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// handleOpenAIChatCompletions answers a chat completion request according
// to ep's capability flags (see openAIEndpointConfig's doc comment in
// settings.go for the full RAG×Tools matrix), then either streams
// Server-Sent-Events chunks or returns one buffered JSON response, matching
// the OpenAI chat completions wire shape either way.
func handleOpenAIChatCompletions(rag *ragSystem, ep openAIEndpointConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var req openAIChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOpenAIError(w, "invalid body: "+err.Error(), "invalid_request_error", "messages", "", http.StatusBadRequest)
			return
		}
		if len(req.Messages) == 0 {
			writeOpenAIError(w, "messages must not be empty", "invalid_request_error", "messages", "", http.StatusBadRequest)
			return
		}
		if req.N > 1 {
			writeOpenAIError(w, "R3 supports one completion per request (n must be 1)", "invalid_request_error", "n", "unsupported_value", http.StatusBadRequest)
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role != "user" {
			writeOpenAIError(w, "the last message must have role user", "invalid_request_error", "messages", "", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(last.Content) == "" {
			writeOpenAIError(w, "the last message must have non-empty text content", "invalid_request_error", "messages", "", http.StatusBadRequest)
			return
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system", "developer", "user", "assistant":
			default:
				writeOpenAIError(w, "unsupported message role "+fmt.Sprintf("%q", m.Role), "invalid_request_error", "messages", "", http.StatusBadRequest)
				return
			}
		}

		s := settings.get()
		preset, _ := findPreset(s.Presets, ep.Preset)
		// External callers have no LDAP session/AD department of their
		// own — same anonymous-department treatment as an API-key-only
		// /api/ask caller today (SourceAccess still applies: only kinds
		// with no department restriction, or the preset's own kinds, are
		// ever reachable).
		const deptCode = ""

		var system string
		var citations []sourceInfo
		if ep.EnableRAG {
			hits, err := rag.rankedSearch(last.Content, s.K, s.Ranking, s.activeEmbedModel(), s.SourceAccess, deptCode, preset.Kinds)
			if err != nil {
				writeOpenAIError(w, "retrieval failed: "+err.Error(), "server_error", "", "", http.StatusInternalServerError)
				return
			}
			var contextText string
			contextText, citations = rag.assembleContext(hits, s.Ranking, s.SourceAccess, deptCode, preset.Kinds)
			for i := range citations {
				citations[i].SourceURL = resolveSourceURL(citations[i].SourceID, s.URLMappings)
			}
			agentPrompt, _ := buildSystemPromptForMode(s.PromptsDir, last.Content, "chat")
			system = agentPrompt + "\n\nKontext:\n" + contextText
		} else {
			// Bare LLM passthrough: no R3 prompt/context of its own — honor
			// whatever system/developer instructions the caller sent instead
			// of silently discarding them (see passthroughSystemFromMessages).
			system = passthroughSystemFromMessages(req.Messages)
		}

		var tools []toolDef
		executors := map[string]toolExecutor{}
		if ep.EnableTools {
			// buildLiveTools (agent.go) is the SAME function Chat/Agent/Mail
			// use — one place builds MSSQL/Shop/HTTP-template tools for every
			// surface, so a tool enabled in Settings is available here with
			// no separate, driftable copy of that wiring. mssqlAllowed=true
			// unconditionally: reaching this handler at all already required
			// a valid API key (requireOpenAIAPIKey), the same "Registriert"
			// trust level mssqlToolAllowed grants a logged-in session.
			sess := agentSession{User: "api", DeptCode: deptCode, IsAdmin: false, PresetKinds: preset.Kinds, PresetTools: preset.Tools}
			if name, ok := apiKeyNameFromContext(r.Context()); ok && name != "" {
				sess.User = "api:" + name
			}
			tools, executors = buildLiveTools(s, sess, preset, true)
			for name, exec := range executors {
				executors[name] = auditExecutor(sess.User, name, exec)
			}
		}

		messages := make([]chatMsg, 0, len(req.Messages)-1)
		for _, m := range req.Messages[:len(req.Messages)-1] {
			if m.Role == "system" || m.Role == "developer" {
				continue // folded into system above (replaced when EnableRAG, passed through otherwise)
			}
			messages = append(messages, chatMsg{Role: m.Role, Content: m.Content})
		}
		messages = append(messages, chatMsg{Role: "user", Content: last.Content})

		// Profile resolution: ep.Profile, if set, pins this endpoint to one
		// backend regardless of what "model" the caller sent — see
		// openAIEndpointConfig.Profile's doc comment. Otherwise the caller's
		// own "model" selects it, falling back to appSettings.ChatProfile
		// (this deployment's Azure default) exactly like Chat's own
		// askRequest.Profile.
		profile := strings.ToLower(strings.TrimSpace(ep.Profile))
		if profile == "" {
			profile = strings.ToLower(strings.TrimSpace(req.Model))
		}
		if profile == "" {
			profile = s.ChatProfile
		}
		if !isSupportedChatProfile(profile) {
			writeOpenAIError(w, "unknown model "+fmt.Sprintf("%q", req.Model), "invalid_request_error", "model", "model_not_found", http.StatusBadRequest)
			return
		}
		chatLM := rag.getChatLM(profile)

		// Tool-round budget: ignored (0, the loop's own "no tools" shape)
		// when tools aren't offered at all — the zero-tools branch of
		// chatWithToolsBudget/chatWithToolsBudgetDeadline (llm.go) degrades
		// straight to a plain stream with no extra tool-decision round-trip,
		// exactly as cheap as a true bare passthrough should be.
		rounds := 0
		if ep.EnableTools {
			rounds = ep.MaxToolRounds
			if rounds <= 0 {
				rounds = 1
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		// Token-usage logging (tokenusage.go): covers both the streaming and
		// non-streaming branches below, since handleOpenAIChatCompletionsStream
		// takes this same ctx and forwards it into its own chatWithToolsBudget
		// call.
		ctx, usageTrace := withTokenUsage(ctx, tokenUsageActor(r), "openai_api")
		defer recordTokenUsage(usageTrace)

		id := openAICompletionID()
		created := time.Now().Unix()
		model := profile

		if req.Stream {
			handleOpenAIChatCompletionsStream(ctx, w, chatLM, system, messages, tools, executors, rounds, s, citations, id, created, model)
			return
		}

		var buffered strings.Builder
		if err := chatLM.chatWithToolsBudget(ctx, system, messages, tools, executors, &buffered, rounds); err != nil {
			writeOpenAIError(w, "chat failed: "+err.Error(), "server_error", "", "", http.StatusInternalServerError)
			return
		}
		answer := strings.TrimSpace(buffered.String())
		answer = appendOpenAICitationsFooter(answer, filterCitations(citations, answer, s))
		writeJSON(w, openAIChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   model,
			Choices: []openAIChatChoice{{Index: 0, Message: openAIChatMessage{Role: "assistant", Content: answer}, FinishReason: "stop"}},
			Usage: openAIUsage{
				PromptTokens:     estimateOpenAITokens(system) + estimateOpenAITokens(strings.Join(messageContents(messages), "\n")),
				CompletionTokens: estimateOpenAITokens(answer),
				TotalTokens:      estimateOpenAITokens(system) + estimateOpenAITokens(strings.Join(messageContents(messages), "\n")) + estimateOpenAITokens(answer),
			},
		})
	}
}

func messageContents(messages []chatMsg) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

// appendOpenAICitationsFooter appends a short, plain-text "Quellen:" list
// of the citations that actually grounded the answer (filterCitations —
// only [Qn] markers the model actually used) — the OpenAI chat completions
// shape has no native citations field, so this is the one broadly
// compatible way to still hand the caller enough to know what the answer
// is based on. A no-op when nothing was cited.
func appendOpenAICitationsFooter(answer string, citations []sourceInfo) string {
	if len(citations) == 0 {
		return answer
	}
	var b strings.Builder
	b.WriteString(answer)
	b.WriteString("\n\nQuellen:\n")
	for _, c := range citations {
		fmt.Fprintf(&b, "[Q%d] %s", c.Marker, c.SourceName)
		if c.SourceURL != "" {
			fmt.Fprintf(&b, " (%s)", c.SourceURL)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// openAISSEWriter turns every Write call into one "data: {...}\n\n"
// chat.completion.chunk event — handed to chatWithToolsBudget exactly like
// flushingTokenWriter (handlers.go) is for the browser's NDJSON stream,
// just framed as SSE instead.
type openAISSEWriter struct {
	w             io.Writer
	flusher       http.Flusher
	id            string
	created       int64
	model         string
	sentRoleDelta bool
}

func (sw *openAISSEWriter) Write(p []byte) (int, error) {
	delta := openAIChatDelta{Content: string(p)}
	if !sw.sentRoleDelta {
		delta.Role = "assistant"
		sw.sentRoleDelta = true
	}
	chunk := openAIChatCompletionChunk{
		ID: sw.id, Object: "chat.completion.chunk", Created: sw.created, Model: sw.model,
		Choices: []openAIChatChunkChoice{{Index: 0, Delta: delta}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", raw); err != nil {
		return 0, err
	}
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
	return len(p), nil
}

// handleOpenAIChatCompletionsStream is handleOpenAIChatCompletions' SSE
// path — split out since the non-streaming path needs the full answer
// buffered anyway (to compute filterCitations), while this one flushes
// tokens live and only appends citations as one final content-only chunk.
func handleOpenAIChatCompletionsStream(ctx context.Context, w http.ResponseWriter, chatLM *lmClient, system string, messages []chatMsg, tools []toolDef, executors map[string]toolExecutor, rounds int, s appSettings, citations []sourceInfo, id string, created int64, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	var buffered strings.Builder
	sw := &openAISSEWriter{w: w, flusher: flusher, id: id, created: created, model: model}
	tokenWriter := io.MultiWriter(&buffered, sw)

	if err := chatLM.chatWithToolsBudget(ctx, system, messages, tools, executors, tokenWriter, rounds); err != nil {
		// Streaming already started (headers committed at 200), so a
		// failure can only be reported in-band — same constraint
		// handleAsk's NDJSON stream has, just framed as an SSE error
		// delta instead of an NDJSON "error" line.
		errChunk := openAIChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatDelta{Content: "\n[Fehler: " + err.Error() + "]"}}},
		}
		raw, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	answer := strings.TrimSpace(buffered.String())
	used := filterCitations(citations, answer, s)
	if len(used) > 0 {
		footer := appendOpenAICitationsFooter("", used)
		footerChunk := openAIChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatDelta{Content: footer}}},
		}
		raw, _ := json.Marshal(footerChunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
	}

	finish := "stop"
	finalChunk := openAIChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openAIChatChunkChoice{{Index: 0, Delta: openAIChatDelta{}, FinishReason: &finish}},
	}
	raw, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ---- Server lifecycle: start/stop/rebind on live settings changes ---------

var (
	openAIServerMu   sync.Mutex
	openAIServer     *http.Server
	openAIServerPort int
)

// openAIEndpointNameRe restricts a non-empty endpoint Name to a clean URL
// path segment — it becomes a literal path prefix other tools' base URLs
// point at ("/"+Name+"/v1/..."), so anything that would produce a broken or
// surprising URL is rejected here first, at save time. Empty is separately
// valid (the unprefixed root path) and deliberately not matched by this
// regex — see validateOpenAIEndpoints.
var openAIEndpointNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

// validateOpenAIEndpoints rejects an endpoint list with a Name that's
// neither empty nor openAIEndpointNameRe-shaped, a duplicate Name
// (including two empty ones — at most one endpoint may claim the
// unprefixed root path), a negative MaxToolRounds, or a Profile that isn't
// a real configured chat backend — checked once at save time
// (handleSettings), same reasoning as every other validate* function in
// this codebase (mssql.go/http_tool.go's validateSQLQueryTemplates/
// validateHTTPQueryTemplates): catch a mistake immediately rather than
// silently misconfiguring a surface other tools already depend on.
func validateOpenAIEndpoints(list []openAIEndpointConfig) error {
	seen := map[string]bool{}
	for i, ep := range list {
		name := strings.TrimSpace(ep.Name)
		if name != "" && !openAIEndpointNameRe.MatchString(name) {
			return fmt.Errorf("openai endpoint %d: name %q must be empty (root path) or match %s", i, ep.Name, openAIEndpointNameRe.String())
		}
		key := strings.ToLower(name)
		if seen[key] {
			if key == "" {
				return fmt.Errorf("openai endpoint %d: only one endpoint may use the empty (root) name", i)
			}
			return fmt.Errorf("openai endpoint %d: duplicate name %q", i, ep.Name)
		}
		seen[key] = true
		if ep.MaxToolRounds < 0 {
			return fmt.Errorf("openai endpoint %q: max_tool_rounds must not be negative", ep.Name)
		}
		if p := strings.TrimSpace(ep.Profile); p != "" && !isSupportedChatProfile(p) {
			return fmt.Errorf("openai endpoint %q: unknown profile %q", ep.Name, ep.Profile)
		}
	}
	return nil
}

// openAIEndpointBasePath returns the URL path prefix ep mounts at: "" for
// the unprefixed root (Name == ""), else "/"+Name.
func openAIEndpointBasePath(ep openAIEndpointConfig) string {
	name := strings.TrimSpace(ep.Name)
	if name == "" {
		return ""
	}
	return "/" + name
}

// reconcileOpenAIAPIServer starts, stops, or fully rebuilds the OpenAI-
// compatible server to match cfg — called once at startup (main.go) and
// again after every settings save (handleSettings), so turning it on/off,
// moving it to a different port, or adding/removing/reconfiguring an
// endpoint takes effect immediately, no process restart needed (the same
// "hot-reloaded on save" treatment ragSystem.setLLM already gives the
// chat/embedding backends).
//
// Unlike the single-endpoint predecessor of this function, which only
// touched the running server when the PORT changed, this always tears any
// running instance down first and (if still wanted) starts a fresh one:
// every endpoint's capability flags are baked into its route closures at
// mux-registration time, so merely changing an endpoint's EnableRAG/
// EnableTools/Profile/Preset — with the port left untouched — would
// otherwise keep serving the stale mux indefinitely. Shutdown is awaited
// synchronously (bounded by a timeout) before rebinding, so the old and new
// listener never race for the same port; settings saves are rare enough
// that the brief resulting pause in the settings-save response is an
// acceptable trade for always-correct endpoint config. A bind failure
// (port in use, no permission for a low port) is logged, not fatal — the
// rest of R3 keeps running either way.
func reconcileOpenAIAPIServer(rag *ragSystem, cfg openAIAPIConfig) {
	openAIServerMu.Lock()
	defer openAIServerMu.Unlock()

	if openAIServer != nil {
		srv := openAIServer
		openAIServer = nil
		openAIServerPort = 0
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}

	wantRunning := cfg.Enabled && cfg.Port > 0 && len(cfg.Endpoints) > 0
	if !wantRunning {
		return
	}

	if len(settings.get().API.Keys) == 0 {
		log.Printf("WARN: OpenAI-compatible API starting on :%d with no API keys configured (Settings -> \"Externe Schnittstellen\" -> API-Zugriff) — every request will be rejected with 401 until a key is created", cfg.Port)
	}

	mux := http.NewServeMux()
	enabledCount := 0
	for _, ep := range cfg.Endpoints {
		if !ep.Enabled {
			continue
		}
		enabledCount++
		base := openAIEndpointBasePath(ep)
		// ep is captured by value per iteration (Go 1.22+ loop-var
		// semantics — each closure below gets its own copy), so every
		// route's capability flags stay pinned to the endpoint that
		// registered it, never a later iteration's.
		mux.HandleFunc(base+"/v1/models", requireOpenAIAPIKey(handleOpenAIModels(ep)))
		mux.HandleFunc(base+"/v1/chat/completions", requireOpenAIAPIKey(handleOpenAIChatCompletions(rag, ep)))
	}
	if enabledCount == 0 {
		return
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Port), Handler: mux}
	openAIServer = srv
	openAIServerPort = cfg.Port
	go func() {
		log.Printf("OpenAI-compatible API listening on :%d (%d endpoint(s))", cfg.Port, enabledCount)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("WARN: OpenAI-compatible API server on :%d stopped: %v", cfg.Port, err)
			openAIServerMu.Lock()
			if openAIServer == srv {
				openAIServer = nil
				openAIServerPort = 0
			}
			openAIServerMu.Unlock()
		}
	}()
}

// stopOpenAIAPIServer shuts the server down (if running) — called on
// process shutdown alongside the scheduler/vector-store cleanup in
// main.go, so a SIGTERM/SIGINT closes it gracefully rather than just
// dying with the process.
func stopOpenAIAPIServer() {
	openAIServerMu.Lock()
	srv := openAIServer
	openAIServer = nil
	openAIServerPort = 0
	openAIServerMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
