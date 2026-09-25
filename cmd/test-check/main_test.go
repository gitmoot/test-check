package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/test-check/internal/check"
	"github.com/gitmoot/test-check/internal/jev"
)

type stubJudge struct {
	advice string
	err    error
}

func (s stubJudge) Evaluate(context.Context, jev.Request) (jev.Exchange, error) {
	if s.err != nil {
		return jev.Exchange{}, s.err
	}
	zero := 0.0
	return jev.Exchange{Response: jev.Response{Answers: map[string]jev.Answer{
		"advice":                {Choice: s.advice},
		"covered_by_diff_tests": {Noul: &zero},
		"low_value_tests_added": {Noul: &zero},
	}}}, nil
}

func repoWithChange(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func withJudge(t *testing.T, judge check.Judge, key string) {
	t.Helper()
	prev := newJudge
	newJudge = func(string) check.Judge { return judge }
	t.Cleanup(func() { newJudge = prev })
	t.Setenv("OPENROUTER_API_KEY", key)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestRunPrintsAdviceForALocalChange(t *testing.T) {
	withJudge(t, stubJudge{advice: check.AdviceNewTest}, "k")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dir", repoWithChange(t), "--base", "main"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "advice: new_regression_test") || !strings.Contains(stdout.String(), "goes red on the old code") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// Without JEV the tool must not print advice it did not get.
func TestRunWithoutJEVExitsUnavailableWithNoAdvice(t *testing.T) {
	dir := repoWithChange(t)
	for name, key := range map[string]string{"no key": "", "JEV error": "k"} {
		t.Run(name, func(t *testing.T) {
			withJudge(t, stubJudge{err: errors.New("HTTP 503")}, key)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"--dir", dir, "--base", "main"}, &stdout, &stderr); code != exitUnavailable {
				t.Fatalf("exit %d, want %d", code, exitUnavailable)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "four questions") {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestAPIKeyFallsBackToGitmootKeychain(t *testing.T) {
	config := t.TempDir()
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", config)
	if err := os.MkdirAll(filepath.Join(config, "gitmoot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "gitmoot", "keychain.env"), []byte("OTHER=x\nOPENROUTER_API_KEY=from-keychain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := apiKey(); got != "from-keychain" {
		t.Fatalf("apiKey = %q", got)
	}
}

func TestRunRejectsConflictingModes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{
		{"--pr", "3"},
		{"--repo", "o/r", "--pr", "3", "--compare", "a...b"},
		{"--repo", "o/r", "--compare", "a..b"},
	} {
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Fatalf("run(%v) = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestProveExitCodes(t *testing.T) {
	dir := repoWithChange(t)
	if err := os.WriteFile(filepath.Join(dir, "a_test.sh"), []byte("grep -q 'X = 1' a.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"prove", "--dir", dir, "--base", "main", "--test", "sh a_test.sh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proven change exit %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "outcome: proven") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "a_test.sh"), []byte("grep -q 'package a' a.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"prove", "--dir", dir, "--base", "main", "--test", "sh a_test.sh"}, &stdout, &stderr); code != exitNotProven {
		t.Fatalf("weak test exit %d, want %d: %s", code, exitNotProven, stdout.String())
	}
	for _, args := range [][]string{{"prove"}, {"prove", "--restore", "--test", "true"}} {
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Fatalf("run(%v) = %d, want %d", args, code, exitUsage)
		}
	}
}
