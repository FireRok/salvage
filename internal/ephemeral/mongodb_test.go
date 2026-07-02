package ephemeral

import (
	"strings"
	"testing"
)

// Note: unlike mysql_test.go's `var _ checks.Queryer = (*MySQL)(nil)`, MongoDB's
// capability interface (mongodb.MongoQueryer) lives in internal/engine/mongodb,
// which itself imports internal/ephemeral (for StartMongoDB) — asserting it here
// would be an import cycle. The equivalent compile-time assertion instead lives
// in internal/engine/mongodb/mongodb_test.go, the package that can see both.

func TestJSQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "orders", `"orders"`},
		{"embedded double quote", `a"b`, `"a\"b"`},
		{"embedded backslash", `a\b`, `"a\\b"`},
		{"embedded newline", "a\nb", `"a\nb"`},
		{"embedded carriage return", "a\rb", `"a\rb"`},
		{"empty", "", `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsQuote(tc.in); got != tc.want {
				t.Errorf("jsQuote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestFieldAccessExpr(t *testing.T) {
	cases := []struct {
		name  string
		root  string
		field string
		want  string
	}{
		{"single field", "d", "status", `d?.["status"]`},
		{"dotted path", "d", "meta.version", `d?.["meta"]?.["version"]`},
		{"three levels", "d", "a.b.c", `d?.["a"]?.["b"]?.["c"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fieldAccessExpr(tc.root, tc.field); got != tc.want {
				t.Errorf("fieldAccessExpr(%q, %q) = %q, want %q", tc.root, tc.field, got, tc.want)
			}
		})
	}
}

// TestBuildExecArgs asserts the docker-exec argv buildExecArgs produces is
// well-formed without ever running Docker — mirroring how mysql/restic unit-
// test pure command-building logic. The password must never appear as a
// literal argument to the outer docker-exec process (only the shell-expansion
// placeholder "$MONGO_PWD" should), and the script it wraps must be a single
// sh -c argument containing every quoted piece.
func TestBuildExecArgs(t *testing.T) {
	captured := buildExecArgs("deadbeef", []string{"mongosh", "--quiet", "seeddb", "--eval", "db.foo.count()"})

	if len(captured) != 7 {
		t.Fatalf("captured args = %#v, want 7 elements", captured)
	}
	if captured[0] != "exec" {
		t.Errorf("args[0] = %q, want exec", captured[0])
	}
	if captured[1] != "-e" || captured[2] != "MONGO_PWD="+mongoPass {
		t.Errorf("expected -e MONGO_PWD=%s, got %q %q", mongoPass, captured[1], captured[2])
	}
	if captured[3] != "deadbeef" {
		t.Errorf("args[3] = %q, want container id deadbeef", captured[3])
	}
	if captured[4] != "sh" || captured[5] != "-c" {
		t.Errorf("expected sh -c, got %q %q", captured[4], captured[5])
	}
	script := captured[6]
	if strings.Contains(script, mongoPass) {
		t.Errorf("script leaks the literal password: %s", script)
	}
	if !strings.Contains(script, `"$MONGO_PWD"`) {
		t.Errorf("script does not reference $MONGO_PWD: %s", script)
	}
	if !strings.Contains(script, "mongosh") || !strings.Contains(script, "db.foo.count()") {
		t.Errorf("script missing expected pieces: %s", script)
	}

	// A password argument passed through to a quoted flag must not appear
	// unquoted such that shell metacharacters in a filter/collection name could
	// break out of their argument — spot-check that embedded spaces stay quoted.
	captured2 := buildExecArgs("id2", []string{"mongosh", "--eval", "db.getCollection('a b').count()"})
	script2 := captured2[6]
	if !strings.Contains(script2, `'db.getCollection`) {
		t.Errorf("expected the eval argument to remain single-quoted as one shell word: %s", script2)
	}
}

// TestMongorestoreArgs (via Restore's script construction) is exercised
// indirectly through buildExecArgs/exec; Restore itself requires Docker and is
// not unit-tested here, mirroring mysql/restic's Restore methods.
