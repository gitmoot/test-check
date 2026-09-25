package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/gitmoot/test-check/internal/check"
)

// Gh runs the gh CLI and returns stdout. It is a variable so tests can fake it.
var Gh = func(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

type ghFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

// PullRequest reads an open or merged pull request's current head.
func PullRequest(ctx context.Context, repo string, number int) (check.Input, error) {
	var pr struct {
		Title string `json:"title"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := ghJSON(ctx, &pr, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number)); err != nil {
		return check.Input{}, err
	}
	// GitHub serves at most 3000 files over 30 pages of 100.
	var files []ghFile
	for page := 1; page <= 30; page++ {
		var batch []ghFile
		if err := ghJSON(ctx, &batch, "api", fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page)); err != nil {
			return check.Input{}, err
		}
		files = append(files, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return fromGitHub(ctx, repo, pr.Title, pr.Head.SHA, files)
}

// Compare reads the change between two commits, for example a pull request's
// first reviewed head against the commit its branch started from.
func Compare(ctx context.Context, repo, base, head, title string) (check.Input, error) {
	var cmp struct {
		Files []ghFile `json:"files"`
	}
	if err := ghJSON(ctx, &cmp, "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head)); err != nil {
		return check.Input{}, err
	}
	return fromGitHub(ctx, repo, title, head, cmp.Files)
}

func fromGitHub(ctx context.Context, repo, title, head string, ghFiles []ghFile) (check.Input, error) {
	if len(ghFiles) == 0 {
		return check.Input{}, errors.New("the change has no files")
	}
	in := check.Input{Repo: repo, Title: title, Head: head}
	for _, f := range ghFiles {
		in.Files = append(in.Files, check.File{Path: f.Filename, Patch: f.Patch})
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := ghJSON(ctx, &tree, "api", fmt.Sprintf("repos/%s/git/trees/%s?recursive=1", repo, head)); err != nil {
		return check.Input{}, err
	}
	all := make([]string, 0, len(tree.Tree))
	for _, entry := range tree.Tree {
		if entry.Type == "blob" {
			all = append(all, entry.Path)
		}
	}
	return withNearby(in, all, func(p string) (string, error) {
		out, err := Gh(ctx, "api", "-H", "Accept: application/vnd.github.raw",
			fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, escapePath(p), head))
		return string(out), err
	}), nil
}

func ghJSON(ctx context.Context, into any, args ...string) error {
	raw, err := Gh(ctx, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decode gh %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
