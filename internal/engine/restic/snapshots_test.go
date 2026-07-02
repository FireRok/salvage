package restic

import (
	"strings"
	"testing"
	"time"
)

// Realistic `restic snapshots --json` output: unordered in the file to prove
// the parser sorts, with the ids restic actually emits.
const snapshotsJSON = `[
  {"time":"2026-06-28T02:00:01.123456789Z","tree":"t1","paths":["/data"],"hostname":"h","username":"u","id":"aaaa111122223333","short_id":"aaaa1111"},
  {"time":"2026-06-30T02:00:01Z","tree":"t3","paths":["/data"],"hostname":"h","username":"u","id":"cccc111122223333","short_id":"cccc1111"},
  {"time":"2026-06-29T02:00:01Z","tree":"t2","paths":["/data"],"hostname":"h","username":"u","id":"bbbb111122223333","short_id":"bbbb1111"}
]`

// Spec 0029 R2: snapshots parse to []spi.Backup ordered newest first, labeled
// by short id, with the fixed display-only "snapshot" type.
func TestParseSnapshotsNewestFirst(t *testing.T) {
	backups, err := parseSnapshots(snapshotsJSON)
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("got %d backups, want 3", len(backups))
	}
	wantOrder := []string{"cccc1111", "bbbb1111", "aaaa1111"}
	for i, want := range wantOrder {
		if backups[i].Label != want {
			t.Errorf("backups[%d].Label = %q, want %q (newest first)", i, backups[i].Label, want)
		}
		if backups[i].Type != "snapshot" {
			t.Errorf("backups[%d].Type = %q, want snapshot", i, backups[i].Type)
		}
	}
	want := time.Date(2026, 6, 30, 2, 0, 1, 0, time.UTC)
	if !backups[0].Timestamp.Equal(want) {
		t.Errorf("newest Timestamp = %v, want %v", backups[0].Timestamp, want)
	}
}

// The metadata command merges stderr, so cache warnings may precede the JSON;
// the parser must skip the preamble.
func TestParseSnapshotsSkipsWarningPreamble(t *testing.T) {
	out := "unable to open cache: unable to locate cache directory\n" + snapshotsJSON
	backups, err := parseSnapshots(out)
	if err != nil {
		t.Fatalf("parseSnapshots with preamble: %v", err)
	}
	if len(backups) != 3 || backups[0].Label != "cccc1111" {
		t.Errorf("preamble broke parsing: %+v", backups)
	}
}

// A missing short_id falls back to the truncated full id.
func TestParseSnapshotsShortIDFallback(t *testing.T) {
	backups, err := parseSnapshots(`[{"time":"2026-06-30T02:00:01Z","id":"deadbeefcafef00d"}]`)
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if backups[0].Label != "deadbeef" {
		t.Errorf("Label = %q, want the 8-char truncated id", backups[0].Label)
	}
}

func TestParseSnapshotsEmptyRepo(t *testing.T) {
	backups, err := parseSnapshots("[]")
	if err != nil {
		t.Fatalf("parseSnapshots on an empty repo: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("got %d backups, want 0", len(backups))
	}
}

func TestParseSnapshotsGarbage(t *testing.T) {
	if _, err := parseSnapshots("Fatal: wrong password or no key found"); err == nil {
		t.Error("parseSnapshots on non-JSON output: want an error, got nil")
	}
}

// A failed metadata call's reason is the Fatal/error line, not the exit code.
func TestCommandFailReason(t *testing.T) {
	out := "some noise\nFatal: wrong password or no key found\n"
	if got := commandFailReason(out, 1); got != "Fatal: wrong password or no key found" {
		t.Errorf("commandFailReason = %q, want the Fatal line", got)
	}
	if got := commandFailReason("", 3); !strings.Contains(got, "3") {
		t.Errorf("commandFailReason on empty output = %q, want the exit code", got)
	}
}
