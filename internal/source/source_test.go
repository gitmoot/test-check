package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/test-check/internal/check"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An agent runs test-check before committing: committed, staged, unstaged,
// and untracked changes since the base must all be seen, and nothing before it.
func TestLocalSeesEveryUncommittedKindOfChangeSinceTheBase(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "pkg/retry.go", "package pkg\n\nfunc Retry() error { return nil }\n")
	write(t, dir, "pkg/retry_test.go", "package pkg\n\nfunc TestRetryStops(t *testing.T) {}\n")
	write(t, dir, "pkg/old.go", "package pkg\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	gitIn(t, dir, "checkout", "-q", "-b", "fix")
	write(t, dir, "pkg/old.go", "package pkg\n\nvar committed = 1\n")
	gitIn(t, dir, "commit", "-q", "-am", "Fix retry loop")
	write(t, dir, "pkg/retry.go", "package pkg\n\nfunc Retry() error { return errStop }\n")
	write(t, dir, "pkg/stop.go", "package pkg\n\nvar errStop error\n")

	in, err := Local(context.Background(), dir, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range in.Files {
		got[f.Path] = f.Patch
	}
	if len(got) != 3 || !strings.Contains(got["pkg/old.go"], "+var committed = 1") ||
		!strings.Contains(got["pkg/retry.go"], "+func Retry() error { return errStop }") ||
		!strings.Contains(got["pkg/stop.go"], "+var errStop error") {
		t.Fatalf("files = %#v", got)
	}
	if in.Title != "Fix retry loop" {
		t.Fatalf("title = %q", in.Title)
	}
	if len(in.Nearby) != 1 || in.Nearby[0].Path != "pkg/retry_test.go" || !strings.Contains(in.Nearby[0].Content, "TestRetryStops") {
		t.Fatalf("nearby = %+v", in.Nearby)
	}
}

func TestLocalWithNoChangesIsAnError(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	if _, err := Local(context.Background(), dir, "main", ""); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitUnifiedDiffKeepsDeletionsAndBinaryPaths(t *testing.T) {
	diff := "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package gone\n" +
		"diff --git a/logo.png b/logo.png\nBinary files a/logo.png and b/logo.png differ\n"
	got := splitUnifiedDiff(diff)
	want := []check.File{{Path: "gone.go", Patch: "@@ -1 +0,0 @@\n-package gone"}, {Path: "logo.png", Patch: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitUnifiedDiff = %#v", got)
	}
}

func TestPullRequestReadsFilesAcrossPagesAndNearbyTestsAtHead(t *testing.T) {
	var calls []string
	prev := Gh
	Gh = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case call == "api repos/o/r/pulls/7":
			return []byte(`{"title":"Stop retry loop","head":{"sha":"abc"}}`), nil
		case call == "api repos/o/r/pulls/7/files?per_page=100&page=1":
			page := make([]string, 100)
			for i := range page {
				page[i] = `{"filename":"src/gen` + strconv.Itoa(i) + `.go","patch":"+x"}`
			}
			page[0] = `{"filename":"pkg/retry.go","patch":"@@ -1 +1 @@\n-a\n+b"}`
			return []byte("[" + strings.Join(page, ",") + "]"), nil
		case call == "api repos/o/r/pulls/7/files?per_page=100&page=2":
			return []byte(`[{"filename":"README.md","patch":"+x"}]`), nil
		case call == "api repos/o/r/git/trees/abc?recursive=1":
			return []byte(`{"tree":[{"path":"pkg","type":"tree"},{"path":"pkg/retry.go","type":"blob"},{"path":"pkg/retry_test.go","type":"blob"},{"path":"pkg/broken_test.go","type":"blob"}]}`), nil
		case call == "api -H Accept: application/vnd.github.raw repos/o/r/contents/pkg/retry_test.go?ref=abc":
			return []byte("func TestRetryStops(t *testing.T) {}"), nil
		case strings.Contains(call, "contents/pkg/broken_test.go"):
			return nil, errors.New("HTTP 404")
		}
		return nil, errors.New("unexpected gh call: " + call)
	}
	t.Cleanup(func() { Gh = prev })

	in, err := PullRequest(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "Stop retry loop" || in.Head != "abc" || len(in.Files) != 101 || in.Files[100].Path != "README.md" {
		t.Fatalf("input = %+v", in)
	}
	if len(in.Nearby) != 1 || in.Nearby[0].Path != "pkg/retry_test.go" {
		t.Fatalf("nearby = %+v (an unreadable test file must be skipped, not fatal)", in.Nearby)
	}
}
