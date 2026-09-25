package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/test-check/internal/prove"
)

const exitNotProven = 4

var proveMessages = map[string]string{
	prove.OutcomeProven:        "The tests fail on the old code and pass on the new: they would catch a regression of this change.",
	prove.OutcomeNotRedOnOld:   "The tests PASS on the old code too, so they would not catch a regression. Make them fail without the fix.",
	prove.OutcomeFailsOnNew:    "The tests FAIL on the new code. Fix that before proving.",
	prove.OutcomeNoTestChanged: "The change has no test files. If a test is needed, add one; otherwise record the one-off check you ran.",
	prove.OutcomeNoCodeChanged: "Only tests changed; there is no old code to prove them against.",
	prove.OutcomeNotRun:        "A test never appeared in the command's output on the new code, so the command probably did not run it. Use a verbose runner (go test -v, pytest -v) and check the name.",
}

func runProve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("test-check prove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "local git checkout")
	base := fs.String("base", "", "base ref (default: the remote default branch)")
	test := fs.String("test", "", "test command, run with sh -c at the repository root; with {name}, each test is proven on its own")
	each := fs.String("each", "", "comma-separated test names for {name} (default: the Go/Python tests the change adds or edits)")
	timeout := fs.Duration("timeout", 10*time.Minute, "limit for each test run")
	restore := fs.Bool("restore", false, "restore files a crashed prove run left reverted")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitUsage
	}
	if fs.NArg() != 0 || (*restore == (strings.TrimSpace(*test) != "")) {
		fmt.Fprintln(stderr, "test-check prove: pass exactly one of --test CMD or --restore")
		return exitUsage
	}
	// Ctrl-C and SIGTERM cancel the run; Run then restores before returning.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *restore {
		if err := prove.Restore(ctx, *dir); err != nil {
			fmt.Fprintf(stderr, "test-check prove: %v\n", err)
			return exitSource
		}
		fmt.Fprintln(stdout, "restored")
		return 0
	}
	var names []string
	for _, name := range strings.Split(*each, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) > 0 && !strings.Contains(*test, prove.NamePlaceholder) {
		fmt.Fprintln(stderr, "test-check prove: --each needs {name} in --test")
		return exitUsage
	}
	report, err := prove.Run(ctx, prove.Options{Dir: *dir, Base: *base, Test: *test, Each: names, Timeout: *timeout})
	if err != nil {
		fmt.Fprintf(stderr, "test-check prove: %v\n", err)
		return exitSource
	}
	code := exitNotProven
	if report.Outcome == prove.OutcomeProven || report.Outcome == prove.OutcomeNoCodeChanged {
		code = 0
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
		return code
	}
	fmt.Fprintf(stdout, "outcome: %s\n%s\n", report.Outcome, proveMessages[report.Outcome])
	if report.BuildErrorSuspected {
		fmt.Fprintln(stdout, "warning: the old-code run looks like a build or import error, not a failed assertion. That only shows the test uses new code; for a bug fix, test the behavior through an interface the old code also has.")
	}
	for _, res := range report.Tests {
		note := ""
		if res.BuildErrorSuspected {
			note = " (old run looks like a build error)"
		}
		fmt.Fprintf(stdout, "  %-16s %s%s\n", res.Outcome, res.Name, note)
	}
	if len(report.TestFiles) > 0 {
		fmt.Fprintf(stdout, "test files: %s\n", strings.Join(report.TestFiles, ", "))
	}
	if len(report.RevertedFiles) > 0 && report.Outcome != prove.OutcomeNoTestChanged {
		fmt.Fprintf(stdout, "reverted for the old-code run: %s\n", strings.Join(report.RevertedFiles, ", "))
	}
	if len(report.Tests) > 0 {
		return code
	}
	switch report.Outcome {
	case prove.OutcomeFailsOnNew:
		fmt.Fprintf(stdout, "--- new-code run (tail) ---\n%s\n", report.NewOutput)
	case prove.OutcomeProven, prove.OutcomeNotRedOnOld:
		fmt.Fprintf(stdout, "--- old-code run (tail) ---\n%s\n", report.OldOutput)
	}
	return code
}
