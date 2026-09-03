package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyDepartment(t *testing.T) {
	cases := []struct {
		name       string
		department string
		title      string
		want       string
	}{
		{"department matches directly", "Vertrieb Augsburg", "", "Vertrieb"},
		{"falls back to title when department is empty", "", "Category Manager", "Einkauf"},
		{"department wins over title when both match", "Fertigung", "Key Account Manager", "Fertigung"},
		{"department has no rule, title does", "Sonstiges", "Category Manager", "Einkauf"},
		{"nothing matches", "Reinigung Nachtschicht", "Hausmeister", defaultDepartmentCode},
		{"empty input", "", "", defaultDepartmentCode},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyDepartment(defaultDepartmentRules, c.department, c.title)
			if got != c.want {
				t.Fatalf("classifyDepartment(%q, %q) = %q, want %q", c.department, c.title, got, c.want)
			}
		})
	}
}

func TestValidateDepartmentRules(t *testing.T) {
	if err := validateDepartmentRules([]departmentRule{{Regex: "Vertrieb", Code: "Vertrieb"}}); err != nil {
		t.Fatalf("valid ruleset rejected: %v", err)
	}
	if err := validateDepartmentRules([]departmentRule{{Regex: "(unclosed", Code: "X"}}); err == nil {
		t.Fatal("invalid regex must be rejected")
	}
	if err := validateDepartmentRules([]departmentRule{{Regex: "Vertrieb", Code: ""}}); err == nil {
		t.Fatal("empty code must be rejected")
	}
}

func TestDepartmentRulesOrDefaultFallsBackWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	got := departmentRulesOrDefault(dir)
	if len(got) != len(defaultDepartmentRules) {
		t.Fatalf("want the built-in defaults when no override file exists, got %d rules", len(got))
	}
}

func TestDepartmentRulesOrDefaultUsesValidOverride(t *testing.T) {
	dir := t.TempDir()
	custom := []departmentRule{{Regex: "Mechatronics", Code: "Mechatronik"}}
	b, _ := os.ReadFile(filepath.Join(dir, departmentRulesFilename)) // sanity: file must not pre-exist
	if b != nil {
		t.Fatal("test setup: override file already exists")
	}
	writeDepartmentRulesFile(t, dir, custom)

	got := departmentRulesOrDefault(dir)
	if len(got) != 1 || got[0].Code != "Mechatronik" {
		t.Fatalf("want the custom override, got %+v", got)
	}
}

func TestDepartmentRulesOrDefaultFallsBackOnInvalidOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, departmentRulesFilename), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := departmentRulesOrDefault(dir)
	if len(got) != len(defaultDepartmentRules) {
		t.Fatalf("want fallback to built-in defaults on invalid JSON, got %d rules", len(got))
	}
}

// writeDepartmentRulesFile is a small test helper writing rules as
// department_rules.json into dir, matching what handleDepartmentRulesSave
// (department_admin.go) does for real.
func writeDepartmentRulesFile(t *testing.T, dir string, rules []departmentRule) {
	t.Helper()
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, departmentRulesFilename), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSourceAccessAllowed(t *testing.T) {
	access := map[string][]string{"imap_email": {"Vertrieb", "Einkauf"}}

	if !sourceAccessAllowed(access, "confluence_page", "") {
		t.Fatal("unrestricted kind must be allowed for anyone, including anonymous")
	}
	if sourceAccessAllowed(access, "imap_email", "") {
		t.Fatal("restricted kind must not be allowed for an anonymous caller")
	}
	if !sourceAccessAllowed(access, "imap_email", "vertrieb") {
		t.Fatal("match must be case-insensitive")
	}
	if sourceAccessAllowed(access, "imap_email", "IT") {
		t.Fatal("a department not in the allow-list must be denied")
	}
	if !sourceAccessAllowed(nil, "imap_email", "") {
		t.Fatal("an absent access map must allow everything (opt-out-not-opt-in)")
	}
}
