package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAuthFreshMergePersistentLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := updateAuth(p, "https://a.example", "a", false); err != nil {
		t.Fatal(err)
	}
	l1, _ := os.Stat(p + ".lock")
	if err := updateAuth(p, "https://b.example", "b", false); err != nil {
		t.Fatal(err)
	}
	if err := updateAuth(p, "https://a.example", "", true); err != nil {
		t.Fatal(err)
	}
	v, err := readState(p, "auth")
	if err != nil || wire(v) != `{"tokens":{"https://b.example":"b"}}` {
		t.Fatal(v, err)
	}
	l2, _ := os.Stat(p + ".lock")
	if !os.SameFile(l1, l2) {
		t.Fatal("lock inode replaced")
	}
	f, err := os.OpenFile(p+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := updateAuth(p, "https://b.example", "", true); err == nil || !strings.Contains(err.Error(), "auth lock") {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestStablePullPreservesUnmanagedAndRejectsChanges(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "stack")
	os.Mkdir(root, 0750)
	writeFiles(t, root, map[string]string{"models/old.yaml": "old", "unknown.txt": "preserved"})
	os.Chmod(filepath.Join(root, "unknown.txt"), 0640)
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("unknown.txt", filepath.Join(root, "relative-link")); err != nil {
		t.Fatal(err)
	}
	prepare := func() (map[string]string, []string, error) {
		return map[string]string{"models/new.yaml": "new\n"}, []string{"models/old.yaml"}, nil
	}
	var staged os.FileInfo
	if _, err := testPull(t, root, prepare, func(stage string) error {
		b, _ := os.ReadFile(filepath.Join(stage, "models/new.yaml"))
		if string(b) != "new\n" {
			t.Fatal("stage missing")
		}
		staged, err = os.Stat(filepath.Join(stage, "models/new.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(root)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("pull replaced root inode", err)
	}
	published, err := os.Stat(filepath.Join(root, "models/new.yaml"))
	if err != nil || !os.SameFile(staged, published) {
		t.Fatal("validated new file was copied before publication", err)
	}
	if s, _ := os.Readlink(filepath.Join(root, "relative-link")); s != "unknown.txt" {
		t.Fatal(s)
	}
	if st, _ := os.Stat(filepath.Join(root, "unknown.txt")); st.Mode().Perm() != 0640 {
		t.Fatal(st.Mode())
	}
	if _, err := os.Stat(filepath.Join(root, "models/old.yaml")); !os.IsNotExist(err) {
		t.Fatal("old managed remains")
	}
	_, err = testPull(t, root, func() (map[string]string, []string, error) {
		os.WriteFile(filepath.Join(root, "unknown.txt"), []byte("edited"), 0640)
		return map[string]string{"models/new.yaml": "new"}, []string{"models/new.yaml"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, "models/new.yaml"))
	os.Symlink("../unknown.txt", filepath.Join(root, "models/new.yaml"))
	_, err = testPull(t, root, func() (map[string]string, []string, error) {
		return map[string]string{"models/new.yaml": "bad"}, nil, nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal(err)
	}
}

func TestPullDetectsManagedContentModeAndDiscoveryChanges(t *testing.T) {
	for _, change := range []string{"content", "mode", "added", "removed"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"models/old.yaml": "original"})
			_, err := testPull(t, root, func() (map[string]string, []string, error) {
				switch change {
				case "content":
					os.WriteFile(filepath.Join(root, "models/old.yaml"), []byte("edited"), 0600)
				case "mode":
					os.Chmod(filepath.Join(root, "models/old.yaml"), 0640)
				case "added":
					writeFiles(t, root, map[string]string{"applications/added.yaml": "user file"})
				case "removed":
					os.Remove(filepath.Join(root, "models/old.yaml"))
				}
				return map[string]string{"models/new.yaml": "new"}, []string{"models/old.yaml"}, nil
			}, nil)
			if err == nil || !strings.Contains(err.Error(), "local root changed") {
				t.Fatal("managed change was not fenced", err)
			}
			if _, err := os.Stat(filepath.Join(root, "models/new.yaml")); !os.IsNotExist(err) {
				t.Fatal("new graph published over local change", err)
			}
		})
	}
}

func TestPullWholeOperationLockAndRefusedStage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stack")
	os.Mkdir(root, 0700)
	writeFiles(t, root, map[string]string{"models/old.yaml": "old"})
	_, e := testPull(t, root, func() (map[string]string, []string, error) {
		_, e := testPull(t, root, func() (map[string]string, []string, error) { t.Fatal("second prepare entered"); return nil, nil, nil }, nil)
		if e == nil || !strings.Contains(e.Error(), "another resource command owns") {
			t.Fatal(e)
		}
		return map[string]string{"models/new.yaml": "new"}, []string{"models/old.yaml"}, nil
	}, func(stage string) error { return commandError("validation_failed", "candidate refused", 1) })
	if e == nil || !strings.Contains(e.Error(), "candidate refused") {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "models/old.yaml")); string(b) != "old" {
		t.Fatal("refusal lost root")
	}
}

func testPull(t *testing.T, root string, prepare func() (map[string]string, []string, error), validate func(string) error) (map[string]string, error) {
	t.Helper()
	root, lock, err := openResourceRoot(root)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := noPendingPull(root); err != nil {
		return nil, err
	}
	return replaceRoot(context.Background(), root, prepare, validate)
}

func TestLocalStateSymlinkMalformedAndFreshAuthRefusal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if e := atomicJSON(p, object{"tokens": object{"https://foreign.example": "foreign"}}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(`{"tokens":{"https://foreign.example":"foreign"},"unknown":"private"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := updateAuth(p, "https://selected.example", "late", false); e == nil {
		t.Fatal("fresh malformed state overwritten")
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "unknown") {
		t.Fatal("state lost")
	}
	os.Remove(p + ".lock")
	outside := filepath.Join(dir, "outside")
	os.WriteFile(outside, []byte("untouched"), 0600)
	os.Symlink(outside, p+".lock")
	if e := updateAuth(p, "https://selected.example", "late", false); e == nil {
		t.Fatal("symlink lock accepted")
	}
	b, _ = os.ReadFile(outside)
	if string(b) != "untouched" {
		t.Fatal("outside changed")
	}
	config := filepath.Join(dir, "config.json")
	os.WriteFile(config, []byte(`{"contexts":{"https://api.example":{"user_id":"usr_0123456789ABCDEF","organization_id":"foreign","token":"private"}}}`), 0600)
	if _, e := readState(config, "config"); e == nil {
		t.Fatal("invalid context accepted")
	}
}

func TestCancelledAuthCannotPublish(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	if e := updateAuth(p, "https://a.example", "old", false); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := updateAuthContext(ctx, p, "https://a.example", "late", false); e == nil {
		t.Fatal("cancelled publication accepted")
	}
	v, e := readState(p, "auth")
	if e != nil || obj(v["tokens"])["https://a.example"] != "old" {
		t.Fatal("late PAT saved")
	}
}
