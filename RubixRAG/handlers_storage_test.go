package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestDefaultStorageModeIsDisk pins the v0.20.0 default flip: new deployments
// must default to "disk" (keeps R3's one hot chunks table resident, no
// per-query HNSW/FTS rebuild cliff), not the pre-v0.20.0 "hybrid". Guards
// against a silent revert of settings.go's default.
func TestDefaultStorageModeIsDisk(t *testing.T) {
	if got := defaultSettings("http://x", "chat", "embed", "de", 800, 5).Storage.Mode; got != "disk" {
		t.Fatalf("default storage mode: want \"disk\", got %q", got)
	}
}

// TestTinySQLStorageStatsDiskMode exercises the disk backend end-to-end (the
// new default) plus the storageStats() telemetry the admin endpoint serves:
// after ingesting chunks a vector search must work, Chunks must match, the
// mode is reported, and Oversized is false (disk keeps tables resident, so it
// has no memory bound to exceed).
func TestTinySQLStorageStatsDiskMode(t *testing.T) {
	s, err := newTinySQLStore(storageSettings{Mode: "disk", Path: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatalf("newTinySQLStore: %v", err)
	}
	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: []string{"a", "b", "c"}}
	if _, err := s.insertChunks(sc, "model-a", [][]float64{{1, 0}, {0, 1}, {1, 1}}, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	hits, err := s.vectorCandidates([]float64{1, 0}, "model-a", 1)
	if err != nil || len(hits) == 0 {
		t.Fatalf("vectorCandidates in disk mode: hits=%d err=%v", len(hits), err)
	}
	st := s.storageStats()
	if st.Backend != "tinysql" || st.Mode != "disk" {
		t.Fatalf("want backend=tinysql mode=disk, got backend=%q mode=%q", st.Backend, st.Mode)
	}
	if st.Chunks != 3 {
		t.Fatalf("want Chunks=3, got %d", st.Chunks)
	}
	if st.Oversized {
		t.Fatalf("disk mode must never report Oversized (it has no memory bound)")
	}
}

// TestTinySQLStorageStatsOversizedHybrid triggers the actual oversized/thrash
// condition the whole safeguard is about: a hybrid store with a 1 MiB budget
// holding several MiB of vectors on disk. storageStats().Oversized (and, at
// startup, warnIfOversized) must report it — this is the case where every
// search would re-decode the table and rebuild the HNSW/FTS caches.
func TestTinySQLStorageStatsOversizedHybrid(t *testing.T) {
	s, err := newTinySQLStore(storageSettings{Mode: "hybrid", Path: filepath.Join(t.TempDir(), "data"), MaxMemoryMB: 1})
	if err != nil {
		t.Fatalf("newTinySQLStore: %v", err)
	}
	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	const (
		rows = 3000
		dim  = 128
	)
	vec := make([]float64, dim)
	for i := range vec {
		vec[i] = float64(i) + 0.5
	}
	chunks := make([]string, rows)
	vecs := make([][]float64, rows)
	for i := range chunks {
		chunks[i] = "chunk"
		vecs[i] = vec
	}
	sc := sourceChunks{SourceID: "doc-1", SourceKind: "file", SourceName: "n", Chunks: chunks}
	if _, err := s.insertChunks(sc, "model-a", vecs, "load-1", 1000, "hash-1"); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	st := s.storageStats()
	if !st.Oversized {
		t.Fatalf("expected Oversized=true (disk=%d MiB should exceed limit=%d MiB in hybrid), got %+v",
			st.DiskUsedMB, st.MemoryLimitMB, st)
	}
}

// TestHandleStorageStats checks the admin endpoint's contract: a decodable
// storageStats body for a tinySQL-backed rag, a 405 for a non-GET, and a
// graceful {"supported": false} for a backend that doesn't implement
// storageStatser (the sqlite backend genuinely doesn't).
func TestHandleStorageStats(t *testing.T) {
	s, err := newTinySQLStore(storageSettings{Mode: "disk", Path: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatalf("newTinySQLStore: %v", err)
	}
	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	rag := &ragSystem{store: s}

	rec := httptest.NewRecorder()
	handleStorageStats(rag)(rec, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var got storageStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode storageStats: %v (body=%s)", err, rec.Body.String())
	}
	if got.Backend != "tinysql" || got.Mode != "disk" {
		t.Fatalf("want backend=tinysql mode=disk, got %+v", got)
	}

	rec = httptest.NewRecorder()
	handleStorageStats(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/admin/storage", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: want 405, got %d", rec.Code)
	}

	sq, err := newSQLiteStore(storageSettings{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("newSQLiteStore: %v", err)
	}
	rec = httptest.NewRecorder()
	handleStorageStats(&ragSystem{store: sq})(rec, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode fallback: %v (body=%s)", err, rec.Body.String())
	}
	if payload["supported"] != false {
		t.Fatalf("want supported=false for a non-statser backend, got %v", payload)
	}
}

// TestValidateStorageSettings exercises the enum/range guard handleSettings
// runs before ever persisting Storage to settings.json — see
// vectorstore.go's doc comment for why this is the only validation point
// (Backend/Mode/Path/MaxMemoryMB only take effect on the next restart, so
// there's no later "live apply" step that could surface a typo more
// visibly).
func TestValidateStorageSettings(t *testing.T) {
	cases := []struct {
		name    string
		s       storageSettings
		wantErr bool
	}{
		{"empty is valid (all defaults)", storageSettings{}, false},
		{"tinysql/disk valid", storageSettings{Backend: "tinysql", Mode: "disk"}, false},
		{"sqlite valid, mode ignored", storageSettings{Backend: "sqlite", Mode: "hybrid"}, false},
		{"case-insensitive", storageSettings{Backend: "TinySQL", Mode: "DISK"}, false},
		{"unknown backend", storageSettings{Backend: "postgres"}, true},
		{"unknown mode", storageSettings{Mode: "columnar"}, true},
		{"negative max_memory_mb", storageSettings{MaxMemoryMB: -1}, true},
		{"zero max_memory_mb valid (means default)", storageSettings{MaxMemoryMB: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStorageSettings(c.s)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateStorageSettings(%+v): wantErr=%v, got %v", c.s, c.wantErr, err)
			}
		})
	}
}

// TestHandleSettingsPersistsStorage confirms Storage now round-trips through
// POST /api/settings — until this change handleSettings silently dropped it
// (see the merge function's doc comment) even though the JSON field already
// existed. Also checks the trim+lowercase normalization applied on save.
func TestHandleSettingsPersistsStorage(t *testing.T) {
	rag, s := newTestRAG(t)
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"storage": map[string]any{
			"backend":       " TinySQL ",
			"mode":          " Disk ",
			"path":          " r3-data-custom ",
			"max_memory_mb": 512,
		},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := settings.get().Storage
	want := storageSettings{Backend: "tinysql", Mode: "disk", Path: "r3-data-custom", MaxMemoryMB: 512}
	if got != want {
		t.Fatalf("Storage after save: want %+v, got %+v", want, got)
	}

	// And it survives a GET round-trip (maskedSettings doesn't touch it —
	// Storage has no secrets to redact).
	rec = httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var out appSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode GET /api/settings: %v (body=%s)", err, rec.Body.String())
	}
	if out.Storage != want {
		t.Fatalf("Storage via GET: want %+v, got %+v", want, out.Storage)
	}
}

// TestHandleSettingsRejectsInvalidStorageSettings guards the 400 path: an
// unknown Mode/Backend or a negative MaxMemoryMB must be rejected before it
// can reach settings.json, and the previously saved Storage must survive
// unchanged (the whole save is rejected, not partially applied).
func TestHandleSettingsRejectsInvalidStorageSettings(t *testing.T) {
	rag, s := newTestRAG(t)
	s.Storage = storageSettings{Backend: "tinysql", Mode: "disk", Path: "r3-data", MaxMemoryMB: 256}
	withTestGlobalSettings(t, s)

	body, _ := json.Marshal(map[string]any{
		"storage": map[string]any{"mode": "columnar"},
	})
	rec := httptest.NewRecorder()
	handleSettings(rag)(rec, httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown storage mode, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := settings.get().Storage; got != s.Storage {
		t.Fatalf("a rejected save must not change Storage: want %+v, got %+v", s.Storage, got)
	}
}
