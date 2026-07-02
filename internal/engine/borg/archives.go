package borg

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"salvage.sh/internal/engine/spi"
)

// borgList mirrors the fields of `borg list --json` that the chain walk needs.
// The repository block is deliberately omitted: its location may have been
// supplied via a secret env var and is never echoed (see repoIdentity).
type borgList struct {
	Archives []borgArchive `json:"archives"`
}

// borgArchive is one archive entry. borg emits both "name" and "archive" with
// the same value across versions; either is accepted. Time is borg's ISO-8601
// local time without a zone (e.g. "2026-06-30T01:02:03.000000").
type borgArchive struct {
	Name    string `json:"name"`
	Archive string `json:"archive"`
	Time    string `json:"time"`
	Start   string `json:"start"`
}

// borgTimeLayouts are the accepted archive time formats, most specific first:
// borg's zoneless ISO-8601 (with and without fractional seconds), plus RFC3339
// in case a zone-aware variant appears.
var borgTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000000",
	"2006-01-02T15:04:05",
}

// parseArchives parses `borg list --json` output into []spi.Backup ordered
// NEWEST FIRST by archive time — the pure, testable half of Chain (spec 0029
// R3), mirroring internal/pgbrinfo for pgBackRest.
//
// The output may carry non-JSON preamble (warnings; stderr is merged by
// listCmd), so parsing starts at the object's opening brace. Label is the
// archive name — the explicit name a chain walk feeds to TestBackup, since borg
// has no "latest" alias. Type is fixed to "archive" (display-only; every borg
// archive is independently extractable).
func parseArchives(out string) ([]spi.Backup, error) {
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return nil, fmt.Errorf("borg list: no JSON object in output %q", firstLine(strings.TrimSpace(out)))
	}
	var list borgList
	if err := json.Unmarshal([]byte(out[i:j+1]), &list); err != nil {
		return nil, fmt.Errorf("parse borg list json: %w", err)
	}
	backups := make([]spi.Backup, 0, len(list.Archives))
	for _, a := range list.Archives {
		name := a.Name
		if name == "" {
			name = a.Archive
		}
		raw := a.Time
		if raw == "" {
			raw = a.Start
		}
		ts, err := parseBorgTime(raw)
		if err != nil {
			return nil, fmt.Errorf("archive %q: %w", name, err)
		}
		backups = append(backups, spi.Backup{Label: name, Type: "archive", Timestamp: ts})
	}
	sort.SliceStable(backups, func(a, b int) bool { return backups[a].Timestamp.After(backups[b].Timestamp) })
	return backups, nil
}

// parseBorgTime parses an archive timestamp against the accepted layouts. A
// garbled time is an error rather than a zero value: the chain walk's ordering
// is the whole point, so silently mis-ordering candidates is worse than failing.
func parseBorgTime(raw string) (time.Time, error) {
	for _, layout := range borgTimeLayouts {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable archive time %q", raw)
}
