package probe_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"salvage.sh/internal/config"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/probe"
)

// The http kind is not exec-only (backlog S4): the restic/borg RestoredTargets
// carry an HTTP prober too. Compile-time proof of the capability, plus a real
// run of an http check against the restic target type through the registered
// evaluator — the same dispatch production uses. (The rejection half — a
// target with no HTTP prober fails cleanly — is TestBadTargetFailsCleanly.)
var (
	_ probe.HTTPProber = (*ephemeral.Restic)(nil)
	_ probe.HTTPProber = (*ephemeral.Borg)(nil)
)

func TestHTTPCheckRunsAgainstNonExecTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"db":{"status":"ok"},"rows":42}`))
	}))
	defer srv.Close()

	// The restic engine's RestoredTarget: the http capability is the embedded
	// host prober, so no Docker is needed to exercise it.
	target := &ephemeral.Restic{}

	status := 200
	for _, tc := range []struct {
		name   string
		check  config.Check
		wantOK bool
	}{
		{"status pass", config.Check{Name: "h", Kind: "http", URL: srv.URL, ExpectStatus: &status}, true},
		{"body contains pass", config.Check{Name: "h", Kind: "http", URL: srv.URL, ExpectBodyContains: `"status":"ok"`}, true},
		{"json path pass", config.Check{Name: "h", Kind: "http", URL: srv.URL, ExpectJSON: "db.status=ok"}, true},
		{"json path fail", config.Check{Name: "h", Kind: "http", URL: srv.URL, ExpectJSON: "db.status=down"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, detail, errStr, _ := run(t, target, tc.check)
			if errStr != "" {
				t.Fatalf("http check on a restic target must not be an operational error: %s", errStr)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (detail=%s)", ok, tc.wantOK, detail)
			}
		})
	}

	// The borg target carries the same capability.
	if ok, detail, errStr, _ := run(t, &ephemeral.Borg{}, config.Check{Name: "h", Kind: "http", URL: srv.URL}); !ok {
		t.Errorf("http check on a borg target should pass (detail=%s err=%s)", detail, errStr)
	}
}
