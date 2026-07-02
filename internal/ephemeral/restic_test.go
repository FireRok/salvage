package ephemeral

import "testing"

// TestSplitExit covers the pure parser that separates RunCommand's stdout from
// the "__EXIT__<n>" marker it appends to capture the exit code even when the
// wrapped command exits non-zero.
func TestSplitExit(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOut  string
		wantExit int
	}{
		{"clean success", "hello\n__EXIT__0", "hello", 0},
		{"non-zero exit", "boom\n__EXIT__2", "boom", 2},
		{"empty stdout, exit 1", "\n__EXIT__1", "", 1},
		{"no marker means success", "just output", "just output", 0},
		{"garbled marker falls back to 0", "out\n__EXIT__x", "out", 0},
		{"marker text appearing in output uses the last", "__EXIT__ in text\n__EXIT__0", "__EXIT__ in text", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, exit := splitExit(tc.in)
			if out != tc.wantOut || exit != tc.wantExit {
				t.Errorf("splitExit(%q) = (%q, %d), want (%q, %d)", tc.in, out, exit, tc.wantOut, tc.wantExit)
			}
		})
	}
}

// TestResticError covers the extractor that pulls restic's Fatal/error line out
// of its output so a failed restore's verdict reason is the real cause.
func TestResticError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fatal wrong password",
			in:   "repository abc opened\nFatal: wrong password or no key found\n",
			want: "Fatal: wrong password or no key found",
		},
		{
			name: "unable to open repo",
			in:   "unable to open repository at /x: stat /x: no such file or directory",
			want: "unable to open repository at /x: stat /x: no such file or directory",
		},
		{
			name: "no error line",
			in:   "restoring snapshot 1234 to /restore\nrestored 3 files",
			want: "",
		},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resticError(tc.in); got != tc.want {
				t.Errorf("resticError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBorgError covers the extractor that pulls borg's error line out of its
// output so a failed extract's verdict reason is the real cause (spec 0022).
func TestBorgError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrong passphrase",
			in:   "Enter passphrase for key /repo:\npassphrase supplied in BORG_PASSPHRASE is incorrect.\n",
			want: "passphrase supplied in BORG_PASSPHRASE is incorrect.",
		},
		{
			name: "missing archive",
			in:   "reading repository index\narchive nightly-2026 does not exist\n",
			want: "archive nightly-2026 does not exist",
		},
		{
			name: "not a repository",
			in:   "/repo does not seem to be a valid repository\n",
			want: "/repo does not seem to be a valid repository",
		},
		{
			name: "error-prefixed line",
			in:   "extracting\nERROR: Repository /repo does not exist.",
			want: "ERROR: Repository /repo does not exist.",
		},
		{
			name: "no error line",
			in:   "Extracting archive nightly-2026\nextracted 3 files",
			want: "",
		},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := borgError(tc.in); got != tc.want {
				t.Errorf("borgError() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShquote covers the single-quote escaping used for paths/globs embedded in
// an `sh -c` string.
func TestShquote(t *testing.T) {
	cases := map[string]string{
		"/restore":       "'/restore'",
		"data/*.parquet": "'data/*.parquet'",
		"it's a file":    `'it'\''s a file'`,
	}
	for in, want := range cases {
		if got := shquote(in); got != want {
			t.Errorf("shquote(%q) = %q, want %q", in, got, want)
		}
	}
}
