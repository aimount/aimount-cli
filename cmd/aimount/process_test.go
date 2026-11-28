package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutableEnvelopesAndNoRuntime(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range []struct {
		args []string
		code int
		ok   bool
	}{{[]string{"help", "--output", "json"}, 0, true}, {[]string{"auth", "status", "--output", "json"}, 1, false}, {[]string{"plan", "--unsupported", "--output", "json"}, 1, false}, {[]string{"unlink", "--output", "json", "--root", t.TempDir()}, 0, true}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c := exec.CommandContext(ctx, bin, tc.args...)
		c.Env = []string{"HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "PATH=/nonexistent"}
		out, e := c.Output()
		cancel()
		code := 0
		if e != nil {
			if ee, ok := e.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatal(e)
			}
		}
		var v map[string]any
		if e := json.Unmarshal(out, &v); e != nil {
			t.Fatalf("not JSON: %s", out)
		}
		if code != tc.code || v["ok"] != tc.ok || v["version"] != float64(1) {
			t.Fatal(tc, code, v)
		}
	}
	if _, e := os.Stat(bin); e != nil {
		t.Fatal(e)
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("AIMOUNT_TEST_APP_CLI"); bin != "" {
		if !filepath.IsAbs(bin) {
			t.Fatal("AIMOUNT_TEST_APP_CLI must be absolute")
		}
		return bin
	}
	bin := filepath.Join(t.TempDir(), "aimount")
	if out, e := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", out, e)
	}
	return bin
}

func TestExecutableUsesExplicitCandidateWithoutCompiler(t *testing.T) {
	if os.Getenv("AIMOUNT_TEST_CANDIDATE_HELPER") == "1" {
		bin := buildBinary(t)
		out, err := exec.Command(bin, "--help").CombinedOutput()
		if err != nil || !bytes.Contains(out, []byte("Usage:")) {
			t.Fatalf("candidate failed: %v %s", err, out)
		}
		return
	}
	bin := buildBinary(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutableUsesExplicitCandidateWithoutCompiler$")
	cmd.Env = isolatedEnv(t.TempDir(), "PATH=/nonexistent", "AIMOUNT_TEST_CANDIDATE_HELPER=1", "AIMOUNT_TEST_APP_CLI="+bin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external candidate required compiler: %v %s", err, out)
	}
}

func TestExecutablePlannerErrorsPreserveExitCategories(t *testing.T) {
	bin := buildBinary(t)
	for _, command := range []string{"plan", "apply"} {
		for _, tc := range []struct {
			code         string
			status, exit int
		}{
			{"stack_prune_required", http.StatusConflict, 1},
			{"stack_read_set_conflict", http.StatusConflict, 2},
			{"stack_credential_conflict", http.StatusPreconditionFailed, 2},
		} {
			t.Run(command+"/"+tc.code, func(t *testing.T) {
				home := t.TempDir()
				root := filepath.Join(home, "stack")
				if err := os.MkdirAll(filepath.Join(root, "applications"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "applications/product.yaml"), []byte("version: 1\nkind: application\nslug: product\nspec:\n  name: Product\n  models: {}\n  agents: {}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v0/auth/identity":
						io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
					case "/v0/projects/prj_0123456789ABCDEF":
						io.WriteString(w, `{"id":"prj_0123456789ABCDEF","slug":"example_project","name":"Example Project","status":"active"}`)
					case "/v0/projects/prj_0123456789ABCDEF/stack/plan":
						w.WriteHeader(tc.status)
						json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": tc.code}})
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, command, "--root", root, "--api-url", server.URL, "--project", "prj_0123456789ABCDEF", "--output=json")
				cmd.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic")
				out, err := cmd.Output()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tc.exit {
					t.Fatalf("wrong planner error exit: %v output=%s want=%d", err, out, tc.exit)
				}
				var result struct {
					OK    bool `json:"ok"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(out, &result); err != nil || result.OK || result.Error.Code != tc.code {
					t.Fatalf("wrong error envelope: %s", out)
				}
			})
		}
	}
}

func TestExecutableStandardHelp(t *testing.T) {
	bin := buildBinary(t)
	for _, args := range [][]string{nil, {"auth"}, {"auth", "-h"}, {"--help"}, {"help", "apply"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/nonexistent", "AIMOUNT_API_URL="}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		if err != nil || stderr.Len() != 0 || !bytes.Contains(out, []byte("Usage:")) {
			t.Fatalf("args=%v err=%v stdout=%s stderr=%s", args, err, out, &stderr)
		}
	}
}

func TestExecutableCompletionDoesNotExposeArguments(t *testing.T) {
	bin := buildBinary(t)
	for _, args := range [][]string{
		{"__complete", "--api-url=https://synthetic-private.example.test", "auth", ""},
		{"__complete", "auth", "--root", "synthetic-private", ""},
		{"__complete", "--output=json", "auth", ""},
		{"--output=json", "__complete", "auth", ""},
		{"__completeNoDesc", "auth", ""},
		{"__complete", "--unsupported=synthetic-private\x1b[31m", "auth", ""},
		{"__complete", "auth", "--output=synthetic-private", ""},
		{"__complete", "auth", "--api-url", ""},
		{"__complete", "auth", "--root=a", "--root=b", ""},
		{"__complete", "auth", "--output=json", "--unsupported=synthetic-private", ""},
		{"__complete", "auth", "--output=json", "--root=a", "--root=b", ""},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		home := t.TempDir()
		debug := filepath.Join(home, "completion-debug")
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = isolatedEnv(home, "BASH_COMP_DEBUG_FILE="+debug, "AIMOUNT_API_URL=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		text := string(out)
		if err != nil || stderr.Len() != 0 || strings.ContainsAny(text, "\x1b\x9b") || strings.Contains(text, "synthetic-private") || strings.HasPrefix(text, "{") {
			t.Fatalf("args=%q err=%v stdout=%q stderr=%q", args, err, out, &stderr)
		}
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if _, err := strconv.Atoi(strings.TrimPrefix(lines[len(lines)-1], ":")); err != nil {
			t.Fatalf("missing final completion directive: %q", out)
		}
		if args[1] == "--api-url=https://synthetic-private.example.test" && !strings.Contains(text, "login\t") {
			t.Fatalf("completed flag blocks subcommand completion: %q", out)
		}
		if len(args) > 3 && args[2] == "--output=json" && text != ":1\n" {
			t.Fatalf("invalid hidden completion must remain a raw error directive: %q", out)
		}
		if data, err := os.ReadFile(debug); err == nil && (bytes.Contains(data, []byte("synthetic-private")) || bytes.ContainsRune(data, '\x1b')) {
			t.Fatalf("completion debug file disclosed arguments: %q", data)
		}
	}
}
func ttyCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.CommandContext(ctx, "/usr/bin/script", append([]string{"-q", "-F", "/dev/null", bin}, args...)...)
	}
	parts := []string{bin}
	parts = append(parts, args...)
	for i, p := range parts {
		parts[i] = "'" + strings.ReplaceAll(p, "'", "'\\''") + "'"
	}
	return exec.CommandContext(ctx, "/usr/bin/script", "-qefc", strings.Join(parts, " "), "/dev/null")
}
func isolatedEnv(home string, extra ...string) []string {
	return append([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "PATH=" + os.Getenv("PATH")}, extra...)
}

func TestExecutableApplyRequireEmptyReviewsThenUsesCAS(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	root := filepath.Join(home, "stack")
	if err := os.MkdirAll(filepath.Join(root, "applications"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "applications/product.yaml"), []byte("version: 1\nkind: application\nslug: product\nspec:\n  name: Reviewed application\n  models: {}\n  agents: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const readSet = `{"resources":[],"bindings":[],"membership_incarnation":"00000000-0000-4000-8000-000000000001","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0,"credentials":[]}`
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v0/auth/identity":
			io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case "GET /v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","slug":"example_project","status":"active","name":"Safe"}`)
		case "POST /v0/projects/prj_0123456789ABCDEF/stack/plan":
			io.WriteString(w, `{"version":1,"read_set":`+readSet+`,"changes":[{"kind":"application","slug":"product","operation":"CREATE","after":{"name":"Reviewed application","models":{},"agents":{}}}]}`)
		case "POST /v0/projects/prj_0123456789ABCDEF/stack/apply":
			writes.Add(1)
			var input struct {
				ReadSet map[string]any `json:"read_set"`
				Prune   *bool          `json:"prune"`
			}
			var expected map[string]any
			if err := json.Unmarshal([]byte(readSet), &expected); err != nil {
				t.Error(err)
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			actualWire, _ := json.Marshal(input.ReadSet)
			expectedWire, _ := json.Marshal(expected)
			if !bytes.Equal(actualWire, expectedWire) || input.Prune == nil || *input.Prune || !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "s1.") {
				t.Error("apply did not carry the approved read set, non-prune mode and idempotency key")
			}
			io.WriteString(w, `{"version":1,"changed":true,"resources":[{"kind":"application","id":"app_0123456789ABCDEF","etag":"new-etag","version":1,"revision":1,"operation":"CREATE","changed":true}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"apply", "--require-empty", "--root", root, "--api-url", server.URL, "--project", "prj_0123456789ABCDEF"}
	cmd := ttyCommand(ctx, bin, append(args, "--output=json")...)
	cmd.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !bytes.Contains(out, []byte(`"code":"confirmation_required"`)) || bytes.Contains(out, []byte("[y/N] ")) || writes.Load() != 0 {
		t.Fatalf("JSON apply prompted or mutated: %v %s", err, out)
	}

	// Redirect only stdout, as the launcher does, so the operator sees the preview.
	receipt := filepath.Join(home, "receipt")
	wrapper := filepath.Join(home, "capture")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$@\" >\"$AIMOUNT_TEST_RECEIPT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = ttyCommand(ctx, wrapper, append([]string{bin}, args...)...)
	cmd.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic", "AIMOUNT_TEST_RECEIPT="+receipt)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	output := &processOutput{notify: make(chan struct{}, 1)}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(output.String(), "Apply this complete Project plan? [y/N]") {
		select {
		case <-output.notify:
		case <-ctx.Done():
			cmd.Wait()
			t.Fatal("confirmation prompt not visible", output.String())
		}
	}
	preview := output.String()
	writesBefore := writes.Load()
	io.WriteString(stdin, "y\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("approved apply failed: %v %s", err, output.String())
	}
	if !strings.Contains(preview, "CREATE application product") || !strings.Contains(preview, "Reviewed application") || writesBefore != 0 || writes.Load() != 1 {
		t.Fatalf("missing plan or premature mutation: %q, writes %d before/%d after", preview, writesBefore, writes.Load())
	}
	data, err := os.ReadFile(receipt)
	if err != nil || !strings.HasSuffix(string(data), "\n  CREATE application app_0123456789ABCDEF\n") || bytes.Contains(data, []byte("[y/N]")) {
		t.Fatalf("invalid stdout receipt: %v %q", err, data)
	}
}

func TestExecutableManualExactApplyRetry(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	root := filepath.Join(home, "stack")
	os.MkdirAll(filepath.Join(root, "models"), 0700)
	os.WriteFile(filepath.Join(root, "models/smart.yaml"), []byte("version: 1\nkind: model\nslug: smart\nspec:\n  name: Smart\n  limits: {context_window_tokens: 8192, max_output_tokens: 1024}\n  capabilities: {input_modalities: [text], output_modalities: [text], tool_calling: false}\n  nodes:\n    - name: primary\n      display_name: Primary\n      provider: openai_compatible\n      api: responses\n      base_url: https://provider.example/v1\n      model: test\n      credentials: {none: {}}\n"), 0600)
	if err := os.MkdirAll(filepath.Join(root, "agents/support"), 0700); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{
		"agents/support/agent.yaml":     "version: 1\nkind: agent\nslug: support\nspec:\n  name: Support Assistant\n  tools: [{slug: lookup, contract: v1}]\n",
		"agents/support/instruction.md": "Help\n",
	} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const agentIntent = `"agents":[{"archived":false,"slug":"support","spec":{"instruction":"Help\n","name":"Support Assistant","tools":[{"contract":"v1","slug":"lookup"}]}}]`
	os.MkdirAll(filepath.Join(root, "applications"), 0700)
	appPath := filepath.Join(root, "applications/renamed.yaml")
	os.WriteFile(appPath, []byte("version: 1\nkind: application\nslug: product\nspec:\n  name: Reviewed application\n  models: {}\n  agents: {}\n"), 0600)
	plans := 0
	type captured struct{ path, body, key string }
	writes := []captured{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/auth/identity":
			io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","slug":"example_project","status":"active","name":"Safe"}`)
		case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
			io.WriteString(w, `{"version":1,"bindings":[{"kind":"application","slug":"product","id":"app_0123456789ABCDEF"}]}`)
		case strings.HasSuffix(r.URL.Path, "/stack/plan"):
			plans++
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), agentIntent) {
				t.Error("plan lost Agent/Tool slug references")
			}
			if !strings.Contains(string(body), `"applications":[{"archived":false,"slug":"product","spec":{"agents":{},"models":{},"name":"Reviewed application"}}]`) {
				t.Error("plan lost complete Application graph")
			}
			io.WriteString(w, `{"version":1,"read_set":{"resources":[{"kind":"application","id":"app_0123456789ABCDEF","etag":"original-application-etag"}],"bindings":[{"kind":"application","slug":"product","id":"app_0123456789ABCDEF"}],"membership_incarnation":"00000000-0000-4000-8000-000000000001","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0,"credentials":[],"future_secret":"synthetic-private"},"changes":[{"kind":"model","slug":"smart","operation":"CREATE"},{"kind":"application","slug":"product","operation":"UPDATE","before":{"name":"Saved application","models":{},"agents":{}},"after":{"name":"Reviewed application","models":{},"agents":{}}}]}`)
		case strings.HasSuffix(r.URL.Path, "/stack/apply"):
			b, _ := io.ReadAll(r.Body)
			writes = append(writes, captured{r.URL.Path, string(b), r.Header.Get("Idempotency-Key")})
			if len(writes) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"code":"service_unavailable"}}`)
			} else {
				io.WriteString(w, `{"version":1,"changed":true,"resources":[{"kind":"application","id":"app_0123456789ABCDEF","etag":"new-etag","version":2,"revision":2,"operation":"UPDATE","changed":true}]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/stack"):
			io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[],"applications":[{"slug":"product","spec":{"name":"Saved application","models":{},"agents":{}}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := ttyCommand(ctx, bin, "apply", "--auto-approve", "--root", root, "--api-url", s.URL, "--project", "prj_0123456789ABCDEF")
	c.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic")
	stdin, e := c.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output := &processOutput{notify: make(chan struct{}, 1)}
	c.Stdout = output
	c.Stderr = output
	if e := c.Start(); e != nil {
		t.Fatal(e)
	}
	for !strings.Contains(output.String(), "Retry the identical request/key/preconditions?") {
		select {
		case <-output.notify:
		case <-ctx.Done():
			c.Wait()
			t.Fatal("retry prompt unavailable", output.String())
		}
	}
	if err := os.WriteFile(appPath, []byte("not the approved graph\n"), 0600); err != nil {
		t.Fatal(err)
	}
	io.WriteString(stdin, "y\n")
	if e := c.Wait(); e != nil {
		t.Fatalf("manual retry failed: %v %s", e, output.String())
	}
	stdin.Close()
	if plans != 1 || len(writes) != 2 || writes[0] != writes[1] || !strings.Contains(writes[0].body, `"membership_incarnation":"00000000-0000-4000-8000-000000000001"`) || strings.Contains(writes[0].body, "synthetic-private") || strings.Contains(output.String(), "synthetic-private") {
		t.Fatal("original operation not retained", plans, len(writes))
	}
	if !strings.Contains(writes[0].body, agentIntent) || !strings.Contains(writes[0].body, `"name":"Reviewed application"`) || !strings.Contains(writes[0].body, `"etag":"original-application-etag"`) || !strings.Contains(output.String(), "UPDATE application product") {
		t.Fatal("Application graph/read-set/review not retained")
	}
}

func TestExecutablePullInsideRootKeepsParentShell(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	root := filepath.Join(home, "stack")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("untouched"), 0600)
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/auth/identity":
			io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case "/v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","slug":"example_project","status":"active","name":"Safe"}`)
		case "/v0/projects/prj_0123456789ABCDEF/stack":
			io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[],"applications":[{"slug":"product","spec":{"name":"Product","models":{},"agents":{}}}]}`)
		case "/v0/projects/prj_0123456789ABCDEF/stack/plan":
			io.WriteString(w, `{"version":1,"read_set":{"resources":[],"bindings":[],"credentials":[],"membership_incarnation":"synthetic","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0},"changes":[]}`)
		case "/v0/projects/prj_0123456789ABCDEF/stack/apply":
			io.WriteString(w, `{"version":1,"changed":false,"resources":[]}`)
		default:
			t.Error("unexpected Stack read")
		}
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-ec", `"$1" link --project prj_0123456789ABCDEF; "$1" pull; test -f applications/product.yaml; "$1" plan; "$1" apply; "$1" pull --force; "$1" plan; test -f applications/product.yaml; test "$(cat notes.txt)" = untouched`, "--", bin)
	c.Dir = root
	c.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic", "AIMOUNT_API_URL="+s.URL)
	out, e := c.CombinedOutput()
	if e != nil {
		t.Fatal(e, string(out))
	}
	after, err := os.Stat(root)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("parent shell root changed", err)
	}
	b, e := os.ReadFile(filepath.Join(root, "notes.txt"))
	if e != nil || string(b) != "untouched" {
		t.Fatal("root altered")
	}
}

func TestExecutableApplicationPullAtomicPublication(t *testing.T) {
	bin := buildBinary(t)
	for _, mode := range []string{"canonical", "connected", "missing-agent", "previous-format", "managed-symlink", "local-edit", "legacy-root-name", "legacy-spec-display-name", "malformed-managed-yaml"} {
		t.Run(mode, func(t *testing.T) {
			publish := mode == "canonical" || mode == "connected" || mode == "local-edit"
			home := t.TempDir()
			root := filepath.Join(home, "stack")
			if err := os.MkdirAll(filepath.Join(root, "applications/examples"), 0750); err != nil {
				t.Fatal(err)
			}
			original := "version: 1\nkind: application\nslug: old\nspec: {name: Old, models: {}, agents: {}}\n"
			legacy := mode == "legacy-root-name" || mode == "legacy-spec-display-name"
			malformed := mode == "malformed-managed-yaml"
			if legacy || malformed {
				original = strings.Replace(original, "agents: {}}", "agents: {}, tool_endpoint: {from_env: CUSTOM_TOOLS_URL}}", 1)
				if mode == "legacy-root-name" {
					original = strings.Replace(original, "slug: old", "name: old", 1)
				} else if malformed {
					original = strings.Replace(original, "{name: Old", "{name: Old, name: Duplicate", 1)
				} else {
					original = strings.Replace(original, "{name: Old", "{display_name: Old", 1)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "applications/old.yaml"), []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{"notes.txt": "unmanaged\n", "project.yaml": "unmanaged project\n", "applications/examples/deeper.yaml": "unmanaged example\n", "applications/ignored.yml": "unmanaged extension\n"} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0640); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("notes.txt", filepath.Join(root, "relative-link")); err != nil {
				t.Fatal(err)
			}
			if mode == "managed-symlink" {
				if err := os.Symlink("old.yaml", filepath.Join(root, "applications/product.yaml")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0/auth/identity":
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case "/v0/projects/prj_0123456789ABCDEF":
					io.WriteString(w, `{"id":"prj_0123456789ABCDEF","slug":"example_project","status":"active","name":"Safe"}`)
				case "/v0/projects/prj_0123456789ABCDEF/stack/bindings":
					io.WriteString(w, `{"version":1,"bindings":[{"kind":"application","slug":"product","id":"app_0123456789ABCDEF"},{"kind":"application","slug":"old","id":"app_0123456789ABCDEF"}]}`)
				case "/v0/projects/prj_0123456789ABCDEF/stack":
					app, models, tools, agents := `"models":{},"agents":{}`, "[]", "[]", "[]"
					if mode == "connected" {
						app = `"models":{"fast":{"model":{"id":"mdl_0123456789ABCDEF","slug":"smart"}}},"agents":{"main":{"agent":{"id":"agt_0123456789ABCDEF","slug":"zulu"}},"helper":{"agent":{"slug":"zulu"}},"form":{"agent":{"slug":"alpha"},"models":["fast"],"storages":[{"id":"sto_0123456789ABCDEF","slug":"docs"}],"memory":"read_write"}},"plans":{"plus":{"display_name":"Plus","agents":{"main":{"input":false},"form":{"memory":"read"}},"limits":[{"agents":["helper","form"],"requests":5,"period":"1h"}],"users":["never-output"]}}`
						models = `[{"slug":"smart","spec":{"name":"Smart","limits":{"context_window_tokens":8192,"max_output_tokens":1024},"capabilities":{"input_modalities":["text"],"output_modalities":["text"],"tool_calling":false},"nodes":[{"name":"primary","display_name":"Primary","provider":"openai_compatible","api":"responses","base_url":"https://provider.example/v1","model":"test","credentials":{"mode":"none"}}]}}]`
						tools = `[{"slug":"lookup","spec":{"name":"Lookup Tool","enabled":true,"current_contract":"v1","contracts":[{"name":"v1","description":"Lookup","type":"client","input_schema":{"type":"object","additionalProperties":false},"output_schema":{}}]}}]`
						agents = `[{"slug":"zulu","spec":{"name":"Zulu Assistant","instruction":"Help","tools":[{"slug":"lookup","contract":"v1","id":"tol_0123456789ABCDEF"}]}},{"slug":"alpha","spec":{"name":"Alpha","instruction":"Help","tools":[{"slug":"lookup"}]}}]`
					}
					if legacy || malformed {
						app += `,"tool_endpoint":{"url_source":"env"}`
					}
					if mode == "missing-agent" {
						app = `"models":{},"agents":{"main":{"agent":{"slug":"missing"}}}`
					}
					if mode == "previous-format" {
						app = `"agents":[{"slug":"missing"}]`
					}
					if mode == "local-edit" {
						if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("edited during pull\n"), 0640); err != nil {
							t.Error(err)
						}
					}
					io.WriteString(w, `{"version":1,"models":`+models+`,"tools":`+tools+`,"agents":`+agents+`,"applications":[{"slug":"product","archived":true,"spec":{"name":"Product",`+app+`},"users":["never-output"],"runtime_key":"never-output"}]}`)
				default:
					t.Error("unexpected request", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, bin, "pull", "--force", "--root", root, "--api-url", s.URL, "--project", "prj_0123456789ABCDEF", "--output", "json")
			c.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic")
			out, err := c.CombinedOutput()
			if legacy {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "applications/old.yaml") || !strings.Contains(string(out), "root name to slug") || !strings.Contains(string(out), "spec.display_name to spec.name") || !strings.Contains(string(out), "from_env") {
					t.Fatal("expected manual rename diagnostic", err, string(out))
				}
			}
			if malformed {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "applications/old.yaml: invalid YAML") {
					t.Fatal("expected managed YAML diagnostic", err, string(out))
				}
			}
			if legacy || malformed {
				for _, path := range []string{"applications/product.yaml", ".aimount/pull"} {
					if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
						t.Fatal("refused pull published", path, err)
					}
				}
			}
			if publish && err != nil || !publish && err == nil || strings.Contains(string(out), "never-output") {
				t.Fatal("wrong atomic pull outcome", err, string(out))
			}
			after, err := os.Stat(root)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("root inode changed", err)
			}
			if publish {
				data, err := os.ReadFile(filepath.Join(root, "applications/product.yaml"))
				want := "version: 1\nkind: application\nslug: product\narchived: true\nspec:\n  name: Product\n  models: {}\n  agents: {}\n"
				if mode == "connected" {
					want = "version: 1\nkind: application\nslug: product\narchived: true\nspec:\n  name: Product\n  models:\n    fast:\n      model: smart\n  agents:\n    form:\n      agent: alpha\n      models:\n        - fast\n      storages:\n        - docs\n      memory: read_write\n    helper:\n      agent: zulu\n    main:\n      agent: zulu\n  plans:\n    plus:\n      display_name: Plus\n      agents:\n        form:\n          memory: read\n        main:\n          input: false\n      limits:\n        - agents:\n            - form\n            - helper\n          requests: 5\n          period: 1h\n"
				}
				if err != nil || string(data) != want {
					t.Fatal("noncanonical Application published", err, string(data))
				}
				if mode == "connected" {
					for file, want := range map[string]string{
						"agents/zulu/agent.yaml":  "version: 1\nkind: agent\nslug: zulu\nspec:\n  name: Zulu Assistant\n  tools:\n    - slug: lookup\n      contract: v1\n",
						"agents/alpha/agent.yaml": "version: 1\nkind: agent\nslug: alpha\nspec:\n  name: Alpha\n  tools:\n    - lookup\n",
					} {
						got, err := os.ReadFile(filepath.Join(root, file))
						if err != nil || string(got) != want {
							t.Fatal("noncanonical Agent/Tool reference published", file, err, string(got))
						}
					}
				}
				if _, err := os.Stat(filepath.Join(root, "applications/old.yaml")); !os.IsNotExist(err) {
					t.Fatal("old managed Application retained", err)
				}
			} else if data, err := os.ReadFile(filepath.Join(root, "applications/old.yaml")); err != nil || string(data) != original {
				t.Fatal("failed pull lost original Application", err)
			}
			notes := "unmanaged\n"
			if mode == "local-edit" {
				notes = "edited during pull\n"
			}
			for path, contents := range map[string]string{"notes.txt": notes, "project.yaml": "unmanaged project\n", "applications/examples/deeper.yaml": "unmanaged example\n", "applications/ignored.yml": "unmanaged extension\n"} {
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || string(data) != contents {
					t.Fatal("unmanaged path changed", path, err)
				}
			}
			if link, err := os.Readlink(filepath.Join(root, "relative-link")); err != nil || link != "notes.txt" {
				t.Fatal("unmanaged symlink changed", err)
			}
			if info, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("unmanaged mode changed", err)
			}
		})
	}
}

func TestExecutableLogoutCanonicalHostPreservesForeignPAT(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range [][2]string{{"https://[0:0:0:0:0:0:0:1]", "https://[::1]"}, {"https://[2001:0db8:0:0:0:0:0:1]", "https://[2001:db8::1]"}, {"https://%65xample.com", "https://example.com"}, {"https://%62%C3%BCcher.example", "https://xn--bcher-kva.example"}, {"http://127.1", "http://127.0.0.1"}, {"https://0177.0.0.1", "https://127.0.0.1"}} {
		for _, selection := range []string{"flag", "env", "config"} {
			t.Run(selection+"/"+tc[1], func(t *testing.T) {
				home := t.TempDir()
				dir := filepath.Join(home, "config", "aimount")
				if runtime.GOOS == "darwin" {
					dir = filepath.Join(home, "Library/Application Support/aimount")
				}
				if e := os.MkdirAll(dir, 0700); e != nil {
					t.Fatal(e)
				}
				before := map[string]any{"tokens": map[string]string{tc[1]: "selected-synthetic-PAT", "https://foreign.example": "foreign-synthetic-PAT"}}
				data, e := json.Marshal(before)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(dir, "auth.json"), data, 0600); e != nil {
					t.Fatal(e)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				args := []string{"auth", "logout", "--output", "json"}
				env := isolatedEnv(home)
				switch selection {
				case "flag":
					args = append(args, "--api-url", tc[0])
				case "env":
					env = append(env, "AIMOUNT_API_URL="+tc[0])
				case "config":
					config, _ := json.Marshal(map[string]string{"api_url": tc[0]})
					if e = os.WriteFile(filepath.Join(dir, "config.json"), config, 0600); e != nil {
						t.Fatal(e)
					}
				}
				c := exec.CommandContext(ctx, bin, args...)
				c.Env = env
				out, e := c.Output()
				rejected := strings.Contains(tc[0], "%") || strings.Contains(tc[0], "127.1") || strings.Contains(tc[0], "0177.")
				if rejected {
					var exit *exec.ExitError
					if !errors.As(e, &exit) || exit.ExitCode() != 1 {
						t.Fatalf("ambiguous endpoint not rejected: %v", e)
					}
					after, err := os.ReadFile(filepath.Join(dir, "auth.json"))
					if err != nil || !bytes.Equal(after, data) {
						t.Fatal("rejected endpoint changed authorization")
					}
					var envelope map[string]any
					if err := json.Unmarshal(out, &envelope); err != nil || envelope["ok"] != false {
						t.Fatalf("invalid rejection envelope: %s", out)
					}
					return
				}
				if e != nil {
					t.Fatal("canonical logout refused", e)
				}
				var result map[string]any
				if e = json.Unmarshal(out, &result); e != nil {
					t.Fatal(e)
				}
				value, ok := result["result"].(map[string]any)
				if !ok || result["ok"] != true || value["api"] != tc[1] || value["logged_out"] != true {
					t.Fatal("logout reported wrong canonical API")
				}
				data, e = os.ReadFile(filepath.Join(dir, "auth.json"))
				if e != nil {
					t.Fatal(e)
				}
				var after struct {
					Tokens map[string]string `json:"tokens"`
				}
				if e = json.Unmarshal(data, &after); e != nil {
					t.Fatal(e)
				}
				if len(after.Tokens) != 1 || after.Tokens["https://foreign.example"] != "foreign-synthetic-PAT" {
					t.Fatal("logout retained selected PAT or changed foreign entry")
				}
			})
		}
	}
}

func TestExecutableDeviceValidatedHTTPSAndCancelledInFlight(t *testing.T) {
	bin := buildBinary(t)
	for _, cancelFlight := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelFlight), func(t *testing.T) {
			home := t.TempDir()
			fake := filepath.Join(home, "bin")
			os.Mkdir(fake, 0700)
			pidPath := filepath.Join(home, "pid")
			for _, name := range []string{"open", "xdg-open"} {
				os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\nprintf '%s' \"$PPID\" > \"$CLI_TEST_PID\"\n"), 0700)
			}
			entered := make(chan struct{})
			cancels := 0
			token := "aim_pat_0123456789ABCDEF_" + strings.Repeat("A", 43)
			release := make(chan struct{})
			defer close(release)
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" && strings.HasPrefix(r.URL.Path, "/v0/oauth/") {
					t.Error("bearer on OAuth")
				}
				switch r.URL.Path {
				case "/v0/oauth/device/authorization":
					io.WriteString(w, `{"device_code":"`+strings.Repeat("b", 43)+`","user_code":"BCDF-GHJK-LMNP","expires_in":60,"interval":5,"verification_uri":"https://console.example/device","verification_uri_complete":"https://console.example/device?request_id=dvr_0123456789ABCDEF"}`)
				case "/v0/oauth/token":
					io.ReadAll(r.Body)
					close(entered)
					if cancelFlight {
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
					io.WriteString(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"`+token+`"}`)
				case "/v0/oauth/device/cancel":
					cancels++
					io.WriteString(w, `{}`)
				case "/v0/auth/identity":
					io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case "/v0/organizations":
					io.WriteString(w, `{"items":[],"next_cursor":null}`)
				default:
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			ca := filepath.Join(home, "ca.pem")
			os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := ttyCommand(ctx, bin, "auth", "login", "--api-url", s.URL)
			c.Env = isolatedEnv(home, "PATH="+fake+":"+os.Getenv("PATH"), "SSL_CERT_FILE="+ca, "CLI_TEST_PID="+pidPath)
			var out bytes.Buffer
			c.Stdout = &out
			c.Stderr = &out
			if e := c.Start(); e != nil {
				t.Fatal(e)
			}
			if cancelFlight {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("token did not start")
				}
				b, e := os.ReadFile(pidPath)
				if e != nil {
					t.Fatal(e)
				}
				pid, e := strconv.Atoi(string(b))
				if e != nil {
					t.Fatal(e)
				}
				proc, e := os.FindProcess(pid)
				if e != nil {
					t.Fatal(e)
				}
				if e = proc.Signal(os.Interrupt); e != nil {
					t.Fatal(e)
				}
			}
			e := c.Wait()
			if cancelFlight && e == nil {
				t.Fatal("cancel succeeded")
			}
			if !cancelFlight && e != nil {
				t.Fatalf("login failed: %v %s", e, out.Bytes())
			}
			if strings.Contains(out.String(), token) {
				t.Fatal("token disclosed")
			}
			dir := filepath.Join(home, ".config", "aimount")
			if runtime.GOOS == "darwin" {
				dir = filepath.Join(home, "Library/Application Support/aimount")
			} else {
				dir = filepath.Join(home, "config", "aimount")
			}
			b, e := os.ReadFile(filepath.Join(dir, "auth.json"))
			if cancelFlight {
				if !os.IsNotExist(e) || cancels != 1 {
					t.Fatal("cancel published or did not cancel once", e, cancels)
				}
			} else {
				if e != nil || !bytes.Contains(b, []byte(token)) {
					t.Fatal("PAT not saved", e)
				}
				for _, action := range []string{"status", "logout"} {
					p := exec.CommandContext(ctx, bin, "auth", action, "--api-url", s.URL, "--output", "json")
					p.Env = c.Env
					raw, e := p.Output()
					if e != nil {
						t.Fatal(action, e, string(raw))
					}
					var v map[string]any
					if json.Unmarshal(raw, &v) != nil || v["ok"] != true {
						t.Fatal("bad auth result")
					}
				}
			}
		})
	}
}

func TestExecutableScopedDeviceLogin(t *testing.T) {
	bin := buildBinary(t)
	for _, scenario := range []string{"missing-grant", "same-user", "different-user", "first-login", "expired", "logout", "other-login"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			fake := filepath.Join(home, "bin")
			if err := os.Mkdir(fake, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"open", "xdg-open"} {
				if err := os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			const oldPAT = "old-synthetic-PAT"
			newPAT := "aim_pat_0123456789ABCDEF_" + strings.Repeat("A", 43)
			otherPAT := "aim_pat_0000000000000001_" + strings.Repeat("B", 43)
			var authorizations atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0/auth/identity":
					user, scope := "usr_0123456789ABCDEF", ""
					switch r.Header.Get("Authorization") {
					case "Bearer " + otherPAT:
						if scenario != "other-login" {
							t.Error("unexpected concurrent PAT")
						}
						scope = `,"application_credentials":true`
					case "Bearer " + oldPAT:
						if scenario == "expired" {
							w.WriteHeader(401)
							io.WriteString(w, `{"error":{"code":"invalid_credential"}}`)
							return
						}
					case "Bearer " + newPAT:
						scope = `,"application_credentials":true,"unknown":"` + newPAT + `"`
						if scenario == "different-user" {
							user = "usr_0000000000000001"
						}
					default:
						t.Error("wrong PAT checked")
					}
					io.WriteString(w, `{"principal":{"type":"user","user_id":"`+user+`"},"authentication":{"method":"pat"`+scope+`}}`)
				case "/v0/oauth/device/authorization":
					n := authorizations.Add(1)
					if r.FormValue("scope") != "application_credentials" || r.FormValue("client_id") != "aimount-app-cli" {
						t.Error("scope not requested with Device authorization")
					}
					code := strings.Repeat("b", 43)
					if n == 2 && scenario == "other-login" {
						code = strings.Repeat("c", 43)
					}
					io.WriteString(w, `{"device_code":"`+code+`","user_code":"BCDF-GHJK-LMNP","expires_in":60,"interval":5,"verification_uri":"https://console.example/device","verification_uri_complete":"https://console.example/device?request_id=dvr_0123456789ABCDEF"}`)
				case "/v0/oauth/token":
					if r.FormValue("scope") != "" || r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
						t.Error("wrong token polling parameters")
					}
					pat := newPAT
					if scenario == "other-login" && r.FormValue("device_code") == strings.Repeat("c", 43) {
						pat = otherPAT
					} else if scenario == "logout" || scenario == "other-login" {
						close(entered)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					scope := ""
					if scenario != "missing-grant" {
						scope = `,"scope":"application_credentials"`
					}
					io.WriteString(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"`+pat+`"`+scope+`}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			ca := filepath.Join(home, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, "config", "aimount")
			if runtime.GOOS == "darwin" {
				dir = filepath.Join(home, "Library/Application Support/aimount")
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "auth.json")
			config := filepath.Join(dir, "config.json")
			configBefore := []byte(`{"contexts":{"` + s.URL + `":{"user_id":"usr_0123456789ABCDEF","organization_id":"org_0123456789ABCDEF"}}}`)
			if scenario != "first-login" {
				if err := os.WriteFile(path, []byte(`{"tokens":{"`+s.URL+`":"`+oldPAT+`","https://another.example":"other-api"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, configBefore, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			env := isolatedEnv(home, "PATH="+fake+":"+os.Getenv("PATH"), "SSL_CERT_FILE="+ca)
			if scenario == "same-user" {
				plain := exec.CommandContext(ctx, bin, "auth", "login", "--api-url", s.URL, "--output=json")
				plain.Env = env
				if _, err := plain.CombinedOutput(); err != nil || authorizations.Load() != 0 {
					t.Fatal("plain login started new authorization despite a saved PAT", err)
				}
			}
			scopedLogin := func(ctx context.Context) *exec.Cmd {
				cmd := ttyCommand(ctx, bin, "auth", "login", "--application-credentials", "--api-url", s.URL)
				cmd.Env = env
				return cmd
			}
			optIn := scopedLogin(ctx)
			var output bytes.Buffer
			optIn.Stdout, optIn.Stderr = &output, &output
			if err := optIn.Start(); err != nil {
				t.Fatal(err)
			}
			defer optIn.Wait()
			wantAuthorizations := int32(1)
			if scenario == "logout" || scenario == "other-login" {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("first token poll did not arrive")
				}
				if scenario == "logout" {
					logout := exec.CommandContext(ctx, bin, "auth", "logout", "--api-url", s.URL)
					logout.Env = env
					if err := logout.Run(); err != nil {
						close(release)
						t.Fatal("concurrent logout failed", err)
					}
				} else {
					wantAuthorizations++
					if out, err := scopedLogin(ctx).CombinedOutput(); err != nil || bytes.Contains(out, []byte(otherPAT)) {
						close(release)
						t.Fatal("concurrent login failed or disclosed PAT", err)
					}
				}
				close(release)
			}
			err := optIn.Wait()
			out := output.Bytes()
			if authorizations.Load() != wantAuthorizations || bytes.Contains(out, []byte(newPAT)) || bytes.Contains(out, []byte(oldPAT)) || bytes.Contains(out, []byte("Choose Organization")) {
				t.Fatal("unexpected scoped authorization count/prompt or disclosed a PAT")
			}
			configAfter, configErr := os.ReadFile(config)
			if scenario == "first-login" {
				if !os.IsNotExist(configErr) {
					t.Fatal("scoped first login created Organization context", configErr)
				}
			} else if configErr != nil || !bytes.Equal(configAfter, configBefore) {
				t.Fatal("scoped login changed Organization context", configErr)
			}
			after, readErr := os.ReadFile(path)
			var auth struct{ Tokens map[string]string }
			if readErr != nil || json.Unmarshal(after, &auth) != nil || (scenario != "first-login" && auth.Tokens["https://another.example"] != "other-api") {
				t.Fatal("auth state unreadable or lost another API's authorization", readErr)
			}
			wantToken, wantError := newPAT, ""
			switch scenario {
			case "missing-grant":
				wantToken, wantError = oldPAT, "did not grant Application credential consent"
			case "different-user":
				wantToken, wantError = oldPAT, "different account"
			case "logout":
				wantToken, wantError = "", "Saved authorization changed"
			case "other-login":
				wantToken, wantError = otherPAT, "Saved authorization changed"
			}
			if auth.Tokens[s.URL] != wantToken || (err != nil) != (wantError != "") || !bytes.Contains(out, []byte(wantError)) {
				t.Fatal("wrong scoped login result or saved authorization", err)
			}
			if err != nil {
				return
			}
			status := exec.CommandContext(ctx, bin, "auth", "status", "--api-url", s.URL, "--output=json")
			status.Env = env
			out, err = status.CombinedOutput()
			if err != nil || !bytes.Contains(out, []byte(`"application_credentials":true`)) || bytes.Contains(out, []byte(newPAT)) {
				t.Fatal("status did not safely project opt-in", err)
			}
		})
	}
}

func TestExecutableApplicationCredentials(t *testing.T) {
	bin := buildBinary(t)
	const project = "prj_0123456789ABCDEF"
	const app = "app_0123456789ABCDEF"
	const id = "0000000000000002"
	const stamp = "2026-11-01T00:00:00Z"
	const base = "/v0/projects/" + project
	secret := strings.Repeat("A", 43)
	for _, tc := range []struct {
		name, group, scenario, expectedCode string
	}{
		{"runtime", "runtime-keys", "success", ""},
		{"webhook", "webhook-secrets", "success", ""},
		{"scope", "runtime-keys", "unscoped", "application_credentials_required"},
		{"app-guard", "runtime-keys", "guard", "application_changed"},
		{"symlink", "runtime-keys", "symlink", "credential_file_exists"},
		{"pending-symlink", "runtime-keys", "pending-symlink", "credential_file_pending"},
		{"refusal", "webhook-secrets", "refusal", "tool_secret_limit"},
		{"server-error", "webhook-secrets", "server-error", "credential_create_unknown"},
		{"lost-response", "webhook-secrets", "lost-response", "credential_create_unknown"},
		{"malformed", "runtime-keys", "wrong-target", "credential_create_unknown"},
		{"race", "webhook-secrets", "race", "credential_file_unconfirmed"},
		{"lost-stage", "runtime-keys", "lost-stage", "credential_file_unconfirmed"},
		{"receipt-blocked", "webhook-secrets", "receipt-blocked", "credential_file_unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			final := filepath.Join(home, "secret")
			marker := final + ".pending"
			if tc.scenario == "symlink" || tc.scenario == "pending-symlink" {
				which := final
				if tc.scenario == "pending-symlink" {
					which = marker
				}
				if err := os.Symlink(filepath.Join(home, "missing"), which); err != nil {
					t.Fatal(err)
				}
			}
			route := base + "/applications/" + app + "/tool-secrets"
			value := "whsec_" + secret
			runtimeKey := tc.group == "runtime-keys"
			if runtimeKey {
				route = base + "/applications/" + app + "/runtime-credentials"
				value = "aim_runtime_" + id + "_" + secret
			}
			meta := `{"id":"` + id + `","created_at":"` + stamp + `","application_id":"` + app + `","name":"local_backend","future_hash":"` + value + `"}`
			if tc.scenario == "wrong-target" {
				meta = strings.Replace(meta, app, "app_0000000000000001", 1)
			}
			var posts atomic.Int32
			var lists atomic.Int32
			var revokes atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-PAT" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("If-Match") != "" {
					t.Error("unexpected auth or mutation headers")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /v0/auth/identity":
					scope := `,"application_credentials":true`
					if tc.scenario == "unscoped" {
						scope = ""
					}
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"`+scope+`}}`)
				case "GET " + base:
					io.WriteString(w, `{"id":"`+project+`","slug":"example","name":"Example","status":"active"}`)
				case "GET " + base + "/stack/bindings":
					io.WriteString(w, `{"version":1,"bindings":[{"kind":"application","slug":"paperdesk","id":"`+app+`"}]}`)
				case "POST " + route:
					posts.Add(1)
					body, _ := io.ReadAll(r.Body)
					if runtimeKey && (string(body) != `{"name":"local_backend"}` || r.Header.Get("Content-Type") != "application/json") || !runtimeKey && len(body) != 0 {
						t.Error("wrong credential create wire body")
					}
					info, err := os.Stat(marker)
					if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
						t.Error("owner-only pending marker absent at POST", err)
					}
					if _, err := os.Lstat(final); !os.IsNotExist(err) {
						t.Error("final visible before file publication")
					}
					switch tc.scenario {
					case "race":
						close(entered)
						<-release
					case "lost-stage":
						if err := os.Remove(filepath.Join(marker, "value")); err != nil {
							t.Error("cannot simulate disappearing staging file", err)
						}
					case "receipt-blocked":
						if err := os.Mkdir(filepath.Join(marker, "receipt.json"), 0700); err != nil {
							t.Error("cannot simulate receipt creation failure", err)
						}
					case "refusal":
						w.WriteHeader(409)
						io.WriteString(w, `{"error":{"code":"tool_secret_limit","message":"`+value+`"}}`)
						return
					case "server-error":
						w.WriteHeader(503)
						io.WriteString(w, `{"error":{"code":"`+strings.ToLower(value)+`"}}`)
						return
					case "lost-response":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
						} else {
							conn.Close()
						}
						return
					}
					w.WriteHeader(201)
					if runtimeKey {
						io.WriteString(w, `{"credential":`+meta+`,"token":"`+value+`"}`)
					} else {
						io.WriteString(w, strings.TrimSuffix(meta, "}")+`,"secret":"`+value+`"}`)
					}
				case "GET " + route:
					lists.Add(1)
					if runtimeKey && r.URL.RawQuery == "" {
						io.WriteString(w, `{"items":[],"next_cursor":"synthetic+/"}`)
						return
					}
					if runtimeKey && r.URL.Query().Get("cursor") != "synthetic+/" {
						t.Error("runtime key list did not advance cursor")
					}
					io.WriteString(w, `{"items":[`+meta+`],"next_cursor":null}`)
				case "DELETE " + route + "/" + id:
					revokes.Add(1)
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			expected := app
			if tc.scenario == "guard" {
				expected = "app_0000000000000001"
			}
			command := func(action string, extra ...string) *exec.Cmd {
				args := append([]string{tc.group, action, "--output=json", "--api-url", s.URL, "--project", project, "--application", "paperdesk", "--expect-application-id", expected}, extra...)
				cmd := exec.CommandContext(ctx, bin, args...)
				cmd.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic-PAT")
				return cmd
			}
			parse := func(raw []byte, err error, action, expectedCode string) map[string]any {
				t.Helper()
				var envelope struct {
					Command string         `json:"command"`
					Result  map[string]any `json:"result"`
					Error   struct {
						Code, Message string
						Details       map[string]any
					} `json:"error"`
				}
				if bytes.Contains(bytes.ToLower(raw), []byte(strings.ToLower(value))) || bytes.Contains(raw, []byte("future_hash")) || bytes.Contains(raw, []byte("synthetic-PAT")) {
					t.Fatal("credential leaked in process output")
				}
				if json.Unmarshal(raw, &envelope) != nil || envelope.Command != tc.group+" "+action || envelope.Error.Code != expectedCode || (err != nil) != (expectedCode != "") {
					t.Fatalf("wrong %s envelope: %s, %v", action, raw, err)
				}
				switch expectedCode {
				case "credential_file_unconfirmed":
					if len(envelope.Error.Details) != 2 || envelope.Error.Details["id"] != id || envelope.Error.Details["application_id"] != app || !strings.Contains(envelope.Error.Message, id) || !strings.Contains(envelope.Error.Message, app) {
						t.Fatal("known issuance did not report exact safe IDs")
					}
				case "credential_create_unknown":
					if len(envelope.Error.Details) != 0 || strings.Contains(envelope.Error.Message, id) {
						t.Fatal("unknown issuance claimed a credential ID")
					}
				}
				return envelope.Result
			}
			args := []string{"--secret-file", final}
			if runtimeKey {
				args = append(args, "--name", "local_backend")
			}
			cmd := command("create", args...)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if tc.scenario == "race" {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("POST did not arrive")
				}
				raw, err := command("create", args...).CombinedOutput()
				parse(raw, err, "create", "credential_file_pending")
				if err := os.WriteFile(final, []byte("do not replace"), 0600); err != nil {
					t.Fatal(err)
				}
				close(release)
			}
			err := cmd.Wait()
			result := parse(out.Bytes(), err, "create", tc.expectedCode)
			expectedPosts := int32(1)
			switch tc.scenario {
			case "unscoped", "guard", "symlink", "pending-symlink":
				expectedPosts = 0
			}
			if posts.Load() != expectedPosts {
				t.Fatalf("unexpected POST count: %d", posts.Load())
			}
			_, err = os.Lstat(marker)
			if (err == nil) != (tc.expectedCode == "credential_create_unknown" || tc.expectedCode == "credential_file_unconfirmed" || tc.scenario == "pending-symlink") {
				t.Fatal("wrong pending marker state", err)
			}
			receiptPath := filepath.Join(marker, "receipt.json")
			switch tc.scenario {
			case "race", "lost-stage":
				receipt, err := os.ReadFile(receiptPath)
				info, statErr := os.Stat(receiptPath)
				var fields map[string]string
				if err != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || json.Unmarshal(receipt, &fields) != nil || len(fields) != 2 || fields["id"] != id || fields["application_id"] != app {
					t.Fatal("receipt does not contain only the validated metadata in a private file", err, statErr)
				}
			case "receipt-blocked":
				staged, err := os.ReadFile(filepath.Join(marker, "value"))
				if err != nil || len(staged) != 0 {
					t.Fatal("secret staging started before receipt storage", err)
				}
			default:
				if _, err := os.Lstat(receiptPath); tc.expectedCode == "credential_create_unknown" && !os.IsNotExist(err) {
					t.Fatal("unknown issuance produced a trusted receipt", err)
				}
			}
			if tc.scenario == "receipt-blocked" || tc.scenario == "lost-stage" {
				if _, err := os.Lstat(final); !os.IsNotExist(err) {
					t.Fatal("secret published despite local file failure", err)
				}
			}
			if tc.expectedCode == "credential_create_unknown" || tc.expectedCode == "credential_file_unconfirmed" {
				raw, err := command("create", args...).CombinedOutput()
				parse(raw, err, "create", "credential_file_pending")
				if posts.Load() != expectedPosts {
					t.Fatal("unreconciled marker allowed another POST")
				}
			}
			if tc.scenario == "race" {
				b, err := os.ReadFile(final)
				if err != nil || string(b) != "do not replace" {
					t.Fatal("existing entry overwritten", err)
				}
			}
			if tc.scenario == "success" {
				b, err := os.ReadFile(final)
				info, statErr := os.Lstat(final)
				if err != nil || statErr != nil || string(b) != value || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatal("secret file missing, wrong, or not owner-only", err, statErr)
				}
				if result["id"] != id || result["application_id"] != app {
					t.Fatal("create metadata missing")
				}
				raw, err := command("list").CombinedOutput()
				items, _ := parse(raw, err, "list", "")["items"].([]any)
				if len(items) != 1 || items[0].(map[string]any)["id"] != id || lists.Load() != map[bool]int32{true: 2, false: 1}[runtimeKey] {
					t.Fatal("wrong list results")
				}
				raw, err = command("revoke", id).CombinedOutput()
				result = parse(raw, err, "revoke", "")
				if result["id"] != id || result["revoked"] != true || revokes.Load() != 1 {
					t.Fatal("exact-ID revoke not acknowledged")
				}
			}
		})
	}
}

type processOutput struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notify chan struct{}
}

func (p *processOutput) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, e := p.buf.Write(b)
	select {
	case p.notify <- struct{}{}:
	default:
	}
	return n, e
}
func (p *processOutput) String() string { p.mu.Lock(); defer p.mu.Unlock(); return p.buf.String() }
