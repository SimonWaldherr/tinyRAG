package main

import (
	"strings"
	"testing"
)

// Covers sqlQueryParam.Options — the enum constraint shared by SQL and HTTP
// query templates (settings.go), its JSON-schema "enum" surfacing
// (connector.go's queryTemplateToolSchema), save-time validation
// (validateTemplateParamOptions), and execution-time enforcement
// (templateParamOptionMatches, wired into mssqlTemplateToolExecutor/
// httpTemplateToolExecutor) — motivated by an SAP se16-style URL template
// (https://logistic.rubix-intern.de/se16/ZITEC/{table}/{id}) where {table}
// should only ever be one of a fixed set of known SAP table names.

func TestQueryTemplateToolSchemaAddsEnumForOptions(t *testing.T) {
	_, props, _ := queryTemplateToolSchema("desc", []sqlQueryParam{
		{Name: "table", Type: "string", Options: []string{"likp", "vbak", "kna1"}},
		{Name: "id", Type: "string"},
	}, "")
	tableProp, ok := props["table"].(map[string]any)
	if !ok {
		t.Fatal("want a schema property for \"table\"")
	}
	enum, ok := tableProp["enum"].([]any)
	if !ok || len(enum) != 3 {
		t.Fatalf("want a 3-value enum on \"table\", got %v", tableProp["enum"])
	}
	idProp := props["id"].(map[string]any)
	if _, has := idProp["enum"]; has {
		t.Error("want no enum key on a parameter with no Options")
	}
}

func TestValidateTemplateParamOptionsRejectsEmptyAndDuplicate(t *testing.T) {
	if err := validateTemplateParamOptions([]string{"likp", "vbak"}); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
	if err := validateTemplateParamOptions([]string{"likp", ""}); err == nil {
		t.Error("want an error for an empty option value")
	}
	if err := validateTemplateParamOptions([]string{"likp", "LIKP"}); err == nil {
		t.Error("want an error for a case-insensitive duplicate")
	}
}

func TestTemplateParamOptionMatchesIsCaseInsensitive(t *testing.T) {
	opts := []string{"likp", "vbak"}
	if !templateParamOptionMatches(opts, "LIKP") {
		t.Error("want a case-insensitive match")
	}
	if templateParamOptionMatches(opts, "mara") {
		t.Error("want no match for an unlisted value")
	}
	if !templateParamOptionMatches([]string{"1", "2"}, int64(1)) {
		t.Error("want a match against a non-string converted value (SQL template int64)")
	}
}

func TestValidateHTTPQueryTemplatesRejectsBadOptions(t *testing.T) {
	templates := []httpQueryTemplate{{
		Name:        "get_se16",
		URLTemplate: "https://example.com/se16/{table}/{id}",
		AuthSource:  "none",
		Parameters: []sqlQueryParam{
			{Name: "table", Type: "string", Required: true, Options: []string{"likp", ""}},
			{Name: "id", Type: "string", Required: true},
		},
		Enabled: true,
	}}
	if err := validateHTTPQueryTemplates(templates, appSettings{}); err == nil {
		t.Fatal("want a validation error for an empty option value")
	}
}

func TestValidateSQLQueryTemplatesRejectsBadOptions(t *testing.T) {
	templates := []sqlQueryTemplate{{
		Name: "lookup_status",
		SQL:  "SELECT TOP 50 * FROM dbo.orders WHERE status = {status}",
		Parameters: []sqlQueryParam{
			{Name: "status", Type: "string", Required: true, Options: []string{"open", "open"}},
		},
		Enabled: true,
	}}
	if err := validateSQLQueryTemplates(templates); err == nil {
		t.Fatal("want a validation error for a duplicate option value")
	}
}

// TestHTTPTemplateToolExecutorEnforcesOptions proves the se16 motivating
// case end-to-end: a model-supplied {table} value outside Options is
// rejected before the request is ever sent, even though the JSON-schema
// enum only steers the model and doesn't strictly guarantee this.
func TestHTTPTemplateToolExecutorEnforcesOptions(t *testing.T) {
	tmpl := httpQueryTemplate{
		Name:        "get_se16",
		URLTemplate: "https://logistic.rubix-intern.de/se16/ZITEC/{table}/{id}",
		AuthSource:  "none",
		Parameters: []sqlQueryParam{
			{Name: "table", Type: "string", Required: true, Options: []string{"likp", "vbak", "kna1", "mbew", "mara"}},
			{Name: "id", Type: "string", Required: true},
		},
	}
	exec := httpTemplateToolExecutor(tmpl, appSettings{})
	_, err := exec(t.Context(), `{"table": "drop_table", "id": "1"}`)
	if err == nil {
		t.Fatal("want an error for a table name outside Options")
	}
	if !strings.Contains(err.Error(), "not one of the allowed values") {
		t.Errorf("want a clear allowed-values error, got: %v", err)
	}
}

func TestMSSQLTemplateToolExecutorEnforcesOptions(t *testing.T) {
	tmpl := sqlQueryTemplate{
		Name: "lookup_status",
		SQL:  "SELECT TOP 50 * FROM dbo.orders WHERE status = {status}",
		Parameters: []sqlQueryParam{
			{Name: "status", Type: "string", Required: true, Options: []string{"open", "closed"}},
		},
	}
	exec := mssqlTemplateToolExecutor(mssqlConfig{}, tmpl)
	_, err := exec(t.Context(), `{"status": "'; DROP TABLE orders; --"}`)
	if err == nil {
		t.Fatal("want an error for a status value outside Options")
	}
	if !strings.Contains(err.Error(), "not one of the allowed values") {
		t.Errorf("want a clear allowed-values error, got: %v", err)
	}
}
