package cli

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

func differences(before, after any, path string) []string {
	if wire(before) == wire(after) {
		return nil
	}
	a, aok := before.([]any)
	b, bok := after.([]any)
	if aok && bok {
		if path == "tools" || strings.HasSuffix(path, ".allowed") || strings.HasSuffix(path, ".denied") {
			key := func(v any) string {
				if s, ok := v.(string); ok {
					return s
				}
				r := obj(v)
				if r["contract"] != nil {
					return str(r["name"]) + "@" + str(r["contract"])
				}
				return str(r["name"])
			}
			left, right := []string{}, []string{}
			for _, v := range a {
				left = append(left, key(v))
			}
			for _, v := range b {
				right = append(right, key(v))
			}
			added, removed := []string{}, []string{}
			for _, v := range right {
				if !slices.Contains(left, v) {
					added = append(added, v)
				}
			}
			for _, v := range left {
				if !slices.Contains(right, v) {
					removed = append(removed, v)
				}
			}
			return []string{literal(path) + ": +" + wire(added) + " -" + wire(removed)}
		}
	}
	if aok || bok {
		return []string{literal(path) + ": " + wire(before) + " → " + wire(after)}
	}
	if obj(before) != nil && obj(after) != nil {
		names := object{}
		for k := range obj(before) {
			names[k] = true
		}
		for k := range obj(after) {
			names[k] = true
		}
		lines := []string{}
		for _, k := range slices.Sorted(maps.Keys(names)) {
			p := k
			if path != "" {
				p = path + "." + k
			}
			lines = append(lines, differences(obj(before)[k], obj(after)[k], p)...)
		}
		return lines
	}
	if strings.HasSuffix(path, "instruction") {
		left, right := str(before), str(after)
		ls, rs := strings.Split(left, "\n"), strings.Split(right, "\n")
		var out strings.Builder
		fmt.Fprintf(&out, "%s:\n--- saved\n+++ desired\n@@ -1,%d +1,%d @@\n", literal(path), len(ls), len(rs))
		for _, l := range ls {
			fmt.Fprintln(&out, "-"+literal(l))
		}
		for i, l := range rs {
			out.WriteString("+" + literal(l))
			if i+1 < len(rs) {
				out.WriteByte('\n')
			}
		}
		return []string{out.String()}
	}
	return []string{literal(path) + ": " + wire(before) + " → " + wire(after)}
}
func semantic(plan object) string {
	ordered := objects(plan["changes"])
	rank := map[string]int{"model": 0, "tool": 1, "agent": 2}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if rank[str(a["kind"])] != rank[str(b["kind"])] {
			return rank[str(a["kind"])] < rank[str(b["kind"])]
		}
		if a["name"] != b["name"] {
			return str(a["name"]) < str(b["name"])
		}
		return str(a["operation"]) < str(b["operation"])
	})
	lines := []string{}
	for _, group := range []struct{ kind, field, label string }{{"model", "nodes", "Node"}, {"tool", "contracts", "Tool Contract"}, {"agent", "policies", "Policy"}} {
		changes := []object{}
		for _, c := range ordered {
			if c["kind"] == group.kind && c["operation"] != "ROTATE" {
				changes = append(changes, c)
			}
		}
		for _, c := range changes {
			line := literal(c["operation"]) + " " + literal(c["kind"]) + " " + literal(c["name"])
			if c["runtime_cascade"] == true {
				line += " — IRREVERSIBLE conversations/Files/private-test erasure intent (physical cleanup asynchronous)"
			}
			lines = append(lines, line)
			if c["operation"] == "CREATE" {
				lifecycle := "active"
				if c["archived"] == true {
					lifecycle = "archived"
				}
				lines = append(lines, "  initial lifecycle: "+lifecycle)
			}
			before, after := obj(c["before"]), obj(c["after"])
			names := object{}
			for k := range before {
				names[k] = true
			}
			for k := range after {
				names[k] = true
			}
			for _, k := range slices.Sorted(maps.Keys(names)) {
				if k != group.field {
					for _, line := range differences(before[k], after[k], k) {
						lines = append(lines, "  "+line)
					}
				}
			}
		}
		type member struct {
			c    object
			name string
		}
		members := []member{}
		for _, c := range changes {
			seen := map[string]bool{}
			for _, v := range append(objects(obj(c["before"])[group.field]), objects(obj(c["after"])[group.field])...) {
				seen[str(v["name"])] = true
			}
			for n := range seen {
				members = append(members, member{c, n})
			}
		}
		sort.Slice(members, func(i, j int) bool {
			a, b := members[i], members[j]
			if a.c["name"] != b.c["name"] {
				return str(a.c["name"]) < str(b.c["name"])
			}
			if a.name != b.name {
				return a.name < b.name
			}
			return str(a.c["operation"]) < str(b.c["operation"])
		})
		for _, m := range members {
			var left, right any
			for _, v := range objects(obj(m.c["before"])[group.field]) {
				if v["name"] == m.name {
					left = v
				}
			}
			for _, v := range objects(obj(m.c["after"])[group.field]) {
				if v["name"] == m.name {
					right = v
				}
			}
			if wire(left) == wire(right) {
				continue
			}
			op := "UPDATE"
			if left == nil {
				op = "CREATE"
			} else if right == nil {
				op = "REMOVE"
			}
			lines = append(lines, group.label+" "+literal(m.c["name"])+"/"+literal(m.name)+" "+op)
			for _, line := range differences(left, right, group.field+"."+m.name) {
				lines = append(lines, "  "+line)
			}
		}
	}
	type credential struct{ c, r object }
	reviews := []credential{}
	for _, c := range ordered {
		for _, r := range objects(c["credential_review"]) {
			reviews = append(reviews, credential{c, r})
		}
	}
	sort.Slice(reviews, func(i, j int) bool {
		a, b := reviews[i], reviews[j]
		ka := str(a.c["kind"]) + "/" + str(a.c["name"]) + "/" + str(a.r["target"]) + "/" + str(a.r["action"])
		kb := str(b.c["kind"]) + "/" + str(b.c["name"]) + "/" + str(b.r["target"]) + "/" + str(b.r["action"])
		return ka < kb
	})
	for _, v := range reviews {
		lines = append(lines, "credential "+literal(v.r["action"])+" "+literal(v.c["kind"])+" "+literal(v.c["name"])+"/"+literal(v.r["target"])+" (expected version "+literal(v.r["expected_version"])+")")
	}
	return strings.Join(lines, "\n") + "\n"
}
