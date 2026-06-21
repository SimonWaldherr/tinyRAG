package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ckanImportRequest struct {
	PortalURL  string   `json:"portal_url"`
	Query      string   `json:"query"`
	PackageID  string   `json:"package_id"`
	Limit      int      `json:"limit"`
	EmbedModel string   `json:"embed_model"`
	Roles      []string `json:"roles"`
}

type ckanActionResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   any             `json:"error,omitempty"`
}

type ckanPackageSearchResult struct {
	Count   int              `json:"count"`
	Results []map[string]any `json:"results"`
}

func normalizeCKANPortalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("portal_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse portal_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("portal_url must use http or https")
	}
	if u.Hostname() == "" {
		return "", errors.New("portal_url must include a host")
	}
	if idx := strings.Index(u.Path, "/api/"); idx >= 0 {
		u.Path = strings.TrimRight(u.Path[:idx], "/")
	}
	u.RawQuery = ""
	u.Fragment = ""
	portal := strings.TrimRight(u.String(), "/")
	if err := isSafeFetchURL(portal); err != nil {
		return "", err
	}
	return portal, nil
}

func fetchCKANPackages(ctx context.Context, client *http.Client, req ckanImportRequest) ([]map[string]any, string, error) {
	portal, err := normalizeCKANPortalURL(req.PortalURL)
	if err != nil {
		return nil, "", err
	}
	if client == nil {
		client = newHTTPClient(20 * time.Second)
	}
	if strings.TrimSpace(req.PackageID) != "" {
		var pkg map[string]any
		if err := doCKANAction(ctx, client, portal, "package_show", url.Values{"id": {strings.TrimSpace(req.PackageID)}}, &pkg); err != nil {
			return nil, portal, err
		}
		return []map[string]any{pkg}, portal, nil
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, portal, errors.New("query or package_id is required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	var search ckanPackageSearchResult
	params := url.Values{
		"q":    {query},
		"rows": {strconv.Itoa(limit)},
	}
	if err := doCKANAction(ctx, client, portal, "package_search", params, &search); err != nil {
		return nil, portal, err
	}
	return search.Results, portal, nil
}

func doCKANAction(ctx context.Context, client *http.Client, portal, action string, params url.Values, out any) error {
	actionURL := strings.TrimRight(portal, "/") + "/api/3/action/" + action
	if len(params) > 0 {
		actionURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actionURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ckan %s request: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ckan %s returned HTTP %d", action, resp.StatusCode)
	}
	var envelope ckanActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode ckan %s: %w", action, err)
	}
	if !envelope.Success {
		return fmt.Errorf("ckan %s failed: %v", action, envelope.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode ckan %s result: %w", action, err)
	}
	return nil
}

func buildCKANDatasetCard(portal string, pkg map[string]any) string {
	title := ckanString(pkg, "title", "name", "id")
	name := ckanString(pkg, "name", "id")
	description := ckanString(pkg, "notes", "description")
	org := ckanNestedString(pkg, "organization", "title", "name")
	license := ckanLicenseText(pkg)
	updated := ckanString(pkg, "metadata_modified", "modified", "updated")
	created := ckanString(pkg, "metadata_created", "created")
	tags := ckanTags(pkg)
	groups := ckanGroupNames(pkg)
	extras := ckanExtras(pkg, 12)
	resources := ckanResources(pkg)

	var lines []string
	add := func(label, value string) {
		value = cleanCKANText(value)
		if value != "" {
			lines = append(lines, label+": "+value)
		}
	}
	add("Dataset", title)
	add("Name", name)
	add("Publisher", org)
	add("Portal", portal)
	add("Dataset URL", ckanDatasetURL(portal, pkg))
	add("License", license)
	add("Updated", updated)
	add("Created", created)
	if len(tags) > 0 {
		add("Tags", strings.Join(tags, ", "))
	}
	if len(groups) > 0 {
		add("Groups", strings.Join(groups, ", "))
	}
	if description != "" {
		lines = append(lines, "", "Description:", cleanCKANText(description))
	}
	if len(extras) > 0 {
		lines = append(lines, "", "Additional metadata:")
		lines = append(lines, extras...)
	}
	if len(resources) > 0 {
		lines = append(lines, "", "Resources:")
		for i, res := range resources {
			if i >= 12 {
				lines = append(lines, fmt.Sprintf("- %d additional resources omitted from card.", len(resources)-i))
				break
			}
			lines = append(lines, ckanResourceLines(res)...)
		}
	}
	lines = append(lines,
		"",
		"AI use guidance:",
		"- Cite the dataset title, publisher, portal, license, and update date when answering.",
		"- Treat the schema and resource metadata as context for interpreting fields, units, coverage, and limitations.",
		"- Do not infer geographic, temporal, or methodological coverage beyond the dataset notes, schema, and resource metadata.",
		"- Prefer newer resources when several resources describe the same measure.",
	)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func ckanPackageR3Metadata(portal string, pkg map[string]any) R3IngestMetadata {
	portalURL, _ := url.Parse(portal)
	host := portal
	if portalURL != nil && portalURL.Hostname() != "" {
		host = portalURL.Hostname()
	}
	objectID := ckanString(pkg, "id", "name")
	if objectID == "" {
		objectID = stableContentHash(buildCKANDatasetCard(portal, pkg))[:16]
	}
	title := ckanString(pkg, "title", "name", "id")
	license := ckanLicenseText(pkg)
	org := ckanNestedString(pkg, "organization", "title", "name")
	updatedAt := ckanPackageUpdatedAt(pkg)
	sourceSystem := "ckan:" + host
	sourceURL := ckanDatasetURL(portal, pkg)
	trust := 0.72
	sourceQuality := 0.82
	trustTier := "open-data-metadata"
	if license != "" {
		trust = 0.80
		sourceQuality = 0.90
		trustTier = "licensed-open-data"
	}
	if org == "" {
		trust = 0.66
	}
	openLink := true
	return R3IngestMetadata{
		DocumentID:         stableContentHash(sourceSystem + "|" + objectID),
		SourceSystem:       sourceSystem,
		SourceType:         "official_dataset",
		SourceTitle:        title,
		SourceURL:          sourceURL,
		SourceObjectID:     objectID,
		SourceVersion:      ckanString(pkg, "metadata_modified", "version", "metadata_created"),
		BusinessOwner:      org,
		Sensitivity:        "public",
		TrustLevel:         trust,
		SourceQuality:      sourceQuality,
		FreshnessScore:     freshnessDecayScore(updatedAt, time.Now().UTC()),
		QualityScore:       ckanPackageQualityScore(pkg),
		FeedbackScore:      0.50,
		UpdatedAt:          updatedAt,
		OpenLinkAllowed:    openLink,
		OpenLinkAllowedSet: true,
		Provenance:         sourceURL,
		Ownership:          org,
		TrustTier:          trustTier,
		Lifecycle:          "active",
		RetentionPolicy:    "external-public-metadata",
	}
}

func ckanPackageQualityScore(pkg map[string]any) float64 {
	score := 0.60
	if ckanString(pkg, "notes", "description") != "" {
		score += 0.10
	}
	if ckanLicenseText(pkg) != "" {
		score += 0.10
	}
	if len(ckanTags(pkg)) > 0 {
		score += 0.05
	}
	for _, res := range ckanResources(pkg) {
		if len(ckanSchemaFields(res["schema"])) > 0 {
			score += 0.10
			break
		}
	}
	return clampUnitInterval(score)
}

func ckanPackageUpdatedAt(pkg map[string]any) time.Time {
	for _, key := range []string{"metadata_modified", "modified", "updated", "metadata_created", "created"} {
		if t, ok := parseCKANTime(ckanString(pkg, key)); ok {
			return t
		}
	}
	return time.Now().UTC()
}

func parseCKANTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05.999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func ckanDatasetURL(portal string, pkg map[string]any) string {
	if raw := ckanString(pkg, "url"); raw != "" {
		return raw
	}
	name := ckanString(pkg, "name", "id")
	if name == "" {
		return strings.TrimRight(portal, "/")
	}
	return strings.TrimRight(portal, "/") + "/dataset/" + url.PathEscape(name)
}

func ckanLicenseText(pkg map[string]any) string {
	parts := []string{}
	for _, key := range []string{"license_title", "license_id", "license_url"} {
		if v := ckanString(pkg, key); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " | ")
}

func ckanString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s := stringFromCKANValue(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func ckanNestedString(m map[string]any, parent string, keys ...string) string {
	raw, ok := m[parent]
	if !ok {
		return ""
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return ckanString(nested, keys...)
}

func stringFromCKANValue(v any) string {
	switch tv := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(tv)
	case fmt.Stringer:
		return strings.TrimSpace(tv.String())
	case float64:
		if tv == float64(int64(tv)) {
			return strconv.FormatInt(int64(tv), 10)
		}
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case int:
		return strconv.Itoa(tv)
	case int64:
		return strconv.FormatInt(tv, 10)
	case bool:
		if tv {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(fmt.Sprint(tv))
	}
}

func cleanCKANText(v string) string {
	fields := strings.Fields(strings.TrimSpace(v))
	return strings.Join(fields, " ")
}

func ckanTags(pkg map[string]any) []string {
	return ckanNamedList(pkg["tags"], "display_name", "name")
}

func ckanGroupNames(pkg map[string]any) []string {
	return ckanNamedList(pkg["groups"], "display_name", "title", "name")
}

func ckanNamedList(raw any, keys ...string) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	seen := map[string]bool{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		value := ckanString(m, keys...)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ckanExtras(pkg map[string]any, limit int) []string {
	arr, ok := pkg["extras"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if len(out) >= limit {
			break
		}
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key := ckanString(m, "key")
		value := ckanString(m, "value")
		if key == "" || value == "" {
			continue
		}
		out = append(out, "- "+cleanCKANText(key)+": "+cleanCKANText(value))
	}
	return out
}

func ckanResources(pkg map[string]any) []map[string]any {
	arr, ok := pkg["resources"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func ckanResourceLines(res map[string]any) []string {
	name := ckanString(res, "name", "id", "url")
	if name == "" {
		name = "resource"
	}
	format := ckanString(res, "format", "mimetype")
	urlValue := ckanString(res, "url")
	updated := ckanString(res, "last_modified", "metadata_modified", "created")
	description := ckanString(res, "description")
	lines := []string{"- Resource: " + cleanCKANText(name)}
	if format != "" {
		lines = append(lines, "  Format: "+cleanCKANText(format))
	}
	if urlValue != "" {
		lines = append(lines, "  URL: "+urlValue)
	}
	if updated != "" {
		lines = append(lines, "  Updated: "+cleanCKANText(updated))
	}
	if description != "" {
		lines = append(lines, "  Description: "+cleanCKANText(description))
	}
	fields := ckanSchemaFields(res["schema"])
	if len(fields) > 0 {
		lines = append(lines, "  Schema fields:")
		lines = append(lines, fields...)
	}
	return lines
}

func ckanSchemaFields(raw any) []string {
	schema, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	fieldsRaw, ok := schema["fields"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(fieldsRaw))
	for _, item := range fieldsRaw {
		if len(out) >= 16 {
			out = append(out, "  - additional fields omitted from card")
			break
		}
		field, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := ckanString(field, "name", "title")
		if name == "" {
			continue
		}
		typ := ckanString(field, "type", "format")
		desc := ckanString(field, "description")
		line := "  - " + cleanCKANText(name)
		if typ != "" {
			line += " (" + cleanCKANText(typ) + ")"
		}
		if desc != "" {
			line += ": " + cleanCKANText(desc)
		}
		out = append(out, line)
	}
	return out
}
