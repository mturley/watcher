package db

import (
	"testing"

	"github.com/mturley/watcher"
)

func TestResourceMeta_SetGet(t *testing.T) {
	conn := mem(t)
	if err := Migrate(conn); err != nil {
		t.Fatal(err)
	}
	r := watcher.Resource{Type: "slack", ID: "C123:1700000000.000100"}

	// Absent -> nil, nil
	got, err := GetResourceMeta(conn, r.Type, r.ID)
	if err != nil {
		t.Fatalf("GetResourceMeta(absent): %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for absent meta, got %+v", got)
	}

	// Set -> Get returns values
	if err := SetResourceMeta(conn, r, "Release blocker", "Tracking the e2e regression"); err != nil {
		t.Fatalf("SetResourceMeta: %v", err)
	}
	got, err = GetResourceMeta(conn, r.Type, r.ID)
	if err != nil {
		t.Fatalf("GetResourceMeta: %v", err)
	}
	if got == nil || got.CustomName != "Release blocker" || got.CustomDescription != "Tracking the e2e regression" {
		t.Fatalf("unexpected meta: %+v", got)
	}

	// Upsert overwrites
	if err := SetResourceMeta(conn, r, "Renamed", ""); err != nil {
		t.Fatalf("SetResourceMeta(upsert): %v", err)
	}
	got, _ = GetResourceMeta(conn, r.Type, r.ID)
	if got.CustomName != "Renamed" || got.CustomDescription != "" {
		t.Fatalf("upsert did not overwrite: %+v", got)
	}
}
