package cli

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExplicitCAFileUsesValidatedTLS(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
	t.Setenv("SSL_CERT_FILE", p)
	c := newClient(s.URL, "synthetic")
	if _, e := c.request(context.Background(), "GET", "/test", "", ""); e != nil {
		t.Fatal("explicit trusted CA refused", e)
	}
}

func TestApplyTransportCannotReplayOnReusedConnection(t *testing.T) {
	var writes atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stack/apply") {
			io.ReadAll(r.Body)
			if writes.Add(1) == 1 {
				conn, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
				} else {
					conn.Close()
				}
				return
			}
			w.Write([]byte(`{"version":1,"changed":false,"resources":[]}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer s.Close()
	c := newClient(s.URL, "synthetic")
	if _, e := c.request(context.Background(), "GET", "/warm", "", ""); e != nil {
		t.Fatal(e)
	}
	_, e := c.request(context.Background(), "POST", "/v0/projects/prj_0123456789ABCDEF/stack/apply", "{}", "s1.original")
	if e == nil || writes.Load() != 1 {
		t.Fatalf("apply replayed automatically: writes=%d error=%v", writes.Load(), e)
	}
}

func TestOptionsAndNormalization(t *testing.T) {
	o, err := parseOptions([]string{"--root", ".", "plan", "--api-url", "https://API.example:443/base/", "--project", "prj_0000000000000001", "--output", "json"}, map[string]string{"AIMOUNT_API_URL": ""}, true)
	if err != nil || o.API != "https://api.example/base" || !o.NonInteractive {
		t.Fatal(o, err)
	}
	for _, url := range []string{"https://user:pass@host.test", "https://host.test?q=x", "http://host.test", "https://host.test/#fragment", "https://host.test\\x"} {
		if _, err := normalizeAPI(url); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	if _, err := parseOptions([]string{"plan"}, map[string]string{"AIMOUNT_API_URL": ""}, false); err == nil {
		t.Fatal("empty env accepted")
	}
	if _, e := parseOptions([]string{"help", "--https://user:synthetic-private@host.invalid/?secret=x"}, nil, false); e == nil || strings.Contains(e.Error(), "synthetic-private") {
		t.Fatal("secret-bearing option diagnostic disclosed")
	}
}

func TestURLNormalizationAndUnicodeWhitespace(t *testing.T) {
	for _, tc := range [][2]string{{"https://API.example:443/a/../base/", "https://api.example/base"}, {"HTTPS://API.example/base/", "https://api.example/base"}, {"https://host.test/a/%2e%2e/base", "https://host.test/base"}} {
		got, e := normalizeAPI(tc[0])
		if e != nil || got != tc[1] {
			t.Errorf("normalization %s: %s %v", tc[0], got, e)
		}
	}
	if _, e := normalizeAPI("https://host.test/\u00a0path"); e == nil {
		t.Fatal("unicode whitespace URL accepted")
	}
}

func TestAPIHostnameAndNumericPortCanonicalization(t *testing.T) {
	for _, tc := range [][2]string{{"https://example.com:0443/", "https://example.com"}, {"http://localhost:00080/base/", "http://localhost/base"}, {"https://bücher.example/", "https://xn--bcher-kva.example"}, {"https://BÜCHER.example:00444/base/", "https://xn--bcher-kva.example:444/base"}} {
		got, e := normalizeAPI(tc[0])
		if e != nil || got != tc[1] {
			t.Errorf("API canonicalization %s: got %s %v", tc[0], got, e)
		}
	}
}

func TestAPIIPv6AndPercentHostnameCanonicalization(t *testing.T) {
	for _, tc := range [][2]string{
		{"https://[0:0:0:0:0:0:0:1]/", "https://[::1]"},
		{"http://[0:0:0:0:0:0:0:1]/base/", "http://[::1]/base"},
		{"https://[2001:0db8:0:0:0:ff00:0042:8329]/", "https://[2001:db8::ff00:42:8329]"},
		{"https://[::ffff:192.0.2.128]/", "https://[::ffff:c000:280]"},
		{"https://[0:0:0:0:0:ffff:c000:0280]/", "https://[::ffff:c000:280]"},
		{"https://[0:0:0:0:0:ffff:192.0.2.1]", "https://[::ffff:c000:201]"},
		{"https://%65xample.com/", "https://example.com"},
		{"https://%62%C3%BCcher.example/base%2Fpart/", "https://xn--bcher-kva.example/base%2Fpart"},
		{"https://%C3%BC.example", "https://xn--tda.example"},
	} {
		got, e := normalizeAPI(tc[0])
		if e != nil || got != tc[1] {
			t.Errorf("canonical host mismatch %s: %s %v", tc[0], got, e)
		}
	}
	for _, u := range []string{"https://exa%25mple.com/", "https://exa%2fmple.com/", "https://exa%40mple.com/", "https://exa%3ample.com/", "https://exa%20mple.com/", "https://exa%00mple.com/", "https://%2565xample.com/", "https://%5b%3a%3a1%5d/", "https://[fe80::1%25en0]/", "https://exa%23mple.com/", "https://exa%3fmple.com/", "https://exa%5cmple.com/", "https://exa%09mple.com/", "https://exa%FFmple.com/"} {
		if _, e := normalizeAPI(u); e == nil {
			t.Errorf("unsafe encoded hostname accepted %s", u)
		}
	}
}

func TestAPINumericIPv4AliasesAndInvalidNumericHosts(t *testing.T) {
	for _, tc := range [][2]string{{"https://127.1", "https://127.0.0.1"}, {"http://127.1", "http://127.0.0.1"}, {"https://0177.0.0.1", "https://127.0.0.1"}, {"https://0x7f.1", "https://127.0.0.1"}, {"https://2130706433", "https://127.0.0.1"}, {"https://127.0.0.01", "https://127.0.0.1"}, {"https://127.0.1", "https://127.0.0.1"}, {"https://127.1.", "https://127.0.0.1"}, {"https://1.2.65535", "https://1.2.255.255"}, {"http://0x7f000001", "http://127.0.0.1"}, {"https://127.0.0.0x1", "https://127.0.0.1"}} {
		got, e := normalizeAPI(tc[0])
		if e != nil || got != tc[1] {
			t.Errorf("numeric host mismatch %s: %s %v", tc[0], got, e)
		}
	}
	for _, u := range []string{"https://256.0.0.1", "https://1.2.3.4.5", "https://example.1", "https://example.0x1", "https://example.09", "https://1.2.3.09", "https://4294967296"} {
		if _, e := normalizeAPI(u); e == nil {
			t.Errorf("invalid numeric host accepted %s", u)
		}
	}
}

func TestAPINumericOverflowAndForbiddenDecodedDomain(t *testing.T) {
	for _, u := range []string{"https://0x100000000", "https://example.0x100000000", "https://exa%7cmple.com", "https://exa%5em ple.com", "https://exa%3cmple.com", "https://exa%3emple.com", "http://[::ffff:127.0.0.1]"} {
		if _, e := normalizeAPI(u); e == nil {
			t.Errorf("invalid canonical hostname accepted %s", u)
		}
	}
}

const validReadSetJSON = `{"resources":[{"kind":"model","id":"mdl_0123456789ABCDEF","etag":"\"original-etag\""}],"bindings":[{"kind":"model","name":"smart","id":"mdl_0123456789ABCDEF"}],"membership_incarnation":"00000000-0000-4000-8000-000000000001","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0,"credentials":[{"kind":"model","id":"","name":"new_model","target":"primary","version":0}]}`

func TestPlanReadSetValidatesAndProjectsConsumedShape(t *testing.T) {
	valid, e := decodeJSON([]byte(validReadSetJSON), false)
	if e != nil {
		t.Fatal(e)
	}
	readSet := obj(valid)
	expected := wire(readSet)
	readSet["future_secret"] = "synthetic-private"
	for _, field := range []string{"resources", "bindings", "credentials"} {
		for _, v := range list(readSet[field]) {
			obj(v)["future_secret"] = "synthetic-private"
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, wire(object{"version": 1, "read_set": readSet, "changes": []any{}}))
	}))
	defer s.Close()
	c := newClient(s.URL, "synthetic")
	plan, e := c.request(context.Background(), "POST", "/v0/projects/prj_0123456789ABCDEF/stack/plan", "{}", "")
	if e != nil || wire(plan["read_set"]) != expected || strings.Contains(wire(plan), "synthetic-private") {
		t.Fatal("unsafe or changed read-set projection", e)
	}
	mutations := []func(object){
		func(m object) { delete(m, "resources") }, func(m object) { m["credentials"] = nil }, func(m object) { m["bindings"] = object{} },
		func(m object) { m["membership_version"] = json.Number("0") }, func(m object) { m["membership_access_epoch"] = json.Number("-1") }, func(m object) { m["project_lifecycle_epoch"] = json.Number("1.0000000000000001") },
		func(m object) { m["membership_incarnation"] = object{"private": "synthetic-private"} },
		func(m object) { obj(list(m["resources"])[0])["id"] = "tol_0123456789ABCDEF" }, func(m object) { obj(list(m["resources"])[0])["etag"] = "" },
		func(m object) { obj(list(m["bindings"])[0])["name"] = "INVALID" }, func(m object) { obj(list(m["credentials"])[0])["version"] = json.Number("-1") },
		func(m object) { obj(list(m["credentials"])[0])["target"] = "INVALID" },
	}
	for i, mutate := range mutations {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			v, e := decodeJSON([]byte(validReadSetJSON), false)
			if e != nil {
				t.Fatal(e)
			}
			m := obj(v)
			mutate(m)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, wire(object{"version": 1, "read_set": m, "changes": []any{}}))
			}))
			defer s.Close()
			_, e = newClient(s.URL, "synthetic").request(context.Background(), "POST", "/v0/projects/prj_0123456789ABCDEF/stack/plan", "{}", "")
			if e == nil || strings.Contains(e.Error(), "synthetic-private") {
				t.Fatal("malformed read-set accepted or disclosed", e)
			}
		})
	}
}

func TestReadSetLosslessInt64Bounds(t *testing.T) {
	for _, literal := range []string{"9223372036854775807", "9223372036854775808"} {
		v, e := decodeJSON([]byte(validReadSetJSON), false)
		if e != nil {
			t.Fatal(e)
		}
		m := obj(v)
		m["membership_version"] = json.Number(literal)
		out, e := projectReadSet(m)
		if literal == "9223372036854775807" {
			if e != nil || out["membership_version"] != json.Number(literal) {
				t.Fatal("valid int64 read-set changed or rejected", e)
			}
		} else if e == nil {
			t.Fatal("int64 overflow accepted")
		}
	}
}

func TestTransportRefusesRedirectAndUnknownApply(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/secret")
		w.WriteHeader(307)
	}))
	defer s.Close()
	c := newClient(s.URL, "synthetic")
	_, err := c.request(context.Background(), "POST", "/v0/projects/prj_0000000000000001/stack/apply", "{}", "original")
	a, ok := err.(*APIError)
	if !ok || !a.Unknown || a.Code != "redirect_refused" || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestResponseProjectionAndServerTime(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("browser headers")
		}
		w.Header().Set("Date", "Mon, 05 Oct 2026 00:00:00 GMT")
		w.Write([]byte(`{"version":1,"changed":false,"resources":[],"future_secret":"never-output"}`))
	}))
	defer s.Close()
	c := newClient(s.URL, "synthetic")
	if _, err := c.serverTime(); err == nil {
		t.Fatal("missing Date accepted")
	}
	v, err := c.request(context.Background(), "POST", "/v0/projects/prj_0000000000000001/stack/apply", "{}", "original")
	if err != nil || strings.Contains(wire(v), "never-output") {
		t.Fatal(v, err)
	}
	if _, err := c.serverTime(); err != nil {
		t.Fatal(err)
	}
}

func TestConsumedResponseShapesAreTolerantAndPrivate(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		invalid    bool
	}{
		{"/v0/projects/prj_0123456789ABCDEF", `{"id":"prj_0123456789ABCDEF","display_name":"Safe","status":"active","future_secret":"never-output"}`, false},
		{"/v0/projects/prj_0123456789ABCDEF", `{"id":"prj_0000000000000001","status":"active"}`, true},
		{"/v0/organizations", `{"items":[{"id":"org_0123456789ABCDEF","display_name":"Safe","future_secret":"never-output"}]}`, false},
		{"/v0/auth/identity", `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF","display_name":{"private":"never-output"}},"authentication":{"method":"pat"}}`, true},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
		c := newClient(s.URL, "synthetic")
		v, e := c.request(context.Background(), "GET", tc.path, "", "")
		s.Close()
		if tc.invalid && e == nil {
			t.Errorf("invalid response accepted %s", tc.path)
		}
		if !tc.invalid && (e != nil || strings.Contains(wire(v), "never-output")) {
			t.Errorf("bad projection %s: %v %s", tc.path, e, wire(v))
		}
	}
}

func TestExportAndPlanDropUnknownOperationalFields(t *testing.T) {
	for _, path := range []string{"/v0/projects/prj_0123456789ABCDEF/stack", "/v0/projects/prj_0123456789ABCDEF/stack/plan"} {
		spec := `{"display_name":"Safe","enabled":true,"current_contract":"v1","future_secret":"never-output","contracts":[{"name":"v1","description":"Safe","type":"server_webhook","input_schema":{"type":"object","additionalProperties":false},"output_schema":{},"webhook":{"url_source":"env","url":"https://private.invalid/never-output","auth":"bearer_token","timeout_seconds":10,"token":"never-output"}}]}`
		body := `{"version":1,"models":[],"agents":[],"tools":[{"name":"lookup","spec":` + spec + `}]}`
		method := "GET"
		if strings.HasSuffix(path, "/plan") {
			method = "POST"
			body = `{"version":1,"read_set":` + validReadSetJSON + `,"changes":[{"kind":"tool","name":"lookup","operation":"CREATE","after":` + spec + `}]}`
		}
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := newClient(s.URL, "synthetic")
		var v any
		var e error
		if method == "GET" {
			v, e = c.stack(context.Background(), "prj_0123456789ABCDEF")
		} else {
			v, e = c.request(context.Background(), method, path, "{}", "")
		}
		s.Close()
		if e != nil || strings.Contains(wire(v), "never-output") {
			t.Fatal("operational response disclosed", e)
		}
	}
}

func TestExportRefusesMalformedConsumedNodes(t *testing.T) {
	for _, nodes := range []string{`null`, `[null]`, `[42]`, `[{"name":"primary","credentials":{"mode":"api_key"},"enabled":"false"}]`} {
		body := `{"version":1,"models":[{"name":"smart","spec":{"nodes":` + nodes + `}}],"tools":[],"agents":[]}`
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := newClient(s.URL, "synthetic")
		_, e := c.stack(context.Background(), "prj_0123456789ABCDEF")
		s.Close()
		if e == nil {
			t.Error("malformed consumed Nodes accepted")
		}
	}
}

func TestExportRefusesMalformedConsumedBoundsAndLifecycle(t *testing.T) {
	for _, extra := range []string{`"archived":"false",`, `"archived":{},`} {
		body := `{"version":1,"models":[{"name":"smart",` + extra + `"spec":{"nodes":[]}}],"tools":[],"agents":[]}`
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := newClient(s.URL, "synthetic")
		_, e := c.stack(context.Background(), "prj_0123456789ABCDEF")
		s.Close()
		if e == nil {
			t.Error("malformed lifecycle accepted")
		}
	}
	for _, extra := range []string{`"limits":{"context_window_tokens":{"private":"secret"},"max_output_tokens":1},`, `"capabilities":{"input_modalities":["unknown"],"output_modalities":["text"],"tool_calling":true},`} {
		body := `{"version":1,"models":[{"name":"smart","spec":{` + extra + `"nodes":[]}}],"tools":[],"agents":[]}`
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c := newClient(s.URL, "synthetic")
		_, e := c.stack(context.Background(), "prj_0123456789ABCDEF")
		s.Close()
		if e == nil {
			t.Error("malformed bounds accepted")
		}
	}
}

func TestStackReturnsLosslessGraph(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v0/projects/prj_0123456789ABCDEF/stack" || r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("wrong Stack export request")
		}
		io.WriteString(w, `{"version":1,"models":[],"agents":[],"tools":[{"name":"lookup","spec":{"contracts":[{"name":"v1","type":"client","input_schema":{},"output_schema":{"const":9007199254740993,"examples":["1e400"]}}]}}]}`)
	}))
	defer s.Close()
	g, e := newClient(s.URL, "synthetic").stack(context.Background(), "prj_0123456789ABCDEF")
	if e != nil || g.Models == nil || g.Agents == nil || len(g.Tools) != 1 {
		t.Fatal("invalid typed export", e)
	}
	schema := obj(objects(obj(g.Tools[0]["spec"])["contracts"])[0]["output_schema"])
	if schema["const"] != json.Number("9007199254740993") || list(schema["examples"])[0] != "1e400" {
		t.Fatal("Stack export changed schema literals")
	}
}
