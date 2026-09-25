package check

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/test-check/internal/jev"
)

type fakeJudge struct {
	advice   string
	covered  float64
	lowValue float64
	drop     string
	err      error
	request  jev.Request
}

func (f *fakeJudge) Evaluate(_ context.Context, request jev.Request) (jev.Exchange, error) {
	f.request = request
	if f.err != nil {
		return jev.Exchange{}, f.err
	}
	covered, lowValue := f.covered, f.lowValue
	answers := map[string]jev.Answer{
		questionAdvice:   {Type: "choice", Choice: f.advice, Probabilities: map[string]float64{f.advice: 0.9}},
		questionCovered:  {Type: "noul", Noul: &covered},
		questionLowValue: {Type: "noul", Noul: &lowValue},
	}
	delete(answers, f.drop)
	return jev.Exchange{Response: jev.Response{Answers: answers}}, nil
}

func sampleInput() Input {
	return Input{
		Repo:   "o/r",
		Files:  []File{{Path: "pkg/retry.go", Patch: "@@ -1 +1 @@\n-return nil\n+return err"}},
		Nearby: []NearbyTest{{Path: "pkg/retry_test.go", Content: "func TestRetryStops(t *testing.T) {}"}},
	}
}

func TestEvaluateNextStepFollowsAdviceAndFlags(t *testing.T) {
	cases := []struct {
		name      string
		judge     fakeJudge
		wantNext  string
		needsWork bool
	}{
		{"missing regression test", fakeJudge{advice: AdviceNewTest, covered: 0.1}, "Add a regression test that goes red", true},
		{"diff already carries the test", fakeJudge{advice: AdviceNewTest, covered: 0.8}, "must go red on the old code", false},
		{"extend names the nearest test", fakeJudge{advice: AdviceExtendTest, covered: 0.2}, "nearest: pkg/retry_test.go", true},
		{"one-off check", fakeJudge{advice: AdviceOneOff}, "Run the changed path once", false},
		{"no test still runs existing tests", fakeJudge{advice: AdviceNoTest}, "Still build and run the existing tests", false},
		{"low-value test flagged even when no test is needed", fakeJudge{advice: AdviceNoTest, lowValue: 0.7}, "looks low-value", true},
		{"need context points at the questions", fakeJudge{advice: AdviceNeedContext}, "four questions", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			judge := tc.judge
			got, err := Evaluate(context.Background(), &judge, jev.DefaultModel, sampleInput())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Next, tc.wantNext) {
				t.Fatalf("Next = %q, want it to contain %q", got.Next, tc.wantNext)
			}
			if got.NeedsTestWork != tc.needsWork {
				t.Fatalf("NeedsTestWork = %v, want %v", got.NeedsTestWork, tc.needsWork)
			}
		})
	}
}

func TestEvaluateRefusesIncompleteAnswers(t *testing.T) {
	for name, judge := range map[string]*fakeJudge{
		"transport error":      {err: errors.New("HTTP 503")},
		"missing advice":       {advice: AdviceNoTest, drop: questionAdvice},
		"missing covered":      {advice: AdviceNoTest, drop: questionCovered},
		"unoffered advice":     {advice: "write_more_tests"},
		"NaN low-value answer": {advice: AdviceNoTest, lowValue: math.NaN()},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := Evaluate(context.Background(), judge, jev.DefaultModel, sampleInput()); err == nil {
				t.Fatalf("Evaluate = %+v, want an error rather than partial advice", got)
			}
		})
	}
	if _, err := Evaluate(context.Background(), nil, jev.DefaultModel, sampleInput()); err == nil {
		t.Fatal("nil judge must be an error")
	}
}

func TestBuildStateSendsNearbyTestNamesAndKeepsEveryPathWithinBudget(t *testing.T) {
	in := sampleInput()
	in.Files = append(in.Files, File{Path: "gen/huge.go", Patch: "@@ -0,0 +1 @@\n+" + strings.Repeat("x", 3*DiffBudgetBytes)})
	state, complete, nearby := BuildState(in)
	if complete {
		t.Fatal("complete = true for a truncated diff")
	}
	diff := state["diff"].(string)
	if len(diff) > DiffBudgetBytes || !strings.Contains(diff, "+return err") {
		t.Fatalf("diff len %d; small file kept: %v", len(diff), strings.Contains(diff, "+return err"))
	}
	files := state["files"].([]string)
	if len(files) != 2 || files[0] != "pkg/retry.go [code] (+1/-1)" {
		t.Fatalf("files = %v", files)
	}
	tests := state["nearbyTests"].([]map[string]any)
	if !reflect.DeepEqual(nearby, []string{"pkg/retry_test.go"}) || !reflect.DeepEqual(tests[0]["tests"], []string{"TestRetryStops"}) {
		t.Fatalf("nearby = %v, tests = %v", nearby, tests)
	}
}

func TestNearbyTestPathsPrefersTestsNamingTheChangedFile(t *testing.T) {
	all := []string{
		"pkg/retry.go", "pkg/retry_test.go", "pkg/retry_backoff_test.go", "pkg/other_test.go", "pkg/new_test.go",
		"web/src/Player.tsx", "web/src/Player.test.tsx", "web/src/__tests__/player.spec.ts",
		"app/billing.py", "tests/test_billing.py",
		"Sources/App/Paywall.swift", "Tests/AppTests/PaywallTests.swift",
		"docs/README.md",
	}
	changed := []string{"pkg/retry.go", "pkg/new_test.go", "web/src/Player.tsx", "app/billing.py", "Sources/App/Paywall.swift", "docs/README.md"}
	got := NearbyTestPaths(changed, all)
	want := []string{"pkg/retry_test.go", "web/src/Player.test.tsx", "Tests/AppTests/PaywallTests.swift", "tests/test_billing.py", "web/src/__tests__/player.spec.ts", "pkg/retry_backoff_test.go", "pkg/other_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NearbyTestPaths =\n%v\nwant\n%v", got, want)
	}
	if got := NearbyTestPaths([]string{"docs/README.md"}, all); len(got) != 0 {
		t.Fatalf("docs-only change got nearby tests %v", got)
	}
}

func TestTestNamesAcrossLanguages(t *testing.T) {
	content := "func TestGo(t *testing.T) {}\n" +
		"func testSwiftXC() {}\n" +
		"@Test(\"x\")\nfunc swiftTesting() {}\n" +
		"def test_python():\n" +
		"#[test]\nfn rust_case() {}\n" +
		"it('renders the cut', () => {})\n"
	want := []string{"TestGo", "testSwiftXC", "swiftTesting", "test_python", "rust_case", "renders the cut"}
	if got := TestNames(content); !reflect.DeepEqual(got, want) {
		t.Fatalf("TestNames = %v, want %v", got, want)
	}
}
