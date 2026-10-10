package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
func fingerprint(ctx context.Context, root string) (string, error) {
	h := sha256.New()
	e := filepath.WalkDir(root, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(h, "%s\x00%d:%d:%d\x00", rel, st.Mode(), st.Size(), st.ModTime().UnixNano())
		if st.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(path)
			if e != nil {
				return e
			}
			io.WriteString(h, link)
		} else if st.Mode().IsRegular() {
			f, e := os.Open(path)
			if e != nil {
				return e
			}
			_, e = io.Copy(h, f)
			ce := f.Close()
			if e != nil {
				return e
			}
			return ce
		}
		return nil
	})
	if e != nil {
		return "", errors.New("cannot fingerprint local root")
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
func copyRoot(ctx context.Context, from, to string) error {
	st, e := os.Lstat(from)
	if e != nil {
		return e
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		link, e := os.Readlink(from)
		if e != nil {
			return e
		}
		return os.Symlink(link, to)
	case st.IsDir():
		if e := os.MkdirAll(to, 0700); e != nil {
			return e
		}
		es, e := os.ReadDir(from)
		if e != nil {
			return e
		}
		for _, entry := range es {
			if e := copyRoot(ctx, filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); e != nil {
				return e
			}
		}
		if e := os.Chmod(to, st.Mode().Perm()); e != nil {
			return e
		}
		return os.Chtimes(to, st.ModTime(), st.ModTime())
	case st.Mode().IsRegular():
		a, e := os.Open(from)
		if e != nil {
			return e
		}
		defer a.Close()
		b, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
		if e != nil {
			return e
		}
		_, e = io.Copy(b, a)
		ce := b.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if e := os.Chmod(to, st.Mode().Perm()); e != nil {
			return e
		}
		return os.Chtimes(to, st.ModTime(), st.ModTime())
	default:
		return errors.New("unsupported unmanaged filesystem entry")
	}
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
func replaceRoot(ctx context.Context, root string, prepare func() (map[string]string, []string, error), validate func(string) error) (map[string]string, error) {
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, errors.New("unreadable root")
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return nil, errors.New("invalid root")
	}
	cwd, e := os.Getwd()
	if e != nil {
		return nil, errors.New("working directory unavailable")
	}
	cwd, e = filepath.EvalSymlinks(cwd)
	if e != nil {
		return nil, errors.New("working directory unavailable")
	}
	if cwd == root || strings.HasPrefix(cwd, root+string(os.PathSeparator)) {
		return nil, errors.New("run atomic pull from outside root, using --root")
	}
	parent, name := filepath.Dir(root), filepath.Base(root)
	st, e := os.Stat(root)
	if e != nil || !st.IsDir() || root == parent || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return nil, errors.New("atomic pull requires a replaceable Linux/macOS root directory")
	}
	lockPath := filepath.Join(parent, "."+name+".aimount-pull.lock")
	lock, e := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, errors.New("another pull owns the root; leftover sibling lock requires explicit recovery")
	}
	defer func() { lock.Close(); os.Remove(lockPath) }()
	before, e := fingerprint(ctx, root)
	if e != nil {
		return nil, e
	}
	files, managed, e := prepare()
	if e != nil {
		return nil, e
	}
	stage, e := os.MkdirTemp(parent, "."+name+".aimount-pull-")
	if e != nil {
		return nil, errors.New("cannot create pull staging directory")
	}
	defer os.RemoveAll(stage)
	paths := append([]string{}, managed...)
	for p := range files {
		paths = append(paths, p)
	}
	for _, rel := range paths {
		if e := managedPath(root, rel); e != nil {
			return nil, e
		}
	}
	if e := copyRoot(ctx, root, stage); e != nil {
		return nil, errors.New("cannot copy unmanaged root entries")
	}
	for _, rel := range managed {
		if e := os.Remove(filepath.Join(stage, rel)); e != nil {
			return nil, errors.New("cannot remove staged managed file")
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(stage, rel)
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			return nil, errors.New("cannot create managed directory")
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, errors.New("cannot create managed file")
		}
		_, e = f.WriteString(files[rel])
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil || ce != nil {
			return nil, errors.New("cannot write managed file")
		}
	}
	if validate != nil {
		if e := validate(stage); e != nil {
			return nil, e
		}
	}
	after, e := fingerprint(ctx, root)
	if e != nil {
		return nil, e
	}
	if before != after {
		return nil, errors.New("local root changed during pull; retry explicitly")
	}
	if ctx.Err() != nil {
		return nil, commandError("cancelled", "Cancelled", 3)
	}
	if e := os.Chmod(stage, st.Mode().Perm()); e != nil {
		return nil, errors.New("cannot preserve root permissions")
	}
	if e := exchangeDirectories(root, stage); e != nil {
		return nil, errors.New("atomic directory exchange unavailable; root preserved")
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
