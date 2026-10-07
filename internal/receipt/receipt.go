// Package receipt stores local evidence independently of prove's recovery state.
// A receipt describes an invocation, not approval or a merge gate. Command and
// runner output may contain secrets supplied by the user; files stay private and
// local, with no claim that their contents have been redacted.
package receipt

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Version = 1
	// Match the existing prove runner's diagnostic tail rather than retaining
	// unbounded command output. Limit count and labels as well as output text.
	outputBytes = 2000
	outputLimit = 256
	labelBytes  = 256
)

// Record deliberately excludes input patches, prompts, environment and config.
// Missing identity or execution evidence must remain unknown, not inferred.
type Record struct {
	Version        int       `json:"version"`
	Mode           string    `json:"mode"`
	Repo           string    `json:"repo,omitempty"`
	Base           string    `json:"base,omitempty"`
	Head           string    `json:"head,omitempty"`
	WorktreeDigest string    `json:"worktree_digest,omitempty"`
	Command        string    `json:"command,omitempty"`
	Outcome        string    `json:"outcome"`
	Error          string    `json:"error,omitempty"`
	ToolRevision   string    `json:"tool_revision"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	Tests          []Test    `json:"tests,omitempty"`
	Pieces         []Piece   `json:"pieces,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
	OutputRef      string    `json:"output_ref"`
	// Checkout is only a storage safety boundary, not persisted metadata. When
	// absent an absolute Repo path supplies the same boundary.
	Checkout string `json:"-"`
}

// Execution values are observed, not_observed, or unknown. They describe named
// test evidence from the runner, not whether a shell process was launched.
type Test struct {
	Name         string   `json:"name"`
	Command      string   `json:"command,omitempty"`
	Outcome      string   `json:"outcome"`
	NewExecution string   `json:"new_execution"`
	OldExecution string   `json:"old_execution"`
	Warnings     []string `json:"warnings,omitempty"`
}

type Piece struct {
	File                string `json:"file"`
	Line                int    `json:"line"`
	Outcome             string `json:"outcome"`
	Reason              string `json:"reason,omitempty"`
	BuildErrorSuspected bool   `json:"build_error_suspected"`
}

type Output struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

type Paths struct {
	Receipt string
	Details string
}

type details struct {
	Version        int      `json:"version"`
	Outputs        []Output `json:"outputs"`
	Truncated      bool     `json:"truncated"`
	OmittedOutputs int      `json:"omitted_outputs,omitempty"`
}

// Save writes details first, then atomically publishes receipt.json as the
// completion marker. Each invocation has its own unpredictable directory.
// Call only after restoration; errors must not replace the original run result.
// No model context is accepted: cancellation of advice must not cancel saving
// its outcome. This does not guarantee capture after SIGKILL or power loss.
// Empty root uses XDG_STATE_HOME/test-check/receipts, falling back to
// ~/.local/state/test-check/receipts. Existing directories are never chmodded.
func Save(root string, record Record, outputs []Output) (paths Paths, err error) {
	var ownedParent string
	if root == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" || !filepath.IsAbs(state) {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return Paths{}, fmt.Errorf("receipt home: %w", homeErr)
			}
			state = filepath.Join(home, ".local", "state")
		}
		ownedParent = filepath.Join(state, "test-check")
		root = filepath.Join(ownedParent, "receipts")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Paths{}, err
	}
	checkout := record.Checkout
	if checkout == "" && filepath.IsAbs(record.Repo) {
		checkout = record.Repo
	}
	if checkout != "" {
		resolvedRoot, resolveErr := resolveFuturePath(root)
		if resolveErr != nil {
			return Paths{}, resolveErr
		}
		resolvedCheckout, resolveErr := resolveFuturePath(checkout)
		if resolveErr != nil {
			return Paths{}, resolveErr
		}
		rel, relErr := filepath.Rel(resolvedCheckout, resolvedRoot)
		if relErr != nil {
			return Paths{}, relErr
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return Paths{}, errors.New("receipt storage must be outside the checkout")
		}
	}
	if ownedParent != "" {
		if err := privateDir(ownedParent); err != nil {
			return Paths{}, err
		}
	}
	if err := privateDir(root); err != nil {
		return Paths{}, err
	}
	// Root anchors operations even if an ancestor is renamed. Fixed artifact
	// names cannot follow a symlink outside this directory.
	storage, err := os.OpenRoot(root)
	if err != nil {
		return Paths{}, err
	}
	defer storage.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Paths{}, err
	}
	name := time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random[:])
	if err := storage.Mkdir(name, 0o700); err != nil {
		return Paths{}, err
	}
	defer func() {
		if err != nil {
			// Only this invocation's unpublished artifacts are removed.
			_ = storage.RemoveAll(name)
		}
	}()
	dir, err := storage.OpenRoot(name)
	if err != nil {
		return Paths{}, err
	}
	defer dir.Close()
	paths = Paths{
		Receipt: filepath.Join(root, name, "receipt.json"),
		Details: filepath.Join(root, name, "detail.json"),
	}
	record.Version = Version
	record.OutputRef = paths.Details
	if err = publish(dir, "detail.json", boundedDetails(outputs)); err != nil {
		return Paths{}, err
	}
	if err = publish(dir, "receipt.json", record); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("receipt directory %q must be a private directory, not a symlink", path)
	}
	return nil
}

// Resolve existing ancestors too, so a symlink cannot hide a checkout-contained
// destination that has not been created yet.
func resolveFuturePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	resolved, err = resolveFuturePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(abs)), nil
}

func publish(dir *os.Root, name string, value any) error {
	file, err := dir.OpenFile("."+name+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return dir.Rename("."+name+".tmp", name)
}

func boundedDetails(outputs []Output) details {
	count := min(len(outputs), outputLimit)
	result := details{Version: Version, Outputs: make([]Output, count), OmittedOutputs: len(outputs) - count}
	result.Truncated = result.OmittedOutputs > 0
	for i, output := range outputs[:count] {
		if len(output.Label) > labelBytes {
			output.Label = strings.ToValidUTF8(output.Label[:labelBytes], "") + "..."
			result.Truncated = true
		}
		if len(output.Text) > outputBytes {
			output.Text = "..." + strings.ToValidUTF8(output.Text[len(output.Text)-outputBytes:], "")
			result.Truncated = true
		}
		result.Outputs[i] = output
	}
	return result
}
