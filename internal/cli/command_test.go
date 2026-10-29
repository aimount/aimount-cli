package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	target := object{"api": "https://api.example.test", "project": object{"id": "prj_0123456789ABCDEF", "display_name": "Pet Care"}}
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

func TestHumanOutputFromCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/auth/identity":
			w.Write([]byte(`{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`))
		case "/v0/organizations/org_0123456789ABCDEF":
			w.Write([]byte(`{"id":"org_0123456789ABCDEF","name":"Demo"}`))
		case "/v0/organizations/org_0123456789ABCDEF/projects":
			w.Write([]byte(`{"items":[{"id":"prj_0123456789ABCDEF","display_name":"Pet Care\u001b[31m\nInjected","status":"active"}]}`))
		case "/v0/projects/prj_0123456789ABCDEF":
			w.Write([]byte(`{"id":"prj_0123456789ABCDEF","display_name":"Pet Care","status":"active"}`))
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
