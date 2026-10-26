package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

func parseYAML(raw, path string) (result object, err error) {
	err = caught(func() {
		if len(raw) > 256<<10 || strings.ContainsAny(raw, "\x00\t") || !utf8.ValidString(raw) {
			fail(path, "invalid YAML", "syntax")
		}
		dec := yaml.NewDecoder(strings.NewReader(raw))
		var doc, extra yaml.Node
		if e := dec.Decode(&doc); e != nil || len(doc.Content) != 1 {
			fail(path, "invalid YAML", "syntax")
		}
		if e := dec.Decode(&extra); e != io.EOF {
			fail(path, "invalid YAML", "syntax")
		}
		v := yamlValue(doc.Content[0], path, 0)
		result = obj(v)
		if result == nil {
			fail(path, "expected mapping", "syntax")
		}
	})
	return
}
func yamlValue(n *yaml.Node, path string, depth int) any {
	if depth > 40 || n.Anchor != "" || n.Kind == yaml.AliasNode || n.Style&yaml.TaggedStyle != 0 {
		fail(path, "invalid YAML", "syntax")
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := object{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				fail(path, "mapping keys must be strings", "syntax")
			}
			key, ok := yamlValue(k, path, depth+1).(string)
			if !ok {
				fail(path, "mapping keys must be strings", "syntax")
			}
			if _, ok := m[key]; ok {
				fail(path, "invalid YAML", "syntax")
			}
			m[key] = yamlValue(n.Content[i+1], path, depth+1)
		}
		return m
	case yaml.SequenceNode:
		a := []any{}
		for _, v := range n.Content {
			a = append(a, yamlValue(v, path, depth+1))
		}
		return a
	case yaml.ScalarNode:
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return n.Value
		}
		v, err := plainYAMLScalar(n.Value)
		if err != nil {
			fail(path, "invalid number", "syntax")
		}
		return v
	}
	fail(path, "invalid YAML", "syntax")
	return nil
}

var yamlIntegerRE = regexp.MustCompile(`^[-+]?[0-9]+$`)
var yamlHexRE = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
var yamlOctalRE = regexp.MustCompile(`^0o[0-7]+$`)
var yamlFloatRE = regexp.MustCompile(`^[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE][-+]?[0-9]+)?$`)
var yamlNonfiniteRE = regexp.MustCompile(`^(?:[-+]?\.(?:inf|Inf|INF)|\.nan|\.NaN|\.NAN)$`)

// Resolve the supported YAML 1.2 core scalars from their original token, not
// the parser's YAML 1.1 tag or machine-width conversion. The same rule handles
// authored values, mapping keys and strings requiring quotes during rendering.
func plainYAMLScalar(s string) (any, error) {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	if yamlNonfiniteRE.MatchString(s) {
		return nil, errors.New("invalid number")
	}
	base, token := 0, s
	switch {
	case yamlIntegerRE.MatchString(s):
		base = 10
		token = strings.TrimPrefix(s, "+")
	case yamlHexRE.MatchString(s):
		base = 16
		token = s[2:]
	case yamlOctalRE.MatchString(s):
		base = 8
		token = s[2:]
	}
	if base != 0 {
		i := new(big.Int)
		if _, ok := i.SetString(token, base); !ok {
			return nil, errors.New("invalid number")
		}
		return json.Number(i.String()), nil
	}
	if yamlFloatRE.MatchString(s) {
		token = strings.TrimPrefix(s, "+")
		if strings.HasPrefix(token, ".") {
			token = "0" + token
		}
		if strings.HasPrefix(token, "-.") {
			token = "-0" + token[1:]
		}
		if i := strings.IndexAny(token, "eE"); i > 0 && token[i-1] == '.' {
			token = token[:i] + "0" + token[i:]
		}
		if strings.HasSuffix(token, ".") {
			token += "0"
		}
		if !jsonNumberRE.MatchString(token) {
			return nil, errors.New("invalid number")
		}
		return json.Number(token), nil
	}
	return s, nil
}

func validateSchema(value any, path string) error {
	return caught(func() {
		leaf := strings.Fields("description type title enum const default examples required dependentRequired minimum maximum exclusiveMinimum exclusiveMaximum multipleOf minLength maxLength pattern minItems maxItems uniqueItems minContains maxContains minProperties maxProperties readOnly writeOnly deprecated")
		mappings := strings.Fields("properties patternProperties $defs dependentSchemas")
		singles := strings.Fields("items additionalProperties contains propertyNames not if then else unevaluatedProperties unevaluatedItems")
		lists := strings.Fields("allOf anyOf oneOf prefixItems")
		numbers := strings.Fields("minimum maximum exclusiveMinimum exclusiveMaximum multipleOf minLength maxLength minItems maxItems minContains maxContains minProperties maxProperties")
		var check func(any, int) any
		check = func(v any, depth int) any {
			if depth > 16 {
				fail(path, "schema depth exceeded")
			}
			if _, ok := v.(bool); ok {
				return v
			}
			s := obj(v)
			if s == nil {
				fail(path, "invalid schema")
			}
			projection := maps.Clone(s)
			for k, item := range s {
				switch {
				case slices.Contains(leaf, k):
					if k == "pattern" {
						pattern, ok := item.(string)
						if !ok {
							fail(path, "pattern must be a Go regexp string")
						}
						if _, err := regexp.Compile(pattern); err != nil {
							fail(path, "pattern is not a valid Go regexp; repair unsupported or malformed syntax")
						}
					}
					if slices.Contains(numbers, k) {
						n, ok := item.(json.Number)
						if !ok {
							fail(path, "invalid schema constraints")
						}
						f, e := strconv.ParseFloat(string(n), 64)
						if e != nil && f != 0 {
							fail(path, "invalid schema constraints")
						}
						d, p, neg, ok := decimalParts(n)
						if !ok {
							fail(path, "invalid schema constraints")
						}
						whole := d == "0" || p.Sign() >= 0
						// Detached grammar-only projection avoids machine int overflow in the compiler.
						project := "0"
						if d != "0" {
							project = "1"
							if !whole {
								project = "0.5"
							}
							if neg {
								project = "-" + project
							}
						}
						projection[k] = json.Number(project)
					}
				case slices.Contains(mappings, k):
					m := obj(item)
					if m == nil {
						fail(path, "invalid schema mapping")
					}
					p := object{}
					for key, child := range m {
						if k == "patternProperties" {
							if _, err := regexp.Compile(key); err != nil {
								fail(path, "patternProperties key is not a valid Go regexp; repair unsupported or malformed syntax")
							}
						}
						p[key] = check(child, depth+1)
					}
					projection[k] = p
				case slices.Contains(singles, k):
					projection[k] = check(item, depth+1)
				case slices.Contains(lists, k):
					a, ok := item.([]any)
					if !ok {
						fail(path, "invalid schema collection")
					}
					p := []any{}
					for _, child := range a {
						p = append(p, check(child, depth+1))
					}
					projection[k] = p
				default:
					fail(path, "unsupported schema keyword")
				}
			}
			return projection
		}
		projection := check(value, 0)
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		if e := c.AddResource("https://schema.aimount.invalid/local", projection); e != nil {
			fail(path, "invalid schema constraints")
		}
		if _, e := c.Compile("https://schema.aimount.invalid/local"); e != nil {
			fail(path, "invalid schema constraints")
		}
	})
}
func endpoint(v any, path string) {
	text(v, 2048, path, true)
	s := str(v)
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(s, "?#\\") || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		fail(path, "invalid public HTTPS endpoint")
	}
}
func reference(v any, path string, tool bool) object {
	if s, ok := v.(string); ok {
		technical(s, path, false)
		return object{"name": s}
	}
	allowed := []string{"name"}
	if tool {
		allowed = append(allowed, "contract")
	}
	m := strictObject(v, allowed, []string{"name"}, path)
	technical(m["name"], path, false)
	if c, ok := m["contract"]; ok {
		technical(c, path, true)
	}
	return maps.Clone(m)
}
func filter(v any, path string, models bool) object {
	allowed := []string{"allowed", "denied"}
	required := []string{}
	if models {
		allowed = append(allowed, "default")
		required = append(required, "default")
	}
	f := strictObject(v, allowed, required, path)
	if _, a := f["allowed"]; a {
		if _, d := f["denied"]; d {
			fail(path, "mutually exclusive filters")
		}
	}
	out := object{}
	if models {
		out["default"] = reference(f["default"], path, false)
	}
	for _, field := range []string{"allowed", "denied"} {
		if v, ok := f[field]; ok {
			a := []any{}
			for _, r := range collection(v, path, 0) {
				a = append(a, reference(r, path, false))
			}
			sort.Slice(a, func(i, j int) bool { return str(obj(a[i])["name"]) < str(obj(a[j])["name"]) })
			out[field] = a
		}
	}
	return out
}
func envelope(raw, kind, path string) object {
	m, e := parseYAML(raw, path)
	if e != nil {
		panic(e)
	}
	strictObject(m, []string{"version", "kind", "name", "archived", "spec"}, []string{"version", "kind", "name", "spec"}, path)
	if !numberIs(m["version"], 1) || m["kind"] != kind {
		fail(path, "wrong version or kind")
	}
	technical(m["name"], path, kind == "tool_contract")
	if v, ok := m["archived"]; ok {
		if kind == "policy" || kind == "tool_contract" {
			fail(path, "nested archived unsupported")
		}
		boolean(v, path)
	}
	return m
}
func entries(path string) []string {
	es, e := os.ReadDir(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		fail(path, "unreadable directory", "discovery")
	}
	ss := []string{}
	for _, v := range es {
		ss = append(ss, v.Name())
	}
	return ss
}
func safeText(root, path string) string {
	actual, e := filepath.EvalSymlinks(path)
	if e != nil {
		fail(path, "unreadable UTF-8 file", "discovery")
	}
	rel, e := filepath.Rel(root, actual)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		fail(path, "file escapes root", "discovery")
	}
	st, e := os.Stat(actual)
	if e != nil || !st.Mode().IsRegular() || st.Size() > 256<<10 {
		fail(path, "invalid resource file", "discovery")
	}
	b, e := os.ReadFile(actual)
	if e != nil || !utf8.Valid(b) {
		fail(path, "unreadable UTF-8 file", "discovery")
	}
	return string(b)
}

type definition struct{ path, kind string }
type packageFiles struct {
	folder           string
	children, nested []string
}
type inventory struct {
	Managed       []string
	Locators      map[string]string
	models        []string
	applications  []string
	tools, agents []packageFiles
	definitions   []definition
}

func discover(root string) inventory {
	i := inventory{Locators: map[string]string{}}
	for _, f := range entries(filepath.Join(root, "applications")) {
		if strings.HasSuffix(f, ".yaml") {
			i.applications = append(i.applications, f)
			i.definitions = append(i.definitions, definition{"applications/" + f, "application"})
			i.Managed = append(i.Managed, "applications/"+f)
		}
	}
	for _, f := range entries(filepath.Join(root, "models")) {
		if strings.HasSuffix(f, ".yaml") {
			i.models = append(i.models, f)
			i.definitions = append(i.definitions, definition{"models/" + f, "model"})
			i.Managed = append(i.Managed, "models/"+f)
		}
	}
	for _, dir := range []string{"tools", "agents"} {
		parent, nested, kind := "tool", "contracts", "tool_contract"
		if dir == "agents" {
			parent, nested, kind = "agent", "policies", "policy"
		}
		for _, folder := range entries(filepath.Join(root, dir)) {
			p := filepath.Join(root, dir, folder)
			st, e := os.Stat(p)
			if e != nil {
				fail(p, "unreadable directory", "discovery")
			}
			if !st.IsDir() {
				continue
			}
			children := entries(p)
			files := []string{}
			if slices.Contains(children, parent+".yaml") {
				rel := dir + "/" + folder + "/" + parent + ".yaml"
				i.Managed = append(i.Managed, rel)
				i.definitions = append(i.definitions, definition{rel, parent})
			}
			if dir == "agents" && slices.Contains(children, "instruction.md") {
				i.Managed = append(i.Managed, dir+"/"+folder+"/instruction.md")
			}
			for _, f := range entries(filepath.Join(p, nested)) {
				if strings.HasSuffix(f, ".yaml") {
					files = append(files, f)
					rel := dir + "/" + folder + "/" + nested + "/" + f
					i.Managed = append(i.Managed, rel)
					i.definitions = append(i.definitions, definition{rel, kind})
				}
			}
			pf := packageFiles{folder, children, files}
			if dir == "tools" {
				i.tools = append(i.tools, pf)
			} else {
				i.agents = append(i.agents, pf)
			}
		}
	}
	sort.Strings(i.Managed)
	return i
}
func inspect(root string) (inv inventory, err error) {
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return inv, errors.New("unreadable root")
	}
	err = caught(func() {
		inv = discover(root)
		for _, rel := range inv.Managed {
			if !strings.HasSuffix(rel, ".yaml") {
				continue
			}
			raw := safeText(root, filepath.Join(root, rel))
			doc, e := parseYAML(raw, rel)
			if e != nil {
				continue
			}
			if doc["kind"] == "model" && nameRE.MatchString(str(doc["name"])) {
				for _, n := range objects(obj(doc["spec"])["nodes"]) {
					loc := str(obj(obj(n["credentials"])["api_key"])["from_env"])
					if nameRE.MatchString(str(n["name"])) && envRE.MatchString(loc) {
						inv.Locators["model/"+str(doc["name"])+"/"+str(n["name"])+"/key"] = loc
					}
				}
			}
			if doc["kind"] == "tool_contract" && contractRE.MatchString(str(doc["name"])) {
				owner := filepath.Join(filepath.Dir(filepath.Dir(rel)), "tool.yaml")
				var raw string
				var errs []error
				collect(&errs, func() { raw = safeText(root, filepath.Join(root, owner)) })
				if len(errs) > 0 {
					continue
				}
				tool, e := parseYAML(raw, owner)
				if e != nil || tool["kind"] != "tool" || !nameRE.MatchString(str(tool["name"])) {
					continue
				}
				w := obj(obj(doc["spec"])["webhook"])
				for purpose, loc := range map[string]string{"url": str(obj(w["url"])["from_env"]), "key": str(obj(obj(w["auth"])["bearer_token"])["from_env"])} {
					if envRE.MatchString(loc) {
						inv.Locators["tool/"+str(tool["name"])+"/"+str(doc["name"])+"/"+purpose] = loc
					}
				}
			}
		}
	})
	return
}

type pendingSecret struct {
	parent          object
	target, locator string
	url             object
}

func load(root string, env map[string]string, pull bool) (g graph, err error) {
	g = emptyGraph()
	actual, e := filepath.EvalSymlinks(root)
	if e != nil {
		return g, errors.New("unreadable root")
	}
	root = actual
	boundaryErr := caught(func() {
		if !pull && slices.Contains(entries(root), "project.yaml") {
			fail("project.yaml", "unsupported Project declaration")
		}
		inv := discover(root)
		parsed := map[string]object{}
		var errs []error
		pending := []pendingSecret{}
		for _, d := range inv.definitions {
			collect(&errs, func() { parsed[d.path] = envelope(safeText(root, filepath.Join(root, d.path)), d.kind, d.path) })
		}
		if len(errs) > 0 {
			err = &fileErrors{errs}
			return
		}
		seen := map[string]bool{}
		parent := func(doc object, kind, path string) object {
			name := str(doc["name"])
			if seen[kind+":"+name] {
				fail(path, "duplicate definition", "identity")
			}
			seen[kind+":"+name] = true
			archived := any(false)
			if v, ok := doc["archived"]; ok {
				archived = v
			}
			if obj(doc["spec"]) == nil {
				fail(path, "invalid or unknown fields")
			}
			return object{"name": name, "archived": archived, "spec": maps.Clone(obj(doc["spec"]))}
		}
		for _, f := range inv.models {
			collect(&errs, func() {
				path := "models/" + f
				m := parent(parsed[path], "model", path)
				s := strictObject(m["spec"], strings.Fields("display_name description limits capabilities nodes"), strings.Fields("display_name limits capabilities nodes"), path)
				display(s, path)
				limits := strictObject(s["limits"], strings.Fields("context_window_tokens max_output_tokens"), strings.Fields("context_window_tokens max_output_tokens"), path)
				contextN := positive(limits["context_window_tokens"], path, maxSafe)
				outputN := positive(limits["max_output_tokens"], path, maxSafe)
				if outputN > contextN {
					fail(path, "output exceeds context")
				}
				limits["context_window_tokens"] = contextN
				limits["max_output_tokens"] = outputN
				caps := strictObject(s["capabilities"], strings.Fields("input_modalities output_modalities tool_calling"), strings.Fields("input_modalities output_modalities tool_calling"), path)
				inputs := collection(caps["input_modalities"], path, 1)
				valid := false
				unique := map[string]bool{}
				for _, v := range inputs {
					x := str(v)
					if !slices.Contains([]string{"text", "image", "file"}, x) || unique[x] {
						fail(path, "invalid capabilities")
					}
					unique[x] = true
					valid = valid || x == "text"
				}
				if !valid || wire(caps["output_modalities"]) != `["text"]` {
					fail(path, "invalid capabilities")
				}
				boolean(caps["tool_calling"], path)
				names := map[string]bool{}
				nodes := []any{}
				enabled := false
				for _, v := range collection(s["nodes"], path, 1) {
					n := strictObject(v, strings.Fields("name display_name enabled route_weight provider model api base_url credentials"), strings.Fields("name display_name provider model credentials"), path)
					technical(n["name"], path, false)
					display(n, path)
					text(n["model"], 256, path, true)
					name := str(n["name"])
					if names[name] {
						fail(path, "duplicate Node")
					}
					names[name] = true
					out := maps.Clone(n)
					if _, ok := out["enabled"]; !ok {
						out["enabled"] = true
					}
					if _, ok := out["route_weight"]; !ok {
						out["route_weight"] = 1
					}
					boolean(out["enabled"], path)
					enabled = enabled || out["enabled"] == true
					out["route_weight"] = positive(out["route_weight"], path, maxSafe)
					cr := strictObject(n["credentials"], []string{"api_key", "none"}, nil, path)
					if len(cr) != 1 {
						fail(path, "one credential mode required")
					}
					if v, ok := cr["api_key"]; ok {
						c := strictObject(v, []string{"from_env"}, []string{"from_env"}, path)
						loc := envName(c["from_env"], path)
						pending = append(pending, pendingSecret{m, name, loc, nil})
						out["credentials"] = object{"mode": "api_key"}
					} else {
						strictObject(cr["none"], nil, nil, path)
						out["credentials"] = object{"mode": "none"}
					}
					switch n["provider"] {
					case "openai":
						if n["api"] != nil || n["base_url"] != nil || obj(out["credentials"])["mode"] != "api_key" {
							fail(path, "invalid OpenAI route")
						}
					case "openai_compatible":
						if !slices.Contains([]string{"responses", "chat_completions"}, str(n["api"])) {
							fail(path, "explicit compatible API required")
						}
						endpoint(n["base_url"], path)
					default:
						fail(path, "unsupported provider")
					}
					nodes = append(nodes, out)
				}
				if !enabled {
					fail(path, "enabled Node required")
				}
				sortNamed(nodes)
				s["nodes"] = nodes
				g.Models = append(g.Models, m)
			})
		}
		for _, p := range inv.tools {
			collect(&errs, func() {
				if !slices.Contains(p.children, "tool.yaml") {
					if len(p.nested) > 0 {
						fail("tools/"+p.folder, "orphan Contracts", "discovery")
					}
					return
				}
				path := "tools/" + p.folder + "/tool.yaml"
				t := parent(parsed[path], "tool", path)
				s := strictObject(t["spec"], strings.Fields("display_name description enabled current_contract"), strings.Fields("display_name current_contract"), path)
				display(s, path)
				if _, ok := s["enabled"]; !ok {
					s["enabled"] = true
				}
				boolean(s["enabled"], path)
				technical(s["current_contract"], path, true)
				contracts := []any{}
				names := map[string]bool{}
				for _, f := range p.nested {
					cp := "tools/" + p.folder + "/contracts/" + f
					doc := parsed[cp]
					c := strictObject(doc["spec"], strings.Fields("description type input_schema output_schema webhook"), strings.Fields("description type input_schema output_schema"), cp)
					text(c["description"], 16384, cp, true)
					name := str(doc["name"])
					if names[name] {
						fail(cp, "duplicate Contract")
					}
					names[name] = true
					input := obj(c["input_schema"])
					if input == nil || input["type"] != "object" || input["additionalProperties"] != false || obj(c["output_schema"]) == nil {
						fail(cp, "invalid Tool schema")
					}
					for _, v := range []any{c["input_schema"], c["output_schema"]} {
						if e := validateSchema(v, cp); e != nil {
							panic(e)
						}
					}
					switch c["type"] {
					case "client":
						if c["webhook"] != nil {
							fail(cp, "client webhook unsupported")
						}
					case "server_webhook":
						w := strictObject(c["webhook"], strings.Fields("url auth timeout_seconds invocation_name"), strings.Fields("url auth"), cp)
						u := strictObject(w["url"], []string{"value", "from_env"}, nil, cp)
						auth := strictObject(w["auth"], []string{"none", "bearer_token"}, nil, cp)
						if len(u) != 1 || len(auth) != 1 {
							fail(cp, "one URL source and auth mode required")
						}
						out := object{"url_source": "value", "auth": "none", "timeout_seconds": int64(10)}
						if v, ok := w["timeout_seconds"]; ok {
							out["timeout_seconds"] = positive(v, cp, 30)
						}
						if v, ok := w["invocation_name"]; ok {
							technical(v, cp, false)
							out["invocation_name"] = v
						}
						if v, ok := u["from_env"]; ok {
							out["url_source"] = "env"
							pending = append(pending, pendingSecret{t, name, envName(v, cp), out})
						} else {
							endpoint(u["value"], cp)
							out["url"] = u["value"]
						}
						if v, ok := auth["bearer_token"]; ok {
							a := strictObject(v, []string{"from_env"}, []string{"from_env"}, cp)
							out["auth"] = "bearer_token"
							pending = append(pending, pendingSecret{t, name, envName(a["from_env"], cp), nil})
						} else {
							strictObject(auth["none"], nil, nil, cp)
						}
						c["webhook"] = out
					default:
						fail(cp, "unsupported Contract type")
					}
					out := maps.Clone(c)
					out["name"] = name
					contracts = append(contracts, out)
				}
				collection(contracts, path, 1)
				s["contracts"] = contracts
				if len(wire(s)) > 256<<10 {
					fail(path, "configuration exceeds 256 KiB", "invariant")
				}
				if !names[str(s["current_contract"])] {
					fail(path, "missing current Contract", "reference")
				}
				g.Tools = append(g.Tools, t)
			})
		}
		for _, p := range inv.agents {
			collect(&errs, func() {
				if !slices.Contains(p.children, "agent.yaml") {
					if slices.Contains(p.children, "instruction.md") || len(p.nested) > 0 {
						fail("agents/"+p.folder, "orphan Agent files", "discovery")
					}
					return
				}
				path := "agents/" + p.folder + "/agent.yaml"
				a := parent(parsed[path], "agent", path)
				s := strictObject(a["spec"], strings.Fields("display_name description models tools"), strings.Fields("display_name models tools"), path)
				display(s, path)
				for _, kind := range []string{"model", "tool"} {
					min := 0
					if kind == "model" {
						min = 1
					}
					refs := []any{}
					seen := map[string]bool{}
					for _, v := range collection(s[kind+"s"], path, min) {
						r := reference(v, path, kind == "tool")
						name := str(r["name"])
						if seen[name] {
							fail(path, "duplicate reference")
						}
						seen[name] = true
						refs = append(refs, r)
					}
					s[kind+"s"] = refs
				}
				ip := "agents/" + p.folder + "/instruction.md"
				instruction := safeText(root, filepath.Join(root, ip))
				text(instruction, 128<<10, ip, true)
				s["instruction"] = instruction
				policies := []any{}
				names := map[string]bool{}
				for _, f := range p.nested {
					pp := "agents/" + p.folder + "/policies/" + f
					doc := parsed[pp]
					pol := strictObject(doc["spec"], strings.Fields("display_name description enabled models tools"), strings.Fields("display_name models tools"), pp)
					display(pol, pp)
					if _, ok := pol["enabled"]; !ok {
						pol["enabled"] = true
					}
					boolean(pol["enabled"], pp)
					name := str(doc["name"])
					if names[name] {
						fail(pp, "duplicate Policy")
					}
					names[name] = true
					pol["models"] = filter(pol["models"], pp, true)
					pol["tools"] = filter(pol["tools"], pp, false)
					out := maps.Clone(pol)
					out["name"] = name
					policies = append(policies, out)
				}
				collection(policies, path, 0)
				s["policies"] = policies
				if len(wire(s)) > 256<<10 {
					fail(path, "configuration exceeds 256 KiB", "invariant")
				}
				g.Agents = append(g.Agents, a)
			})
		}
		for _, f := range inv.applications {
			collect(&errs, func() {
				path := "applications/" + f
				a := parent(parsed[path], "application", path)
				s := strictObject(a["spec"], []string{"display_name", "agents"}, []string{"display_name", "agents"}, path)
				display(s, path)
				refs := []any{}
				seen := map[string]bool{}
				for _, v := range collection(s["agents"], path, 0) {
					r := strictObject(v, []string{"name"}, []string{"name"}, path)
					technical(r["name"], path, false)
					name := str(r["name"])
					if seen[name] {
						fail(path, "duplicate Agent reference")
					}
					seen[name] = true
					refs = append(refs, maps.Clone(r))
				}
				sortNamed(refs)
				s["agents"] = refs
				g.Applications = append(g.Applications, a)
			})
		}
		if len(errs) > 0 {
			err = &fileErrors{errs}
			return
		}
		if !pull && len(g.Models)+len(g.Tools)+len(g.Agents)+len(g.Applications) == 0 {
			fail(root, "no resource definitions", "discovery")
		}
		for _, group := range [][]object{g.Models, g.Tools, g.Agents, g.Applications} {
			sort.Slice(group, func(i, j int) bool { return str(group[i]["name"]) < str(group[j]["name"]) })
		}
		for _, p := range pending {
			v, ok := env[p.locator]
			if !ok {
				continue
			}
			if v == "" || len(v) > 16384 || strings.ContainsRune(v, 0) {
				fail(p.locator, "invalid environment value", "secret")
			}
			if p.url != nil {
				endpoint(v, p.locator)
				p.url["url"] = v
			} else {
				a := list(p.parent["credential_changes"])
				if a == nil {
					a = []any{}
				}
				p.parent["credential_changes"] = append(a, object{"target": p.target, "action": "replace", "value": v})
			}
		}
		for _, m := range g.Models {
			if len(wire(m["spec"])) > 256<<10 {
				fail("models/"+str(m["name"]), "configuration exceeds 256 KiB", "invariant")
			}
		}
		if len(wire(g)) > 16<<20 {
			fail(root, "Stack exceeds request limit", "invariant")
		}
	})
	if boundaryErr != nil {
		err = boundaryErr
	}
	return
}
func sortNamed(a []any) {
	sort.Slice(a, func(i, j int) bool { return str(obj(a[i])["name"]) < str(obj(a[j])["name"]) })
}

// YAML nodes retain exact schema tokens and canonical domain field order.
func yamlNode(v any) *yaml.Node {
	switch x := v.(type) {
	case object:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range orderedKeys(x) {
			n.Content = append(n.Content, yamlNode(k), yamlNode(x[k]))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, v := range x {
			n.Content = append(n.Content, yamlNode(v))
		}
		return n
	case json.Number:
		// The raw token determines its implicit YAML 1.2 type. Supplying an int
		// tag makes the encoder emit !!int when its legacy resolver overflows.
		return &yaml.Node{Kind: yaml.ScalarNode, Value: string(x)}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(x)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(x, 10)}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case string:
		n := &yaml.Node{}
		n.SetString(x)
		v, err := plainYAMLScalar(x)
		if _, stringValue := v.(string); err != nil || !stringValue {
			n.Style = yaml.DoubleQuotedStyle
		}
		return n
	}
	fail("export", "invalid exported resource", "remote")
	return nil
}
func orderedKeys(m object) []string {
	order := []string{}
	switch {
	case m["kind"] != nil && m["spec"] != nil:
		order = strings.Fields("version kind name archived spec")
	case m["limits"] != nil:
		order = strings.Fields("display_name description limits capabilities nodes")
	case m["provider"] != nil:
		order = strings.Fields("name display_name enabled route_weight provider model api base_url credentials")
	case m["context_window_tokens"] != nil:
		order = strings.Fields("context_window_tokens max_output_tokens")
	case m["input_modalities"] != nil:
		order = strings.Fields("input_modalities output_modalities tool_calling")
	case m["current_contract"] != nil:
		order = strings.Fields("display_name description enabled current_contract")
	case m["input_schema"] != nil:
		order = strings.Fields("description type input_schema output_schema webhook")
	case m["timeout_seconds"] != nil:
		order = strings.Fields("url auth timeout_seconds invocation_name")
	case m["models"] != nil:
		order = strings.Fields("display_name description enabled models tools")
	case m["agents"] != nil:
		order = strings.Fields("display_name agents")
	case m["default"] != nil || m["allowed"] != nil || m["denied"] != nil:
		order = strings.Fields("default allowed denied")
	case m["contract"] != nil && m["name"] != nil:
		order = []string{"name", "contract"}
	}
	out := []string{}
	for _, k := range order {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}
func yamlString(v any) string {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	e.DefaultSeqIndent()
	if err := e.Encode(yamlNode(v)); err != nil {
		fail("export", "invalid exported resource", "remote")
	}
	e.Close()
	return strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\r", "\n"), "\n") + "\n"
}
func locator(kind, parent, target, purpose string, locs map[string]string) string {
	if v, ok := locs[kind+"/"+parent+"/"+target+"/"+purpose]; ok {
		return v
	}
	return "AIMOUNT_" + strings.ToUpper(kind) + "_" + strconv.Itoa(len(parent)) + "_" + strings.ToUpper(parent) + "_" + strconv.Itoa(len(target)) + "_" + strings.ToUpper(target) + "_" + strings.ToUpper(purpose)
}
func authored(kind, name string, archived any, s object) object {
	m := object{"version": 1, "kind": kind, "name": name, "spec": s}
	if archived == true {
		m["archived"] = true
	}
	return m
}
func render(g graph, locs map[string]string) (files map[string]string, err error) {
	files = map[string]string{}
	err = caught(func() {
		for _, a := range g.Applications {
			name := str(a["name"])
			technical(name, "export", false)
			s := obj(a["spec"])
			refs := []any{}
			for _, r := range objects(s["agents"]) {
				refs = append(refs, fields(r, "name"))
			}
			sortNamed(refs)
			files["applications/"+name+".yaml"] = yamlString(authored("application", name, a["archived"], object{"display_name": s["display_name"], "agents": refs}))
		}
		for _, m := range g.Models {
			name := str(m["name"])
			technical(name, "export", false)
			s := obj(m["spec"])
			out := fields(s, "display_name", "description")
			out["limits"] = fields(obj(s["limits"]), "context_window_tokens", "max_output_tokens")
			out["capabilities"] = fields(obj(s["capabilities"]), "input_modalities", "output_modalities", "tool_calling")
			nodes := []any{}
			for _, n := range objects(s["nodes"]) {
				v := fields(n, "name", "display_name", "enabled", "route_weight", "provider", "model", "api", "base_url")
				if obj(n["credentials"])["mode"] == "api_key" {
					v["credentials"] = object{"api_key": object{"from_env": locator("model", name, str(n["name"]), "key", locs)}}
				} else {
					v["credentials"] = object{"none": object{}}
				}
				nodes = append(nodes, v)
			}
			out["nodes"] = nodes
			files["models/"+name+".yaml"] = yamlString(authored("model", name, m["archived"], out))
		}
		for _, t := range g.Tools {
			name := str(t["name"])
			technical(name, "export", false)
			s := obj(t["spec"])
			files["tools/"+name+"/tool.yaml"] = yamlString(authored("tool", name, t["archived"], fields(s, "display_name", "description", "enabled", "current_contract")))
			for _, c := range objects(s["contracts"]) {
				cn := str(c["name"])
				technical(cn, "export", true)
				out := fields(c, "description", "type", "input_schema", "output_schema")
				if w := obj(c["webhook"]); w != nil {
					u := object{"value": w["url"]}
					if w["url_source"] == "env" {
						u = object{"from_env": locator("tool", name, cn, "url", locs)}
					}
					a := object{"none": object{}}
					if w["auth"] == "bearer_token" {
						a = object{"bearer_token": object{"from_env": locator("tool", name, cn, "key", locs)}}
					}
					wh := object{"url": u, "auth": a, "timeout_seconds": w["timeout_seconds"]}
					if v, ok := w["invocation_name"]; ok {
						wh["invocation_name"] = v
					}
					out["webhook"] = wh
				}
				files["tools/"+name+"/contracts/"+cn+".yaml"] = yamlString(authored("tool_contract", cn, false, out))
			}
		}
		for _, a := range g.Agents {
			name := str(a["name"])
			technical(name, "export", false)
			s := obj(a["spec"])
			out := fields(s, "display_name", "description")
			models, tools := []any{}, []any{}
			for _, r := range objects(s["models"]) {
				models = append(models, r["name"])
			}
			for _, r := range objects(s["tools"]) {
				if r["contract"] != nil {
					tools = append(tools, fields(r, "name", "contract"))
				} else {
					tools = append(tools, r["name"])
				}
			}
			out["models"] = models
			out["tools"] = tools
			files["agents/"+name+"/agent.yaml"] = yamlString(authored("agent", name, a["archived"], out))
			instruction, ok := s["instruction"].(string)
			if !ok {
				fail("export", "invalid instruction", "remote")
			}
			files["agents/"+name+"/instruction.md"] = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(instruction, "\r\n", "\n"), "\r", "\n"), "\n") + "\n"
			for _, p := range objects(s["policies"]) {
				pn := str(p["name"])
				technical(pn, "export", false)
				out := fields(p, "display_name", "description", "enabled")
				for _, kind := range []string{"model", "tool"} {
					f := obj(p[kind+"s"])
					af := object{}
					for _, k := range []string{"default", "allowed", "denied"} {
						if v, ok := f[k]; ok {
							if k == "default" {
								af[k] = obj(v)["name"]
							} else {
								names := []string{}
								for _, r := range objects(v) {
									names = append(names, str(r["name"]))
								}
								sort.Strings(names)
								ls := []any{}
								for _, n := range names {
									ls = append(ls, n)
								}
								af[k] = ls
							}
						}
					}
					out[kind+"s"] = af
				}
				files["agents/"+name+"/policies/"+pn+".yaml"] = yamlString(authored("policy", pn, false, out))
			}
		}
	})
	return
}

// readRemoteSpec validates consumed response fields and returns only safe fields.
// A nil result is invalid; an empty object is a valid partial spec.
func readRemoteSpec(kind string, s object, partial bool) object {
	if s == nil {
		return nil
	}
	if kind == "application" {
		out := fields(s, "display_name")
		if v, ok := s["display_name"]; ok {
			if err := caught(func() { display(object{"display_name": v}, "export") }); err != nil {
				return nil
			}
		} else if !partial {
			return nil
		}
		v, ok := s["agents"]
		if !ok {
			if !partial {
				return nil
			}
			return out
		}
		refs, ok := v.([]any)
		if !ok || len(refs) > 100 {
			return nil
		}
		seen := map[string]bool{}
		safe := []any{}
		for _, v := range refs {
			r := obj(v)
			name := str(r["name"])
			if !nameRE.MatchString(name) || seen[name] {
				return nil
			}
			seen[name] = true
			safe = append(safe, fields(r, "name"))
		}
		sortNamed(safe)
		out["agents"] = safe
		return out
	}
	for _, k := range []string{"display_name", "description", "instruction", "current_contract"} {
		if v, ok := s[k]; ok {
			if _, ok := v.(string); !ok {
				return nil
			}
		}
	}
	if v, ok := s["enabled"]; ok {
		if _, ok := v.(bool); !ok {
			return nil
		}
	}
	var out object
	readArray := func(field string, required bool, read func(object) object) bool {
		v, ok := s[field]
		if !ok {
			return !required
		}
		a, ok := v.([]any)
		if !ok {
			return false
		}
		safe := []any{}
		for _, v := range a {
			m := obj(v)
			if m == nil {
				return false
			}
			p := read(m)
			if p == nil {
				return false
			}
			safe = append(safe, p)
		}
		out[field] = safe
		return true
	}
	name := func(m object) bool { return nameRE.MatchString(str(m["name"])) }
	switch kind {
	case "model":
		out = fields(s, "display_name", "description")
		if v, ok := s["limits"]; ok {
			m := obj(v)
			if m == nil {
				return nil
			}
			contextN, cok := exactInt(m["context_window_tokens"], maxSafe, 1)
			outputN, ook := exactInt(m["max_output_tokens"], maxSafe, 1)
			if !cok || !ook || outputN > contextN {
				return nil
			}
			out["limits"] = fields(m, "context_window_tokens", "max_output_tokens")
		}
		if v, ok := s["capabilities"]; ok {
			m := obj(v)
			if m == nil {
				return nil
			}
			a, ok := m["input_modalities"].([]any)
			if !ok || len(a) == 0 || wire(m["output_modalities"]) != `["text"]` {
				return nil
			}
			seen := map[string]bool{}
			for _, v := range a {
				value := str(v)
				if !slices.Contains([]string{"text", "image", "file"}, value) || seen[value] {
					return nil
				}
				seen[value] = true
			}
			if !seen["text"] {
				return nil
			}
			if _, ok := m["tool_calling"].(bool); !ok {
				return nil
			}
			out["capabilities"] = fields(m, "input_modalities", "output_modalities", "tool_calling")
		}
		if !readArray("nodes", !partial, func(m object) object {
			if !name(m) || !slices.Contains([]string{"api_key", "none"}, str(obj(m["credentials"])["mode"])) {
				return nil
			}
			if v, ok := m["enabled"]; ok {
				if _, ok := v.(bool); !ok {
					return nil
				}
			}
			if v, ok := m["route_weight"]; ok {
				if _, ok := exactInt(v, maxSafe, 1); !ok {
					return nil
				}
			}
			for _, k := range []string{"display_name", "provider", "model", "api", "base_url"} {
				if v, ok := m[k]; ok {
					if _, ok := v.(string); !ok {
						return nil
					}
				}
			}
			p := fields(m, "name", "display_name", "enabled", "route_weight", "provider", "model", "api", "base_url")
			p["credentials"] = fields(obj(m["credentials"]), "mode")
			return p
		}) {
			return nil
		}
	case "tool":
		out = fields(s, "display_name", "description", "enabled", "current_contract")
		if !readArray("contracts", !partial, func(m object) object {
			if !contractRE.MatchString(str(m["name"])) || !slices.Contains([]string{"client", "server_webhook"}, str(m["type"])) || obj(m["input_schema"]) == nil || obj(m["output_schema"]) == nil {
				return nil
			}
			p := fields(m, "name", "description", "type", "input_schema", "output_schema")
			if w, ok := m["webhook"]; ok {
				m := obj(w)
				if m == nil || !slices.Contains([]string{"value", "env"}, str(m["url_source"])) || !slices.Contains([]string{"none", "bearer_token"}, str(m["auth"])) {
					return nil
				}
				if _, ok := exactInt(m["timeout_seconds"], 30, 1); !ok {
					return nil
				}
				p["webhook"] = fields(m, "url_source", "auth", "timeout_seconds", "invocation_name")
				if m["url_source"] == "value" {
					if _, ok := m["url"].(string); !ok {
						return nil
					}
					obj(p["webhook"])["url"] = m["url"]
				}
			}
			return p
		}) {
			return nil
		}
	case "agent":
		out = fields(s, "display_name", "description", "instruction")
		if !readArray("models", !partial, func(m object) object {
			if !name(m) {
				return nil
			}
			return fields(m, "name", "contract")
		}) || !readArray("tools", !partial, func(m object) object {
			if !name(m) {
				return nil
			}
			if v, ok := m["contract"]; ok && !contractRE.MatchString(str(v)) {
				return nil
			}
			return fields(m, "name", "contract")
		}) || !readArray("policies", false, func(m object) object {
			if !name(m) || obj(m["models"]) == nil || obj(m["tools"]) == nil {
				return nil
			}
			safe := fields(m, "name", "display_name", "description", "enabled")
			for _, field := range []string{"models", "tools"} {
				f := obj(m[field])
				clean := object{}
				for _, k := range []string{"default", "allowed", "denied"} {
					if v, ok := f[k]; ok {
						if k == "default" {
							clean[k] = fields(obj(v), "name")
						} else {
							a := []any{}
							for _, r := range objects(v) {
								a = append(a, fields(r, "name"))
							}
							clean[k] = a
						}
					}
				}
				safe[field] = clean
			}
			return safe
		}) {
			return nil
		}
	}
	return out
}
