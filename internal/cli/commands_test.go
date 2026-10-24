package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandsPlanApplyNoAutoRetryAndState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/smart.yaml": modelYAML})
	plans, writes := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/auth/identity":
			w.Write([]byte(`{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`))
		case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
			w.Write([]byte(`{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Synthetic"}`))
		case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
			w.Write([]byte(`{"version":1,"bindings":[]}`))
		case strings.HasSuffix(r.URL.Path, "/stack/plan"):
			plans++
			w.Write([]byte(`{"version":1,"read_set":` + validReadSetJSON + `,"changes":[{"kind":"model","name":"smart","operation":"CREATE"}]}`))
		case strings.HasSuffix(r.URL.Path, "/stack/apply"):
			writes++
			if !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "s1.") {
				t.Error("bad key")
			}
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"code":"service_unavailable"}}`))
		case strings.HasSuffix(r.URL.Path, "/stack"):
			w.Write([]byte(`{"version":1,"models":[],"tools":[],"agents":[],"applications":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	base := []string{"--root", root, "--project", "prj_0123456789ABCDEF", "--api-url", s.URL, "--output", "json"}
	env := map[string]string{"AIMOUNT_TOKEN": "synthetic-token", "OPENAI_TEST": "synthetic-key"}
	r := runner{ctx: context.Background(), in: strings.NewReader(""), errOut: &bytes.Buffer{}, env: env}
	pullOptions, e := parseOptions([]string{"pull", "--root", t.TempDir(), "--project", "prj_0123456789ABCDEF", "--api-url", s.URL}, env, false)
	if e != nil {
		t.Fatal(e)
	}
	empty, e := r.run(pullOptions)
	if e != nil || wire(obj(empty.Value)["files"]) != "[]" {
		t.Fatal("empty pull file list must remain a JSON array", e)
	}
	o, e := parseOptions(append([]string{"plan"}, base...), env, false)
	if e != nil {
		t.Fatal(e)
	}
	v, e := r.run(o)
	if e != nil || v.Command != "plan" {
		t.Fatal(v, e)
	}
	o, e = parseOptions(append([]string{"apply", "--auto-approve"}, base...), env, false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.run(o)
	a, ok := e.(*APIError)
	if !ok || !a.Unknown || writes != 1 || plans != 2 {
		t.Fatal(e, writes, plans)
	}
	o, e = parseOptions(append([]string{"link"}, base...), env, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.run(o); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, ".aimount/project.json"))
	if e != nil || !strings.Contains(string(b), "prj_0123456789ABCDEF") {
		t.Fatal(e)
	}
}

func TestPromptSignalCancellationIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in, out := io.Pipe()
	defer in.Close()
	defer out.Close()
	r := runner{ctx: ctx, in: in, errOut: io.Discard}
	done := make(chan error, 1)
	go func() { _, e := r.ask("Confirm", Options{}); done <- e }()
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancel accepted")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("prompt ignores cancellation")
	}
}

func TestPrepareCredentialsAliasesPolicyAndSemantic(t *testing.T) {
	desired := emptyGraph()
	current := emptyGraph()
	current.Models = []object{{"name": "smart", "spec": object{"nodes": []any{object{"name": "remove", "credentials": object{"mode": "api_key"}}, object{"name": "keep", "provider": "openai", "credentials": object{"mode": "api_key"}}}}}}
	desired.Models = []object{{"name": "old_smart", "spec": object{"nodes": []any{object{"name": "keep", "provider": "openai", "credentials": object{"mode": "api_key"}}}}}}
	bindings := []object{{"kind": "model", "name": "smart", "id": "mdl_0000000000000001"}, {"kind": "model", "name": "old_smart", "id": "mdl_0000000000000001"}}
	if e := prepareGraph(desired, current, bindings); e != nil {
		t.Fatal(e)
	}
	if wire(desired.Models[0]["credential_changes"]) != `[{"action":"remove","target":"remove"}]` {
		t.Fatal(desired)
	}
	out := semantic(object{"changes": []any{object{"kind": "agent", "name": "support", "operation": "UPDATE", "before": object{"instruction": "Old", "tools": []any{object{"name": "lookup", "contract": "v1"}}}, "after": object{"instruction": "New\x1b", "tools": []any{object{"name": "lookup", "contract": "v2"}}}}}})
	for _, s := range []string{"lookup@v1", "lookup@v2", "--- saved\n+++ desired", "\\u001b"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %s", s)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatal("terminal control")
	}
}

func TestExactSchemaPullRoundTrip(t *testing.T) {
	schema, e := parseYAML("const: 9007199254740993\nmultipleOf: 1e-400", "schema")
	if e != nil {
		t.Fatal(e)
	}
	g := emptyGraph()
	g.Tools = []object{{"name": "lookup", "spec": object{"display_name": "Lookup", "enabled": true, "current_contract": "v1", "contracts": []any{object{"name": "v1", "description": "Lookup", "type": "client", "input_schema": object{"type": "object", "additionalProperties": false}, "output_schema": schema}}}}}
	files, e := render(g, nil)
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	writeFiles(t, root, files)
	loaded, e := load(root, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(wire(loaded), "9007199254740993") || !strings.Contains(wire(loaded), "1e-400") {
		t.Fatal("precision lost")
	}
}

func TestPullLocatorAliasIdentityAndAmbiguity(t *testing.T) {
	g := emptyGraph()
	g.Models = []object{{"name": "smart"}}
	bindings := []object{{"kind": "model", "name": "old_smart", "id": "mdl_0000000000000001"}, {"kind": "model", "name": "smart", "id": "mdl_0000000000000001"}}
	locs := map[string]string{"model/old_smart/primary/key": "MY_KEY"}
	out, e := pullLocators(g, locs, bindings)
	if e != nil || out["model/smart/primary/key"] != "MY_KEY" {
		t.Fatal(out, e)
	}
	if _, e = pullLocators(g, locs, append(bindings, bindings[0])); e == nil {
		t.Fatal("ambiguous mapping accepted")
	}
	if locator("model", "a_b", "c", "key", nil) == locator("model", "a", "b_c", "key", nil) {
		t.Fatal("generated locator collision")
	}
}

func TestPoliciesAliasCeilingAndNewCredentials(t *testing.T) {
	g := emptyGraph()
	g.Models = []object{{"name": "smart", "spec": object{"nodes": []any{object{"name": "primary", "credentials": object{"mode": "none"}}}}}}
	g.Agents = []object{{"name": "support", "spec": object{"models": []any{object{"name": "old_smart"}}, "tools": []any{}, "policies": []any{object{"name": "member", "models": object{"default": object{"name": "smart"}, "allowed": []any{object{"name": "smart"}}}, "tools": object{}}}}}}
	bindings := []object{{"kind": "model", "name": "smart", "id": "mdl_0000000000000001"}, {"kind": "model", "name": "old_smart", "id": "mdl_0000000000000001"}}
	if e := prepareGraph(g, g, bindings); e != nil {
		t.Fatal(e)
	}
	obj(objects(obj(g.Agents[0]["spec"])["policies"])[0]["models"])["default"] = object{"name": "outside"}
	if e := prepareGraph(g, g, bindings); e == nil {
		t.Fatal("policy outside effective ceiling accepted")
	}
	g.Agents = nil
	obj(objects(obj(g.Models[0]["spec"])["nodes"])[0]["credentials"])["mode"] = "api_key"
	if e := prepareGraph(g, emptyGraph(), nil); e == nil {
		t.Fatal("new key without secret accepted")
	}
}

func TestActualPullPreservesLocatorsThroughAliasAndDetectsChangedBindings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	g := emptyGraph()
	schema := object{"type": "object", "additionalProperties": false}
	g.Applications = []object{{"name": "product", "spec": object{"display_name": "Product", "agents": []any{}}}}
	g.Tools = []object{{"name": "lookup", "archived": true, "spec": object{"display_name": "Lookup", "enabled": true, "current_contract": "v1", "contracts": []any{object{"name": "v1", "description": "Lookup", "type": "server_webhook", "input_schema": schema, "output_schema": object{}, "webhook": object{"url_source": "env", "auth": "bearer_token", "timeout_seconds": 10}}}}}}
	local := emptyGraph()
	local.Tools = []object{maps.Clone(g.Tools[0])}
	local.Tools[0]["name"] = "old_lookup"
	bindings := []any{object{"kind": "tool", "name": "lookup", "id": "tol_0000000000000001"}, object{"kind": "tool", "name": "old_lookup", "id": "tol_0000000000000001"}}
	for _, changed := range []bool{false, true} {
		root := t.TempDir()
		files, e := render(local, map[string]string{"tool/old_lookup/v1/url": "MY_URL", "tool/old_lookup/v1/key": "MY_TOKEN"})
		if e != nil {
			t.Fatal(e)
		}
		writeFiles(t, root, files)
		writeFiles(t, root, map[string]string{"unknown.txt": "untouched"})
		reads := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/v0/auth/identity":
				io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
			case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
				io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
			case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
				reads++
				bs := append([]any{}, bindings...)
				if changed && reads == 2 {
					bs[1] = object{"kind": "tool", "name": "old_lookup", "id": "tol_0000000000000002"}
				}
				io.WriteString(w, wire(object{"version": 1, "bindings": bs}))
			case strings.HasSuffix(r.URL.Path, "/stack"):
				io.WriteString(w, wire(g))
			default:
				w.WriteHeader(404)
			}
		}))
		o, e := parseOptions([]string{"pull", "--force", "--root", root, "--project", "prj_0123456789ABCDEF", "--api-url", s.URL}, map[string]string{"AIMOUNT_TOKEN": "synthetic"}, false)
		if e != nil {
			t.Fatal(e)
		}
		r := runner{ctx: context.Background(), env: map[string]string{"AIMOUNT_TOKEN": "synthetic"}, errOut: io.Discard}
		_, e = r.run(o)
		s.Close()
		if changed {
			if e == nil {
				t.Fatal("changed bindings published")
			}
			for p, text := range files {
				b, _ := os.ReadFile(filepath.Join(root, p))
				if string(b) != text {
					t.Fatal("old root lost")
				}
			}
		} else {
			if e != nil {
				t.Fatal(e)
			}
			i, e := inspect(root)
			if e != nil || i.Locators["tool/lookup/v1/url"] != "MY_URL" || i.Locators["tool/lookup/v1/key"] != "MY_TOKEN" {
				t.Fatal(i, e)
			}
			if _, e := os.Stat(filepath.Join(root, "applications/product.yaml")); e != nil {
				t.Fatal("Application not published with preserved locators", e)
			}
		}
		b, e := os.ReadFile(filepath.Join(root, "unknown.txt"))
		if e != nil || string(b) != "untouched" {
			t.Fatal("unknown lost")
		}
	}
}

func TestPullUnsupportedGoRegexpPreservesOriginalRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, site := range []string{"pattern", "patternProperties"} {
		t.Run(site, func(t *testing.T) {
			root := t.TempDir()
			original := map[string]string{"models/old.yaml": modelYAML, "notes.txt": "unmanaged remains\n"}
			writeFiles(t, root, original)
			before, err := fingerprint(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			unsupported := object{"pattern": "(?=synthetic-private)"}
			if site == "patternProperties" {
				unsupported = object{"patternProperties": object{"(?=synthetic-private)": object{}}}
			}
			g := emptyGraph()
			g.Tools = []object{{"name": "lookup", "spec": object{"display_name": "Lookup", "enabled": true, "current_contract": "v1", "contracts": []any{object{"name": "v1", "description": "Lookup", "type": "client", "input_schema": object{"type": "object", "additionalProperties": false}, "output_schema": unsupported}}}}}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v0/auth/identity":
					io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
					io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
				case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
					io.WriteString(w, `{"version":1,"bindings":[]}`)
				case strings.HasSuffix(r.URL.Path, "/stack"):
					io.WriteString(w, wire(g))
				default:
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			env := map[string]string{"AIMOUNT_TOKEN": "synthetic"}
			o, err := parseOptions([]string{"pull", "--force", "--root", root, "--project", "prj_0123456789ABCDEF", "--api-url", s.URL}, env, false)
			if err != nil {
				t.Fatal(err)
			}
			r := runner{ctx: context.Background(), env: env, errOut: io.Discard}
			_, err = r.run(o)
			if err == nil || !strings.Contains(err.Error(), "Go regexp") || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("pull did not safely explain rejected regexp", err)
			}
			after, e := fingerprint(context.Background(), root)
			if e != nil || before != after {
				t.Fatal("failed pull changed original root", e)
			}
			for path, want := range original {
				got, e := os.ReadFile(filepath.Join(root, path))
				if e != nil || string(got) != want {
					t.Fatal("original file changed", e)
				}
			}
		})
	}
}

func TestActualPullPreservesSchemaScalarMeaning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	schema := object{"type": "object", "properties": object{"string": object{"const": "1e400"}, "1_000": object{"const": "-0x10"}, "large_hex": object{"const": json.Number("18446744073709551616")}, "numeric": object{"const": json.Number("9007199254740993")}, "1e400": object{"const": "null"}}}
	g := emptyGraph()
	g.Tools = []object{{"name": "lookup", "spec": object{"display_name": "Lookup", "enabled": true, "current_contract": "v1", "contracts": []any{object{"name": "v1", "description": "Lookup", "type": "client", "input_schema": object{"type": "object", "additionalProperties": false}, "output_schema": schema}}}}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/auth/identity":
			io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
		case strings.HasSuffix(r.URL.Path, "/stack"):
			io.WriteString(w, wire(g))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	env := map[string]string{"AIMOUNT_TOKEN": "synthetic"}
	o, e := parseOptions([]string{"pull", "--root", root, "--project", "prj_0123456789ABCDEF", "--api-url", s.URL}, env, false)
	if e != nil {
		t.Fatal(e)
	}
	r := runner{ctx: context.Background(), env: env, errOut: io.Discard}
	result, e := r.run(o)
	if e != nil {
		t.Fatal(e)
	}
	if wire(obj(result.Value)["files"]) != `["tools/lookup/contracts/v1.yaml","tools/lookup/tool.yaml"]` {
		t.Fatal("pull file list lost deterministic ordering")
	}
	loaded, e := load(root, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	actual := objects(obj(loaded.Tools[0]["spec"])["contracts"])[0]["output_schema"]
	if wire(actual) != wire(schema) {
		t.Fatal("actual pull changed schema scalar meaning")
	}
}

func TestApplicationCompleteReferencesAliasesAndLifecycle(t *testing.T) {
	current := emptyGraph()
	current.Agents = []object{{"name": "main"}}
	bindings := []object{{"kind": "agent", "name": "main", "id": "agt_0123456789ABCDEF"}, {"kind": "agent", "name": "old_main", "id": "agt_0123456789ABCDEF"}}
	for _, tc := range []struct {
		name                                     string
		refs                                     []any
		local, agentArchived, appArchived, valid bool
	}{
		{"empty", []any{}, false, false, false, true},
		{"server dependency is not local", []any{object{"name": "main"}}, false, false, false, false},
		{"portable alias", []any{object{"name": "old_main"}}, true, false, false, true},
		{"duplicate stable Agent", []any{object{"name": "main"}, object{"name": "old_main"}}, true, false, false, false},
		{"active uses archived", []any{object{"name": "main"}}, true, true, false, false},
		{"archived retains archived", []any{object{"name": "main"}}, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"applications/product.yaml": applicationYAML})
			g, err := load(root, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			a := g.group("application")[0]
			a["archived"] = tc.appArchived
			obj(a["spec"])["agents"] = tc.refs
			if tc.local {
				g.Agents = []object{{"name": "main", "archived": tc.agentArchived, "spec": object{"models": []any{}, "tools": []any{}}}}
			}
			err = prepareGraph(g, current, bindings)
			if (err == nil) != tc.valid {
				t.Fatal("wrong complete Application validation", err)
			}
		})
	}
}

func TestApplicationSemanticSetsAndAccurateErasureScope(t *testing.T) {
	plan := object{"changes": []any{
		object{"kind": "application", "name": "product", "operation": "DELETE", "runtime_cascade": true},
		object{"kind": "agent", "name": "old", "operation": "DELETE", "erasure_scope": []any{"private_tests", "private_files", "configuration"}},
		object{"kind": "application", "name": "other", "operation": "UPDATE", "before": object{"agents": []any{object{"name": "z"}, object{"name": "b"}}}, "after": object{"agents": []any{object{"name": "d"}, object{"name": "a"}}}},
	}}
	out := semantic(plan)
	for _, want := range []string{`agents: +["a","d"] -["b","z"]`, "IRREVERSIBLE Application-owned Users/conversations/Files", "IRREVERSIBLE Agent-owned private-test/files/configuration", "ordinary conversation history preserved", "physical cleanup asynchronous", "detach alone preserves accepted work and ordinary history"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Index(out, "DELETE agent") > strings.Index(out, "UPDATE application") || strings.Contains(out, "default") {
		t.Fatal("wrong semantic order or inferred responder", out)
	}
	if got := differences([]any{object{"name": "z"}, object{"name": "a"}}, []any{object{"name": "a"}, object{"name": "z"}}, "agents"); len(got) != 0 {
		t.Fatal("set reorder is not a semantic change", got)
	}
}

func TestApplicationReadSetRejectsChangedOrIncompleteIdentities(t *testing.T) {
	g := emptyGraph()
	g.Applications = []object{{"name": "product"}}
	binding := object{"kind": "application", "name": "product", "id": "app_0123456789ABCDEF"}
	for _, tc := range []struct {
		name                string
		bindings, resources []any
		valid               bool
	}{
		{"complete", []any{binding}, []any{object{"kind": "application", "id": binding["id"], "etag": "opaque"}}, true},
		{"unprotected application", []any{binding}, []any{}, false},
		{"missing binding", []any{}, []any{object{"kind": "application", "id": binding["id"], "etag": "opaque"}}, false},
		{"recreated name", []any{object{"kind": "application", "name": "product", "id": "app_0000000000000001"}}, []any{object{"kind": "application", "id": "app_0000000000000001", "etag": "opaque"}}, false},
		{"extra saved application", []any{binding, object{"kind": "application", "name": "new", "id": "app_0000000000000001"}}, []any{}, false},
		{"duplicate binding", []any{binding, binding}, []any{}, false},
		{"duplicate resource", []any{binding}, []any{object{"kind": "application", "id": binding["id"], "etag": "opaque"}, object{"kind": "application", "id": binding["id"], "etag": "opaque"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkApplicationReadSet(g, []object{binding}, object{"bindings": tc.bindings, "resources": tc.resources})
			if (err == nil) != tc.valid {
				t.Fatal("incorrect Application read-set protection", err)
			}
		})
	}
}

func TestApplicationWholeProjectPruneReadSetAndEmptyGuards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		name, definition, want string
		prune, completeReadSet bool
		plans, writes          int
	}{
		{"empty prune is not delete all", "", "no resource definitions", true, true, 0, 0},
		{"omitted application", modelYAML, "application saved", false, true, 0, 0},
		{"prune allows omission", modelYAML, "", true, true, 1, 1},
		{"missing application read-set", applicationYAML, "Application read-set", false, false, 1, 0},
		{"complete application read-set", applicationYAML, "", false, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			definition := tc.definition
			if definition != "" {
				path := "models/smart.yaml"
				if definition == applicationYAML {
					definition = strings.Replace(definition, "name: product", "name: saved", 1)
					path = "applications/saved.yaml"
				}
				writeFiles(t, root, map[string]string{path: definition})
			}
			plans, writes := 0, 0
			binding := object{"kind": "application", "name": "saved", "id": "app_0123456789ABCDEF"}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v0/auth/identity":
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
					io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
				case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
					io.WriteString(w, wire(object{"version": 1, "bindings": []any{binding}}))
				case strings.HasSuffix(r.URL.Path, "/stack/plan"):
					plans++
					v, _ := decodeJSON([]byte(validReadSetJSON), false)
					readSet := obj(v)
					if tc.completeReadSet {
						readSet["bindings"] = append(list(readSet["bindings"]), binding)
						readSet["resources"] = append(list(readSet["resources"]), object{"kind": "application", "id": binding["id"], "etag": "opaque"})
					}
					io.WriteString(w, wire(object{"version": 1, "read_set": readSet, "changes": []any{}}))
				case strings.HasSuffix(r.URL.Path, "/stack/apply"):
					writes++
					io.WriteString(w, `{"version":1,"changed":false,"resources":[]}`)
				case strings.HasSuffix(r.URL.Path, "/stack"):
					io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[],"applications":[{"name":"saved","spec":{"display_name":"Saved","agents":[]}}]}`)
				}
			}))
			defer s.Close()
			env := map[string]string{"AIMOUNT_TOKEN": "synthetic", "OPENAI_TEST": "synthetic-key"}
			o := Options{Command: "apply", Root: root, API: s.URL, Project: "prj_0123456789ABCDEF", Approve: true, NonInteractive: true, Prune: tc.prune}
			r := runner{ctx: context.Background(), env: env, errOut: io.Discard}
			_, err := r.run(o)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) || plans != tc.plans || writes != tc.writes {
				t.Fatal("wrong whole-Project guard", err, plans, writes)
			}
		})
	}
}
