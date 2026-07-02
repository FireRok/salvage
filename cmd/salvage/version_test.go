package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Spec 0033 R6: only strict X.Y.Z release versions (optionally v-prefixed)
// are comparable; dev strings, git-describe forms, and junk are not.
func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"0.2.0", [3]int{0, 2, 0}, true},
		{"v0.2.0", [3]int{0, 2, 0}, true},
		{"v10.20.30", [3]int{10, 20, 30}, true},
		{"0.0.0-dev", [3]int{}, false},
		{"(devel)", [3]int{}, false},
		{"v0.2.0-3-gabc1234", [3]int{}, false},
		{"v0.2.0-3-gabc1234-dirty", [3]int{}, false},
		{"0.2", [3]int{}, false},
		{"0.2.0.1", [3]int{}, false},
		{"", [3]int{}, false},
		{"v", [3]int{}, false},
		{"1..2", [3]int{}, false},
		{"a.b.c", [3]int{}, false},
	}
	for _, tc := range cases {
		got, ok := parseSemver(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseSemver(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b [3]int
		want bool
	}{
		{[3]int{0, 2, 0}, [3]int{0, 2, 0}, false},
		{[3]int{0, 2, 0}, [3]int{0, 2, 1}, true},
		{[3]int{0, 2, 0}, [3]int{0, 3, 0}, true},
		{[3]int{0, 2, 0}, [3]int{1, 0, 0}, true},
		{[3]int{0, 10, 0}, [3]int{0, 9, 9}, false},
		{[3]int{1, 0, 0}, [3]int{0, 9, 9}, false},
	}
	for _, tc := range cases {
		if got := semverLess(tc.a, tc.b); got != tc.want {
			t.Errorf("semverLess(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// releaseAPI serves a minimal GitHub releases/latest response.
func releaseAPI(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","name":"Salvage ` + tag + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Spec 0033 R6: exit 0 up to date, 1 newer release available, 2 check failed.
func TestRunUpdateCheckExitCodes(t *testing.T) {
	t.Run("up to date", func(t *testing.T) {
		srv := releaseAPI(t, "v0.2.0")
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got, errw.String())
		}
		if !strings.Contains(out.String(), "up to date") {
			t.Errorf("output missing 'up to date': %q", out.String())
		}
	})

	t.Run("newer release available", func(t *testing.T) {
		srv := releaseAPI(t, "v0.3.0")
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 1 {
			t.Fatalf("exit = %d, want 1 (stderr: %s)", got, errw.String())
		}
		// Both versions and the install one-liner are printed.
		for _, want := range []string{"running  0.2.0", "latest   0.3.0", installOneLiner} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q: %q", want, out.String())
			}
		}
	})

	t.Run("running ahead of latest is up to date", func(t *testing.T) {
		srv := releaseAPI(t, "v0.2.0")
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.3.0", srv.URL); got != 0 {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got, errw.String())
		}
	})

	t.Run("dev build cannot compare", func(t *testing.T) {
		srv := releaseAPI(t, "v0.2.0")
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.0.0-dev", srv.URL); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
		if !strings.Contains(errw.String(), "not a release version") {
			t.Errorf("stderr missing clear message: %q", errw.String())
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
		if !strings.Contains(errw.String(), "update check failed") {
			t.Errorf("stderr missing failure message: %q", errw.String())
		}
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		srv := releaseAPI(t, "v0.2.0")
		url := srv.URL
		srv.Close()
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", url); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		t.Cleanup(srv.Close)
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
	})

	t.Run("missing tag_name", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"name":"x"}`))
		}))
		t.Cleanup(srv.Close)
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
	})

	t.Run("junk latest tag cannot compare", func(t *testing.T) {
		srv := releaseAPI(t, "nightly")
		var out, errw strings.Builder
		if got := runUpdateCheck(&out, &errw, "0.2.0", srv.URL); got != 2 {
			t.Fatalf("exit = %d, want 2", got)
		}
	})
}
