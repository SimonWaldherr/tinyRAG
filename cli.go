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
//
//   Interactive REPL:
//     tinyRAG -web=false
//     Commands: /help /search /add /url /sources /count /role /stats /quit
//
// Colors honour the NO_COLOR convention and the -nocolor flag.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

// cliAskSystemPrompt is the system prompt for CLI single-turn answers.
const cliAskSystemPrompt = "Du bist ein hilfreicher Assistent. Beantworte Fragen basierend auf dem bereitgestellten Kontext. Wenn der Kontext die Antwort nicht enthält, sage das ehrlich."

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

// printCLIHelp lists the REPL commands.
func printCLIHelp(p cliPalette) {
	fmt.Println(p.accent("Befehle:"))
	fmt.Println("  /search <query>   semantische Suche in der Wissensbasis")
	fmt.Println("  /add <Artikel>    Wikipedia-Artikel ingesten")
	fmt.Println("  /url <https://…>  Webseite laden und ingesten")
	fmt.Println("  /sources          gespeicherte Quellen auflisten")
	fmt.Println("  /count            Anzahl gespeicherter Chunks")
	fmt.Println("  /role [rolle]     aktive Rolle anzeigen/setzen (it|logistik|vertrieb|hr)")
	fmt.Println("  /stats            Nutzungsstatistik (30 Tage)")
	fmt.Println("  /help             diese Hilfe")
	fmt.Println("  /quit             beenden")
	fmt.Println(p.dim("Alles andere wird als Frage an die Wissensbasis interpretiert."))
}

// runCLI starts the interactive REPL.
func runCLI(rag *ragSystem, p cliPalette) {
	fmt.Println(p.accent("tinyRAG CLI") + p.dim(" — /help für Befehle"))
	fmt.Println()

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
			// Single-turn ask: top-k context + streamed answer.
			ctxText, _, err := rag.prepareContext(line, false)
			if err != nil {
				fmt.Println(p.fail(fmt.Sprintf("Fehler: %v", err)))
				continue
			}
			msgs := []chatMsg{{Role: "user", Content: fmt.Sprintf("Kontext:\n%s\n\nFrage: %s", ctxText, line)}}
			fmt.Print("\n" + p.ok(">> "))
			_ = rag.getLM().chatStream(context.Background(), cliAskSystemPrompt, msgs, os.Stdout)
			fmt.Println()
		}
		fmt.Println()
	}
}
