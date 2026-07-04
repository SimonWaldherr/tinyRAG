package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testConnector(id string) Connector {
	return Connector{
		ID: id, Name: "Test HTTP", Type: ConnectorTypeHTTP, Enabled: true,
		BaseURL: "https://example.com",
		Capabilities: []Capability{
			{Name: "search_" + id, Description: "search things", Type: CapabilityTypeTool,
				InputSchema: JSONSchema{Type: "object", Required: []string{"query"}}},
		},
	}
}

func TestConnectorStoreCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.json")
	store, err := newConnectorStore(path)
	if err != nil {
		t.Fatalf("newConnectorStore failed: %v", err)
	}
	if len(store.list()) != 0 {
		t.Fatal("expected empty store on fresh file")
	}

	c, err := store.upsert(testConnector(""))
	if err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected generated ID for empty ID input")
	}
	if len(store.list()) != 1 {
		t.Fatalf("expected 1 connector after upsert, got %d", len(store.list()))
	}

	got, ok := store.get(c.ID)
	if !ok || got.Name != "Test HTTP" {
		t.Fatalf("get mismatch: %+v ok=%v", got, ok)
	}

	c.Name = "Renamed"
	if _, err := store.upsert(c); err != nil {
		t.Fatalf("upsert (update) failed: %v", err)
	}
	got, _ = store.get(c.ID)
	if got.Name != "Renamed" {
		t.Errorf("expected renamed connector, got %+v", got)
	}

	removed, err := store.remove(c.ID)
	if err != nil || !removed {
		t.Fatalf("remove failed: removed=%v err=%v", removed, err)
	}
	if _, ok := store.get(c.ID); ok {
		t.Error("connector should be gone after remove")
	}
	if removed, _ := store.remove("nope"); removed {
		t.Error("removing unknown id should report false")
	}
}

func TestConnectorStoreUpsertValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.json")
	store, _ := newConnectorStore(path)

	if _, err := store.upsert(Connector{Name: "", Type: ConnectorTypeHTTP}); err == nil {
		t.Error("empty name must be rejected")
	}
	if _, err := store.upsert(Connector{Name: "x", Type: "ftp"}); err == nil {
		t.Error("invalid connector type must be rejected")
	}
	c, err := store.upsert(Connector{Name: "x", Type: ConnectorTypeSQL})
	if err != nil {
		t.Fatalf("valid sql connector should be accepted: %v", err)
	}
	if c.Config == nil || c.Headers == nil {
		t.Error("nil Config/Headers should be normalized to empty maps")
	}
}

func TestConnectorStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.json")
	store1, _ := newConnectorStore(path)
	if _, err := store1.upsert(testConnector("persisted")); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	store2, err := newConnectorStore(path)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len(store2.list()) != 1 {
		t.Fatalf("expected 1 connector after reload, got %d", len(store2.list()))
	}
}

func TestConnectorStoreEnabledToolDefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.json")
	store, _ := newConnectorStore(path)

	enabled := testConnector("enabled")
	disabled := testConnector("disabled")
	disabled.Enabled = false
	disabled.Capabilities[0].Name = "search_disabled"

	if _, err := store.upsert(enabled); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if _, err := store.upsert(disabled); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	tools := store.enabledToolDefs()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool from the enabled connector only, got %d", len(tools))
	}
	if tools[0].Name != "search_enabled" {
		t.Errorf("unexpected tool name %q", tools[0].Name)
	}
	if tools[0].ParamHint == "" || tools[0].ParamHint == "JSON input matching connector schema" {
		t.Errorf("single required field should produce a specific param hint, got %q", tools[0].ParamHint)
	}
}

func TestResolveSecret(t *testing.T) {
	t.Setenv("TINYRAG_TEST_SECRET", "s3cret")
	if got := resolveSecret("secret:TINYRAG_TEST_SECRET"); got != "s3cret" {
		t.Errorf("expected resolved secret, got %q", got)
	}
	if got := resolveSecret("plain-value"); got != "plain-value" {
		t.Errorf("non-secret value should pass through unchanged, got %q", got)
	}
	if got := resolveSecret("secret:DOES_NOT_EXIST_XYZ"); got != "" {
		t.Errorf("unset env var should resolve to empty string, got %q", got)
	}
}

func TestResolveConfigSecret(t *testing.T) {
	t.Setenv("TINYRAG_TEST_SECRET2", "hunter2")
	cfg := map[string]string{"password": "secret:TINYRAG_TEST_SECRET2", "user": "admin"}
	resolved := resolveConfigSecret(cfg)
	if resolved["password"] != "hunter2" || resolved["user"] != "admin" {
		t.Errorf("unexpected resolved config: %+v", resolved)
	}
	if resolveConfigSecret(nil) != nil {
		t.Error("nil config should resolve to nil")
	}
}

func TestConnectorTimeoutAndMaxBody(t *testing.T) {
	cases := []struct {
		in, want int
	}{{0, 10}, {5, 5}, {500, 120}, {-1, 10}}
	for _, c := range cases {
		conn := Connector{TimeoutSec: c.in}
		if got := int(conn.timeout().Seconds()); got != c.want {
			t.Errorf("timeout(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	var withDefault Connector
	if withDefault.maxBody() != 1<<20 {
		t.Errorf("expected 1MiB default, got %d", withDefault.maxBody())
	}
	withLimit := Connector{MaxBodyBytes: 4096}
	if withLimit.maxBody() != 4096 {
		t.Errorf("expected explicit limit 4096, got %d", withLimit.maxBody())
	}
}

func TestNewConnectorStoreHandlesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	store, err := newConnectorStore(path)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(store.list()) != 0 {
		t.Error("expected empty list for missing file")
	}
}

func TestNewConnectorStoreRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := newConnectorStore(path); err == nil {
		t.Error("corrupt connectors file should produce an error")
	}
}
