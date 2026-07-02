package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// HTTPProber is the capability a `kind: http` check needs: make a single HTTP
// request from the Salvage host and return the restored service's response.
// Any engine whose RestoredTarget can carry HTTP satisfies it (backlog S4) —
// the exec engine's host target and the restic/borg container targets all
// embed HostHTTP below — with net/http only; no HTTP client dependency is
// introduced.
type HTTPProber interface {
	// Do performs the request described by req and returns the response, or an
	// error only for an operational failure (could not reach/parse) — a non-2xx
	// status is a normal Response the evaluator judges, not an error.
	Do(ctx context.Context, req HTTPRequest) (HTTPResponse, error)
}

// HostHTTP is a ready-made HTTPProber that performs the request from the
// Salvage host via net/http. Embed it in a RestoredTarget to give the target
// the http check kind: the exec engine's host target uses it (spec 0020 R3),
// and the restic/borg container targets embed it so a service brought up from
// a restored tree can be HTTP-probed too (backlog S4). Requests originate on
// the host — the throwaway container's post-restore network isolation (spec
// 0003 R2) is untouched.
type HostHTTP struct{}

// Do performs the HTTP request from the Salvage host using net/http only.
func (HostHTTP) Do(ctx context.Context, r HTTPRequest) (HTTPResponse, error) {
	var body io.Reader
	if r.Body != "" {
		body = strings.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return HTTPResponse{}, err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HTTPResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return HTTPResponse{}, err
	}
	return HTTPResponse{Status: resp.StatusCode, Body: string(b)}, nil
}

// HTTPRequest describes the request an http check makes.
type HTTPRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

// HTTPResponse is what the prober returns for judging.
type HTTPResponse struct {
	Status int
	Body   string
}

// evalHTTP performs the check's HTTP request and applies its expectations:
// expect_status (default 200), expect_body_contains (substring), and expect_json
// (a single dotted.path=value assertion over a JSON body). It type-asserts the
// target to HTTPProber and returns a clear failing result when the target cannot
// make HTTP requests. Uses net/http (via the prober) and encoding/json only.
func evalHTTP(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	hp, ok := target.(HTTPProber)
	if !ok {
		res.Error = "http check requires a target with an HTTP prober (target.type restic, borg, or exec)"
		return res
	}
	method := c.Method
	if method == "" {
		method = "GET"
	}
	resp, err := hp.Do(ctx, HTTPRequest{
		URL:     c.URL,
		Method:  method,
		Headers: c.Headers,
		Body:    c.Body,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Got = strconv.Itoa(resp.Status)

	wantStatus := 200
	if c.ExpectStatus != nil {
		wantStatus = *c.ExpectStatus
	}
	if resp.Status != wantStatus {
		res.OK = false
		res.Detail = fmt.Sprintf("status %d, want %d", resp.Status, wantStatus)
		return res
	}

	if c.ExpectBodyContains != "" && !strings.Contains(resp.Body, c.ExpectBodyContains) {
		res.OK = false
		res.Detail = fmt.Sprintf("body does not contain %q", c.ExpectBodyContains)
		return res
	}

	if c.ExpectJSON != "" {
		path, want, perr := splitPathEquals(c.ExpectJSON)
		if perr != nil {
			res.Error = perr.Error()
			return res
		}
		got, jerr := jsonPath([]byte(resp.Body), path)
		if jerr != nil {
			res.OK = false
			res.Detail = jerr.Error()
			return res
		}
		if got != want {
			res.OK = false
			res.Detail = fmt.Sprintf("%s = %q, want %q", path, got, want)
			return res
		}
	}

	res.OK = true
	return res
}

// splitPathEquals splits a "dotted.path=value" assertion into its path and
// expected value. The first '=' separates them, so values may contain '='.
func splitPathEquals(s string) (path, value string, err error) {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return "", "", fmt.Errorf("expect_json must be path=value, got %q", s)
	}
	path = strings.TrimSpace(s[:i])
	value = s[i+1:]
	if path == "" {
		return "", "", fmt.Errorf("expect_json needs a path before '=' (got %q)", s)
	}
	return path, value, nil
}

// jsonPath walks a JSON document by a dotted path (e.g. "db.status" or
// "items.0.name", where a numeric segment indexes an array) and returns the
// scalar at that path rendered as a string. It returns an error if the path is
// absent or the document is not valid JSON. Minimal by design (spec 0020 R3):
// no wildcards, one path, scalar comparison — grows with demand.
func jsonPath(body []byte, path string) (string, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("response body is not valid JSON")
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return "", fmt.Errorf("json path %q: key %q not found", path, seg)
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return "", fmt.Errorf("json path %q: %q is not an array index", path, seg)
			}
			if idx < 0 || idx >= len(node) {
				return "", fmt.Errorf("json path %q: index %d out of range", path, idx)
			}
			cur = node[idx]
		default:
			return "", fmt.Errorf("json path %q: %q is not traversable", path, seg)
		}
	}
	return scalarString(cur), nil
}

// scalarString renders a JSON scalar the way expect_json values are written in
// config: strings verbatim, numbers without a trailing ".0", booleans as
// true/false, null as "null". A non-scalar (object/array) renders as its JSON.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// json numbers decode to float64; render integers without a decimal point.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case nil:
		return "null"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
