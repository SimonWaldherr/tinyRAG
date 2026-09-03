package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetAgentAudit clears the in-memory audit ring between tests.
func resetAgentAudit() {
	agentAuditMu.Lock()
	agentAudit = nil
	agentAuditMu.Unlock()
}

func toolNames(tools []toolDef) map[string]bool {
	out := map[string]bool{}
	for _, td := range tools {
		out[td.Function.Name] = true
	}
	return out
}

func TestBuildAgentToolsGating(t *testing.T) {
	rag, s := newTestRAG(t)
	sess := agentSession{User: "test@rubix.com", DeptCode: "", IsAdmin: false}

	// Baseline: only the three knowledge-base tools.
	tools, _ := buildAgentTools(rag, s, sess)
	names := toolNames(tools)
	for _, want := range []string{"search_knowledge_base", "get_source_content", "list_sources"} {
		if !names[want] {
			t.Errorf("want baseline tool %q, got %v", want, names)
		}
	}
	if names["draft_new_mail"] || names["save_draft_to_mailbox"] || names[runCodeToolName] || names[fetchURLToolName] {
		t.Fatalf("gated tools must be absent by default, got %v", names)
	}

	// Draft replies on: draft_new_mail appears; the mailbox write still
	// needs IMAP + admin.
	s.EnableDraftReplies = true
	names = toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if !names["draft_new_mail"] || names["save_draft_to_mailbox"] {
		t.Fatalf("want draft_new_mail only, got %v", names)
	}

	s.IMAP = []mailboxConfig{{Enabled: true}}
	names = toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if names["save_draft_to_mailbox"] {
		t.Fatalf("non-admin session must not get save_draft_to_mailbox, got %v", names)
	}
	sess.IsAdmin = true
	names = toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if !names["save_draft_to_mailbox"] {
		t.Fatalf("admin + IMAP + drafts enabled should expose save_draft_to_mailbox, got %v", names)
	}

	// run_code: setting alone is NOT enough — a compiled-in sandbox is
	// required too (defense in depth).
	s.Agent.AllowCodeExecution = true
	names = toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if names[runCodeToolName] {
		t.Fatalf("run_code must stay hidden with no sandbox in the build, got %v", names)
	}
	activeCodeSandbox = fakeSandbox{}
	t.Cleanup(func() { activeCodeSandbox = nil })
	names = toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if !names[runCodeToolName] {
		t.Fatalf("run_code should appear once sandbox AND setting are present, got %v", names)
	}
}

func firstOf(tools []toolDef, _ map[string]toolExecutor) []toolDef { return tools }

// TestFetchURLToolGatingAndExecution covers docs/AGENT_PLAN.md's Phase 2
// stage 1: fetch_url must stay hidden until agent.allow_web_fetch is on,
// and once on, must actually return the fetched page's text wrapped in
// the "this is data, not instructions" note (prompt-injection discipline).
func TestFetchURLToolGatingAndExecution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Testseite</title></head><body><p>Hallo von der Testseite.</p></body></html>`))
	}))
	defer server.Close()
	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	rag, s := newTestRAG(t)
	sess := agentSession{User: "test@rubix.com"}

	names := toolNames(firstOf(buildAgentTools(rag, s, sess)))
	if names[fetchURLToolName] {
		t.Fatalf("fetch_url must be hidden with agent.allow_web_fetch off, got %v", names)
	}

	s.Agent.AllowWebFetch = true
	_, execs := buildAgentTools(rag, s, sess)
	if _, ok := execs[fetchURLToolName]; !ok {
		t.Fatalf("fetch_url should be present once agent.allow_web_fetch is on")
	}
	args, _ := json.Marshal(map[string]string{"url": server.URL})
	out, err := execs[fetchURLToolName](context.Background(), string(args))
	if err != nil {
		t.Fatalf("fetch_url execution: %v", err)
	}
	if !strings.Contains(out, "Hallo von der Testseite") {
		t.Errorf("want fetched page text in result, got %q", out)
	}
	if !strings.Contains(out, "keine Anweisung") {
		t.Errorf("want the prompt-injection data/instruction note in result, got %q", out)
	}
}

// TestWebResearchToolBlocksNestedRecursion mirrors delegate_subtasks'
// recursion guard: a context already marked as running inside a sub-agent
// must refuse to start web_research, without making any HTTP call at all.
func TestWebResearchToolBlocksNestedRecursion(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Agent.AllowWebFetch = true
	s.Agent.AllowWebResearch = true

	exec := webResearchToolExecutor(rag, s)
	ctx := context.WithValue(context.Background(), subAgentDepthKey{}, true)
	args, _ := json.Marshal(map[string]string{"goal": "irrelevant", "start_url": "http://should-never-be-fetched.invalid/"})
	if _, err := exec(ctx, string(args)); err == nil {
		t.Fatalf("want an error when already nested inside a sub-agent, got nil")
	}
}

// TestWebResearchToolFollowsLinksWithinBudget exercises the whole nested
// loop end to end: a scripted chat backend requests the research fetch
// tool for a seed page, then for a link discovered on that page, then
// answers directly — the tool result returned to the (fake) parent agent
// must be the synthesized final answer, not raw page HTML/text.
func TestWebResearchToolFollowsLinksWithinBudget(t *testing.T) {
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/seed":
			_, _ = w.Write([]byte(`<html><head><title>Seed</title></head><body><p>Start page.</p><a href="/next">Next</a></body></html>`))
		case "/next":
			_, _ = w.Write([]byte(`<html><head><title>Next</title></head><body><p>ANSWER-FOUND: the secret number is 42.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer pages.Close()
	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	var chatCalls int
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case chatCalls == 1:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": %q, "arguments": %q}}
			]}}]}`, researchFetchToolName, fmt.Sprintf(`{"url":%q}`, pages.URL+"/seed"))
		case chatCalls == 2:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_2", "type": "function", "function": {"name": %q, "arguments": %q}}
			]}}]}`, researchFetchToolName, fmt.Sprintf(`{"url":%q}`, pages.URL+"/next"))
		default:
			// The model decides on its own to stop calling tools and answer
			// directly — this is the normal non-streaming tool-decision
			// round returning plain content, same as
			// TestChatWithToolsDirectAnswerSkipsToolCall, not the
			// budget-exhaustion forced-stream path (which isn't exercised
			// here since the model stops well within its round budget).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Der gesuchte Wert ist 42."}}]}`))
		}
	}))
	defer chat.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", chat.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Agent.AllowWebFetch = true
	s.Agent.AllowWebResearch = true

	exec := webResearchToolExecutor(rag, s)
	args, _ := json.Marshal(map[string]string{"goal": "Finde die geheime Zahl.", "start_url": pages.URL + "/seed"})
	out, err := exec(context.Background(), string(args))
	if err != nil {
		t.Fatalf("web_research execution: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("want the synthesized answer to mention the found value, got %q", out)
	}
	if strings.Contains(out, "<html>") || strings.Contains(out, "ANSWER-FOUND") {
		t.Fatalf("want a synthesized summary, not raw page content, got %q", out)
	}
	if chatCalls < 3 {
		t.Fatalf("want at least 2 fetch rounds + 1 final-answer round, got %d chat calls", chatCalls)
	}
}

// TestWebResearchNestedStepsParentUnderOwnToolCall confirms web_research's
// ParentID threading (currentStepIDFromContext, llm.go/agent.go): unlike
// delegate_subtasks (which emits its own subagent_start/end and hands that
// ID straight to withSubAgentLabel), web_research rides runToolCalls'
// generic tool_start/tool_end wrapping for the "web_research" call itself —
// its nested fetch_page_with_links calls must carry THAT tool_start's ID as
// their ParentID, not the empty top-level ParentID, or a graphical
// hierarchy view would show them as siblings of web_research instead of
// its children.
func TestWebResearchNestedStepsParentUnderOwnToolCall(t *testing.T) {
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/seed":
			_, _ = w.Write([]byte(`<html><head><title>Seed</title></head><body><p>Start page.</p><a href="/next">Next</a></body></html>`))
		case "/next":
			_, _ = w.Write([]byte(`<html><head><title>Next</title></head><body><p>ANSWER-FOUND: the secret number is 42.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer pages.Close()
	webAllowPrivateHosts = true
	t.Cleanup(func() { webAllowPrivateHosts = false })

	var chatCalls int
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		switch chatCalls {
		case 1:
			// Top-level model decides to call web_research.
			args, _ := json.Marshal(map[string]string{"goal": "Finde die geheime Zahl.", "start_url": pages.URL + "/seed"})
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_0", "type": "function", "function": {"name": "web_research", "arguments": %q}}
			]}}]}`, string(args))
		case 2:
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": %q, "arguments": %q}}
			]}}]}`, researchFetchToolName, fmt.Sprintf(`{"url":%q}`, pages.URL+"/seed"))
		case 3:
			fmt.Fprintf(w, `{"choices": [{"message": {"role": "assistant", "tool_calls": [
				{"id": "call_2", "type": "function", "function": {"name": %q, "arguments": %q}}
			]}}]}`, researchFetchToolName, fmt.Sprintf(`{"url":%q}`, pages.URL+"/next"))
		case 4:
			// web_research's own inner loop settles on a final answer.
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Der gesuchte Wert ist 42."}}]}`))
		default:
			// Top-level model's own final answer after web_research returns.
			_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "Antwort: 42."}}]}`))
		}
	}))
	defer chat.Close()

	rag, s := newTestRAG(t)
	chatClient := newLMClientFull("local", chat.URL, "", "test-embed", "test-chat", "")
	rag.setLLM(rag.getEmbedLM(), map[string]*lmClient{"local": chatClient}, "local")
	s.Agent.AllowWebFetch = true
	s.Agent.AllowWebResearch = true

	var steps []agentStep
	ctx := withAgentProgress(context.Background(), func(st agentStep) {
		steps = append(steps, st)
	})
	tools := []toolDef{webResearchToolDef()}
	executors := map[string]toolExecutor{"web_research": webResearchToolExecutor(rag, s)}

	var out strings.Builder
	if err := chatClient.chatWithToolsBudget(ctx, "system", []chatMsg{{Role: "user", Content: "Finde die geheime Zahl."}}, tools, executors, &out, 6); err != nil {
		t.Fatalf("chatWithToolsBudget: %v", err)
	}

	var webResearchStepID string
	var nestedParentIDs []string
	for _, st := range steps {
		if st.Tool == "web_research" {
			if webResearchStepID == "" {
				webResearchStepID = st.ID
			} else if st.ID != webResearchStepID {
				t.Errorf("web_research start/end must share one ID, got %q then %q", webResearchStepID, st.ID)
			}
		}
		if st.Tool == researchFetchToolName {
			nestedParentIDs = append(nestedParentIDs, st.ParentID)
		}
	}
	if webResearchStepID == "" {
		t.Fatal("want a recorded step for the web_research tool call itself")
	}
	if len(nestedParentIDs) == 0 {
		t.Fatal("want at least one recorded fetch_page_with_links step")
	}
	for _, pid := range nestedParentIDs {
		if pid != webResearchStepID {
			t.Errorf("want every fetch_page_with_links step's ParentID to be web_research's own step ID %q, got %q", webResearchStepID, pid)
		}
	}
}

type fakeSandbox struct{}

func (fakeSandbox) Run(ctx context.Context, code string) (string, error) {
	return "out: " + code, nil
}

func TestAgentGetSourceContentRespectsSourceAccess(t *testing.T) {
	rag, s := ingestFamilyRAG(t)
	s.SourceAccess = map[string][]string{"pst_attachment": {"Vertrieb"}}

	call := func(deptCode, sourceID string) (string, error) {
		_, execs := buildAgentTools(rag, s, agentSession{User: "t", DeptCode: deptCode})
		args, _ := json.Marshal(map[string]string{"source_id": sourceID})
		return execs["get_source_content"](context.Background(), string(args))
	}

	attachmentID := "pst:m.pst:Inbox:42:attachment:0:Angebot.pdf"
	if _, err := call("", attachmentID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("anonymous caller must get a not-found (no oracle) for a restricted kind, got err=%v", err)
	}
	content, err := call("Vertrieb", attachmentID)
	if err != nil {
		t.Fatalf("allowed department: %v", err)
	}
	if !strings.Contains(content, "ATTACHMENT-PDF-TEXT") {
		t.Fatalf("want attachment content, got %q", content)
	}
}

func TestAgentSearchAndListTools(t *testing.T) {
	resetAgentAudit()
	rag, s := ingestFamilyRAG(t)
	// The fixture embedded with model "test-embed"; the search tool uses
	// s.activeEmbedModel(), so the test settings must agree or the
	// embed_model filter in vectorCandidates hides every chunk.
	s.Profiles.Local.EmbedModel = "test-embed"
	_, execs := buildAgentTools(rag, s, agentSession{User: "test@rubix.com"})

	out, err := execs["search_knowledge_base"](context.Background(), `{"query": "Angebot offer document", "k": 3}`)
	if err != nil {
		t.Fatalf("search_knowledge_base: %v", err)
	}
	if !strings.Contains(out, "source_id:") {
		t.Fatalf("want formatted hits with source_id, got %q", out)
	}

	out, err = execs["list_sources"](context.Background(), `{"extension": "pdf"}`)
	if err != nil {
		t.Fatalf("list_sources: %v", err)
	}
	if !strings.Contains(out, "Angebot.pdf") || strings.Contains(out, "Unrelated.txt") {
		t.Fatalf("extension filter wrong, got %q", out)
	}

	// Both calls must have landed in the audit log, newest first.
	audit := agentAuditSnapshot()
	if len(audit) != 2 {
		t.Fatalf("want 2 audit entries, got %d", len(audit))
	}
	if audit[0].Tool != "list_sources" || audit[1].Tool != "search_knowledge_base" {
		t.Fatalf("want newest-first audit order, got %+v", audit)
	}
	if !audit[0].OK || audit[0].User != "test@rubix.com" || audit[0].DurationMS < 0 {
		t.Fatalf("audit entry incomplete: %+v", audit[0])
	}
}

func TestAgentMaxRounds(t *testing.T) {
	if got := agentMaxRounds(agentConfig{}); got != agentDefaultMaxRounds {
		t.Fatalf("default rounds: want %d, got %d", agentDefaultMaxRounds, got)
	}
	if got := agentMaxRounds(agentConfig{MaxToolRounds: 3}); got != 3 {
		t.Fatalf("configured rounds: want 3, got %d", got)
	}
}

func TestRunCodeExecutorViaFakeSandbox(t *testing.T) {
	exec := runCodeExecutor(fakeSandbox{}, agentSourceContentChars)
	out, err := exec(context.Background(), `{"code": "fmt.Println(1+1)"}`)
	if err != nil {
		t.Fatalf("runCodeExecutor: %v", err)
	}
	if out != "out: fmt.Println(1+1)" {
		t.Fatalf("unexpected sandbox output %q", out)
	}
	if _, err := exec(context.Background(), `{"code": "  "}`); err == nil {
		t.Fatalf("empty code must be rejected")
	}
}
