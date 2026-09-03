package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestModuleSettings(t *testing.T) *settingsStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	ss, err := loadOrCreateSettings(path, defaultSettingsFromFlags("http://x", "c", "e", "de", 500, 3))
	if err != nil {
		t.Fatalf("loadOrCreateSettings failed: %v", err)
	}
	return ss
}

func TestStripXMLTagsPreservesDocxParagraphAndTableBoundaries(t *testing.T) {
	docxXML := `<w:document><w:body>` +
		`<w:p><w:r><w:t>Erster Absatz.</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Zweiter Absatz.</w:t></w:r></w:p>` +
		`<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>A</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr>` +
		`</w:tbl>` +
		`</w:body></w:document>`
	out := stripXMLTags(docxXML)
	lines := strings.Split(out, "\n")

	if !containsLine(lines, "Erster Absatz.") || !containsLine(lines, "Zweiter Absatz.") {
		t.Fatalf("expected paragraphs on separate lines, got %q", out)
	}
	if !containsLine(lines, "A") || !containsLine(lines, "B") || !containsLine(lines, "1") || !containsLine(lines, "2") {
		t.Fatalf("expected table cell text preserved, got %q", out)
	}
	// A and B must land on the same row (before the next row's "1"), i.e.
	// row boundaries survived as line breaks distinct from cell content.
	aIdx, bIdx, oneIdx := indexOfLine(lines, "A"), indexOfLine(lines, "B"), indexOfLine(lines, "1")
	if aIdx < 0 || bIdx < 0 || oneIdx < 0 || !(aIdx < oneIdx && bIdx < oneIdx) {
		t.Fatalf("expected row 'A|B' entirely before row '1|2', got lines %+v", lines)
	}
}

func TestStripXMLTagsPreservesXLSXRowBoundaries(t *testing.T) {
	xlsxXML := `<sheetData>` +
		`<row><c><v>Name</v></c><c><v>Wert</v></c></row>` +
		`<row><c><v>Alpha</v></c><c><v>1</v></c></row>` +
		`<row><c><v>Beta</v></c><c><v>2</v></c></row>` +
		`</sheetData>`
	out := stripXMLTags(xlsxXML)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected each spreadsheet row on its own line, got %d line(s): %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "Name") || !strings.Contains(lines[0], "Wert") {
		t.Errorf("expected header row intact on one line, got %q", lines[0])
	}
}

func TestStripXMLTagsCollapsesNonBoundaryTagsToSpace(t *testing.T) {
	// Unchanged historical behavior: a tag that isn't a recognized
	// paragraph/row/cell boundary still collapses to a plain space, not a
	// newline, and stays on one line.
	out := stripXMLTags(`<span class="x">Hello</span> <b>World</b>`)
	if strings.Contains(out, "\n") {
		t.Fatalf("ordinary inline tags must not introduce a line break, got %q", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "World") {
		t.Errorf("expected both words preserved, got %q", out)
	}
}

func containsLine(lines []string, want string) bool {
	return indexOfLine(lines, want) >= 0
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) == want {
			return i
		}
	}
	return -1
}

func TestNormalizeModulesFillsDefaults(t *testing.T) {
	out := normalizeModules(nil)
	if len(out) != len(defaultModules()) {
		t.Fatalf("expected %d default modules, got %d", len(defaultModules()), len(out))
	}
}

func TestNormalizeModulesPreservesCustomAndFillsMissing(t *testing.T) {
	custom := []moduleConfig{{ID: "module-sql", Name: "Custom SQL", Kind: "sql", Enabled: true}}
	out := normalizeModules(custom)

	var sqlMod, mailMod *moduleConfig
	for i := range out {
		switch out[i].ID {
		case "module-sql":
			sqlMod = &out[i]
		case "module-mail":
			mailMod = &out[i]
		}
	}
	if sqlMod == nil || sqlMod.Name != "Custom SQL" || !sqlMod.Enabled {
		t.Fatalf("custom module-sql should be preserved as-is, got %+v", sqlMod)
	}
	if mailMod == nil {
		t.Fatal("missing default module-mail should be appended")
	}
	if sqlMod.Config == nil {
		t.Error("nil Config map should be normalized to empty map")
	}
}

func TestModuleStoreCRUD(t *testing.T) {
	ss := newTestModuleSettings(t)
	store := newModuleStore(ss)

	mods := store.list()
	if len(mods) == 0 {
		t.Fatal("expected default modules to be listed")
	}

	updated, err := store.upsert(moduleConfig{ID: "module-http-folder", Name: "Renamed", Enabled: true})
	if err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if updated.Name != "Renamed" {
		t.Errorf("upsert should return the updated module, got %+v", updated)
	}
	got, ok := store.get("module-http-folder")
	if !ok || got.Name != "Renamed" {
		t.Fatalf("get after upsert mismatch: %+v ok=%v", got, ok)
	}

	newMod, err := store.upsert(moduleConfig{ID: "module-custom", Name: "", Kind: "sql"})
	if err != nil {
		t.Fatalf("upsert new module failed: %v", err)
	}
	if newMod.Name != "module-custom" {
		t.Errorf("empty name should fall back to id, got %q", newMod.Name)
	}

	if _, err := store.upsert(moduleConfig{ID: "  "}); err == nil {
		t.Error("empty id should be rejected")
	}
}

func TestModuleStoreEnabledTools(t *testing.T) {
	ss := newTestModuleSettings(t)
	store := newModuleStore(ss)

	if _, err := store.upsert(moduleConfig{ID: "module-sql", Kind: "sql", Enabled: true}); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if _, err := store.upsert(moduleConfig{ID: "module-mail", Kind: "mail", Enabled: false}); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	tools := store.enabledTools()
	foundSQL, foundMail := false, false
	for _, tool := range tools {
		if tool.Name == "module:module-sql" {
			foundSQL = true
		}
		if tool.Name == "module:module-mail" {
			foundMail = true
		}
	}
	if !foundSQL {
		t.Error("enabled sql module should produce a tool")
	}
	if foundMail {
		t.Error("disabled mail module must not produce a tool")
	}
}

func TestParseBoolString(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "yes", "on", " On "}
	falsy := []string{"0", "false", "no", "off", "", "garbage"}
	for _, v := range truthy {
		if !parseBoolString(v) {
			t.Errorf("parseBoolString(%q) should be true", v)
		}
	}
	for _, v := range falsy {
		if parseBoolString(v) {
			t.Errorf("parseBoolString(%q) should be false", v)
		}
	}
}

func TestParseIntString(t *testing.T) {
	if got := parseIntString("42", 5); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := parseIntString("not-a-number", 5); got != 5 {
		t.Errorf("expected fallback 5, got %d", got)
	}
	if got := parseIntString("-3", 5); got != 5 {
		t.Errorf("negative values should fall back, got %d", got)
	}
	if got := parseIntString("0", 5); got != 5 {
		t.Errorf("zero should fall back, got %d", got)
	}
}

func TestAllowedTextExtensions(t *testing.T) {
	allowed := allowedTextExtensions()
	for _, ext := range []string{".txt", ".md", ".json", ".go"} {
		if !allowed[ext] {
			t.Errorf("expected %q to be an allowed extension", ext)
		}
	}
	if allowed[".exe"] {
		t.Error(".exe must not be an allowed text extension")
	}
}
