package inspect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writePGDATA lays down a minimal PGDATA skeleton in a temp dir. version is the
// PG_VERSION contents; conf and autoConf are written only when non-empty. It
// returns the directory path.
func writePGDATA(t *testing.T, version, conf, autoConf string) string {
	t.Helper()
	dir := t.TempDir()
	if version != "" {
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte(version), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if conf != "" {
		if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if autoConf != "" {
		if err := os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte(autoConf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestVersionParsing(t *testing.T) {
	dir := writePGDATA(t, "17\n", "", "")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PGVersion != "17" {
		t.Errorf("PGVersion = %q, want %q", got.PGVersion, "17")
	}
}

func TestVersionTrimsWhitespace(t *testing.T) {
	dir := writePGDATA(t, "  16  \n", "", "")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PGVersion != "16" {
		t.Errorf("PGVersion = %q, want %q", got.PGVersion, "16")
	}
}

func TestMissingPGVersionIsError(t *testing.T) {
	dir := t.TempDir() // no PG_VERSION
	if _, err := Inspect(dir); err == nil {
		t.Fatal("expected error for missing PG_VERSION, got nil")
	}
}

func TestEmptyPGVersionIsError(t *testing.T) {
	dir := writePGDATA(t, "  \n", "", "")
	if _, err := Inspect(dir); err == nil {
		t.Fatal("expected error for empty PG_VERSION, got nil")
	}
}

func TestPreloadParsing(t *testing.T) {
	cases := []struct {
		name string
		conf string
		want []string
	}{
		{
			name: "single quoted",
			conf: "shared_preload_libraries = 'timescaledb'\n",
			want: []string{"timescaledb"},
		},
		{
			name: "comma separated quoted",
			conf: "shared_preload_libraries = 'timescaledb, pg_stat_statements'\n",
			want: []string{"timescaledb", "pg_stat_statements"},
		},
		{
			name: "unquoted",
			conf: "shared_preload_libraries = timescaledb\n",
			want: []string{"timescaledb"},
		},
		{
			name: "messy whitespace",
			conf: "shared_preload_libraries   =   '  timescaledb ,  pg_stat_statements  '  \n",
			want: []string{"timescaledb", "pg_stat_statements"},
		},
		{
			name: "double quoted value",
			conf: "shared_preload_libraries = \"timescaledb,citus\"\n",
			want: []string{"timescaledb", "citus"},
		},
		{
			name: "empty quoted value",
			conf: "shared_preload_libraries = ''\n",
			want: nil,
		},
		{
			name: "absent setting",
			conf: "max_connections = 100\n",
			want: nil,
		},
		{
			name: "commented out",
			conf: "#shared_preload_libraries = 'timescaledb'\n",
			want: nil,
		},
		{
			name: "trailing comment",
			conf: "shared_preload_libraries = 'timescaledb' # load tsdb\n",
			want: []string{"timescaledb"},
		},
		{
			name: "last assignment wins",
			conf: "shared_preload_libraries = 'a'\nshared_preload_libraries = 'b,c'\n",
			want: []string{"b", "c"},
		},
		{
			name: "trailing comma",
			conf: "shared_preload_libraries = 'timescaledb,'\n",
			want: []string{"timescaledb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writePGDATA(t, "17\n", tc.conf, "")
			got, err := Inspect(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.RequiredPreloadExtensions, tc.want) {
				t.Errorf("RequiredPreloadExtensions = %#v, want %#v",
					got.RequiredPreloadExtensions, tc.want)
			}
		})
	}
}

func TestAutoConfOverridesConf(t *testing.T) {
	dir := writePGDATA(t, "17\n",
		"shared_preload_libraries = 'pg_stat_statements'\n",
		"shared_preload_libraries = 'timescaledb'\n")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"timescaledb"}
	if !reflect.DeepEqual(got.RequiredPreloadExtensions, want) {
		t.Errorf("RequiredPreloadExtensions = %#v, want %#v", got.RequiredPreloadExtensions, want)
	}
}

func TestAutoConfFallsBackToConf(t *testing.T) {
	// auto.conf exists but does not set the key; conf's value must survive.
	dir := writePGDATA(t, "17\n",
		"shared_preload_libraries = 'timescaledb'\n",
		"max_wal_size = '1GB'\n")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"timescaledb"}
	if !reflect.DeepEqual(got.RequiredPreloadExtensions, want) {
		t.Errorf("RequiredPreloadExtensions = %#v, want %#v", got.RequiredPreloadExtensions, want)
	}
}

func TestAbsentSettingIsEmptySlice(t *testing.T) {
	dir := writePGDATA(t, "17\n", "max_connections = 100\n", "")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredPreloadExtensions) != 0 {
		t.Errorf("RequiredPreloadExtensions = %#v, want empty", got.RequiredPreloadExtensions)
	}
}

func TestNoConfFilesIsEmptySlice(t *testing.T) {
	dir := writePGDATA(t, "17\n", "", "")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredPreloadExtensions) != 0 {
		t.Errorf("RequiredPreloadExtensions = %#v, want empty", got.RequiredPreloadExtensions)
	}
}

func TestDatabaseCount(t *testing.T) {
	dir := writePGDATA(t, "17\n", "", "")
	base := filepath.Join(dir, "base")
	// Three OID-named database dirs, one non-numeric dir, and a stray file.
	for _, name := range []string{"1", "13750", "16384"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "pgsql_tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseCount != 3 {
		t.Errorf("DatabaseCount = %d, want 3", got.DatabaseCount)
	}
}

func TestDatabaseCountNoBaseDir(t *testing.T) {
	dir := writePGDATA(t, "17\n", "", "")
	got, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseCount != 0 {
		t.Errorf("DatabaseCount = %d, want 0", got.DatabaseCount)
	}
}
