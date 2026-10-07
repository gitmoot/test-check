package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gitmoot/test-check/internal/check"
	"github.com/gitmoot/test-check/internal/prove"
	"github.com/gitmoot/test-check/internal/receipt"
	"github.com/gitmoot/test-check/internal/source"
)

// receiptFields are additive JSON metadata; stdout remains one JSON value.
type receiptFields struct {
	Receipt       string `json:"receipt,omitempty"`
	ReceiptStatus string `json:"receipt_status"`
	ReceiptError  string `json:"receipt_error,omitempty"`
}

type commandReceipt struct{ record receipt.Record }

func startReceipt(mode, dir, base, command string, local bool) *commandReceipt {
	r := &commandReceipt{record: receipt.Record{Mode: mode, Base: base, Command: command, StartedAt: time.Now().UTC()}}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				r.record.ToolRevision = setting.Value
			}
		}
	}
	if !local {
		return r
	}
	// Metadata is best effort and bounded; it must never prevent proving or
	// recovery. It is collected before any proof swaps files.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	git := source.Git(ctx, dir)
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		r.record.Checkout, _ = filepath.Abs(dir)
		r.record.Repo = r.record.Checkout
		r.record.Warnings = append(r.record.Warnings, "repository identity unavailable")
		return r
	}
	r.record.Repo = strings.TrimSpace(root)
	r.record.Checkout = r.record.Repo
	head, err := git("rev-parse", "HEAD")
	if err == nil {
		r.record.Head = strings.TrimSpace(head)
	}
	if base == "" {
		base, _ = source.DefaultBase(git)
	}
	if base != "" {
		mergeBase, err := git("merge-base", base, "HEAD")
		if err == nil {
			r.record.Base = strings.TrimSpace(mergeBase)
		}
	}
	return r
}

func (r *commandReceipt) input(in check.Input) {
	if in.Repo != "" {
		r.record.Repo = in.Repo
	}
	if in.Head != "" {
		r.record.Head = in.Head
	}
	// Hash the already-collected input without copying or retaining its diff.
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(in)
	r.record.WorktreeDigest = hex.EncodeToString(h.Sum(nil))
	if in.Incomplete {
		r.record.Warnings = append(r.record.Warnings, "input snapshot incomplete")
	}
}

func (r *commandReceipt) save(outcome string, runErr error, outputs []receipt.Output, stderr io.Writer) receiptFields {
	r.record.Outcome = outcome
	r.record.FinishedAt = time.Now().UTC()
	if runErr != nil {
		r.record.Error = runErr.Error()
	}
	// Save is local and independent of a canceled run context. The caller has
	// already restored files before arriving here.
	paths, err := receipt.Save("", r.record, outputs)
	if err != nil {
		fmt.Fprintf(stderr, "test-check: warning: receipt could not be saved: %v\n", err)
		return receiptFields{ReceiptStatus: "failed", ReceiptError: err.Error()}
	}
	fmt.Fprintf(stderr, "receipt: %s\n", paths.Receipt)
	return receiptFields{Receipt: paths.Receipt, ReceiptStatus: "saved"}
}

func (r *commandReceipt) proof(report prove.Report, command string, runErr error, stderr io.Writer) receiptFields {
	if report.Base != "" {
		r.record.Base = report.Base
	}
	if report.Head != "" {
		r.record.Head = report.Head
	}
	r.record.WorktreeDigest = report.WorktreeDigest
	if report.WorktreeDigest == "" {
		warning := report.SnapshotWarning
		if warning == "" {
			warning = "changed-input snapshot unknown (proof did not reach input capture)"
		}
		r.record.Warnings = append(r.record.Warnings, warning)
	}
	var outputs []receipt.Output
	addOutput := func(label, text string) {
		if text != "" {
			outputs = append(outputs, receipt.Output{Label: label, Text: text})
		}
	}
	addOutput("new", report.NewOutput)
	addOutput("old", report.OldOutput)
	for _, test := range report.Tests {
		item := receipt.Test{Name: test.Name, Command: strings.ReplaceAll(command, prove.NamePlaceholder, test.Name), Outcome: test.Outcome,
			NewExecution: executionObserved(test.NewOutput, test.Name), OldExecution: executionObserved(test.OldOutput, test.Name)}
		if test.BuildErrorSuspected {
			item.Warnings = append(item.Warnings, "old_code_build_failed")
		}
		if test.Outcome == prove.OutcomeNotRun {
			item.NewExecution = "not_observed"
		}
		r.record.Tests = append(r.record.Tests, item)
		addOutput(test.Name+"/new", test.NewOutput)
		addOutput(test.Name+"/old", test.OldOutput)
	}
	for _, piece := range report.Pieces {
		r.record.Pieces = append(r.record.Pieces, receipt.Piece{File: piece.File, Line: piece.Line, Outcome: piece.Outcome, Reason: piece.Reason, BuildErrorSuspected: piece.BuildErrorSuspected})
		addOutput(fmt.Sprintf("%s:%d/old", piece.File, piece.Line), piece.OldOutput)
	}
	if len(report.Tests) == 0 {
		seen := make(map[string]bool)
		for _, text := range []string{report.NewOutput, report.OldOutput} {
			for _, match := range runnerNames.FindAllStringSubmatch(text, -1) {
				name := ""
				for _, captured := range match[1:] {
					if captured != "" {
						name = captured
						break
					}
				}
				if seen[name] {
					continue
				}
				seen[name] = true
				r.record.Tests = append(r.record.Tests, receipt.Test{Name: name, Command: command, Outcome: "aggregate_only",
					NewExecution: executionObserved(report.NewOutput, name), OldExecution: executionObserved(report.OldOutput, name)})
			}
		}
	}
	if report.BuildErrorSuspected {
		r.record.Warnings = append(r.record.Warnings, "old_code_build_failed")
	}
	if report.Reason != "" && report.Reason != "old_code_build_failed" {
		r.record.Warnings = append(r.record.Warnings, report.Reason)
	}
	outcome := report.Outcome
	if runErr != nil {
		outcome = "error"
	}
	return r.save(outcome, runErr, outputs, stderr)
}

var runnerNames = regexp.MustCompile(`(?m)^(?:=== RUN\s+(\S+)|[^\r\n]*::(test\S+)\s+(?:PASSED|FAILED)|((?:test)\S*) \([^\r\n]+\) \.\.\.)`)

// Unsupported runners and truncated output are unknown, not evidence that a
// test was skipped. Only concrete verbose Go/Python markers show execution.
func executionObserved(output, name string) string {
	quoted := regexp.QuoteMeta(name)
	marker := regexp.MustCompile(`(?m)^(=== RUN\s+` + quoted + `(?:\s|$)|[^\r\n]*::` + quoted + `(?:\s|\[)|` + quoted + ` \([^\r\n]+\) \.\.\.)`)
	if marker.MatchString(output) {
		return "observed"
	}
	return "unknown"
}

func writeReceiptError(stdout io.Writer, err error, fields receiptFields) {
	_ = json.NewEncoder(stdout).Encode(struct {
		Error string `json:"error"`
		receiptFields
	}{err.Error(), fields})
}
