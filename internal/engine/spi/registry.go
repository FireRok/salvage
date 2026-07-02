package spi

import (
	"fmt"

	"salvage.sh/internal/config"
)

// registry maps target.type → Engine. Populated by engine packages' init() via
// Register; read by the orchestrator via Lookup. Registration happens at import
// time (single-threaded), so no locking is needed.
var registry = map[string]Engine{}

// Register makes eng available under eng.Type(). It panics on a duplicate or
// empty type — both are programmer errors surfaced at startup, not runtime.
func Register(eng Engine) {
	t := eng.Type()
	if t == "" {
		panic("spi.Register: engine has empty Type()")
	}
	if _, dup := registry[t]; dup {
		panic("spi.Register: duplicate engine for target type " + t)
	}
	registry[t] = eng
	// Discover the optional ConfigValidator capability (spec 0016 R6) and wire
	// it into config's validation registry: registering an engine is what makes
	// its target.type valid at load, and — if the engine implements the
	// capability — what routes config.Validate to the engine's own rules.
	if v, ok := eng.(ConfigValidator); ok {
		config.RegisterTargetValidator(t, v.ValidateConfig)
	} else {
		config.RegisterTargetValidator(t, nil)
	}
	// Discover the optional CapabilityDeclarer capability (backlog S4) and wire
	// the engine's probe capabilities into config's registry, so the shared
	// file/command/http check kinds validate for exactly the engines whose
	// RestoredTarget can carry them.
	if d, ok := eng.(CapabilityDeclarer); ok {
		config.RegisterTargetCapabilities(t, d.TargetCapabilities()...)
	}
}

// Lookup returns the engine for target type t, or an error naming the type and
// what is available — the "unknown target.type" operational error.
func Lookup(t string) (Engine, error) {
	if eng, ok := registry[t]; ok {
		return eng, nil
	}
	return nil, fmt.Errorf("no engine registered for target.type %q (registered: %v)", t, Types())
}

// Types returns the registered target types, for diagnostics.
func Types() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	return out
}
