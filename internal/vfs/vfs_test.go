//go:build windows

package vfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/db"
	"telemount/internal/desktop"
	"telemount/internal/diskcache"
)

func newTestFS(t *testing.T) (*TeleFS, *diskcache.Manager) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return NewTeleFS(store, cache), cache
}

func TestTeleFSAtomicSaveWritebackAndCaseInsensitivity(t *testing.T) {
	fs, _ := newTestFS(t)

	// Simulate Office/VSCode atomic temp-file save pattern: create .tmp -> write -> flush/release -> rename over target.
	rc, fh := fs.Create("/doc.txt", fuse.O_CREAT|fuse.O_RDWR|fuse.O_TRUNC, 0o644)
	if rc != 0 {
		t.Fatalf("Create(/doc.txt) rc=%d", rc)
	}
	v1 := []byte("version-1-content")
	if n := fs.Write("/doc.txt", v1, 0, fh); n != len(v1) {
		t.Fatalf("Write v1 = %d", n)
	}
	if rc := fs.Flush("/doc.txt", fh); rc != 0 {
		t.Fatalf("Flush v1 rc=%d", rc)
	}
	if rc := fs.Release("/doc.txt", fh); rc != 0 {
		t.Fatalf("Release v1 rc=%d", rc)
	}

	// Write new version to temp file then atomically rename over /DOC.TXT (different case).
	rc, fh2 := fs.Create("/doc.txt.tmp", fuse.O_CREAT|fuse.O_RDWR|fuse.O_TRUNC, 0o644)
	if rc != 0 {
		t.Fatalf("Create tmp rc=%d", rc)
	}
	v2 := []byte("version-2-longer-content-from-editor")
	if n := fs.Write("/doc.txt.tmp", v2, 0, fh2); n != len(v2) {
		t.Fatalf("Write v2 = %d", n)
	}
	_ = fs.Flush("/doc.txt.tmp", fh2)
	_ = fs.Release("/doc.txt.tmp", fh2)

	if rc := fs.Rename("/doc.txt.tmp", "/doc.txt"); rc != 0 {
		t.Fatalf("atomic Rename overwrite rc=%d", rc)
	}

	// Case-insensitive lookup + Read verifies v2 intact.
	var st fuse.Stat_t
	if rc := fs.Getattr("/DOC.TXT", &st, 0); rc != 0 || st.Size != int64(len(v2)) {
		t.Fatalf("Getattr(/DOC.TXT) = (rc=%d, size=%d), want (0, %d)", rc, st.Size, len(v2))
	}
	rc, rfh := fs.Open("/DOC.TXT", fuse.O_RDONLY)
	if rc != 0 {
		t.Fatalf("Open rc=%d", rc)
	}
	buf := make([]byte, len(v2))
	n := fs.Read("/DOC.TXT", buf, 0, rfh)
	_ = fs.Release("/DOC.TXT", rfh)
	if n != len(v2) || !bytes.Equal(buf, v2) {
		t.Fatalf("Read got %q (n=%d), want %q", buf[:max(0, n)], n, v2)
	}
}

func TestTeleFSOpenRDWRIsLazyAndUtimensBeforeFlushPreservesMtime(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cacheDir := filepath.Join(dir, "cache")
	cache, err := diskcache.Open(cacheDir, filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	fs := NewTeleFS(store, cache)

	// Simulate Explorer CopyFileEx: Create -> Write -> SetFileTime (Utimens) -> Flush -> Release.
	rc, fh := fs.Create("/copy.bin", fuse.O_CREAT|fuse.O_WRONLY|fuse.O_TRUNC, 0o644)
	if rc != 0 {
		t.Fatalf("Create rc=%d", rc)
	}
	data := []byte("explorer-large-copy-sim")
	if n := fs.Write("/copy.bin", data, 0, fh); n != len(data) {
		t.Fatalf("Write = %d", n)
	}
	const wantMtime int64 = 1712345678
	if rc := fs.Utimens("/copy.bin", []fuse.Timespec{{Sec: wantMtime}, {Sec: wantMtime}}); rc != 0 {
		t.Fatalf("Utimens on open dirty handle rc=%d", rc)
	}
	if rc := fs.Flush("/copy.bin", fh); rc != 0 {
		t.Fatalf("Flush rc=%d", rc)
	}
	if rc := fs.Release("/copy.bin", fh); rc != 0 {
		t.Fatalf("Release rc=%d", rc)
	}

	var st fuse.Stat_t
	if rc := fs.Getattr("/copy.bin", &st, 0); rc != 0 || st.Mtim.Sec != wantMtime || st.Size != int64(len(data)) {
		t.Fatalf("Getattr = (rc=%d, size=%d, mtim=%d), want (0, %d, %d)", rc, st.Size, st.Mtim.Sec, len(data), wantMtime)
	}

	// Opening with O_RDWR without O_TRUNC and without writing must NOT copy the blob into staging/.
	rc, rwfh := fs.Open("/copy.bin", fuse.O_RDWR)
	if rc != 0 {
		t.Fatalf("Open(O_RDWR) rc=%d", rc)
	}
	stagingEntries, err := os.ReadDir(filepath.Join(cacheDir, "staging"))
	if err != nil {
		t.Fatalf("ReadDir staging: %v", err)
	}
	if len(stagingEntries) != 0 {
		t.Fatalf("expected lazy Open(O_RDWR) to create 0 staging files, got %d", len(stagingEntries))
	}
	got := make([]byte, len(data))
	if n := fs.Read("/copy.bin", got, 0, rwfh); n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("Read on lazy O_RDWR handle = (%d, %q)", n, got[:max(0, n)])
	}
	_ = fs.Release("/copy.bin", rwfh)
}

func TestMountManagerRejectsMissingWinFspSafely(t *testing.T) {
	fs, _ := newTestFS(t)
	probe := &stubProbe{st: desktop.WinFspStatus{Installed: false, Guidance: "Install WinFsp from https://winfsp.dev"}}
	mm := NewMountManager(fs, probe, &stubDriveChecker{})
	if err := mm.Mount("V:"); err == nil {
		t.Fatal("expected Mount to fail when WinFsp is missing")
	}
	if mm.IsMounted() {
		t.Fatal("expected IsMounted=false")
	}
	if err := mm.Unmount(context.Background()); err != nil {
		t.Fatalf("idempotent Unmount failed: %v", err)
	}
}

type stubProbe struct{ st desktop.WinFspStatus }
func (s *stubProbe) Check() desktop.WinFspStatus { return s.st }

type stubDriveChecker struct{}
func (s *stubDriveChecker) IsDriveInUse(string) (bool, error) { return false, nil }
