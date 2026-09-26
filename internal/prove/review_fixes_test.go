package prove

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/test-check/internal/source"
)

// Review P2: a changed content line starting "++" or "--" was read as a file
// header, so the test it belonged to was silently dropped.
func TestChangedTestNamesKeepsTestsWithPlusPlusAndMinusMinusLines(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	put(t, dir, "u_test.py", "def test_v():\n    x = 1\n    assert x\n\n\ndef test_w():\n    s = 1\n--z\n    assert s\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	put(t, dir, "u_test.py", "def test_v():\n    x = 1\n++y\n    assert x\n\n\ndef test_w():\n    s = 1\n    assert s\n")
	names, err := ChangedTestNames(source.Git(context.Background(), dir), base, dir, []string{"u_test.py"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"test_v", "test_w"}) {
		t.Fatalf("names = %v, want [test_v test_w]", names)
	}
}

// Review P2: a second run must not touch the first run's stash. While a live
// process holds the lock, Run and Restore refuse and leave the stash intact.
func TestLiveLockBlocksRunAndRestore(t *testing.T) {
	dir := fixture(t)
	gitDir := strings.TrimSpace(git(t, dir, "rev-parse", "--absolute-git-dir"))
	stash := filepath.Join(gitDir, stashDir)
	if err := os.Mkdir(stash, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, stash, manifestName, `[{"path":"retry.go","existed":false}]`)
	// A live process that is not this test: the parent (go test).
	if err := os.WriteFile(filepath.Join(gitDir, lockName), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "true"}); err == nil || !strings.Contains(err.Error(), "another prove run") {
		t.Fatalf("Run err = %v, want refusal", err)
	}
	if err := Restore(context.Background(), dir); err == nil {
		t.Fatal("Restore consumed a live run's stash")
	}
	if _, err := os.Stat(filepath.Join(stash, manifestName)); err != nil {
		t.Fatalf("live run's manifest gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "retry.go")); err != nil {
		t.Fatalf("working file touched: %v", err)
	}
}

// A lock left by a dead process is taken over.
func TestStaleLockIsTakenOver(t *testing.T) {
	dir := fixture(t)
	gitDir := strings.TrimSpace(git(t, dir, "rev-parse", "--absolute-git-dir"))
	if err := os.WriteFile(filepath.Join(gitDir, lockName), []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "sh retry_test.sh"}); err != nil {
		t.Fatalf("Run with stale lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, lockName)); !os.IsNotExist(err) {
		t.Fatalf("lock not released: %v", err)
	}
}

// Review P3: a base-side symlink stays a symlink for the old-code run.
func TestOldRunSeesBaseSymlinkAsSymlink(t *testing.T) {
	dir := fixture(t)
	git(t, dir, "checkout", "-q", "main")
	if err := os.Symlink("retry.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "link")
	git(t, dir, "checkout", "-q", "-b", "fix2")
	if err := os.Remove(filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "link.go", "not a link\n")
	put(t, dir, "retry.go", "func Retry() error { return errStop }\n")
	put(t, dir, "link_test.sh", "test -L link.go && exit 1; exit 0\n")
	report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "sh link_test.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeProven {
		t.Fatalf("outcome = %s, want proven (old run must see a symlink)", report.Outcome)
	}
}

// Found on gitmoot#2261: a test the change deletes was still listed, and then
// reported test_not_run because it no longer exists.
func TestChangedTestNamesSkipsDeletedAndRenamedAwayTests(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	put(t, dir, "x_test.go", "package x\n\nfunc TestKeep(t *testing.T) {\n\t_ = 1\n}\n\nfunc TestGone(t *testing.T) {\n\t_ = 2\n\t_ = 3\n}\n\nfunc TestOldName(t *testing.T) {\n\t_ = 4\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	put(t, dir, "x_test.go", "package x\n\nfunc TestKeep(t *testing.T) {\n\t_ = 10\n}\n\nfunc TestNewName(t *testing.T) {\n\t_ = 4\n}\n")
	names, err := ChangedTestNames(source.Git(context.Background(), dir), base, dir, []string{"x_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"TestKeep", "TestNewName"}) {
		t.Fatalf("names = %v, want [TestKeep TestNewName]", names)
	}
}
