package check

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// MaxNearbyTests bounds how many existing test files are shown to the model.
const MaxNearbyTests = 8

var testFilePattern = regexp.MustCompile(`(_test\.go$|\.(test|spec)\.[cm]?[jt]sx?$|(^|/)test_[^/]*\.py$|_test\.py$|Tests?\.swift$|_test\.rs$|_spec\.rb$|(^|/)(tests?|__tests__|spec|Tests)/)`)

// IsTestFile reports whether a repository path looks like test code.
func IsTestFile(p string) bool {
	return testFilePattern.MatchString(p)
}

var codeExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".swift": true, ".rs": true, ".rb": true, ".kt": true, ".java": true, ".vue": true,
}

// IsCode reports whether a non-test path is source code whose behavior a test
// could protect. Docs, config, and assets are not.
func IsCode(p string) bool {
	return codeExtensions[strings.ToLower(path.Ext(p))] && !IsTestFile(p)
}

var testNamePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*func\s+(Test\w+)\s*\(`),                          // Go
	regexp.MustCompile(`(?m)^\s*(?:@\w+\s+)*func\s+(test\w+)\s*\(`),              // Swift XCTest
	regexp.MustCompile(`(?m)@Test[^\n]*\n\s*func\s+(\w+)`),                       // Swift Testing
	regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+(test_\w+)\s*\(`),             // Python
	regexp.MustCompile(`(?m)#\[(?:tokio::)?test\]\s*(?:async\s+)?fn\s+(\w+)`),    // Rust
	regexp.MustCompile("(?m)\\b(?:it|test)\\s*\\(\\s*['\"`]([^'\"`\\n]{1,100})"), // JS/TS
}

// TestNames lists the test cases declared in one test file, in file order,
// bounded so a huge file cannot crowd out the others.
func TestNames(content string) []string {
	type hit struct {
		at   int
		name string
	}
	var hits []hit
	for _, pattern := range testNamePatterns {
		for _, m := range pattern.FindAllStringSubmatchIndex(content, -1) {
			hits = append(hits, hit{at: m[2], name: content[m[2]:m[3]]})
		}
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].at < hits[b].at })
	names := make([]string, 0, min(len(hits), 40))
	for _, h := range hits {
		if len(names) == 40 {
			break
		}
		names = append(names, h.name)
	}
	return names
}

// NearbyTestPaths picks existing test files most likely to cover the changed
// code: test files that name a changed file (same directory first), then other
// tests in a changed file's directory. Files the change itself touches are
// excluded; their content is already in the diff.
func NearbyTestPaths(changed, all []string) []string {
	touched := map[string]bool{}
	for _, p := range changed {
		touched[p] = true
	}
	type candidate struct {
		path string
		rank int
	}
	best := map[string]int{}
	for _, c := range changed {
		if !IsCode(c) {
			continue
		}
		dir := path.Dir(c)
		stem := strings.ToLower(strings.TrimSuffix(path.Base(c), path.Ext(c)))
		for _, p := range all {
			if touched[p] || !IsTestFile(p) {
				continue
			}
			base := strings.ToLower(path.Base(p))
			sameDir := path.Dir(p) == dir
			exact := testSubject(base) == stem
			named := len(stem) >= 3 && strings.Contains(base, stem)
			rank := 0
			switch {
			case exact && sameDir:
				rank = 5
			case exact:
				rank = 4
			case named && sameDir:
				rank = 3
			case named:
				rank = 2
			case sameDir:
				rank = 1
			}
			if rank > best[p] {
				best[p] = rank
			}
		}
	}
	candidates := make([]candidate, 0, len(best))
	for p, rank := range best {
		if rank > 0 {
			candidates = append(candidates, candidate{p, rank})
		}
	}
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].rank != candidates[b].rank {
			return candidates[a].rank > candidates[b].rank
		}
		return candidates[a].path < candidates[b].path
	})
	out := make([]string, 0, min(len(candidates), MaxNearbyTests))
	for _, c := range candidates {
		if len(out) == MaxNearbyTests {
			break
		}
		out = append(out, c.path)
	}
	return out
}

// testSubject strips test markers from a lower-cased test file name, so
// "retry_test.go", "retry.test.ts", "test_retry.py" and "RetryTests.swift"
// all name "retry".
func testSubject(base string) string {
	name, _, _ := strings.Cut(base, ".")
	name = strings.TrimPrefix(name, "test_")
	for _, suffix := range []string{"_test", "_spec", "tests", "test"} {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok && trimmed != "" {
			return trimmed
		}
	}
	return name
}
