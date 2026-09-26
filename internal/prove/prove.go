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
	// OutcomeNotRun: in per-test mode, the command's output on the new code
	// never mentioned the test, so it probably selected nothing.
	OutcomeNotRun = "test_not_run"

	// NamePlaceholder in Options.Test switches to per-test mode.
	NamePlaceholder = "{name}"

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
	// Tests holds one result per test in per-test mode. The overall Outcome is
	// proven only when every test is.
	Tests []TestResult `json:"tests,omitempty"`
}

// TestResult is one test's red/green result in per-test mode.
type TestResult struct {
	Name                string `json:"name"`
	Outcome             string `json:"outcome"`
	BuildErrorSuspected bool   `json:"build_error_suspected"`
	NewOutput           string `json:"new_output,omitempty"`
	OldOutput           string `json:"old_output,omitempty"`
}

// Options configure one run. Test is run with `sh -c` at the repository root.
// If Test contains {name}, each test is proven on its own: the command is run
// once per name with {name} replaced, and every test must go red by itself.
// Names are Each, or when Each is empty the tests the change adds or edits.
type Options struct {
	Dir     string
	Base    string
	Test    string
	Each    []string
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
	release, err := acquire(r)
	if err != nil {
		return Report{}, err
	}
	defer release()
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

	if !strings.Contains(opts.Test, NamePlaceholder) {
		return runWhole(ctx, r, mergeBase, opts, report)
	}
	names := opts.Each
	if len(names) == 0 {
		if names, err = ChangedTestNames(r.git, mergeBase, r.root, report.TestFiles); err != nil {
			return Report{}, err
		}
		if len(names) == 0 {
			return Report{}, errors.New("{name} given but no added or edited test functions were found in the change; pass --each NAME,...")
		}
	}
	return runEach(ctx, r, mergeBase, opts, report, names)
}

func runWhole(ctx context.Context, r repo, mergeBase string, opts Options, report Report) (Report, error) {
	newOut, newErr := runTest(ctx, r.root, opts.Test, opts.Timeout)
	report.NewOutput = tail(newOut)
	if newErr != nil {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		report.Outcome = OutcomeFailsOnNew
		return report, nil
	}
	var oldOut string
	var oldErr error
	if err := onBase(ctx, r, mergeBase, report.RevertedFiles, func() {
		oldOut, oldErr = runTest(ctx, r.root, opts.Test, opts.Timeout)
	}); err != nil {
		return Report{}, err
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

// runEach runs every test on the new code first, then reverts once and runs
// the ones that passed on the old code, so the tree is swapped a single time.
func runEach(ctx context.Context, r repo, mergeBase string, opts Options, report Report, names []string) (Report, error) {
	results := make([]TestResult, len(names))
	var candidates []int
	for i, name := range names {
		results[i].Name = name
		out, err := runTest(ctx, r.root, strings.ReplaceAll(opts.Test, NamePlaceholder, name), opts.Timeout)
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		results[i].NewOutput = tail(out)
		switch {
		case err != nil:
			results[i].Outcome = OutcomeFailsOnNew
		case !strings.Contains(out, name):
			results[i].Outcome = OutcomeNotRun
		default:
			candidates = append(candidates, i)
		}
	}
	if len(candidates) > 0 {
		if err := onBase(ctx, r, mergeBase, report.RevertedFiles, func() {
			for _, i := range candidates {
				if ctx.Err() != nil {
					return
				}
				out, err := runTest(ctx, r.root, strings.ReplaceAll(opts.Test, NamePlaceholder, names[i]), opts.Timeout)
				results[i].OldOutput = tail(out)
				if err == nil {
					results[i].Outcome = OutcomeNotRedOnOld
					continue
				}
				results[i].Outcome = OutcomeProven
				results[i].BuildErrorSuspected = buildError.MatchString(out)
			}
		}); err != nil {
			return Report{}, err
		}
	}
	report.Tests = results
	report.Outcome = OutcomeProven
	for _, worst := range []string{OutcomeNotRedOnOld, OutcomeNotRun, OutcomeFailsOnNew} {
		for _, res := range results {
			if res.Outcome == worst {
				report.Outcome = worst
			}
		}
	}
	for _, res := range results {
		report.BuildErrorSuspected = report.BuildErrorSuspected || res.BuildErrorSuspected
	}
	return report, nil
}

// onBase reverts the non-test files to the merge base, calls fn, and restores
// them even if the caller cancelled: the working tree must come back.
func onBase(ctx context.Context, r repo, mergeBase string, paths []string, fn func()) error {
	if err := swapToBase(r, mergeBase, paths); err != nil {
		if errors.Is(err, errStashTaken) {
			return err
		}
		if restoreErr := Restore(context.WithoutCancel(ctx), r.root); restoreErr != nil {
			return fmt.Errorf("revert to base failed: %v; restore also failed: %w", err, restoreErr)
		}
		return fmt.Errorf("revert to base failed (files restored): %w", err)
	}
	fn()
	if err := Restore(context.WithoutCancel(ctx), r.root); err != nil {
		return fmt.Errorf("restore failed; recover with `test-check prove --restore`: %w", err)
	}
	return ctx.Err()
}

var (
	goTestDecl = regexp.MustCompile(`^func (Test\w+)\(`)
	pyTestDecl = regexp.MustCompile(`^\s*(?:async\s+)?def (test_\w+)\(`)
)

// ChangedTestNames lists the Go and Python test functions the change adds or
// edits. Within each hunk, a changed line belongs to the nearest test
// declaration above it — the hunk header's function, until the hunk declares
// another — so an unchanged test merely preceding new ones is not selected.
// Untracked test files contribute all of their tests.
func ChangedTestNames(git func(...string) (string, error), mergeBase, root string, testFiles []string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	decl := func(line string) string {
		for _, re := range []*regexp.Regexp{goTestDecl, pyTestDecl} {
			if m := re.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
		return ""
	}
	if len(testFiles) == 0 {
		return nil, nil
	}
	diff, err := git(append([]string{"diff", "--no-color", "--no-ext-diff", mergeBase, "--"}, testFiles...)...)
	if err != nil {
		return nil, err
	}
	current := ""
	// File headers ("+++ b/x", "--- a/x") appear only between "diff --git" and
	// the first hunk; inside a hunk, a line starting "++" or "--" is content.
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			current, inHunk = "", false
		case !inHunk && (strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---")):
		case !inHunk && !strings.HasPrefix(line, "@@"):
			// index, mode and rename lines of the file header
		case strings.HasPrefix(line, "@@"):
			current, inHunk = "", true
			if parts := strings.SplitN(line, "@@", 3); len(parts) == 3 {
				current = decl(strings.TrimSpace(parts[2]))
			}
		case line == "":
		default:
			body := line[1:]
			if name := decl(body); name != "" {
				switch line[0] {
				case '+':
					current = name
					add(name)
				case '-':
					// A removed declaration: the lines below it belong to a test
					// the change deleted (or renamed, and the "+" line names the
					// new one). Nothing here can be run on the new code.
					current = ""
				default:
					current = name
				}
				continue
			}
			if (line[0] == '+' || line[0] == '-') && strings.TrimSpace(body) != "" {
				add(current)
			}
		}
	}
	untracked, err := git(append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, testFiles...)...)
	if err != nil {
		return nil, err
	}
	for _, p := range source.SplitZ(untracked) {
		content, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(content), "\n") {
			add(decl(line))
		}
	}
	return names, nil
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
		if errors.Is(err, fs.ErrExist) {
			return errStashTaken
		}
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
		if err := writeBase(r, mergeBase, p); err != nil {
			return err
		}
	}
	return nil
}

// writeBase puts one path back to its merge-base version: a symlink as a
// symlink, and deleted only when the base does not have the path.
func writeBase(r repo, mergeBase, p string) error {
	full := filepath.Join(r.root, p)
	listing, err := r.git("ls-tree", mergeBase, "--", p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(listing) == "" {
		// Not in the base: the change added it.
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		return nil
	}
	old, err := r.git("show", mergeBase+":"+p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(listing, "120000"):
		return os.Symlink(old, full)
	case strings.HasPrefix(listing, "100755"):
		return os.WriteFile(full, []byte(old), 0o755)
	default:
		return os.WriteFile(full, []byte(old), 0o644)
	}
}

// Restore puts back every file a prove run reverted, from the manifest under
// the git dir, then removes the stash. It is safe to run again if it fails
// half way, and a no-op when nothing is stashed.
func Restore(ctx context.Context, dir string) error {
	r, err := open(ctx, dir)
	if err != nil {
		return err
	}
	if pid := lockHolder(filepath.Join(r.gitDir, lockName)); pid > 0 && pid != os.Getpid() && processAlive(pid) {
		return fmt.Errorf("prove run (pid %d) is still using this checkout; it restores its own files", pid)
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
			if err := os.RemoveAll(full); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// A test may have created a directory where a reverted file was.
		if err := os.RemoveAll(full); err != nil {
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
func runTest(ctx context.Context, root, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
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

// errStashTaken: another run's stash exists; it must not be restored or
// removed by this run.
var errStashTaken = errors.New("another prove run's stash exists under the git dir; wait for it, or run `test-check prove --restore` if it crashed")

const lockName = "test-check-prove.lock"

// acquire takes the per-worktree prove lock with O_EXCL, so two runs never
// share the stash. A lock whose process is gone is taken over.
func acquire(r repo) (func(), error) {
	path := filepath.Join(r.gitDir, lockName)
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString(strconv.Itoa(os.Getpid()))
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if pid := lockHolder(path); pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("another prove run (pid %d) is using this checkout", pid)
		}
		_ = os.Remove(path)
	}
	return nil, errors.New("could not take the prove lock")
}

func lockHolder(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}
