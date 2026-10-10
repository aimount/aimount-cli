package cli

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerificationURLs(t *testing.T) {
	valid := object{"verification_uri": "https://console.example/device", "verification_uri_complete": "https://console.example/device?request_id=dvr_0000000000000001"}
	if _, _, e := verificationURIs(valid); e != nil {
		t.Fatal(e)
	}
	for _, u := range []string{"https://other.example/device?request_id=dvr_0000000000000001", "https://console.example/device?device_code=secret", "http://localhost/device?request_id=dvr_0000000000000001", "https://console.example/device?request_id=dvr_0000000000000001&redirect=x"} {
		m := maps.Clone(valid)
		m["verification_uri_complete"] = u
		if _, _, e := verificationURIs(m); e == nil {
			t.Fatal("bad verification URL accepted")
		}
	}
}

func TestDeviceFormBoundsBodiesAndRefusesRedirects(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{{307, "", "redirect refused"}, {200, strings.Repeat("x", 32769), "invalid Device response"}, {200, "{broken", "invalid Device response"}, {200, "{\"error\":\"private-payload\"}", ""}} {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("invalid OAuth headers")
			}
			w.Header().Set("Location", "/different")
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		h := boundedHTTP()
		_, _, e := deviceForm(h, s.URL)(context.Background(), "/oauth", map[string]string{"client_id": "aimount-app-cli"}, time.Second)
		h.CloseIdleConnections()
		s.Close()
		if tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
			t.Fatal(e)
		}
		if e != nil && strings.Contains(e.Error(), "private-payload") {
			t.Fatal("raw response disclosed")
		}
		if calls != 1 {
			t.Fatal("OAuth retried", calls)
		}
	}
}

func TestDeviceCompletionRelativeBackoffAndCancellation(t *testing.T) {
	var now time.Duration
	starts := []time.Duration{}
	calls := 0
	clock := deviceClock{now: func() time.Duration { return now }, wait: func(ctx context.Context, d time.Duration) error { now += d; return ctx.Err() }, request: func(ctx context.Context, path string, values map[string]string, timeout time.Duration) (int, object, error) {
		starts = append(starts, now)
		now += 2 * time.Second
		calls++
		switch calls {
		case 1:
			return 0, nil, errors.New("transport unavailable")
		case 2, 4:
			return 400, object{"error": "authorization_pending"}, nil
		case 3:
			return 400, object{"error": "slow_down"}, nil
		default:
			return 200, object{"token_type": "Bearer", "expires_in": 10, "access_token": "aim_pat_0000000000000001_" + strings.Repeat("A", 43)}, nil
		}
	}}
	_, e := exchange(context.Background(), object{"device_code": strings.Repeat("A", 43), "interval": 5, "expires_in": 600}, clock)
	if e != nil {
		t.Fatal(e)
	}
	want := []time.Duration{5, 17, 24, 36, 48}
	for i, s := range starts {
		if s != want[i]*time.Second {
			t.Fatal(starts)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	now = 0
	clock.request = func(ctx context.Context, path string, v map[string]string, d time.Duration) (int, object, error) {
		calls++
		if strings.HasSuffix(path, "cancel") {
			if d != 3*time.Second {
				t.Fatal(d)
			}
			return 200, object{}, nil
		}
		cancel()
		return 200, object{"token_type": "Bearer", "expires_in": 10, "access_token": "aim_pat_0000000000000001_" + strings.Repeat("A", 43)}, nil
	}
	_, e = exchange(ctx, object{"device_code": "a", "interval": 5, "expires_in": 60}, clock)
	c, ok := e.(*CommandError)
	if !ok || c.Exit != 3 || calls != 2 {
		t.Fatal(e, calls)
	}
}
