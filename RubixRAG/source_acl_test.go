package main

import (
	"path/filepath"
	"testing"
)

func TestSourceACLAllowsOnlyConfiguredDepartmentOrUser(t *testing.T) {
	store, err := newSourceACLStore(filepath.Join(t.TempDir(), "source-acl.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.set("sharepoint:one", sourceACL{Departments: []string{" Vertrieb "}, Users: []string{"Ada@Example.test"}}); err != nil {
		t.Fatal(err)
	}
	if !store.allowed("sharepoint:one", "vertrieb", "") {
		t.Fatal("configured department must be allowed")
	}
	if !store.allowed("sharepoint:one", "", "ada@example.test") {
		t.Fatal("configured user must be allowed case-insensitively")
	}
	if store.allowed("sharepoint:one", "IT", "bob@example.test") {
		t.Fatal("unlisted department and user must be denied")
	}
	if !store.allowed("sharepoint:one", adminDeptCode, "") {
		t.Fatal("admin must retain access")
	}
	if !store.allowed("sharepoint:unconfigured", "IT", "bob@example.test") {
		t.Fatal("an absent ACL must inherit the broader source-kind policy")
	}
}

func TestSourceACLPersistsNormalizedRulesAndCanBeRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-acl.json")
	store, err := newSourceACLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.set("doc", sourceACL{Departments: []string{"IT", "it", ""}, Users: []string{" BOB ", "bob"}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newSourceACLStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := reloaded.get("doc")
	if !ok || len(rule.Departments) != 1 || rule.Departments[0] != "it" || len(rule.Users) != 1 || rule.Users[0] != "bob" {
		t.Fatalf("want normalized persisted rule, got %#v configured=%v", rule, ok)
	}
	if err := reloaded.set("doc", sourceACL{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.get("doc"); ok {
		t.Fatal("empty ACL must remove the document-specific rule")
	}
}

func TestDocumentACLCanNarrowButNeverWidenSourceKindAccess(t *testing.T) {
	store, err := newSourceACLStore(filepath.Join(t.TempDir(), "source-acl.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.set("doc", sourceACL{Users: []string{"ada@example.test"}}); err != nil {
		t.Fatal(err)
	}
	rag := &ragSystem{sourceACLs: store}
	access := map[string][]string{"sharepoint_file": {"IT"}}
	if rag.sourceAccessAllowed(access, "doc", "sharepoint_file", "Vertrieb", "ada@example.test") {
		t.Fatal("a document ACL must not widen a denied source-kind rule")
	}
	if !rag.sourceAccessAllowed(access, "doc", "sharepoint_file", "IT", "ada@example.test") {
		t.Fatal("a caller allowed by both layers must be able to read")
	}
	if rag.sourceAccessAllowed(access, "doc", "sharepoint_file", "IT", "bob@example.test") {
		t.Fatal("the document ACL must narrow an otherwise allowed source-kind rule")
	}
}
