//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"telemount/internal/desktop"
	"telemount/internal/diskcache"
)

func TestTier1_F5_PersistentDiskCache(t *testing.T) {
	t.Run("T1-F5-01_ColdReadPopulatesDiskCache", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		payload := bytes.Repeat([]byte("hello-telemount-"), 1024) // 16 KiB
		uploadSeedFile(t, eng, store, fake, "/hello.txt", payload)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("diskcache.Open: %v", err)
		}
		defer mgr.Close()

		got, err := readAllCacheFile(context.Background(), mgr, "/hello.txt", int64(len(payload)))
		if err != nil {
			t.Fatalf("ReadFileAt: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("read mismatch: got %d bytes, want %d", len(got), len(payload))
		}
		if fake.Fetches() == 0 {
			t.Fatalf("expected cold read to fetch from backend")
		}
		st := mgr.Stats()
		if st.EntryCount < 1 || st.TotalBytes < int64(len(payload)) {
			t.Fatalf("unexpected cache stats after cold read: %+v", st)
		}
	})

	t.Run("T1-F5-02_WarmReadHitsDiskWithoutBackendFetch", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		payload := bytes.Repeat([]byte("warm-read-"), 512)
		uploadSeedFile(t, eng, store, fake, "/warm.bin", payload)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("diskcache.Open: %v", err)
		}
		defer mgr.Close()

		if _, err := readAllCacheFile(context.Background(), mgr, "/warm.bin", int64(len(payload))); err != nil {
			t.Fatalf("cold read: %v", err)
		}
		eng.Cache().Clear()
		before := fake.Fetches()

		got, err := readAllCacheFile(context.Background(), mgr, "/warm.bin", int64(len(payload)))
		if err != nil {
			t.Fatalf("warm read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("warm read bytes mismatch")
		}
		if fake.Fetches() != before {
			t.Fatalf("warm read triggered %d extra backend fetches", fake.Fetches()-before)
		}
	})

	t.Run("T1-F5-03_SurvivesRestartOffline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		payload := []byte("persisted across restart payload")
		uploadSeedFile(t, eng, store, fake, "/persist.txt", payload)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("diskcache.Open: %v", err)
		}
		if _, err := readAllCacheFile(context.Background(), mgr, "/persist.txt", int64(len(payload))); err != nil {
			t.Fatalf("warm cache: %v", err)
		}
		if err := mgr.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		fake.ResetCounters()

		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("reopen diskcache.Open: %v", err)
		}
		defer mgr2.Close()

		got, err := readAllCacheFile(context.Background(), mgr2, "/persist.txt", int64(len(payload)))
		if err != nil {
			t.Fatalf("offline read after restart: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("got %q, want %q", got, payload)
		}
		if fake.Fetches() != 0 {
			t.Fatalf("expected 0 backend fetches offline, got %d", fake.Fetches())
		}
	})

	t.Run("T1-F5-04_RandomOffsetReadAfterRestartOffline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		payload := make([]byte, 2<<20) // 2 MiB
		for i := range payload {
			payload[i] = byte((i * 31) % 251)
		}
		uploadSeedFile(t, eng, store, fake, "/big.bin", payload)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := readAllCacheFile(context.Background(), mgr, "/big.bin", int64(len(payload))); err != nil {
			t.Fatalf("populate cache: %v", err)
		}
		_ = mgr.Close()

		fake.SetOnline(false)
		eng.Cache().Clear()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer mgr2.Close()

		off := int64(512*1024 + 13)
		buf := make([]byte, 4096)
		n, err := mgr2.ReadFileAt(context.Background(), "/big.bin", buf, off)
		if err != nil || n != len(buf) {
			t.Fatalf("ReadFileAt random offset = (%d, %v), want (%d, nil)", n, err, len(buf))
		}
		if !bytes.Equal(buf, payload[off:off+int64(len(buf))]) {
			t.Fatalf("random-offset slice mismatch")
		}
	})

	t.Run("T1-F5-05_MultiFilePersistenceAcrossRestart", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		files := map[string][]byte{
			"/a.bin":     []byte("alpha-data"),
			"/dir/b.bin": []byte("bravo-data-longer"),
			"/dir/c.bin": []byte("charlie-data-content"),
		}
		for p, data := range files {
			uploadSeedFile(t, eng, store, fake, p, data)
		}

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		for p, data := range files {
			if _, err := readAllCacheFile(context.Background(), mgr, p, int64(len(data))); err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
		}
		_ = mgr.Close()

		fake.SetOnline(false)
		eng.Cache().Clear()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer mgr2.Close()

		if st := mgr2.Stats(); st.EntryCount != 3 {
			t.Fatalf("EntryCount = %d, want 3", st.EntryCount)
		}
		for p, want := range files {
			got, err := readAllCacheFile(context.Background(), mgr2, p, int64(len(want)))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("offline read %s = (%q, %v), want %q", p, got, err, want)
			}
		}
	})
}

func TestTier1_F6_LRUEviction(t *testing.T) {
	t.Run("T1-F6-01_DefaultLimit10GiB", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		if got := mgr.Stats().MaxBytes; got != 10*1024*1024*1024 {
			t.Fatalf("MaxBytes = %d, want 10737418240", got)
		}
	})

	t.Run("T1-F6-02_EvictsLRUUnpinnedWhenOverBudget", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		a := bytes.Repeat([]byte("A"), 1000)
		b := bytes.Repeat([]byte("B"), 1000)
		c := bytes.Repeat([]byte("C"), 1000)
		uploadSeedFile(t, eng, store, fake, "/a.bin", a)
		uploadSeedFile(t, eng, store, fake, "/b.bin", b)
		uploadSeedFile(t, eng, store, fake, "/c.bin", c)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2500, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		_, _ = readAllCacheFile(ctx, mgr, "/a.bin", 1000)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/b.bin", 1000)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/c.bin", 1000)

		st := mgr.Stats()
		if st.TotalBytes > 2500 || st.EvictionCount < 1 {
			t.Fatalf("unexpected stats after eviction: %+v", st)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		if _, err := readAllCacheFile(ctx, mgr, "/a.bin", 1000); err == nil {
			t.Fatalf("expected /a.bin (LRU) to be evicted and unreadable offline")
		}
		if gotB, err := readAllCacheFile(ctx, mgr, "/b.bin", 1000); err != nil || !bytes.Equal(gotB, b) {
			t.Fatalf("expected /b.bin cached offline, got err=%v", err)
		}
		if gotC, err := readAllCacheFile(ctx, mgr, "/c.bin", 1000); err != nil || !bytes.Equal(gotC, c) {
			t.Fatalf("expected /c.bin cached offline, got err=%v", err)
		}
	})

	t.Run("T1-F6-03_ReadingOlderEntryRefreshesRecency", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		a := bytes.Repeat([]byte("A"), 1000)
		b := bytes.Repeat([]byte("B"), 1000)
		c := bytes.Repeat([]byte("C"), 1000)
		uploadSeedFile(t, eng, store, fake, "/a.bin", a)
		uploadSeedFile(t, eng, store, fake, "/b.bin", b)
		uploadSeedFile(t, eng, store, fake, "/c.bin", c)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2500, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		_, _ = readAllCacheFile(ctx, mgr, "/a.bin", 1000)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/b.bin", 1000)
		time.Sleep(15 * time.Millisecond)
		// Touch A to make B the LRU entry.
		_, _ = readAllCacheFile(ctx, mgr, "/a.bin", 1000)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/c.bin", 1000)

		fake.SetOnline(false)
		eng.Cache().Clear()
		if _, err := readAllCacheFile(ctx, mgr, "/b.bin", 1000); err == nil {
			t.Fatalf("expected /b.bin (now LRU) to be evicted")
		}
		if gotA, err := readAllCacheFile(ctx, mgr, "/a.bin", 1000); err != nil || !bytes.Equal(gotA, a) {
			t.Fatalf("expected refreshed /a.bin to remain cached, err=%v", err)
		}
		if gotC, err := readAllCacheFile(ctx, mgr, "/c.bin", 1000); err != nil || !bytes.Equal(gotC, c) {
			t.Fatalf("expected /c.bin to remain cached, err=%v", err)
		}
	})

	t.Run("T1-F6-04_RuntimeSetMaxBytesShrinkTrimsImmediately", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		for _, name := range []string{"/1.bin", "/2.bin", "/3.bin"} {
			uploadSeedFile(t, eng, store, fake, name, bytes.Repeat([]byte("X"), 1000))
		}
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 4000, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		for _, name := range []string{"/1.bin", "/2.bin", "/3.bin"} {
			if _, err := readAllCacheFile(ctx, mgr, name, 1000); err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
		}
		if err := mgr.SetMaxBytes(1500); err != nil {
			t.Fatalf("SetMaxBytes(1500): %v", err)
		}
		st := mgr.Stats()
		if st.TotalBytes > 1500 || st.EntryCount != 1 || st.EvictionCount < 2 {
			t.Fatalf("unexpected stats after shrink: %+v", st)
		}
	})

	t.Run("T1-F6-05_ClearUnpinnedFreesAllUnpinned", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		for _, name := range []string{"/1.bin", "/2.bin", "/3.bin"} {
			uploadSeedFile(t, eng, store, fake, name, bytes.Repeat([]byte("U"), 500))
		}
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		for _, name := range []string{"/1.bin", "/2.bin", "/3.bin"} {
			if _, err := readAllCacheFile(ctx, mgr, name, 500); err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
		}
		if err := mgr.ClearUnpinned(); err != nil {
			t.Fatalf("ClearUnpinned: %v", err)
		}
		st := mgr.Stats()
		if st.UnpinnedBytes != 0 || st.EntryCount != 0 {
			t.Fatalf("expected empty cache after ClearUnpinned, got %+v", st)
		}
	})
}

func TestTier1_F7_OfflinePinning(t *testing.T) {
	t.Run("T1-F7-01_PinSingleFileExemptFromLRU", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		vip := bytes.Repeat([]byte("V"), 1000)
		uploadSeedFile(t, eng, store, fake, "/vip.bin", vip)
		for _, name := range []string{"/u1.bin", "/u2.bin", "/u3.bin"} {
			uploadSeedFile(t, eng, store, fake, name, bytes.Repeat([]byte("U"), 1000))
		}

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2500, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		if err := mgr.SetPinnedPaths([]string{"/vip.bin"}); err != nil {
			t.Fatalf("SetPinnedPaths: %v", err)
		}
		ctx := context.Background()
		if _, err := readAllCacheFile(ctx, mgr, "/vip.bin", 1000); err != nil {
			t.Fatalf("read /vip.bin: %v", err)
		}
		for _, name := range []string{"/u1.bin", "/u2.bin", "/u3.bin"} {
			time.Sleep(10 * time.Millisecond)
			if _, err := readAllCacheFile(ctx, mgr, name, 1000); err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		got, err := readAllCacheFile(ctx, mgr, "/vip.bin", 1000)
		if err != nil || !bytes.Equal(got, vip) {
			t.Fatalf("pinned /vip.bin was evicted or unreadable offline: err=%v", err)
		}
	})

	t.Run("T1-F7-02_PinFolderPrefixExemptsNestedFiles", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		wa := bytes.Repeat([]byte("A"), 800)
		wb := bytes.Repeat([]byte("B"), 800)
		uploadSeedFile(t, eng, store, fake, "/work/a.txt", wa)
		uploadSeedFile(t, eng, store, fake, "/work/sub/b.txt", wb)
		for _, name := range []string{"/x1.bin", "/x2.bin", "/x3.bin"} {
			uploadSeedFile(t, eng, store, fake, name, bytes.Repeat([]byte("X"), 1000))
		}

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2500, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		if err := mgr.SetPinnedPaths([]string{"/work"}); err != nil {
			t.Fatalf("SetPinnedPaths: %v", err)
		}
		ctx := context.Background()
		_, _ = readAllCacheFile(ctx, mgr, "/work/a.txt", 800)
		_, _ = readAllCacheFile(ctx, mgr, "/work/sub/b.txt", 800)
		for _, name := range []string{"/x1.bin", "/x2.bin", "/x3.bin"} {
			time.Sleep(10 * time.Millisecond)
			_, _ = readAllCacheFile(ctx, mgr, name, 1000)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		if gotA, err := readAllCacheFile(ctx, mgr, "/work/a.txt", 800); err != nil || !bytes.Equal(gotA, wa) {
			t.Fatalf("/work/a.txt lost after churn: err=%v", err)
		}
		if gotB, err := readAllCacheFile(ctx, mgr, "/work/sub/b.txt", 800); err != nil || !bytes.Equal(gotB, wb) {
			t.Fatalf("/work/sub/b.txt lost after churn: err=%v", err)
		}
	})

	t.Run("T1-F7-03_PinSyncPrefetchesFolderSubtree", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		d1 := []byte("document-one")
		d2 := []byte("document-two-nested")
		uploadSeedFile(t, eng, store, fake, "/docs/1.txt", d1)
		uploadSeedFile(t, eng, store, fake, "/docs/sub/2.txt", d2)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		if err := mgr.SetPinnedPaths([]string{"/docs"}); err != nil {
			t.Fatalf("SetPinnedPaths: %v", err)
		}
		ctx := context.Background()
		if err := mgr.PinSync(ctx, "/docs"); err != nil {
			t.Fatalf("PinSync: %v", err)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		fake.ResetCounters()

		if got1, err := readAllCacheFile(ctx, mgr, "/docs/1.txt", int64(len(d1))); err != nil || !bytes.Equal(got1, d1) {
			t.Fatalf("/docs/1.txt offline read = (%q, %v)", got1, err)
		}
		if got2, err := readAllCacheFile(ctx, mgr, "/docs/sub/2.txt", int64(len(d2))); err != nil || !bytes.Equal(got2, d2) {
			t.Fatalf("/docs/sub/2.txt offline read = (%q, %v)", got2, err)
		}
		if fake.Fetches() != 0 {
			t.Fatalf("expected 0 backend fetches after PinSync, got %d", fake.Fetches())
		}
	})

	t.Run("T1-F7-04_ClearUnpinnedPreservesPinned", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		keep := []byte("keep-pinned-data")
		drop := []byte("drop-unpinned-data")
		uploadSeedFile(t, eng, store, fake, "/pin/keep.bin", keep)
		uploadSeedFile(t, eng, store, fake, "/temp/drop.bin", drop)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		_ = mgr.SetPinnedPaths([]string{"/pin"})
		ctx := context.Background()
		_, _ = readAllCacheFile(ctx, mgr, "/pin/keep.bin", int64(len(keep)))
		_, _ = readAllCacheFile(ctx, mgr, "/temp/drop.bin", int64(len(drop)))

		if err := mgr.ClearUnpinned(); err != nil {
			t.Fatalf("ClearUnpinned: %v", err)
		}
		st := mgr.Stats()
		if st.PinnedBytes == 0 || st.UnpinnedBytes != 0 {
			t.Fatalf("unexpected stats after ClearUnpinned: %+v", st)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		if got, err := readAllCacheFile(ctx, mgr, "/pin/keep.bin", int64(len(keep))); err != nil || !bytes.Equal(got, keep) {
			t.Fatalf("pinned file lost after ClearUnpinned: err=%v", err)
		}
		if _, err := readAllCacheFile(ctx, mgr, "/temp/drop.bin", int64(len(drop))); err == nil {
			t.Fatalf("expected /temp/drop.bin to be cleared")
		}
	})

	t.Run("T1-F7-05_UnpinningAllowsSubsequentLRUEviction", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		a := bytes.Repeat([]byte("A"), 1000)
		b := bytes.Repeat([]byte("B"), 1000)
		c := bytes.Repeat([]byte("C"), 1000)
		uploadSeedFile(t, eng, store, fake, "/a.bin", a)
		uploadSeedFile(t, eng, store, fake, "/b.bin", b)
		uploadSeedFile(t, eng, store, fake, "/c.bin", c)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 1500, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		_ = mgr.SetPinnedPaths([]string{"/a.bin"})
		_, _ = readAllCacheFile(ctx, mgr, "/a.bin", 1000)
		time.Sleep(10 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/b.bin", 1000)

		// Unpin /a.bin and read /c.bin to trigger eviction of /a.bin (oldest).
		if err := mgr.SetPinnedPaths(nil); err != nil {
			t.Fatalf("SetPinnedPaths(nil): %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = readAllCacheFile(ctx, mgr, "/c.bin", 1000)

		fake.SetOnline(false)
		eng.Cache().Clear()
		if _, err := readAllCacheFile(ctx, mgr, "/a.bin", 1000); err == nil {
			t.Fatalf("expected unpinned /a.bin to be evicted after churn")
		}
	})
}

func TestTier1_F8_OfflineWriteQueue(t *testing.T) {
	t.Run("T1-F8-01_OfflineCreateImmediatelyReadableAndQueued", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		staged, err := mgr.StageOpenForWrite("/offline.txt", true)
		if err != nil {
			t.Fatalf("StageOpenForWrite: %v", err)
		}
		data := []byte("hello offline")
		if err := os.WriteFile(staged, data, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := mgr.CommitFile(ctx, "/offline.txt", staged, int64(len(data)), time.Now().Unix()); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}

		ov, ok, err := mgr.Stat("/offline.txt")
		if err != nil || !ok || !ov.Dirty || ov.Size != int64(len(data)) {
			t.Fatalf("Stat(/offline.txt) = (%+v, %v, %v)", ov, ok, err)
		}
		entries, err := mgr.ListDir("/")
		if err != nil || len(entries) != 1 || entries[0].Path != "/offline.txt" {
			t.Fatalf("ListDir(/) = (%+v, %v)", entries, err)
		}
		got, err := readAllCacheFile(ctx, mgr, "/offline.txt", int64(len(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("ReadFileAt = (%q, %v), want %q", got, err, data)
		}
		if qs := mgr.QueueStatus(); qs.PendingCount != 1 {
			t.Fatalf("PendingCount = %d, want 1", qs.PendingCount)
		}
	})

	t.Run("T1-F8-02_DirtyWriteSurvivesRestartOffline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, false)
		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		ctx := context.Background()
		staged, err := mgr.StageOpenForWrite("/persist.txt", true)
		if err != nil {
			t.Fatalf("StageOpenForWrite: %v", err)
		}
		data := []byte("dirty across restart")
		_ = os.WriteFile(staged, data, 0o600)
		if err := mgr.CommitFile(ctx, "/persist.txt", staged, int64(len(data)), time.Now().Unix()); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}
		_ = mgr.Close()

		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer mgr2.Close()

		ov, ok, err := mgr2.Stat("/persist.txt")
		if err != nil || !ok || ov.Size != int64(len(data)) {
			t.Fatalf("Stat after restart = (%+v, %v, %v)", ov, ok, err)
		}
		got, err := readAllCacheFile(ctx, mgr2, "/persist.txt", int64(len(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("ReadFileAt after restart = (%q, %v)", got, err)
		}
		if qs := mgr2.QueueStatus(); qs.PendingCount != 1 {
			t.Fatalf("PendingCount after restart = %d, want 1", qs.PendingCount)
		}
	})

	t.Run("T1-F8-03_OfflineMkdirRenameDeleteUtimensOverlay", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/old.txt", []byte("old"))
		fake.SetOnline(false)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		if err := mgr.QueueMkdir(ctx, "/dir"); err != nil {
			t.Fatalf("QueueMkdir: %v", err)
		}
		staged, err := mgr.StageOpenForWrite("/dir/a.txt", true)
		if err != nil {
			t.Fatalf("StageOpenForWrite: %v", err)
		}
		_ = os.WriteFile(staged, []byte("moved-content"), 0o600)
		if err := mgr.CommitFile(ctx, "/dir/a.txt", staged, 13, 1700000000); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}
		if err := mgr.QueueRename(ctx, "/dir/a.txt", "/dir/b.txt"); err != nil {
			t.Fatalf("QueueRename: %v", err)
		}
		const wantMtime int64 = 1712345678
		if err := mgr.QueueUtimens(ctx, "/dir/b.txt", wantMtime); err != nil {
			t.Fatalf("QueueUtimens: %v", err)
		}
		if err := mgr.QueueDelete(ctx, "/old.txt", false); err != nil {
			t.Fatalf("QueueDelete: %v", err)
		}

		if _, ok, _ := mgr.Stat("/dir"); !ok {
			t.Fatalf("expected /dir in overlay")
		}
		if _, ok, _ := mgr.Stat("/dir/a.txt"); ok {
			t.Fatalf("expected /dir/a.txt absent after rename")
		}
		ovB, ok, err := mgr.Stat("/dir/b.txt")
		if err != nil || !ok || ovB.Mtime != wantMtime {
			t.Fatalf("Stat(/dir/b.txt) = (%+v, %v, %v), want mtime=%d", ovB, ok, err, wantMtime)
		}
		if _, ok, _ := mgr.Stat("/old.txt"); ok {
			t.Fatalf("expected /old.txt hidden after QueueDelete")
		}
	})

	t.Run("T1-F8-04_ReconnectFlushDrainsFIFOInOrder", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		for _, item := range []struct {
			path string
			data []byte
		}{
			{"/1.txt", []byte("one")},
			{"/2.txt", []byte("two")},
		} {
			st, err := mgr.StageOpenForWrite(item.path, true)
			if err != nil {
				t.Fatalf("Stage(%s): %v", item.path, err)
			}
			_ = os.WriteFile(st, item.data, 0o600)
			if err := mgr.CommitFile(ctx, item.path, st, int64(len(item.data)), time.Now().Unix()); err != nil {
				t.Fatalf("Commit(%s): %v", item.path, err)
			}
		}
		if err := mgr.QueueRename(ctx, "/2.txt", "/2b.txt"); err != nil {
			t.Fatalf("QueueRename: %v", err)
		}
		if err := mgr.QueueDelete(ctx, "/1.txt", false); err != nil {
			t.Fatalf("QueueDelete: %v", err)
		}

		fake.SetOnline(true)
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if err := eng.Packer().FlushNow(ctx); err != nil {
			t.Fatalf("Packer.FlushNow: %v", err)
		}

		if qs := mgr.QueueStatus(); qs.PendingCount != 0 {
			t.Fatalf("PendingCount = %d, want 0", qs.PendingCount)
		}
		if _, err := store.StatByPath("/1.txt"); err == nil {
			t.Fatalf("expected /1.txt deleted in db.Store after FIFO flush")
		}
		if _, err := store.StatByPath("/2.txt"); err == nil {
			t.Fatalf("expected /2.txt renamed away in db.Store")
		}
		got, err := readAllEngineFile(t, eng, store, "/2b.txt")
		if err != nil || string(got) != "two" {
			t.Fatalf("/2b.txt engine read = (%q, %v), want two", got, err)
		}
	})

	t.Run("T1-F8-05_OverwriteExistingFileOfflineAndSync", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/doc.txt", []byte("version-1"))
		fake.SetOnline(false)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		staged, err := mgr.StageOpenForWrite("/doc.txt", true)
		if err != nil {
			t.Fatalf("StageOpenForWrite: %v", err)
		}
		v2 := []byte("version-2-offline-overwrite")
		_ = os.WriteFile(staged, v2, 0o600)
		if err := mgr.CommitFile(ctx, "/doc.txt", staged, int64(len(v2)), time.Now().Unix()); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}

		gotOffline, err := readAllCacheFile(ctx, mgr, "/doc.txt", int64(len(v2)))
		if err != nil || !bytes.Equal(gotOffline, v2) {
			t.Fatalf("offline read = (%q, %v), want %q", gotOffline, err, v2)
		}

		fake.SetOnline(true)
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if err := eng.Packer().FlushNow(ctx); err != nil {
			t.Fatalf("Packer.FlushNow: %v", err)
		}
		gotOnline, err := readAllEngineFile(t, eng, store, "/doc.txt")
		if err != nil || !bytes.Equal(gotOnline, v2) {
			t.Fatalf("post-flush engine read = (%q, %v), want %q", gotOnline, err, v2)
		}
	})
}

func TestTier1_F9_CleanFlushShutdown(t *testing.T) {
	t.Run("T1-F9-01_QueueStatusTracksCountAndBytes", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		for _, item := range []struct {
			path string
			sz   int
		}{
			{"/q1.bin", 100},
			{"/q2.bin", 250},
		} {
			st, err := mgr.StageOpenForWrite(item.path, true)
			if err != nil {
				t.Fatalf("Stage: %v", err)
			}
			_ = os.WriteFile(st, bytes.Repeat([]byte("Q"), item.sz), 0o600)
			if err := mgr.CommitFile(ctx, item.path, st, int64(item.sz), time.Now().Unix()); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		}

		qs := mgr.QueueStatus()
		if qs.PendingCount != 2 || qs.PendingBytes != 350 || qs.Syncing {
			t.Fatalf("unexpected QueueStatus: %+v", qs)
		}
	})

	t.Run("T1-F9-02_FlushUpdatesLastSyncedAtAndClearsLastError", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		st, err := mgr.StageOpenForWrite("/f.txt", true)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		_ = os.WriteFile(st, []byte("retry-me"), 0o600)
		_ = mgr.CommitFile(ctx, "/f.txt", st, 8, time.Now().Unix())

		if err := mgr.FlushQueue(ctx); err == nil {
			t.Fatalf("expected error flushing while offline")
		}
		if qs := mgr.QueueStatus(); qs.LastError == "" {
			t.Fatalf("expected LastError populated after offline flush failure")
		}

		fake.SetOnline(true)
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("FlushQueue online: %v", err)
		}
		qs := mgr.QueueStatus()
		if qs.PendingCount != 0 || qs.PendingBytes != 0 || qs.LastError != "" || qs.LastSyncedAt <= 0 {
			t.Fatalf("unexpected QueueStatus after flush: %+v", qs)
		}
	})

	t.Run("T1-F9-03_StartSyncLoopDrainsWhenOnline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		st, err := mgr.StageOpenForWrite("/auto.txt", true)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		_ = os.WriteFile(st, []byte("auto-sync"), 0o600)
		if err := mgr.CommitFile(ctx, "/auto.txt", st, 9, time.Now().Unix()); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		mgr.StartSyncLoop(ctx, 25*time.Millisecond)
		fake.SetOnline(true)
		mustWaitUntil(t, 3*time.Second, func() bool {
			return mgr.QueueStatus().PendingCount == 0
		}, "sync loop did not drain queued write")
	})

	t.Run("T1-F9-04_CleanShutdownFlushesBeforeClose", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		ctx := context.Background()
		st, _ := mgr.StageOpenForWrite("/clean.txt", true)
		_ = os.WriteFile(st, []byte("shutdown-flush"), 0o600)
		_ = mgr.CommitFile(ctx, "/clean.txt", st, 14, time.Now().Unix())

		fake.SetOnline(true)
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if err := mgr.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer mgr2.Close()
		if qs := mgr2.QueueStatus(); qs.PendingCount != 0 {
			t.Fatalf("PendingCount after clean shutdown reopen = %d, want 0", qs.PendingCount)
		}
		if _, err := store.StatByPath("/clean.txt"); err != nil {
			t.Fatalf("expected /clean.txt committed in db.Store: %v", err)
		}
	})

	t.Run("T1-F9-05_FlushQueueEmptyIsIdempotentNoOp", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer mgr.Close()

		ctx := context.Background()
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("first empty FlushQueue: %v", err)
		}
		if err := mgr.FlushQueue(ctx); err != nil {
			t.Fatalf("second empty FlushQueue: %v", err)
		}
		if fake.Uploads() != 0 {
			t.Fatalf("expected 0 backend uploads on empty queue, got %d", fake.Uploads())
		}
	})
}
