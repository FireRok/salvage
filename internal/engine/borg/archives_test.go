package borg

import (
	"strings"
	"testing"
	"time"
)

// Realistic `borg list --json` output: borg's zoneless ISO-8601 times,
// unordered in the file to prove the parser sorts. The repository block is
// present (as borg emits it) but deliberately unused.
const listJSON = `{
  "archives": [
    {"archive": "app-2026-06-28", "barchive": "app-2026-06-28", "id": "a1", "name": "app-2026-06-28", "start": "2026-06-28T02:00:00.000000", "time": "2026-06-28T02:00:00.000000"},
    {"archive": "app-2026-06-30", "barchive": "app-2026-06-30", "id": "a3", "name": "app-2026-06-30", "start": "2026-06-30T02:00:00.000000", "time": "2026-06-30T02:00:00.000000"},
    {"archive": "app-2026-06-29", "barchive": "app-2026-06-29", "id": "a2", "name": "app-2026-06-29", "start": "2026-06-29T02:00:00.000000", "time": "2026-06-29T02:00:00.000000"}
  ],
  "encryption": {"mode": "repokey"},
  "repository": {"id": "r1", "last_modified": "2026-06-30T02:00:05.000000", "location": "/srv/borg"}
}`

// Spec 0029 R3: archives parse to []spi.Backup ordered newest first, labeled by
// explicit archive name (borg has no "latest" alias), type "archive".
func TestParseArchivesNewestFirst(t *testing.T) {
	backups, err := parseArchives(listJSON)
	if err != nil {
		t.Fatalf("parseArchives: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("got %d backups, want 3", len(backups))
	}
	wantOrder := []string{"app-2026-06-30", "app-2026-06-29", "app-2026-06-28"}
	for i, want := range wantOrder {
		if backups[i].Label != want {
			t.Errorf("backups[%d].Label = %q, want %q (newest first)", i, backups[i].Label, want)
		}
		if backups[i].Type != "archive" {
			t.Errorf("backups[%d].Type = %q, want archive", i, backups[i].Type)
		}
	}
	want := time.Date(2026, 6, 30, 2, 0, 0, 0, time.UTC)
	if !backups[0].Timestamp.Equal(want) {
		t.Errorf("newest Timestamp = %v, want %v", backups[0].Timestamp, want)
	}
}

// The metadata command merges stderr, so warnings may precede the JSON; the
// parser must skip the preamble.
func TestParseArchivesSkipsWarningPreamble(t *testing.T) {
	out := "Warning: Attempting to access a previously unknown unencrypted repository!\n" + listJSON
	backups, err := parseArchives(out)
	if err != nil {
		t.Fatalf("parseArchives with preamble: %v", err)
	}
	if len(backups) != 3 || backups[0].Label != "app-2026-06-30" {
		t.Errorf("preamble broke parsing: %+v", backups)
	}
}

// A missing name falls back to the "archive" field; a missing time to "start".
func TestParseArchivesFieldFallbacks(t *testing.T) {
	backups, err := parseArchives(`{"archives":[{"archive":"only-archive-field","start":"2026-06-30T02:00:00"}]}`)
	if err != nil {
		t.Fatalf("parseArchives: %v", err)
	}
	if backups[0].Label != "only-archive-field" {
		t.Errorf("Label = %q, want the archive-field fallback", backups[0].Label)
	}
}

func TestParseArchivesEmptyRepo(t *testing.T) {
	backups, err := parseArchives(`{"archives": [], "repository": {"id": "r1"}}`)
	if err != nil {
		t.Fatalf("parseArchives on an empty repo: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("got %d backups, want 0", len(backups))
	}
}

// A garbled archive time is an error, not a silently mis-ordered chain.
func TestParseArchivesBadTime(t *testing.T) {
	_, err := parseArchives(`{"archives":[{"name":"a","time":"yesterday-ish"}]}`)
	if err == nil || !strings.Contains(err.Error(), "unparseable archive time") {
		t.Errorf("parseArchives with a bad time = %v, want an unparseable-time error", err)
	}
}

func TestParseArchivesGarbage(t *testing.T) {
	if _, err := parseArchives("Error: Repository /srv/borg does not exist."); err == nil {
		t.Error("parseArchives on non-JSON output: want an error, got nil")
	}
}

// A failed metadata call's reason is borg's error line, not the exit code.
func TestCommandFailReason(t *testing.T) {
	out := "some noise\npassphrase supplied in BORG_PASSPHRASE is incorrect\n"
	if got := commandFailReason(out, 2); got != "passphrase supplied in BORG_PASSPHRASE is incorrect" {
		t.Errorf("commandFailReason = %q, want borg's error line", got)
	}
}
