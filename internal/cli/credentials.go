package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const applicationCredentialsScope = "application_credentials"

func hasApplicationCredentials(identity object) bool {
	return obj(identity["authentication"])[applicationCredentialsScope] == true
}

func credentialIDValid(id string) bool {
	return len(id) == 16 && strings.Trim(id, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") == ""
}

func credentialCollection(path string) string {
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(path, "/")
	if (len(parts) == 7 || len(parts) == 8) && parts[1] == "v0" && parts[2] == "projects" && parts[4] == "applications" {
		if parts[6] == "runtime-credentials" || parts[6] == "tool-secrets" {
			return parts[6]
		}
	}
	return ""
}

// Only recognized pre-commit refusals allow removing the create marker.
func safeCredentialError(status int, code string) string {
	switch status {
	case 400:
		if code == "validation_failed" || code == "name_reserved" {
			return code
		}
	case 401:
		if code == "invalid_credential" || code == "unauthorized" {
			return code
		}
	case 403:
		if code == "permission_denied" || code == "application_credentials_required" {
			return code
		}
	case 404:
		if code == "not_found" {
			return code
		}
	case 409:
		if code == "application_archived" || code == "tool_secret_limit" {
			return code
		}
	case 413:
		if code == "payload_too_large" {
			return code
		}
	}
	return "request_failed"
}

func credentialMetadata(m object, application string, runtime bool) object {
	if !credentialIDValid(str(m["id"])) {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, str(m["created_at"])); err != nil {
		return nil
	}
	out := fields(m, "id", "created_at")
	if runtime {
		if m["application_id"] != application || !nameRE.MatchString(str(m["name"])) {
			return nil
		}
		out["name"] = m["name"]
		for _, field := range []string{"expires_at", "revoked_at"} {
			if value, present := m[field]; present {
				if _, err := time.Parse(time.RFC3339Nano, str(value)); err != nil {
					return nil
				}
				out[field] = value
			}
		}
	}
	out["application_id"] = application
	return out
}

func manageApplicationCredentials(ctx context.Context, c *Client, o Options, project, application string) (object, error) {
	runtime := o.Command == "runtime-keys"
	path := "/v0/projects/" + project + "/applications/" + application + "/tool-secrets"
	if runtime {
		path = "/v0/projects/" + project + "/applications/" + application + "/runtime-credentials"
	}
	invalid := &APIError{Status: 200, Code: "invalid_response"}
	switch o.Args[0] {
	case "list":
		var items []any
		var err error
		if runtime {
			items, err = c.pages(ctx, path)
		} else {
			var page object
			page, err = c.request(ctx, "GET", path, "", "")
			items, _ = page["items"].([]any)
		}
		if err != nil {
			return nil, err
		}
		if items == nil || !runtime && len(items) > 2 {
			return nil, invalid
		}
		safe := []any{}
		seen := map[string]bool{}
		for _, item := range items {
			m := credentialMetadata(obj(item), application, runtime)
			id := str(m["id"])
			if m == nil || seen[id] {
				return nil, invalid
			}
			safe = append(safe, m)
			seen[id] = true
		}
		return object{"application_id": application, "items": safe}, nil
	case "revoke":
		id := o.Args[1]
		if _, err := c.request(ctx, "DELETE", path+"/"+id, "", ""); err != nil {
			return nil, err
		}
		return object{"id": id, "application_id": application, "revoked": true}, nil
	default: // create
		return createApplicationCredential(ctx, c, o, path, application, runtime)
	}
}

func createApplicationCredential(ctx context.Context, c *Client, o Options, path, application string, runtime bool) (object, error) {
	root, dir, final, marker, staged, err := prepareCredentialFile(o.SecretFile)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer staged.Close()
	stage := filepath.Join(marker, "value")
	stageInfo, err := staged.Stat()
	if err != nil {
		return nil, commandError("credential_file_unavailable", "Cannot verify --secret-file staging file; credential creation was not sent", 1)
	}
	markerInfo, err := root.Lstat(marker)
	if err != nil || !markerInfo.IsDir() {
		return nil, commandError("credential_file_unavailable", "Cannot verify --secret-file pending marker; credential creation was not sent", 1)
	}
	unchanged := func(path string, original os.FileInfo) bool {
		current, err := root.Lstat(path)
		return err == nil && os.SameFile(current, original) && current.Mode() == original.Mode()
	}
	removeStage := func() bool {
		return unchanged(marker, markerInfo) && unchanged(stage, stageInfo) && root.Remove(stage) == nil
	}
	clean := func() bool { return removeStage() && root.Remove(marker) == nil }
	if ctx.Err() != nil {
		clean()
		return nil, commandError("cancelled", "Cancelled; credential creation was not sent", 3)
	}
	unknown := commandError("credential_create_unknown", "Credential issuance could not be confirmed; keep the .pending marker, inspect credential metadata and reconcile before trying again", 1)
	body := ""
	if runtime {
		body = wire(object{"name": o.CredentialName})
	}
	response, err := c.request(ctx, "POST", path, body, "")
	if err != nil {
		var refusal *APIError
		if errors.As(err, &refusal) && safeCredentialError(refusal.Status, refusal.Code) != "request_failed" {
			if !clean() {
				return nil, commandError("credential_file_cleanup_failed", "Credential creation was refused, but the .pending marker could not be cleared; inspect it before trying again", 1)
			}
			return nil, err
		}
		return nil, unknown
	}
	m, value := response, str(response["secret"])
	if runtime {
		m, value = obj(response["credential"]), str(response["token"])
	}
	safe := credentialMetadata(m, application, runtime)
	prefix := "whsec_"
	if runtime {
		prefix = "aim_runtime_" + str(m["id"]) + "_"
	}
	b64, ok := strings.CutPrefix(value, prefix)
	if safe == nil || runtime && safe["name"] != o.CredentialName || !ok || len(b64) != 43 {
		return nil, unknown
	}
	if decoded, err := base64.RawURLEncoding.Strict().DecodeString(b64); err != nil || len(decoded) != 32 {
		return nil, unknown
	}
	details := fields(safe, "id", "application_id")
	unconfirmed := &CommandError{
		Code:    "credential_file_unconfirmed",
		Message: "Credential " + str(safe["id"]) + " was issued for Application " + application + ", but local file publication did not complete; keep the .pending marker, inspect credential metadata and reconcile before trying again",
		Exit:    1,
		Details: details,
	}
	receipt := filepath.Join(marker, "receipt.json")
	receiptData := []byte(wire(details) + "\n")
	if !unchanged(marker, markerInfo) || writePullFile(root, receipt, receiptData, 0600) != nil || syncPullDir(root, marker) != nil {
		return nil, unconfirmed
	}
	receiptInfo, err := root.Lstat(receipt)
	if err != nil || !unchanged(marker, markerInfo) || !unchanged(stage, stageInfo) {
		return nil, unconfirmed
	}
	if n, err := staged.WriteString(value); err != nil || n != len(value) || staged.Sync() != nil || staged.Close() != nil {
		return nil, unconfirmed
	}
	currentDir, err := os.Lstat(dir)
	pinnedDir, pinnedErr := root.Stat(".")
	if err != nil || pinnedErr != nil || !credentialDirectorySafe(currentDir) || !os.SameFile(currentDir, pinnedDir) {
		return nil, unconfirmed
	}
	if root.Link(stage, final) != nil || !unchanged(final, stageInfo) || syncPullDir(root, ".") != nil || !removeStage() || !unchanged(receipt, receiptInfo) || root.Remove(receipt) != nil {
		return nil, unconfirmed
	}
	if root.Remove(marker) != nil {
		if unchanged(marker, markerInfo) && writePullFile(root, receipt, receiptData, 0600) == nil {
			_ = syncPullDir(root, marker)
		}
		return nil, unconfirmed
	}
	// The final link is already durable; a crash before this sync may leave a conservative marker.
	_ = syncPullDir(root, ".")
	return safe, nil
}

func credentialDirectorySafe(info os.FileInfo) bool {
	sys, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && info.Mode().Perm()&0022 == 0 && int(sys.Uid) == os.Geteuid()
}

func prepareCredentialFile(path string) (*os.Root, string, string, string, *os.File, error) {
	bad := func(code, message string) (*os.Root, string, string, string, *os.File, error) {
		return nil, "", "", "", nil, commandError(code, message, 1)
	}
	unsafe := func() (*os.Root, string, string, string, *os.File, error) {
		return bad("credential_file_unavailable", "Cannot prepare --secret-file in a safe writable directory; credential creation was not sent")
	}
	if strings.HasSuffix(path, string(filepath.Separator)) || strings.HasSuffix(path, "/.") || strings.HasSuffix(path, "/..") {
		return unsafe()
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return unsafe()
	}
	dir, final := filepath.Dir(path), filepath.Base(path)
	if final == string(filepath.Separator) {
		return unsafe()
	}
	namedDir, err := os.Lstat(dir)
	if err != nil || !namedDir.IsDir() {
		return unsafe()
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return unsafe()
	}
	dirInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(namedDir, dirInfo) || !credentialDirectorySafe(dirInfo) {
		root.Close()
		return unsafe()
	}
	marker := final + ".pending"
	if err := root.Mkdir(marker, 0700); err != nil {
		root.Close()
		if os.IsExist(err) {
			return bad("credential_file_pending", "The --secret-file .pending marker already exists; reconcile the previous attempt before creating again")
		}
		return unsafe()
	}
	stage := filepath.Join(marker, "value")
	probe := filepath.Join(marker, "probe")
	var staged *os.File
	ready := false
	defer func() {
		if !ready {
			if staged != nil {
				staged.Close()
				_ = root.Remove(stage)
			}
			_ = root.Remove(marker)
			root.Close()
		}
	}()
	if _, err := root.Lstat(final); err == nil {
		return bad("credential_file_exists", "The --secret-file destination already exists; credential creation was not sent")
	} else if !os.IsNotExist(err) {
		return unsafe()
	}
	staged, err = root.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil || staged.Chmod(0600) != nil {
		return unsafe()
	}
	// Verify no-clobber publication support and persist the marker before any POST.
	if root.Link(stage, probe) != nil || root.Remove(probe) != nil || syncPullDir(root, marker) != nil || syncPullDir(root, ".") != nil {
		return unsafe()
	}
	ready = true
	return root, dir, final, marker, staged, nil
}
