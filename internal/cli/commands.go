package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Command  string
	Value    any
	Warnings []string
}
type runner struct {
	ctx    context.Context
	in     io.Reader
	errOut io.Writer
	env    map[string]string
	input  *bufio.Reader
}

func (r *runner) ask(message string, o Options) (string, error) {
	if o.NonInteractive {
		return "", commandError("confirmation_required", message, 3)
	}
	fmt.Fprint(r.errOut, message+" ")
	if r.input == nil {
		r.input = bufio.NewReader(r.in)
	}
	type answer struct {
		s string
		e error
	}
	ready := make(chan answer, 1)
	go func() { s, e := r.input.ReadString('\n'); ready <- answer{s, e} }()
	var s string
	var e error
	select {
	case a := <-ready:
		s, e = a.s, a.e
	case <-r.ctx.Done():
		return "", commandError("cancelled", "Cancelled", 3)
	}
	if e != nil && e != io.EOF {
		return "", commandError("cancelled", "Cancelled", 3)
	}
	if r.ctx.Err() != nil {
		return "", commandError("cancelled", "Cancelled", 3)
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), nil
}
func (r *runner) confirm(message string, o Options, approved bool) error {
	if approved {
		return nil
	}
	s, e := r.ask(message+" [y/N]", o)
	if e != nil {
		return e
	}
	if strings.ToLower(s) != "y" {
		return commandError("cancelled", "Cancelled", 3)
	}
	return nil
}
func (r *runner) selectItem(items []any, message string, o Options, single bool) (object, error) {
	if len(items) == 0 {
		return nil, nil
	}
	for _, v := range items {
		m := obj(v)
		if m == nil || str(m["id"]) == "" {
			return nil, &APIError{Status: 200, Code: "invalid_response"}
		}
	}
	if single && len(items) == 1 {
		return obj(items[0]), nil
	}
	if o.NonInteractive {
		return nil, commandError("selection_required", message, 3)
	}
	for i, v := range items {
		m := obj(v)
		label := m["display_name"]
		if label == nil {
			label = m["name"]
		}
		if label == nil {
			label = m["id"]
		}
		fmt.Fprintf(r.errOut, "%d. %s (%s)\n", i+1, wire(label), wire(m["id"]))
	}
	s, e := r.ask(message+" (empty cancels):", o)
	if e != nil {
		return nil, e
	}
	if s == "" {
		return nil, commandError("cancelled", "Selection cancelled; authentication is retained", 3)
	}
	i, e := strconv.Atoi(s)
	if e != nil || i < 1 || i > len(items) {
		return nil, commandError("selection_invalid", "Invalid selection", 1)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, commandError("selection_invalid", "Invalid selection", 1)
		}
	}
	return obj(items[i-1]), nil
}

// Export uses only primary names. Validate its staged graph before local publication.
func validateExportGraph(desired graph) error {
	included := func(kind, name string) object {
		for _, r := range desired.group(kind) {
			if r["name"] == name {
				return r
			}
		}
		return nil
	}
	for _, kind := range []string{"model", "tool", "agent", "application"} {
		seen := map[string]bool{}
		for _, r := range desired.group(kind) {
			id := str(r["name"])
			if seen[id] {
				return commandError("duplicate_identity", "Several definitions resolve to one resource", 1)
			}
			seen[id] = true
		}
	}
	for _, a := range desired.Applications {
		seen := map[string]bool{}
		for _, ref := range objects(obj(a["spec"])["agents"]) {
			name := str(ref["name"])
			agent := included("agent", name)
			if agent == nil {
				return commandError("incomplete_graph", "Application "+str(a["name"])+": Agent "+name+" missing locally", 1)
			}
			identity := name
			if seen[identity] {
				return commandError("duplicate_identity", "Application "+str(a["name"])+": several references resolve to one Agent", 1)
			}
			seen[identity] = true
			if a["archived"] != true && agent["archived"] == true {
				return commandError("invalid_lifecycle", "Application "+str(a["name"])+": active Application requires active Agents", 1)
			}
		}
	}
	for _, a := range desired.Agents {
		s := obj(a["spec"])
		for _, r := range objects(s["models"]) {
			if included("model", str(r["name"])) == nil {
				return commandError("incomplete_graph", "Agent "+str(a["name"])+": Model "+str(r["name"])+" missing locally", 1)
			}
		}
		for _, r := range objects(s["tools"]) {
			t := included("tool", str(r["name"]))
			if t == nil {
				return commandError("incomplete_graph", "Agent "+str(a["name"])+": Tool/Contract missing locally", 1)
			}
			if r["contract"] != nil {
				found := false
				for _, c := range objects(obj(t["spec"])["contracts"]) {
					found = found || c["name"] == r["contract"]
				}
				if !found {
					return commandError("incomplete_graph", "Agent "+str(a["name"])+": Tool/Contract missing locally", 1)
				}
			}
		}
		for _, p := range objects(s["policies"]) {
			models := obj(p["models"])
			match := func(name string, rs []object) bool {
				for _, r := range rs {
					if r["name"] == name {
						return true
					}
				}
				return false
			}
			effective := []object{}
			for _, m := range objects(s["models"]) {
				_, allowed := models["allowed"]
				if (!allowed || match(str(m["name"]), objects(models["allowed"]))) && !match(str(m["name"]), objects(models["denied"])) {
					effective = append(effective, m)
				}
			}
			if len(effective) == 0 || !match(str(obj(models["default"])["name"]), effective) {
				return commandError("invalid_policy", "Agent "+str(a["name"])+": invalid Policy Model selection", 1)
			}
			for _, kind := range []string{"model", "tool"} {
				for _, group := range []string{"allowed", "denied"} {
					for _, ref := range objects(obj(p[kind+"s"])[group]) {
						if !match(str(ref["name"]), objects(s[kind+"s"])) {
							return commandError("invalid_policy", "Agent "+str(a["name"])+": Policy reference outside ceiling", 1)
						}
					}
				}
			}
		}
	}
	return nil
}
func pullLocators(g graph, locs map[string]string, bindings []object) (map[string]string, error) {
	bad := func() (map[string]string, error) {
		return nil, commandError("stack_binding_conflict", "Uncertain or changed locator parent correspondence; retry explicitly", 2)
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		k := str(b["kind"]) + "/" + str(b["name"])
		if seen[k] {
			return bad()
		}
		seen[k] = true
	}
	out := map[string]string{}
	for key, v := range locs {
		parts := strings.Split(key, "/")
		if len(parts) != 4 {
			return bad()
		}
		kind, alias, target, purpose := parts[0], parts[1], parts[2], parts[3]
		id := ""
		for _, b := range bindings {
			if b["kind"] == kind && b["name"] == alias {
				id = str(b["id"])
				break
			}
		}
		if id == "" {
			continue
		}
		parents := []string{}
		for _, r := range g.group(kind) {
			for _, b := range bindings {
				if b["kind"] == kind && b["name"] == r["name"] && b["id"] == id {
					parents = append(parents, str(r["name"]))
				}
			}
		}
		if len(parents) != 1 {
			return bad()
		}
		k := kind + "/" + parents[0] + "/" + target + "/" + purpose
		if prior, ok := out[k]; ok && prior != v {
			return bad()
		}
		out[k] = v
	}
	return out, nil
}

func (r *runner) run(o Options) (Result, error) {
	result := Result{Command: o.Command, Warnings: []string{}}
	finish := func(command string, v any) (Result, error) {
		result.Command = command
		result.Value = v
		return result, nil
	}
	bad := func(code, message string) (Result, error) { return result, commandError(code, message, 1) }
	if o.Command == "unlink" {
		e := os.Remove(filepath.Join(o.Root, ".aimount/project.json"))
		if e != nil && !os.IsNotExist(e) {
			return result, errors.New("cannot remove Project binding")
		}
		return finish("unlink", object{"unlinked": true})
	}
	directory, e := globalDirectory()
	if e != nil {
		return result, e
	}
	configPath, authPath := filepath.Join(directory, "config.json"), filepath.Join(directory, "auth.json")
	config := object{}
	if !(o.API != "" && !slices.Contains([]string{"auth", "switch", "projects", "status", "link"}, o.Command)) {
		config, e = readState(configPath, "config")
		if e != nil {
			return result, e
		}
	}
	api := o.API
	if api == "" {
		value := "https://api.aimount.io"
		if v, ok := config["api_url"]; ok {
			value = str(v)
		}
		api, e = normalizeAPI(value)
		if e != nil {
			return result, e
		}
	}
	_, explicitToken := r.env["AIMOUNT_TOKEN"]
	auth := object{"tokens": object{}}
	logout := o.Command == "auth" && o.Args[0] == "logout"
	if !explicitToken || logout {
		auth, e = readState(authPath, "auth")
		if e != nil {
			return result, e
		}
	}
	if logout {
		if e = updateAuth(authPath, api, "", true); e != nil {
			return result, e
		}
		if explicitToken {
			result.Warnings = append(result.Warnings, "AIMOUNT_TOKEN remains selected; logout removed saved authorization only")
		}
		return finish("auth logout", object{"api": api, "logged_out": true})
	}
	source := "saved PAT"
	token := str(obj(auth["tokens"])[api])
	if explicitToken {
		source = "AIMOUNT_TOKEN"
		token = r.env["AIMOUNT_TOKEN"]
		if strings.TrimSpace(token) == "" {
			return bad("credential_invalid", "AIMOUNT_TOKEN is empty")
		}
	}
	client := newClient(api, token)
	defer client.http.CloseIdleConnections()
	var identity object
	if token != "" {
		identity, e = client.request(r.ctx, "GET", "/v0/auth/identity", "", "")
		if e != nil {
			a, ok := e.(*APIError)
			if !(o.Command == "auth" && o.Args[0] == "login" && source == "saved PAT" && ok && a.Status == 401) {
				return result, e
			}
			token = ""
		}
	}
	if o.Command == "auth" && o.Args[0] == "login" && token == "" {
		if o.NonInteractive {
			return bad("interactive_login_required", "Run auth login interactively or supply AIMOUNT_TOKEN")
		}
		token, e = deviceLogin(r.ctx, api, r.errOut)
		if e != nil {
			return result, e
		}
		if r.ctx.Err() != nil {
			return result, commandError("cancelled", "Device login cancelled; no local publication", 3)
		}
		client = newClient(api, token)
		defer client.http.CloseIdleConnections()
		identity, e = client.request(r.ctx, "GET", "/v0/auth/identity", "", "")
		if e != nil {
			return result, e
		}
		if r.ctx.Err() != nil {
			return result, commandError("cancelled", "Device login cancelled; no local publication", 3)
		}
		if e = updateAuthContext(r.ctx, authPath, api, token, false); e != nil {
			return result, e
		}
		orgs, e := client.pages(r.ctx, "/v0/organizations")
		if e != nil {
			return result, e
		}
		prior := obj(obj(config["contexts"])[api])
		var chosen object
		if prior["user_id"] == obj(identity["principal"])["user_id"] {
			for _, v := range orgs {
				if obj(v)["id"] == prior["organization_id"] {
					chosen = obj(v)
					break
				}
			}
		}
		if chosen == nil {
			chosen, e = r.selectItem(orgs, "Choose Organization", o, true)
			if e != nil {
				return result, e
			}
		}
		contexts := obj(config["contexts"])
		if contexts == nil {
			contexts = object{}
			config["contexts"] = contexts
		}
		context := object{"user_id": obj(identity["principal"])["user_id"]}
		if chosen != nil {
			context["organization_id"] = chosen["id"]
		}
		contexts[api] = context
		if e = atomicJSON(configPath, config); e != nil {
			return result, e
		}
		if chosen == nil {
			result.Warnings = append(result.Warnings, "Create an Organization or obtain access in Console, then run switch")
		}
		return finish("auth login", object{"api": api, "identity": identity, "authenticated": true})
	}
	if token == "" || str(obj(identity["principal"])["user_id"]) == "" {
		return bad("authentication_required", "Run auth login or supply AIMOUNT_TOKEN")
	}
	if o.Command == "auth" {
		return finish("auth "+o.Args[0], object{"api": api, "identity": identity, "credential_source": source})
	}
	organization := ""
	context := obj(obj(config["contexts"])[api])
	if context["user_id"] == obj(identity["principal"])["user_id"] {
		organization = str(context["organization_id"])
	}
	if o.Command == "switch" {
		var chosen object
		if o.Organization != "" {
			chosen, e = client.request(r.ctx, "GET", "/v0/organizations/"+o.Organization, "", "")
		} else {
			var items []any
			items, e = client.pages(r.ctx, "/v0/organizations")
			if e == nil {
				chosen, e = r.selectItem(items, "Choose Organization", o, false)
			}
		}
		if e != nil {
			return result, e
		}
		if chosen == nil {
			return bad("organization_required", "Create an Organization or obtain access in Console")
		}
		if !idValid(str(chosen["id"]), "org") {
			return result, &APIError{Status: 200, Code: "invalid_response"}
		}
		contexts := obj(config["contexts"])
		if contexts == nil {
			contexts = object{}
			config["contexts"] = contexts
		}
		contexts[api] = object{"user_id": obj(identity["principal"])["user_id"], "organization_id": chosen["id"]}
		config["api_url"] = api
		if e = atomicJSON(configPath, config); e != nil {
			return result, e
		}
		return finish("switch", object{"api": api, "organization": chosen})
	}
	if o.Command == "projects" {
		if organization == "" {
			return bad("organization_required", "Run switch, then projects list")
		}
		items, e := client.pages(r.ctx, "/v0/organizations/"+organization+"/projects")
		if e != nil {
			return result, e
		}
		return finish("projects list", object{"api": api, "organization_id": organization, "items": items})
	}
	bindingPath := filepath.Join(o.Root, ".aimount/project.json")
	binding := object{}
	if !(o.Project != "" && !slices.Contains([]string{"status", "link"}, o.Command)) {
		binding, e = readState(bindingPath, "binding")
		if e != nil {
			return result, e
		}
	}
	projectID := o.Project
	if projectID == "" {
		projectID = str(binding["project_id"])
	}
	if o.Command == "link" && o.Project == "" {
		if organization == "" {
			return bad("organization_required", "Run switch or link --project <id>")
		}
		items, e := client.pages(r.ctx, "/v0/organizations/"+organization+"/projects")
		if e != nil {
			return result, e
		}
		chosen, e := r.selectItem(items, "Choose active Project", o, false)
		if e != nil {
			return result, e
		}
		if chosen == nil {
			return bad("project_required", "Create a Project in Console")
		}
		projectID = str(chosen["id"])
	}
	if projectID == "" {
		if o.Command == "status" {
			return finish("status", object{"api": api, "identity": identity, "organization_id": nullable(organization), "saved_project": binding["project_id"], "project": nil})
		}
		return bad("project_required", "Use --project, AIMOUNT_PROJECT or link")
	}
	if !idValid(projectID, "prj") {
		return result, &APIError{Status: 200, Code: "invalid_response"}
	}
	project, e := client.request(r.ctx, "GET", "/v0/projects/"+projectID, "", "")
	if e != nil {
		return result, e
	}
	if project["status"] != "active" {
		return bad("project_unavailable", "Target Project is inactive")
	}
	projectSource := "local binding"
	if o.Project != "" {
		projectSource = "--project/AIMOUNT_PROJECT"
	}
	target := object{"api": api, "project": project, "prune": o.Prune, "project_source": projectSource, "credential_source": source}
	if o.Command == "status" {
		v := maps.Clone(target)
		v["identity"] = identity
		v["organization_id"] = nullable(organization)
		v["saved_project"] = binding["project_id"]
		return finish("status", v)
	}
	if o.Command == "link" {
		if binding["project_id"] != projectID {
			if binding["project_id"] != nil {
				if e = r.confirm("Replace Project "+wire(binding["project_id"])+" with "+wire(projectID)+"?", o, o.Yes); e != nil {
					return result, e
				}
			}
			if e = atomicJSON(bindingPath, object{"project_id": projectID}); e != nil {
				return result, e
			}
			ignore := filepath.Join(o.Root, ".gitignore")
			b, e := os.ReadFile(ignore)
			if e == nil || os.IsNotExist(e) {
				contents := string(b)
				if !slices.Contains(strings.Split(strings.ReplaceAll(contents, "\r\n", "\n"), "\n"), ".aimount/") {
					prefix := ""
					if contents != "" && !strings.HasSuffix(contents, "\n") {
						prefix = "\n"
					}
					f, err := os.OpenFile(ignore, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
					if err == nil {
						_, e = f.WriteString(prefix + ".aimount/\n")
						ce := f.Close()
						if e == nil {
							e = ce
						}
					} else {
						e = err
					}
				}
			}
			if e != nil && !os.IsNotExist(e) {
				result.Warnings = append(result.Warnings, ".gitignore could not be updated; binding was saved")
			}
		}
		return finish("link", target)
	}
	path := "/v0/projects/" + projectID + "/stack"
	if o.Command == "pull" {
		var baseline object
		files, e := replaceRoot(r.ctx, o.Root, func() (map[string]string, []string, error) {
			local, e := inspect(o.Root)
			if e != nil {
				return nil, nil, e
			}
			if len(local.Managed) > 0 && !o.Force {
				return nil, nil, commandError("force_required", "Existing managed files require --force", 1)
			}
			if len(local.Locators) > 0 {
				baseline, e = client.request(r.ctx, "GET", path+"/bindings", "", "")
				if e != nil {
					return nil, nil, e
				}
			}
			g, e := client.stack(r.ctx, projectID)
			if e != nil {
				return nil, nil, e
			}
			locs := local.Locators
			if baseline != nil {
				locs, e = pullLocators(g, local.Locators, objects(baseline["bindings"]))
				if e != nil {
					return nil, nil, e
				}
			}
			files, e := render(g, locs)
			return files, local.Managed, e
		}, func(stage string) error {
			staged, e := load(stage, nil, true)
			if e != nil {
				return e
			}
			if e := validateExportGraph(staged); e != nil {
				return e
			}
			if baseline != nil {
				fresh, e := client.request(r.ctx, "GET", path+"/bindings", "", "")
				if e != nil {
					return e
				}
				if wire(fresh["bindings"]) != wire(baseline["bindings"]) {
					return commandError("stack_binding_conflict", "Changed locator parent correspondence; retry explicitly", 2)
				}
			}
			return nil
		})
		if e != nil {
			return result, e
		}
		names := append([]string{}, slices.Sorted(maps.Keys(files))...)
		v := maps.Clone(target)
		v["files"] = names
		return finish("pull", v)
	}
	loaded, e := load(o.Root, r.env, false)
	if e != nil {
		return result, e
	}
	input := object{"graph": loaded, "prune": o.Prune}
	plan, e := client.request(r.ctx, "POST", path+"/plan", wire(input), "")
	if e != nil {
		return result, e
	}
	if o.Command == "plan" {
		return finish("plan", object{"target": target, "plan": plan})
	}
	if o.Command != "apply" {
		return bad("command_invalid", "Unsupported command")
	}
	if o.Output == "text" {
		fmt.Fprintf(r.errOut, "API: %s\nProject: %s (%s)\nWhole Project; prune=%t. Partial nonempty roots can remove remote resources.\n%s", textValue(api), textValue(str(project["id"])), textValue(str(project["display_name"])), o.Prune, semantic(plan))
	}
	if len(list(plan["changes"])) > 0 {
		if e = r.confirm("Apply this complete Project plan?", o, o.Approve); e != nil {
			return result, e
		}
	}
	serverTime, e := client.serverTime()
	if e != nil {
		return result, e
	}
	key := "s1." + strconv.FormatInt(serverTime, 10) + "." + uuid()
	body := wire(object{"graph": loaded, "prune": o.Prune, "read_set": plan["read_set"]})
	for {
		ack, e := client.request(r.ctx, "POST", path+"/apply", body, key)
		if e == nil {
			v := maps.Clone(target)
			out := object{"target": v}
			for k, v := range ack {
				out[k] = v
			}
			return finish("apply", out)
		}
		a, ok := e.(*APIError)
		if !ok || !a.Unknown || o.NonInteractive {
			return result, e
		}
		fmt.Fprintln(r.errOut, "Outcome unknown. Leaving loses memory-only exact retry material; it does not cancel or roll back server effects.")
		if a.RetryAfter > 0 {
			d := time.Duration(a.RetryAfter * float64(time.Second))
			if d < 0 {
				d = time.Duration(1<<63 - 1)
			}
			if e = waitContext(r.ctx, d); e != nil {
				return result, commandError("cancelled", "Cancelled; apply outcome remains unknown", 3)
			}
		}
		if e = r.confirm("Retry the identical request/key/preconditions?", o, false); e != nil {
			return result, e
		}
	}
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
