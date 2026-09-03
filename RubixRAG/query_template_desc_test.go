package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestQueryTemplateToolSchemaEnrichesDescription is the core guarantee of
// the "explain the query to the model" work: the description the model
// actually sees folds in the admin description, a per-parameter reference
// (type, required/optional, description, example) and the result hint —
// so the model can decide whether/how to call the query without ever
// seeing the SQL/URL.
func TestQueryTemplateToolSchemaEnrichesDescription(t *testing.T) {
	desc, props, required := queryTemplateToolSchema(
		"Letzte Bestellungen einer Kundennummer.",
		[]sqlQueryParam{
			{Name: "customer_id", Type: "integer", Description: "Kundennummer", Required: true, Example: "4711"},
			{Name: "seit", Type: "date", Description: "nur ab diesem Datum", Required: false, Example: "2026-01-01"},
		},
		"Eine Zeile je Bestellung: Nr, Kunde, Betrag EUR.",
	)

	for _, want := range []string{
		"Letzte Bestellungen einer Kundennummer.",
		"customer_id (integer, erforderlich): Kundennummer [Beispiel: 4711]",
		"seit (date, optional): nur ab diesem Datum [Beispiel: 2026-01-01]",
		"Rückgabe: Eine Zeile je Bestellung: Nr, Kunde, Betrag EUR.",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q\n---\n%s", want, desc)
		}
	}

	// Required list carries exactly the required params.
	if len(required) != 1 || required[0] != "customer_id" {
		t.Fatalf("want required=[customer_id], got %v", required)
	}
	// JSON schema property carries the type and the example.
	p, ok := props["customer_id"].(map[string]any)
	if !ok {
		t.Fatalf("want a customer_id property object, got %T", props["customer_id"])
	}
	if p["type"] != "integer" {
		t.Errorf("want integer JSON-schema type, got %v", p["type"])
	}
	if ex, ok := p["examples"].([]any); !ok || len(ex) != 1 || ex[0] != "4711" {
		t.Errorf("want the example surfaced in the JSON-schema property, got %v", p["examples"])
	}
}

// TestQueryTemplateToolSchemaNoParams keeps a bare template (no params, no
// hint) producing just its plain description — no dangling "Parameter:" or
// "Rückgabe:" headers.
func TestQueryTemplateToolSchemaNoParams(t *testing.T) {
	desc, props, required := queryTemplateToolSchema("Zählt alle offenen Tickets.", nil, "")
	if desc != "Zählt alle offenen Tickets." {
		t.Errorf("want the plain description unchanged, got %q", desc)
	}
	if len(props) != 0 || len(required) != 0 {
		t.Errorf("want empty props/required, got %v / %v", props, required)
	}
	if required == nil {
		t.Fatal("want required to be an empty slice, not nil — json.Marshal(nil []string) produces `null`, which several providers reject as an invalid JSON-schema \"required\" field (must be an array); see queryTemplateToolSchema's doc comment")
	}
}

// TestHTTPTemplateToolDefRequiredIsNeverNullInJSON is
// TestQueryTemplateToolSchemaNoParams' regression test taken all the way
// to the actual wire format a chat backend receives: a template with zero
// required parameters must still marshal "required" as `[]`, never
// `null` — the exact failure this codebase hit in production ("Invalid
// schema for function ...: None is not of type 'array'", rejecting the
// whole request, not just this one template).
func TestHTTPTemplateToolDefRequiredIsNeverNullInJSON(t *testing.T) {
	def := httpTemplateToolDef(httpQueryTemplate{
		Name:        "sap_live",
		Description: "SAP-Tabellen live lesen.",
		URLTemplate: "https://logistic.rubix-intern.de/se16/ZITEC/{table}/{number}",
		Parameters: []sqlQueryParam{
			{Name: "table", Type: "string", Required: false},
			{Name: "number", Type: "integer", Required: false},
		},
		Enabled: true,
	})
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"required":null`) {
		t.Fatalf("want \"required\" marshaled as [], got null in: %s", raw)
	}
	if !strings.Contains(string(raw), `"required":[]`) {
		t.Fatalf("want an explicit empty \"required\" array in: %s", raw)
	}
}

// TestMSSQLTemplateToolDefUsesEnrichedDescription proves the enrichment
// actually reaches the tool definition the model is handed.
func TestMSSQLTemplateToolDefUsesEnrichedDescription(t *testing.T) {
	def := mssqlTemplateToolDef(sqlQueryTemplate{
		Name:        "get_customer_orders",
		Description: "Letzte Bestellungen einer Kundennummer.",
		SQL:         "SELECT TOP 50 * FROM Orders WHERE CustomerID = {customer_id}",
		Parameters:  []sqlQueryParam{{Name: "customer_id", Type: "integer", Description: "Kundennummer", Required: true, Example: "4711"}},
		ResultHint:  "Eine Zeile je Bestellung.",
		Enabled:     true,
	})
	if def.Function.Name != "get_customer_orders" {
		t.Fatalf("unexpected tool name %q", def.Function.Name)
	}
	if !strings.Contains(def.Function.Description, "[Beispiel: 4711]") ||
		!strings.Contains(def.Function.Description, "Rückgabe: Eine Zeile je Bestellung.") {
		t.Fatalf("tool def description not enriched:\n%s", def.Function.Description)
	}
	// The SQL text must NEVER leak into what the model sees.
	if strings.Contains(def.Function.Description, "SELECT") {
		t.Fatalf("SQL text leaked into the model-facing description:\n%s", def.Function.Description)
	}
}
