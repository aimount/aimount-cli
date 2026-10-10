package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
)

func verificationURIs(v object) (string, string, error) {
	bad := func() (string, string, error) { return "", "", errors.New("invalid verification URLs") }
	a, e := url.Parse(str(v["verification_uri"]))
	if e != nil {
		return bad()
	}
	b, e := url.Parse(str(v["verification_uri_complete"]))
	if e != nil {
		return bad()
	}
	for _, u := range []*url.URL{a, b} {
		if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(u.String(), "\\") || strings.IndexFunc(u.String(), unicode.IsSpace) >= 0 {
			return bad()
		}
	}
	q, e := url.ParseQuery(b.RawQuery)
	if e != nil || a.RawQuery != "" || a.ForceQuery || strings.Contains(a.String(), "#") || strings.Contains(b.String(), "#") || !strings.EqualFold(a.Host, b.Host) || a.EscapedPath() != b.EscapedPath() || len(q) != 1 || len(q["request_id"]) != 1 || !idValid(q.Get("request_id"), "dvr") {
		return bad()
	}
	return a.String(), b.String(), nil
}

type deviceClock struct {
	now     func() time.Duration
	wait    func(context.Context, time.Duration) error
	request func(context.Context, string, map[string]string, time.Duration) (int, object, error)
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func deviceForm(h *http.Client, api string) func(context.Context, string, map[string]string, time.Duration) (int, object, error) {
	return func(ctx context.Context, path string, values map[string]string, timeout time.Duration) (int, object, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		form := url.Values{}
		for k, v := range values {
			form.Set(k, v)
		}
		r, e := http.NewRequestWithContext(ctx, "POST", api+path, strings.NewReader(form.Encode()))
		if e != nil {
			return 0, nil, errors.New("Device transport unavailable")
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, e := h.Do(r)
		if e != nil {
			return 0, nil, errors.New("Device transport unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return 0, nil, errors.New("Device redirect refused")
		}
		raw, e := io.ReadAll(io.LimitReader(response.Body, 32769))
		if e != nil {
			return 0, nil, errors.New("Device transport unavailable")
		}
		if len(raw) > 32768 {
			return 0, nil, errors.New("invalid Device response")
		}
		v, e := decodeJSON(raw, false)
		if e != nil || obj(v) == nil {
			return 0, nil, errors.New("invalid Device response")
		}
		return response.StatusCode, obj(v), nil
	}
}
func deviceLogin(ctx context.Context, api string, errOut io.Writer) (string, error) {
	u, e := url.Parse(api)
	if e != nil || u.Scheme != "https" {
		return "", errors.New("Device Flow requires validated HTTPS, including locally")
	}
	h := boundedHTTP()
	defer h.CloseIdleConnections()
	request := deviceForm(h, api)
	status, device, e := request(ctx, "/v0/oauth/device/authorization", map[string]string{"client_id": "aimount-app-cli"}, 15*time.Second)
	if ctx.Err() != nil {
		return "", commandError("cancelled", "Device login cancelled before authorization; no local publication", 3)
	}
	if e != nil {
		return "", e
	}
	expires, validExpires := exactInt(device["expires_in"], 600, 1)
	interval, validInterval := exactInt(device["interval"], maxSafe, 5)
	if status < 200 || status >= 300 || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(str(device["device_code"])) || !regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}(?:-[BCDFGHJKLMNPQRSTVWXZ]{4}){2}$`).MatchString(str(device["user_code"])) || !validExpires || !validInterval {
		return "", errors.New("Device authorization failed")
	}
	ordinary, complete, e := verificationURIs(device)
	if e != nil {
		return "", e
	}
	fmt.Fprintf(errOut, "Open %s\nEnter %s\nOr open %s\n", ordinary, str(device["user_code"]), complete)
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.CommandContext(ctx, opener, complete)
	if e := cmd.Start(); e != nil {
		fmt.Fprintln(errOut, "Open the verification link manually.")
	} else {
		go func() {
			if e := cmd.Wait(); e != nil {
				fmt.Fprintln(errOut, "Open the verification link manually.")
			}
		}()
	}
	start := time.Now()
	device["expires_in"] = expires
	device["interval"] = interval
	defer func() { device["device_code"] = "" }()
	return exchange(ctx, device, deviceClock{now: func() time.Duration { return time.Since(start) }, wait: waitContext, request: request})
}
func exchange(ctx context.Context, device object, clock deviceClock) (string, error) {
	expires, ok := exactInt(device["expires_in"], 600, 1)
	if !ok {
		return "", errors.New("Device authorization failed")
	}
	intervalN, ok := exactInt(device["interval"], maxSafe, 5)
	if !ok {
		return "", errors.New("Device authorization failed")
	}
	lifetime := time.Duration(expires) * time.Second
	deadline := clock.now() + lifetime
	// Intervals beyond the original lifetime cannot produce another attempt.
	interval := lifetime
	if intervalN <= expires {
		interval = time.Duration(intervalN) * time.Second
	}
	backoff := interval
	cancel := func() (string, error) {
		_, _, _ = clock.request(context.Background(), "/v0/oauth/device/cancel", map[string]string{"client_id": "aimount-app-cli", "device_code": str(device["device_code"])}, 3*time.Second)
		return "", commandError("validation_failed", "Device login cancelled; server cancellation may be unknown", 3)
	}
	for clock.now() < deadline {
		wait := interval
		if backoff > wait {
			wait = backoff
		}
		if remaining := deadline - clock.now(); wait > remaining {
			wait = remaining
		}
		e := clock.wait(ctx, wait)
		if e != nil || ctx.Err() != nil {
			return cancel()
		}
		if clock.now() >= deadline {
			break
		}
		timeout := 15 * time.Second
		if remaining := deadline - clock.now(); timeout > remaining {
			timeout = remaining
		}
		status, v, e := clock.request(ctx, "/v0/oauth/token", map[string]string{"client_id": "aimount-app-cli", "device_code": str(device["device_code"]), "grant_type": "urn:ietf:params:oauth:grant-type:device_code"}, timeout)
		if ctx.Err() != nil {
			return cancel()
		}
		if e != nil {
			if strings.Contains(e.Error(), "redirect") || strings.Contains(e.Error(), "invalid Device response") {
				return "", e
			}
			if backoff < lifetime {
				backoff *= 2
				if backoff > lifetime {
					backoff = lifetime
				}
			}
			if backoff < interval {
				backoff = interval
			}
			continue
		}
		backoff = interval
		if status >= 200 && status < 300 {
			_, expiresOK := exactInt(v["expires_in"], maxSafe, 1)
			if v["token_type"] != "Bearer" || !expiresOK || !regexp.MustCompile(`^aim_pat_[0-9A-HJKMNP-TV-Z]{16}_[A-Za-z0-9_-]{43}$`).MatchString(str(v["access_token"])) {
				return "", errors.New("Device token response invalid; no replacement login was started")
			}
			return str(v["access_token"]), nil
		}
		switch v["error"] {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			backoff = interval
			continue
		case "temporarily_unavailable":
			if status == 429 || status == 503 {
				return "", errors.New("Device service temporarily unavailable; no automatic retry or replacement login was started")
			}
		case "access_denied":
			return "", commandError("validation_failed", "Device consent denied", 3)
		}
		return "", errors.New("Device exchange failed; check Console PAT inventory before starting another login")
	}
	return "", errors.New("Device request expired")
}
