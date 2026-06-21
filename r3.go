package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// tinyRAG R³ domain model
// ─────────────────────────────────────────────────────────────────────────────

type RetrievalUnit struct {
	ChunkID         string    `json:"chunk_id"`
	DocumentID      string    `json:"document_id"`
	ChunkIdx        int       `json:"chunk_idx"`
	Content         string    `json:"content"`
	SourceSystem    string    `json:"source_system"`
	SourceType      string    `json:"source_type"`
	SourceTitle     string    `json:"source_title"`
	SourceURL       string    `json:"source_url"`
	SourceObjectID  string    `json:"source_object_id"`
	SourceVersion   string    `json:"source_version"`
	RoleScope       string    `json:"role_scope"`
	ACLGroups       string    `json:"acl_groups"`
	BusinessOwner   string    `json:"business_owner"`
	Sensitivity     string    `json:"sensitivity"`
	TrustLevel      float64   `json:"trust_level"`
	SourceQuality   float64   `json:"source_quality"`
	FreshnessScore  float64   `json:"freshness_score"`
	QualityScore    float64   `json:"quality_score"`
	FeedbackScore   float64   `json:"feedback_score"`
	ImportedAt      time.Time `json:"imported_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ContentHash     string    `json:"content_hash"`
	OpenLinkAllowed bool      `json:"open_link_allowed"`
}

type SourceRegistryRecord struct {
	DocumentID      string `json:"document_id"`
	Provenance      string `json:"provenance"`
	Ownership       string `json:"ownership"`
	TrustTier       string `json:"trust_tier"`
	Lifecycle       string `json:"lifecycle"`
	RetentionPolicy string `json:"retention_policy"`
	ACLMetadata     string `json:"acl_metadata"`
}

type R3IngestMetadata struct {
	DocumentID         string    `json:"document_id,omitempty"`
	SourceSystem       string    `json:"source_system,omitempty"`
	SourceType         string    `json:"source_type,omitempty"`
	SourceTitle        string    `json:"source_title,omitempty"`
	SourceURL          string    `json:"source_url,omitempty"`
	SourceObjectID     string    `json:"source_object_id,omitempty"`
	SourceVersion      string    `json:"source_version,omitempty"`
	ACLGroups          string    `json:"acl_groups,omitempty"`
	BusinessOwner      string    `json:"business_owner,omitempty"`
	Sensitivity        string    `json:"sensitivity,omitempty"`
	TrustLevel         float64   `json:"trust_level,omitempty"`
	SourceQuality      float64   `json:"source_quality,omitempty"`
	FreshnessScore     float64   `json:"freshness_score,omitempty"`
	QualityScore       float64   `json:"quality_score,omitempty"`
	FeedbackScore      float64   `json:"feedback_score,omitempty"`
	ImportedAt         time.Time `json:"imported_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
	OpenLinkAllowed    bool      `json:"open_link_allowed,omitempty"`
	OpenLinkAllowedSet bool      `json:"open_link_allowed_set,omitempty"`
	Provenance         string    `json:"provenance,omitempty"`
	Ownership          string    `json:"ownership,omitempty"`
	TrustTier          string    `json:"trust_tier,omitempty"`
	Lifecycle          string    `json:"lifecycle,omitempty"`
	RetentionPolicy    string    `json:"retention_policy,omitempty"`
	UpdateMode         string    `json:"update_mode,omitempty"`
}

type Citation struct {
	ChunkID       string  `json:"chunk_id"`
	DocumentID    string  `json:"document_id"`
	Title         string  `json:"title"`
	SourceSystem  string  `json:"source_system"`
	SourceType    string  `json:"source_type"`
	UpdatedAt     string  `json:"updated_at"`
	TrustLevel    float64 `json:"trust_level"`
	SourceURL     string  `json:"source_url,omitempty"`
	Sensitivity   string  `json:"sensitivity"`
	R3Score       float64 `json:"r3_score"`
	Stale         bool    `json:"stale"`
	OpenLinkAllow bool    `json:"open_link_allowed"`
}

type AuditEvent struct {
	EventID     string    `json:"event_id"`
	EventType   string    `json:"event_type"`
	Actor       string    `json:"actor"`
	EntityType  string    `json:"entity_type"`
	EntityID    string    `json:"entity_id"`
	Decision    string    `json:"decision"`
	PolicyClass string    `json:"policy_class"`
	Details     string    `json:"details"`
	CreatedAt   time.Time `json:"created_at"`
}

type ImportJobStatus string

const (
	ImportJobPending   ImportJobStatus = "pending"
	ImportJobRunning   ImportJobStatus = "running"
	ImportJobCompleted ImportJobStatus = "completed"
	ImportJobFailed    ImportJobStatus = "failed"
)

type ImportJob struct {
	JobID         string          `json:"job_id"`
	SourceSystem  string          `json:"source_system"`
	Cursor        string          `json:"cursor"`
	Status        ImportJobStatus `json:"status"`
	Processed     int             `json:"processed"`
	Imported      int             `json:"imported"`
	Skipped       int             `json:"skipped"`
	LastError     string          `json:"last_error,omitempty"`
	LastHash      string          `json:"last_hash,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	IdempotencyID string          `json:"idempotency_id,omitempty"`
}

type ToolPersistenceClass string

const (
	ToolTransientOnly          ToolPersistenceClass = "transient_only"
	ToolPersistableAfterPolicy ToolPersistenceClass = "persistable_after_policy"
	ToolNeverPersist           ToolPersistenceClass = "never_persist"
)

type ToolPersistencePolicy struct{}

func (ToolPersistencePolicy) Classify(toolName string, source string) ToolPersistenceClass {
	t := strings.ToLower(strings.TrimSpace(toolName))
	switch {
	case strings.Contains(t, "hr"):
		return ToolNeverPersist
	case strings.HasPrefix(t, "module:module-http-folder"):
		return ToolPersistableAfterPolicy
	case strings.HasPrefix(t, "connector:"):
		return ToolTransientOnly
	case t == "sql_query", t == "url_fetch", t == "websearch", t == "news", t == "duckduckgo":
		return ToolTransientOnly
	case strings.HasPrefix(source, "module:module-http-folder"):
		return ToolPersistableAfterPolicy
	default:
		return ToolTransientOnly
	}
}

type ACLPolicy struct{}

func (ACLPolicy) CanRoleAccess(role, roleScope, aclGroups string) bool {
	role = normalizeDemoRole(role)
	scopeToken := roleScopeToken(role)
	roleScope = strings.ToLower(strings.TrimSpace(roleScope))
	aclGroups = strings.ToLower(strings.TrimSpace(aclGroups))
	if roleScope == "" || roleScope == "|all|" {
		roleScope = "|all|"
	}
	if aclGroups == "" {
		aclGroups = roleScope
	}
	if strings.Contains(roleScope, "|all|") || strings.Contains(aclGroups, "|all|") {
		return true
	}
	return strings.Contains(roleScope, scopeToken) || strings.Contains(aclGroups, scopeToken)
}

type SensitivityPolicy struct{}

func (SensitivityPolicy) Penalty(sensitivity string) float64 {
	switch strings.ToLower(strings.TrimSpace(sensitivity)) {
	case "public":
		return 0.0
	case "internal":
		return 0.05
	case "confidential":
		return 0.12
	case "restricted":
		return 0.25
	default:
		return 0.08
	}
}

func (SensitivityPolicy) MustPseudonymize(sensitivity string) bool {
	switch strings.ToLower(strings.TrimSpace(sensitivity)) {
	case "confidential", "restricted":
		return true
	default:
		return false
	}
}

type RankingPolicy struct {
	WeightSemantic         float64
	WeightSourceQuality    float64
	WeightTrustLevel       float64
	WeightFreshnessDecay   float64
	WeightFeedbackSignal   float64
	WeightContentQuality   float64
	WeightSensitivity      float64
	WeightConflictPenalty  float64
	MinActionableTicketLen int
}

func defaultRankingPolicy() RankingPolicy {
	return RankingPolicy{
		WeightSemantic:         1.00,
		WeightSourceQuality:    0.35,
		WeightTrustLevel:       0.30,
		WeightFreshnessDecay:   0.25,
		WeightFeedbackSignal:   0.20,
		WeightContentQuality:   0.20,
		WeightSensitivity:      0.30,
		WeightConflictPenalty:  0.20,
		MinActionableTicketLen: 80,
	}
}

func sourceTypeQualityDefault(sourceType string) float64 {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "official_doc", "official", "documentation", "official_dataset", "data_dictionary":
		return 0.95
	case "open_dataset":
		return 0.85
	case "wiki":
		return 0.75
	case "ticket":
		return 0.55
	case "chat":
		return 0.35
	default:
		return 0.60
	}
}

func freshnessDecayScore(updatedAt time.Time, now time.Time) float64 {
	if updatedAt.IsZero() {
		return 0.4
	}
	if now.Before(updatedAt) {
		return 1.0
	}
	days := now.Sub(updatedAt).Hours() / 24
	switch {
	case days <= 7:
		return 1.0
	case days <= 30:
		return 0.9
	case days <= 90:
		return 0.75
	case days <= 180:
		return 0.6
	case days <= 365:
		return 0.45
	default:
		return 0.25
	}
}

func conflictPenaltyForText(content string) float64 {
	l := strings.ToLower(content)
	if strings.Contains(l, "conflict") || strings.Contains(l, "widerspruch") {
		return 0.15
	}
	return 0.0
}

func nonActionableTicket(content string) bool {
	l := strings.ToLower(strings.TrimSpace(content))
	statusDone := strings.Contains(l, " done") || strings.Contains(l, " closed") || strings.Contains(l, " erledigt") || strings.Contains(l, " fixed")
	noResolution := strings.Contains(l, "no resolution") || strings.Contains(l, "ohne lösung") || strings.Contains(l, "without resolution")
	empty := len(l) < defaultRankingPolicy().MinActionableTicketLen
	return statusDone && (noResolution || empty)
}

func (p RankingPolicy) Score(unit RetrievalUnit, semanticSimilarity float64, now time.Time) float64 {
	freshness := unit.FreshnessScore
	if freshness == 0 {
		freshness = freshnessDecayScore(unit.UpdatedAt, now)
	}
	sq := unit.SourceQuality
	if sq == 0 {
		sq = sourceTypeQualityDefault(unit.SourceType)
	}
	sensPenalty := (SensitivityPolicy{}).Penalty(unit.Sensitivity)
	confPenalty := conflictPenaltyForText(unit.Content)
	if strings.EqualFold(strings.TrimSpace(unit.SourceType), "ticket") && nonActionableTicket(unit.Content) {
		confPenalty += 0.90
	}
	return (semanticSimilarity * p.WeightSemantic) +
		(sq * p.WeightSourceQuality) +
		(unit.TrustLevel * p.WeightTrustLevel) +
		(freshness * p.WeightFreshnessDecay) +
		(unit.FeedbackScore * p.WeightFeedbackSignal) +
		(unit.QualityScore * p.WeightContentQuality) -
		(sensPenalty * p.WeightSensitivity) -
		(confPenalty * p.WeightConflictPenalty)
}

func buildCitation(u RetrievalUnit, score float64) Citation {
	updated := ""
	if !u.UpdatedAt.IsZero() {
		updated = u.UpdatedAt.UTC().Format(time.RFC3339)
	}
	srcURL := ""
	if u.OpenLinkAllowed {
		srcURL = u.SourceURL
	}
	return Citation{
		ChunkID:       u.ChunkID,
		DocumentID:    u.DocumentID,
		Title:         u.SourceTitle,
		SourceSystem:  u.SourceSystem,
		SourceType:    u.SourceType,
		UpdatedAt:     updated,
		TrustLevel:    u.TrustLevel,
		SourceURL:     srcURL,
		Sensitivity:   u.Sensitivity,
		R3Score:       score,
		Stale:         freshnessDecayScore(u.UpdatedAt, time.Now()) < 0.5,
		OpenLinkAllow: u.OpenLinkAllowed,
	}
}

func citationsText(citations []Citation) string {
	if len(citations) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Quellenbasis:\n")
	seen := map[string]bool{}
	for i, c := range citations {
		key := c.ChunkID + "|" + c.DocumentID
		if seen[key] {
			continue
		}
		seen[key] = true
		title := strings.TrimSpace(c.Title)
		if title == "" {
			title = c.DocumentID
		}
		link := ""
		if c.SourceURL != "" {
			link = " | URL: " + c.SourceURL
		}
		stale := ""
		if c.Stale {
			stale = " | stale"
		}
		b.WriteString(fmt.Sprintf("- [%d] %s | %s | %s | trust=%.2f%s%s\n", i+1, title, c.SourceSystem, c.SourceType, c.TrustLevel, stale, link))
	}
	return strings.TrimSpace(b.String())
}

func ensureCitedAnswer(answer string, citations []Citation) string {
	answer = strings.TrimSpace(answer)
	if len(citations) == 0 {
		return answer
	}
	l := strings.ToLower(answer)
	if strings.Contains(l, "quellenbasis:") || strings.Contains(l, "[quelle:") {
		return answer
	}
	return strings.TrimSpace(answer + "\n\n" + citationsText(citations))
}

func validateCitationsAgainstSources(answer string, citations []Citation) bool {
	if len(citations) == 0 {
		return true
	}
	l := strings.ToLower(answer)
	hasMarker := strings.Contains(l, "quellenbasis:") || strings.Contains(l, "[quelle:")
	if !hasMarker {
		return false
	}
	for _, c := range citations {
		if c.Title == "" && c.DocumentID == "" {
			continue
		}
		title := strings.ToLower(strings.TrimSpace(c.Title))
		doc := strings.ToLower(strings.TrimSpace(c.DocumentID))
		if title != "" && strings.Contains(l, title) {
			continue
		}
		if doc != "" && strings.Contains(l, doc) {
			continue
		}
		return false
	}
	return true
}

func normalizeR3SourceType(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch {
	case strings.Contains(s, "wiki"):
		return "wiki"
	case strings.Contains(s, "ticket"), strings.Contains(s, "jira"), strings.Contains(s, "incident"):
		return "ticket"
	case strings.Contains(s, "chat"), strings.Contains(s, "mail"):
		return "chat"
	case strings.Contains(s, "dataset"), strings.Contains(s, "ckan"), strings.Contains(s, "data package"), strings.Contains(s, "datapackage"):
		return "open_dataset"
	case strings.Contains(s, "doc"), strings.Contains(s, "manual"), strings.Contains(s, "handbook"), strings.Contains(s, "policy"):
		return "official_doc"
	default:
		return "document"
	}
}

func stableContentHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// ─────────────────────────────────────────────────────────────────────────────
// Pseudonymization and ticket intelligence
// ─────────────────────────────────────────────────────────────────────────────

var (
	nameLikeRe        = regexp.MustCompile(`\b([A-ZÄÖÜ][a-zäöüß]+)\s+([A-ZÄÖÜ][a-zäöüß]+)\b`)
	moneyLikeRe       = regexp.MustCompile(`(?i)(?:€|\$|eur|usd)\s?[0-9][0-9.,]{2,}`)
	roleExecLikeRe    = regexp.MustCompile(`(?i)\b(ceo|cto|cfo|coo|geschäftsführung|vorstand)\b`)
	companySuffixLike = regexp.MustCompile(`\b[A-Z][\w&.-]{2,}\s(?:GmbH|AG|Inc\.?|Ltd\.?|S\.A\.?|LLC)\b`)
)

type ticketArtifact struct {
	Problem          string   `json:"problem"`
	Cause            string   `json:"cause"`
	Solution         string   `json:"solution"`
	RemediationSteps []string `json:"remediation_steps"`
	Tags             []string `json:"tags"`
	Confidence       float64  `json:"confidence"`
	Sensitivity      string   `json:"sensitivity"`
}

func extractTicketArtifact(content string) (ticketArtifact, bool) {
	l := strings.ToLower(content)
	if !(strings.Contains(l, "ticket") || strings.Contains(l, "incident") || strings.Contains(l, "issue")) {
		return ticketArtifact{}, false
	}
	if nonActionableTicket(content) {
		return ticketArtifact{}, false
	}
	lines := strings.Split(content, "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(t), "re:") || strings.HasPrefix(strings.ToLower(t), "fw:") {
			continue
		}
		clean = append(clean, t)
	}
	if len(clean) < 2 {
		return ticketArtifact{}, false
	}
	art := ticketArtifact{
		Problem:    clean[0],
		Cause:      firstMatchingLine(clean, []string{"cause", "grund", "root cause"}),
		Solution:   firstMatchingLine(clean, []string{"solution", "fix", "lösung", "resolved"}),
		Confidence: 0.62,
		Sensitivity: func() string {
			if (SensitivityPolicy{}).MustPseudonymize(detectSensitivityClass(content)) {
				return "confidential"
			}
			return "internal"
		}(),
	}
	steps := collectMatchingLines(clean, []string{"step", "schritt", "action", "maßnahme", "todo"})
	if len(steps) == 0 && art.Solution != "" {
		steps = []string{art.Solution}
	}
	art.RemediationSteps = steps
	if strings.Contains(l, "outage") || strings.Contains(l, "incident") {
		art.Tags = append(art.Tags, "incident")
	}
	if strings.Contains(l, "ticket") {
		art.Tags = append(art.Tags, "ticket")
	}
	if art.Problem == "" || (art.Cause == "" && art.Solution == "") {
		return ticketArtifact{}, false
	}
	return art, true
}

func firstMatchingLine(lines []string, needles []string) string {
	for _, line := range lines {
		ll := strings.ToLower(line)
		for _, n := range needles {
			if strings.Contains(ll, strings.ToLower(n)) {
				return line
			}
		}
	}
	return ""
}

func collectMatchingLines(lines []string, needles []string) []string {
	var out []string
	for _, line := range lines {
		ll := strings.ToLower(line)
		for _, n := range needles {
			if strings.Contains(ll, strings.ToLower(n)) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

func detectSensitivityClass(text string) string {
	l := strings.ToLower(text)
	if strings.Contains(l, "salary") || strings.Contains(l, "gehalt") || strings.Contains(l, "hr") || strings.Contains(l, "legal") {
		return "restricted"
	}
	if piiEmailRe.MatchString(text) || piiPhoneRe.MatchString(text) || piiIBANRe.MatchString(text) || piiCardRe.MatchString(text) {
		return "confidential"
	}
	return "internal"
}

func pseudonymizeText(text string, documentID string, irreversible bool) (string, int) {
	if strings.TrimSpace(text) == "" {
		return text, 0
	}
	var replacements int
	did := stableContentHash(documentID)[:8]
	counters := map[string]int{}
	replaceStable := func(re *regexp.Regexp, token string) {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			replacements++
			key := stableContentHash(did + "|" + token + "|" + strings.ToLower(strings.TrimSpace(m)))
			if irreversible {
				return "[" + token + "_" + strings.ToUpper(key[:6]) + "]"
			}
			counters[token]++
			return fmt.Sprintf("%s_%d", token, counters[token])
		})
	}
	replaceStable(roleExecLikeRe, "ROLE_EXECUTIVE")
	replaceStable(nameLikeRe, "USER")
	replaceStable(companySuffixLike, "CUSTOMER")
	replaceStable(piiEmailRe, "EMAIL")
	replaceStable(piiPhoneRe, "PHONE")
	replaceStable(piiIBANRe, "IBAN")
	replaceStable(piiCardRe, "CARD")
	replaceStable(moneyLikeRe, "HIGH_VALUE")
	return text, replacements
}

func maybeTransformTicket(content string) string {
	art, ok := extractTicketArtifact(content)
	if !ok {
		return content
	}
	j, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return content
	}
	return string(j)
}

func sanitizeAndPseudonymize(text string, s appSettings, documentID string, irreversible bool) (string, int) {
	t := maybeTransformTicket(text)
	t, redactions := sanitizeTextForIngest(t, s)
	sens := detectSensitivityClass(t)
	if (SensitivityPolicy{}).MustPseudonymize(sens) || s.RedactPII {
		p, n := pseudonymizeText(t, documentID, irreversible)
		return p, redactions + n
	}
	return t, redactions
}

// ─────────────────────────────────────────────────────────────────────────────
// URL safety
// ─────────────────────────────────────────────────────────────────────────────

func isSafeFetchURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https URLs are allowed")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("missing host")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("localhost URLs are blocked")
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("private or local network targets are blocked")
		}
	}
	return nil
}

func sortHitsDeterministic[T any](hits []T, less func(a, b T) bool) {
	sort.SliceStable(hits, func(i, j int) bool { return less(hits[i], hits[j]) })
}
