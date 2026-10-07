package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProofAutomaticallyPersistsPrivateReceipt(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := receiptFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"prove", "--dir", dir, "--base", "main", "--json", "--each", "TestValue", "--test", "echo '=== RUN   {name}'; sh value_test.sh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d: %s %s", code, stdout.String(), stderr.String())
	}
	var response struct {
		Outcome, Receipt string
		ReceiptStatus    string `json:"receipt_status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Outcome != "proven" || response.Receipt == "" || response.ReceiptStatus != "saved" {
		t.Fatalf("missing durable evidence: %s", stdout.String())
	}
	if !strings.HasPrefix(response.Receipt, state+string(os.PathSeparator)) || strings.HasPrefix(response.Receipt, dir+string(os.PathSeparator)) {
		t.Fatalf("unexpected storage: %s", response.Receipt)
	}
	raw, err := os.ReadFile(response.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Command, Head, Outcome, OutputRef string
	}
	// Decode the documented snake-case keys, not private package fields.
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data["command"], &record.Command)
	_ = json.Unmarshal(data["head"], &record.Head)
	_ = json.Unmarshal(data["outcome"], &record.Outcome)
	_ = json.Unmarshal(data["output_ref"], &record.OutputRef)
	var tests []struct {
		Name string `json:"name"`
		New  string `json:"new_execution"`
		Old  string `json:"old_execution"`
	}
	if err := json.Unmarshal(data["tests"], &tests); err != nil {
		t.Fatal(err)
	}
	if record.Command == "" || len(record.Head) != 40 || record.Outcome != "proven" || len(tests) != 1 || tests[0].Name != "TestValue" || tests[0].New != "observed" || tests[0].Old != "observed" {
		t.Fatalf("incomplete receipt: %s", raw)
	}
	if record.OutputRef == "" {
		t.Fatalf("missing diagnostic link: %s", raw)
	}
	detail := record.OutputRef
	if !filepath.IsAbs(detail) {
		detail = filepath.Join(filepath.Dir(response.Receipt), detail)
	}
	details, err := os.ReadFile(detail)
	if err != nil || !bytes.Contains(details, []byte("=== RUN")) {
		t.Fatalf("detail link: %v: %s", err, details)
	}
	for _, path := range []string{response.Receipt, detail} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private file %s: %v %v", path, info, err)
		}
	}
}

func TestReceiptFailurePreservesProofAndReportsNoPath(t *testing.T) {
	state := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(state, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	dir := receiptFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"prove", "--dir", dir, "--base", "main", "--json", "--test", "sh value_test.sh"}, &stdout, &stderr)
	var response struct {
		Outcome, Receipt string
		Error            string `json:"receipt_error"`
		Status           string `json:"receipt_status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if code != 0 || response.Outcome != "proven" || response.Receipt != "" || response.Error == "" || response.Status != "failed" || !strings.Contains(stderr.String(), "receipt could not be saved") {
		t.Fatalf("exit=%d response=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	content, err := os.ReadFile(filepath.Join(dir, "value.txt"))
	if err != nil || string(content) != "new\n" {
		t.Fatalf("restoration: %q %v", content, err)
	}
}

func TestReceiptsDistinguishChangedInputAtSameCommit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := receiptFixture(t)
	var previousDigest, previousHead string
	for _, text := range []string{"new\n", "new\nanother changed input\n"} {
		proofWrite(t, dir, "value.txt", text)
		var stdout, stderr bytes.Buffer
		code := run([]string{"prove", "--dir", dir, "--base", "main", "--json", "--test", "sh value_test.sh"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit=%d: %s %s", code, stdout.String(), stderr.String())
		}
		var response struct{ Receipt string }
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(response.Receipt)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Head   string
			Digest string `json:"worktree_digest"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if len(record.Digest) != 64 || len(record.Head) != 40 {
			t.Fatalf("unknown identity: %s", data)
		}
		if previousDigest != "" && (record.Digest == previousDigest || record.Head != previousHead) {
			t.Fatalf("different dirty inputs not distinguished: %s", data)
		}
		previousDigest, previousHead = record.Digest, record.Head
	}
}

func TestAdviceReceiptPreservesOptionsWithoutPromptTitle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(apiKeyName, "")
	dir := receiptFixture(t)
	for _, titleArgs := range [][]string{{"--title", "private prompt title"}, {"--title=private prompt title"}} {
		args := []string{"--dir", dir, "--base", "main", "--model", "fixture-model", "--json"}
		args = append(args, titleArgs...)
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUnavailable {
			t.Fatalf("exit=%d: %s %s", code, stdout.String(), stderr.String())
		}
		var response struct{ Receipt string }
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(response.Receipt)
		if err != nil {
			t.Fatal(err)
		}
		var record struct{ Command string }
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		for _, option := range []string{"--dir", dir, "--base", "main", "--model", "fixture-model", "--json", "--title", "[redacted]"} {
			if !strings.Contains(record.Command, option) {
				t.Fatalf("missing option %q in command %q", option, record.Command)
			}
		}
		if bytes.Contains(raw, []byte("private prompt title")) {
			t.Fatal("receipt persisted raw prompt title")
		}
	}
}

func receiptFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	proofGit(t, dir, "init", "-q", "-b", "main")
	proofWrite(t, dir, "value.txt", "old\n")
	proofGit(t, dir, "add", ".")
	proofGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	proofWrite(t, dir, "value.txt", "new\n")
	proofWrite(t, dir, "value_test.sh", "grep -q '^new$' value.txt\n")
	return dir
}
