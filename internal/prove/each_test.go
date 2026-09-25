package prove

import (
	"context"
	"reflect"
	"testing"
)

// goFixture: the fix changes Stop's boundary. TestStopAtThree catches it;
// TestStopAtTen passes on both versions, the weak test reviewers kept finding.
func goFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOFLAGS", "-buildvcs=false")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	put(t, dir, "go.mod", "module example.com/stop\n\ngo 1.22\n")
	put(t, dir, "stop.go", "package stop\n\nfunc Stop(n int) bool { return n > 3 }\n")
	put(t, dir, "stop_test.go", "package stop\n\nimport \"testing\"\n\nfunc TestStopExisting(t *testing.T) {\n\tif !Stop(9) {\n\t\tt.Fatal(\"9\")\n\t}\n}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "fix")
	put(t, dir, "stop.go", "package stop\n\nfunc Stop(n int) bool { return n >= 3 }\n")
	put(t, dir, "stop_test.go", "package stop\n\nimport \"testing\"\n\nfunc TestStopExisting(t *testing.T) {\n\tif !Stop(9) {\n\t\tt.Fatal(\"9\")\n\t}\n}\n\nfunc TestStopAtThree(t *testing.T) {\n\tif !Stop(3) {\n\t\tt.Fatal(\"3\")\n\t}\n}\n\nfunc TestStopAtTen(t *testing.T) {\n\tif !Stop(10) {\n\t\tt.Fatal(\"10\")\n\t}\n}\n")
	return dir
}

func TestEachFindsTheWeakTestTheWholeRunHides(t *testing.T) {
	dir := goFixture(t)
	before := snapshot(t, dir)
	ctx := context.Background()

	whole, err := Run(ctx, Options{Dir: dir, Base: "main", Test: "go test -count=1 ."})
	if err != nil {
		t.Fatal(err)
	}
	if whole.Outcome != OutcomeProven {
		t.Fatalf("whole-change outcome = %s, want proven (one good test hides the weak one)", whole.Outcome)
	}

	each, err := Run(ctx, Options{Dir: dir, Base: "main", Test: "go test -count=1 -v -run '^{name}$' ."})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var order []string
	for _, res := range each.Tests {
		got[res.Name] = res.Outcome
		order = append(order, res.Name)
	}
	if !reflect.DeepEqual(order, []string{"TestStopAtThree", "TestStopAtTen"}) {
		t.Fatalf("auto-selected tests = %v, want only the added ones", order)
	}
	if got["TestStopAtThree"] != OutcomeProven || got["TestStopAtTen"] != OutcomeNotRedOnOld {
		t.Fatalf("per-test outcomes = %v", got)
	}
	if each.Outcome != OutcomeNotRedOnOld {
		t.Fatalf("overall = %s, want not_red_on_old", each.Outcome)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("files changed by prove:\nbefore %v\nafter  %v", before, after)
	}
}

func TestEachReportsATestTheCommandNeverRan(t *testing.T) {
	dir := goFixture(t)
	report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "go test -count=1 -v -run '^{name}$' .", Each: []string{"TestStopAtThree", "TestMissing"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeNotRun || report.Tests[1].Outcome != OutcomeNotRun || report.Tests[0].Outcome != OutcomeProven {
		t.Fatalf("report = %+v", report)
	}
}
