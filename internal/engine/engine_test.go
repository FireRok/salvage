package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
)

// fakeChainEngine is a Docker-free engine implementing both optional
// capabilities, registered under a type of its own. It exists to prove the
// orchestrator's dispatch is capability-only (spec 0029 R1): its source.kind is
// never "pgbackrest", yet last-good and fleet must work against it.
type fakeChainEngine struct{}

func init() { spi.Register(fakeChainEngine{}) }

func (fakeChainEngine) Type() string { return "chainfake" }

func (fakeChainEngine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	return nil, "", errors.New("not used in these tests")
}

var fakeNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// Chain: newest first — "new-bad" (will fail), then "old-good" (will pass).
func (fakeChainEngine) Chain(ctx context.Context, cfg *config.Config) ([]spi.Backup, error) {
	return []spi.Backup{
		{Label: "new-bad", Type: "snapshot", Timestamp: fakeNow},
		{Label: "old-good", Type: "snapshot", Timestamp: fakeNow.Add(-24 * time.Hour)},
	}, nil
}

func (fakeChainEngine) TestBackup(ctx context.Context, cfg *config.Config, label string) string {
	if label == "new-bad" {
		return "checksum mismatch"
	}
	return ""
}

func (fakeChainEngine) Survey(ctx context.Context, cfg *config.Config) ([]spi.FleetUnit, error) {
	ts := fakeNow
	return []spi.FleetUnit{{
		Name: "repo-a", Status: "ok", BackupCount: 3, NewestLabel: "new-bad", NewestBackup: &ts,
	}}, nil
}

func (fakeChainEngine) SkeletonSource(cfg *config.Config, unit string) config.Source {
	return cfg.Target.Source
}

func fakeCfg() *config.Config {
	return &config.Config{Target: config.Target{
		Name:    "fake-target",
		Type:    "chainfake",
		Source:  config.Source{Kind: "fakekind"},
		Restore: config.Restore{Image: "img", Timeout: config.Duration(time.Minute)},
	}}
}

// Spec 0029 R1 + acceptance 2 (shape): with a non-pgbackrest source, last-good
// dispatches on the ChainTester capability alone, skips the failing newest
// backup (recording its reason), and reports the older one as the recovery point.
func TestLastGoodCapabilityOnlyDispatch(t *testing.T) {
	lg, err := LastGood(context.Background(), fakeCfg(), 0)
	if err != nil {
		t.Fatalf("LastGood on a non-pgbackrest ChainTester: %v", err)
	}
	if len(lg.Tested) != 2 {
		t.Fatalf("Tested = %d entries, want 2 (newest fail + older pass)", len(lg.Tested))
	}
	if lg.Tested[0].Label != "new-bad" || lg.Tested[0].Verdict != "fail" || lg.Tested[0].Reason != "checksum mismatch" {
		t.Errorf("newest verdict = %+v, want fail with the failure reason", lg.Tested[0])
	}
	if lg.RecoveryPoint == nil || lg.RecoveryPoint.Label != "old-good" || lg.RecoveryPoint.Verdict != "pass" {
		t.Errorf("RecoveryPoint = %+v, want the older passing backup", lg.RecoveryPoint)
	}
	if lg.Stanza != "" {
		t.Errorf("Stanza = %q, want empty for a non-pgbackrest engine", lg.Stanza)
	}
}

// Spec 0010 R6 / 0029 R6: -max caps the walk, and what was tried is reported.
func TestLastGoodHonorsMax(t *testing.T) {
	lg, err := LastGood(context.Background(), fakeCfg(), 1)
	if err != nil {
		t.Fatalf("LastGood: %v", err)
	}
	if len(lg.Tested) != 1 || lg.Tested[0].Label != "new-bad" {
		t.Fatalf("Tested = %+v, want only the newest candidate", lg.Tested)
	}
	if lg.RecoveryPoint != nil {
		t.Errorf("RecoveryPoint = %+v, want nil when the walk is capped before a pass", lg.RecoveryPoint)
	}
}

// Spec 0029 R1 + acceptance 6: an engine without the capability still gets the
// existing "not supported for target.type X" error — from the capability gate,
// not a source.kind gate.
func TestCapabilityGateErrors(t *testing.T) {
	cfg := fakeCfg()
	cfg.Target.Type = "mysql"
	if _, err := LastGood(context.Background(), cfg, 0); err == nil ||
		!strings.Contains(err.Error(), `last-good is not supported for target.type "mysql"`) {
		t.Errorf("LastGood(mysql) error = %v, want the capability not-supported message", err)
	}
	cfg.Target.Type = "mongodb"
	if _, err := Fleet(context.Background(), cfg, ""); err == nil ||
		!strings.Contains(err.Error(), `fleet is not supported for target.type "mongodb"`) {
		t.Errorf("Fleet(mongodb) error = %v, want the capability not-supported message", err)
	}
}

// Spec 0029 R1/R4: fleet dispatches on the FleetSurveyor capability alone, and
// the emitted skeleton re-parses via config.Load.
func TestFleetCapabilityOnlyDispatchAndSkeleton(t *testing.T) {
	dir := t.TempDir()
	fl, err := Fleet(context.Background(), fakeCfg(), dir)
	if err != nil {
		t.Fatalf("Fleet on a non-pgbackrest FleetSurveyor: %v", err)
	}
	if len(fl.Stanzas) != 1 || fl.Stanzas[0].Name != "repo-a" || fl.Stanzas[0].BackupCount != 3 {
		t.Fatalf("Fleet stanzas = %+v, want the surveyed unit", fl.Stanzas)
	}
	path := fl.Stanzas[0].ConfigWritten
	if path == "" {
		t.Fatal("ConfigWritten is empty, want a skeleton path")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("skeleton at %s does not re-parse via config.Load: %v", path, err)
	}
	if loaded.Target.Type != "chainfake" {
		t.Errorf("skeleton target.type = %q, want the surveyed engine's type", loaded.Target.Type)
	}
}

// Spec 0029 R4 (acceptance 3): a restic fleet skeleton — repository carried
// over, target.type restic, file structural checks — re-parses via config.Load.
func TestResticSkeletonRoundTrips(t *testing.T) {
	eng, err := spi.Lookup("restic")
	if err != nil {
		t.Fatalf("restic engine not registered: %v", err)
	}
	fs, ok := eng.(spi.FleetSurveyor)
	if !ok {
		t.Fatal("restic engine does not implement spi.FleetSurveyor")
	}
	cfg := &config.Config{Target: config.Target{
		Name:    "files",
		Type:    "restic",
		Source:  config.Source{Kind: "restic", Repository: "/srv/repo", RepoVolume: "vol"},
		Restore: config.Restore{Image: "restic/restic:0.19.0", Timeout: config.Duration(time.Minute)},
	}}
	dir := t.TempDir()
	path, err := writeSkeleton(cfg, fs, "/srv/repo", dir)
	if err != nil {
		t.Fatalf("writeSkeleton: %v", err)
	}
	if filepath.Base(path) != "srv-repo.yaml" {
		t.Errorf("skeleton file = %q, want the sanitized srv-repo.yaml", filepath.Base(path))
	}
	loaded, err := config.Load(path)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("restic skeleton does not re-parse via config.Load: %v\n%s", err, b)
	}
	if loaded.Target.Type != "restic" || loaded.Target.Source.Repository != "/srv/repo" {
		t.Errorf("skeleton target = %+v, want restic with the repository carried over", loaded.Target)
	}
	if len(loaded.Target.Checks) == 0 || loaded.Target.Checks[0].Kind != "file_count" {
		t.Errorf("skeleton checks = %+v, want the file structural check", loaded.Target.Checks)
	}
}

// Spec 0029 R4: same round-trip for borg, whose skeleton must keep the
// (required) archive pin.
func TestBorgSkeletonRoundTrips(t *testing.T) {
	eng, err := spi.Lookup("borg")
	if err != nil {
		t.Fatalf("borg engine not registered: %v", err)
	}
	fs, ok := eng.(spi.FleetSurveyor)
	if !ok {
		t.Fatal("borg engine does not implement spi.FleetSurveyor")
	}
	cfg := &config.Config{Target: config.Target{
		Name:    "files",
		Type:    "borg",
		Source:  config.Source{Kind: "borg", Repository: "/srv/borg", RepoVolume: "vol", Archive: "app-2026-06-30"},
		Restore: config.Restore{Image: "ghcr.io/borgmatic-collective/borgmatic:2.1.6", Timeout: config.Duration(time.Minute)},
	}}
	path, err := writeSkeleton(cfg, fs, "/srv/borg", t.TempDir())
	if err != nil {
		t.Fatalf("writeSkeleton: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("borg skeleton does not re-parse via config.Load: %v\n%s", err, b)
	}
	if loaded.Target.Type != "borg" || loaded.Target.Source.Archive != "app-2026-06-30" {
		t.Errorf("skeleton target = %+v, want borg with the archive pin carried over", loaded.Target)
	}
}

// skeletonFileName: ordinary stanza names pass through untouched (spec 0029 R7);
// repository identities become safe file stems.
func TestSkeletonFileName(t *testing.T) {
	cases := map[string]string{
		"app-db":                "app-db", // pgBackRest stanza: identity
		"main_01.x":             "main_01.x",
		"/srv/repo":             "srv-repo", // filesystem path
		"s3:s3.example.com/b/r": "s3-s3.example.com-b-r",
		"":                      "repository",
	}
	for in, want := range cases {
		if got := skeletonFileName(in); got != want {
			t.Errorf("skeletonFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
