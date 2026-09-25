// Package prove checks that a change's tests go red on the old code and green
// on the new: it runs the test command on the working tree, then temporarily
// puts the non-test files back to their base versions, runs it again, and
// restores them. Test files and everything else stay as they are, so build
// caches, dependencies and the environment are identical for both runs.
package prove

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/test-check/internal/check"
	"github.com/gitmoot/test-check/internal/source"
)

const (
	OutcomeProven        = "proven"
	OutcomeNotRedOnOld   = "not_red_on_old"
	OutcomeFailsOnNew    = "fails_on_new"
	OutcomeNoTestChanged = "no_test_in_change"
	OutcomeNoCodeChanged = "no_code_in_change"

	// stashDir is under the per-worktree git dir, so it never shows up as a
	// change and cannot be committed.
	stashDir     = "test-check-prove"
	manifestName = "manifest.json"
	tailBytes    = 2000
)

// Report is the outcome of one prove run.
type Report struct {
	Outcome string `json:"outcome"`
	// BuildErrorSuspected: the old-code run failed with what looks like a
	// compile or import error, not a failing assertion. A test of a brand-new
	// API always does this; it proves nothing about a regression.
	BuildErrorSuspected bool     `json:"build_error_suspected"`
	TestFiles           []string `json:"test_files"`
	RevertedFiles       []string `json:"reverted_files"`
	NewOutput           string   `json:"new_output,omitempty"`
	OldOutput           string   `json:"old_output,omitempty"`
}

// Options configure one run. Test is run with `sh -c` at the repository root.
type Options struct {
	Dir     string
	Base    string
	Test    string
	Timeout time.Duration
}

var testSupport = regexp.MustCompile(`(^|/)(testdata|fixtures?|__snapshots__|__fixtures__)/`)

// IsTestSide reports whether a changed path belongs to the tests: test files
// and their fixtures are kept at their new versions for the old-code run.
func IsTestSide(p string) bool {
	return check.IsTestFile(p) || testSupport.MatchString(p)
}

type repo struct {
	git    func(...string) (string, error)
	root   string
	gitDir string
}

func open(ctx context.Context, dir string) (repo, error) {
	git := source.Git(ctx, dir)
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return repo{}, err
	}
	gitDir, err := git("rev-parse", "--absolute-git-dir")
	if err != nil {
		return repo{}, err
	}
	return repo{git: git, root: strings.TrimSpace(root), gitDir: strings.TrimSpace(gitDir)}, nil
}

// Run proves one change. It returns an error only when it could not run the
// check; a test that does not go red is a Report, not an error.
func Run(ctx context.Context, opts Options) (Report, error) {
	if strings.TrimSpace(opts.Test) == "" {
		return Report{}, errors.New("no test command")
	}
	r, err := open(ctx, opts.Dir)
	if err != nil {
		return Report{}, err
	}
	stash := filepath.Join(r.gitDir, stashDir)
	if _, err := os.Stat(stash); err == nil {
		return Report{}, fmt.Errorf("a previous prove run did not restore its files; run `test-check prove --restore` first (%s)", stash)
	}
	base := opts.Base
	if base == "" {
		if base, err = source.DefaultBase(r.git); err != nil {
			return Report{}, err
		}
	}
	mergeBase, err := r.git("merge-base", base, "HEAD")
	if err != nil {
		return Report{}, err
	}
	mergeBase = strings.TrimSpace(mergeBase)
	changed, err := changedPaths(r, mergeBase)
	if err != nil {
		return Report{}, err
	}
	var report Report
	for _, p := range changed {
		if IsTestSide(p) {
			report.TestFiles = append(report.TestFiles, p)
		} else {
			report.RevertedFiles = append(report.RevertedFiles, p)
		}
	}
	switch {
	case len(report.TestFiles) == 0:
		report.Outcome = OutcomeNoTestChanged
		return report, nil
	case len(report.RevertedFiles) == 0:
		report.Outcome = OutcomeNoCodeChanged
		return report, nil
	}

	newOut, newErr := runTest(ctx, r.root, opts)
	report.NewOutput = tail(newOut)
	if newErr != nil {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		report.Outcome = OutcomeFailsOnNew
		return report, nil
	}

	if err := swapToBase(r, mergeBase, report.RevertedFiles); err != nil {
		if restoreErr := Restore(context.WithoutCancel(ctx), opts.Dir); restoreErr != nil {
			return Report{}, fmt.Errorf("revert to base failed: %v; restore also failed: %w", err, restoreErr)
		}
		return Report{}, fmt.Errorf("revert to base failed (files restored): %w", err)
	}
	oldOut, oldErr := runTest(ctx, r.root, opts)
	// Restore even if the caller cancelled: the working tree must come back.
	if err := Restore(context.WithoutCancel(ctx), opts.Dir); err != nil {
		return Report{}, fmt.Errorf("restore failed; recover with `test-check prove --restore`: %w", err)
	}
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	report.OldOutput = tail(oldOut)
	if oldErr == nil {
		report.Outcome = OutcomeNotRedOnOld
		return report, nil
	}
	report.Outcome = OutcomeProven
	report.BuildErrorSuspected = buildError.MatchString(oldOut)
	return report, nil
}

// changedPaths lists every path that differs from the merge base, including
// uncommitted, deleted and untracked files.
func changedPaths(r repo, mergeBase string) ([]string, error) {
	tracked, err := r.git("diff", "--name-only", "--no-renames", "-z", mergeBase)
	if err != nil {
		return nil, err
	}
	untracked, err := r.git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return append(source.SplitZ(tracked), source.SplitZ(untracked)...), nil
}

type entry struct {
	Path string `json:"path"`
	// Existed is false for a file the change added; restore recreates it.
	Existed bool        `json:"existed"`
	Mode    fs.FileMode `json:"mode"`
	Link    string      `json:"link,omitempty"`
	Saved   string      `json:"saved,omitempty"`
}

// swapToBase saves the current version of each path under the git dir, writes
// the manifest BEFORE touching the working tree, then puts each path back to
// its base version (deleting it if the base did not have it).
func swapToBase(r repo, mergeBase string, paths []string) error {
	stash := filepath.Join(r.gitDir, stashDir)
	if err := os.Mkdir(stash, 0o700); err != nil {
		return err
	}
	entries := make([]entry, len(paths))
	for i, p := range paths {
		full := filepath.Join(r.root, p)
		info, err := os.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			entries[i] = entry{Path: p}
			continue
		case err != nil:
			return err
		}
		entries[i] = entry{Path: p, Existed: true, Mode: info.Mode()}
		if info.Mode()&fs.ModeSymlink != 0 {
			if entries[i].Link, err = os.Readlink(full); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		entries[i].Saved = strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(stash, entries[i].Saved), content, 0o600); err != nil {
			return err
		}
	}
	manifest, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := writeSynced(filepath.Join(stash, manifestName), manifest); err != nil {
		return err
	}
	for _, p := range paths {
		full := filepath.Join(r.root, p)
		old, err := r.git("show", mergeBase+":"+p)
		if err != nil {
			// Not in the base: the change added it.
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		mode, err := baseMode(r, mergeBase, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		_ = os.Remove(full)
		if err := os.WriteFile(full, []byte(old), mode); err != nil {
			return err
		}
	}
	return nil
}

func baseMode(r repo, mergeBase, p string) (fs.FileMode, error) {
	out, err := r.git("ls-tree", mergeBase, "--", p)
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(out, "100755") {
		return 0o755, nil
	}
	return 0o644, nil
}

// Restore puts back every file a prove run reverted, from the manifest under
// the git dir, then removes the stash. It is safe to run again if it fails
// half way, and a no-op when nothing is stashed.
func Restore(ctx context.Context, dir string) error {
	r, err := open(ctx, dir)
	if err != nil {
		return err
	}
	stash := filepath.Join(r.gitDir, stashDir)
	raw, err := os.ReadFile(filepath.Join(stash, manifestName))
	if errors.Is(err, fs.ErrNotExist) {
		// A crash before the manifest was written left no working-tree change.
		return os.RemoveAll(stash)
	}
	if err != nil {
		return err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("unreadable manifest %s: %w", stash, err)
	}
	for _, e := range entries {
		full := filepath.Join(r.root, e.Path)
		if !e.Existed {
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if e.Link != "" {
			if err := os.Symlink(e.Link, full); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(filepath.Join(stash, e.Saved))
		if err != nil {
			return err
		}
		if err := os.WriteFile(full, content, e.Mode.Perm()); err != nil {
			return err
		}
	}
	return os.RemoveAll(stash)
}

func writeSynced(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// runTest runs the command in its own process group, so a timeout or cancel
// stops everything it started, and returns combined output.
func runTest(ctx context.Context, root string, opts Options) (string, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", opts.Test)
	cmd.Dir = root
	setProcessGroup(cmd)
	// A grandchild holding the output pipe must not block the return.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return string(out), fmt.Errorf("test command timed out after %s", timeout)
	}
	return string(out), err
}

var buildError = regexp.MustCompile(`(?m)(undefined: |cannot find package|no required module|build failed|compilation failed|error TS\d+|Cannot find module|ModuleNotFoundError|ImportError: |NameError: name|error: cannot find '|error\[E0(425|433)\]|SyntaxError: )`)

func tail(out string) string {
	if len(out) <= tailBytes {
		return out
	}
	return "..." + strings.ToValidUTF8(out[len(out)-tailBytes:], "")
}
