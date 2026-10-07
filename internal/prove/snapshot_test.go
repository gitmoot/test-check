package prove

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestChangedInputSnapshotDistinguishesLinksAndBoundsReads(t *testing.T) {
	dir := fixture(t)
	r, err := open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.git("merge-base", "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, first, err := snapshotDigest(context.Background(), r, base, []string{"link"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("changed external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, second, err := snapshotDigest(context.Background(), r, base, []string{"link"})
	if err != nil || first != second {
		t.Fatalf("link target content was followed: %s %s %v", first, second, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"directory", "oversized"} {
		_, digest, err := snapshotDigest(context.Background(), r, base, []string{name})
		if err == nil || digest != "" {
			t.Fatalf("%s: unsupported identity claimed: %q %v", name, digest, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, digest, err := snapshotDigest(ctx, r, base, []string{"retry.go"})
	if err == nil || digest != "" {
		t.Fatalf("canceled snapshot claimed identity: %q %v", digest, err)
	}
}
