package checks

import (
	"testing"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// eval is a small harness: run EvaluateScalar against a fresh result and return
// (result, hadExpectation).
func eval(got string, c config.Check) (report.CheckResult, bool) {
	res := report.CheckResult{Name: "n"}
	ok := EvaluateScalar(got, c, &res)
	return res, ok
}

func TestEvaluateScalarBounds(t *testing.T) {
	cases := []struct {
		name string
		got  string
		c    config.Check
		ok   bool
	}{
		{"within min", "5", config.Check{ExpectMin: pf(1)}, true},
		{"below min", "0", config.Check{ExpectMin: pf(1)}, false},
		{"within max", "5", config.Check{ExpectMax: pf(10)}, true},
		{"above max", "100", config.Check{ExpectMax: pf(10)}, false},
		{"within both", "5", config.Check{ExpectMin: pf(1), ExpectMax: pf(10)}, true},
	}
	for _, tc := range cases {
		res, had := eval(tc.got, tc.c)
		if !had {
			t.Errorf("%s: expected an expectation to be recognized", tc.name)
		}
		if res.OK != tc.ok {
			t.Errorf("%s: OK = %v, want %v (detail=%s err=%s)", tc.name, res.OK, tc.ok, res.Detail, res.Error)
		}
		if res.Got != tc.got {
			t.Errorf("%s: Got = %q, want %q", tc.name, res.Got, tc.got)
		}
	}
}

func TestEvaluateScalarBoundsNonNumeric(t *testing.T) {
	res, had := eval("not-a-number", config.Check{ExpectMin: pf(1)})
	if !had || res.OK || res.Error == "" {
		t.Errorf("non-numeric scalar under bounds should error; got %+v", res)
	}
}

func TestEvaluateScalarEquals(t *testing.T) {
	if res, _ := eval("42", config.Check{Equals: ps("42")}); !res.OK {
		t.Errorf("equals match should pass; got %+v", res)
	}
	res, _ := eval("41", config.Check{Equals: ps("42")})
	if res.OK {
		t.Error("equals mismatch should fail")
	}
	if res.Detail == "" {
		t.Error("equals mismatch should carry a detail")
	}
}

func TestEvaluateScalarMaxAge(t *testing.T) {
	// Both the Postgres text rendering and the ISO-8601/RFC3339 rendering
	// mongosh produces (Date.toISOString()) must parse.
	fresh := []string{
		time.Now().Add(-1 * time.Hour).Format("2006-01-02 15:04:05"),
		time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
	}
	for _, v := range fresh {
		if res, _ := eval(v, config.Check{MaxAge: pd(24 * time.Hour)}); !res.OK {
			t.Errorf("fresh timestamp %q should pass 24h window; got %+v", v, res)
		}
	}
	stale := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	if res, _ := eval(stale, config.Check{MaxAge: pd(24 * time.Hour)}); res.OK {
		t.Error("stale (72h) timestamp should fail a 24h window")
	}
	if res, _ := eval("yesterday-ish", config.Check{MaxAge: pd(24 * time.Hour)}); res.OK || res.Error == "" {
		t.Error("unparseable timestamp should surface as an error")
	}
}

func TestEvaluateScalarBool(t *testing.T) {
	for _, v := range []string{"t", "true", "1"} {
		if res, _ := eval(v, config.Check{Bool: pb(true)}); !res.OK {
			t.Errorf("%q should satisfy bool:true; got %+v", v, res)
		}
	}
	if res, _ := eval("f", config.Check{Bool: pb(true)}); res.OK {
		t.Error("f should fail bool:true")
	}
	if res, _ := eval("maybe", config.Check{Bool: pb(true)}); res.OK || res.Error == "" {
		t.Error("unparseable boolean should surface as an error")
	}
}

// No expectation: EvaluateScalar reports false and records nothing beyond Got,
// so each kind can supply its own "no expectation" message.
func TestEvaluateScalarNoExpectation(t *testing.T) {
	res, had := eval("5", config.Check{})
	if had {
		t.Fatal("a check with no expectation should report false")
	}
	if res.OK || res.Error != "" || res.Detail != "" {
		t.Errorf("no-expectation result should be untouched beyond Got; got %+v", res)
	}
	if res.Got != "5" {
		t.Errorf("Got should still record the scalar; got %q", res.Got)
	}
}
