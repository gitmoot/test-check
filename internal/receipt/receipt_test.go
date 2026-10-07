package receipt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Concurrent invocations must not overwrite one another or expose a receipt
// whose detail link is missing. The warning distinguishes each invocation.
func TestConcurrentReceiptsArePrivateAndComplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "receipts")
	const runs = 24
	var workers sync.WaitGroup
	var paths sync.Map
	stop := make(chan struct{})
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			files, _ := filepath.Glob(filepath.Join(root, "*", "receipt.json"))
			for _, file := range files {
				var record Record
				data, err := os.ReadFile(file)
				if err != nil {
					t.Errorf("read published receipt: %v", err)
					return
				}
				if err := json.Unmarshal(data, &record); err != nil {
					t.Errorf("partially published receipt: %v", err)
					return
				}
				var detail details
				data, err = os.ReadFile(record.OutputRef)
				if err != nil {
					t.Errorf("receipt published before detail: %v", err)
					return
				}
				if err := json.Unmarshal(data, &detail); err != nil {
					t.Errorf("partially published detail: %v", err)
					return
				}
			}
		}
	}()
	for i := range runs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			warning := fmt.Sprintf("run %d: old runner failed to build", i)
			path, err := Save(root, Record{Outcome: "inconclusive", Warnings: []string{warning}}, []Output{{Label: "old", Text: warning}})
			if err != nil {
				t.Error(err)
				return
			}
			if _, duplicate := paths.LoadOrStore(path.Receipt, true); duplicate {
				t.Error("concurrent invocation reused a receipt path")
			}
			var record Record
			readJSON(t, path.Receipt, &record)
			if len(record.Warnings) != 1 || record.Warnings[0] != warning {
				t.Error("warning lost or overwritten by another invocation")
			}
			if record.OutputRef != path.Details {
				t.Error("receipt does not link its own detail")
			}
			var detail details
			readJSON(t, record.OutputRef, &detail)
			if len(detail.Outputs) != 1 || detail.Outputs[0].Text != warning {
				t.Error("output lost or overwritten by another invocation")
			}
			assertMode(t, path.Receipt, 0o600)
			assertMode(t, path.Details, 0o600)
			assertMode(t, filepath.Dir(path.Receipt), 0o700)
		}()
	}
	workers.Wait()
	close(stop)
	<-observerDone
	assertMode(t, root, 0o700)
	files, err := filepath.Glob(filepath.Join(root, "*", "receipt.json"))
	if err != nil || len(files) != runs {
		t.Fatalf("retained receipts: %d, error: %v", len(files), err)
	}
}

func TestUnsafeStorageDoesNotTouchCheckoutOrChangePermissions(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "checkout")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	// Stand in for recovery state: saving a failed run must not touch it.
	recovery := filepath.Join(checkout, "recovery")
	if err := os.WriteFile(recovery, []byte("restore this original"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(base, "public")
	if err := os.Mkdir(unsafe, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	privateLink := filepath.Join(base, "private-link")
	if err := os.Symlink(private, privateLink); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		unsafe,
		privateLink,
		filepath.Join(checkout, "receipts"),
		filepath.Join(link, "receipts"),
		recovery, // An occupied path must fail even when tests run as root.
	}
	for _, root := range cases {
		path, err := Save(root, Record{Checkout: checkout, Outcome: "failed"}, nil)
		if err == nil || path != (Paths{}) {
			t.Errorf("unsafe storage %q returned paths %#v, error %v", root, path, err)
		}
	}
	assertMode(t, unsafe, 0o755)
	original, err := os.ReadFile(recovery)
	if err != nil || string(original) != "restore this original" {
		t.Fatalf("receipt save changed recovery state: %q, %v", original, err)
	}
	entries, err := os.ReadDir(checkout)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed receipt created artifacts in checkout: %v, %v", entries, err)
	}
}

func TestDefaultStorageIsPrivateAndIndependentOfCheckout(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	checkout := t.TempDir()
	path, err := Save("", Record{Repo: checkout}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "test-check", "receipts")
	if !strings.HasPrefix(path.Receipt, root+string(os.PathSeparator)) {
		t.Fatalf("receipt not in durable state directory: %s", path.Receipt)
	}
	assertMode(t, filepath.Dir(root), 0o700)
	assertMode(t, root, 0o700)
	entries, err := os.ReadDir(checkout)
	if err != nil || len(entries) != 0 {
		t.Fatalf("receipt storage touched checkout: %v, %v", entries, err)
	}
	// Tool-owned parent may not silently relax or repair existing permissions.
	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("", Record{Repo: checkout}, nil); err == nil {
		t.Fatal("accepted unsafe preexisting tool state directory")
	}
	assertMode(t, filepath.Dir(root), 0o755)
}

func TestBoundedDetailDoesNotAlterOriginalDiagnostic(t *testing.T) {
	text := strings.Repeat("private diagnostic ", 400) + "final assertion failure"
	outputs := make([]Output, outputLimit+1)
	for i := range outputs {
		outputs[i] = Output{Label: strings.Repeat("x", labelBytes+1), Text: text}
	}
	path, err := Save(filepath.Join(t.TempDir(), "receipts"), Record{Warnings: []string{"original warning"}}, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var detail details
	readJSON(t, path.Details, &detail)
	if !detail.Truncated || detail.OmittedOutputs != 1 || len(detail.Outputs) != outputLimit {
		t.Fatal("unbounded detail or silent output omission")
	}
	for _, output := range detail.Outputs {
		if len(output.Text) > outputBytes+3 || !strings.HasSuffix(output.Text, "final assertion failure") {
			t.Fatal("detail did not retain bounded diagnostic tail")
		}
		if len(output.Label) > labelBytes+3 {
			t.Fatal("unbounded output label")
		}
	}
	for _, output := range outputs {
		if output.Text != text || len(output.Label) != labelBytes+1 {
			t.Fatal("saving a receipt mutated the caller's original diagnostic")
		}
	}
}

// A failed publication must leave earlier evidence intact and must not return
// a path that callers could mistake for a successfully saved receipt.
func TestFailedSaveDoesNotReplaceEarlierEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "receipts")
	first, err := Save(root, Record{Outcome: "original"}, []Output{{Text: "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(first.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	// time.Time's JSON encoder rejects this year after details have been
	// written, exercising cleanup of a controlled late write failure.
	path, err := Save(root, Record{FinishedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, nil)
	if err == nil || path != (Paths{}) {
		t.Fatalf("failed publication returned %#v, error %v", path, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed publication left artifacts: %v, %v", entries, err)
	}
	after, err := os.ReadFile(first.Receipt)
	if err != nil || string(after) != string(original) {
		t.Fatalf("failed save changed earlier evidence: %v", err)
	}
	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(occupied, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = Save(occupied, Record{Outcome: "failed"}, nil)
	if err == nil || path != (Paths{}) {
		t.Fatalf("occupied storage returned %#v, error %v", path, err)
	}
	after, err = os.ReadFile(occupied)
	if err != nil || string(after) != "do not overwrite" {
		t.Fatalf("failed storage creation changed existing file: %v", err)
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s permissions %04o, want %04o", path, info.Mode().Perm(), want)
	}
}
