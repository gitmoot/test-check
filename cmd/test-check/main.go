// Command test-check asks JEV what testing a code change needs, and proves
// that a change's tests go red on the old code and green on the new.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/test-check/internal/check"
	"github.com/gitmoot/test-check/internal/jev"
	"github.com/gitmoot/test-check/internal/source"
)

const (
	exitUsage       = 2
	exitSource      = 1
	exitUnavailable = 3
	apiKeyName      = "OPENROUTER_API_KEY"
)

// newJudge is a variable so tests can substitute a fake.
var newJudge = func(key string) check.Judge {
	return &jev.Client{
		Endpoint:   jev.DefaultEndpoint,
		APIKey:     key,
		HTTP:       http.DefaultClient,
		Timeout:    jev.DefaultTimeout,
		MaxRetries: jev.DefaultMaxRetries,
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "prove" {
		return runProve(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("test-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `Usage:
  test-check [--dir DIR] [--base REF] [--title TEXT] [--json]
      the change in a local checkout since its merge base with REF
      (default: the remote default branch), including uncommitted files
  test-check --repo OWNER/REPO --pr N [--json]
      a pull request's current head
  test-check --repo OWNER/REPO --compare BASE...HEAD [--title TEXT] [--json]
      the change between two commits
  test-check prove --test CMD [--each A,B] [--dir DIR] [--base REF] [--timeout DUR] [--json]
      run CMD on the new code (must pass), then with every non-test change
      reverted (must fail); files are restored afterwards. With {name} in
      CMD, every added or edited test must go red on its own
  test-check prove --restore [--dir DIR]
      put back files a crashed prove run left reverted

Exit: 0 advice printed / proven, 1 error, 2 usage, 3 JEV unavailable,
4 not proven (passes on the old code, fails on the new, or no test changed).
Key: $OPENROUTER_API_KEY, else OPENROUTER_API_KEY in ~/.config/gitmoot/keychain.env.`)
	}
	dir := fs.String("dir", ".", "local git checkout")
	base := fs.String("base", "", "base ref for a local change")
	title := fs.String("title", "", "short description of the change")
	repo := fs.String("repo", "", "OWNER/REPO on GitHub")
	pr := fs.Int("pr", 0, "pull request number")
	compare := fs.String("compare", "", "BASE...HEAD commits on GitHub")
	model := fs.String("model", jev.DefaultModel, "JEV model")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitUsage
	}
	if fs.NArg() != 0 || (*pr > 0 && *compare != "") || ((*pr > 0 || *compare != "") && *repo == "") {
		fs.Usage()
		return exitUsage
	}

	ctx := context.Background()
	var in check.Input
	var err error
	switch {
	case *pr > 0:
		in, err = source.PullRequest(ctx, *repo, *pr)
	case *compare != "":
		b, h, ok := strings.Cut(*compare, "...")
		if !ok || b == "" || h == "" {
			fmt.Fprintln(stderr, "test-check: --compare must be BASE...HEAD")
			return exitUsage
		}
		in, err = source.Compare(ctx, *repo, b, h, *title)
	default:
		in, err = source.Local(ctx, *dir, *base, *title)
		in.Repo = *repo
	}
	if err != nil {
		fmt.Fprintf(stderr, "test-check: %v\n", err)
		return exitSource
	}

	key := apiKey()
	if key == "" {
		fmt.Fprintf(stderr, "test-check: JEV unavailable: no %s. Answer the four questions in the test-check skill yourself.\n", apiKeyName)
		return exitUnavailable
	}
	result, err := check.Evaluate(ctx, newJudge(key), *model, in)
	if err != nil {
		fmt.Fprintf(stderr, "test-check: JEV unavailable: %v. Answer the four questions in the test-check skill yourself.\n", err)
		return exitUnavailable
	}

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(result)
		return 0
	}
	fmt.Fprintf(stdout, "advice: %s\n", result.Advice)
	fmt.Fprintf(stdout, "next: %s\n", result.Next)
	fmt.Fprintf(stdout, "covered by this change's own tests: p=%.2f\n", result.Covered)
	fmt.Fprintf(stdout, "low-value tests added: p=%.2f\n", result.LowValue)
	if len(result.NearbyTests) > 0 {
		fmt.Fprintf(stdout, "nearby tests: %s\n", strings.Join(result.NearbyTests, ", "))
	}
	if !result.DiffComplete {
		fmt.Fprintln(stdout, "note: the diff was truncated or missing parts; judge the omitted files yourself")
	}
	return 0
}

// apiKey prefers the environment, then the Gitmoot keychain file the fleet
// already keeps the key in. Any read failure means no key.
func apiKey() string {
	if key := strings.TrimSpace(os.Getenv(apiKeyName)); key != "" {
		return key
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	file, err := os.Open(filepath.Join(configDir, "gitmoot", "keychain.env"))
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), apiKeyName+"="); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}
