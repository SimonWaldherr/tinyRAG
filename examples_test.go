package main

import (
	"strings"
	"testing"
)

// TestExamplesFSContainsGallery verifies the static gallery survives the
// go:embed directive and stays servable at the path http.FileServer expects
// (examples/gallery.html, matching the /examples/gallery.html route).
func TestExamplesFSContainsGallery(t *testing.T) {
	data, err := examplesFS.ReadFile("examples/gallery.html")
	if err != nil {
		t.Fatalf("failed to read embedded examples/gallery.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, "<!doctype html>") {
		t.Error("gallery.html does not look like a full HTML document")
	}
	if !strings.Contains(html, `id="themeGrid"`) || !strings.Contains(html, `id="scenarioGrid"`) {
		t.Error("gallery.html is missing the theme/scenario grid containers")
	}
}

// TestExamplesFSGalleryDataMatchesGoDefaults guards against the static
// gallery's hardcoded THEMES/SCENARIOS arrays drifting out of sync with
// defaultCustomThemes()/scenarioTemplates() in ui_templates.go — it only
// checks id coverage, not exact colors (those would need a JS-in-Go parser).
func TestExamplesFSGalleryDataMatchesGoDefaults(t *testing.T) {
	data, err := examplesFS.ReadFile("examples/gallery.html")
	if err != nil {
		t.Fatalf("failed to read embedded examples/gallery.html: %v", err)
	}
	html := string(data)

	for _, th := range defaultCustomThemes() {
		if !strings.Contains(html, "id:'"+th.ID+"'") {
			t.Errorf("gallery.html is missing theme id %q — update examples/gallery.html's THEMES array", th.ID)
		}
	}
	for _, tmpl := range scenarioTemplates() {
		if !strings.Contains(html, "id:'"+tmpl.ID+"'") {
			t.Errorf("gallery.html is missing scenario id %q — update examples/gallery.html's SCENARIOS array", tmpl.ID)
		}
	}
}
