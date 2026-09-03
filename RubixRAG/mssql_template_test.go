package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRewriteSQLTemplatePlaceholders proves {name} placeholders become
// real @name bind references at execution time — the syntax admins author
// with is unified with HTTP templates' {name}, but the driver still only
// ever sees genuine T-SQL parameter syntax, never {}.
func TestRewriteSQLTemplatePlaceholders(t *testing.T) {
	got := rewriteSQLTemplatePlaceholders("SELECT * FROM Orders WHERE CustomerID = {customer_id} AND Status = {status}")
	want := "SELECT * FROM Orders WHERE CustomerID = @customer_id AND Status = @status"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestMigrateLegacySQLTemplateParamSyntax proves an existing settings.json
// written before {name} replaced @name as the SQL template authoring
// syntax is upgraded in place on load — an admin who already has working
// templates never has to hand-edit them.
func TestMigrateLegacySQLTemplateParamSyntax(t *testing.T) {
	templates := []sqlQueryTemplate{{
		Name: "get_customer",
		SQL:  "SELECT * FROM Customers WHERE CustomerID = @id AND Region = @region",
		Parameters: []sqlQueryParam{
			{Name: "id", Type: "integer"},
			{Name: "region", Type: "string"},
		},
	}}
	migrateLegacySQLTemplateParamSyntax(templates)
	want := "SELECT * FROM Customers WHERE CustomerID = {id} AND Region = {region}"
	if templates[0].SQL != want {
		t.Fatalf("got %q, want %q", templates[0].SQL, want)
	}

	// Already-migrated (or freshly authored in the new syntax) templates
	// must be left untouched — no @ references left to rewrite.
	already := []sqlQueryTemplate{{
		Name:       "get_order",
		SQL:        "SELECT * FROM Orders WHERE OrderID = {order_id}",
		Parameters: []sqlQueryParam{{Name: "order_id", Type: "integer"}},
	}}
	migrateLegacySQLTemplateParamSyntax(already)
	if already[0].SQL != "SELECT * FROM Orders WHERE OrderID = {order_id}" {
		t.Fatalf("want an already-migrated template left unchanged, got %q", already[0].SQL)
	}
}

// TestMigrateLegacySQLTemplateParamSyntaxDoesNotTouchUnrelatedAt proves the
// migration only rewrites @name for names actually declared as this
// template's own parameters — a coincidental "@" elsewhere (e.g. an email
// address in an unrelated literal) that happens not to match any declared
// parameter name is left alone.
func TestMigrateLegacySQLTemplateParamSyntaxDoesNotTouchUnrelatedAt(t *testing.T) {
	templates := []sqlQueryTemplate{{
		Name:       "lookup",
		SQL:        "SELECT * FROM Log WHERE Actor = @actor AND Note LIKE '%support@rubix.com%'",
		Parameters: []sqlQueryParam{{Name: "actor", Type: "string"}},
	}}
	migrateLegacySQLTemplateParamSyntax(templates)
	want := "SELECT * FROM Log WHERE Actor = {actor} AND Note LIKE '%support@rubix.com%'"
	if templates[0].SQL != want {
		t.Fatalf("got %q, want %q", templates[0].SQL, want)
	}
}

func TestValidateSQLQueryTemplatesAcceptsGoodTemplate(t *testing.T) {
	templates := []sqlQueryTemplate{{
		Name: "get_customer_orders",
		SQL:  "SELECT TOP 50 * FROM Orders WHERE CustomerID = {customer_id}",
		Parameters: []sqlQueryParam{
			{Name: "customer_id", Type: "integer", Required: true},
		},
		Enabled: true,
	}}
	if err := validateSQLQueryTemplates(templates); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
}

func TestValidateSQLQueryTemplatesRejections(t *testing.T) {
	cases := []struct {
		name      string
		templates []sqlQueryTemplate
	}{
		{"invalid name", []sqlQueryTemplate{{Name: "bad name!", SQL: "SELECT 1"}}},
		{"collides with built-in tool", []sqlQueryTemplate{{Name: "query_mssql", SQL: "SELECT 1"}}},
		{"duplicate name", []sqlQueryTemplate{
			{Name: "t1", SQL: "SELECT 1"},
			{Name: "t1", SQL: "SELECT 2"},
		}},
		{"non-select statement", []sqlQueryTemplate{{Name: "danger", SQL: "DELETE FROM Orders"}}},
		{"unknown parameter type", []sqlQueryTemplate{{
			Name: "t1", SQL: "SELECT * FROM T WHERE id={id}",
			Parameters: []sqlQueryParam{{Name: "id", Type: "wat"}},
		}}},
		{"parameter not referenced in sql", []sqlQueryTemplate{{
			Name: "t1", SQL: "SELECT * FROM T",
			Parameters: []sqlQueryParam{{Name: "unused", Type: "string"}},
		}}},
		{"sql references undeclared parameter", []sqlQueryTemplate{{
			Name: "t1", SQL: "SELECT * FROM T WHERE id={id}",
		}}},
		{"duplicate parameter name", []sqlQueryTemplate{{
			Name: "t1", SQL: "SELECT * FROM T WHERE id={id}",
			Parameters: []sqlQueryParam{{Name: "id", Type: "string"}, {Name: "id", Type: "integer"}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateSQLQueryTemplates(c.templates); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestMssqlTemplateToolDefSchema(t *testing.T) {
	tmpl := sqlQueryTemplate{
		Name:        "get_customer_orders",
		Description: "Fetch recent orders for a customer.",
		SQL:         "SELECT TOP 50 * FROM Orders WHERE CustomerID = {customer_id}",
		Parameters: []sqlQueryParam{
			{Name: "customer_id", Type: "integer", Required: true, Description: "The customer's numeric ID."},
		},
	}
	def := mssqlTemplateToolDef(tmpl)
	if def.Function.Name != "get_customer_orders" {
		t.Fatalf("want tool name to match template name, got %q", def.Function.Name)
	}
	props, ok := def.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("want a properties map, got %T", def.Function.Parameters["properties"])
	}
	custProp, ok := props["customer_id"].(map[string]any)
	if !ok || custProp["type"] != "integer" {
		t.Fatalf("want customer_id typed as integer, got %+v", props["customer_id"])
	}
	required, _ := def.Function.Parameters["required"].([]string)
	if len(required) != 1 || required[0] != "customer_id" {
		t.Fatalf("want customer_id required, got %+v", required)
	}
}

func TestConvertSQLTemplateParam(t *testing.T) {
	asRaw := func(v string) json.RawMessage { return json.RawMessage(v) }

	if v, err := convertSQLTemplateParam("integer", asRaw(`42`)); err != nil || v != int64(42) {
		t.Fatalf("integer: got %v, %v", v, err)
	}
	if v, err := convertSQLTemplateParam("number", asRaw(`3.5`)); err != nil || v != 3.5 {
		t.Fatalf("number: got %v, %v", v, err)
	}
	if v, err := convertSQLTemplateParam("boolean", asRaw(`true`)); err != nil || v != true {
		t.Fatalf("boolean: got %v, %v", v, err)
	}
	if v, err := convertSQLTemplateParam("string", asRaw(`"hello"`)); err != nil || v != "hello" {
		t.Fatalf("string: got %v, %v", v, err)
	}
	if _, err := convertSQLTemplateParam("integer", asRaw(`"not a number"`)); err == nil {
		t.Fatal("expected a type-mismatch error for integer parsed from a string")
	}
}

func TestMssqlTemplateToolExecutorMissingRequiredParam(t *testing.T) {
	tmpl := sqlQueryTemplate{
		Name: "get_customer_orders",
		SQL:  "SELECT * FROM Orders WHERE CustomerID = {customer_id}",
		Parameters: []sqlQueryParam{
			{Name: "customer_id", Type: "integer", Required: true},
		},
	}
	// No live DB configured — mssqlDSN will fail regardless, but the
	// missing-required-parameter check must happen (and error) before any
	// connection attempt is even made.
	exec := mssqlTemplateToolExecutor(mssqlConfig{}, tmpl)
	_, err := exec(nil, `{}`)
	if err == nil {
		t.Fatal("expected an error for a missing required parameter")
	}
}

// TestExampleArgsJSON exercises the "Vorlage testen" feature's argument
// builder: typed conversion per parameter, required-without-example is a
// hard error, optional-without-example is simply omitted.
func TestExampleArgsJSON(t *testing.T) {
	params := []sqlQueryParam{
		{Name: "customer_id", Type: "integer", Required: true, Example: "42"},
		{Name: "min_total", Type: "number", Required: false, Example: "9.5"},
		{Name: "active", Type: "boolean", Required: false, Example: "true"},
		{Name: "name", Type: "string", Required: false, Example: "Acme"},
		{Name: "unused_optional", Type: "string", Required: false},
	}
	argsJSON, err := exampleArgsJSON(params)
	if err != nil {
		t.Fatalf("exampleArgsJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &got); err != nil {
		t.Fatalf("decode result: %v (json=%s)", err, argsJSON)
	}
	if got["customer_id"] != float64(42) {
		t.Errorf("customer_id: want 42, got %v (%T)", got["customer_id"], got["customer_id"])
	}
	if got["min_total"] != 9.5 {
		t.Errorf("min_total: want 9.5, got %v", got["min_total"])
	}
	if got["active"] != true {
		t.Errorf("active: want true, got %v", got["active"])
	}
	if got["name"] != "Acme" {
		t.Errorf("name: want Acme, got %v", got["name"])
	}
	if _, ok := got["unused_optional"]; ok {
		t.Errorf("unused_optional: want omitted (no example, not required), got present: %v", got)
	}
}

// TestExampleArgsJSONRequiresExampleForRequiredParam guards the hard-error
// path: a required parameter with no Example has nothing sensible to test
// with.
func TestExampleArgsJSONRequiresExampleForRequiredParam(t *testing.T) {
	params := []sqlQueryParam{{Name: "customer_id", Type: "integer", Required: true}}
	if _, err := exampleArgsJSON(params); err == nil {
		t.Fatal("expected an error when a required parameter has no example value")
	}
}

// TestExampleArgsJSONRejectsUnparsableExample guards against a bad example
// value silently becoming a wrong-typed argument (e.g. an "integer"
// parameter whose Example isn't actually a number).
func TestExampleArgsJSONRejectsUnparsableExample(t *testing.T) {
	params := []sqlQueryParam{{Name: "customer_id", Type: "integer", Required: true, Example: "not-a-number"}}
	if _, err := exampleArgsJSON(params); err == nil {
		t.Fatal("expected an error for a non-numeric example on an integer parameter")
	}
}

// TestHandleTestMSSQLTemplateRejectsInvalidTemplate confirms the endpoint
// runs validateSQLQueryTemplates before ever attempting a connection —
// an invalid template name is rejected with ok=false, not a 500 or a real
// connection attempt.
func TestHandleTestMSSQLTemplateRejectsInvalidTemplate(t *testing.T) {
	body, _ := json.Marshal(mssqlTemplateTestRequest{
		Config:   mssqlConfig{Host: "localhost", Database: "db", Username: "u", Password: "p"},
		Template: sqlQueryTemplate{Name: "bad name!", SQL: "SELECT 1"},
	})
	rec := httptest.NewRecorder()
	handleTestMSSQLTemplate(rec, httptest.NewRequest(http.MethodPost, "/api/settings/test/mssql-template", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (test results are always 200, see writeTestResult), got %d", rec.Code)
	}
	var got testResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if got.OK {
		t.Fatalf("want ok=false for an invalid template name, got %+v", got)
	}
}

// TestHandleTestMSSQLTemplateRejectsMissingExample confirms a required
// parameter with no Example value is caught (via exampleArgsJSON) before
// any connection is attempted.
func TestHandleTestMSSQLTemplateRejectsMissingExample(t *testing.T) {
	body, _ := json.Marshal(mssqlTemplateTestRequest{
		Config: mssqlConfig{Host: "localhost", Database: "db", Username: "u", Password: "p"},
		Template: sqlQueryTemplate{
			Name:       "get_customer_orders",
			SQL:        "SELECT * FROM Orders WHERE CustomerID = {customer_id}",
			Parameters: []sqlQueryParam{{Name: "customer_id", Type: "integer", Required: true}},
			Enabled:    true,
		},
	})
	rec := httptest.NewRecorder()
	handleTestMSSQLTemplate(rec, httptest.NewRequest(http.MethodPost, "/api/settings/test/mssql-template", bytes.NewReader(body)))
	var got testResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if got.OK {
		t.Fatalf("want ok=false when a required parameter has no example value, got %+v", got)
	}
}

// TestHandleTestMSSQLTemplateRejectsGetMethod confirms the endpoint only
// accepts POST, same as every other test-connection handler.
func TestHandleTestMSSQLTemplateRejectsGetMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTestMSSQLTemplate(rec, httptest.NewRequest(http.MethodGet, "/api/settings/test/mssql-template", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}
