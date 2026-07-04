package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks for hot paths: chunking, reranking, SQL/JSON helpers, usage
// aggregation, tool filtering, and the streaming XML tool-call parser.
// Run with: go test -bench=. -benchmem -run=^$
// ─────────────────────────────────────────────────────────────────────────────

func benchText(paragraphs int) string {
	var sb strings.Builder
	for i := 0; i < paragraphs; i++ {
		fmt.Fprintf(&sb, "Dies ist Absatz Nummer %d mit etwas Beispieltext, der eine realistische Laenge simuliert und mehrere Woerter enthaelt.\n", i)
	}
	return sb.String()
}

func BenchmarkChunkTextSmall(b *testing.B) {
	text := benchText(10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		chunkText(text, 800)
	}
}

func BenchmarkChunkTextLarge(b *testing.B) {
	text := benchText(500)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		chunkText(text, 800)
	}
}

func BenchmarkSanitizeTextForIngestWithPII(b *testing.B) {
	s := appSettings{RedactPII: true}
	text := benchText(50) + " Kontakt: max.mustermann@example.com, +49 170 1234567, DE89370400440532013000."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sanitizeTextForIngest(text, s)
	}
}

func benchHits(n int) []retrievalHit {
	hits := make([]retrievalHit, n)
	for i := 0; i < n; i++ {
		hits[i] = retrievalHit{
			Article:  fmt.Sprintf("article-%d", i%20),
			ChunkIdx: i,
			ChunkID:  fmt.Sprintf("c-%d", i),
			R3Score:  float64(i%100) / 100,
			Content:  "Kubernetes Deployment Rollback Anleitung mit Schritt fuer Schritt Beispielen und Konfigurationsdateien.",
		}
	}
	return hits
}

func BenchmarkLexicalOverlapScore(b *testing.B) {
	content := "Kubernetes Deployment Rollback Anleitung mit Schritt fuer Schritt Beispielen und Konfigurationsdateien fuer produktive Cluster."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lexicalOverlapScore("Kubernetes Deployment Rollback", content)
	}
}

func BenchmarkRerankLexical50(b *testing.B) {
	base := benchHits(50)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hits := make([]retrievalHit, len(base))
		copy(hits, base)
		rerankLexical("Kubernetes Deployment Rollback", hits)
	}
}

func BenchmarkRerankLexical500(b *testing.B) {
	base := benchHits(500)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hits := make([]retrievalHit, len(base))
		copy(hits, base)
		rerankLexical("Kubernetes Deployment Rollback", hits)
	}
}

func BenchmarkVecJSON(b *testing.B) {
	vec := make([]float64, 768) // typical embedding dimensionality
	for i := range vec {
		vec[i] = float64(i) / 768
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		vecJSON(vec)
	}
}

func BenchmarkEscapeSQ(b *testing.B) {
	s := "O'Brien's \"quoted\" string with a few ' apostrophes ' inside it."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		escapeSQ(s)
	}
}

func BenchmarkStableContentHash(b *testing.B) {
	text := benchText(20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		stableContentHash(text)
	}
}

func BenchmarkRoleAndACLFilterSQL(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		roleAndACLFilterSQL("it")
	}
}

func BenchmarkExtractFirstJSONValue(b *testing.B) {
	raw := "Hier ist etwas Text davor. " + `[{"tool":"wikipedia","query":"Go (Sprache)","reason":"Fakten nachschlagen"},{"tool":"calculate","query":"2+2"}]` + " und Text danach."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := extractFirstJSONValue(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilterToolsForRole(b *testing.B) {
	tools := make([]toolDef, 0, len(builtinTools)+10)
	tools = append(tools, builtinTools...)
	for i := 0; i < 10; i++ {
		tools = append(tools, toolDef{Name: fmt.Sprintf("module:custom-%d", i)})
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		filterToolsForRole(tools, "it")
	}
}

func BenchmarkXMLParseFeedPlainText(b *testing.B) {
	chunk := "Dies ist ein normaler Antworttext ohne irgendwelche Tool-Aufrufe, nur Fliesstext zum Streamen. "
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := &XMLParseState{}
		for j := 0; j < 20; j++ {
			p.Feed(chunk)
		}
	}
}

func BenchmarkXMLParseFeedWithToolCall(b *testing.B) {
	text := `Vorheriger Text. <tool name="wikipedia"><query>Go Programmiersprache</query></tool> Nachfolgender Text.`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := &XMLParseState{}
		for _, r := range text {
			p.Feed(string(r))
		}
	}
}

func BenchmarkUsageStoreSummarize1k(b *testing.B) {
	us := newUsageStore("") // memory-only, no disk I/O in the benchmark
	now := time.Now()
	roles := []string{"it", "hr", "vertrieb", "logistik"}
	for i := 0; i < 1000; i++ {
		us.record(usageRecord{
			Time: now.Add(-time.Duration(i) * time.Minute), Role: roles[i%len(roles)],
			Mode: "rag", DurationMS: int64(100 + i%50), TokensStreamed: 50 + i%20,
			ToolCalls: i % 3, Success: i%10 != 0, Tools: []string{"wikipedia"},
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		us.summarize(30)
	}
}

func BenchmarkNormalizeAPIRouteRules(b *testing.B) {
	rules := []apiRouteRule{
		{Path: "/api/ask", Enabled: false},
		{Path: "/api/custom", Enabled: true, Public: true, MatchType: "prefix"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		normalizeAPIRouteRules(rules)
	}
}

func BenchmarkPlanToolStepsParse(b *testing.B) {
	raw := `[{"tool":"wikipedia","query":"A"},{"tool":"calculate","query":"2+2"},{"tool":"duckduckgo","query":"C"}]`
	tools := []toolDef{{Name: "wikipedia"}, {Name: "calculate"}, {Name: "duckduckgo"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parsePlannedSteps(raw, tools, 3); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEngineDirectStreamedAnswer(b *testing.B) {
	response := strings.Repeat("Dies ist eine Beispielantwort ohne Tool-Aufrufe. ", 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		eng, _ := buildTestEngine(response)
		rec := &discardResponseWriter{header: make(http.Header)}
		sw := &sseWriter{w: rec, flusher: rec}
		tel := newRequestTelemetry("bench", "chat", "q")
		_, _ = eng.Run(context.Background(), EngineRequest{
			RequestID: "bench", Question: "q",
			Messages: []chatMsg{{Role: "user", Content: "q"}},
		}, sw, tel)
	}
}

// discardResponseWriter is a minimal http.ResponseWriter+http.Flusher that
// discards all output, avoiding httptest.ResponseRecorder's buffering
// overhead in the streaming benchmark above.
type discardResponseWriter struct{ header http.Header }

func (d *discardResponseWriter) Header() http.Header         { return d.header }
func (d *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardResponseWriter) WriteHeader(statusCode int)  {}
func (d *discardResponseWriter) Flush()                      {}
