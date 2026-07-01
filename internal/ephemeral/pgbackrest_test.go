package ephemeral

import (
	"reflect"
	"testing"
)

// TestParseNetworkList covers the pure parser that turns the space-separated
// `docker inspect` template output into a network-name slice. The disconnect
// behavior itself needs Docker and is left to integration tests; this exercises
// only the parsing, including the local-repo "no networks" case.
func TestParseNetworkList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single network with trailing space",
			in:   "bridge ",
			want: []string{"bridge"},
		},
		{
			name: "multiple networks",
			in:   "bridge salvage_net ",
			want: []string{"bridge", "salvage_net"},
		},
		{
			name: "no networks (local-repo restore) is empty",
			in:   "",
			want: nil,
		},
		{
			name: "whitespace only is empty",
			in:   "   \n\t ",
			want: nil,
		},
		{
			name: "extra internal whitespace and newline are tolerated",
			in:   "  bridge   custom_net \n",
			want: []string{"bridge", "custom_net"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNetworkList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNetworkList(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPgbackrestError exercises the pure extractor that pulls pgBackRest's
// ERROR: line out of the console log it writes to stdout, so a failed restore's
// verdict reason is the real cause rather than a bare "exit status 55".
func TestPgbackrestError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "missing-file error with timestamp prefix is stripped",
			in: "2026-06-30 21:07:23.303 P00   INFO: restore command begin 2.58.0\n" +
				"2026-06-30 21:07:38.781 P00  ERROR: [055]: raised from local-1 protocol: unable to open missing file '/x/pg_control.gz' for read\n" +
				"2026-06-30 21:07:38.781 P00   INFO: restore command end: aborted",
			want: "ERROR: [055]: raised from local-1 protocol: unable to open missing file '/x/pg_control.gz' for read",
		},
		{
			name: "no error line returns empty",
			in:   "2026-06-30 P00   INFO: restore command begin\nP00 INFO: completed successfully",
			want: "",
		},
		{
			name: "empty input returns empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgbackrestError(tc.in); got != tc.want {
				t.Errorf("pgbackrestError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestControldataValue exercises the pure parser that pulls a labelled value out
// of pg_controldata output (used to copy recovery-critical max_* settings into a
// synthesized postgresql.conf for config-outside-PGDATA clusters).
func TestControldataValue(t *testing.T) {
	out := "pg_control version number:            1300\n" +
		"max_connections setting:              200\n" +
		"max_worker_processes setting:         16\n" +
		"max_prepared_xacts setting:           0\n"
	cases := map[string]string{
		"max_connections setting:":      "200",
		"max_worker_processes setting:": "16",
		"max_prepared_xacts setting:":   "0",
		"max_wal_senders setting:":      "", // absent → empty
	}
	for label, want := range cases {
		if got := controldataValue(out, label); got != want {
			t.Errorf("controldataValue(%q) = %q, want %q", label, got, want)
		}
	}
}
