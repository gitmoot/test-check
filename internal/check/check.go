// Package check asks JEV what testing a code change needs. It is advice, not a
// gate: the agent still decides, and every answer still requires the change to
// be verified.
package check

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/test-check/internal/jev"
)

const (
	AdviceNewTest     = "new_regression_test"
	AdviceExtendTest  = "extend_existing_test"
	AdviceOneOff      = "one_off_check"
	AdviceNoTest      = "no_permanent_test"
	AdviceNeedContext = "need_context"

	// A probability ABOVE these thresholds sets the flag.
	CoveredThreshold  = 0.5
	LowValueThreshold = 0.5

	DiffBudgetBytes  = 40000
	TestsBudgetBytes = 16000

	questionAdvice   = "advice"
	questionCovered  = "covered_by_diff_tests"
	questionLowValue = "low_value_tests_added"
	truncatedMarker  = "\n[... truncated ...]\n"
)

// File is one changed file. Patch is empty when the source could not provide
// it (binary, or too large for GitHub), which makes the diff incomplete.
type File struct {
	Path  string
	Patch string
}

// NearbyTest is an existing test file that is not part of the change.
type NearbyTest struct {
	Path    string
	Content string
}

// Input is one change and the tests around it.
type Input struct {
	// Incomplete: the source knows files are missing from Files.
	Incomplete bool
	Repo       string
	Title      string
	Head       string
	Files      []File
	Nearby     []NearbyTest
}

// Result is JEV's advice and the one next step it implies.
type Result struct {
	Advice string `json:"advice"`
	// NeedsTestWork: the advice asks for a permanent test the change does not
	// yet have, or an added test looks low-value.
	NeedsTestWork bool               `json:"needs_test_work"`
	Next          string             `json:"next"`
	Covered       float64            `json:"covered_by_diff_tests"`
	LowValue      float64            `json:"low_value_tests_added"`
	Probabilities map[string]float64 `json:"probabilities"`
	DiffComplete  bool               `json:"diff_complete"`
	NearbyTests   []string           `json:"nearby_tests"`
}

// Judge evaluates one JEV request; *jev.Client satisfies it.
type Judge interface {
	Evaluate(ctx context.Context, request jev.Request) (jev.Exchange, error)
}

// Questions are the three questions sent with every change.
func Questions() map[string]jev.Question {
	return map[string]jev.Question{
		questionAdvice: {
			Type:         "choice",
			Instructions: "Decide what testing this code change needs. Judge the diff itself and the nearby existing tests. A permanent test earns its place only if it protects behavior a user or caller relies on AND would fail on a plausible regression of this change. Pick what the change needs even if the diff already includes it. Choose need_context only when essential evidence is missing, never because the change is large or complex.",
			Criteria: map[string]string{
				AdviceNewTest:     "Fixes a bug or adds behavior that no nearby test exercises; a regression would go unnoticed. Needs a new test that fails on the old code.",
				AdviceExtendTest:  "Changes behavior an existing nearby test already covers; add a case or assertion to that test rather than a new test.",
				AdviceOneOff:      "Behavior changes, but a permanent test would be brittle, slow, or only restate the code (UI layout, wiring, configuration, external services, scripts). Verify once by running it.",
				AdviceNoTest:      "No behavior changes: docs, comments, formatting, renames, version bumps, or a refactor existing tests already cover.",
				AdviceNeedContext: "Essential evidence is missing, so no option can be chosen.",
			},
		},
		questionCovered: {
			Type:         "noul",
			Instructions: "Do tests added or changed in this diff exercise the changed behavior, so that a plausible regression of this change would make them fail? Answer no if the diff adds or changes no tests, or if its tests only check unrelated code, restate the source, or would still pass on the old code.",
			Criteria: map[string]string{
				"true":  "The diff's own tests would fail on a plausible regression of the change.",
				"false": "The diff has no such tests.",
			},
		},
		questionLowValue: {
			Type:         "noul",
			Instructions: "Does the diff add or change tests that are low-value: they only restate source text or constants, copy fixtures or lists, assert private implementation details or call order, duplicate a stronger existing test, or would still pass on the old broken code? Answer no if the diff adds or changes no tests.",
			Criteria: map[string]string{
				"true":  "At least one added or changed test is low-value in one of those ways.",
				"false": "No added or changed test is low-value, or the diff has no tests.",
			},
		},
	}
}

// Evaluate asks JEV about one change. Any failure to get a complete answer is
// an error: the caller must not present a partial answer as advice.
func Evaluate(ctx context.Context, judge Judge, model string, in Input) (Result, error) {
	if judge == nil {
		return Result{}, errors.New("no JEV client")
	}
	state, complete, nearby := BuildState(in)
	exchange, err := judge.Evaluate(ctx, jev.Request{Model: model, State: state, Questions: Questions()})
	if err != nil {
		return Result{}, err
	}
	answers := exchange.Response.Answers
	advice, ok := answers[questionAdvice]
	if !ok {
		return Result{}, fmt.Errorf("%s answer missing", questionAdvice)
	}
	switch advice.Choice {
	case AdviceNewTest, AdviceExtendTest, AdviceOneOff, AdviceNoTest, AdviceNeedContext:
	default:
		return Result{}, fmt.Errorf("%s answer %q is not an offered option", questionAdvice, advice.Choice)
	}
	covered, err := probability(answers, questionCovered)
	if err != nil {
		return Result{}, err
	}
	lowValue, err := probability(answers, questionLowValue)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Advice:        advice.Choice,
		Covered:       covered,
		LowValue:      lowValue,
		Probabilities: advice.Probabilities,
		DiffComplete:  complete,
		NearbyTests:   nearby,
	}
	result.NeedsTestWork = needsTestWork(result)
	result.Next = nextStep(result)
	return result, nil
}

func probability(answers map[string]jev.Answer, key string) (float64, error) {
	answer, ok := answers[key]
	if !ok {
		return 0, fmt.Errorf("%s answer missing", key)
	}
	if answer.Noul == nil || !(*answer.Noul >= 0 && *answer.Noul <= 1) {
		return 0, fmt.Errorf("%s answer has no probability in [0,1]", key)
	}
	return *answer.Noul, nil
}

func needsTestWork(r Result) bool {
	missing := (r.Advice == AdviceNewTest || r.Advice == AdviceExtendTest) && r.Covered <= CoveredThreshold
	return missing || r.LowValue > LowValueThreshold
}

func nextStep(r Result) string {
	var next string
	switch r.Advice {
	case AdviceNewTest, AdviceExtendTest:
		switch {
		case r.Covered > CoveredThreshold:
			next = "The change's own tests look like they cover it. Prove it: they must go red on the old code and green on the new."
		case r.Advice == AdviceExtendTest && len(r.NearbyTests) > 0:
			next = "Add a case to an existing test (nearest: " + r.NearbyTests[0] + ") that goes red on the old code and green on the new."
		case r.Advice == AdviceExtendTest:
			next = "Add a case to the existing test that covers this behavior; it must go red on the old code and green on the new."
		default:
			next = "Add a regression test that goes red on the old code and green on the new."
		}
	case AdviceOneOff:
		next = "Skip a permanent test. Run the changed path once and record the command and what you observed."
	case AdviceNoTest:
		next = "No permanent test needed. Still build and run the existing tests."
	default:
		next = "JEV lacked context. Answer the four questions in the test-check skill yourself."
	}
	if r.LowValue > LowValueThreshold {
		next += " Warning: an added test looks low-value (restates code, duplicates another test, tests internals, or would pass on the old code); rework or drop it."
	}
	return next
}

// BuildState renders the model input. Every changed path and nearby test path
// is always listed; diff and test text are cut to fair shares of their budgets
// so one huge file cannot hide the others.
func BuildState(in Input) (state map[string]any, complete bool, nearby []string) {
	complete = !in.Incomplete
	names := make([]string, len(in.Files))
	texts := make([]string, len(in.Files))
	for i, file := range in.Files {
		if file.Patch == "" {
			complete = false
		}
		added, deleted := 0, 0
		for _, line := range strings.Split(file.Patch, "\n") {
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				deleted++
			}
		}
		kind := "code"
		switch {
		case IsTestFile(file.Path):
			kind = "test"
		case !IsCode(file.Path):
			kind = "other"
		}
		names[i] = fmt.Sprintf("%s [%s] (+%d/-%d)", file.Path, kind, added, deleted)
		texts[i] = "diff --git a/" + file.Path + " b/" + file.Path + "\n" + file.Patch
	}
	if !fitBudget(texts, DiffBudgetBytes) {
		complete = false
	}
	tests := make([]map[string]any, len(in.Nearby))
	contents := make([]string, len(in.Nearby))
	nearby = make([]string, len(in.Nearby))
	for i, t := range in.Nearby {
		nearby[i] = t.Path
		tests[i] = map[string]any{"path": t.Path, "tests": TestNames(t.Content)}
		contents[i] = "== " + t.Path + "\n" + t.Content
	}
	fitBudget(contents, TestsBudgetBytes)
	return map[string]any{
		"repo":              in.Repo,
		"title":             in.Title,
		"head":              in.Head,
		"files":             names,
		"diffComplete":      complete,
		"diff":              strings.Join(texts, ""),
		"nearbyTests":       tests,
		"nearbyTestContent": strings.Join(contents, "\n"),
	}, complete, nearby
}

// fitBudget truncates texts in place so their total stays within budget,
// giving each a fair share, smallest first. A text whose share cannot hold the
// marker is dropped. It reports whether nothing was cut.
func fitBudget(texts []string, budget int) bool {
	total := 0
	for _, t := range texts {
		total += len(t)
	}
	if total <= budget {
		return true
	}
	order := make([]int, len(texts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(texts[order[a]]) < len(texts[order[b]]) })
	remaining := budget
	for n, index := range order {
		share := remaining / (len(order) - n)
		if len(texts[index]) > share {
			if share < len(truncatedMarker) {
				texts[index] = ""
			} else {
				texts[index] = strings.ToValidUTF8(texts[index][:share-len(truncatedMarker)], "") + truncatedMarker
			}
		}
		remaining -= len(texts[index])
	}
	return false
}
