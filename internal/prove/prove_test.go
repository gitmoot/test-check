package prove

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func put(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: main has a buggy retry.go and a weak existing test. The branch fixes
// retry.go (committed), adds stop.go (untracked) and edits notes.md
// (uncommitted); tests are shell scripts that grep the code.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	put(t, dir, "retry.go", "func Retry() error { return nil }\n")
	put(t, dir, "retry_test.sh", "grep -q 'func Retry' retry.go\n")
	put(t, dir, "notes.md", "old\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "fix")
	put(t, dir, "retry.go", "func Retry() error { return errStop }\n")
	git(t, dir, "commit", "-q", "-am", "fix retry")
	put(t, dir, "stop.go", "var errStop error\n")
	put(t, dir, "notes.md", "new\n")
	return dir
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			files[rel] = string(content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertRestored(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := snapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("files after = %v, before = %v", after, before)
	}
	for path, content := range before {
		if after[path] != content {
			t.Fatalf("%s = %q after prove, want %q", path, after[path], content)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", stashDir)); !os.IsNotExist(err) {
		t.Fatalf("stash left behind: %v", err)
	}
}

func TestRunOutcomesAndTheWorkingTreeIsRestored(t *testing.T) {
	cases := []struct {
		name     string
		test     string // new content of retry_test.sh; "" leaves the test unchanged
		want     string
		buildErr bool
	}{
		{"test that catches the bug", "grep -q 'return errStop' retry.go\n", OutcomeProven, false},
		{"test that passes on the old code", "grep -q 'func Retry' retry.go && true\n", OutcomeNotRedOnOld, false},
		{"test that fails on the new code", "grep -q 'return nil' retry.go\n", OutcomeFailsOnNew, false},
		{"red only because a new name is undefined", "grep -q errStop stop.go || { echo 'undefined: errStop'; exit 1; }\n", OutcomeProven, true},
		{"no test changed", "", OutcomeNoTestChanged, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t)
			if tc.test != "" {
				put(t, dir, "retry_test.sh", tc.test)
			}
			before := snapshot(t, dir)
			report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "sh retry_test.sh"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Outcome != tc.want || report.BuildErrorSuspected != tc.buildErr {
				t.Fatalf("report = %+v, want %s (build error %v)", report, tc.want, tc.buildErr)
			}
			assertRestored(t, dir, before)
		})
	}
}

// The old-code run must see: committed code at base, an added file gone, and
// uncommitted non-test edits reverted too; the test file stays new.
func TestOldRunSeesEveryNonTestChangeReverted(t *testing.T) {
	dir := fixture(t)
	put(t, dir, "retry_test.sh", `grep -q 'return errStop' retry.go || { test ! -e stop.go && grep -qx old notes.md && echo OLD-STATE-OK; exit 1; }`+"\n")
	before := snapshot(t, dir)
	report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "sh retry_test.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeProven || !strings.Contains(report.OldOutput, "OLD-STATE-OK") {
		t.Fatalf("report = %+v", report)
	}
	for _, p := range []string{"retry.go", "stop.go", "notes.md"} {
		if !strings.Contains(strings.Join(report.RevertedFiles, " "), p) {
			t.Fatalf("reverted = %v, missing %s", report.RevertedFiles, p)
		}
	}
	assertRestored(t, dir, before)
}

// Ctrl-C during the old-code run must still put the new code back.
func TestCancelDuringOldRunStillRestores(t *testing.T) {
	dir := fixture(t)
	put(t, dir, "retry_test.sh", "grep -q 'return errStop' retry.go || sleep 30\n")
	before := snapshot(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, ".git", stashDir, manifestName)); err == nil {
				time.Sleep(200 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	start := time.Now()
	if _, err := Run(ctx, Options{Dir: dir, Base: "main", Test: "sh retry_test.sh"}); err == nil {
		t.Fatal("cancelled run returned no error")
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("cancel did not stop the test process")
	}
	assertRestored(t, dir, before)
}

// A crash between reverting and restoring leaves the stash; the next run must
// refuse, and --restore must bring the new code back.
func TestLeftoverStashBlocksRunsUntilRestored(t *testing.T) {
	dir := fixture(t)
	before := snapshot(t, dir)
	r, err := open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	mergeBase := strings.TrimSpace(git(t, dir, "merge-base", "main", "HEAD"))
	if err := swapToBase(r, mergeBase, []string{"retry.go", "stop.go", "notes.md"}); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dir)["retry.go"]; !strings.Contains(got, "return nil") {
		t.Fatalf("swap did not revert retry.go: %q", got)
	}
	if _, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "true"}); err == nil || !strings.Contains(err.Error(), "--restore") {
		t.Fatalf("Run with leftover stash err = %v", err)
	}
	if err := Restore(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	assertRestored(t, dir, before)
	if err := Restore(context.Background(), dir); err != nil {
		t.Fatalf("second Restore = %v, want no-op", err)
	}
}
