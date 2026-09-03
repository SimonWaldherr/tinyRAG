package main

// ─────────────────────────────────────────────────────────────────────────────
// CLI / TUI mode
//
// tinyRAG doubles as a headless RAG engine. Two entry points:
//
//   One-shot (scripting/pipelines):
//     tinyRAG -web=false -ask "Wie funktioniert X?"          → answer to stdout
//     tinyRAG -web=false -ask "…" -jsonout                    → JSON envelope
//     tinyRAG -web=false -searchq "begriff" [-jsonout]        → top-k matches
//     tinyRAG -list-themes | -list-templates                  → provisioning helpers
//     tinyRAG -apply-template support-widget                  → apply a scenario headlessly
//
//   Interactive REPL (multi-turn — keeps conversation history like the web
//   chat, up to cliHistoryLimit messages):
//     tinyRAG -web=false
//     Commands: /help /search /add /url /sources /count /role /persona
//               /model /export /new /stats /quit
//
// Colors honour the NO_COLOR convention and the -nocolor flag.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// cliPalette renders ANSI colors when enabled.
type cliPalette struct{ on bool }

func (p cliPalette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p cliPalette) accent(s string) string { return p.wrap("36", s) } // cyan
func (p cliPalette) ok(s string) string     { return p.wrap("32", s) } // green
func (p cliPalette) warn(s string) string   { return p.wrap("33", s) } // yellow
func (p cliPalette) fail(s string) string   { return p.wrap("31", s) } // red
func (p cliPalette) dim(s string) string    { return p.wrap("2", s) }  // faint

// newCLIPalette decides whether colors are enabled.
func newCLIPalette(noColorFlag bool) cliPalette {
	if noColorFlag || os.Getenv("NO_COLOR") != "" {
		return cliPalette{on: false}
	}
	return cliPalette{on: true}
}

// cliAskSystemPrompt is the base system prompt for CLI answers.
const cliAskSystemPrompt = "Du bist ein hilfreicher Assistent. Beantworte Fragen basierend auf dem bereitgestellten Kontext. Wenn der Kontext die Antwort nicht enthält, sage das ehrlich."

// cliHistoryLimit caps how many prior messages are kept as conversation
// context, mirroring the web chat's history window (see /api/ask).
const cliHistoryLimit = 10

// runOneShotAsk answers a single question and exits. With jsonOut the full
// answer is captured and emitted as a JSON envelope for scripting.
func runOneShotAsk(rag *ragSystem, question string, jsonOut bool) int {
	ctxText, di, err := rag.prepareContext(question, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	msgs := []chatMsg{{Role: "user", Content: fmt.Sprintf("Kontext:\n%s\n\nFrage: %s", ctxText, question)}}
	if jsonOut {
		var buf strings.Builder
		if err := rag.getLM().chatStream(context.Background(), cliAskSystemPrompt, msgs, &buf); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		out := map[string]any{
			"question":      question,
			"answer":        strings.TrimSpace(stripInternalThinking(buf.String())),
			"context_chars": len(ctxText),
		}
		if di != nil {
			out["chunks_used"] = len(di.Chunks)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if err := rag.getLM().chatStream(context.Background(), cliAskSystemPrompt, msgs, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		return 1
	}
	fmt.Println()
	return 0
}

// runOneShotSearch prints the top-k semantic matches for a query and exits.
func runOneShotSearch(rag *ragSystem, query string, jsonOut bool) int {
	k := 5
	if settings != nil {
		k = settings.get().K
	}
	results, err := rag.searchJSON(query, k)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{"query": query, "results": results}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	for i, r := range results {
		fmt.Printf("%d. [%.4f] %s\n\n", i+1, r.Score, r.Content)
	}
	return 0
}

// runListThemes prints available theme ids (builtin + custom) as JSON, for
// provisioning scripts that want to validate a theme id before applying it.
func runListThemes() int {
	s := settings.get()
	all := append([]map[string]string{}, builtinThemes...)
	for _, t := range s.CustomThemes {
		all = append(all, map[string]string{"id": t.ID, "label": t.Label})
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	fmt.Println(string(b))
	return 0
}

// runListTemplates prints the built-in scenario templates as JSON.
func runListTemplates() int {
	b, _ := json.MarshalIndent(scenarioTemplates(), "", "  ")
	fmt.Println(string(b))
	return 0
}

// runApplyTemplate applies a scenario template's theme/density/UI config to
// the persisted settings and exits — useful for provisioning a deployment
// (e.g. a Docker entrypoint) without going through the web UI.
func runApplyTemplate(id string) int {
	tmpl, ok := findScenarioTemplate(strings.TrimSpace(id))
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: unknown template id %q\n", id)
		return 1
	}
	settings.mu.Lock()
	themeValid := isBuiltinTheme(tmpl.Theme)
	if !themeValid {
		for _, t := range settings.s.CustomThemes {
			if t.ID == tmpl.Theme {
				themeValid = true
				break
			}
		}
	}
	if themeValid {
		settings.s.Theme = tmpl.Theme
	}
	if tmpl.Density != "" {
		settings.s.Density = normalizeDensity(tmpl.Density)
	}
	settings.s.UI = normalizeUIConfig(tmpl.Config)
	err := settings.saveLocked()
	settings.mu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to save settings: %v\n", err)
		return 1
	}
	fmt.Printf("Applied template %q (theme=%s, density=%s)\n", tmpl.ID, tmpl.Theme, normalizeDensity(tmpl.Density))
	return 0
}

// printCLIHelp lists the REPL commands.
func printCLIHelp(p cliPalette) {
	fmt.Println(p.accent("Befehle:"))
	fmt.Println("  /search <query>     semantische Suche in der Wissensbasis")
	fmt.Println("  /add <Artikel>      Wikipedia-Artikel ingesten")
	fmt.Println("  /url <https://…>    Webseite laden und ingesten")
	fmt.Println("  /sources            gespeicherte Quellen auflisten")
	fmt.Println("  /count              Anzahl gespeicherter Chunks")
	fmt.Println("  /role [rolle]       aktive Rolle anzeigen/setzen (it|logistik|vertrieb|hr)")
	fmt.Println("  /persona [id]       Personas auflisten oder für diese Session setzen")
	fmt.Println("  /model [name]       aktuelles Chat-Modell anzeigen oder wechseln")
	fmt.Println("  /export <md|html> [datei]   Sitzungsverlauf exportieren (stdout ohne Datei)")
	fmt.Println("  /new                Konversationsverlauf dieser Session zurücksetzen")
	fmt.Println("  /stats              Nutzungsstatistik (30 Tage)")
	fmt.Println("  /help               diese Hilfe")
	fmt.Println("  /quit               beenden")
	fmt.Println(p.dim("Alles andere wird als Frage an die Wissensbasis interpretiert (mit Gedächtnis über die Session)."))
}

// cliSession holds the state of one interactive REPL run: the running
// conversation history (so follow-up questions like "und was noch?" work)
// and the currently selected persona.
type cliSession struct {
	history   []chatMessage
	personaID string
}

// systemPrompt returns the base system prompt, optionally prefixed with the
// active persona's prompt (mirrors buildToolSystemPrompt's persona handling
// in the web chat path, without the tool-calling machinery).
func (cs *cliSession) systemPrompt(personas *personaStore) string {
	if cs.personaID == "" || personas == nil {
		return cliAskSystemPrompt
	}
	if persona, ok := personas.get(cs.personaID); ok && persona.Prompt != "" {
		return persona.Prompt + "\n\n" + cliAskSystemPrompt
	}
	return cliAskSystemPrompt
}

// recentHistory returns up to cliHistoryLimit prior messages as chatMsg,
// suitable for passing to chatStream alongside the current turn.
func (cs *cliSession) recentHistory() []chatMsg {
	start := 0
	if len(cs.history) > cliHistoryLimit {
		start = len(cs.history) - cliHistoryLimit
	}
	out := make([]chatMsg, 0, len(cs.history)-start)
	for _, m := range cs.history[start:] {
		out = append(out, chatMsg{Role: m.Role, Content: m.Content})
	}
	return out
}

// asConversation renders the session history through the same export
// pipeline used by the web UI's chat export (see chat_export.go), so /export
// produces identical Markdown/HTML formatting from the CLI.
func (cs *cliSession) asConversation() *conversation {
	return &conversation{ID: "cli-session", Title: "tinyRAG CLI Session", Messages: cs.history}
}

// runCLI starts the interactive, multi-turn REPL.
func runCLI(rag *ragSystem, personas *personaStore, p cliPalette) {
	fmt.Println(p.accent("tinyRAG CLI") + p.dim(" — /help für Befehle"))
	fmt.Println()

	session := &cliSession{}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(p.accent("tinyRAG> "))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s := settings.get()

		switch {
		case line == "/quit" || line == "/exit":
			fmt.Println(p.dim("Bye!"))
			return

		case line == "/help":
			printCLIHelp(p)

		case line == "/new":
			session.history = nil
			fmt.Println(p.dim("Konversationsverlauf zurückgesetzt."))

		case line == "/count":
			fmt.Printf("%s chunks\n", p.ok(fmt.Sprintf("%d", rag.docCount())))

		case line == "/sources":
			srcs := rag.listSourcesForRole(s.ActiveRole)
			if len(srcs) == 0 {
				fmt.Println(p.dim("Keine Quellen gespeichert."))
			}
			for _, src := range srcs {
				fmt.Printf("  %v %s\n", src["article"], p.dim(fmt.Sprintf("(%v chunks)", src["chunks"])))
			}

		case line == "/stats":
			sum := usageStats.summarize(30)
			fmt.Printf("Anfragen: %s | Erfolg: %s | Ø Dauer: %s | Tokens: %s | Tools: %s\n",
				p.ok(fmt.Sprintf("%d", sum.TotalRequests)),
				p.ok(fmt.Sprintf("%.0f%%", sum.SuccessRate*100)),
				p.ok(fmt.Sprintf("%d ms", sum.AvgDurationMS)),
				p.ok(fmt.Sprintf("%d", sum.TotalTokens)),
				p.ok(fmt.Sprintf("%d", sum.TotalToolCalls)))

		case line == "/role":
			fmt.Printf("Aktive Rolle: %s\n", p.ok(demoRoleLabel(s.ActiveRole)))

		case strings.HasPrefix(line, "/role "):
			role := normalizeDemoRole(strings.TrimPrefix(line, "/role "))
			settings.mu.Lock()
			settings.s.ActiveRole = role
			_ = settings.saveLocked()
			settings.mu.Unlock()
			fmt.Printf("Aktive Rolle: %s\n", p.ok(demoRoleLabel(role)))

		case line == "/persona":
			if personas == nil {
				fmt.Println(p.dim("Keine Personas verfügbar."))
				continue
			}
			for _, persona := range personas.list() {
				marker := "  "
				if persona.ID == session.personaID || (session.personaID == "" && persona.ID == personas.defaultID()) {
					marker = p.ok("→ ")
				}
				fmt.Printf("%s%s %s\n", marker, persona.ID, p.dim(persona.Name))
			}

		case strings.HasPrefix(line, "/persona "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/persona "))
			if personas == nil {
				fmt.Println(p.fail("Keine Personas verfügbar."))
				continue
			}
			if _, ok := personas.get(id); !ok {
				fmt.Println(p.fail(fmt.Sprintf("Unbekannte Persona: %q", id)))
				continue
			}
			session.personaID = id
			fmt.Printf("Persona gesetzt: %s\n", p.ok(id))

		case line == "/model":
			fmt.Printf("Chat-Modell: %s\n", p.ok(s.ChatModel))

		case strings.HasPrefix(line, "/model "):
			newModel := strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			if newModel == "" {
				fmt.Println(p.fail("Modellname fehlt."))
				continue
			}
			settings.mu.Lock()
			settings.s.ChatModel = newModel
			_ = settings.saveLocked()
			applied := settings.s
			settings.mu.Unlock()
			key := applied.OpenAIKey
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			chatLM := newLMClientWithAPI(applied.ChatBase, applied.EmbedModel, applied.ChatModel, key, applied.InferenceAPI)
			embedLM := newLMClientWithAPI(applied.EmbedBase, applied.EmbedModel, applied.ChatModel, key, applied.InferenceAPI)
			var provider lmProvider = chatLM
			if applied.ChatBase != applied.EmbedBase {
				provider = &compositeLM{embedClient: embedLM, chatClient: chatLM}
			}
			rag.setLM(provider)
			fmt.Printf("Chat-Modell gewechselt zu: %s\n", p.ok(newModel))

		case strings.HasPrefix(line, "/export"):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "/export"))
			parts := strings.Fields(rest)
			if len(parts) == 0 {
				fmt.Println(p.fail("Format fehlt: /export <md|html> [datei]"))
				continue
			}
			format := strings.ToLower(parts[0])
			appName := s.AppName
			if appName == "" {
				appName = "tinyRAG"
			}
			conv := session.asConversation()
			var content string
			switch format {
			case "html":
				content = conv.exportHTML(appName)
			default:
				content = conv.exportMarkdown(appName)
			}
			if len(parts) >= 2 {
				filename := parts[1]
				if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
					fmt.Println(p.fail(fmt.Sprintf("Fehler beim Schreiben: %v", err)))
					continue
				}
				fmt.Printf("Exportiert nach %s\n", p.ok(filename))
			} else {
				fmt.Println(content)
			}

		case strings.HasPrefix(line, "/add "):
			art := strings.TrimSpace(strings.TrimPrefix(line, "/add "))
			fmt.Printf("Lade %s…\n", p.accent(art))
			text, err := fetchWikipedia(art, s.Lang)
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			chunks, _ := chunksForIngest(text, s)
			fmt.Printf("  %d Zeichen → %d Chunks\n", len(text), len(chunks))
			if err := rag.addChunks(art, chunks, s.EmbedModel); err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			fmt.Printf("Gesamt: %s Chunks\n", p.ok(fmt.Sprintf("%d", rag.docCount())))

		case strings.HasPrefix(line, "/url "):
			u := strings.TrimSpace(strings.TrimPrefix(line, "/url "))
			fmt.Printf("Lade %s…\n", p.accent(u))
			text, err := fetchURL(u)
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			chunks, _ := chunksForIngest(text, s)
			fmt.Printf("  %d Zeichen → %d Chunks\n", len(text), len(chunks))
			if err := rag.addChunks(u, chunks, s.EmbedModel); err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			fmt.Printf("Gesamt: %s Chunks\n", p.ok(fmt.Sprintf("%d", rag.docCount())))

		case strings.HasPrefix(line, "/search "):
			query := strings.TrimSpace(strings.TrimPrefix(line, "/search "))
			results, err := rag.searchJSON(query, s.K)
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			if len(results) == 0 {
				fmt.Println(p.dim("Keine Treffer."))
			}
			for i, r := range results {
				fmt.Printf("%s %s\n%s\n\n",
					p.accent(fmt.Sprintf("%d.", i+1)),
					p.dim(fmt.Sprintf("[score %.4f]", r.Score)),
					r.Content)
			}

		default:
			// Multi-turn ask: prior turns + fresh RAG context for this question.
			priorHistory := append([]chatMessage(nil), session.history...)
			now := time.Now().Format(time.RFC3339)
			session.history = append(session.history, chatMessage{Role: "user", Content: line, Time: now})

			retrievalQuestion, _ := rewriteRetrievalQuery(context.Background(), rag.getLM(), line, priorHistory)
			ctxText, _, err := rag.prepareContext(retrievalQuestion, false)
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			history := session.recentHistory()
			// Drop the just-appended raw question from history and replace
			// it with a context-augmented version for this turn only; prior
			// turns stay as originally exchanged.
			if len(history) > 0 {
				history = history[:len(history)-1]
			}
			msgs := append(history, chatMsg{Role: "user", Content: fmt.Sprintf("Kontext:\n%s\n\nFrage: %s", ctxText, line)})

			fmt.Print("\n" + p.ok(">> "))
			var answer strings.Builder
			out := io.MultiWriter(os.Stdout, &answer)
			err = rag.getLM().chatStream(context.Background(), session.systemPrompt(personas), msgs, out)
			fmt.Println()
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			session.history = append(session.history, chatMessage{
				Role: "assistant", Content: strings.TrimSpace(stripInternalThinking(answer.String())),
				Time: time.Now().Format(time.RFC3339), Model: s.ChatModel,
			})
		}
		fmt.Println()
	}
}
