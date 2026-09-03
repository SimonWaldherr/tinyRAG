package main

import (
	"context"
	"strings"
	"testing"
)

func TestValidateSelectOnly(t *testing.T) {
	cases := []struct {
		query   string
		wantErr bool
	}{
		{"SELECT * FROM Customers", false},
		{"select id, name from customers where id = 1", false},
		{"  SELECT 1;  ", false}, // single trailing semicolon is fine
		{"WITH recent AS (SELECT * FROM Orders) SELECT * FROM recent", false},
		{"", true},
		{"DROP TABLE Customers", true},
		{"SELECT * FROM Customers; DROP TABLE Customers", true},
		{"INSERT INTO Customers VALUES (1)", true},
		{"UPDATE Customers SET Name = 'x'", true},
		{"EXEC sp_who", true},
		{"SELECT * FROM Customers; EXEC xp_cmdshell 'dir'", true},
		{"exec master..xp_cmdshell 'dir'", true},
		{"MERGE INTO Customers USING Staging ON (1=1)", true},
		{"SELECT * INTO copied_customers FROM Customers", true},
		{"SELECT 'DELETE is just text' AS note", false},
		{"-- DELETE is a comment\nSELECT 1", false},
		{"SELECT 1 /* DROP TABLE Customers */", false},
		{"SELECT 1; -- trailing note", false},
	}
	for _, c := range cases {
		err := validateSelectOnly(c.query)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSelectOnly(%q): err=%v, wantErr=%v", c.query, err, c.wantErr)
		}
	}
}

func TestMSSQLDSNRequiresCredentials(t *testing.T) {
	if _, err := mssqlDSN(mssqlConfig{}); err == nil {
		t.Fatal("expected an error for an empty config")
	}
	cfg := mssqlConfig{Host: "sql.example.com", Database: "R3", Username: "reader", Password: "s3cret", Port: 1433}
	dsn, err := mssqlDSN(cfg)
	if err != nil {
		t.Fatalf("mssqlDSN: %v", err)
	}
	if !strings.HasPrefix(dsn, "sqlserver://reader:s3cret@sql.example.com:1433?") {
		t.Fatalf("unexpected DSN shape: %s", dsn)
	}
	if !strings.Contains(dsn, "database=R3") {
		t.Fatalf("expected database=R3 in DSN, got %s", dsn)
	}
}

func TestMSSQLConfigResolvedPasswordPrefersEnv(t *testing.T) {
	t.Setenv("R3_TEST_MSSQL_PASSWORD", "from-env")
	cfg := mssqlConfig{Password: "inline", PasswordEnv: "R3_TEST_MSSQL_PASSWORD"}
	if got := cfg.resolvedPassword(); got != "from-env" {
		t.Fatalf("want from-env, got %q", got)
	}
}

// TestRunMSSQLQueryRejectsBeforeConnecting verifies validateSelectOnly
// runs before any network I/O — a disallowed statement never reaches a
// real database, regardless of whether one is even configured/reachable.
// There is no SQL Server reachable from this sandbox, so the "valid SELECT
// against a real database" path (connection, query execution, row
// formatting) is untested here and needs verification against a real/
// staging instance.
func TestRunMSSQLQueryRejectsBeforeConnecting(t *testing.T) {
	cfg := mssqlConfig{Host: "sql.example.invalid", Database: "R3", Username: "reader", Password: "x"}
	_, err := runMSSQLQuery(context.Background(), cfg, "DROP TABLE Customers")
	if err == nil {
		t.Fatal("expected a validation error, not a connection attempt")
	}
	if strings.Contains(err.Error(), "mssql: open") || strings.Contains(err.Error(), "mssql: query") {
		t.Fatalf("expected a validation error before any connection attempt, got %v", err)
	}
}

func TestMSSQLToolExecutorDecodesArguments(t *testing.T) {
	exec := mssqlToolExecutor(mssqlConfig{Host: "sql.example.invalid", Database: "R3", Username: "reader", Password: "x"})
	if _, err := exec(context.Background(), "not json"); err == nil {
		t.Fatal("expected an error for invalid JSON arguments")
	}
	// A validation failure surfaces from the executor too — the query
	// still never reaches a connection attempt.
	if _, err := exec(context.Background(), `{"query": "DELETE FROM Customers"}`); err == nil {
		t.Fatal("expected a validation error for a non-SELECT query")
	}
}

// TestValidateSQLQueryTemplatesRejectsLiteralOnlyParameter is a regression
// test for a real, reported bug: a template like "LIKE '%{kunde}%'" looks
// like it correctly uses the {kunde} parameter, and the pre-existing
// strings.Contains(t.SQL, "{kunde}") check agreed — but SQL Server parses
// text inside single quotes as a literal, never substituting a parameter
// there, so mssqlTemplateToolExecutor's sql.Named("kunde", ...) binding
// never actually takes effect and the query silently returns no rows for
// every input value. Confirmed against a real deployment: the exact same
// SQL, run directly with the value typed in by hand instead of bound as a
// parameter, returned real matching rows.
func TestValidateSQLQueryTemplatesRejectsLiteralOnlyParameter(t *testing.T) {
	broken := []sqlQueryTemplate{{
		Name: "sap_customer",
		SQL:  "SELECT KUNNR, NAME1 FROM KNA1 WHERE NAME1+NAME2 LIKE '%{kunde}%'",
		Parameters: []sqlQueryParam{
			{Name: "kunde", Type: "string", Required: true},
		},
		Enabled: true,
	}}
	err := validateSQLQueryTemplates(broken)
	if err == nil {
		t.Fatal("expected an error for a parameter referenced only inside a string literal")
	}
	if !strings.Contains(err.Error(), "string literal") {
		t.Fatalf("expected the error to explain the string-literal problem, got: %v", err)
	}

	// The fix (string concatenation, a genuine parameter reference outside
	// any literal) must be accepted.
	fixed := []sqlQueryTemplate{{
		Name: "sap_customer",
		SQL:  "SELECT KUNNR, NAME1 FROM KNA1 WHERE NAME1+NAME2 LIKE '%' + {kunde} + '%'",
		Parameters: []sqlQueryParam{
			{Name: "kunde", Type: "string", Required: true},
		},
		Enabled: true,
	}}
	if err := validateSQLQueryTemplates(fixed); err != nil {
		t.Fatalf("expected the concatenated form to be valid, got: %v", err)
	}

	// A parameter used in a normal (non-literal) comparison alongside an
	// unrelated string literal elsewhere in the query must not be flagged
	// just because the SQL happens to contain quotes somewhere.
	mixed := []sqlQueryTemplate{{
		Name: "order_status",
		SQL:  "SELECT * FROM Orders WHERE CustomerID = {id} AND Status <> 'cancelled'",
		Parameters: []sqlQueryParam{
			{Name: "id", Type: "integer", Required: true},
		},
		Enabled: true,
	}}
	if err := validateSQLQueryTemplates(mixed); err != nil {
		t.Fatalf("expected a parameter used outside any literal to be valid, got: %v", err)
	}
}

func TestMaskedColumnSet(t *testing.T) {
	cols := []string{"ID", "Email", "CustomerName", "Phone"}

	got := maskedColumnSet([]string{"email", "PHONE"}, cols)
	want := []bool{false, true, false, true}
	for i := range cols {
		if got[i] != want[i] {
			t.Errorf("maskedColumnSet: col %q: got masked=%v, want %v", cols[i], got[i], want[i])
		}
	}

	// No mask_columns configured: nothing masked, not even an empty slice
	// mismatch — every entry must be false.
	none := maskedColumnSet(nil, cols)
	for i, m := range none {
		if m {
			t.Errorf("maskedColumnSet(nil, ...): col %q unexpectedly masked", cols[i])
		}
	}

	// A configured name that doesn't match any actual column is simply a
	// no-op, not an error — an admin renaming/removing a column in the
	// underlying table shouldn't break unrelated queries.
	noMatch := maskedColumnSet([]string{"nonexistent_column"}, cols)
	for i, m := range noMatch {
		if m {
			t.Errorf("maskedColumnSet with unmatched name: col %q unexpectedly masked", cols[i])
		}
	}
}
