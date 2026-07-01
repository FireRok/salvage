package spi

import "fmt"

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
