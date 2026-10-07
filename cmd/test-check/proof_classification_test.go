package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the public CLI contract through interfaces available before this fix:
// a build-only old run must never return a successful proof, and diagnostic
// words in an actual assertion must not be reported as a build failure.
func TestProofDistinguishesBuildFailureFromBehavior(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cases := []struct {
		name, file, old, new, testFile, test, command string
		build                                         bool
	}{
		{"go_build", "value.go", "package value\nfunc Value() int { return 1 }\n", "package value\nfunc Value() int { return 1 }; func Added() int { return 2 }\n", "value_test.go", "package value\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Added() != 2 { t.Fatal(\"wrong value\") } }\n", "go test -count=1 -v -run '^TestValue$' .", true},
		{"go_assertion", "value.go", "package value\nfunc Value() int { return 1 }\n", "package value\nfunc Value() int { return 2 }\n", "value_test.go", "package value\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fatal(\"ImportError: expected diagnostic differs\") } }\n", "go test -count=1 -v -run '^TestValue$' .", false},
		{"python_import", "value.py", "def value(): return 1\n", "def value(): return 1\ndef added(): return 2\n", "test_value.py", "import unittest\nfrom value import added\nclass ValueTest(unittest.TestCase):\n def test_value(self): self.assertEqual(added(), 2)\n", "python3 -B -m unittest -v test_value", true},
		{"python_assertion", "value.py", "def value(): return 1\n", "def value(): return 2\n", "test_value.py", "import unittest\nfrom value import value\nclass ValueTest(unittest.TestCase):\n def test_value(self): self.assertEqual(value(), 2, 'ImportError: expected diagnostic differs')\n", "python3 -B -m unittest -v test_value", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			proofGit(t, dir, "init", "-q", "-b", "main")
			proofWrite(t, dir, "go.mod", "module value\n\ngo 1.23\n")
			proofWrite(t, dir, tc.file, tc.old)
			proofGit(t, dir, "add", ".")
			proofGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
			proofWrite(t, dir, tc.file, tc.new)
			proofWrite(t, dir, tc.testFile, tc.test)
			for _, mode := range []string{"whole", "each", "pieces"} {
				args := []string{"prove", "--dir", dir, "--base", "main", "--json", "--test", tc.command}
				if mode == "each" {
					name := "TestValue"
					if strings.HasPrefix(tc.name, "python") {
						name = "test_value"
					}
					args[len(args)-1] = strings.ReplaceAll(tc.command, name, "{name}")
					args = append(args, "--each", name)
				}
				if mode == "pieces" {
					args = append(args, "--pieces")
				}
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				var report struct {
					Outcome string `json:"outcome"`
					Build   bool   `json:"build_error_suspected"`
					Tests   []struct {
						Outcome string `json:"outcome"`
						Build   bool   `json:"build_error_suspected"`
					} `json:"tests"`
					Pieces []struct {
						Outcome string `json:"outcome"`
						Build   bool   `json:"build_error_suspected"`
					} `json:"pieces"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatalf("%s: %v: %s %s", mode, err, stdout.String(), stderr.String())
				}
				if tc.build {
					if code != exitNotProven || report.Outcome == "proven" || !report.Build {
						t.Fatalf("%s: build-only failure claimed proof: exit=%d report=%s stderr=%s", mode, code, stdout.String(), stderr.String())
					}
					for _, result := range report.Tests {
						if result.Outcome == "proven" || !result.Build {
							t.Fatalf("%s: test claimed proof: %+v", mode, result)
						}
					}
					for _, result := range report.Pieces {
						if result.Outcome == "guarded" || !result.Build {
							t.Fatalf("%s: piece claimed behavioral coverage: %+v", mode, result)
						}
					}
				} else if code != 0 || report.Outcome != "proven" || report.Build {
					t.Fatalf("%s: assertion rejected: exit=%d report=%s stderr=%s", mode, code, stdout.String(), stderr.String())
				}
				got, err := os.ReadFile(filepath.Join(dir, tc.file))
				if err != nil || string(got) != tc.new {
					t.Fatalf("%s: working file not restored: %q %v", mode, got, err)
				}
			}
		})
	}
}

func proofGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func proofWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
