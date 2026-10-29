package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlanAndApplyUseServerPlannerWithoutPrefetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, command := range []string{"plan", "apply"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			// Shape is valid; the missing dependency is interpreted by the API, not a preliminary export.
			writeFiles(t, root, map[string]string{"applications/product.yaml": strings.Replace(applicationYAML, "agents: []", "agents: [{name: alias}]", 1)})
			plans, prefetches := 0, 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case req.URL.Path == "/v0/auth/identity":
					io.WriteString(w, `{"principal":{"user_id":"usr_0123456789ABCDEF"},"authentication":{"method":"pat"}}`)
				case req.URL.Path == "/v0/projects/prj_0123456789ABCDEF":
					io.WriteString(w, `{"id":"prj_0123456789ABCDEF","status":"active"}`)
				case strings.HasSuffix(req.URL.Path, "/stack/plan"):
					plans++
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"code":"validation_failed"}}`)
				default:
					prefetches++
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer s.Close()
			r := runner{ctx: context.Background(), env: map[string]string{"AIMOUNT_TOKEN": "synthetic"}, errOut: io.Discard}
			_, err := r.run(Options{Command: command, Root: root, API: s.URL, Project: "prj_0123456789ABCDEF", NonInteractive: true})
			if api, ok := err.(*APIError); !ok || api.Code != "validation_failed" || plans != 1 || prefetches != 0 {
				t.Fatalf("wrong planning boundary: err=%v plans=%d other requests=%d", err, plans, prefetches)
			}
		})
	}
}
