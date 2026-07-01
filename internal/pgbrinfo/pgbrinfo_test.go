package pgbrinfo

import "testing"

const sample = `[
  {
    "name": "demo",
    "backup": [
      {"label": "20260101-000000F", "type": "full", "timestamp": {"start": 1735689600, "stop": 1735689660}},
      {"label": "20260103-000000F_20260104-000000I", "type": "incr", "timestamp": {"start": 1735948800, "stop": 1735948860}},
      {"label": "20260102-000000F", "type": "full", "timestamp": {"start": 1735776000, "stop": 1735776060}}
    ]
  },
  {"name": "other", "backup": [{"label": "x", "type": "full", "timestamp": {"start": 1, "stop": 2}}]}
]`

func TestParseNewestFirst(t *testing.T) {
	got, err := Parse([]byte(sample), "demo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"20260103-000000F_20260104-000000I", // stop 1735948860 (newest)
		"20260102-000000F",                  // stop 1735776060
		"20260101-000000F",                  // stop 1735689660 (oldest)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d backups, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Label != want[i] {
			t.Errorf("position %d = %q, want %q (ordering wrong)", i, got[i].Label, want[i])
		}
	}
	if got[0].Type != "incr" {
		t.Errorf("newest type = %q, want incr", got[0].Type)
	}
	if got[0].Timestamp.Unix() != 1735948860 {
		t.Errorf("newest timestamp = %d, want 1735948860", got[0].Timestamp.Unix())
	}
}

func TestParseStanzaNotFound(t *testing.T) {
	if _, err := Parse([]byte(sample), "nope"); err == nil {
		t.Fatal("expected error for missing stanza")
	}
}

func TestParseBadJSON(t *testing.T) {
	if _, err := Parse([]byte("not json"), "demo"); err == nil {
		t.Fatal("expected error for bad json")
	}
}

const multiStanza = `[
  {
    "name": "beta",
    "status": {"code": 0, "message": "ok"},
    "backup": [
      {"label": "20260102-000000F", "type": "full", "timestamp": {"start": 1735776000, "stop": 1735776060}}
    ]
  },
  {
    "name": "alpha",
    "status": {"code": 0, "message": "ok"},
    "backup": [
      {"label": "20260101-000000F", "type": "full", "timestamp": {"start": 1735689600, "stop": 1735689660}},
      {"label": "20260103-000000F", "type": "full", "timestamp": {"start": 1735948800, "stop": 1735948860}}
    ]
  },
  {
    "name": "empty",
    "status": {"code": 2, "message": "no valid backups"},
    "backup": []
  }
]`

func TestStanzasSortedWithSummary(t *testing.T) {
	got, err := Stanzas([]byte(multiStanza))
	if err != nil {
		t.Fatalf("Stanzas: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d stanzas, want 3", len(got))
	}
	// Sorted by name.
	if got[0].Name != "alpha" || got[1].Name != "beta" || got[2].Name != "empty" {
		t.Fatalf("stanza order = %q/%q/%q, want alpha/beta/empty", got[0].Name, got[1].Name, got[2].Name)
	}
	// alpha's backups newest-first.
	if len(got[0].Backups) != 2 || got[0].Backups[0].Label != "20260103-000000F" {
		t.Errorf("alpha newest = %+v, want 20260103-000000F first", got[0].Backups)
	}
	nb, ok := got[0].Newest()
	if !ok || nb.Label != "20260103-000000F" {
		t.Errorf("alpha Newest() = %+v ok=%v, want 20260103-000000F", nb, ok)
	}
	// empty stanza: no backups, status surfaced.
	if _, ok := got[2].Newest(); ok {
		t.Error("empty stanza should have no newest backup")
	}
	if got[2].StatusMessage != "no valid backups" || got[2].StatusCode != 2 {
		t.Errorf("empty status = %q/%d, want 'no valid backups'/2", got[2].StatusMessage, got[2].StatusCode)
	}
}

func TestStanzasBadJSON(t *testing.T) {
	if _, err := Stanzas([]byte("not json")); err == nil {
		t.Fatal("expected error for bad json")
	}
}
