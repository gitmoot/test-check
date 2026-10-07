package prove

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// snapshotDigest identifies the already-enumerated change before any runner or
// temporary revert. Stream contents, never retain another full diff or copy.
// The base and HEAD identify unchanged tracked inputs; changed paths include
// test files and untracked files. Symlinks are hashed as links, not followed.
func snapshotDigest(ctx context.Context, r repo, base string, paths []string) (head, digest string, err error) {
	head, err = r.git("rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(head)
	h := sha256.New()
	fmt.Fprintf(h, "base:%s\nhead:%s\n", base, head)
	const maxSnapshotBytes = 64 << 20
	var total int64
	var buffer [32 << 10]byte
	for _, name := range paths {
		fmt.Fprintf(h, "path:%d:%s\n", len(name), name)
		if err := ctx.Err(); err != nil {
			return head, "", err
		}
		path := filepath.Join(r.root, name)
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			io.WriteString(h, "deleted\n")
			continue
		}
		if statErr != nil {
			return head, "", statErr
		}
		fmt.Fprintf(h, "mode:%s\n", info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return head, "", readErr
			}
			fmt.Fprintf(h, "link:%d:%s\n", len(target), target)
			continue
		}
		if !info.Mode().IsRegular() {
			return head, "", fmt.Errorf("cannot snapshot non-regular changed path %q", name)
		}
		if info.Size() > maxSnapshotBytes-total {
			return head, "", fmt.Errorf("changed input exceeds %d-byte snapshot limit", maxSnapshotBytes)
		}
		fmt.Fprintf(h, "size:%d\n", info.Size())
		file, openErr := os.Open(path)
		if openErr != nil {
			return head, "", openErr
		}
		var readErr error
		for {
			if readErr = ctx.Err(); readErr != nil {
				break
			}
			var n int
			n, readErr = file.Read(buffer[:])
			total += int64(n)
			if total > maxSnapshotBytes {
				readErr = fmt.Errorf("changed input exceeds %d-byte snapshot limit", maxSnapshotBytes)
				break
			}
			h.Write(buffer[:n])
			if readErr == io.EOF {
				readErr = nil
				break
			}
			if readErr != nil {
				break
			}
		}
		closeErr := file.Close()
		if readErr != nil {
			return head, "", readErr
		}
		if closeErr != nil {
			return head, "", closeErr
		}
		io.WriteString(h, "\n")
	}
	return head, hex.EncodeToString(h.Sum(nil)), nil
}
