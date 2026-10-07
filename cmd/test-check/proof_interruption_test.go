package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/test-check/internal/prove"
)

func TestRunnerDeadlinesNeverProveRegression(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, stage := range []string{"old", "new"} {
		for _, mode := range []string{"whole", "each", "pieces"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				dir := runnerFixture(t)
				proofWrite(t, dir, "value_test.sh", "echo TestValue\nif grep -q '^"+stage+"$' value.go; then sleep 30; fi\ngrep -q '^new$' value.go\n")
				args := []string{"prove", "--dir", dir, "--base", "main", "--json", "--timeout", "1s", "--test", "sh value_test.sh"}
				if mode == "each" {
					args[len(args)-1] = "sh value_test.sh # {name}"
					args = append(args, "--each", "TestValue")
				}
				if mode == "pieces" {
					args = append(args, "--pieces")
				}
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				var report struct {
					Outcome, Receipt string
					Tests            []struct{ Outcome string }
					Pieces           []struct{ Outcome string }
				}
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatalf("%v: %s %s", err, stdout.String(), stderr.String())
				}
				if code != exitSource || report.Outcome == "proven" {
					t.Fatalf("owned timeout claimed proof: exit=%d %s", code, stdout.String())
				}
				for _, child := range report.Tests {
					if child.Outcome == "proven" {
						t.Fatalf("interrupted test claimed proof: %s", stdout.String())
					}
				}
				for _, child := range report.Pieces {
					if child.Outcome == "guarded" {
						t.Fatalf("interrupted piece claimed coverage: %s", stdout.String())
					}
				}
				raw, err := os.ReadFile(report.Receipt)
				if err != nil {
					t.Fatal(err)
				}
				var receipt struct{ Outcome string }
				if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Outcome != "error" {
					t.Fatalf("timeout receipt: %s %v", raw, err)
				}
				content, err := os.ReadFile(filepath.Join(dir, "value.go"))
				if err != nil || string(content) != "new\n" {
					t.Fatalf("restoration: %q %v", content, err)
				}
			})
		}
	}
}

func TestCanceledOldRunNeverClaimsChildProof(t *testing.T) {
	for _, mode := range []string{"whole", "each", "pieces"} {
		t.Run(mode, func(t *testing.T) {
			dir := runnerFixture(t)
			ready := filepath.Join(t.TempDir(), "ready")
			proofWrite(t, dir, "value_test.sh", "echo TestValue\nif grep -q '^new$' value.go; then exit 0; fi\ntouch '"+ready+"'\nsleep 30\n")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					if _, err := os.Stat(ready); err == nil {
						cancel()
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
			opts := prove.Options{Dir: dir, Base: "main", Test: "sh value_test.sh"}
			if mode == "each" {
				opts.Test += " # {name}"
				opts.Each = []string{"TestValue"}
			}
			if mode == "pieces" {
				opts.Pieces = true
			}
			report, err := prove.Run(ctx, opts)
			<-stopped
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want caller cancellation, got %v", err)
			}
			if mode == "each" && len(report.Tests) != 1 {
				t.Fatalf("lost interrupted result: %+v", report)
			}
			if mode == "pieces" && len(report.Pieces) != 1 {
				t.Fatalf("lost interrupted piece: %+v", report)
			}
			for _, child := range report.Tests {
				if child.Outcome == "proven" {
					t.Fatalf("canceled child claimed proof: %+v", child)
				}
			}
			for _, child := range report.Pieces {
				if child.Outcome == "guarded" {
					t.Fatalf("canceled piece claimed coverage: %+v", child)
				}
			}
			content, readErr := os.ReadFile(filepath.Join(dir, "value.go"))
			if readErr != nil || string(content) != "new\n" {
				t.Fatalf("restoration: %q %v", content, readErr)
			}
		})
	}
}

func runnerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	proofGit(t, dir, "init", "-q", "-b", "main")
	proofWrite(t, dir, "value.go", "old\n")
	proofGit(t, dir, "add", ".")
	proofGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	proofWrite(t, dir, "value.go", "new\n")
	return dir
}
