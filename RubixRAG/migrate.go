package main

import "fmt"

// runMigration copies every chunk from src into dst verbatim — original
// provenance (source_id/kind/name/load_id/loaded_at/doc_date/chunk_idx/
// content_hash) and embedding vectors preserved byte-for-byte, so
// switching STORAGE_BACKEND doesn't require a working embedding endpoint
// or re-paying embedding cost for content already indexed once. See
// `-migrate-from-backend` (main.go) and `make migrate` (Makefile).
func runMigration(src, dst vectorStore) (int, error) {
	rows, err := src.exportAll()
	if err != nil {
		return 0, fmt.Errorf("export source: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := dst.importRaw(rows); err != nil {
		return 0, fmt.Errorf("import target: %w", err)
	}
	return len(rows), nil
}
