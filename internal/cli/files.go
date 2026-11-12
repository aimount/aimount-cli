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
func reference(v any, path string) object {
	if s, ok := v.(string); ok {
		technical(s, path, false)
		return object{"name": s}
	}
	m := strictObject(v, []string{"name", "contract"}, []string{"name"}, path)
	technical(m["name"], path, false)
	if c, ok := m["contract"]; ok {
		technical(c, path, true)
	}
	return maps.Clone(m)
}

var periodRE = regexp.MustCompile(`^[1-9][0-9]{0,6}[smhd]$`)

// periodValid accepts a positive s/m/h/d sliding window of at most 30 days.
func periodValid(v any) bool {
	s, ok := v.(string)
	if !ok || !periodRE.MatchString(s) {
		return false
	}
	n, _ := strconv.ParseInt(s[:len(s)-1], 10, 64)
	return n*map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}[s[len(s)-1]] <= 30*86400
}

// resolve mirrors the API resolveDefault: explicit, then inherited if in set, then the sole key.
func resolve(explicit, inherited string, set []string) (string, bool) {
	switch {
	case explicit != "":
		return explicit, slices.Contains(set, explicit)
	case inherited != "" && slices.Contains(set, inherited):
		return inherited, true
	case len(set) == 1:
		return set[0], true
	}
	return "", false
}

// applicationSpec strictly parses authored Application configuration into the
// Stack shape with name-only references. Local key membership and effective
// defaults are checked as in the API domain; Project resource existence,
// lifecycle and Plan Tool membership stay with the API planner.
func applicationSpec(v any, path string) object {
	at := func(v any, allowed, required, field string) object {
		if caught(func() { strictObject(v, strings.Fields(allowed), strings.Fields(required), path) }) != nil {
			fail(path, field+": invalid or unknown fields")
		}
		return obj(v)
	}
	name := func(v any, field string) string {
		n, ok := v.(string)
		if !ok || !nameRE.MatchString(n) {
			fail(path, field+": expected a technical name")
		}
		return n
	}
	dict := func(v any, field string) object {
		m := obj(v)
		if m == nil || len(m) > 100 {
			fail(path, field+": expected a mapping of at most 100 local keys")
		}
		for k := range m {
			if !nameRE.MatchString(k) {
				fail(path, field+": invalid local key")
			}
		}
		return m
	}
	// set returns sorted unique names; local keys must be in known, references are checked by the API.
	set := func(v any, field string, minimum, maximum int, known []string, local bool) []string {
		a, ok := v.([]any)
		if !ok || len(a) < minimum || len(a) > maximum {
			fail(path, field+": expected "+strconv.Itoa(minimum)+" to "+strconv.Itoa(maximum)+" items")
		}
		names := []string{}
		for _, x := range a {
			n := name(x, field)
			if slices.Contains(names, n) {
				fail(path, field+": duplicate "+n)
			}
			if local && !slices.Contains(known, n) {
				fail(path, field+": unknown local key "+n)
			}
			names = append(names, n)
		}
		sort.Strings(names)
		return names
	}
	// wired converts names to the []any form export validation reads.
	wired := func(names []string) []any {
		out := []any{}
		for _, n := range names {
			out = append(out, n)
		}
		return out
	}
	refs := func(v any, field string, maximum int) []any {
		out := []any{}
		for _, n := range set(v, field, 0, maximum, nil, false) {
			out = append(out, object{"name": n})
		}
		return out
	}
	var models object
	member := func(v any, field string, known []string) string {
		n := name(v, field)
		if !slices.Contains(known, n) {
			if _, model := models[n]; model && strings.HasSuffix(field, ".default_model") && field != "spec.default_model" {
				fail(path, field+": "+n+" is not in the local Agent's models")
			}
			fail(path, field+": unknown local key "+n)
		}
		return n
	}
	if _, ok := obj(v)["agents"].([]any); ok {
		fail(path, "spec.agents: Application Agent reference arrays are unsupported; declare a mapping of local Agent keys")
	}
	s := at(v, "display_name models default_model agents default_plan plans", "display_name models agents", "spec")
	display(s, path)
	out := object{"display_name": s["display_name"]}
	models = dict(s["models"], "spec.models")
	modelKeys := slices.Sorted(maps.Keys(models))
	bindings := object{}
	for _, k := range modelKeys {
		b := models[k]
		bindings[k] = object{"model": object{"name": name(at(b, "model", "model", "spec.models."+k)["model"], "spec.models."+k+".model")}}
	}
	out["models"] = bindings
	explicit := ""
	if v, ok := s["default_model"]; ok {
		explicit = member(v, "spec.default_model", modelKeys)
		out["default_model"] = explicit
	}
	applicationDefault, valid := resolve(explicit, "", modelKeys)
	if !valid && len(modelKeys) >= 2 {
		fail(path, "spec.default_model: required with two or more Models")
	}
	agents := dict(s["agents"], "spec.agents")
	agentKeys := slices.Sorted(maps.Keys(agents))
	ceilings := map[string][]string{}
	defaults := map[string]string{}
	locals := object{}
	for _, k := range agentKeys {
		v := agents[k]
		f := "spec.agents." + k
		a := at(v, "agent models default_model storages", "agent", f)
		local := object{"agent": object{"name": name(a["agent"], f+".agent")}}
		ceilings[k] = modelKeys
		if v, ok := a["models"]; ok {
			ceilings[k] = set(v, f+".models", 1, 100, modelKeys, true)
			local["models"] = wired(ceilings[k])
		}
		if len(ceilings[k]) == 0 {
			fail(path, f+": requires at least one Application Model")
		}
		explicit := ""
		if v, ok := a["default_model"]; ok {
			explicit = member(v, f+".default_model", ceilings[k])
			local["default_model"] = explicit
		}
		d, valid := resolve(explicit, applicationDefault, ceilings[k])
		if !valid {
			fail(path, f+".default_model: required because spec.default_model is not in this local Agent's models")
		}
		defaults[k] = d
		if v, ok := a["storages"]; ok {
			local["storages"] = refs(v, f+".storages", 32)
		}
		locals[k] = local
	}
	out["agents"] = locals
	plans := object{}
	if v, ok := s["plans"]; ok {
		plans = dict(v, "spec.plans")
		result := object{}
		for _, pk := range slices.Sorted(maps.Keys(plans)) {
			pv := plans[pk]
			f := "spec.plans." + pk
			p := at(pv, "display_name agents limits", "", f)
			plan := object{}
			if v, ok := p["display_name"]; ok {
				display(object{"display_name": v}, path)
				plan["display_name"] = v
			}
			if v, ok := p["agents"]; ok {
				rules := object{}
				given := dict(v, f+".agents")
				for _, ak := range slices.Sorted(maps.Keys(given)) {
					rv := given[ak]
					rf := f + ".agents." + ak
					ceiling, ok := ceilings[ak]
					if !ok {
						fail(path, rf+": unknown local Agent key")
					}
					r := at(rv, "input models default_model tools", "", rf)
					rule := object{}
					if v, ok := r["input"]; ok {
						if _, ok := v.(bool); !ok {
							fail(path, rf+".input: expected boolean")
						}
						rule["input"] = v
					}
					effective := ceiling
					if v, ok := r["models"]; ok {
						effective = set(at(v, "allowed", "allowed", rf+".models")["allowed"], rf+".models.allowed", 1, 100, ceiling, true)
						rule["models"] = object{"allowed": wired(effective)}
					}
					explicit := ""
					if v, ok := r["default_model"]; ok {
						explicit = member(v, rf+".default_model", ceiling)
						rule["default_model"] = explicit
					}
					_, valid := resolve(explicit, defaults[ak], effective)
					// An excluded inherited default needs an explicit replacement, not the sole allowed key.
					if explicit == "" && !slices.Contains(effective, defaults[ak]) {
						valid = false
					}
					if !valid && r["input"] != false {
						if explicit != "" {
							fail(path, rf+".default_model: "+explicit+" is not in models.allowed")
						}
						fail(path, rf+".default_model: required because inherited default "+defaults[ak]+" is not in models.allowed")
					}
					if v, ok := r["tools"]; ok {
						rule["tools"] = object{"allowed": refs(at(v, "allowed", "allowed", rf+".tools")["allowed"], rf+".tools.allowed", 100)}
					}
					rules[ak] = rule
				}
				plan["agents"] = rules
			}
			if v, ok := p["limits"]; ok {
				a, ok := v.([]any)
				if !ok || len(a) > 100 {
					fail(path, f+".limits: expected at most 100 items")
				}
				limits := []any{}
				for i, l := range a {
					lf := f + ".limits[" + strconv.Itoa(i) + "]"
					m := at(l, "agents requests period", "agents requests period", lf)
					requests, ok := exactInt(m["requests"], maxSafe, 1)
					if !ok {
						fail(path, lf+".requests: expected a positive integer")
					}
					if !periodValid(m["period"]) {
						fail(path, lf+".period: expected a positive s/m/h/d duration of at most 30d")
					}
					limits = append(limits, object{"agents": wired(set(m["agents"], lf+".agents", 1, 100, agentKeys, true)), "requests": requests, "period": m["period"]})
				}
				plan["limits"] = limits
			}
			result[pk] = plan
		}
		out["plans"] = result
	}
	if v, ok := s["default_plan"]; ok {
		out["default_plan"] = member(v, "spec.default_plan", slices.Collect(maps.Keys(plans)))
	} else if len(plans) >= 2 {
		fail(path, "spec.default_plan: required with two or more Plans")
	}
	if len(wire(out)) > 256<<10 {
		fail(path, "configuration exceeds 256 KiB", "invariant")
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
		if kind == "tool_contract" {
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
		parent := strings.TrimSuffix(dir, "s")
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
			if dir == "agents" {
				if slices.Contains(children, "policies") {
					fail("agents/"+folder+"/policies", "Agent Policies are unsupported; remove this directory and declare Plans in applications/*.yaml", "discovery")
				}
				if slices.Contains(children, "instruction.md") {
					i.Managed = append(i.Managed, dir+"/"+folder+"/instruction.md")
				}
			} else {
				for _, f := range entries(filepath.Join(p, "contracts")) {
					if strings.HasSuffix(f, ".yaml") {
						files = append(files, f)
						rel := dir + "/" + folder + "/contracts/" + f
						i.Managed = append(i.Managed, rel)
						i.definitions = append(i.definitions, definition{rel, "tool_contract"})
					}
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
					if slices.Contains(p.children, "instruction.md") {
						fail("agents/"+p.folder, "orphan Agent files", "discovery")
					}
					return
				}
				path := "agents/" + p.folder + "/agent.yaml"
				a := parent(parsed[path], "agent", path)
				if _, ok := obj(a["spec"])["models"]; ok {
					fail(path, "spec.models is unsupported; select Models in Application local Agents")
				}
				s := strictObject(a["spec"], strings.Fields("display_name description tools"), strings.Fields("display_name tools"), path)
				display(s, path)
				refs := []any{}
				seen := map[string]bool{}
				for _, v := range collection(s["tools"], path, 0) {
					r := reference(v, path)
					name := str(r["name"])
					if seen[name] {
						fail(path, "duplicate reference")
					}
					seen[name] = true
					refs = append(refs, r)
				}
				s["tools"] = refs
				ip := "agents/" + p.folder + "/instruction.md"
				instruction := safeText(root, filepath.Join(root, ip))
				text(instruction, 128<<10, ip, true)
				s["instruction"] = instruction
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
				a["spec"] = applicationSpec(a["spec"], path)
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

// keyed is a rendered local-key dictionary: always sorted, never field-ordered.
type keyed map[string]any

// YAML nodes retain exact schema tokens and canonical domain field order.
func yamlNode(v any) *yaml.Node {
	switch x := v.(type) {
	case keyed:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			n.Content = append(n.Content, yamlNode(k), yamlNode(x[k]))
		}
		return n
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
	case m["capabilities"] != nil:
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
	case m["models"] != nil && m["agents"] != nil:
		order = strings.Fields("display_name models default_model agents default_plan plans")
	case m["agent"] != nil:
		order = strings.Fields("agent models default_model storages")
	case m["requests"] != nil:
		order = strings.Fields("agents requests period")
	case m["agents"] != nil || m["limits"] != nil:
		order = strings.Fields("display_name agents limits")
	case m["display_name"] != nil && m["tools"] != nil:
		order = strings.Fields("display_name description tools")
	case m["input"] != nil || m["models"] != nil || m["default_model"] != nil || m["tools"] != nil:
		order = strings.Fields("input models default_model tools")
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
			files["applications/"+name+".yaml"] = yamlString(authored("application", name, a["archived"], authoredApplication(obj(a["spec"]))))
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
			tools := []any{}
			for _, r := range objects(s["tools"]) {
				if r["contract"] != nil {
					tools = append(tools, fields(r, "name", "contract"))
				} else {
					tools = append(tools, r["name"])
				}
			}
			out["tools"] = tools
			files["agents/"+name+"/agent.yaml"] = yamlString(authored("agent", name, a["archived"], out))
			instruction, ok := s["instruction"].(string)
			if !ok {
				fail("export", "invalid instruction", "remote")
			}
			files["agents/"+name+"/instruction.md"] = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(instruction, "\r\n", "\n"), "\r", "\n"), "\n") + "\n"
		}
	})
	return
}

// authoredApplication renders references as names; omitted optional fields stay omitted.
func authoredApplication(s object) object {
	names := func(v any) []any {
		out := []any{}
		for _, r := range objects(v) {
			out = append(out, r["name"])
		}
		return out
	}
	out := fields(s, "display_name", "default_model", "default_plan")
	models := keyed{}
	for k, b := range obj(s["models"]) {
		models[k] = object{"model": obj(obj(b)["model"])["name"]}
	}
	out["models"] = models
	agents := keyed{}
	for k, v := range obj(s["agents"]) {
		a := obj(v)
		local := fields(a, "models", "default_model")
		local["agent"] = obj(a["agent"])["name"]
		if v, ok := a["storages"]; ok {
			local["storages"] = names(v)
		}
		agents[k] = local
	}
	out["agents"] = agents
	if v, ok := s["plans"]; ok {
		plans := keyed{}
		for k, v := range obj(v) {
			p := obj(v)
			plan := fields(p, "display_name", "limits")
			if v, ok := p["agents"]; ok {
				rules := keyed{}
				for k, v := range obj(v) {
					r := obj(v)
					rule := fields(r, "input", "models", "default_model")
					if v, ok := r["tools"]; ok {
						rule["tools"] = object{"allowed": names(obj(v)["allowed"])}
					}
					rules[k] = rule
				}
				plan["agents"] = rules
			}
			plans[k] = plan
		}
		out["plans"] = plans
	}
	return out
}

// remoteApplication projects consumed Application fields, dropping IDs and additions.
func remoteApplication(s object, partial bool) object {
	bad := func() { fail("export", "invalid exported Application", "remote") }
	present := func(m object, k string, required bool) bool {
		_, ok := m[k]
		if !ok && required {
			bad()
		}
		return ok
	}
	key := func(v any) string {
		k, ok := v.(string)
		if !ok || !nameRE.MatchString(k) {
			bad()
		}
		return k
	}
	ref := func(v any) any { return object{"name": key(obj(v)["name"])} }
	byWire := func(a []any) { sort.Slice(a, func(i, j int) bool { return wire(a[i]) < wire(a[j]) }) }
	set := func(v any, minimum, maximum int, read func(any) any) []any {
		a, ok := v.([]any)
		if !ok || len(a) < minimum || len(a) > maximum {
			bad()
		}
		out := []any{}
		for _, x := range a {
			r := read(x)
			if slices.ContainsFunc(out, func(y any) bool { return wire(y) == wire(r) }) {
				bad()
			}
			out = append(out, r)
		}
		byWire(out)
		return out
	}
	keys := func(v any) []any { return set(v, 1, 100, func(x any) any { return key(x) }) }
	dict := func(v any, read func(object) object) object {
		m := obj(v)
		if m == nil || len(m) > 100 {
			bad()
		}
		out := object{}
		for k, x := range m {
			key(k)
			if obj(x) == nil {
				bad()
			}
			out[k] = read(obj(x))
		}
		return out
	}
	displayName := func(m, out object) {
		if v, ok := m["display_name"]; ok {
			if caught(func() { display(object{"display_name": v}, "export") }) != nil {
				bad()
			}
			out["display_name"] = v
		}
	}
	out := object{}
	present(s, "display_name", !partial)
	displayName(s, out)
	for _, k := range []string{"default_model", "default_plan"} {
		if v, ok := s[k]; ok {
			out[k] = key(v)
		}
	}
	if present(s, "models", !partial) {
		out["models"] = dict(s["models"], func(b object) object { return object{"model": ref(b["model"])} })
	}
	if present(s, "agents", !partial) {
		out["agents"] = dict(s["agents"], func(a object) object {
			local := object{"agent": ref(a["agent"])}
			if v, ok := a["models"]; ok {
				local["models"] = keys(v)
			}
			if v, ok := a["default_model"]; ok {
				local["default_model"] = key(v)
			}
			if v, ok := a["storages"]; ok {
				local["storages"] = set(v, 0, 32, ref)
			}
			return local
		})
	}
	if v, ok := s["plans"]; ok {
		out["plans"] = dict(v, func(p object) object {
			plan := object{}
			displayName(p, plan)
			if v, ok := p["agents"]; ok {
				plan["agents"] = dict(v, func(r object) object {
					rule := object{}
					if v, ok := r["input"]; ok {
						if _, ok := v.(bool); !ok {
							bad()
						}
						rule["input"] = v
					}
					if v, ok := r["models"]; ok {
						rule["models"] = object{"allowed": keys(obj(v)["allowed"])}
					}
					if v, ok := r["default_model"]; ok {
						rule["default_model"] = key(v)
					}
					if v, ok := r["tools"]; ok {
						rule["tools"] = object{"allowed": set(obj(v)["allowed"], 0, 100, ref)}
					}
					return rule
				})
			}
			if v, ok := p["limits"]; ok {
				a, ok := v.([]any)
				if !ok || len(a) > 100 {
					bad()
				}
				limits := []any{}
				for _, x := range a {
					l := obj(x)
					if _, ok := exactInt(l["requests"], maxSafe, 1); !ok || !periodValid(l["period"]) {
						bad()
					}
					limits = append(limits, object{"agents": keys(l["agents"]), "requests": l["requests"], "period": l["period"]})
				}
				// Limits have no identity or order; sorting makes a reorder no change.
				byWire(limits)
				plan["limits"] = limits
			}
			return plan
		})
	}
	return out
}

// readRemoteSpec validates consumed response fields and returns only safe fields.
// A nil result is invalid; an empty object is a valid partial spec.
func readRemoteSpec(kind string, s object, partial bool) object {
	if s == nil {
		return nil
	}
	if kind == "application" {
		var out object
		if caught(func() { out = remoteApplication(s, partial) }) != nil {
			return nil
		}
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
		if !readArray("tools", !partial, func(m object) object {
			if !name(m) {
				return nil
			}
			if v, ok := m["contract"]; ok && !contractRE.MatchString(str(v)) {
				return nil
			}
			return fields(m, "name", "contract")
		}) {
			return nil
		}
	}
	return out
}
