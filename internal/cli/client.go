package cli

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

type Options struct {
	Command                                    string
	Help                                       string
	Args                                       []string
	Root, API, Project, Organization, Output   string
	NonInteractive, Prune, Force, Approve, Yes bool
	Recover                                    bool
}

func normalizeAPI(value string) (string, error) {
	bad := func() (string, error) { return "", errors.New("invalid API URL") }
	if strings.ContainsAny(value, "?#\\") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return bad()
	}
	// net/url decodes escaped non-ASCII host bytes, so inspect the authority first.
	_, authority, _ := strings.Cut(value, "://")
	authority, _, _ = strings.Cut(authority, "/")
	if strings.Contains(authority, "%") {
		return bad()
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || !slices.Contains([]string{"http", "https"}, u.Scheme) {
		return bad()
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	if strings.HasPrefix(u.Host, "[") {
		if ipErr != nil || !ip.Is6() || ip.Zone() != "" {
			return bad()
		}
		host = ip.String()
		if ip.Is4In6() {
			b := ip.As16()
			host = "::ffff:" + strconv.FormatUint(uint64(b[12])<<8|uint64(b[13]), 16) + ":" + strconv.FormatUint(uint64(b[14])<<8|uint64(b[15]), 16)
		}
	} else {
		host, err = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false), idna.BidiRule(), idna.CheckHyphens(false), idna.VerifyDNSLength(false)).ToASCII(host)
		if err != nil || host == "" || strings.ContainsAny(host, "%/:@[]\\?#<>^|") || strings.IndexFunc(host, unicode.IsControl) >= 0 {
			return bad()
		}
		host = strings.ToLower(host)
		if ip, e := netip.ParseAddr(host); e == nil {
			if !ip.Is4() {
				return bad()
			}
		} else {
			// Numeric final labels are ambiguous browser IPv4 spellings, not DNS.
			domain := strings.TrimSuffix(host, ".")
			last := domain[strings.LastIndexByte(domain, '.')+1:]
			hex := strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == ""
			if hex || last != "" && strings.Trim(last, "0123456789") == "" {
				return bad()
			}
		}
	}
	loopback := host == "::1"
	if ip, e := netip.ParseAddr(host); e == nil && ip.Is4() {
		loopback = ip.As4()[0] == 127
	}
	if u.Scheme == "http" && host != "localhost" && !loopback {
		return bad()
	}
	port := u.Port()
	if port != "" {
		number, e := strconv.ParseUint(port, 10, 16)
		if e != nil {
			return bad()
		}
		port = strconv.FormatUint(number, 10)
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	parts := strings.Split(u.EscapedPath(), "/")
	stack := []string{}
	for i, part := range parts {
		dot := strings.ToLower(strings.ReplaceAll(strings.ToLower(part), "%2e", "."))
		switch dot {
		case ".":
			if i == len(parts)-1 {
				stack = append(stack, "")
			}
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			if i == len(parts)-1 {
				stack = append(stack, "")
			}
		default:
			stack = append(stack, part)
		}
	}
	escaped := strings.Join(stack, "/")
	decoded, e := url.PathUnescape(escaped)
	if e != nil {
		return bad()
	}
	u.Path, u.RawPath = decoded, escaped
	return strings.TrimRight(u.String(), "/"), nil
}

func resolveOptions(o Options, explicit map[string]bool, env map[string]string, tty bool) (Options, error) {
	if !slices.Contains([]string{"text", "json"}, o.Output) {
		return o, errors.New("invalid --output")
	}
	o.NonInteractive = o.NonInteractive || o.Output == "json" || env["CI"] == "true" || !tty
	var err error
	o.Root, err = filepath.Abs(o.Root)
	if err != nil {
		return o, errors.New("invalid root")
	}
	if o.Recover {
		if o.Force {
			return o, errors.New("--recover cannot be combined with --force")
		}
		return o, nil // Offline recovery needs common local options, not a remote target.
	}
	if explicit["api-url"] {
		o.API, err = normalizeAPI(o.API)
	} else if v, ok := env["AIMOUNT_API_URL"]; ok {
		if strings.TrimSpace(v) == "" {
			return o, errors.New("AIMOUNT_API_URL is empty")
		}
		o.API, err = normalizeAPI(v)
	}
	if err != nil {
		return o, err
	}
	if v, ok := env["AIMOUNT_PROJECT"]; ok && !explicit["project"] {
		if strings.TrimSpace(v) == "" {
			return o, errors.New("AIMOUNT_PROJECT is empty")
		}
		o.Project = v
	}
	if o.Project != "" && !idValid(o.Project, "prj") {
		return o, errors.New("invalid Project ID")
	}
	if o.Organization != "" && !idValid(o.Organization, "org") {
		return o, errors.New("invalid Organization ID")
	}
	return o, nil
}

type APIError struct {
	Status     int
	Code       string
	RetryAfter float64
	Unknown    bool
}

func (e *APIError) Error() string {
	if e.Unknown {
		return "apply outcome unknown; preserve the original request and key"
	}
	return "API " + strconv.Itoa(e.Status) + ": " + e.Code
}

type Client struct {
	API, token string
	http       *http.Client
	date       time.Time
	observed   time.Time
}

func boundedHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 35 * time.Second
	t.MaxResponseHeaderBytes = 64 << 10
	return &http.Client{Transport: t, Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func newClient(api, token string) *Client {
	return &Client{API: api, token: token, http: boundedHTTP()}
}
func (c *Client) serverTime() (int64, error) {
	if c.date.IsZero() {
		return 0, errors.New("server Date unavailable; apply was not sent")
	}
	return c.date.Add(time.Since(c.observed)).UnixMilli(), nil
}
func (c *Client) request(ctx context.Context, method, path, body, key string) (object, error) {
	mutation := method == "POST" && strings.HasSuffix(path, "/stack/apply")
	bad := func(status int, code string) (object, error) {
		return nil, &APIError{Status: status, Code: code, Unknown: mutation}
	}
	r, err := http.NewRequestWithContext(ctx, method, c.API+path, strings.NewReader(body))
	if err != nil {
		return bad(0, "transport_unavailable")
	}
	// net/http treats Idempotency-Key as permission to replay a POST when a
	// pooled connection dies. Recovery here is exclusively manual and exact.
	if mutation {
		r.GetBody = nil
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return bad(0, "transport_unavailable")
	}
	defer response.Body.Close()
	if date, err := http.ParseTime(response.Header.Get("Date")); err == nil {
		c.date = date
		c.observed = time.Now()
	}
	s := response.StatusCode
	if s >= 300 && s < 400 {
		return bad(s, "redirect_refused")
	}
	var v any = object{}
	if s != 204 {
		raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		if err != nil || len(raw) > 16<<20 {
			return bad(s, "invalid_response")
		}
		v, err = decodeJSON(raw, false)
		if err != nil {
			return bad(s, "invalid_response")
		}
	}
	m := obj(v)
	if m == nil {
		return bad(s, "invalid_response")
	}
	if s < 200 || s >= 300 {
		code := str(obj(m["error"])["code"])
		if !regexp.MustCompile(`^[a-z0-9_]{1,80}$`).MatchString(code) {
			code = "request_failed"
		}
		retry := 0.0
		h := response.Header.Get("Retry-After")
		if regexp.MustCompile(`^\d+$`).MatchString(h) {
			retry, _ = strconv.ParseFloat(h, 64)
		} else if tm, e := http.ParseTime(h); e == nil {
			retry = time.Until(tm).Seconds()
			if retry < 0 {
				retry = 0
			}
		}
		return nil, &APIError{s, code, retry, mutation && (s >= 500 || code == "idempotency_in_progress")}
	}
	if mutation {
		rs, ok := m["resources"].([]any)
		_, changed := m["changed"].(bool)
		if !numberIs(m["version"], 1) || !ok || !changed {
			return bad(s, "invalid_response")
		}
		out := []any{}
		for _, v := range rs {
			r := obj(v)
			if r == nil || !slices.Contains([]string{"model", "tool", "agent", "application"}, str(r["kind"])) {
				return bad(s, "invalid_response")
			}
			for _, k := range []string{"id", "etag", "operation"} {
				if _, ok := r[k].(string); !ok {
					return bad(s, "invalid_response")
				}
			}
			for _, k := range []string{"version", "revision"} {
				if _, ok := exactInt(r[k], maxSafe, -maxSafe); !ok {
					return bad(s, "invalid_response")
				}
			}
			if _, ok := r["changed"].(bool); !ok {
				return bad(s, "invalid_response")
			}
			out = append(out, fields(r, "kind", "id", "etag", "version", "revision", "operation", "changed"))
		}
		return object{"version": 1, "changed": m["changed"], "resources": out}, nil
	}
	if method == "GET" && path == "/v0/auth/identity" {
		p, a := obj(m["principal"]), obj(m["authentication"])
		if !idValid(str(p["user_id"]), "usr") || a["method"] != "pat" {
			return bad(s, "invalid_response")
		}
		for _, k := range []string{"type", "display_name"} {
			if v, ok := p[k]; ok {
				if _, ok := v.(string); !ok {
					return bad(s, "invalid_response")
				}
			}
		}
		for _, k := range []string{"expires_at", "pat_id"} {
			if v, ok := a[k]; ok {
				if _, ok := v.(string); !ok {
					return bad(s, "invalid_response")
				}
			}
		}
		return object{"principal": fields(p, "type", "user_id", "display_name"), "authentication": fields(a, "method", "expires_at", "pat_id")}, nil
	}
	if method == "GET" && strings.HasSuffix(path, "/stack/bindings") {
		bs, ok := m["bindings"].([]any)
		if !numberIs(m["version"], 1) || !ok {
			return bad(s, "invalid_response")
		}
		out := []any{}
		for _, v := range bs {
			b := obj(v)
			if !slices.Contains([]string{"model", "tool", "agent", "application"}, str(b["kind"])) || !nameRE.MatchString(str(b["name"])) || str(b["id"]) == "" {
				return bad(s, "invalid_response")
			}
			out = append(out, fields(b, "kind", "name", "id"))
		}
		return object{"version": 1, "bindings": out}, nil
	}
	if method == "POST" && strings.HasSuffix(path, "/stack/plan") {
		changes, ok := m["changes"].([]any)
		if !numberIs(m["version"], 1) || obj(m["read_set"]) == nil || !ok {
			return bad(s, "invalid_response")
		}
		readSet, e := projectReadSet(obj(m["read_set"]))
		if e != nil {
			return bad(s, "invalid_response")
		}
		out := []any{}
		for _, v := range changes {
			r := obj(v)
			if r == nil || !slices.Contains([]string{"model", "tool", "agent", "application"}, str(r["kind"])) || str(r["name"]) == "" || str(r["operation"]) == "" {
				return bad(s, "invalid_response")
			}
			projected := fields(r, "kind", "name", "id", "operation", "archived", "runtime_cascade")
			if v, ok := r["erasure_scope"]; ok {
				scope, ok := v.([]any)
				if !ok {
					return bad(s, "invalid_response")
				}
				for _, item := range scope {
					if !slices.Contains([]string{"users", "conversations", "files", "configuration", "private_tests", "private_files"}, str(item)) {
						return bad(s, "invalid_response")
					}
				}
				projected["erasure_scope"] = scope
			}
			for _, field := range []string{"before", "after"} {
				if spec, ok := r[field]; ok {
					p := readRemoteSpec(str(r["kind"]), obj(spec), true)
					if p == nil {
						return bad(s, "invalid_response")
					}
					projected[field] = p
				}
			}
			if reviews, ok := r["credential_review"]; ok {
				a, ok := reviews.([]any)
				if !ok {
					return bad(s, "invalid_response")
				}
				safe := []any{}
				for _, v := range a {
					review := obj(v)
					if str(review["target"]) == "" || !slices.Contains([]string{"replace", "remove", "retain"}, str(review["action"])) {
						return bad(s, "invalid_response")
					}
					if _, ok := exactInt(review["expected_version"], maxSafe, 0); !ok {
						return bad(s, "invalid_response")
					}
					safe = append(safe, fields(review, "target", "action", "expected_version"))
				}
				projected["credential_review"] = safe
			}
			out = append(out, projected)
		}
		return object{"version": 1, "read_set": readSet, "changes": out}, nil
	}
	if method == "GET" && regexp.MustCompile(`^/v0/projects/prj_[0-9A-HJKMNP-TV-Z]{16}$`).MatchString(path) {
		if !idValid(str(m["id"]), "prj") || str(m["id"]) != strings.TrimPrefix(path, "/v0/projects/") || str(m["status"]) == "" {
			return bad(s, "invalid_response")
		}
		if e := validateDiscoveryResource(m, "prj"); e != nil {
			return bad(s, "invalid_response")
		}
		return fields(m, "id", "organization_id", "display_name", "created_at", "updated_at", "version", "status", "deleted_at", "restore_until"), nil
	}
	if method == "GET" && (path == "/v0/organizations" || strings.HasPrefix(path, "/v0/organizations?cursor=") || strings.HasPrefix(path, "/v0/organizations/")) {
		if strings.Contains(strings.Split(path, "?")[0], "/projects") || path == "/v0/organizations" || strings.HasPrefix(path, "/v0/organizations?cursor=") {
			a, ok := m["items"].([]any)
			if !ok {
				return bad(s, "invalid_response")
			}
			out := []any{}
			prefix := "org"
			if strings.Contains(path, "/projects") {
				prefix = "prj"
			}
			for _, v := range a {
				r := obj(v)
				if e := validateDiscoveryResource(r, prefix); e != nil {
					return bad(s, "invalid_response")
				}
				out = append(out, fields(r, "id", "name", "organization_id", "display_name", "created_at", "updated_at", "version", "status", "deleted_at", "restore_until", "role"))
			}
			projected := object{"items": out}
			if cursor, ok := m["next_cursor"]; ok {
				if cursor != nil {
					if _, ok := cursor.(string); !ok {
						return bad(s, "invalid_response")
					}
				}
				projected["next_cursor"] = cursor
			}
			return projected, nil
		}
		if e := validateDiscoveryResource(m, "org"); e != nil || str(m["id"]) != strings.TrimPrefix(path, "/v0/organizations/") {
			return bad(s, "invalid_response")
		}
		return fields(m, "id", "name", "display_name", "created_at", "updated_at", "version", "role"), nil
	}
	return m, nil
}

// stack owns the export boundary: fetch once, validate consumed fields and
// discard operational additions before returning the graph to its consumers.
func (c *Client) stack(ctx context.Context, projectID string) (graph, error) {
	g := emptyGraph()
	m, e := c.request(ctx, "GET", "/v0/projects/"+projectID+"/stack", "", "")
	if e != nil {
		return g, e
	}
	if !numberIs(m["version"], 1) {
		return g, &APIError{Status: 200, Code: "invalid_response"}
	}
	for _, kind := range []string{"model", "tool", "agent", "application"} {
		a, ok := m[kind+"s"].([]any)
		if !ok {
			return g, &APIError{Status: 200, Code: "invalid_response"}
		}
		out := []object{}
		seen := map[string]bool{}
		for _, v := range a {
			r := obj(v)
			if r == nil || !nameRE.MatchString(str(r["name"])) || obj(r["spec"]) == nil {
				return g, &APIError{Status: 200, Code: "invalid_response"}
			}
			if kind == "application" && seen[str(r["name"])] {
				return g, &APIError{Status: 200, Code: "invalid_response"}
			}
			seen[str(r["name"])] = true
			if v, ok := r["archived"]; ok {
				if _, ok := v.(bool); !ok {
					return g, &APIError{Status: 200, Code: "invalid_response"}
				}
			}
			spec := readRemoteSpec(kind, obj(r["spec"]), false)
			if spec == nil {
				return g, &APIError{Status: 200, Code: "invalid_response"}
			}
			p := fields(r, "name", "archived")
			p["spec"] = spec
			out = append(out, p)
		}
		switch kind {
		case "model":
			g.Models = out
		case "tool":
			g.Tools = out
		case "agent":
			g.Agents = out
		case "application":
			g.Applications = out
		}
	}
	return g, nil
}

func projectReadSet(m object) (object, error) {
	bad := func() (object, error) { return nil, errors.New("invalid read-set response") }
	if m == nil {
		return bad()
	}
	out := object{}
	incarnation, ok := m["membership_incarnation"].(string)
	if !ok || incarnation == "" || len(incarnation) > 1024 {
		return bad()
	}
	out["membership_incarnation"] = incarnation
	for _, field := range []string{"membership_version", "membership_access_epoch", "project_lifecycle_epoch"} {
		minimum := int64(0)
		if field == "membership_version" {
			minimum = 1
		}
		if _, ok := exactInt(m[field], math.MaxInt64, minimum); !ok {
			return bad()
		}
		out[field] = m[field]
	}
	prefixes := map[string]string{"model": "mdl", "tool": "tol", "agent": "agt", "application": "app"}
	for _, field := range []string{"resources", "bindings", "credentials"} {
		a, ok := m[field].([]any)
		if !ok || len(a) > 100000 {
			return bad()
		}
		safe := []any{}
		for _, v := range a {
			r := obj(v)
			if r == nil {
				return bad()
			}
			kind := str(r["kind"])
			prefix, ok := prefixes[kind]
			if !ok {
				return bad()
			}
			id, ok := r["id"].(string)
			if !ok || (!idValid(id, prefix) && !(field == "credentials" && id == "")) {
				return bad()
			}
			switch field {
			case "resources":
				etag, ok := r["etag"].(string)
				if !ok || etag == "" || len(etag) > 1024 {
					return bad()
				}
				safe = append(safe, fields(r, "kind", "id", "etag"))
			case "bindings":
				if !nameRE.MatchString(str(r["name"])) {
					return bad()
				}
				safe = append(safe, fields(r, "kind", "name", "id"))
			case "credentials":
				if (kind != "model" && kind != "tool") || !nameRE.MatchString(str(r["name"])) {
					return bad()
				}
				targetRE := nameRE
				if kind == "tool" {
					targetRE = contractRE
				}
				if !targetRE.MatchString(str(r["target"])) {
					return bad()
				}
				if _, ok := exactInt(r["version"], math.MaxInt64, 0); !ok {
					return bad()
				}
				safe = append(safe, fields(r, "kind", "id", "name", "target", "version"))
			}
		}
		out[field] = safe
	}
	return out, nil
}
func validateDiscoveryResource(m object, prefix string) error {
	if m == nil || !idValid(str(m["id"]), prefix) {
		return errors.New("invalid_response")
	}
	for _, k := range []string{"name", "organization_id", "display_name", "created_at", "updated_at", "status", "deleted_at", "restore_until", "role"} {
		if v, ok := m[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return errors.New("invalid_response")
			}
		}
	}
	if v, ok := m["version"]; ok {
		if _, ok := exactInt(v, maxSafe, 1); !ok {
			return errors.New("invalid_response")
		}
	}
	if v, ok := m["organization_id"]; ok && !idValid(str(v), "org") {
		return errors.New("invalid_response")
	}
	return nil
}
func (c *Client) pages(ctx context.Context, path string) ([]any, error) {
	items := []any{}
	seen := map[string]bool{}
	for {
		p, err := c.request(ctx, "GET", path, "", "")
		if err != nil {
			return nil, err
		}
		a, ok := p["items"].([]any)
		if !ok {
			return nil, &APIError{Status: 200, Code: "invalid_response"}
		}
		items = append(items, a...)
		if len(items) > 100000 {
			return nil, &APIError{Status: 200, Code: "invalid_response"}
		}
		if p["next_cursor"] == nil || p["next_cursor"] == "" {
			return items, nil
		}
		cursor, ok := p["next_cursor"].(string)
		if !ok || seen[cursor] {
			return nil, &APIError{Status: 200, Code: "invalid_response"}
		}
		seen[cursor] = true
		path = strings.Split(path, "?")[0] + "?cursor=" + url.QueryEscape(cursor)
	}
}
