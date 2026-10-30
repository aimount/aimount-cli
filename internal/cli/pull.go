package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const pullJournal = ".aimount/pull"

// A manifest covers the entire graph, not a single resource file (1 MiB).
const pullManifestLimit = 64 << 20

type pullEntry struct {
	Path, Before, After string
	Mode                uint32
}
type pullState struct {
	Version int
	Root    string
	Entries []pullEntry
	Dirs    []string
}

func publicationLock(root string) (*os.File, error) {
	state := filepath.Join(root, ".aimount")
	if err := os.Mkdir(state, 0700); err != nil && !os.IsExist(err) {
		return nil, errors.New("cannot create local publication state")
	}
	st, err := os.Lstat(state)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid local publication directory")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// Persist the parent's .aimount entry before any journal or resource writes.
	if err := syncPullDir(r, "."); err != nil {
		return nil, err
	}
	lock, err := authLock(filepath.Join(state, "pull.lock"))
	if err != nil {
		return nil, commandError("pull_locked", "another resource command owns the Stack; retry explicitly", 2)
	}
	return lock, nil
}

func noPendingPull(root string) error {
	if _, err := os.Lstat(filepath.Join(root, pullJournal)); !os.IsNotExist(err) {
		return commandError("pull_recovery_required", "Unfinished local pull; run 'aimount pull --recover' for this root before continuing", 1)
	}
	return nil
}

func pullPath(path string) bool {
	if strings.Contains(path, "\\") {
		return false
	}
	p := strings.Split(path, "/")
	for _, s := range p {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	switch {
	case len(p) == 2 && (p[0] == "models" || p[0] == "applications"):
		return strings.HasSuffix(p[1], ".yaml")
	case len(p) == 3 && p[0] == "tools":
		return p[2] == "tool.yaml"
	case len(p) == 3 && p[0] == "agents":
		return p[2] == "agent.yaml" || p[2] == "instruction.md"
	case len(p) == 4 && p[0] == "tools" && p[2] == "contracts":
		return strings.HasSuffix(p[3], ".yaml")
	case len(p) == 4 && p[0] == "agents" && p[2] == "policies":
		return strings.HasSuffix(p[3], ".yaml")
	}
	return false
}

func pullHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func pullFile(root *os.Root, path string) ([]byte, os.FileMode, error) {
	return boundedPullFile(root, path, 1<<20)
}

func boundedPullFile(root *os.Root, path string, limit int64) ([]byte, os.FileMode, error) {
	st, err := root.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, 0, errors.New("unsafe or unreadable pull file")
	}
	data, err := root.ReadFile(path)
	return data, st.Mode().Perm(), err
}

func syncPullDir(root *os.Root, path string) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writePullFile(root *os.Root, path string, data []byte, mode os.FileMode) error {
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	return errors.Join(err, closed)
}

// Snapshot only authored files: unrelated edits do not affect publication.
func snapshotPull(ctx context.Context, path string) (map[string]pullEntry, error) {
	if ctx.Err() != nil {
		return nil, commandError("cancelled", "Cancelled", 3)
	}
	state := map[string]pullEntry{}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	err = caught(func() {
		for _, name := range discover(path).Managed {
			if ctx.Err() != nil {
				return
			}
			if err := managedPath(path, name); err != nil {
				fail(path, err.Error())
			}
			data, mode, err := pullFile(r, name)
			if err != nil || data == nil {
				fail(path, "unreadable managed file")
			}
			state[name] = pullEntry{Path: name, Before: pullHash(data), Mode: uint32(mode)}
		}
	})
	if ctx.Err() != nil {
		return nil, commandError("cancelled", "Cancelled", 3)
	}
	return state, err
}

func preparePull(ctx context.Context, path string, files map[string]string, beforeState map[string]pullEntry, validate func(string) error) (pullState, error) {
	s := pullState{Version: 1, Root: path, Entries: []pullEntry{}, Dirs: []string{}}
	if ctx.Err() != nil {
		return s, commandError("cancelled", "Cancelled", 3)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return s, err
	}
	defer r.Close()
	tmp := ".aimount/.pull-prepare-" + uuid()
	if err = r.Mkdir(tmp, 0700); err != nil {
		return s, err
	}
	defer r.RemoveAll(tmp)
	paths := slices.Collect(maps.Keys(beforeState))
	for name := range files {
		if ctx.Err() != nil {
			return s, commandError("cancelled", "Cancelled", 3)
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	// Different authored spellings can select the same inode on case-insensitive
	// filesystems (or via hard links). Independent journal entries would not replay.
	physical := map[[2]uint64]bool{}
	for _, name := range paths {
		if ctx.Err() != nil {
			return s, commandError("cancelled", "Cancelled", 3)
		}
		if !pullPath(name) {
			return s, errors.New("invalid managed pull path")
		}
		if err := managedPath(path, name); err != nil {
			return s, err
		}
		if st, err := r.Lstat(name); err == nil {
			native, ok := st.Sys().(*syscall.Stat_t)
			if !ok || !st.Mode().IsRegular() {
				return s, errors.New("invalid managed pull file")
			}
			id := [2]uint64{uint64(native.Dev), native.Ino}
			if physical[id] {
				return s, errors.New("managed paths resolve to the same physical file; rename them unambiguously before pull")
			}
			physical[id] = true
		} else if !os.IsNotExist(err) {
			return s, err
		}
	}
	for i, name := range paths {
		if ctx.Err() != nil {
			return s, commandError("cancelled", "Cancelled", 3)
		}
		if err = managedPath(path, name); err != nil {
			return s, err
		}
		before, mode, e := pullFile(r, name)
		if e != nil {
			return s, e
		}
		entry := pullEntry{Path: name, Mode: uint32(mode)}
		if before != nil {
			entry.Before = pullHash(before)
		}
		initial := beforeState[name]
		if entry.Before != initial.Before || entry.Mode != initial.Mode {
			return s, errors.New("local root changed during pull; retry explicitly")
		}
		if before != nil {
			if err = writePullFile(r, fmt.Sprintf("%s/old-%d", tmp, i), before, 0600); err != nil {
				return s, err
			}
		}
		if _, ok := files[name]; ok {
			next := []byte(files[name])
			entry.After = pullHash(next)
			if err = r.MkdirAll(tmp+"/next/"+filepath.Dir(name), 0700); err != nil {
				return s, err
			}
			if err = writePullFile(r, tmp+"/next/"+name, next, 0600); err != nil {
				return s, err
			}
			for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
				if _, e := r.Lstat(dir); os.IsNotExist(e) && !slices.Contains(s.Dirs, dir) {
					s.Dirs = append(s.Dirs, dir)
				}
			}
		}
		s.Entries = append(s.Entries, entry)
	}
	if err = r.MkdirAll(tmp+"/next", 0700); err != nil {
		return s, err
	}
	stage := filepath.Join(path, tmp, "next")
	if ctx.Err() != nil {
		return s, commandError("cancelled", "Cancelled", 3)
	}
	if validate != nil {
		if err := validate(stage); err != nil {
			return s, err
		}
	}
	// Persist the complete next tree before activating this same directory as journal.
	err = filepath.WalkDir(stage, func(name string, entry os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return commandError("cancelled", "Cancelled", 3)
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			rel, err := filepath.Rel(path, name)
			if err != nil {
				return err
			}
			return syncPullDir(r, rel)
		}
		return nil
	})
	if err != nil {
		return s, err
	}
	slices.Sort(s.Dirs)
	data, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	if len(data) > pullManifestLimit {
		return s, errors.New("pull recovery manifest exceeds supported size")
	}
	if err = writePullFile(r, tmp+"/manifest.json", data, 0600); err != nil {
		return s, err
	}
	if err = syncPullDir(r, tmp); err != nil {
		return s, err
	}
	after, err := snapshotPull(ctx, path)
	if err != nil {
		return s, err
	}
	if !maps.Equal(beforeState, after) {
		return s, errors.New("local root changed during pull; retry explicitly")
	}
	if ctx.Err() != nil {
		return s, commandError("cancelled", "Cancelled", 3)
	}
	// Validate with recovery's own decoder before making the journal active.
	if _, err = readPullAt(ctx, path, r, tmp); err != nil {
		return s, err
	}
	if ctx.Err() != nil {
		return s, commandError("cancelled", "Cancelled", 3)
	}
	if err = r.Rename(tmp, pullJournal); err != nil {
		return s, err
	}
	return s, syncPullDir(r, ".aimount")
}

func readPullAt(ctx context.Context, path string, r *os.Root, journal string) (pullState, error) {
	bad := errors.New("invalid pull recovery data; preserve .aimount/pull for inspection")
	var s pullState
	if ctx.Err() != nil {
		return s, commandError("cancelled", "Cancelled", 3)
	}
	st, err := r.Lstat(journal)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return s, bad
	}
	data, _, err := boundedPullFile(r, journal+"/manifest.json", pullManifestLimit)
	if err != nil || len(data) == 0 {
		return s, bad
	}
	if _, err = decodeJSON(data, true); err != nil {
		return s, bad
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil || s.Version != 1 || s.Root != path || s.Entries == nil || s.Dirs == nil {
		return s, bad
	}
	seen := map[string]bool{}
	validHash := func(h string) bool { return h == "" || len(h) == 64 && strings.Trim(h, "0123456789abcdef") == "" }
	for i, e := range s.Entries {
		if ctx.Err() != nil {
			return s, commandError("cancelled", "Cancelled", 3)
		}
		if !pullPath(e.Path) || seen[e.Path] || !validHash(e.Before) || !validHash(e.After) || e.Mode > 0777 || e.Before == "" && e.Mode != 0 || e.Before == "" && e.After == "" {
			return s, bad
		}
		seen[e.Path] = true
		if e.Before != "" {
			data, _, err := pullFile(r, fmt.Sprintf("%s/old-%d", journal, i))
			if err != nil || pullHash(data) != e.Before {
				return s, bad
			}
		}
	}
	for _, dir := range s.Dirs {
		if ctx.Err() != nil {
			return s, commandError("cancelled", "Cancelled", 3)
		}
		if !slices.ContainsFunc(s.Entries, func(e pullEntry) bool { return strings.HasPrefix(e.Path, dir+"/") }) || filepath.Clean(dir) != dir || strings.HasPrefix(dir, ".") || strings.Contains(dir, "\\") {
			return s, bad
		}
	}
	return s, nil
}

func finishPull(r *os.Root) error {
	tmp := ".aimount/.pull-cleanup-" + uuid()
	if err := r.Rename(pullJournal, tmp); err != nil {
		return err
	}
	if err := syncPullDir(r, ".aimount"); err != nil {
		return err
	}
	return r.RemoveAll(tmp)
}

func recoverPull(path string) error {
	if err := noPendingPull(path); err == nil {
		return nil
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer r.Close()
	s, err := readPullAt(context.Background(), path, r, pullJournal)
	if err != nil {
		return err
	}
	if data, _, e := pullFile(r, pullJournal+"/committed"); e != nil {
		return e
	} else if data != nil {
		if string(data) != "committed\n" {
			return errors.New("invalid pull commit marker")
		}
		return finishPull(r)
	}
	// Validate every target before rollback; never overwrite post-interruption edits.
	for _, e := range s.Entries {
		if err := managedPath(path, e.Path); err != nil {
			return err
		}
		data, mode, err := pullFile(r, e.Path)
		if err != nil {
			return err
		}
		h := ""
		if data != nil {
			h = pullHash(data)
		}
		afterMode := uint32(0600)
		if e.After == "" {
			afterMode = 0
		}
		if !(h == e.Before && uint32(mode) == e.Mode) && !(h == e.After && uint32(mode) == afterMode) {
			return commandError("pull_recovery_conflict", "Local files changed after interrupted pull; preserve .aimount/pull backups and inspect before recovery", 2)
		}
	}
	for i := len(s.Entries) - 1; i >= 0; i-- {
		e := s.Entries[i]
		if e.Before == "" {
			if err := r.Remove(e.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else {
			data, _, err := pullFile(r, fmt.Sprintf("%s/old-%d", pullJournal, i))
			if err != nil {
				return err
			}
			tmp := pullJournal + "/restore-" + uuid()
			if err := writePullFile(r, tmp, data, os.FileMode(e.Mode)); err != nil {
				return err
			}
			if err := r.MkdirAll(filepath.Dir(e.Path), 0755); err != nil {
				return err
			}
			if err := r.Rename(tmp, e.Path); err != nil {
				return err
			}
		}
		if _, err := r.Stat(filepath.Dir(e.Path)); err == nil {
			if err := syncPullDir(r, filepath.Dir(e.Path)); err != nil {
				return err
			}
		}
	}
	for i := len(s.Dirs) - 1; i >= 0; i-- {
		// Empty directories created by this pull only; unmanaged additions stay.
		_ = r.Remove(s.Dirs[i])
		if _, err := r.Stat(filepath.Dir(s.Dirs[i])); err == nil {
			if err := syncPullDir(r, filepath.Dir(s.Dirs[i])); err != nil {
				return err
			}
		}
	}
	return finishPull(r)
}

func publishPull(ctx context.Context, path string, s pullState) (err error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer r.Close()
	defer func() {
		if err != nil {
			if rollback := recoverPull(path); rollback != nil {
				err = fmt.Errorf("pull interrupted; run 'aimount pull --recover': %w", errors.Join(err, rollback))
			}
		}
	}()
	for _, e := range s.Entries {
		if ctx.Err() != nil {
			return commandError("cancelled", "Cancelled", 3)
		}
		if err = managedPath(path, e.Path); err != nil {
			return err
		}
		data, mode, e2 := pullFile(r, e.Path)
		if e2 != nil {
			return e2
		}
		h := ""
		if data != nil {
			h = pullHash(data)
		}
		if h != e.Before || uint32(mode) != e.Mode {
			return errors.New("local root changed during pull; retry explicitly")
		}
		if e.After == "" {
			if err = r.Remove(e.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else {
			if err = r.MkdirAll(filepath.Dir(e.Path), 0755); err != nil {
				return err
			}
			if err = r.Rename(pullJournal+"/next/"+e.Path, e.Path); err != nil {
				return err
			}
		}
		if err = syncPullDir(r, filepath.Dir(e.Path)); err != nil {
			return err
		}
	}
	for i := len(s.Dirs) - 1; i >= 0; i-- {
		if err = syncPullDir(r, s.Dirs[i]); err != nil {
			return err
		}
		if err = syncPullDir(r, filepath.Dir(s.Dirs[i])); err != nil {
			return err
		}
	}
	if err = writePullFile(r, pullJournal+"/commit.tmp", []byte("committed\n"), 0600); err != nil {
		return err
	}
	if err = r.Rename(pullJournal+"/commit.tmp", pullJournal+"/committed"); err != nil {
		return err
	}
	if err = syncPullDir(r, pullJournal); err != nil {
		return err
	}
	return finishPull(r)
}
