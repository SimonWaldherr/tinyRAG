package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sapS4MaxResponseBytes int64 = 8 << 20

var sapS4FieldNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func sapS4BaseURL(cfg sapS4Config) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("sap_s4: base_url must be a valid https URL without credentials/query")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func sapS4EntityPath(cfg sapS4Config) (string, error) {
	p := strings.TrimSpace(cfg.EntityPath)
	u, err := url.Parse(p)
	if p == "" || err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") {
		return "", fmt.Errorf("sap_s4: entity_path must be a relative absolute path without query (e.g. /sap/opu/odata/sap/API_BUSINESS_PARTNER/A_BusinessPartner)")
	}
	return u.EscapedPath(), nil
}

func sapS4SelectedFields(cfg sapS4Config) ([]string, error) {
	fields := []string{strings.TrimSpace(cfg.IDField), strings.TrimSpace(cfg.TitleField), strings.TrimSpace(cfg.UpdatedAtField)}
	fields = append(fields, cfg.ContentFields...)
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !sapS4FieldNameRe.MatchString(field) {
			return nil, fmt.Errorf("sap_s4: field %q must contain only letters, digits and underscores", field)
		}
		if !seen[field] {
			seen[field] = true
			out = append(out, field)
		}
	}
	if strings.TrimSpace(cfg.IDField) == "" || !sapS4FieldNameRe.MatchString(strings.TrimSpace(cfg.IDField)) {
		return nil, fmt.Errorf("sap_s4: id_field is required and must be a simple OData field name")
	}
	if len(cfg.ContentFields) == 0 {
		return nil, fmt.Errorf("sap_s4: configure at least one content_fields entry")
	}
	return out, nil
}

func sapS4InitialURL(cfg sapS4Config, maxItems int) (string, error) {
	base, err := sapS4BaseURL(cfg)
	if err != nil {
		return "", err
	}
	entity, err := sapS4EntityPath(cfg)
	if err != nil {
		return "", err
	}
	fields, err := sapS4SelectedFields(cfg)
	if err != nil {
		return "", err
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + entity
	q := url.Values{}
	q.Set("$select", strings.Join(fields, ","))
	q.Set("$top", strconv.Itoa(maxItems))
	q.Set("$format", "json")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// sapS4CursorURL accepts a continuation only on the configured HTTPS origin.
// The upstream service is allowed to choose a different path/query for an
// OData token, but not a different host that could receive the SAP token.
func sapS4CursorURL(cfg sapS4Config, cursor string) (string, error) {
	base, err := sapS4BaseURL(cfg)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(cursor))
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) || u.User != nil {
		return "", fmt.Errorf("sap_s4: continuation link must use the configured SAP host over https")
	}
	return u.String(), nil
}

func sapS4ResolvedPassword(cfg sapS4Config) string {
	return resolveSecret(cfg.Password, cfg.PasswordEnv)
}
func sapS4ResolvedToken(cfg sapS4Config) string { return resolveSecret(cfg.Token, cfg.TokenEnv) }

func sapS4ApplyAuth(req *http.Request, cfg sapS4Config) error {
	switch strings.ToLower(strings.TrimSpace(cfg.AuthType)) {
	case "", "basic":
		if strings.TrimSpace(cfg.Username) == "" || sapS4ResolvedPassword(cfg) == "" {
			return fmt.Errorf("sap_s4: username/password not configured (set password or password_env)")
		}
		req.SetBasicAuth(cfg.Username, sapS4ResolvedPassword(cfg))
	case "bearer":
		if token := sapS4ResolvedToken(cfg); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			return fmt.Errorf("sap_s4: bearer token not configured (set token or token_env)")
		}
	case "header":
		if token := sapS4ResolvedToken(cfg); token != "" {
			name := strings.TrimSpace(cfg.HeaderName)
			if name == "" {
				name = "Authorization"
			}
			req.Header.Set(name, token)
		} else {
			return fmt.Errorf("sap_s4: header token not configured (set token or token_env)")
		}
	case "none":
	default:
		return fmt.Errorf("sap_s4: unknown auth_type %q (want basic|bearer|header|none)", cfg.AuthType)
	}
	for name, value := range cfg.Headers {
		// Credentials must come from the dedicated, redacted fields above;
		// a static header cannot replace or accidentally override them.
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") {
			continue
		}
		req.Header.Set(name, value)
	}
	return nil
}

func sapS4Get(ctx context.Context, cfg sapS4Config, fullURL string, trackChanges bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sap_s4: build request: %w", err)
	}
	if err := sapS4ApplyAuth(req, cfg); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", connectorUserAgent)
	if trackChanges {
		req.Header.Set("Prefer", "odata.track-changes")
	}
	raw, err := doWithRetryLimitedNoRedirect(req, false, sapS4MaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("sap_s4 GET: %w", err)
	}
	return raw, nil
}

type sapS4Page struct {
	Records   []map[string]any
	NextLink  string
	DeltaLink string
}

// parseSAPS4Page accepts both OData V4 ({value, @odata.nextLink}) and the
// older SAP-common V2 ({d:{results, __next}}) envelope. Values stay generic
// because each S/4 API has a different entity schema; the configured field
// allow-list above is the schema contract.
func parseSAPS4Page(raw []byte) (sapS4Page, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return sapS4Page{}, fmt.Errorf("sap_s4: parse OData JSON: %w", err)
	}
	page := sapS4Page{}
	decodeRecords := func(value json.RawMessage) error {
		if len(value) == 0 {
			return nil
		}
		return json.Unmarshal(value, &page.Records)
	}
	if value, ok := top["value"]; ok {
		if err := decodeRecords(value); err != nil {
			return sapS4Page{}, fmt.Errorf("sap_s4: parse OData V4 values: %w", err)
		}
		_ = json.Unmarshal(top["@odata.nextLink"], &page.NextLink)
		_ = json.Unmarshal(top["@odata.deltaLink"], &page.DeltaLink)
		return page, nil
	}
	if rawD, ok := top["d"]; ok {
		var d map[string]json.RawMessage
		if err := json.Unmarshal(rawD, &d); err != nil {
			return sapS4Page{}, fmt.Errorf("sap_s4: parse OData V2 envelope: %w", err)
		}
		if err := decodeRecords(d["results"]); err != nil {
			return sapS4Page{}, fmt.Errorf("sap_s4: parse OData V2 results: %w", err)
		}
		_ = json.Unmarshal(d["__next"], &page.NextLink)
		_ = json.Unmarshal(d["__delta"], &page.DeltaLink)
		return page, nil
	}
	return sapS4Page{}, fmt.Errorf("sap_s4: response is neither OData V2 nor V4")
}

func sapS4Value(record map[string]any, field string) string {
	v, ok := record[field]
	if !ok || v == nil {
		return ""
	}
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func sapS4RecordText(cfg sapS4Config, record map[string]any) string {
	var b strings.Builder
	id := sapS4Value(record, cfg.IDField)
	title := sapS4Value(record, cfg.TitleField)
	if title == "" {
		title = id
	}
	fmt.Fprintf(&b, "%s\n", title)
	fmt.Fprintf(&b, "ID: %s\n", id)
	fields := append([]string(nil), cfg.ContentFields...)
	sort.Strings(fields) // deterministic text/hash even when config order changes.
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if value := sapS4Value(record, field); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", field, value)
		}
	}
	return b.String()
}

func sapS4SourcePrefix(cfg sapS4Config) (string, error) {
	base, err := sapS4BaseURL(cfg)
	if err != nil {
		return "", err
	}
	return "sap_s4:" + base.Host + ":" + strings.Trim(strings.TrimSpace(cfg.EntityPath), "/"), nil
}

func sapS4DocumentDate(cfg sapS4Config, record map[string]any) int64 {
	v := sapS4Value(record, cfg.UpdatedAtField)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Unix()
		}
	}
	return 0
}

type sapS4ImportResult struct {
	baseImportResult
	Records int `json:"records"`
	Deleted int `json:"deleted"`
}

// syncSAPS4 imports a bounded OData page, honours an upstream delta link if
// present, and otherwise remains idempotent through ingestDocument's content
// hash. State is returned rather than written here so handlers and scheduler
// can persist it through the same guarded settings update path.
func syncSAPS4(ctx context.Context, rag *ragSystem, s appSettings, cfg sapS4Config, embedModel string, dryRun bool) (sapS4ImportResult, sapS4Config, error) {
	res := sapS4ImportResult{baseImportResult: baseImportResult{DryRun: dryRun}}
	if _, err := sapS4SelectedFields(cfg); err != nil {
		return res, cfg, err
	}
	var fetchURL string
	var err error
	trackChanges := false
	switch {
	case cfg.NextLink != "":
		fetchURL, err = sapS4CursorURL(cfg, cfg.NextLink)
	case cfg.DeltaLink != "":
		fetchURL, err = sapS4CursorURL(cfg, cfg.DeltaLink)
	default:
		fetchURL, err = sapS4InitialURL(cfg, cfg.effectiveMaxItems(s.Import))
		trackChanges = true
	}
	if err != nil {
		return res, cfg, err
	}
	raw, err := sapS4Get(ctx, cfg, fetchURL, trackChanges)
	if err != nil {
		return res, cfg, err
	}
	page, err := parseSAPS4Page(raw)
	if err != nil {
		return res, cfg, err
	}
	prefix, err := sapS4SourcePrefix(cfg)
	if err != nil {
		return res, cfg, err
	}
	for _, record := range page.Records {
		if err := ctx.Err(); err != nil {
			return res, cfg, err
		}
		id := sapS4Value(record, cfg.IDField)
		if id == "" {
			res.Errors = append(res.Errors, "record without configured id_field")
			continue
		}
		sourceID := prefix + ":" + id
		if _, deleted := record["@removed"]; deleted {
			if !dryRun {
				if err := rag.deleteSource(sourceID); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: delete failed: %v", id, err))
					continue
				}
			}
			res.Deleted++
			continue
		}
		title := sapS4Value(record, cfg.TitleField)
		if title == "" {
			title = id
		}
		outcome, err := ingestDocument(rag, s, embedModel, sourceID, "sap_s4_record", title, sapS4RecordText(cfg, record), sapS4DocumentDate(cfg, record), dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Records++
		if outcome.Skipped {
			res.Skipped++
		} else {
			res.Chunks += outcome.Chunks
		}
	}
	if len(res.Errors) > 0 {
		return res, cfg, nil // leave cursors unchanged so failures retry.
	}
	next := cfg
	if page.NextLink != "" {
		nextURL, err := sapS4CursorURL(cfg, page.NextLink)
		if err != nil {
			return res, cfg, err
		}
		next.NextLink = nextURL
		return res, next, nil
	}
	next.NextLink = ""
	if page.DeltaLink != "" {
		deltaURL, err := sapS4CursorURL(cfg, page.DeltaLink)
		if err != nil {
			return res, cfg, err
		}
		next.DeltaLink = deltaURL
	}
	return res, next, nil
}
