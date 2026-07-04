package main

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	completeThinkBlockRE = regexp.MustCompile(`(?is)<think>.*?</think>|\[THINK\].*?\[/THINK\]`)
	openThinkTailRE      = regexp.MustCompile(`(?is)(<think>|\[THINK\]).*$`)
)

func stripInternalThinking(text string) string {
	cleaned := completeThinkBlockRE.ReplaceAllString(text, "")
	cleaned = openThinkTailRE.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func languageLabel(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "de":
		return "Deutsch"
	case "en":
		return "English"
	case "fr":
		return "Français"
	case "es":
		return "Español"
	case "it":
		return "Italiano"
	case "nl":
		return "Nederlands"
	case "pt":
		return "Português"
	case "pl":
		return "Polski"
	default:
		if code == "" {
			return "Deutsch"
		}
		return strings.ToLower(code)
	}
}

func buildAssistantPolicyPrompt(s appSettings) string {
	var sb strings.Builder
	sb.WriteString("Du bist ein praeziser RAG- und Research-Assistent fuer persoenliche und unternehmerische Nutzung.\n")
	sb.WriteString("Deine Prioritaeten sind: 1) Korrektheit, 2) klare Trennung von Fakten und Unsicherheit, 3) knappe, nuetzliche Antworten.\n")
	sb.WriteString(fmt.Sprintf("Aktive Rolle (Demo-RBAC): %s.\n", demoRoleLabel(s.ActiveRole)))
	switch s.ResponseLanguageMode {
	case "settings":
		sb.WriteString(fmt.Sprintf("Antworte durchgaengig auf %s.\n", languageLabel(s.Lang)))
	default:
		sb.WriteString(fmt.Sprintf("Antworte in der Sprache der Nutzeranfrage. Falls unklar, nutze %s.\n", languageLabel(s.Lang)))
	}
	if s.UsageProfile == "commercial" {
		sb.WriteString("Kontext: gewerbliche Nutzung in einem europaweiten Unternehmen. Priorisiere Nachvollziehbarkeit, neutrale Sprache und risikoarme Aussagen.\n")
	}
	sb.WriteString("Erfinde nichts. Wenn Informationen fehlen oder duenn belegt sind, sage das klar.\n")
	sb.WriteString("Vermeide Marketing-Sprache, Wiederholungen, Halluzinationen und unnoetige Ausschmueckungen.\n")
	sb.WriteString("Nutze interne Denkschritte nur implizit und zeige sie nicht.\n\n")
	return sb.String()
}

func buildContextPrompt(ctxText string) string {
	var sb strings.Builder
	if ctxText != "" {
		sb.WriteString("### RAG-Kontext\n")
		sb.WriteString("Hier sind relevante Informationen aus der lokalen Wissensbasis:\n")
		sb.WriteString(ctxText)
		sb.WriteString("\n\n")
		sb.WriteString("Behandle diesen Kontext als primaere Quelle. Wenn er nicht ausreicht oder zeitlich fraglich ist, nutze ein Tool.\n\n")
		sb.WriteString("Pflichtregeln: Keine uncitierte RAG-Antwort. Keine erfundenen Quellen. Keine unautorisierten Links aus eingeschraenkten Quellen.\n\n")
	} else {
		sb.WriteString("Es liegt kein hinreichender lokaler Kontext fuer diese Anfrage vor. Nutze bei Bedarf Tools.\n\n")
	}
	return sb.String()
}

func buildToolingPrompt(tools []toolDef) string {
	var sb strings.Builder
	sb.WriteString("### Tool-Nutzung\n")
	sb.WriteString("Wenn externe Informationen, Berechnungen oder Codeausfuehrung noetig sind, emittiere einen XML-Tool-Block.\n")
	sb.WriteString("Das XML-Format ist strikt und muss exakt so aussehen (ein Block pro Tool-Aufruf):\n\n")
	sb.WriteString("  <tool name=\"TOOL_NAME\"><query>INHALT</query></tool>\n\n")
	sb.WriteString("Varianten je nach Tool:\n")
	sb.WriteString("  <tool name=\"rag_knowledge\"><query>Suchbegriff</query></tool>\n")
	sb.WriteString("  <tool name=\"url_fetch\"><url>https://example.com/seite</url></tool>\n")
	sb.WriteString("  <tool name=\"nanogo\"><source>fmt.Println(2+2)</source></tool>\n\n")
	sb.WriteString("Regeln:\n")
	sb.WriteString("- Erklaere vor dem XML-Block in Klartext, welches Tool du verwendest und warum.\n")
	sb.WriteString("- Der XML-Block darf mitten in deiner Antwort erscheinen – du musst nicht warten.\n")
	sb.WriteString("- Kein Markdown-Code-Block um das XML, keine Backticks, keine zusaetzlichen Attribute.\n")
	sb.WriteString("- Maximale Anzahl Tool-Aufrufe pro Antwort: 3.\n")
	sb.WriteString("- Wenn kein Tool noetig ist, emittiere keinen XML-Block.\n")
	sb.WriteString("- Behaupte nie, ein Tool verwendet zu haben, wenn du keinen XML-Block emittiert hast.\n")
	sb.WriteString("- Wenn ein Tool fehlschlaegt, erklaere das offen ohne erfundene Daten.\n\n")

	sb.WriteString("### Verfügbare Tools:\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- **%s**: %s (Parameter: %s)\n", t.Name, t.Description, t.ParamHint))
	}
	sb.WriteString("\n")
	return sb.String()
}

func buildResponseInstructionsPrompt(deep bool, usageProfile string) string {
	var sb strings.Builder
	sb.WriteString("\n### Instruktionen:\n")
	sb.WriteString("- Beginne mit der Frage: Reicht der lokale Kontext aus, ist er unsicher oder fehlt er?\n")
	sb.WriteString("- Wenn der lokale Kontext ausreicht, antworte direkt und sage knapp, dass die Antwort auf der Wissensbasis beruht.\n")
	sb.WriteString("- Wenn Informationen fehlen oder potenziell veraltet sind, nutze ein passendes Tool via XML-Block.\n")
	sb.WriteString("- Fuer allgemeine externe Recherche bevorzuge `websearch`; fuer aktuelle Ereignisse `news`; fuer strukturierte Entitaeten `wikidata`; fuer Code-Themen `github` und `stackoverflow`; fuer Rechenlogik `calculate` oder `nanogo`.\n")
	sb.WriteString("- Um eine URL direkt abzurufen und als Plaintext zu erhalten, verwende `url_fetch` mit dem `<url>`-Element.\n")
	sb.WriteString("- Fuer interne Wissensbasis-Suche nutze `rag_knowledge`; fuer praezise Vektorsuche `vector_query` (optional: k:N threshold:F Suchbegriff).\n")
	sb.WriteString("- Fuer Filtern oder Durchsuchen der Wissensbasis nutze `sql_query` mit SELECT auf `chunks` (id, article, chunk_idx, content, embed_model, role_scope). Nur SELECT erlaubt!\n")
	sb.WriteString("- Behaupte nie mehr Sicherheit, als der Kontext hergibt.\n")
	sb.WriteString("- Erfinde keine Kontaktinformationen, URLs, APIs, Produktdetails, Roadmaps oder technische Interna.\n")
	sb.WriteString("- Wenn ein Tool benutzt wurde, liefere danach genau eine ueberarbeitete finale Antwort, nicht zwei Versionen.\n")
	sb.WriteString("- Trenne lokale Wissensbasis und externe Recherche explizit, wenn beide verwendet wurden.\n")
	sb.WriteString("- Wenn externe Recherche wenig hergibt, sage das offen statt Luecken zu fuellen.\n")
	if usageProfile == "commercial" {
		sb.WriteString("- Wenn die Wissensbasis genutzt wurde, fuege am Ende den Abschnitt `Quellenbasis` mit den verwendeten `[Quelle: ...]`-Bezeichnern an.\n")
		sb.WriteString("- Bei fehlender Beleglage liefere eine vorsichtige Empfehlung statt einer harten Zusage.\n")
	}
	if deep {
		sb.WriteString("- Im Deep-Research-Modus strukturiere die Antwort in: Kurzfazit, Befunde, Unsicherheiten, Schlussfolgerung.\n")
		sb.WriteString("- Priorisiere Genauigkeit und Einordnung vor Vollstaendigkeit.\n")
	} else {
		sb.WriteString("- Standardmodus: antworte kompakt, konkret und mit hoher Informationsdichte.\n")
	}
	return sb.String()
}

// buildToolSystemPrompt constructs the system prompt describing
// available tools and how the assistant should emit tool requests.
func buildToolSystemPrompt(ctxText string, tools []toolDef, deep bool, s appSettings) string {
	return buildAssistantPolicyPrompt(s) +
		buildContextPrompt(ctxText) +
		buildToolingPrompt(tools) +
		buildResponseInstructionsPrompt(deep, s.UsageProfile)
}

// ── Debug / Search models ─────────────────────────────────────────

// debugChunk contains information about a retrieved chunk useful for
// emitting debug payloads back to the frontend.
type debugChunk struct {
	Score         float64  `json:"score"`
	SemanticScore float64  `json:"semantic_score,omitempty"`
	Content       string   `json:"content"`
	Article       string   `json:"article"`
	ChunkIdx      int      `json:"chunk_idx"`
	R3Score       float64  `json:"r3_score,omitempty"`
	Citation      Citation `json:"citation,omitempty"`
	IsNeighbor    bool     `json:"is_neighbor"`
}

// debugInfo aggregates retrieval timing and chunk-level debug data.
type debugInfo struct {
	Chunks       []debugChunk `json:"chunks"`
	Citations    []Citation   `json:"citations,omitempty"`
	EmbedMs      int64        `json:"embed_ms"`
	SearchMs     int64        `json:"search_ms"`
	TotalChunks  int          `json:"total_chunks"`
	UsedK        int          `json:"used_k"`
	Decision     string       `json:"decision,omitempty"`
	RankingModel string       `json:"ranking_model,omitempty"`
}

// debugModels records which LLM endpoint and models were used for a request.
type debugModels struct {
	BaseURL    string `json:"base_url"`
	ChatModel  string `json:"chat_model"`
	EmbedModel string `json:"embed_model"`
}

// debugPayload is the top-level debug information emitted alongside
// SSE responses to help diagnose retrieval and model behavior.
type debugPayload struct {
	RequestID          string      `json:"request_id"`
	Mode               string      `json:"mode"`
	AutoSearch         bool        `json:"auto_search"`
	Offline            bool        `json:"offline"`
	Deep               bool        `json:"deep"`
	Question           string      `json:"question"`
	UsedK              int         `json:"used_k"`
	BaseK              int         `json:"base_k"`
	ChunkSize          int         `json:"chunk_size"`
	TotalChunks        int         `json:"total_chunks"`
	ContextChars       int         `json:"context_chars"`
	SystemPromptChars  int         `json:"system_prompt_chars"`
	HistoryMessages    int         `json:"history_messages"`
	StorageMode        string      `json:"storage_mode"`
	DBPath             string      `json:"db_path"`
	Models             debugModels `json:"models"`
	Retrieval          *debugInfo  `json:"retrieval"`
	ActiveRole         string      `json:"active_role"`
	RoleLabel          string      `json:"role_label"`
	PersonaID          string      `json:"persona_id"`
	PersonaName        string      `json:"persona_name"`
	PersonaPromptChars int         `json:"persona_prompt_chars"`
}

// prepareContext does the embedding + vector search and returns the context string and optional debug info.
// prepareContext computes embeddings for `question`, runs a vector
// search against the DB and returns the assembled context text and
// optional debug information.
func (r *ragSystem) prepareContext(question string, debug bool) (string, *debugInfo, error) {
	searchQuery := refineSearchQuery(question)
	hits, embedMs, searchMs, err := r.searchCandidates(searchQuery, r.k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
		return ctx, di, nil
	}

	// If we have a clear high-confidence hit, return context immediately.
	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.R3Score > highThreshold {
			primaryCount++
		}
	}

	// If high-confidence primary found, use those hits (top k by score)
	if primaryCount > 0 {
		var sel []retrievalHit
		for _, h := range hits {
			if h.R3Score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return r.assembleContext(sel, r.k, "high_confidence", embedMs, searchMs)
	}

	// Prepare a concise summary of top candidates to let the LM decide
	// whether more retrieval is needed.
	var summaryParts []string
	topN := 5
	if len(hits) < topN {
		topN = len(hits)
	}
	for i := 0; i < topN; i++ {
		h := hits[i]
		summaryParts = append(summaryParts, fmt.Sprintf("%s (r3=%.4f, semantic=%.4f)", h.Article, h.R3Score, h.Score))
	}
	summary := strings.Join(summaryParts, "; ")

	// Ask LM whether to answer directly or retrieve more context.
	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		var sel []retrievalHit
		thresh := r.scoreThreshold()
		for _, h := range hits {
			if h.R3Score >= thresh {
				sel = append(sel, h)
				if len(sel) >= r.k {
					break
				}
			}
		}
		return r.assembleContext(sel, r.k, "relaxed_fallback", embedMs, searchMs)
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		// Let the chat model answer without extra context.
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCountForRole(activeRole), UsedK: 0, Decision: "answer_direct"}
		return "", di, nil
	}

	// Otherwise, gather retrieval parameters and perform relaxed retrieval.
	desiredK := r.k
	if v, ok := decisionMap["k"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			desiredK = int(fv)
		}
	}
	thresh := r.scoreThreshold()
	if v, ok := decisionMap["threshold"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			thresh = fv
		}
	}
	// Optionally allow the LM to suggest a refined query
	if v, ok := decisionMap["query"]; ok {
		if qs, ok2 := v.(string); ok2 && strings.TrimSpace(qs) != "" {
			searchQuery = qs
		}
	}

	if searchQuery != refineSearchQuery(question) {
		hits, _, searchMs, err = r.searchCandidates(searchQuery, desiredK)
		if err != nil {
			return "", nil, err
		}
		if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
			di.UsedK = desiredK
			di.Decision = "article_specific_refined"
			return ctx, di, nil
		}
	}

	var sel []retrievalHit
	for _, h := range hits {
		if h.R3Score >= thresh {
			sel = append(sel, h)
			if len(sel) >= desiredK {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		// fallback to top-k by score
		for i := 0; i < desiredK && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	return r.assembleContext(sel, desiredK, "lm_requested_retrieval", embedMs, searchMs)
}

// prepareContextWithK does the same as prepareContext but allows specifying k (number of primary hits)
// prepareContextWithK behaves like prepareContext but allows specifying
// the number `k` of primary retrieval hits to consider.
func (r *ragSystem) prepareContextWithK(question string, debug bool, k int) (string, *debugInfo, error) {
	searchQuery := refineSearchQuery(question)
	hits, embedMs, searchMs, err := r.searchCandidates(searchQuery, k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
		di.UsedK = k
		return ctx, di, nil
	}

	const highThreshold = 0.90
	var primaryCount int
	for _, h := range hits {
		if h.R3Score > highThreshold {
			primaryCount++
		}
	}

	if primaryCount > 0 {
		var sel []retrievalHit
		for _, h := range hits {
			if h.R3Score > highThreshold {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return r.assembleContext(sel, k, "high_confidence", embedMs, searchMs)
	}

	// Summarize top candidates
	var summaryParts []string
	topN := 5
	if len(hits) < topN {
		topN = len(hits)
	}
	for i := 0; i < topN; i++ {
		h := hits[i]
		summaryParts = append(summaryParts, fmt.Sprintf("%s (r3=%.4f, semantic=%.4f)", h.Article, h.R3Score, h.Score))
	}
	summary := strings.Join(summaryParts, "; ")

	decisionMap, derr := r.analyzeQuestion(question, summary)
	if derr != nil {
		var sel []retrievalHit
		thresh := r.scoreThreshold()
		for _, h := range hits {
			if h.R3Score >= thresh {
				sel = append(sel, h)
				if len(sel) >= k {
					break
				}
			}
		}
		return r.assembleContext(sel, k, "relaxed_fallback", embedMs, searchMs)
	}

	action, _ := decisionMap["action"].(string)
	if strings.ToUpper(action) == "ANSWER_DIRECT" {
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		di := &debugInfo{EmbedMs: embedMs, SearchMs: searchMs, TotalChunks: r.docCountForRole(activeRole), UsedK: 0, Decision: "answer_direct"}
		return "", di, nil
	}

	desiredK := k
	if v, ok := decisionMap["k"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			desiredK = int(fv)
		}
	}
	thresh := r.scoreThreshold()
	if v, ok := decisionMap["threshold"]; ok {
		if fv, ok2 := v.(float64); ok2 {
			thresh = fv
		}
	}
	if v, ok := decisionMap["query"]; ok {
		if qs, ok2 := v.(string); ok2 && strings.TrimSpace(qs) != "" {
			searchQuery = qs
		}
	}

	if searchQuery != refineSearchQuery(question) {
		hits, _, searchMs, err = r.searchCandidates(searchQuery, desiredK)
		if err != nil {
			return "", nil, err
		}
		if ctx, di, ok := r.loadArticleContext(searchQuery, debug, embedMs); ok {
			di.UsedK = desiredK
			di.Decision = "article_specific_refined"
			return ctx, di, nil
		}
	}

	var sel []retrievalHit
	for _, h := range hits {
		if h.R3Score >= thresh {
			sel = append(sel, h)
			if len(sel) >= desiredK {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		for i := 0; i < desiredK && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	return r.assembleContext(sel, desiredK, "lm_requested_retrieval", embedMs, searchMs)
}

func (r *ragSystem) prepareDirectContext(query string, k int) (string, *debugInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", &debugInfo{UsedK: 0, Decision: "no_query"}, nil
	}
	if k <= 0 {
		k = r.k
	}
	hits, embedMs, searchMs, err := r.searchCandidates(query, k)
	if err != nil {
		return "", nil, err
	}
	if ctx, di, ok := r.loadArticleContext(query, false, embedMs); ok {
		di.UsedK = k
		di.Decision = "article_specific_direct"
		return ctx, di, nil
	}
	var sel []retrievalHit
	for _, h := range hits {
		if h.R3Score >= r.scoreThreshold() {
			sel = append(sel, h)
			if len(sel) >= k {
				break
			}
		}
	}
	if len(sel) == 0 && len(hits) > 0 {
		for i := 0; i < k && i < len(hits); i++ {
			sel = append(sel, hits[i])
		}
	}
	if len(sel) == 0 {
		activeRole := "it"
		if settings != nil {
			activeRole = settings.get().ActiveRole
		}
		return "", &debugInfo{
			EmbedMs:     embedMs,
			SearchMs:    searchMs,
			TotalChunks: r.docCountForRole(activeRole),
			UsedK:       k,
			Decision:    "no_hits",
		}, nil
	}
	return r.assembleContext(sel, k, "direct_query", embedMs, searchMs)
}
