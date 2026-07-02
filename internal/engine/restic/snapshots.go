package restic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"salvage.sh/internal/engine/spi"
)

// resticSnapshot mirrors the fields of one element of `restic snapshots --json`
// that the chain walk needs. Time is RFC3339, which encoding/json parses natively.
type resticSnapshot struct {
	Time    time.Time `json:"time"`
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
}

// parseSnapshots parses `restic snapshots --json` output into []spi.Backup
// ordered NEWEST FIRST by snapshot time — the pure, testable half of Chain
// (spec 0029 R2), mirroring internal/pgbrinfo for pgBackRest.
//
// The output may carry non-JSON preamble (cache warnings; stderr is merged by
// snapshotsCmd), so parsing starts at the array's opening bracket. Label is the
// snapshot's short ID; Type is fixed to "snapshot" — restic snapshots are always
// independently restorable, so there is no full/incr/diff distinction (a benign,
// display-only difference from pgBackRest).
func parseSnapshots(out string) ([]spi.Backup, error) {
	i := strings.Index(out, "[")
	j := strings.LastIndex(out, "]")
	if i < 0 || j < i {
		return nil, fmt.Errorf("restic snapshots: no JSON array in output %q", firstLine(strings.TrimSpace(out)))
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(out[i:j+1]), &snaps); err != nil {
		return nil, fmt.Errorf("parse restic snapshots json: %w", err)
	}
	backups := make([]spi.Backup, 0, len(snaps))
	for _, s := range snaps {
		label := s.ShortID
		if label == "" {
			label = s.ID
			if len(label) > 8 {
				label = label[:8] // restic's own short-id width
			}
		}
		backups = append(backups, spi.Backup{Label: label, Type: "snapshot", Timestamp: s.Time.UTC()})
	}
	sort.SliceStable(backups, func(a, b int) bool { return backups[a].Timestamp.After(backups[b].Timestamp) })
	return backups, nil
}
