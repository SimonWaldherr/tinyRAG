package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func sampleCKANPackage() map[string]any {
	return map[string]any{
		"id":                "pkg-1",
		"name":              "air-quality",
		"title":             "Air Quality Measurements",
		"notes":             "Hourly particulate matter measurements from official sensors.",
		"license_id":        "cc-by",
		"license_title":     "Creative Commons Attribution",
		"metadata_created":  "2025-01-01T08:00:00",
		"metadata_modified": "2025-04-01T12:00:00",
		"organization": map[string]any{
			"title": "City Data Office",
		},
		"tags": []any{
			map[string]any{"name": "air"},
			map[string]any{"name": "environment"},
		},
		"resources": []any{
			map[string]any{
				"name":   "Hourly CSV",
				"format": "CSV",
				"url":    "https://data.example.org/air.csv",
				"schema": map[string]any{
					"fields": []any{
						map[string]any{
							"name":        "pm25",
							"type":        "number",
							"description": "Particulate matter in micrograms per cubic meter.",
						},
					},
				},
			},
		},
	}
}

func TestCKANDatasetCardIncludesLicenseSchemaAndGuidance(t *testing.T) {
	card := buildCKANDatasetCard("https://data.example.org", sampleCKANPackage())
	for _, want := range []string{
		"Dataset: Air Quality Measurements",
		"License: Creative Commons Attribution | cc-by",
		"pm25 (number)",
		"AI use guidance:",
		"City Data Office",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("expected card to contain %q, got:\n%s", want, card)
		}
	}

	meta := ckanPackageR3Metadata("https://data.example.org", sampleCKANPackage())
	if meta.SourceSystem != "ckan:data.example.org" {
		t.Fatalf("expected ckan source system, got %q", meta.SourceSystem)
	}
	if meta.SourceType != "official_dataset" || meta.Sensitivity != "public" {
		t.Fatalf("unexpected source metadata: %#v", meta)
	}
	if meta.TrustLevel < 0.79 || meta.SourceQuality < 0.89 {
		t.Fatalf("expected licensed dataset trust/quality boost, got trust %.2f quality %.2f", meta.TrustLevel, meta.SourceQuality)
	}
}

func TestAddChunksWithMetadataPersistsOKFFields(t *testing.T) {
	r, err := newRAG(r3MockLM{}, 3, "", tinysql.ModeMemory, 32)
	if err != nil {
		t.Fatalf("newRAG failed: %v", err)
	}
	if err := r.init(); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	s := &settingsStore{}
	s.s.EmbedModel = "embed"
	s.s.ActiveRole = "it"
	settings = s

	meta := R3IngestMetadata{
		DocumentID:         "okf-doc-1",
		SourceSystem:       "ckan:data.example.org",
		SourceType:         "official_dataset",
		SourceTitle:        "Air Quality Measurements",
		SourceURL:          "https://data.example.org/dataset/air-quality",
		SourceObjectID:     "air-quality",
		SourceVersion:      "2025-04-01T12:00:00",
		Sensitivity:        "public",
		TrustLevel:         0.80,
		SourceQuality:      0.90,
		OpenLinkAllowed:    true,
		OpenLinkAllowedSet: true,
		Provenance:         "https://data.example.org/dataset/air-quality",
		Ownership:          "City Data Office",
		TrustTier:          "licensed-open-data",
		RetentionPolicy:    "external-public-metadata",
	}
	if err := r.addChunksWithMetadata("Air Quality Measurements", []string{"Dataset card content."}, "embed", nil, meta); err != nil {
		t.Fatalf("addChunksWithMetadata failed: %v", err)
	}

	stmt, err := tinysql.ParseSQL("SELECT source_system, source_url, source_type, source_title, sensitivity, trust_level, open_link_allowed FROM chunks WHERE document_id = 'okf-doc-1'")
	if err != nil {
		t.Fatalf("parse SQL failed: %v", err)
	}
	rs, err := tinysql.Execute(context.Background(), r.db, "default", stmt)
	if err != nil || rs == nil || len(rs.Rows) != 1 {
		rows := 0
		if rs != nil {
			rows = len(rs.Rows)
		}
		t.Fatalf("query chunks failed: %v rows=%d", err, rows)
	}
	row := rs.Rows[0]
	assertVal := func(col, want string) {
		gotRaw, _ := tinysql.GetVal(row, col)
		got := fmt.Sprint(gotRaw)
		if got != want {
			t.Fatalf("expected %s=%q, got %q", col, want, got)
		}
	}
	assertVal("source_system", "ckan:data.example.org")
	assertVal("source_url", "https://data.example.org/dataset/air-quality")
	assertVal("source_type", "official_dataset")
	assertVal("source_title", "Air Quality Measurements")
	assertVal("sensitivity", "public")
}
