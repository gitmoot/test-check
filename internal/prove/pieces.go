package prove

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gitmoot/test-check/internal/check"
)

const (
	// OutcomePieceNotTested: undoing one changed piece of code left every test
	// passing, so no test notices that piece.
	OutcomePieceNotTested = "piece_not_tested"
	// OutcomeTooManyPieces: the change has more pieces than MaxPieces; nothing
	// was run. Narrow the change or raise the limit.
	OutcomeTooManyPieces = "too_many_pieces"

	PieceGuarded   = "guarded"
	PieceUnguarded = "not_tested"

	DefaultMaxPieces = 60
)

// PieceResult is one changed piece of code, undone on its own.
type PieceResult struct {
	File string `json:"file"`
	// Line is where the piece starts in the new file; 0 for a whole file.
	Line                int    `json:"line"`
	Lines               int    `json:"lines"`
	Outcome             string `json:"outcome"`
	BuildErrorSuspected bool   `json:"build_error_suspected"`
	Snippet             string `json:"snippet"`
}

// piece is one hunk of a code file: undoing it means writing revert.
type piece struct {
	file    string
	line    int
	lines   int
	snippet string
	// revert is the file content with only this piece undone; exists=false
	// means undoing it removes the file (the change added it).
	revert []byte
	exists bool
	mode   fs.FileMode
	// fromBase: undo by writing the merge-base version (a deleted file).
	fromBase  bool
	mergeBase string
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// commentOnly reports whether a changed line carries no behavior: blank, or a
// comment in one of the languages test-check knows.
func commentOnly(line string) bool {
	t := strings.TrimSpace(line)
	for _, prefix := range []string{"//", "#", "/*", "*", "--", `"""`, "'''"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return t == ""
}

// codePieces splits the change to each code file into hunks. Comment- and
// whitespace-only hunks are skipped: no test can notice them.
func codePieces(r repo, mergeBase string, files []string) ([]piece, error) {
	var pieces []piece
	for _, p := range files {
		if !check.IsCode(p) {
			continue
		}
		full := filepath.Join(r.root, p)
		info, statErr := os.Lstat(full)
		current, readErr := os.ReadFile(full)
		listing, err := r.git("ls-tree", mergeBase, "--", p)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.TrimSpace(listing) == "":
			// Added by the change: the whole file is one piece.
			if readErr != nil {
				continue
			}
			pieces = append(pieces, piece{file: p, lines: strings.Count(string(current), "\n"), snippet: "(new file)", exists: false})
			continue
		case errors.Is(statErr, fs.ErrNotExist):
			// Deleted by the change: undoing it restores the base file.
			pieces = append(pieces, piece{file: p, snippet: "(deleted file)", fromBase: true, mergeBase: mergeBase})
			continue
		case statErr != nil:
			return nil, statErr
		case readErr != nil:
			return nil, readErr
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		diff, err := r.git("diff", "-U0", "--no-color", "--no-ext-diff", mergeBase, "--", p)
		if err != nil {
			return nil, err
		}
		hunks, err := fileHunks(string(current), diff)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, h := range hunks {
			h.file, h.exists, h.mode = p, true, info.Mode().Perm()
			pieces = append(pieces, h)
		}
	}
	return pieces, nil
}

// fileHunks parses a -U0 diff of one file and builds, for each behavioral
// hunk, the current content with only that hunk undone.
func fileHunks(current, diff string) ([]piece, error) {
	lines := strings.SplitAfter(current, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var pieces []piece
	diffLines := strings.Split(diff, "\n")
	for i := 0; i < len(diffLines); i++ {
		m := hunkHeader.FindStringSubmatch(diffLines[i])
		if m == nil {
			continue
		}
		newStart, _ := strconv.Atoi(m[3])
		newCount := 1
		if m[4] != "" {
			newCount, _ = strconv.Atoi(m[4])
		}
		var oldLines []string
		behavioral := false
		snippet := ""
		newSnippet := false
		for i+1 < len(diffLines) {
			next := diffLines[i+1]
			if next == "" || (next[0] != '+' && next[0] != '-' && next[0] != '\\') {
				break
			}
			i++
			switch next[0] {
			case '-':
				oldLines = append(oldLines, next[1:]+"\n")
			case '\\':
				// "\ No newline at end of file": drop it from the old side.
				if n := len(oldLines); n > 0 && strings.HasPrefix(diffLines[i-1], "-") {
					oldLines[n-1] = strings.TrimSuffix(oldLines[n-1], "\n")
				}
				continue
			}
			if !commentOnly(next[1:]) {
				behavioral = true
				// Show the first new line; a pure deletion shows the removed one.
				if next[0] == '+' && !newSnippet {
					snippet, newSnippet = strings.TrimSpace(next[1:]), true
				} else if snippet == "" {
					snippet = "(removed) " + strings.TrimSpace(next[1:])
				}
			}
		}
		if !behavioral {
			continue
		}
		at := newStart - 1
		if newCount == 0 {
			at = newStart
		}
		if at < 0 || at+newCount > len(lines) {
			return nil, fmt.Errorf("hunk at +%d,%d outside a %d-line file", newStart, newCount, len(lines))
		}
		reverted := make([]string, 0, len(lines)-newCount+len(oldLines))
		reverted = append(reverted, lines[:at]...)
		reverted = append(reverted, oldLines...)
		reverted = append(reverted, lines[at+newCount:]...)
		if len(snippet) > 120 {
			snippet = strings.ToValidUTF8(snippet[:120], "")
		}
		pieces = append(pieces, piece{line: newStart, lines: newCount, snippet: snippet, revert: []byte(strings.Join(reverted, ""))})
	}
	return pieces, nil
}

// applyPiece stashes the file, then writes it with only this piece undone.
func applyPiece(r repo, pc piece) error {
	if err := stashPaths(r, []string{pc.file}); err != nil {
		return err
	}
	full := filepath.Join(r.root, pc.file)
	if pc.fromBase {
		return writeBase(r, pc.mergeBase, pc.file)
	}
	if !pc.exists {
		return os.RemoveAll(full)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	_ = os.Remove(full)
	return os.WriteFile(full, pc.revert, pc.mode)
}

// runPieces undoes each changed piece of code on its own and runs the test
// command: every piece must turn some test red.
func runPieces(ctx context.Context, r repo, mergeBase string, opts Options, report Report) (Report, error) {
	pieces, err := codePieces(r, mergeBase, report.RevertedFiles)
	if err != nil {
		return Report{}, err
	}
	limit := opts.MaxPieces
	if limit <= 0 {
		limit = DefaultMaxPieces
	}
	if len(pieces) > limit {
		report.Outcome = OutcomeTooManyPieces
		for _, pc := range pieces {
			report.Pieces = append(report.Pieces, PieceResult{File: pc.file, Line: pc.line, Lines: pc.lines, Snippet: pc.snippet})
		}
		return report, nil
	}
	if len(pieces) == 0 {
		report.Outcome = OutcomeNoCodeChanged
		return report, nil
	}
	newOut, newErr := runTest(ctx, r.root, opts.Test, opts.Timeout)
	report.NewOutput = tail(newOut)
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	if newErr != nil {
		report.Outcome = OutcomeFailsOnNew
		return report, nil
	}
	report.Outcome = OutcomeProven
	for _, pc := range pieces {
		res := PieceResult{File: pc.file, Line: pc.line, Lines: pc.lines, Snippet: pc.snippet}
		if err := applyPiece(r, pc); err != nil {
			if errors.Is(err, errStashTaken) {
				return Report{}, err
			}
			if restoreErr := Restore(context.WithoutCancel(ctx), r.root); restoreErr != nil {
				return Report{}, fmt.Errorf("undo piece %s:%d failed: %v; restore also failed: %w", pc.file, pc.line, err, restoreErr)
			}
			return Report{}, fmt.Errorf("undo piece %s:%d failed (files restored): %w", pc.file, pc.line, err)
		}
		out, testErr := runTest(ctx, r.root, opts.Test, opts.Timeout)
		if err := Restore(context.WithoutCancel(ctx), r.root); err != nil {
			return Report{}, fmt.Errorf("restore failed; recover with `test-check prove --restore`: %w", err)
		}
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		if testErr == nil {
			res.Outcome = PieceUnguarded
			report.Outcome = OutcomePieceNotTested
		} else {
			res.Outcome = PieceGuarded
			res.BuildErrorSuspected = buildError.MatchString(out)
		}
		report.Pieces = append(report.Pieces, res)
	}
	return report, nil
}
