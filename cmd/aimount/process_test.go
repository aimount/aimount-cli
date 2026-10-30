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
				if err := os.WriteFile(filepath.Join(root, "applications/product.yaml"), []byte("version: 1\nkind: application\nname: product\nspec:\n  display_name: Product\n  agents: []\n"), 0600); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v0/auth/identity":
						io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
					case "/v0/projects/prj_0123456789ABCDEF":
						io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active"}`)
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

func TestExecutableManualExactApplyRetry(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	root := filepath.Join(home, "stack")
	os.MkdirAll(filepath.Join(root, "models"), 0700)
	os.WriteFile(filepath.Join(root, "models/smart.yaml"), []byte("version: 1\nkind: model\nname: smart\nspec:\n  display_name: Smart\n  limits: {context_window_tokens: 8192, max_output_tokens: 1024}\n  capabilities: {input_modalities: [text], output_modalities: [text], tool_calling: false}\n  nodes:\n    - name: primary\n      display_name: Primary\n      provider: openai_compatible\n      api: responses\n      base_url: https://provider.example/v1\n      model: test\n      credentials: {none: {}}\n"), 0600)
	os.MkdirAll(filepath.Join(root, "applications"), 0700)
	appPath := filepath.Join(root, "applications/renamed.yaml")
	os.WriteFile(appPath, []byte("version: 1\nkind: application\nname: product\nspec:\n  display_name: Reviewed application\n  agents: []\n"), 0600)
	plans := 0
	type captured struct{ path, body, key string }
	writes := []captured{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/auth/identity":
			io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case r.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
		case strings.HasSuffix(r.URL.Path, "/stack/bindings"):
			io.WriteString(w, `{"version":1,"bindings":[{"kind":"application","name":"product","id":"app_0123456789ABCDEF"}]}`)
		case strings.HasSuffix(r.URL.Path, "/stack/plan"):
			plans++
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"applications":[{"archived":false,"name":"product","spec":{"agents":[],"display_name":"Reviewed application"}}]`) {
				t.Error("plan lost complete Application graph")
			}
			io.WriteString(w, `{"version":1,"read_set":{"resources":[{"kind":"application","id":"app_0123456789ABCDEF","etag":"original-application-etag"}],"bindings":[{"kind":"application","name":"product","id":"app_0123456789ABCDEF"}],"membership_incarnation":"00000000-0000-4000-8000-000000000001","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0,"credentials":[],"future_secret":"synthetic-private"},"changes":[{"kind":"model","name":"smart","operation":"CREATE"},{"kind":"application","name":"product","operation":"UPDATE","before":{"display_name":"Saved application","agents":[]},"after":{"display_name":"Reviewed application","agents":[]}}]}`)
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
			io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[],"applications":[{"name":"product","spec":{"display_name":"Saved application","agents":[]}}]}`)
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
	if !strings.Contains(writes[0].body, `"display_name":"Reviewed application"`) || !strings.Contains(writes[0].body, `"etag":"original-application-etag"`) || !strings.Contains(output.String(), "UPDATE application product") {
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
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
		case "/v0/projects/prj_0123456789ABCDEF/stack":
			io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[],"applications":[{"name":"product","spec":{"display_name":"Product","agents":[]}}]}`)
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
	for _, mode := range []string{"canonical", "connected", "missing-agent", "duplicate-agent", "managed-symlink", "local-edit"} {
		t.Run(mode, func(t *testing.T) {
			publish := mode == "canonical" || mode == "connected" || mode == "local-edit"
			home := t.TempDir()
			root := filepath.Join(home, "stack")
			if err := os.MkdirAll(filepath.Join(root, "applications/examples"), 0750); err != nil {
				t.Fatal(err)
			}
			original := "version: 1\nkind: application\nname: old\nspec: {display_name: Old, agents: []}\n"
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
					io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
				case "/v0/projects/prj_0123456789ABCDEF/stack":
					refs, models, agents := "[]", "[]", "[]"
					if mode == "connected" {
						refs = `[{"name":"zulu"},{"name":"alpha"}]`
						models = `[{"name":"smart","spec":{"display_name":"Smart","limits":{"context_window_tokens":8192,"max_output_tokens":1024},"capabilities":{"input_modalities":["text"],"output_modalities":["text"],"tool_calling":false},"nodes":[{"name":"primary","display_name":"Primary","provider":"openai_compatible","api":"responses","base_url":"https://provider.example/v1","model":"test","credentials":{"mode":"none"}}]}}]`
						agents = `[{"name":"zulu","spec":{"display_name":"Zulu","instruction":"Help","models":[{"name":"smart"}],"tools":[],"policies":[]}},{"name":"alpha","spec":{"display_name":"Alpha","instruction":"Help","models":[{"name":"smart"}],"tools":[],"policies":[]}}]`
					}
					if mode == "missing-agent" {
						refs = `[{"name":"missing"}]`
					}
					if mode == "duplicate-agent" {
						refs = `[{"name":"missing"},{"name":"missing"}]`
					}
					if mode == "local-edit" {
						if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("edited during pull\n"), 0640); err != nil {
							t.Error(err)
						}
					}
					io.WriteString(w, `{"version":1,"models":`+models+`,"tools":[],"agents":`+agents+`,"applications":[{"name":"product","archived":true,"spec":{"display_name":"Product","agents":`+refs+`},"users":["never-output"],"runtime_key":"never-output"}]}`)
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
			if publish && err != nil || !publish && err == nil || strings.Contains(string(out), "never-output") {
				t.Fatal("wrong atomic pull outcome", err, string(out))
			}
			after, err := os.Stat(root)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("root inode changed", err)
			}
			if publish {
				data, err := os.ReadFile(filepath.Join(root, "applications/product.yaml"))
				want := "version: 1\nkind: application\nname: product\narchived: true\nspec:\n  display_name: Product\n  agents: []\n"
				if mode == "connected" {
					want = strings.Replace(want, "agents: []", "agents:\n    - name: alpha\n    - name: zulu", 1)
				}
				if err != nil || string(data) != want {
					t.Fatal("noncanonical Application published", err, string(data))
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
					io.WriteString(w, `{"items":[]}`)
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
