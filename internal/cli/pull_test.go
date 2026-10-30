package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullHardInterruptionAndOfflineRecovery(t *testing.T) {
	if os.Getenv("AIMOUNT_TEST_PULL_CRASH") == "1" {
		root := os.Getenv("AIMOUNT_TEST_PULL_ROOT")
		files := map[string]string{"models/same.yaml": "after", "applications/new.yaml": "new"}
		root, lock, err := openResourceRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		before, err := snapshotPull(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		s, err := preparePull(context.Background(), root, files, before, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, e := range s.Entries {
			if e.Path == "models/same.yaml" {
				if err := r.Rename(pullJournal+"/next/"+e.Path, e.Path); err != nil {
					t.Fatal(err)
				}
				if err := syncPullDir(r, "models"); err != nil {
					t.Fatal(err)
				}
			}
		}
		os.Exit(47) // Simulate process loss: no defer, rollback or lock release cleanup.
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/same.yaml": "before", "notes.txt": "preserve"})
	cmd := exec.Command(os.Args[0], "-test.run=^TestPullHardInterruptionAndOfflineRecovery$")
	cmd.Env = append(os.Environ(), "AIMOUNT_TEST_PULL_CRASH=1", "AIMOUNT_TEST_PULL_ROOT="+root)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 47 {
		t.Fatalf("child did not reach interrupted publication: %v %s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "models/same.yaml"))
	if string(data) != "after" {
		t.Fatal("child did not publish one file")
	}
	if err := noPendingPull(root); err == nil {
		t.Fatal("partial publication not fenced")
	}
	r := runner{ctx: context.Background()}
	if _, err := r.run(Options{Command: "pull", Recover: true, Root: root}); err != nil {
		t.Fatal("offline recovery after process loss failed", err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "models/same.yaml"))
	if string(data) != "before" {
		t.Fatal("original state not recovered")
	}
	if err := noPendingPull(root); err != nil {
		t.Fatal("recovered root still fenced", err)
	}
}

func TestPullJournalRollbackRecoveryAndConflicts(t *testing.T) {
	for _, mode := range []string{"rollback", "interrupted", "committed", "edited", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"models/old.yaml": "old", "models/same.yaml": "before", "notes.txt": "untouched"})
			os.Chmod(filepath.Join(root, "models/same.yaml"), 0640)
			files := map[string]string{"models/same.yaml": "after", "applications/new.yaml": "new"}
			root, lock, err := openResourceRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			// The writer lock is tested separately; construct the persisted interrupted state.
			before, err := snapshotPull(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := preparePull(context.Background(), root, files, before, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "rollback" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				err = publishPull(ctx, root, journal)
				if err == nil {
					t.Fatal("cancelled publication succeeded")
				}
			} else {
				writeFiles(t, root, map[string]string{"models/same.yaml": "after"})
				os.Chmod(filepath.Join(root, "models/same.yaml"), 0600) // Published next files are owner-only.
				os.Remove(filepath.Join(root, "models/old.yaml"))
				if mode == "committed" {
					writeFiles(t, root, map[string]string{pullJournal + "/committed": "committed\n"})
				}
				if mode == "edited" {
					writeFiles(t, root, map[string]string{"models/same.yaml": "user edit"})
				}
				if mode == "symlink" {
					os.Remove(filepath.Join(root, "models/same.yaml"))
					os.Symlink("../notes.txt", filepath.Join(root, "models/same.yaml"))
				}
			}
			err = recoverPull(root)
			if mode == "edited" || mode == "symlink" {
				if err == nil {
					t.Fatal("recovery overwrote unsafe target")
				}
				if _, e := os.Stat(filepath.Join(root, pullJournal)); e != nil {
					t.Fatal("recovery material lost")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "before"
			if mode == "committed" {
				want = "after"
			}
			data, _ := os.ReadFile(filepath.Join(root, "models/same.yaml"))
			if string(data) != want {
				t.Fatalf("want %s got %s", want, data)
			}
			if mode != "committed" {
				st, _ := os.Stat(filepath.Join(root, "models/same.yaml"))
				if st.Mode().Perm() != 0640 {
					t.Fatal("old mode lost")
				}
				data, _ = os.ReadFile(filepath.Join(root, "models/old.yaml"))
				if string(data) != "old" {
					t.Fatal("old file not restored")
				}
			}
			if _, err := os.Stat(filepath.Join(root, pullJournal)); !os.IsNotExist(err) {
				t.Fatal("completed journal retained")
			}
			if err := recoverPull(root); err != nil {
				t.Fatal("recovery not repeatable", err)
			}
		})
	}
}

func TestPublicationLockAndPendingJournal(t *testing.T) {
	root := t.TempDir()
	root, lock, err := openResourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := publicationLock(root); err == nil {
		t.Fatal("concurrent publication lock acquired")
	}
	writeFiles(t, root, map[string]string{pullJournal + "/manifest.json": "{}"})
	if err := noPendingPull(root); err == nil || !strings.Contains(err.Error(), "--recover") {
		t.Fatal("pending publication not blocked", err)
	}
	if err := recoverPull(root); err == nil {
		t.Fatal("malformed recovery accepted")
	}
	if _, err := os.Stat(filepath.Join(root, pullJournal)); errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid journal deleted")
	}
}

func TestPullWriteFailureRollsBackAlreadyPublishedFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"applications/existing.yaml": "before", "models/old.yaml": "old"})
	root, lock, err := openResourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	files := map[string]string{"applications/existing.yaml": "after", "models/new.yaml": "new"}
	before, err := snapshotPull(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := preparePull(context.Background(), root, files, before, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Missing prepared source forces a later rename failure after the first file changed.
	for _, e := range s.Entries {
		if e.Path == "models/new.yaml" {
			if err := os.Remove(filepath.Join(root, pullJournal, "next", e.Path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := publishPull(context.Background(), root, s); err == nil {
		t.Fatal("write failure reported success")
	}
	for name, want := range map[string]string{"applications/existing.yaml": "before", "models/old.yaml": "old"} {
		data, _ := os.ReadFile(filepath.Join(root, name))
		if string(data) != want {
			t.Fatal("rollback lost original", name)
		}
	}
	if err := noPendingPull(root); err != nil {
		t.Fatal("ordinary rollback left pending publication", err)
	}
}

func TestPullRefusesPhysicalPathAliasesBeforePublication(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/FOO.yaml": "original"})
	old, err := os.Stat(filepath.Join(root, "models/FOO.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	alias, err := os.Stat(filepath.Join(root, "models/foo.yaml"))
	if err != nil || !os.SameFile(old, alias) {
		// Hard links exercise the same duplicate physical-identity boundary on Linux.
		if err := os.Link(filepath.Join(root, "models/FOO.yaml"), filepath.Join(root, "models/foo.yaml")); err != nil {
			t.Fatal(err)
		}
	}
	_, err = testPull(t, root, func() (map[string]string, []string, error) {
		return map[string]string{"models/foo.yaml": "new"}, []string{"models/FOO.yaml"}, nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "same physical file") {
		t.Fatalf("aliased paths not refused in preflight: %v", err)
	}
	data, e := os.ReadFile(filepath.Join(root, "models/FOO.yaml"))
	if e != nil || string(data) != "original" {
		t.Fatal("preflight lost original", e)
	}
	if err := noPendingPull(root); err != nil {
		t.Fatal("preflight installed active journal", err)
	}
}

func TestPullLargeManifestRoundTripsBeforePublication(t *testing.T) {
	root := t.TempDir()
	root, lock, err := openResourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Mkdir(pullJournal, 0700); err != nil {
		t.Fatal(err)
	}
	s := pullState{Version: 1, Root: root, Entries: []pullEntry{}, Dirs: []string{}}
	s.Root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12000; i++ {
		s.Entries = append(s.Entries, pullEntry{Path: fmt.Sprintf("applications/%060d.yaml", i), After: pullHash([]byte("new"))})
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 1<<20 {
		t.Fatal("fixture is not large enough")
	}
	if err := writePullFile(r, pullJournal+"/manifest.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPullAt(context.Background(), s.Root, r, pullJournal); err != nil {
		t.Fatal("writer-compatible large manifest refused", len(data), err)
	}
	if err := recoverPull(root); err != nil {
		t.Fatal("large unchanged root cannot recover", err)
	}
}

func TestRecoveryOfflineAndPendingCommandsRefuseBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{pullJournal + "/manifest.json": "{}"})
	for _, command := range []string{"plan", "apply"} {
		r := runner{ctx: context.Background()}
		_, err := r.run(Options{Command: command, Root: root, API: "https://unreachable.invalid"})
		if err == nil || !strings.Contains(err.Error(), "--recover") {
			t.Fatal("pending state used or auth/network entered", err)
		}
	}
	o, err := parseOptions([]string{"pull", "--recover", "--root", root}, map[string]string{"AIMOUNT_API_URL": "", "AIMOUNT_PROJECT": "invalid"}, false)
	if err != nil || !o.Recover {
		t.Fatal("offline recovery validates unrelated target", err)
	}
	if _, err := parseOptions([]string{"pull", "--recover", "--force"}, nil, false); err == nil {
		t.Fatal("recovery mixed with download force")
	}
}

func TestPullPreparationDetectsEditsAndRestoresPrivateMode(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"models/same.yaml": "before"})
	root, lock, err := openResourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	files := map[string]string{"models/same.yaml": "after"}
	before, err := snapshotPull(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := preparePull(context.Background(), root, files, before, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := snapshotPull(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(before, after) {
		t.Fatal("journal bookkeeping counted as an external edit")
	}
	os.WriteFile(filepath.Join(root, "models/same.yaml"), []byte("changed"), 0600)
	if err := publishPull(context.Background(), root, s); err == nil {
		t.Fatal("newer edit overwritten")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "models/same.yaml")); string(data) != "changed" {
		t.Fatal("edit lost")
	}
}

func TestPullRecoveryPreservesUnknownPermissionChanges(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"models/same.yaml": "before"})
			if err := os.Chmod(filepath.Join(root, "models/same.yaml"), 0644); err != nil {
				t.Fatal(err)
			}
			root, lock, err := openResourceRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			before, err := snapshotPull(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			s, err := preparePull(context.Background(), root, map[string]string{"models/same.yaml": "after"}, before, nil)
			if err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0600)
			if published {
				if err := os.Rename(filepath.Join(root, pullJournal, "next/models/same.yaml"), filepath.Join(root, "models/same.yaml")); err != nil {
					t.Fatal(err)
				}
				mode = 0640
			}
			if err := os.Chmod(filepath.Join(root, "models/same.yaml"), mode); err != nil {
				t.Fatal(err)
			}
			if !published {
				if err := publishPull(context.Background(), root, s); err == nil {
					t.Fatal("chmod conflict accepted")
				}
			}
			if err := recoverPull(root); err == nil {
				t.Fatal("recovery accepted unknown content/mode combination")
			}
			st, err := os.Stat(filepath.Join(root, "models/same.yaml"))
			if err != nil || st.Mode().Perm() != mode {
				t.Fatal("local chmod overwritten", st, err)
			}
			if err := noPendingPull(root); err == nil {
				t.Fatal("conflicting journal lost")
			}
		})
	}
}

func TestCancelledPullPreparationStopsBeforeValidation(t *testing.T) {
	root, lock, err := openResourceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = preparePull(ctx, root, map[string]string{"models/new.yaml": "new"}, map[string]pullEntry{}, func(string) error { t.Error("cancelled preparation reached validation"); return nil })
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Exit != 3 {
		t.Fatal("cancel did not return cancellation", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".aimount"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pull-prepare-") {
			t.Fatal("cancelled preparation left staged files")
		}
	}
}

func TestCancelledPullJournalReaderStopsBeforeBackupReads(t *testing.T) {
	root, lock, err := openResourceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readPullAt(ctx, root, r, pullJournal)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Exit != 3 {
		t.Fatal("cancelled journal reader continued into filesystem validation", err)
	}
}
