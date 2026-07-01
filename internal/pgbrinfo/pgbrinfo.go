// Package pgbrinfo parses `pgbackrest info --output=json` into ordered backup
// lists — the chain that last-known-good discovery walks newest→oldest, and the
// per-stanza summary that fleet discovery enumerates.
package pgbrinfo

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Backup is one pgBackRest backup in a stanza.
type Backup struct {
	Label     string
	Type      string    // full | incr | diff
	Timestamp time.Time // backup stop time
}

// Stanza is one pgBackRest stanza with its status and backups (newest first).
type Stanza struct {
	Name string
	// StatusCode is the stanza status code (0 = ok) from pgBackRest.
	StatusCode int
	// StatusMessage is the human-readable status ("ok", "missing stanza path", …).
	StatusMessage string
	// Backups are this stanza's backups ordered NEWEST FIRST (by stop time).
	Backups []Backup
}

// Newest returns the most recent backup, or false if the stanza has none.
func (s Stanza) Newest() (Backup, bool) {
	if len(s.Backups) == 0 {
		return Backup{}, false
	}
	return s.Backups[0], true
}

// rawStanza mirrors the shape of one element of `pgbackrest info --output=json`.
type rawStanza struct {
	Name   string `json:"name"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
	Backup []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Timestamp struct {
			Stop int64 `json:"stop"`
		} `json:"timestamp"`
	} `json:"backup"`
}

func decode(infoJSON []byte) ([]rawStanza, error) {
	var stanzas []rawStanza
	if err := json.Unmarshal(infoJSON, &stanzas); err != nil {
		return nil, fmt.Errorf("parse pgbackrest info json: %w", err)
	}
	return stanzas, nil
}

// backupsNewestFirst converts and sorts a raw stanza's backups by stop time,
// newest first.
func backupsNewestFirst(r rawStanza) []Backup {
	out := make([]Backup, 0, len(r.Backup))
	for _, b := range r.Backup {
		out = append(out, Backup{
			Label:     b.Label,
			Type:      b.Type,
			Timestamp: time.Unix(b.Timestamp.Stop, 0).UTC(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// Parse parses `pgbackrest info --output=json` output and returns the named
// stanza's backups ordered NEWEST FIRST (by backup stop time).
func Parse(infoJSON []byte, stanza string) ([]Backup, error) {
	stanzas, err := decode(infoJSON)
	if err != nil {
		return nil, err
	}
	for _, s := range stanzas {
		if s.Name == stanza {
			return backupsNewestFirst(s), nil
		}
	}
	return nil, fmt.Errorf("stanza %q not found in pgbackrest info", stanza)
}

// Stanzas parses `pgbackrest info --output=json` (run without --stanza, so all
// stanzas in the repo are reported) into a per-stanza summary ordered by stanza
// name. Used by fleet discovery to enumerate a whole repo.
func Stanzas(infoJSON []byte) ([]Stanza, error) {
	raw, err := decode(infoJSON)
	if err != nil {
		return nil, err
	}
	out := make([]Stanza, 0, len(raw))
	for _, r := range raw {
		out = append(out, Stanza{
			Name:          r.Name,
			StatusCode:    r.Status.Code,
			StatusMessage: r.Status.Message,
			Backups:       backupsNewestFirst(r),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
