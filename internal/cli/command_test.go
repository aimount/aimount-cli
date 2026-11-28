package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineCommandHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := globalDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "Available Commands:"},
		{[]string{"-h"}, "Available Commands:"},
		{[]string{"--help"}, "Available Commands:"},
		{[]string{"auth"}, "login"},
		{[]string{"auth", "-h"}, "logout"},
		{[]string{"auth", "login", "--help"}, "aimount auth login"},
		{[]string{"projects"}, "list"},
		{[]string{"help", "auth"}, "login"},
		{[]string{"apply", "--help"}, "--auto-approve"},
		{[]string{"completion", "--help"}, "zsh"},
		{[]string{"resolve"}, "application"},
		{[]string{"resolve", "application", "--help"}, "application <slug>"},
		{[]string{"status", "--help"}, "--stack"},
		{[]string{"apply", "--help"}, "--require-empty"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			exit := Main(context.Background(), tc.args, map[string]string{"AIMOUNT_API_URL": "", "AIMOUNT_TOKEN": ""}, os.Stdin, &out, &stderr)
			if exit != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
			}
		})
	}
}

func TestHelpAndParseErrorsUseOneJSONEnvelope(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"auth", "--help", "--output", "json"}, true},
		{[]string{"help", "apply", "--output=json"}, true},
		{[]string{"plan", "--unsupported", "--output", "json"}, false},
		{[]string{"--https://synthetic-private@example.test", "--output=json"}, false},
		{[]string{"auth", "synthetic-private", "--output", "json"}, false},
		{[]string{"resolve", "application", "synthetic-private", "--output", "json"}, false},
	} {
		var out, stderr bytes.Buffer
		exit := Main(context.Background(), tc.args, nil, os.Stdin, &out, &stderr)
		var envelope object
		if json.Unmarshal(out.Bytes(), &envelope) != nil || envelope["version"] != float64(1) || envelope["ok"] != tc.ok || stderr.Len() != 0 || (exit == 0) != tc.ok {
			t.Fatalf("args=%v exit=%d stdout=%s stderr=%s", tc.args, exit, &out, &stderr)
		}
		if strings.Contains(out.String(), "synthetic-private") {
			t.Fatal("invalid argument disclosed")
		}
	}
}

func TestCobraFlagCompatibility(t *testing.T) {
	o, err := parseOptions([]string{"--root", ".", "--api-url=https://API.example:443", "auth", "--output", "json", "status"}, nil, true)
	if err != nil || o.Command != "auth" || len(o.Args) != 1 || o.Args[0] != "status" || o.API != "https://api.example" || !o.NonInteractive {
		t.Fatal(o, err)
	}
	for _, args := range [][]string{
		{"plan", "--root", "a", "--root", "b"},
		{"--api-url", "https://example.test", "auth", "status", "--api-url=https://example.test"},
		{"plan", "--root="},
		{"plan", "--api-url", ""},
		{"auth", "status", "--prune"},
		{"auth", "status", "--prune=false"},
		{"--force=false", "plan"},
		{"plan", "--auto-approve"},
		{"apply", "--force"},
		{"auth", "status", "extra"},
		{"plan", "--root", "--project"},
		{"plan", "--require-empty"},
		{"apply", "--require-empty", "--prune"},
		{"apply", "--stack"},
		{"resolve", "application"},
		{"resolve", "application", "first", "second"},
	} {
		if _, err := parseOptions(args, nil, false); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestScopedFlagsKeepInterspersedPlacement(t *testing.T) {
	for _, args := range [][]string{
		{"--prune", "plan"},
		{"--auto-approve", "apply", "--prune"},
		{"--force", "pull"},
		{"--yes", "link"},
	} {
		if o, err := parseOptions(args, nil, false); err != nil || !(o.Prune || o.Approve || o.Force || o.Yes) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestSafeCommandSuggestions(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Main(context.Background(), []string{"aplpy"}, nil, os.Stdin, &out, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "apply") || strings.Contains(stderr.String(), "aplpy") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
}

func TestUnknownRootCommandKeepsValidationCode(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Main(context.Background(), []string{"unknown", "--output", "json"}, nil, os.Stdin, &out, &stderr)
	var envelope object
	if json.Unmarshal(out.Bytes(), &envelope) != nil || code != 1 || obj(envelope["error"])["code"] != "validation_failed" || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
}

func TestAllHumanSummaries(t *testing.T) {
	target := object{"api": "https://api.example.test", "project": object{"id": "prj_0123456789ABCDEF", "name": "Pet Care"}}
	for _, tc := range []struct {
		result Result
		want   string
	}{
		{Result{Command: "auth login", Value: object{"identity": object{"principal": object{"user_id": "usr_0123456789ABCDEF"}}}}, "Signed in"},
		{Result{Command: "auth logout", Value: object{"api": "https://api.example.test"}}, "Server PATs were not revoked"},
		{Result{Command: "link", Value: target}, "Project linked"},
		{Result{Command: "status", Value: object{"api": "https://api.example.test"}}, "No Project selected"},
		{Result{Command: "projects list", Value: object{"items": []any{}}}, "No active Projects"},
		{Result{Command: "pull", Value: object{"files": []string{"tools/pets/tool.yaml"}}}, "Pulled 1 managed files"},
		{Result{Command: "apply", Value: object{"target": target, "changed": true, "resources": []any{object{"operation": "CREATE", "kind": "tool", "id": "tol_0123456789ABCDEF"}}}}, "CREATE tool tol_0123456789ABCDEF"},
		{Result{Command: "apply", Value: object{"target": target, "changed": false}}, "No changes applied"},
		{Result{Command: "plan", Value: object{"target": target, "plan": object{"changes": []any{}}}}, "Pet Care"},
	} {
		var out bytes.Buffer
		printText(&out, tc.result)
		if !strings.Contains(out.String(), tc.want) || strings.HasPrefix(out.String(), "{") {
			t.Errorf("%s: %s", tc.result.Command, &out)
		}
	}
	if got := textValue("line\n\x1b[31m\u009b\t"); got != "line\\u000a\\u001b[31m\\u009b\\u0009" {
		t.Fatalf("unsafe text: %q", got)
	}
}

func TestTextErrorsAreReadableAndSafe(t *testing.T) {
	var out, stderr bytes.Buffer
	exit := Main(context.Background(), []string{"auth", "status", "--unsupported"}, nil, os.Stdin, &out, &stderr)
	if exit != 1 || out.Len() != 0 || !strings.HasPrefix(stderr.String(), "Error: ") || strings.HasPrefix(stderr.String(), `"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
	}
}

func TestCompletionIsOffline(t *testing.T) {
	var out, stderr bytes.Buffer
	exit := Main(context.Background(), []string{"completion", "zsh"}, map[string]string{"AIMOUNT_API_URL": ""}, os.Stdin, &out, &stderr)
	if exit != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "#compdef aimount") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
	}
}

func TestCompletionProtocolKeepsDirectiveLast(t *testing.T) {
	var out, stderr bytes.Buffer
	exit := Main(context.Background(), []string{"__complete", "auth", ""}, nil, os.Stdin, &out, &stderr)
	if exit != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "login\t") || !strings.HasSuffix(out.String(), ":4\n") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
	}
}

func TestResolveApplicationOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const projectID = "prj_0123456789ABCDEF"
	const appID = "app_0123456789ABCDEF"
	const otherID = "app_0000000000000001"
	const goodBindings = `{"version":1,"bindings":[{"kind":"model","slug":"product","id":"mdl_0123456789ABCDEF"},{"kind":"application","slug":"other","id":"` + otherID + `"},{"kind":"application","slug":"product","id":"` + appID + `"},{"kind":"application","slug":"old_product","id":"` + appID + `","future":true}]}`
	root := t.TempDir()
	writeFiles(t, root, map[string]string{".aimount/project.json": `{"project_id":"` + projectID + `"}`})
	for _, tc := range []struct {
		name, slug, body, wantID, wantError string
		status                              int
		text, linked                        bool
	}{
		{name: "primary text via linked project", slug: "product", body: goodBindings, wantID: appID, text: true, linked: true},
		{name: "alias JSON via explicit project", slug: "old_product", body: goodBindings, wantID: appID},
		{name: "second Application resolves independently", slug: "other", body: goodBindings, wantID: otherID},
		{name: "no prefix match", slug: "prod", body: goodBindings, wantError: "application_not_found"},
		{name: "empty bindings", slug: "product", body: `{"version":1,"bindings":[]}`, wantError: "application_not_found"},
		{name: "duplicate binding even with same ID", slug: "product", body: `{"version":1,"bindings":[{"kind":"application","slug":"product","id":"` + appID + `"},{"kind":"application","slug":"product","id":"` + appID + `"}]}`, wantError: "invalid_response"},
		{name: "opaque Application ID", slug: "product", body: `{"version":1,"bindings":[{"kind":"application","slug":"product","id":"opaque"}]}`, wantError: "invalid_response"},
		{name: "wrong Application ID prefix", slug: "product", body: strings.ReplaceAll(goodBindings, appID, "prj_0123456789ABCDEF"), wantError: "invalid_response"},
		{name: "malformed bindings", slug: "product", body: `{"version":1,"bindings":null}`, wantError: "invalid_response"},
		{name: "API not found", slug: "product", body: `{"error":{"code":"not_found"}}`, status: 404, wantError: "not_found"},
		{name: "API permission failure", slug: "product", body: `{"error":{"code":"permission_denied"}}`, status: 403, wantError: "permission_denied"},
		{name: "transport failure", slug: "product", status: -1, wantError: "transport_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v0/auth/identity":
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case "GET /v0/projects/" + projectID:
					io.WriteString(w, `{"id":"`+projectID+`","slug":"example_project","name":"Example","status":"active"}`)
				case "GET /v0/projects/" + projectID + "/stack/bindings":
					if tc.status == -1 {
						panic(http.ErrAbortHandler)
					}
					if tc.status > 0 {
						w.WriteHeader(tc.status)
					}
					io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			args := []string{"resolve", "application", "--root", root, tc.slug, "--api-url", server.URL}
			if !tc.linked {
				args = append(args, "--project", projectID)
			}
			if !tc.text {
				args = append(args, "--output", "json")
			}
			var out, stderr bytes.Buffer
			exit := Main(context.Background(), args, map[string]string{"AIMOUNT_TOKEN": "synthetic"}, os.Stdin, &out, &stderr)
			if stderr.Len() != 0 {
				t.Fatalf("unexpected stderr %q", &stderr)
			}
			if tc.text {
				if exit != 0 || out.String() != tc.wantID+"\n" {
					t.Fatalf("exit=%d stdout=%q", exit, &out)
				}
				return
			}
			var envelope object
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope["version"] != float64(1) || envelope["command"] != "resolve application" {
				t.Fatalf("unexpected envelope %q: %v", &out, err)
			}
			if tc.wantError == "" {
				if exit != 0 || envelope["ok"] != true || obj(envelope["result"])["application_id"] != tc.wantID {
					t.Fatalf("exit=%d stdout=%q", exit, &out)
				}
			} else if exit != 1 || envelope["ok"] != false || obj(envelope["error"])["code"] != tc.wantError {
				t.Fatalf("exit=%d stdout=%q want error %s", exit, &out, tc.wantError)
			}
		})
	}
}

func TestStatusStackUsesProjectOrganizationAndAllBindings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const projectID = "prj_0123456789ABCDEF"
	const projectOrg = "org_0123456789ABCDEF"
	const selectedOrg = "org_0000000000000001"
	for _, tc := range []struct {
		name, bindings string
		stack, empty   bool
	}{
		{name: "ordinary status skips both extra reads"},
		{name: "empty", stack: true, empty: true, bindings: "[]"},
		{name: "storage-only is not empty", stack: true, bindings: `[{"kind":"storage","slug":"docs","id":"sto_0123456789ABCDEF"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v0/auth/identity":
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case "GET /v0/projects/" + projectID:
					io.WriteString(w, `{"id":"`+projectID+`","organization_id":"`+projectOrg+`","slug":"playground","name":"Playground","status":"active"}`)
				case "GET /v0/organizations/" + projectOrg:
					if !tc.stack {
						t.Error("ordinary status fetched Organization")
					}
					io.WriteString(w, `{"id":"`+projectOrg+`","slug":"project_org","name":"Project Organization"}`)
				case "GET /v0/projects/" + projectID + "/stack/bindings":
					if !tc.stack {
						t.Error("ordinary status fetched bindings")
					}
					io.WriteString(w, `{"version":1,"bindings":`+tc.bindings+`}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			dir, err := globalDirectory()
			if err != nil {
				t.Fatal(err)
			}
			if err := atomicJSON(filepath.Join(dir, "config.json"), object{"api_url": server.URL, "contexts": object{server.URL: object{"user_id": "usr_0123456789ABCDEF", "organization_id": selectedOrg}}}); err != nil {
				t.Fatal(err)
			}
			args := []string{"status", "--project", projectID, "--root", t.TempDir(), "--output=json"}
			if tc.stack {
				args = append(args, "--stack")
			}
			var out, stderr bytes.Buffer
			exit := Main(context.Background(), args, map[string]string{"AIMOUNT_TOKEN": "synthetic"}, os.Stdin, &out, &stderr)
			var envelope object
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || exit != 0 || stderr.Len() != 0 || envelope["ok"] != true {
				t.Fatalf("exit=%d stdout=%q stderr=%q: %v", exit, &out, &stderr, err)
			}
			result := obj(envelope["result"])
			if result["organization_id"] != selectedOrg || obj(result["project"])["organization_id"] != projectOrg {
				t.Fatal("wrong Organization identities", result)
			}
			if tc.stack {
				if result["project_organization_slug"] != "project_org" || result["stack_empty"] != tc.empty {
					t.Fatal("wrong Stack status", result)
				}
			} else if _, present := result["stack_empty"]; present {
				t.Fatal("ordinary status gained opt-in fields", result)
			}
		})
	}
}

func TestHumanOutputFromCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/auth/identity":
			w.Write([]byte(`{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`))
		case "/v0/organizations/org_0123456789ABCDEF":
			w.Write([]byte(`{"id":"org_0123456789ABCDEF","slug":"demo_org","name":"Demo"}`))
		case "/v0/organizations/org_0123456789ABCDEF/projects":
			w.Write([]byte(`{"items":[{"id":"prj_0123456789ABCDEF","slug":"example_project","name":"Pet Care\u001b[31m\nInjected","status":"active"}],"next_cursor":null}`))
		case "/v0/projects/prj_0123456789ABCDEF":
			w.Write([]byte(`{"id":"prj_0123456789ABCDEF","slug":"example_project","name":"Pet Care","status":"active"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	env := map[string]string{"AIMOUNT_TOKEN": "synthetic-private-token", "AIMOUNT_API_URL": server.URL}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"auth", "status"}, []string{"Account", "usr_0123456789ABCDEF", "AIMOUNT_TOKEN"}},
		{[]string{"switch", "--organization", "org_0123456789ABCDEF"}, []string{"Organization", "Demo"}},
		{[]string{"projects", "list"}, []string{"ID", "NAME", "Pet Care\\u001b[31m\\u000aInjected"}},
		{[]string{"status", "--project", "prj_0123456789ABCDEF"}, []string{"API", "Project", "Pet Care"}},
		{[]string{"unlink", "--root", t.TempDir()}, []string{"Project binding removed"}},
	} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), tc.args, env, os.Stdin, &out, &stderr); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", tc.args, code, &stderr)
		}
		for _, want := range tc.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: missing %s in %s", tc.args, want, &out)
			}
		}
		if strings.HasPrefix(out.String(), "{") || strings.ContainsAny(out.String(), "\x1b\x9b") || strings.Contains(out.String(), "synthetic-private") {
			t.Fatalf("unreadable/unsafe output: %s", &out)
		}
	}
}
