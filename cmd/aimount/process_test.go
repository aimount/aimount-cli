package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
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
	bin := filepath.Join(t.TempDir(), "aimount")
	if out, e := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", out, e)
	}
	return bin
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
			io.WriteString(w, `{"version":1,"bindings":[]}`)
		case strings.HasSuffix(r.URL.Path, "/stack/plan"):
			plans++
			io.WriteString(w, `{"version":1,"read_set":{"resources":[],"bindings":[],"membership_incarnation":"00000000-0000-4000-8000-000000000001","membership_version":1,"membership_access_epoch":0,"project_lifecycle_epoch":0,"credentials":[],"future_secret":"synthetic-private"},"changes":[{"kind":"model","name":"smart","operation":"CREATE"}]}`)
		case strings.HasSuffix(r.URL.Path, "/stack/apply"):
			b, _ := io.ReadAll(r.Body)
			writes = append(writes, captured{r.URL.Path, string(b), r.Header.Get("Idempotency-Key")})
			if len(writes) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"code":"service_unavailable"}}`)
			} else {
				io.WriteString(w, `{"version":1,"changed":true,"resources":[]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/stack"):
			io.WriteString(w, `{"version":1,"models":[],"tools":[],"agents":[]}`)
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
	io.WriteString(stdin, "y\n")
	if e := c.Wait(); e != nil {
		t.Fatalf("manual retry failed: %v %s", e, output.String())
	}
	stdin.Close()
	if plans != 1 || len(writes) != 2 || writes[0] != writes[1] || !strings.Contains(writes[0].body, `"membership_incarnation":"00000000-0000-4000-8000-000000000001"`) || strings.Contains(writes[0].body, "synthetic-private") || strings.Contains(output.String(), "synthetic-private") {
		t.Fatal("original operation not retained", plans, len(writes))
	}
}

func TestExecutablePullInsideRootRefuses(t *testing.T) {
	bin := buildBinary(t)
	home := t.TempDir()
	root := filepath.Join(home, "stack")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("untouched"), 0600)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/auth/identity":
			io.WriteString(w, `{"principal":{"type":"user","user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
		case "/v0/projects/prj_0123456789ABCDEF":
			io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active","display_name":"Safe"}`)
		default:
			t.Error("unexpected Stack read")
		}
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "pull", "--api-url", s.URL, "--project", "prj_0123456789ABCDEF")
	c.Dir = root
	c.Env = isolatedEnv(home, "AIMOUNT_TOKEN=synthetic")
	out, e := c.CombinedOutput()
	if e == nil || !strings.Contains(string(out), "outside root") {
		t.Fatal(e, string(out))
	}
	b, e := os.ReadFile(filepath.Join(root, "notes.txt"))
	if e != nil || string(b) != "untouched" {
		t.Fatal("root altered")
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
