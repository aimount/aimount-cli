package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func globalDirectory() (string, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return "", errors.New("HOME unavailable")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library/Application Support/aimount"), nil
	}
	if runtime.GOOS != "linux" {
		return "", errors.New("local state requires Linux/macOS")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "aimount"), nil
}
func readState(path, kind string) (object, error) {
	bad := func() (object, error) { return nil, errors.New(path + ": invalid or unreadable " + kind + " file") }
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		if kind == "auth" {
			return object{"tokens": object{}}, nil
		}
		return object{}, nil
	}
	if e != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 || (kind == "auth" && st.Mode().Perm()&0077 != 0) {
		return bad()
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return bad()
	}
	v, e := decodeJSON(b, true)
	m := obj(v)
	if e != nil || m == nil {
		return bad()
	}
	switch kind {
	case "auth":
		if len(m) != 1 || obj(m["tokens"]) == nil {
			return bad()
		}
		for _, v := range obj(m["tokens"]) {
			s, ok := v.(string)
			if !ok || s == "" {
				return bad()
			}
		}
	case "binding":
		if len(m) != 1 || !idValid(str(m["project_id"]), "prj") {
			return bad()
		}
	case "config":
		for k, v := range m {
			switch k {
			case "api_url":
				if _, ok := v.(string); !ok {
					return bad()
				}
			case "contexts":
				if obj(v) == nil {
					return bad()
				}
				for _, v := range obj(v) {
					c := obj(v)
					if c == nil || !idValid(str(c["user_id"]), "usr") {
						return bad()
					}
					for k, v := range c {
						if k != "user_id" && k != "organization_id" {
							return bad()
						}
						if k == "organization_id" && !idValid(str(v), "org") {
							return bad()
						}
					}
				}
			default:
				return bad()
			}
		}
	default:
		return bad()
	}
	return m, nil
}
func uuid() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("operating system randomness unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func atomicJSON(path string, data any) error {
	return atomicJSONContext(context.Background(), path, data)
}
func atomicJSONContext(ctx context.Context, path string, data any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return errors.New("cannot create local state directory")
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+uuid())
	defer os.Remove(tmp)
	f, e := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New("cannot create local state file")
	}
	defer f.Close()
	if _, e = f.WriteString(wire(data) + "\n"); e != nil {
		return errors.New("cannot write local state file")
	}
	if e = f.Sync(); e != nil {
		return errors.New("cannot sync local state file")
	}
	if e = f.Close(); e != nil {
		return errors.New("cannot close local state file")
	}
	if ctx.Err() != nil {
		return commandError("validation_failed", "Device login cancelled; no local publication", 3)
	}
	if e = os.Rename(tmp, path); e != nil {
		return errors.New("cannot publish local state file")
	}
	return nil
}
func updateAuth(path, api, token string, remove bool) error {
	return updateAuthContext(context.Background(), path, api, token, remove)
}
func updateAuthContext(ctx context.Context, path, api, token string, remove bool) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return errors.New("cannot create auth directory")
	}
	lock, e := authLock(path + ".lock")
	if e != nil {
		return e
	}
	defer lock.Close()
	auth, e := readState(path, "auth")
	if e != nil {
		return e
	}
	tokens := obj(auth["tokens"])
	if remove {
		delete(tokens, api)
	} else {
		tokens[api] = token
	}
	return atomicJSONContext(ctx, path, auth)
}
func managedPath(root, relative string) error {
	if filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return errors.New("invalid managed path")
	}
	for _, p := range strings.Split(relative, "/") {
		if p == "" || p == "." || p == ".." {
			return errors.New("invalid managed path")
		}
		root = filepath.Join(root, p)
		st, e := os.Lstat(root)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return errors.New("unreadable managed path")
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed symlink paths cannot be replaced")
		}
	}
	return nil
}
func openResourceRoot(root string) (string, *os.File, error) {
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", nil, errors.New("unreadable root")
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return "", nil, errors.New("invalid root")
	}
	st, e := os.Stat(root)
	if e != nil || !st.IsDir() || root == filepath.Dir(root) || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return "", nil, errors.New("resource commands require a Linux/macOS root directory")
	}
	lock, e := publicationLock(root)
	return root, lock, e
}

func replaceRoot(ctx context.Context, root string, prepare func() (map[string]string, []string, error), validate func(string) error) (map[string]string, error) {
	before, e := snapshotPull(ctx, root)
	if e != nil {
		return nil, e
	}
	files, _, e := prepare()
	if e != nil {
		return nil, e
	}
	journal, e := preparePull(ctx, root, files, before, validate)
	if e != nil {
		return nil, e
	}
	if e = publishPull(ctx, root, journal); e != nil {
		return nil, e
	}
	return files, nil
}

func checkLockIdentity(f *os.File, path string) error {
	st, e := f.Stat()
	if e != nil {
		return errors.New("invalid auth lock")
	}
	named, e := os.Lstat(path)
	if e != nil {
		return errors.New("invalid auth lock")
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || int(sys.Uid) != os.Getuid() || sys.Nlink != 1 || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, named) {
		return errors.New("invalid or non-owner-only auth lock")
	}
	return nil
}
