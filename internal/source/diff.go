// Package source collects a change and its nearby tests, either from a local
// git checkout or from GitHub.
package source

import (
	"strconv"
	"strings"

	"github.com/gitmoot/test-check/internal/check"
)

// splitUnifiedDiff turns `git diff` output into one File per path, keeping each
// file's hunks (from the first "@@") as its patch, which matches the shape the
// GitHub API returns. A file with no hunks (binary, pure rename, mode change)
// gets an empty patch.
func splitUnifiedDiff(diff string) []check.File {
	var files []check.File
	for _, chunk := range strings.Split("\n"+diff, "\ndiff --git ")[1:] {
		header, body, _ := strings.Cut(chunk, "\n")
		path := diffPath(header, body)
		if path == "" {
			continue
		}
		patch := ""
		if at := strings.Index(body, "\n@@"); at >= 0 {
			patch = body[at+1:]
		} else if strings.HasPrefix(body, "@@") {
			patch = body
		}
		files = append(files, check.File{Path: path, Patch: strings.TrimRight(patch, "\n")})
	}
	return files
}

// diffPath prefers the "+++ b/" line, which is unambiguous even when a path
// contains spaces; deletions fall back to "--- a/".
func diffPath(header, body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "@@") {
			break
		}
		if rest, ok := strings.CutPrefix(line, "+++ b/"); ok {
			return rest
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "@@") {
			break
		}
		if rest, ok := strings.CutPrefix(line, "--- a/"); ok {
			return rest
		}
	}
	if _, b, ok := strings.Cut(header, " b/"); ok {
		return b
	}
	return ""
}

// newFilePatch renders an untracked file as an all-added patch.
func newFilePatch(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var b strings.Builder
	b.WriteString("@@ -0,0 +1," + strconv.Itoa(len(lines)) + " @@")
	for _, line := range lines {
		b.WriteString("\n+" + line)
	}
	return b.String()
}
