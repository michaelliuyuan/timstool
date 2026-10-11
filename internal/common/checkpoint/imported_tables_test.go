package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportedTablesRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := m.GetImportedTables(); got != 0 {
		t.Errorf("initial ImportedTables = %d, want 0", got)
	}

	if err := m.SetImportedTables(3); err != nil {
		t.Fatalf("SetImportedTables: %v", err)
	}
	if got := m.GetImportedTables(); got != 3 {
		t.Errorf("GetImportedTables = %d, want 3", got)
	}

	if err := m.SetImportedTables(0); err != nil {
		t.Fatalf("SetImportedTables(0): %v", err)
	}
	if got := m.GetImportedTables(); got != 0 {
		t.Errorf("GetImportedTables after reset = %d, want 0", got)
	}
}

func TestImportedTablesPersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetImportedTables(7); err != nil {
		t.Fatalf("SetImportedTables: %v", err)
	}

	// The on-disk JSON must carry imported_tables.
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	v, ok := doc["imported_tables"]
	if !ok {
		t.Fatal("checkpoint.json missing imported_tables key")
	}
	if num, ok := v.(float64); !ok || int(num) != 7 {
		t.Errorf("imported_tables = %v, want 7", v)
	}

	// A fresh manager over the same dir reads the value back.
	m2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.GetImportedTables(); got != 7 {
		t.Errorf("reloaded ImportedTables = %d, want 7", got)
	}
}

func TestImportModeRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := m.GetImportMode(); got != "" {
		t.Errorf("initial ImportMode = %q, want empty", got)
	}

	if err := m.SetImportMode(ImportModeLightning); err != nil {
		t.Fatalf("SetImportMode: %v", err)
	}
	if got := m.GetImportMode(); got != ImportModeLightning {
		t.Errorf("GetImportMode = %q, want %q", got, ImportModeLightning)
	}

	// Overwrite semantics (lightning → stream fallback chain).
	if err := m.SetImportMode(ImportModeStream); err != nil {
		t.Fatalf("SetImportMode(stream): %v", err)
	}
	if got := m.GetImportMode(); got != ImportModeStream {
		t.Errorf("GetImportMode after overwrite = %q, want %q", got, ImportModeStream)
	}

	// Persisted on disk as import_mode and read back by a fresh manager.
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if v, _ := doc["import_mode"].(string); v != ImportModeStream {
		t.Errorf("checkpoint.json import_mode = %v, want %q", doc["import_mode"], ImportModeStream)
	}
	m2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.GetImportMode(); got != ImportModeStream {
		t.Errorf("reloaded ImportMode = %q, want %q", got, ImportModeStream)
	}
}
