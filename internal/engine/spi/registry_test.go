package spi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"salvage.sh/internal/config"
)

// fakeEngine is a no-op engine used only to exercise the registry.
type fakeEngine struct{ typ string }

func (f fakeEngine) Type() string { return f.typ }
func (fakeEngine) Restore(context.Context, *config.Config) (RestoredTarget, string, error) {
	return nil, "", nil
}

func TestRegisterAndLookup(t *testing.T) {
	Register(fakeEngine{typ: "test-fake"})

	got, err := Lookup("test-fake")
	if err != nil {
		t.Fatalf("Lookup(test-fake): %v", err)
	}
	if got.Type() != "test-fake" {
		t.Fatalf("Type() = %q, want test-fake", got.Type())
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("no-such-engine")
	if err == nil {
		t.Fatal("Lookup of an unregistered type should error")
	}
	if !strings.Contains(err.Error(), "no-such-engine") {
		t.Fatalf("error should name the unknown type; got %q", err.Error())
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Register(fakeEngine{typ: "dup"})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate type should panic")
		}
	}()
	Register(fakeEngine{typ: "dup"})
}

func TestRegisterEmptyTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering an empty type should panic")
		}
	}()
	Register(fakeEngine{typ: ""})
}

// validatingEngine is a fake engine that also implements the optional
// ConfigValidator capability (spec 0016 R6).
type validatingEngine struct{ fakeEngine }

func (validatingEngine) ValidateConfig(cfg *config.Config) error {
	return errors.New("engine validator rejected " + cfg.Target.Name)
}

// Register must discover the optional ConfigValidator capability and wire it
// into config's validator registry, so config.Validate dispatches to the
// engine: a validating engine's error propagates, and a plain engine's type is
// simply known (no engine-specific validation).
func TestRegisterWiresConfigValidator(t *testing.T) {
	Register(validatingEngine{fakeEngine{typ: "test-validating"}})
	Register(fakeEngine{typ: "test-plain"})

	cfg := &config.Config{
		Target: config.Target{Name: "n", Type: "test-validating"},
		Report: config.Report{Format: "json"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "engine validator rejected n") {
		t.Fatalf("Validate should run the engine's ValidateConfig; got %v", err)
	}

	plain := &config.Config{
		Target: config.Target{Name: "n", Type: "test-plain"},
		Report: config.Report{Format: "json"},
	}
	if err := plain.Validate(); err != nil {
		t.Fatalf("a registered engine without ConfigValidator should validate cleanly; got %v", err)
	}

	unknown := &config.Config{
		Target: config.Target{Name: "n", Type: "test-unregistered"},
		Report: config.Report{Format: "json"},
	}
	if err := unknown.Validate(); err == nil {
		t.Fatal("an unregistered target.type should fail validation once engines are registered")
	}
}

// Fault must be distinguishable from a plain error via errors.As, since the
// orchestrator uses that to split operational failures from verdict failures.
func TestFaultUnwrap(t *testing.T) {
	base := errors.New("docker down")
	f := Faultf(base)

	var target *Fault
	if !errors.As(error(f), &target) {
		t.Fatal("Faultf result should match errors.As(*Fault)")
	}
	if !errors.Is(f, base) {
		t.Fatal("Fault should unwrap to its cause")
	}

	// A plain error must NOT be seen as a Fault.
	var t2 *Fault
	if errors.As(errors.New("plain"), &t2) {
		t.Fatal("a plain error should not match *Fault")
	}
}
