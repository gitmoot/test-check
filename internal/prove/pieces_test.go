package prove

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The hunk arithmetic decides what "undo one piece" writes, so check it
// against git's own -U0 diffs for replacement, pure insertion, pure deletion,
// and a missing final newline.
func TestFileHunksUndoExactlyOneHunk(t *testing.T) {
	cases := []struct{ name, base, current string }{
		{"replace", "a\nb\nc\nd\n", "a\nB\nc\nd\n"},
		{"insert", "a\nb\nc\n", "a\nb\nx\ny\nc\n"},
		{"delete", "a\nb\nc\nd\n", "a\nd\n"},
		{"insert at start", "a\nb\n", "z\na\nb\n"},
		{"delete at end", "a\nb\nc\n", "a\n"},
		{"no final newline", "a\nb", "a\nB"},
		{"two hunks", "a\nb\nc\nd\ne\nf\n", "A\nb\nc\nd\ne\nF\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "-b", "main")
			put(t, dir, "f.go", tc.base)
			git(t, dir, "add", ".")
			git(t, dir, "commit", "-q", "-m", "base")
			put(t, dir, "f.go", tc.current)
			out, err := exec.Command("git", "-C", dir, "diff", "-U0", "--no-color", "HEAD", "--", "f.go").Output()
			if err != nil {
				t.Fatal(err)
			}
			hunks, err := fileHunks(tc.current, string(out))
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "two hunks" {
				// Undoing each hunk alone keeps the other one.
				want := []string{"a\nb\nc\nd\ne\nF\n", "A\nb\nc\nd\ne\nf\n"}
				if len(hunks) != 2 || string(hunks[0].revert) != want[0] || string(hunks[1].revert) != want[1] {
					t.Fatalf("reverts = %q", hunkReverts(hunks))
				}
				return
			}
			if len(hunks) != 1 || string(hunks[0].revert) != tc.base {
				t.Fatalf("reverts = %q, want [%q]", hunkReverts(hunks), tc.base)
			}
		})
	}
}

func hunkReverts(hunks []piece) []string {
	var out []string
	for _, h := range hunks {
		out = append(out, string(h.revert))
	}
	return out
}

// piecesFixture: the fix changes Stop (tested) and Clamp (never tested) and
// edits a comment. Only Clamp's piece must be reported.
func piecesFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOFLAGS", "-buildvcs=false")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	put(t, dir, "go.mod", "module example.com/stop\n\ngo 1.22\n")
	code := "package stop\n\n// Stop says when to stop.\nfunc Stop(n int) bool { return n > 3 }\n\nfunc Clamp(n int) int {\n\tif n > 10 {\n\t\treturn 10\n\t}\n\treturn n\n}\n"
	put(t, dir, "stop.go", code)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "fix")
	code = strings.Replace(code, "// Stop says when to stop.", "// Stop reports whether n reached the limit.", 1)
	code = strings.Replace(code, "n > 3", "n >= 3", 1)
	code = strings.Replace(code, "if n > 10", "if n >= 10", 1)
	put(t, dir, "stop.go", code)
	put(t, dir, "stop_test.go", "package stop\n\nimport \"testing\"\n\nfunc TestStopAtThree(t *testing.T) {\n\tif !Stop(3) {\n\t\tt.Fatal(\"3\")\n\t}\n}\n")
	return dir
}

func TestPiecesReportsTheUntestedPieceAndRestores(t *testing.T) {
	dir := piecesFixture(t)
	before := snapshot(t, dir)
	report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "go test -count=1 .", Pieces: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, pc := range report.Pieces {
		got = append(got, pc.Outcome+" "+pc.Snippet)
	}
	want := []string{"guarded func Stop(n int) bool { return n >= 3 }", "not_tested if n >= 10 {"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pieces = %q, want %q (the comment-only hunk must be skipped)", got, want)
	}
	if report.Outcome != OutcomePieceNotTested {
		t.Fatalf("outcome = %s", report.Outcome)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("files changed by prove:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPiecesRefusesAboveTheLimitWithoutRunning(t *testing.T) {
	dir := piecesFixture(t)
	report, err := Run(context.Background(), Options{Dir: dir, Base: "main", Test: "touch ran && false", Pieces: true, MaxPieces: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeTooManyPieces || len(report.Pieces) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := snapshot(t, dir)["ran"]; ok {
		t.Fatal("test command ran above the piece limit")
	}
}
