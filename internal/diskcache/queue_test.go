package diskcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"telemount/internal/db"
)

func writeStagedFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestQueueScenario1And2_StageNonTruncateAndTruncate(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/file.txt", []byte("HELLO-WORLD"), "e1")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	// Populate cache while online, then go offline.
	if _, err := mgr.ReadFileAt(context.Background(), "/file.txt", make([]byte, 11), 0); err != nil {
		t.Fatalf("ReadFileAt: %v", err)
	}
	fb.SetOnline(false)

	// Scenario 1: StageOpenForWrite(truncate=false) materializes cached bytes for partial write.
	staged1, err := mgr.StageOpenForWrite("/file.txt", false)
	if err != nil {
		t.Fatalf("StageOpenForWrite(false): %v", err)
	}
	f, err := os.OpenFile(staged1, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open staged1: %v", err)
	}
	if _, err := f.WriteAt([]byte("THERE"), 6); err != nil {
		_ = f.Close()
		t.Fatalf("WriteAt: %v", err)
	}
	_ = f.Close()

	if err := mgr.CommitFile(context.Background(), "/file.txt", staged1, 11, 1700000001); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	out := make([]byte, 11)
	if _, err := mgr.ReadFileAt(context.Background(), "/file.txt", out, 0); err != nil {
		t.Fatalf("ReadFileAt merged: %v", err)
	}
	if string(out) != "HELLO-THERE" {
		t.Fatalf("got %q, want HELLO-THERE", string(out))
	}

	// Scenario 2: StageOpenForWrite(truncate=true) produces 0-byte staging; pre-commit reads still see HELLO-THERE.
	staged2, err := mgr.StageOpenForWrite("/file.txt", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite(true): %v", err)
	}
	fi, err := os.Stat(staged2)
	if err != nil || fi.Size() != 0 {
		t.Fatalf("expected 0-byte staging file, got size=%d err=%v", fi.Size(), err)
	}
	pre := make([]byte, 11)
	if _, err := mgr.ReadFileAt(context.Background(), "/file.txt", pre, 0); err != nil || string(pre) != "HELLO-THERE" {
		t.Fatalf("pre-commit read = %q (%v), want HELLO-THERE", string(pre), err)
	}
	writeStagedFile(t, staged2, []byte("NEW"))
	if err := mgr.CommitFile(context.Background(), "/file.txt", staged2, 3, 1700000002); err != nil {
		t.Fatalf("CommitFile truncated: %v", err)
	}
	post := make([]byte, 11)
	n, err := mgr.ReadFileAt(context.Background(), "/file.txt", post, 0)
	if !errors.Is(err, io.EOF) || n != 3 || string(post[:n]) != "NEW" {
		t.Fatalf("post-commit read = (%d, %q, %v), want (3, NEW, EOF)", n, string(post[:n]), err)
	}
}

func TestQueueScenario3_StageUncachedOfflineVsOnline(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	fb.PutFile("/remote.txt", []byte("REMOTE-DATA"), "er")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	if _, err := mgr.StageOpenForWrite("/remote.txt", false); !errors.Is(err, ErrOfflineUncached) {
		t.Fatalf("offline stage uncached = %v, want ErrOfflineUncached", err)
	}

	fb.SetOnline(true)
	staged, err := mgr.StageOpenForWrite("/remote.txt", false)
	if err != nil {
		t.Fatalf("online stage uncached: %v", err)
	}
	defer os.Remove(staged)
	data, err := os.ReadFile(staged)
	if err != nil || string(data) != "REMOTE-DATA" {
		t.Fatalf("staged content = %q (%v), want REMOTE-DATA", string(data), err)
	}
}

func TestQueueScenario4_OfflineDurabilityAcrossRestartAndLRUExemption(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "cache.db")
	fb := newFakeBackend(false)

	// Tiny maxBytes (10 bytes) smaller than dirty write (100 bytes).
	mgr, err := OpenWithBackend(cacheDir, dbPath, 10, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	payload := bytes.Repeat([]byte("DURABLE"), 15)
	staged, err := mgr.StageOpenForWrite("/offline.bin", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	writeStagedFile(t, staged, payload)
	if err := mgr.CommitFile(context.Background(), "/offline.bin", staged, int64(len(payload)), 1700000123); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}

	if err := mgr.SetMaxBytes(0); err != nil {
		t.Fatalf("SetMaxBytes(0): %v", err)
	}
	if err := mgr.ClearUnpinned(); err != nil {
		t.Fatalf("ClearUnpinned: %v", err)
	}
	if qs := mgr.QueueStatus(); qs.PendingCount != 1 || qs.PendingBytes != int64(len(payload)) {
		t.Fatalf("QueueStatus = %+v", qs)
	}
	_ = mgr.Close()

	// Reopen and verify Stat, ListDir, ReadFileAt, and QueueStatus all survived.
	mgr2, err := OpenWithBackend(cacheDir, dbPath, 0, nil, fb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()

	ov, found, err := mgr2.Stat("/offline.bin")
	if err != nil || !found || ov.Size != int64(len(payload)) || !ov.Dirty {
		t.Fatalf("Stat after restart = (%+v, %v, %v)", ov, found, err)
	}
	list, err := mgr2.ListDir("/")
	if err != nil || len(list) != 1 || list[0].Path != "/offline.bin" {
		t.Fatalf("ListDir = (%+v, %v)", list, err)
	}
	got := make([]byte, len(payload))
	n, err := mgr2.ReadFileAt(context.Background(), "/offline.bin", got, 0)
	if err != nil || n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("ReadFileAt after restart = (%d, %v)", n, err)
	}
	if qs := mgr2.QueueStatus(); qs.PendingCount != 1 {
		t.Fatalf("QueueStatus after restart = %+v", qs)
	}
}

func TestQueueScenario5_MultipleWritesRenameDeleteSamePathFIFO(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()
	write := func(p string, content string) {
		s, err := mgr.StageOpenForWrite(p, true)
		if err != nil {
			t.Fatalf("stage %s: %v", p, err)
		}
		writeStagedFile(t, s, []byte(content))
		if err := mgr.CommitFile(ctx, p, s, int64(len(content)), 1700000010); err != nil {
			t.Fatalf("commit %s: %v", p, err)
		}
	}

	write("/a.txt", "v1")
	write("/a.txt", "v2")
	if err := mgr.QueueRename(ctx, "/a.txt", "/b.txt"); err != nil {
		t.Fatalf("QueueRename: %v", err)
	}
	if err := mgr.QueueDelete(ctx, "/b.txt", false); err != nil {
		t.Fatalf("QueueDelete: %v", err)
	}
	write("/a.txt", "v3")

	// Overlay must show only /a.txt = v3, and /b.txt absent.
	if _, found, _ := mgr.Stat("/b.txt"); found {
		t.Fatal("expected /b.txt deleted in overlay")
	}
	buf := make([]byte, 2)
	if n, err := mgr.ReadFileAt(ctx, "/a.txt", buf, 0); err != nil || n != 2 || string(buf) != "v3" {
		t.Fatalf("overlay /a.txt = %q (%v), want v3", string(buf[:n]), err)
	}
	if qs := mgr.QueueStatus(); qs.PendingCount != 5 {
		t.Fatalf("PendingCount = %d, want 5", qs.PendingCount)
	}

	// Go online and flush FIFO; every intermediate op must replay with its exact original payload.
	fb.SetOnline(true)
	if err := mgr.FlushQueue(ctx); err != nil {
		t.Fatalf("FlushQueue: %v", err)
	}
	if qs := mgr.QueueStatus(); qs.PendingCount != 0 {
		t.Fatalf("PendingCount after flush = %d", qs.PendingCount)
	}
	ops := fb.Ops()
	if len(ops) != 5 {
		t.Fatalf("replayed ops count = %d, want 5: %+v", len(ops), ops)
	}
	if ops[0].op != OpWrite || ops[0].data != "v1" ||
		ops[1].op != OpWrite || ops[1].data != "v2" ||
		ops[2].op != OpRename || ops[2].path != "/a.txt" || ops[2].newPath != "/b.txt" ||
		ops[3].op != OpDelete || ops[3].path != "/b.txt" ||
		ops[4].op != OpWrite || ops[4].data != "v3" {
		t.Fatalf("unexpected FIFO replay order: %+v", ops)
	}
}

func TestQueueScenario6_OfflineDirRenameAndSubtreeOverlay(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "cache.db")
	fb := newFakeBackend(true)
	fb.PutFile("/dir/f1.txt", []byte("F1-CACHED"), "ef1")
	fb.PutFile("/dir/sub/f2.txt", []byte("F2-REMOTE"), "ef2")

	mgr, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	ctx := context.Background()

	// Cache /dir/f1.txt only, leave /dir/sub/f2.txt uncached, then go offline.
	if _, err := mgr.ReadFileAt(ctx, "/dir/f1.txt", make([]byte, 9), 0); err != nil {
		t.Fatalf("cache f1: %v", err)
	}
	fb.SetOnline(false)

	// Write dirty /dir/f3.txt offline, then rename /dir -> /dir2.
	s3, err := mgr.StageOpenForWrite("/dir/f3.txt", true)
	if err != nil {
		t.Fatalf("stage f3: %v", err)
	}
	writeStagedFile(t, s3, []byte("F3-DIRTY"))
	if err := mgr.CommitFile(ctx, "/dir/f3.txt", s3, 8, 1700000020); err != nil {
		t.Fatalf("commit f3: %v", err)
	}
	if err := mgr.QueueRename(ctx, "/dir", "/dir2"); err != nil {
		t.Fatalf("QueueRename dir: %v", err)
	}

	if _, found, _ := mgr.Stat("/dir"); found {
		t.Fatal("/dir should be hidden after rename")
	}
	if _, found, _ := mgr.Stat("/dir/sub/f2.txt"); found {
		t.Fatal("/dir/sub/f2.txt should be hidden after rename")
	}
	if _, found, _ := mgr.Stat("/dir2/sub/f2.txt"); !found {
		t.Fatal("/dir2/sub/f2.txt should be visible after rename")
	}
	b1 := make([]byte, 9)
	if _, err := mgr.ReadFileAt(ctx, "/dir2/f1.txt", b1, 0); err != nil || string(b1) != "F1-CACHED" {
		t.Fatalf("read /dir2/f1.txt = %q (%v)", string(b1), err)
	}
	b3 := make([]byte, 8)
	if _, err := mgr.ReadFileAt(ctx, "/dir2/f3.txt", b3, 0); err != nil || string(b3) != "F3-DIRTY" {
		t.Fatalf("read /dir2/f3.txt = %q (%v)", string(b3), err)
	}
	_ = mgr.Close()

	// Restart offline, verify overlay survived, then go online and flush.
	mgr2, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()
	if _, found, _ := mgr2.Stat("/dir2/sub/f2.txt"); !found {
		t.Fatal("/dir2/sub/f2.txt missing after restart")
	}
	fb.SetOnline(true)
	if err := mgr2.FlushQueue(ctx); err != nil {
		t.Fatalf("FlushQueue: %v", err)
	}
	// After flush, /dir2/f1.txt ETag refreshed -> still reads from disk cache without hitting backend!
	beforeReads := fb.readCount.Load()
	if _, err := mgr2.ReadFileAt(ctx, "/dir2/f1.txt", b1, 0); err != nil || string(b1) != "F1-CACHED" {
		t.Fatalf("post-flush read /dir2/f1.txt = %q (%v)", string(b1), err)
	}
	if fb.readCount.Load() != beforeReads {
		t.Fatalf("expected post-rename-flush cache hit, got backend read")
	}
}

func TestQueueScenario7_DeleteAndRecreateSameDirHidesOldChildren(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	fb.PutFile("/dir/old.txt", []byte("OLD"), "eo")
	fb.PutFile("/dir/sub/deep.txt", []byte("DEEP"), "ed")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()
	ctx := context.Background()

	if err := mgr.QueueDelete(ctx, "/dir", true); err != nil {
		t.Fatalf("QueueDelete: %v", err)
	}
	if err := mgr.QueueMkdir(ctx, "/dir"); err != nil {
		t.Fatalf("QueueMkdir: %v", err)
	}
	if _, found, _ := mgr.Stat("/dir/old.txt"); found {
		t.Fatal("/dir/old.txt resurrected after recreate")
	}
	if _, found, _ := mgr.Stat("/dir/sub/deep.txt"); found {
		t.Fatal("/dir/sub/deep.txt resurrected after recreate")
	}
	children, err := mgr.ListDir("/dir")
	if err != nil || len(children) != 0 {
		t.Fatalf("ListDir(/dir) = (%+v, %v), want empty", children, err)
	}

	fb.SetOnline(true)
	if err := mgr.FlushQueue(ctx); err != nil {
		t.Fatalf("FlushQueue: %v", err)
	}
	if _, found, _ := mgr.Stat("/dir/old.txt"); found {
		t.Fatal("/dir/old.txt still present after flush")
	}
	if ov, found, _ := mgr.Stat("/dir"); !found || !ov.IsDir {
		t.Fatalf("/dir missing after flush: %+v %v", ov, found)
	}
}

func TestQueueScenario8_FIFOPreservationWhenOnlineWithNonEmptyQueue(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()
	ctx := context.Background()

	if err := mgr.QueueMkdir(ctx, "/offline_dir"); err != nil {
		t.Fatalf("QueueMkdir: %v", err)
	}
	// Flip online WITHOUT flushing first; CommitFile will enqueue behind mkdir and flush in FIFO order.
	fb.SetOnline(true)
	s, err := mgr.StageOpenForWrite("/offline_dir/f.txt", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	writeStagedFile(t, s, []byte("ordered"))
	if err := mgr.CommitFile(ctx, "/offline_dir/f.txt", s, 7, 1700000030); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}

	ops := fb.Ops()
	if len(ops) != 2 || ops[0].op != OpMkdir || ops[0].path != "/offline_dir" || ops[1].op != OpWrite || ops[1].path != "/offline_dir/f.txt" {
		t.Fatalf("FIFO order broken: %+v", ops)
	}
}

func TestQueueScenario9_MidFlushFailureAndCrashRecoveryIdempotency(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "cache.db")
	fb := newFakeBackend(false)

	mgr, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	ctx := context.Background()

	_ = mgr.QueueMkdir(ctx, "/d1")
	_ = mgr.QueueMkdir(ctx, "/d2")
	_ = mgr.QueueMkdir(ctx, "/d3")

	fb.SetOnline(true)
	fb.mu.Lock()
	fb.failOpAt = 2
	fb.failErr = errors.New("transient network drop")
	fb.mu.Unlock()

	err = mgr.FlushQueue(ctx)
	if err == nil {
		t.Fatal("expected FlushQueue to stop and return error on op #2")
	}
	qs := mgr.QueueStatus()
	if qs.PendingCount != 2 || qs.LastError == "" {
		t.Fatalf("unexpected QueueStatus after partial failure: %+v", qs)
	}
	// Overlay must still reflect /d2 and /d3.
	if _, found, _ := mgr.Stat("/d2"); !found {
		t.Fatal("/d2 missing from overlay")
	}
	if _, found, _ := mgr.Stat("/d3"); !found {
		t.Fatal("/d3 missing from overlay")
	}

	// Simulate crash right after Op #2 succeeded remotely on backend before DB delete (status='applying').
	fb.mu.Lock()
	fb.nodes["/d2"] = &fakeNode{id: 99, isDir: true}
	fb.canonPaths["/d2"] = "/d2"
	fb.mu.Unlock()
	_ = mgr.Close()

	mgr2, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()
	if err := mgr2.FlushQueue(ctx); err != nil {
		t.Fatalf("crash-recovery FlushQueue: %v", err)
	}
	if qs := mgr2.QueueStatus(); qs.PendingCount != 0 {
		t.Fatalf("PendingCount = %d after recovery, want 0", qs.PendingCount)
	}
}

func TestQueueScenario10_RealEngineAndMockTGOfflineToOnlineRoundTrip(t *testing.T) {
	store, eng, mtg := setupTestEngine(t)
	ctx := context.Background()

	// Start offline (mockTG unauthorized).
	mtg.SetAuthorized(false)
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "desktop_cache.db")

	mgr, err := Open(cacheDir, dbPath, DefaultMaxBytes, store, eng)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := mgr.QueueMkdir(ctx, "/offline_work"); err != nil {
		t.Fatalf("QueueMkdir: %v", err)
	}
	s, err := mgr.StageOpenForWrite("/offline_work/hello.txt", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	want := []byte("hello real TeleMount engine offline queue!")
	writeStagedFile(t, s, want)
	if err := mgr.CommitFile(ctx, "/offline_work/hello.txt", s, int64(len(want)), 1700000500); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	if err := mgr.QueueRename(ctx, "/offline_work/hello.txt", "/offline_work/renamed.txt"); err != nil {
		t.Fatalf("QueueRename: %v", err)
	}
	_ = mgr.Close()

	// Reopen & go online.
	mtg.SetAuthorized(true)
	mgr2, err := Open(cacheDir, dbPath, DefaultMaxBytes, store, eng)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()

	if err := mgr2.FlushQueue(ctx); err != nil {
		t.Fatalf("FlushQueue: %v", err)
	}
	if qs := mgr2.QueueStatus(); qs.PendingCount != 0 {
		t.Fatalf("PendingCount = %d, want 0", qs.PendingCount)
	}

	// Verify Packer.FlushNow uploaded encrypted pack to mockTG.docs and fresh FileReader reads plaintext.
	mtg.mu.Lock()
	docCount := len(mtg.docs)
	mtg.mu.Unlock()
	if docCount == 0 {
		t.Fatal("expected Packer.FlushNow to upload ciphertext to mockTG")
	}

	node, err := store.StatByPath("/offline_work/renamed.txt")
	if err != nil {
		t.Fatalf("StatByPath in db.Store: %v", err)
	}
	fr, err := eng.OpenFile(ctx, node)
	if err != nil {
		t.Fatalf("eng.OpenFile: %v", err)
	}
	defer fr.Close()
	got, err := io.ReadAll(fr)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("decrypted content = %q (%v), want %q", string(got), err, string(want))
	}
}

func TestQueueScenario11_StartSyncLoopAndCleanClose(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.StartSyncLoop(ctx, 20*time.Millisecond)

	if err := mgr.QueueMkdir(ctx, "/sync_dir"); err != nil {
		t.Fatalf("QueueMkdir: %v", err)
	}
	fb.SetOnline(true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.QueueStatus().PendingCount == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if mgr.QueueStatus().PendingCount != 0 {
		t.Fatalf("sync loop did not flush queue in time: %+v", mgr.QueueStatus())
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestQueueScenario12_UtimensPersistenceAcrossFlushAndRestart(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "cache.db")
	fb := newFakeBackend(true)
	fb.PutFile("/timed.txt", []byte("abc"), "et1")

	mgr, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	ctx := context.Background()

	const customMtime int64 = 1650000123
	fb.SetOnline(false)
	if err := mgr.QueueUtimens(ctx, "/timed.txt", customMtime); err != nil {
		t.Fatalf("QueueUtimens: %v", err)
	}
	if ov, ok, _ := mgr.Stat("/timed.txt"); !ok || ov.Mtime != customMtime {
		t.Fatalf("offline Stat mtime = %+v, want %d", ov, customMtime)
	}

	fb.SetOnline(true)
	if err := mgr.FlushQueue(ctx); err != nil {
		t.Fatalf("FlushQueue: %v", err)
	}
	_ = mgr.Close()

	mgr2, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()

	if ov, ok, err := mgr2.Stat("/timed.txt"); err != nil || !ok || ov.Mtime != customMtime {
		t.Fatalf("post-restart Stat mtime = (%+v, %v, %v), want %d", ov, ok, err, customMtime)
	}
}

func TestQueueRenameOverwriteGuardsAndCrossDirStageCommit(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(false)
	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()
	ctx := context.Background()

	_ = mgr.QueueMkdir(ctx, "/d1")
	_ = mgr.QueueMkdir(ctx, "/d2")

	// Cross-dir stagedPath outside cacheDir (tests copy fallback / commit).
	extStage := filepath.Join(t.TempDir(), "external-stage.tmp")
	writeStagedFile(t, extStage, []byte("ext-data"))
	if err := mgr.CommitFile(ctx, "/d1/f.txt", extStage, 8, 1700000000); err != nil {
		t.Fatalf("CommitFile external stage: %v", err)
	}
	// Commit 0-byte file with empty stagedPath.
	if err := mgr.CommitFile(ctx, "/zero.txt", "", 0, 1700000000); err != nil {
		t.Fatalf("CommitFile 0-byte: %v", err)
	}

	// Cannot rename dir onto file or non-empty dir.
	if err := mgr.QueueRename(ctx, "/d1", "/zero.txt"); err == nil {
		t.Fatal("expected error renaming dir onto file")
	}
	if err := mgr.QueueRename(ctx, "/d2", "/d1"); err == nil {
		t.Fatal("expected error renaming dir onto non-empty dir")
	}
	// Cannot move dir into itself.
	if err := mgr.QueueRename(ctx, "/d1", "/d1/sub"); !errors.Is(err, db.ErrInvalidMove) {
		t.Fatalf("expected ErrInvalidMove, got %v", err)
	}
}

type blockingWriteBackend struct {
	*fakeBackend
	writeStarted chan string
	releaseWrite chan struct{}
}

func (b *blockingWriteBackend) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) (*RemoteNode, error) {
	select {
	case b.writeStarted <- p:
	default:
	}
	select {
	case <-b.releaseWrite:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeBackend.WriteFile(ctx, p, r, size, mtime)
}

func TestAsyncSyncLoopDoesNotBlockCommitOrMutationsDuringSlowUpload(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	fb := newFakeBackend(true)
	fb.PutDir("/Games")
	bb := &blockingWriteBackend{
		fakeBackend:  fb,
		writeStarted: make(chan string, 4),
		releaseWrite: make(chan struct{}),
	}

	mgr, err := OpenWithBackend(cacheDir, filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, bb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Long poll interval proves flushWakeCh triggers background upload immediately.
	mgr.StartSyncLoop(ctx, 10*time.Second)

	payload := bytes.Repeat([]byte("BIG-5GB-SIM-"), 4096)
	staged, err := mgr.StageOpenForWrite("/Games/big.zip", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	writeStagedFile(t, staged, payload)

	start := time.Now()
	if err := mgr.CommitFile(ctx, "/Games/big.zip", staged, int64(len(payload)), 1700000100); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("CommitFile blocked for %v while upload was in progress", elapsed)
	}

	select {
	case gotPath := <-bb.writeStarted:
		if gotPath != "/Games/big.zip" {
			t.Fatalf("upload started for %q, want /Games/big.zip", gotPath)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("background upload did not start immediately via flushWakeCh")
	}

	if st := mgr.OverlayState("/Games/big.zip"); st != OverlaySyncing {
		t.Fatalf("OverlayState while uploading = %q, want %q", st, OverlaySyncing)
	}

	// Subsequent filesystem metadata mutations, reads, and directory listings must stay non-blocking.
	opStart := time.Now()
	if err := mgr.QueueMkdir(ctx, "/Games/Other"); err != nil {
		t.Fatalf("QueueMkdir during upload: %v", err)
	}
	if err := mgr.QueueUtimens(ctx, "/Games/big.zip", 1700000200); err != nil {
		t.Fatalf("QueueUtimens during upload: %v", err)
	}
	if _, err := mgr.ListDir("/Games"); err != nil {
		t.Fatalf("ListDir during upload: %v", err)
	}
	readBuf := make([]byte, len(payload))
	if n, err := mgr.ReadFileAt(ctx, "/Games/big.zip", readBuf, 0); err != nil || n != len(payload) || !bytes.Equal(readBuf, payload) {
		t.Fatalf("ReadFileAt during upload = (%d, %v)", n, err)
	}
	if elapsed := time.Since(opStart); elapsed > 500*time.Millisecond {
		t.Fatalf("concurrent ops blocked for %v behind slow upload", elapsed)
	}

	// Explicit FlushQueue with a short deadline must respect ctx.Done() instead of hanging on flushSem.
	shortCtx, shortCancel := context.WithTimeout(ctx, 40*time.Millisecond)
	err = mgr.FlushQueue(shortCtx)
	shortCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FlushQueue with short timeout = %v, want DeadlineExceeded", err)
	}

	// Unblock upload and verify queue payload is unlinked while hard-linked blob stays intact.
	close(bb.releaseWrite)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.QueueStatus().PendingCount == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if qs := mgr.QueueStatus(); qs.PendingCount != 0 {
		t.Fatalf("queue did not drain after unblocking upload: %+v", qs)
	}
	if st := mgr.OverlayState("/Games/big.zip"); st != OverlayCached {
		t.Fatalf("OverlayState after upload = %q, want %q", st, OverlayCached)
	}
	if qEntries, err := os.ReadDir(filepath.Join(cacheDir, "queue")); err != nil || len(qEntries) != 0 {
		t.Fatalf("expected empty queue/ after flush, got %d entries (err=%v)", len(qEntries), err)
	}
	postBuf := make([]byte, len(payload))
	if n, err := mgr.ReadFileAt(ctx, "/Games/big.zip", postBuf, 0); err != nil || n != len(payload) || !bytes.Equal(postBuf, payload) {
		t.Fatalf("hard-linked blob read after queue unlink = (%d, %v)", n, err)
	}
}

func TestCloseCancelsInFlightSlowUpload(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	bb := &blockingWriteBackend{
		fakeBackend:  fb,
		writeStarted: make(chan string, 2),
		releaseWrite: make(chan struct{}),
	}

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, bb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	mgr.StartSyncLoop(context.Background(), 10*time.Second)

	staged, err := mgr.StageOpenForWrite("/in-flight.bin", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	writeStagedFile(t, staged, []byte("keep-safe"))
	if err := mgr.CommitFile(context.Background(), "/in-flight.bin", staged, 9, 1700000300); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}

	select {
	case <-bb.writeStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("upload did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- mgr.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung waiting on in-flight upload")
	}
}
