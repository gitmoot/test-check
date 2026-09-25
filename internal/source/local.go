package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gitmoot/test-check/internal/check"
)

// Local reads the change in a git checkout: everything since the merge base
// with base, including uncommitted and untracked files, so it can run before a
// commit exists. An empty base means the remote's default branch.
func Local(ctx context.Context, dir, base, title string) (check.Input, error) {
	git := Git(ctx, dir)
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return check.Input{}, err
	}
	root = strings.TrimSpace(root)
	if base == "" {
		base, err = DefaultBase(git)
		if err != nil {
			return check.Input{}, err
		}
	}
	diff, err := git("diff", "--merge-base", base, "--no-color", "--no-ext-diff", "--no-renames")
	if err != nil {
		return check.Input{}, err
	}
	files := splitUnifiedDiff(diff)
	untracked, err := git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return check.Input{}, err
	}
	for _, p := range SplitZ(untracked) {
		content, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return check.Input{}, err
		}
		patch := ""
		if !strings.ContainsRune(string(content), 0) {
			patch = newFilePatch(string(content))
		}
		files = append(files, check.File{Path: p, Patch: patch})
	}
	if len(files) == 0 {
		return check.Input{}, fmt.Errorf("no changes since %s", base)
	}
	tracked, err := git("ls-files", "-z")
	if err != nil {
		return check.Input{}, err
	}
	all := append(SplitZ(tracked), SplitZ(untracked)...)
	head, _ := git("rev-parse", "HEAD")
	if title == "" {
		if subject, err := git("log", "-1", "--format=%s", base+"..HEAD"); err == nil {
			title = strings.TrimSpace(subject)
		}
	}
	return withNearby(check.Input{Title: title, Head: strings.TrimSpace(head), Files: files}, all, func(p string) (string, error) {
		content, err := os.ReadFile(filepath.Join(root, p))
		return string(content), err
	}), nil
}

// Git returns a runner for git commands in dir; errors carry git's stderr.
func Git(ctx context.Context, dir string) func(args ...string) (string, error) {
	return func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
			}
			return "", err
		}
		return string(out), nil
	}
}

// DefaultBase is the remote's default branch, else a local main or master.
func DefaultBase(git func(...string) (string, error)) (string, error) {
	if ref, err := git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(ref), nil
	}
	for _, candidate := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := git("rev-parse", "--verify", "--quiet", candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("cannot find a base branch; pass --base")
}

// SplitZ splits NUL-separated git output.
func SplitZ(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// withNearby attaches the nearest existing test files. A test file that cannot
// be read is skipped rather than failing the whole check.
func withNearby(in check.Input, all []string, read func(string) (string, error)) check.Input {
	changed := make([]string, len(in.Files))
	for i, f := range in.Files {
		changed[i] = f.Path
	}
	for _, p := range check.NearbyTestPaths(changed, all) {
		content, err := read(p)
		if err != nil {
			continue
		}
		in.Nearby = append(in.Nearby, check.NearbyTest{Path: p, Content: content})
	}
	return in
}
