package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
)

// fakeScaffoldTarget is a Docker-free restored target answering scalar SQL
// queries, so generated sql-kind candidates can be verified by checks.Run.
type fakeScaffoldTarget struct{}

func (fakeScaffoldTarget) Stop() error { return nil }

// Query satisfies checks.Queryer: any SELECT yields "1", so a candidate with
// equals "1" (or expect_min 1) passes verification and one expecting "2" fails.
func (fakeScaffoldTarget) Query(ctx context.Context, sql string) (string, error) {
	return "1", nil
}

// fakeScaffoldEngine implements spi.Scaffolder with scripted candidates,
// registered under a type of its own. It proves the orchestrator's scaffold
// dispatch is capability-only (spec 0028 R1): no Postgres/RowQueryer assumption
// survives in engine.Scaffold.
type fakeScaffoldEngine struct{}

func init() { spi.Register(fakeScaffoldEngine{}) }

func (fakeScaffoldEngine) Type() string { return "scafffake" }

func (fakeScaffoldEngine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	return fakeScaffoldTarget{}, "", nil
}

// scaffoldCandidates is what the fake proposes: reassigned per test.
var scaffoldCandidates []spi.ScaffoldCandidate

func (fakeScaffoldEngine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	if _, ok := rt.(fakeScaffoldTarget); !ok {
		return nil, fmt.Errorf("Discover got %T, want the engine's own restored target", rt)
	}
	return scaffoldCandidates, nil
}

func scaffoldCfg() *config.Config {
	return &config.Config{Target: config.Target{
		Name:    "scaffolded",
		Type:    "scafffake",
		Source:  config.Source{Kind: "fakekind"},
		Restore: config.Restore{Image: "img", Timeout: config.Duration(time.Minute)},
	}}
}

func sqlEquals(name, want, severity string) config.Check {
	return config.Check{Name: name, SQL: "SELECT 1", Equals: &want, Severity: severity}
}

// Spec 0028 R1 (acceptance 4): scaffold dispatches through the engine
// capability; the emitted config carries the engine's own target.type and
// round-trips strict config.Load.
func TestScaffoldCapabilityDispatchAndRoundTrip(t *testing.T) {
	scaffoldCandidates = []spi.ScaffoldCandidate{
		{Check: sqlEquals("server_reachable", "1", "required")},
		{Check: sqlEquals("orders_ok", "1", "advisory"), Group: "table", Subject: "orders", Weight: 10},
	}
	rendered, err := Scaffold(context.Background(), scaffoldCfg())
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	path := filepath.Join(t.TempDir(), "scaffolded.yaml")
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("scaffolded config does not re-parse via config.Load: %v\n%s", err, rendered)
	}
	if loaded.Target.Type != "scafffake" {
		t.Errorf("target.type = %q, want the discovering engine's type (not \"postgres\")", loaded.Target.Type)
	}
	if len(loaded.Target.Checks) != 2 {
		t.Fatalf("checks = %d, want 2 (both candidates verified)", len(loaded.Target.Checks))
	}
	if bytes.Contains(rendered, []byte("NOTE: generated checks were capped")) {
		t.Errorf("uncapped output carries a truncation note:\n%s", rendered)
	}
}

// Spec 0028 R1 (acceptance 4): an engine without the capability gates off with
// the existing message — MongoDB until 0028 R8 lands.
func TestScaffoldGatesOffWithoutCapability(t *testing.T) {
	cfg := scaffoldCfg()
	cfg.Target.Type = "mongodb"
	_, err := Scaffold(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), `scaffold is not supported for target.type "mongodb"`) {
		t.Errorf("Scaffold(mongodb) error = %v, want the capability not-supported message", err)
	}
}

// Spec 0028 R5 (acceptance 5): a candidate that cannot pass on the snapshot is
// dropped by the shared verify-by-running net, whatever its severity.
func TestScaffoldVerifyDropsFailingCandidates(t *testing.T) {
	scaffoldCandidates = []spi.ScaffoldCandidate{
		{Check: sqlEquals("passes", "1", "required")},
		{Check: sqlEquals("cannot_pass", "2", "advisory")},
	}
	rendered, err := Scaffold(context.Background(), scaffoldCfg())
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if bytes.Contains(rendered, []byte("cannot_pass")) {
		t.Errorf("failing candidate was emitted:\n%s", rendered)
	}
	if !bytes.Contains(rendered, []byte("passes")) {
		t.Errorf("passing candidate missing:\n%s", rendered)
	}
}

// Spec 0028 R6 (acceptance 6): the shared cap keeps the top-N subjects per
// group by weight, a subject's checks live or die together, structural checks
// are never capped, the truncation note is emitted, and re-running is
// deterministic.
func TestScaffoldCapsBySubjectDeterministically(t *testing.T) {
	one := "1"
	scaffoldCandidates = []spi.ScaffoldCandidate{
		{Check: sqlEquals("structural", "1", "required")}, // ungrouped: never capped
		// big: two checks on one subject, the largest table.
		{Check: sqlEquals("big_min_rows", "1", "advisory"), Group: "table", Subject: "big", Weight: 100},
		{Check: config.Check{Name: "big_fresh", SQL: "SELECT 1", Equals: &one, Severity: "advisory"},
			Group: "table", Subject: "big", Weight: 100},
		{Check: sqlEquals("mid_min_rows", "1", "advisory"), Group: "table", Subject: "mid", Weight: 50},
		{Check: sqlEquals("tiny_min_rows", "1", "advisory"), Group: "table", Subject: "tiny", Weight: 1},
		// A different group is capped independently.
		{Check: sqlEquals("dir_logs_files", "1", "advisory"), Group: "dir", Subject: "logs", Weight: 3},
	}
	cfg := scaffoldCfg()
	first, err := ScaffoldWithCap(context.Background(), cfg, 2)
	if err != nil {
		t.Fatalf("ScaffoldWithCap: %v", err)
	}
	for _, want := range []string{"structural", "big_min_rows", "big_fresh", "mid_min_rows", "dir_logs_files",
		"NOTE: generated checks were capped to the 2 largest"} {
		if !bytes.Contains(first, []byte(want)) {
			t.Errorf("capped output missing %q:\n%s", want, first)
		}
	}
	if bytes.Contains(first, []byte("tiny_min_rows")) {
		t.Errorf("cap kept the smallest subject:\n%s", first)
	}

	second, err := ScaffoldWithCap(context.Background(), cfg, 2)
	if err != nil {
		t.Fatalf("ScaffoldWithCap (second run): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("scaffold is not deterministic across runs:\n--- first\n%s\n--- second\n%s", first, second)
	}
}
