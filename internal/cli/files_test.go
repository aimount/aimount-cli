package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modelYAML = `version: 1
kind: model
name: smart
spec:
  display_name: Smart
  limits: {context_window_tokens: 8192, max_output_tokens: 1024}
  capabilities: {input_modalities: [text], output_modalities: [text], tool_calling: true}
  nodes:
    - name: primary
      display_name: Primary
      provider: openai
      model: test
      credentials: {api_key: {from_env: OPENAI_TEST}}
`

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, text := range files {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStrictYAMLAndExactNumbers(t *testing.T) {
	for _, raw := range []string{"a: 1\na: 2", "a: &x [1]\nb: *x", "a: !!str value", "a: !custom value", "a: .nan", "a: 1\n---\nb: 2", "a:\t1", "[1]", "? [a, b]\n: c"} {
		if _, err := parseYAML(raw, "safe.yaml"); err == nil {
			t.Errorf("accepted invalid YAML %q", raw)
		}
	}
	for _, raw := range []string{"!!str a: value", "&key a: value"} {
		if _, e := parseYAML(raw, "safe.yaml"); e == nil {
			t.Errorf("tag/anchor key accepted")
		}
	}
	m, err := parseYAML("const: 9007199254740993\nminimum: 1e-400\nexamples: [9007199254740993.1, -0e-400]\n", "schema.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m["const"] != json.Number("9007199254740993") || m["minimum"] != json.Number("1e-400") {
		t.Fatal("numeric literal changed")
	}
	if strings.Contains(wire("a\u009b\x1b"), "\u009b") {
		t.Fatal("raw C1 terminal control")
	}
}

func TestCompleteLoadBeforeSecretsAndRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/smart.yaml": modelYAML, "agents/support/agent.yaml": "version: 1\nkind: agent\nname: support\nspec:\n  display_name: Support\n  models: [smart]\n  tools: []\n"})
	if _, err := load(root, map[string]string{"OPENAI_TEST": ""}, false); err == nil || !strings.Contains(err.Error(), "instruction.md") {
		t.Fatal("missing instruction accepted", err)
	}
	writeFiles(t, root, map[string]string{"agents/support/instruction.md": "Help\n"})
	g, err := load(root, map[string]string{"OPENAI_TEST": "synthetic-secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire(g), "synthetic-secret") {
		t.Fatal("request credential missing")
	}
	inv, err := inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := render(g, inv.Locators)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire(files), "synthetic-secret") {
		t.Fatal("secret exported")
	}
	other := t.TempDir()
	writeFiles(t, other, files)
	if _, err := load(other, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, fraction := range []string{"1.0000000000000001", "1e-400", "9007199254740993.1"} {
		writeFiles(t, root, map[string]string{"models/smart.yaml": strings.Replace(modelYAML, "max_output_tokens: 1024", "max_output_tokens: "+fraction, 1)})
		if _, err := load(root, nil, false); err == nil {
			t.Errorf("fraction %s accepted", fraction)
		}
	}
}

func TestSchemaGrammarLiteralBounds(t *testing.T) {
	for _, value := range []string{"{multipleOf: 1e-400}", "{minimum: -1e-400, maximum: 1e308}", "{minItems: 9007199254740993}", "{minItems: 1.00e2}", "{minItems: -0e-400}", "{enum: [1e-400, 2e-400]}", "{const: {multipleOf: 0, minItems: 1e-400}}"} {
		m, err := parseYAML(value, "schema")
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(m, "schema"); err != nil {
			t.Errorf("rejected %s: %v", value, err)
		}
	}
	for _, value := range []string{"{$ref: 'https://not-read.example/schema'}", "{type: unsupported}", "{minItems: -1}", "{pattern: '['}", "{multipleOf: 1e400}", "{multipleOf: 0e-400}", "{multipleOf: -1e-400}", "{minItems: 9007199254740993.1}", "{minItems: 1e-400}"} {
		m, err := parseYAML(value, "schema")
		if err != nil {
			continue
		}
		if err := validateSchema(m, "schema"); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}

func TestDiscoveryAggregatesAndPreservesOrphans(t *testing.T) {
	root := t.TempDir()
	if _, err := load(root, nil, false); err == nil {
		t.Fatal("empty accepted")
	}
	writeFiles(t, root, map[string]string{"models/a.yaml": "broken", "models/b.yaml": "broken", "agents/orphan/instruction.md": "orphan", "notes.txt": "kept"})
	_, err := load(root, nil, false)
	if err == nil || !strings.Contains(err.Error(), "a.yaml") || !strings.Contains(err.Error(), "b.yaml") {
		t.Fatal(err)
	}
	i, err := inspect(root)
	if err != nil || len(i.Managed) != 3 {
		t.Fatal(i, err)
	}
}

func TestCanonicalPullModel(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/smart.yaml": modelYAML})
	g, e := load(root, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	f, e := render(g, nil)
	if e != nil {
		t.Fatal(e)
	}
	want := `version: 1
kind: model
name: smart
spec:
  display_name: Smart
  limits:
    context_window_tokens: 8192
    max_output_tokens: 1024
  capabilities:
    input_modalities:
      - text
    output_modalities:
      - text
    tool_calling: true
  nodes:
    - name: primary
      display_name: Primary
      enabled: true
      route_weight: 1
      provider: openai
      model: test
      credentials:
        api_key:
          from_env: AIMOUNT_MODEL_5_SMART_7_PRIMARY_KEY
`
	if f["models/smart.yaml"] != want {
		t.Fatalf("canonical model differs:\n%s", f["models/smart.yaml"])
	}
}

func TestStrictSpecMappingAndPrivateYAMLDiagnostics(t *testing.T) {
	for _, spec := range []string{"[display_name]", "42", "{credential: synthetic-private-value}"} {
		root := t.TempDir()
		writeFiles(t, root, map[string]string{"models/smart.yaml": "version: 1\nkind: model\nname: smart\nspec: " + spec + "\n"})
		_, e := load(root, nil, false)
		if e == nil {
			t.Fatal("non-map or unknown spec accepted")
		}
		if strings.Contains(e.Error(), "synthetic-private-value") {
			t.Fatal("private input in diagnostic")
		}
	}
}

func TestSchemaDataOverflowRoundTripWithoutExplicitTags(t *testing.T) {
	for _, literal := range []string{"1e400", "1e10000", "1e-10000", "900719925474099312345678901234567890"} {
		m, e := parseYAML("const: "+literal, "schema")
		if e != nil {
			t.Fatal(e)
		}
		if m["const"] != json.Number(literal) {
			t.Errorf("numeric data %s decoded as %T", literal, m["const"])
		}
		s := yamlString(m)
		if strings.Contains(s, "!!") {
			t.Errorf("explicit tag exported for %s", literal)
		}
		again, e := parseYAML(s, "schema")
		if e != nil || wire(again) != wire(m) {
			t.Errorf("literal %s changed: %v %s", literal, e, s)
		}
	}
}

func TestYAMLOverflowDecimalSyntaxRemainsNumeric(t *testing.T) {
	for _, tc := range [][2]string{{"+1e400", "1e400"}, {".1e400", "0.1e400"}, {"-.1e400", "-0.1e400"}, {"1.e400", "1.0e400"}} {
		m, e := parseYAML("const: "+tc[0], "schema")
		if e != nil || m["const"] != json.Number(tc[1]) {
			t.Errorf("overflow %s changed: %T %v", tc[0], m["const"], e)
		}
	}
	if _, e := parseYAML("900719925474099312345678901234567890: value", "schema"); e == nil {
		t.Fatal("numeric mapping key accepted")
	}
}

func TestYAML12NumericSpellingMatchesOracle(t *testing.T) {
	for _, s := range []string{"1_000", "0b101", "0x_1", "1_000.0", "0XFF", "0O77"} {
		m, e := parseYAML("const: "+s, "schema")
		if e != nil || m["const"] != s {
			t.Errorf("YAML1.2 scalar %s changed: %T %v", s, m["const"], e)
		}
	}
}

func TestYAML12RawScalarClassificationAndKeys(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want any
	}{
		{"0x10000000000000000", json.Number("18446744073709551616")},
		{"0x10000000000000000000000000", json.Number("1267650600228229401496703205376")},
		{"0o100000000000000000000000", json.Number("590295810358705651712")},
		{"-0x10", "-0x10"}, {"+0o10", "+0o10"}, {"-0o10", "-0o10"}, {"+0x10", "+0x10"},
		{"1_000", "1_000"}, {"0b101", "0b101"}, {"+0123", json.Number("123")}, {"-0123", json.Number("-123")},
		{"1e400", json.Number("1e400")}, {"+1e400", json.Number("1e400")}, {"-.1e400", json.Number("-0.1e400")},
		{"true", true}, {"FALSE", false}, {"null", nil}, {"~", nil}, {"2026-10-10", "2026-10-10"},
	} {
		m, e := parseYAML("value: "+tc.raw, "oracle.yaml")
		if e != nil || m["value"] != tc.want {
			t.Errorf("raw %s changed: got %T %v", tc.raw, m["value"], e)
		}
	}
	for _, key := range []string{"1_000", "-0x10", "+0o10", "0b101", "2026-10-10"} {
		m, e := parseYAML(key+": value", "oracle.yaml")
		if e != nil || m[key] != "value" {
			t.Errorf("valid string mapping key %s rejected: %v", key, e)
		}
	}
	for _, key := range []string{"0x10000000000000000", "0o100000000000000000000000", "1e400", "+0123", "true", "null"} {
		if _, e := parseYAML(key+": value", "oracle.yaml"); e == nil {
			t.Errorf("nonstring mapping key %s accepted", key)
		}
	}
}

func TestYAMLRenderingPreservesInferredStringTypes(t *testing.T) {
	schema := object{"const": "1e400", "enum": []any{"1e400", "1e10000", "0x10000000000000000", "0o100000000000000000000000", "true", "null", ".nan", "+0123", "-.1e400"}, "properties": object{"1e400": object{"const": "1e400"}, "0x10000000000000000": object{"const": "true"}, "1_000": object{}}}
	rendered := yamlString(schema)
	again, e := parseYAML(rendered, "schema.yaml")
	if e != nil || wire(again) != wire(schema) {
		t.Fatal("rendered string became a different type", e)
	}
	if _, ok := again["const"].(string); !ok {
		t.Fatal("string const became a number")
	}
}

func TestSchemaGoRegexpDialect(t *testing.T) {
	for _, site := range []string{"pattern", "patternProperties"} {
		t.Run(site, func(t *testing.T) {
			for _, pattern := range []string{`^[a-z][a-z0-9_]*$`, `\p{L}+`, `(?P<word>[[:alpha:]]+)`, `(?<word>\w+)`, `(?i:hello)`, ""} {
				schema := object{"pattern": pattern}
				if site == "patternProperties" {
					schema = object{"patternProperties": object{pattern: object{"type": "string"}}}
				}
				before := wire(schema)
				if err := validateSchema(schema, "tools/lookup/contracts/v1.yaml"); err != nil {
					t.Fatalf("supported Go regexp rejected: %v", err)
				}
				if wire(schema) != before {
					t.Fatal("authored pattern changed during validation")
				}
				rendered := yamlString(schema)
				again, err := parseYAML(rendered, "tools/lookup/contracts/v1.yaml")
				if err != nil || wire(again) != before {
					t.Fatal("authored pattern changed during rendering", err)
				}
			}
			for _, pattern := range []string{`(?=synthetic-private)`, `(?<=synthetic-private)b`, `(?<!synthetic-private)b`, `(synthetic-private)\1`, `[synthetic-private`} {
				schema := object{"pattern": pattern}
				if site == "patternProperties" {
					schema = object{"patternProperties": object{pattern: object{}}}
				}
				err := validateSchema(schema, "tools/lookup/contracts/v1.yaml")
				if err == nil {
					t.Fatal("unsupported or malformed Go regexp accepted")
				}
				fileErr, ok := err.(*FileError)
				if !ok || fileErr.Diagnostic.Category != "schema" || fileErr.Diagnostic.File != "tools/lookup/contracts/v1.yaml" || !strings.Contains(fileErr.Diagnostic.Message, "Go regexp") || !strings.Contains(fileErr.Diagnostic.Message, site) {
					t.Fatalf("missing safe location-bound Go regexp explanation: %v", err)
				}
				if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(wire(fileErr.Diagnostic), "synthetic-private") {
					t.Fatal("authored regex disclosed in diagnostics")
				}
			}
		})
	}
}

func TestSchemaGoRegexpChecksOnlySchemaKeywords(t *testing.T) {
	for _, schema := range []object{
		{"const": object{"pattern": "(?=literal data)", "patternProperties": object{"(?=literal key)": true}}},
		{"enum": []any{object{"pattern": "(?=literal data)"}}},
	} {
		if err := validateSchema(schema, "schema.yaml"); err != nil {
			t.Fatal("literal schema data interpreted as a regexp", err)
		}
	}
	for _, schema := range []object{
		{"properties": object{"safe": object{"pattern": "(?=synthetic-private)"}}},
		{"items": object{"patternProperties": object{"(?=synthetic-private)": true}}},
	} {
		err := validateSchema(schema, "nested.yaml")
		if err == nil || !strings.Contains(err.Error(), "Go regexp") || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("nested regexp validation is not safe", err)
		}
	}
}

func TestExactIntegerAndSchemaCharacterization(t *testing.T) {
	for _, sign := range []string{"", "-"} {
		for _, coefficient := range []string{"0", "0.000", "1", "1.00", "10.01", "100.000", "1.0000000000000001", "9007199254740993.1", "1024.0000000000000001", "30.0000000000000001", "819200", "1.024"} {
			for _, exponent := range []string{"", "e0", "e1", "e-1", "e2", "e-2", "e308", "e400", "e-400", "e-1000"} {
				literal := sign + coefficient + exponent
				n := json.Number(literal)
				d, p, neg, _ := decimalParts(n)
				whole := p.Sign() >= 0 || d == "0"
				f, e := strconv.ParseFloat(literal, 64)
				finite := e == nil || f == 0
				want := whole && finite && (!neg || d == "0")
				m, err := parseYAML("minItems: "+literal, "schema")
				if err != nil {
					t.Fatal(err)
				}
				err = validateSchema(m, "schema")
				if (err == nil) != want {
					t.Errorf("schema %s: want %v: %v", literal, want, err)
				}
				_, ok := exactInt(n, maxSafe, 1)
				wantField := whole && f >= 1 && f <= float64(maxSafe) && finite
				if ok != wantField {
					t.Errorf("integer %s: want %v got %v", literal, wantField, ok)
				}
			}
		}
	}
}

const applicationYAML = `version: 1
kind: application
name: product
spec:
  display_name: Product assistant
  agents: []
`

func TestApplicationFilesStrictFixedDepthAndCanonical(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"applications/renamed.yaml": applicationYAML, "applications/example/deeper.yaml": "broken", "applications/ignored.yml": "broken"})
	g, err := load(root, nil, false)
	if err != nil || !strings.Contains(wire(g), `"applications":[{"archived":false,"name":"product","spec":{"agents":[],"display_name":"Product assistant"}}]`) {
		t.Fatal("empty Application not discovered", err, wire(g))
	}
	i, err := inspect(root)
	if err != nil || wire(i.Managed) != `["applications/renamed.yaml"]` {
		t.Fatal("wrong Application discovery", i.Managed, err)
	}
	files, err := render(g, nil)
	if err != nil || files["applications/product.yaml"] != applicationYAML {
		t.Fatal("noncanonical Application", files, err)
	}
	archived := strings.Replace(applicationYAML, "spec:", "archived: true\nspec:", 1)
	writeFiles(t, root, map[string]string{"applications/renamed.yaml": archived})
	g, err = load(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	files, err = render(g, nil)
	if err != nil || files["applications/product.yaml"] != archived {
		t.Fatal("archived lifecycle lost", files, err)
	}
	for _, raw := range []string{
		strings.Replace(applicationYAML, "  agents: []\n", "", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: null", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: [main]", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: [{name: main, id: agt_0123456789ABCDEF}]", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: [{name: main}, {name: main}]", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: []\n  description: unsupported", 1),
		strings.Replace(applicationYAML, "agents: []", "agents: []\n  archived: true", 1),
		strings.Replace(applicationYAML, "spec:", "archived: 1\nspec:", 1),
		strings.Replace(applicationYAML, "name: product", "name: Product", 1),
		strings.Replace(applicationYAML, "display_name: Product assistant", "display_name: ' '", 1),
	} {
		writeFiles(t, root, map[string]string{"applications/renamed.yaml": raw})
		if _, err := load(root, nil, false); err == nil {
			t.Errorf("invalid Application accepted: %s", raw)
		}
	}
	writeFiles(t, root, map[string]string{"applications/renamed.yaml": applicationYAML, "applications/duplicate.yaml": applicationYAML})
	if _, err := load(root, nil, false); err == nil {
		t.Fatal("duplicate Application technical name accepted")
	}
}

func TestApplicationCanonicalConnectionNamesAreAnUnorderedSet(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"applications/product.yaml": strings.Replace(applicationYAML, "agents: []", "agents: [{name: zulu}, {name: alpha}]", 1)})
	g, err := load(root, nil, false)
	if err != nil || wire(obj(g.Applications[0]["spec"])["agents"]) != `[{"name":"alpha"},{"name":"zulu"}]` {
		t.Fatal("normalized Application connections are not sorted", err)
	}
	obj(g.Applications[0]["spec"])["agents"] = []any{object{"name": "zulu", "id": "agt_0000000000000002"}, object{"name": "alpha", "id": "agt_0000000000000001"}}
	files, err := render(g, nil)
	want := strings.Replace(applicationYAML, "agents: []", "agents:\n    - name: alpha\n    - name: zulu", 1)
	if err != nil || files["applications/product.yaml"] != want {
		t.Fatal("portable canonical connections changed", err, files)
	}
}
