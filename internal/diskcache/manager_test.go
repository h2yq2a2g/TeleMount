package diskcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telemount/internal/crypto"
	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
)

type fakeOpRecord struct {
	op      QueueOpType
	path    string
	newPath string
	data    string
	mtime   int64
}

type fakeNode struct {
	id    int64
	isDir bool
	data  []byte
	mtime int64
	ctime int64
	etag  string
}

type fakeBackend struct {
	mu          sync.Mutex
	online      bool
	nextID      int64
	etagSeq     int64
	nodes       map[string]*fakeNode // keyed by normalized lowercase path
	canonPaths  map[string]string    // lower -> canonical path
	ops         []fakeOpRecord
	readCount   atomic.Int64
	openHook    func(ctx context.Context, path string)
	failOpAt    int // 1-based op index to fail once
	failErr     error
	opCallCount int
	flushCount  atomic.Int64
}

func newFakeBackend(online bool) *fakeBackend {
	fb := &fakeBackend{
		online:     online,
		nextID:     2,
		nodes:      make(map[string]*fakeNode),
		canonPaths: make(map[string]string),
	}
	fb.nodes["/"] = &fakeNode{id: 1, isDir: true, etag: "root"}
	fb.canonPaths["/"] = "/"
	return fb
}

func (fb *fakeBackend) SetOnline(v bool) {
	fb.mu.Lock()
	fb.online = v
	fb.mu.Unlock()
}

func (fb *fakeBackend) IsOnline() bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.online
}

func (fb *fakeBackend) PutFile(p string, data []byte, etag string) {
	norm := NormalizePath(p)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.ensureParentsLocked(norm)
	fb.etagSeq++
	if etag == "" {
		etag = fmt.Sprintf("etag-%d", fb.etagSeq)
	}
	lower := strings.ToLower(norm)
	now := time.Now().Unix()
	id := fb.nextID
	if existing, ok := fb.nodes[lower]; ok {
		id = existing.id
	} else {
		fb.nextID++
	}
	fb.nodes[lower] = &fakeNode{
		id:    id,
		isDir: false,
		data:  append([]byte(nil), data...),
		mtime: now,
		ctime: now,
		etag:  etag,
	}
	fb.canonPaths[lower] = norm
}

func (fb *fakeBackend) PutDir(p string) {
	norm := NormalizePath(p)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.ensureParentsLocked(norm)
	lower := strings.ToLower(norm)
	if _, ok := fb.nodes[lower]; !ok {
		now := time.Now().Unix()
		id := fb.nextID
		fb.nextID++
		fb.nodes[lower] = &fakeNode{id: id, isDir: true, mtime: now, ctime: now, etag: fmt.Sprintf("dir-%d", id)}
		fb.canonPaths[lower] = norm
	}
}

func (fb *fakeBackend) ensureParentsLocked(norm string) {
	dir := parentOfNormalized(norm)
	if dir == "/" {
		return
	}
	fb.ensureParentsLocked(dir)
	lower := strings.ToLower(dir)
	if _, ok := fb.nodes[lower]; !ok {
		now := time.Now().Unix()
		id := fb.nextID
		fb.nextID++
		fb.nodes[lower] = &fakeNode{id: id, isDir: true, mtime: now, ctime: now, etag: fmt.Sprintf("dir-%d", id)}
		fb.canonPaths[lower] = dir
	}
}

func (fb *fakeBackend) Stat(p string) (*RemoteNode, bool, error) {
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	n, ok := fb.nodes[lower]
	if !ok {
		return nil, false, nil
	}
	return &RemoteNode{
		ID:    n.id,
		Path:  fb.canonPaths[lower],
		IsDir: n.isDir,
		Size:  int64(len(n.data)),
		Mtime: n.mtime,
		Ctime: n.ctime,
		ETag:  n.etag,
	}, true, nil
}

func (fb *fakeBackend) ListDir(dirPath string) ([]RemoteNode, error) {
	norm := NormalizePath(dirPath)
	dirLower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	n, ok := fb.nodes[dirLower]
	if !ok {
		return nil, os.ErrNotExist
	}
	if !n.isDir {
		return nil, os.ErrInvalid
	}
	var out []RemoteNode
	for lower, child := range fb.nodes {
		if lower == "/" {
			continue
		}
		cp := fb.canonPaths[lower]
		if strings.ToLower(parentOfNormalized(cp)) == dirLower {
			out = append(out, RemoteNode{
				ID:    child.id,
				Path:  cp,
				IsDir: child.isDir,
				Size:  int64(len(child.data)),
				Mtime: child.mtime,
				Ctime: child.ctime,
				ETag:  child.etag,
			})
		}
	}
	return out, nil
}

type fakeReadCloser struct {
	*bytes.Reader
	closed bool
}

func (f *fakeReadCloser) Close() error {
	f.closed = true
	return nil
}

func (fb *fakeBackend) OpenReader(ctx context.Context, p string) (io.ReadCloser, int64, string, error) {
	if fb.openHook != nil {
		fb.openHook(ctx, p)
	}
	fb.readCount.Add(1)
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return nil, 0, "", ErrOfflineUncached
	}
	n, ok := fb.nodes[lower]
	if !ok {
		return nil, 0, "", os.ErrNotExist
	}
	if n.isDir {
		return nil, 0, "", os.ErrInvalid
	}
	cp := append([]byte(nil), n.data...)
	return &fakeReadCloser{Reader: bytes.NewReader(cp)}, int64(len(cp)), n.etag, nil
}

func (fb *fakeBackend) maybeFailLocked() error {
	fb.opCallCount++
	if fb.failOpAt > 0 && fb.opCallCount == fb.failOpAt {
		fb.failOpAt = 0
		if fb.failErr != nil {
			return fb.failErr
		}
		return errors.New("injected backend failure")
	}
	return nil
}

func (fb *fakeBackend) WriteFile(_ context.Context, p string, r io.Reader, size int64, mtime int64) (*RemoteNode, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return nil, storage.ErrNotReady
	}
	if err := fb.maybeFailLocked(); err != nil {
		return nil, err
	}
	fb.ensureParentsLocked(norm)
	fb.etagSeq++
	etag := fmt.Sprintf("etag-%d", fb.etagSeq)
	id := fb.nextID
	if existing, ok := fb.nodes[lower]; ok {
		id = existing.id
	} else {
		fb.nextID++
	}
	if mtime <= 0 {
		mtime = time.Now().Unix()
	}
	fb.nodes[lower] = &fakeNode{
		id:    id,
		isDir: false,
		data:  data,
		mtime: mtime,
		ctime: mtime,
		etag:  etag,
	}
	fb.canonPaths[lower] = norm
	fb.ops = append(fb.ops, fakeOpRecord{op: OpWrite, path: norm, data: string(data), mtime: mtime})
	return &RemoteNode{
		ID:    id,
		Path:  norm,
		IsDir: false,
		Size:  int64(len(data)),
		Mtime: mtime,
		Ctime: mtime,
		ETag:  etag,
	}, nil
}

func (fb *fakeBackend) Mkdir(_ context.Context, p string) error {
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return storage.ErrNotReady
	}
	if err := fb.maybeFailLocked(); err != nil {
		return err
	}
	fb.ensureParentsLocked(norm)
	if existing, ok := fb.nodes[lower]; ok {
		if !existing.isDir {
			return db.ErrNotDir
		}
		fb.ops = append(fb.ops, fakeOpRecord{op: OpMkdir, path: norm})
		return nil
	}
	now := time.Now().Unix()
	id := fb.nextID
	fb.nextID++
	fb.nodes[lower] = &fakeNode{id: id, isDir: true, mtime: now, ctime: now, etag: fmt.Sprintf("dir-%d", id)}
	fb.canonPaths[lower] = norm
	fb.ops = append(fb.ops, fakeOpRecord{op: OpMkdir, path: norm})
	return nil
}

func (fb *fakeBackend) Rename(_ context.Context, oldPath, newPath string) error {
	normOld := NormalizePath(oldPath)
	normNew := NormalizePath(newPath)
	oldLower := strings.ToLower(normOld)
	newLower := strings.ToLower(normNew)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return storage.ErrNotReady
	}
	if err := fb.maybeFailLocked(); err != nil {
		return err
	}
	if _, ok := fb.nodes[oldLower]; !ok {
		return db.ErrNotFound
	}
	fb.ensureParentsLocked(normNew)

	// Delete existing destination if any.
	for k := range fb.nodes {
		if isSubtreeMatch(k, newLower) {
			delete(fb.nodes, k)
			delete(fb.canonPaths, k)
		}
	}
	var toMove []string
	for k := range fb.nodes {
		if isSubtreeMatch(k, oldLower) {
			toMove = append(toMove, k)
		}
	}
	for _, k := range toMove {
		n := fb.nodes[k]
		cp := fb.canonPaths[k]
		rel := ""
		if len(cp) > len(normOld) {
			rel = cp[len(normOld):]
		}
		mapped := normNew + rel
		mappedLower := strings.ToLower(mapped)
		fb.etagSeq++
		n.etag = fmt.Sprintf("etag-%d", fb.etagSeq)
		delete(fb.nodes, k)
		delete(fb.canonPaths, k)
		fb.nodes[mappedLower] = n
		fb.canonPaths[mappedLower] = mapped
	}
	fb.ops = append(fb.ops, fakeOpRecord{op: OpRename, path: normOld, newPath: normNew})
	return nil
}

func (fb *fakeBackend) Delete(_ context.Context, p string, _ bool) error {
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return storage.ErrNotReady
	}
	if err := fb.maybeFailLocked(); err != nil {
		return err
	}
	if _, ok := fb.nodes[lower]; !ok {
		return db.ErrNotFound
	}
	for k := range fb.nodes {
		if isSubtreeMatch(k, lower) {
			delete(fb.nodes, k)
			delete(fb.canonPaths, k)
		}
	}
	fb.ops = append(fb.ops, fakeOpRecord{op: OpDelete, path: norm})
	return nil
}

func (fb *fakeBackend) SetMtime(_ context.Context, p string, mtime int64) error {
	norm := NormalizePath(p)
	lower := strings.ToLower(norm)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return storage.ErrNotReady
	}
	if err := fb.maybeFailLocked(); err != nil {
		return err
	}
	if n, ok := fb.nodes[lower]; ok {
		n.mtime = mtime
	}
	fb.ops = append(fb.ops, fakeOpRecord{op: OpUtimens, path: norm, mtime: mtime})
	return nil
}

func (fb *fakeBackend) FlushPending(_ context.Context) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.online {
		return storage.ErrNotReady
	}
	fb.flushCount.Add(1)
	return nil
}

func (fb *fakeBackend) Ops() []fakeOpRecord {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([]fakeOpRecord, len(fb.ops))
	copy(out, fb.ops)
	return out
}

// --- Real storage.Engine + mockTG test harness ---

type testMockTG struct {
	mu         sync.Mutex
	authorized bool
	nextID     int
	docs       map[int]*testMockDoc
	tracker    *tg.Tracker
	fetches    int
}

type testMockDoc struct {
	info tg.ChannelDoc
	data []byte
}

func newTestMockTG() *testMockTG {
	return &testMockTG{
		authorized: true,
		nextID:     1,
		docs:       make(map[int]*testMockDoc),
		tracker:    tg.NewTracker(),
	}
}

func (m *testMockTG) SetAuthorized(v bool) {
	m.mu.Lock()
	m.authorized = v
	m.mu.Unlock()
}

func (m *testMockTG) IsAuthorized() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authorized
}

func (m *testMockTG) Tracker() *tg.Tracker { return m.tracker }

func (m *testMockTG) UploadBlob(_ context.Context, fileName, caption string, r io.Reader, _ int64, _ int, onProgress func(done int64)) (*tg.UploadedDoc, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(int64(len(data)))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorized {
		return nil, tg.ErrNotAuthorized
	}
	id := m.nextID
	m.nextID++
	m.docs[id] = &testMockDoc{
		info: tg.ChannelDoc{
			MsgID:      id,
			FileName:   fileName,
			Caption:    caption,
			FileID:     int64(id * 100),
			AccessHash: int64(id * 200),
			FileRef:    []byte("ref"),
			DCID:       2,
			Size:       int64(len(data)),
		},
		data: data,
	}
	return &tg.UploadedDoc{
		MsgID:      id,
		FileID:     int64(id * 100),
		AccessHash: int64(id * 200),
		FileRef:    []byte("ref"),
		DCID:       2,
		Size:       int64(len(data)),
	}, nil
}

func (m *testMockTG) ForwardCopyDoc(_ context.Context, _, _ int64, _ int) (*tg.UploadedDoc, error) {
	return nil, io.EOF
}

func (m *testMockTG) FetchChunk(_ context.Context, loc tg.DocRef, offset int64, limit int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorized {
		return nil, tg.ErrNotAuthorized
	}
	m.fetches++
	doc, ok := m.docs[loc.MsgID]
	if !ok {
		return nil, io.EOF
	}
	if offset >= int64(len(doc.data)) {
		return nil, nil
	}
	end := offset + int64(limit)
	if end > int64(len(doc.data)) {
		end = int64(len(doc.data))
	}
	out := make([]byte, end-offset)
	copy(out, doc.data[offset:end])
	return out, nil
}

func (m *testMockTG) RefreshFileRef(_ context.Context, _ int) ([]byte, error) {
	return []byte("ref"), nil
}

func (m *testMockTG) DeleteMessages(_ context.Context, msgIDs []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range msgIDs {
		delete(m.docs, id)
	}
	return nil
}

func (m *testMockTG) ListChannelDocs(_ context.Context, prefix string, limit int) ([]tg.ChannelDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tg.ChannelDoc
	for _, d := range m.docs {
		if prefix == "" || strings.HasPrefix(d.info.FileName, prefix) {
			out = append(out, d.info)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *testMockTG) DownloadSmallDoc(_ context.Context, doc tg.ChannelDoc) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[doc.MsgID]
	if !ok {
		return nil, io.EOF
	}
	return append([]byte(nil), d.data...), nil
}

func setupTestEngine(t *testing.T) (*db.Store, *storage.Engine, *testMockTG) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if err := st.SetAdmin("admin", "secret123"); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if err := st.SetChannel(123456, 789, "TestChan"); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	key := crypto.DeriveMasterKey("passphrase-1234", salt)
	if err := st.SetMasterKey(key, salt); err != nil {
		t.Fatalf("SetMasterKey: %v", err)
	}
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mtg := newTestMockTG()
	eng := storage.NewEngine(store, st, mtg, filepath.Join(dir, "tmp"))
	return store, eng, mtg
}

// --- Tests ---

func TestPersistentCacheAcrossRestartAndOffline(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "desktop_cache.db")
	fb := newFakeBackend(true)
	payload := bytes.Repeat([]byte("hello-diskcache-"), 100)
	fb.PutFile("/docs/readme.txt", payload, "etag-1")

	mgr, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	buf := make([]byte, len(payload))
	n, err := mgr.ReadFileAt(context.Background(), "/docs/readme.txt", buf, 0)
	if err != nil || n != len(payload) || !bytes.Equal(buf, payload) {
		t.Fatalf("first ReadFileAt = (%d, %v), want %d bytes", n, err, len(payload))
	}
	if fb.readCount.Load() != 1 {
		t.Fatalf("readCount = %d, want 1", fb.readCount.Load())
	}

	// Second read while online must hit disk cache, not backend.
	buf2 := make([]byte, 32)
	n, err = mgr.ReadFileAt(context.Background(), "/docs/readme.txt", buf2, 16)
	if err != nil || n != 32 || !bytes.Equal(buf2, payload[16:48]) {
		t.Fatalf("second ReadFileAt = (%d, %v)", n, err)
	}
	if fb.readCount.Load() != 1 {
		t.Fatalf("expected cache hit; readCount = %d", fb.readCount.Load())
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen offline (engine == nil / backend offline) and read again.
	fb.SetOnline(false)
	mgr2, err := OpenWithBackend(cacheDir, dbPath, DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer mgr2.Close()

	buf3 := make([]byte, len(payload))
	n, err = mgr2.ReadFileAt(context.Background(), `\Docs\ReadMe.txt`, buf3, 0)
	if err != nil || n != len(payload) || !bytes.Equal(buf3, payload) {
		t.Fatalf("offline restart ReadFileAt = (%d, %v)", n, err)
	}
	if fb.readCount.Load() != 1 {
		t.Fatalf("offline read hit backend! readCount = %d", fb.readCount.Load())
	}
}

func TestReadFileAtEdgeCasesAndEOF(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/data.bin", []byte("0123456789"), "e1")
	fb.PutFile("/empty.bin", []byte{}, "e2")
	fb.PutFile("/uncached.bin", []byte("abc"), "e3")
	fb.PutDir("/folder")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	// Negative offset
	if _, err := mgr.ReadFileAt(context.Background(), "/data.bin", make([]byte, 4), -1); err == nil {
		t.Fatal("expected error for negative offset")
	}
	// Root or dir
	if _, err := mgr.ReadFileAt(context.Background(), "/", make([]byte, 4), 0); err == nil {
		t.Fatal("expected error reading root")
	}
	if _, err := mgr.ReadFileAt(context.Background(), "/folder", make([]byte, 4), 0); err == nil {
		t.Fatal("expected error reading dir")
	}
	// Full read ending exactly at EOF -> (len(buf), nil)
	buf := make([]byte, 4)
	n, err := mgr.ReadFileAt(context.Background(), "/data.bin", buf, 6)
	if err != nil || n != 4 || string(buf) != "6789" {
		t.Fatalf("exact EOF read = (%d, %q, %v), want (4, 6789, nil)", n, string(buf), err)
	}
	// Partial read crossing EOF -> (n, io.EOF)
	buf2 := make([]byte, 6)
	n, err = mgr.ReadFileAt(context.Background(), "/data.bin", buf2, 7)
	if !errors.Is(err, io.EOF) || n != 3 || string(buf2[:n]) != "789" {
		t.Fatalf("partial EOF read = (%d, %q, %v), want (3, 789, io.EOF)", n, string(buf2[:n]), err)
	}
	// Offset >= size -> (0, io.EOF)
	n, err = mgr.ReadFileAt(context.Background(), "/data.bin", buf2, 10)
	if !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("offset == size = (%d, %v), want (0, io.EOF)", n, err)
	}
	// 0-byte file -> (0, io.EOF)
	n, err = mgr.ReadFileAt(context.Background(), "/empty.bin", buf2, 0)
	if !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("empty file = (%d, %v), want (0, io.EOF)", n, err)
	}
	// Offline uncached -> ErrOfflineUncached
	fb.SetOnline(false)
	_, err = mgr.ReadFileAt(context.Background(), "/uncached.bin", buf2, 0)
	if !errors.Is(err, ErrOfflineUncached) {
		t.Fatalf("offline uncached err = %v, want ErrOfflineUncached", err)
	}
	// Non-existent -> os.ErrNotExist
	_, err = mgr.ReadFileAt(context.Background(), "/missing.bin", buf2, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v, want os.ErrNotExist", err)
	}
}

func TestStalenessInvalidationOnETagChange(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/notes.txt", []byte("version-1"), "etag-v1")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	buf := make([]byte, 9)
	if _, err := mgr.ReadFileAt(context.Background(), "/notes.txt", buf, 0); err != nil {
		t.Fatalf("initial read: %v", err)
	}
	// Simulate remote update / .tmdb restore changing ETag (same size!).
	fb.PutFile("/notes.txt", []byte("version-2"), "etag-v2")

	buf2 := make([]byte, 9)
	n, err := mgr.ReadFileAt(context.Background(), "/notes.txt", buf2, 0)
	if err != nil || n != 9 || string(buf2) != "version-2" {
		t.Fatalf("stale read got %q (%v), want version-2", string(buf2[:n]), err)
	}
	if fb.readCount.Load() != 2 {
		t.Fatalf("readCount = %d, want 2", fb.readCount.Load())
	}
}

func TestStrictLRUEvictionAndBudgetEnforcement(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/a.bin", bytes.Repeat([]byte("A"), 100), "ea")
	fb.PutFile("/b.bin", bytes.Repeat([]byte("B"), 100), "eb")
	fb.PutFile("/c.bin", bytes.Repeat([]byte("C"), 100), "ec")
	fb.PutFile("/big.bin", bytes.Repeat([]byte("X"), 500), "ex")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), 250, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	buf := make([]byte, 100)
	// Read a, then b, then touch a again -> b is LRU!
	_, _ = mgr.ReadFileAt(context.Background(), "/a.bin", buf, 0)
	_, _ = mgr.ReadFileAt(context.Background(), "/b.bin", buf, 0)
	_, _ = mgr.ReadFileAt(context.Background(), "/a.bin", buf, 0)
	// Reading c (100B) pushes total to 300 > 250 -> must evict b (LRU), keeping a and c!
	_, _ = mgr.ReadFileAt(context.Background(), "/c.bin", buf, 0)

	st := mgr.Stats()
	if st.TotalBytes != 200 || st.EntryCount != 2 || st.EvictionCount != 1 {
		t.Fatalf("unexpected stats after LRU eviction: %+v", st)
	}

	// Verify offline: a and c are cached, b was evicted.
	fb.SetOnline(false)
	if _, err := mgr.ReadFileAt(context.Background(), "/a.bin", buf, 0); err != nil {
		t.Fatalf("expected /a.bin cached: %v", err)
	}
	if _, err := mgr.ReadFileAt(context.Background(), "/c.bin", buf, 0); err != nil {
		t.Fatalf("expected /c.bin cached: %v", err)
	}
	if _, err := mgr.ReadFileAt(context.Background(), "/b.bin", buf, 0); !errors.Is(err, ErrOfflineUncached) {
		t.Fatalf("expected /b.bin evicted, got %v", err)
	}

	// Oversized unpinned file (> maxBytes) online read succeeds without exceeding maxBytes!
	fb.SetOnline(true)
	bigBuf := make([]byte, 500)
	n, err := mgr.ReadFileAt(context.Background(), "/big.bin", bigBuf, 0)
	if err != nil || n != 500 {
		t.Fatalf("oversized read = (%d, %v)", n, err)
	}
	if st2 := mgr.Stats(); st2.TotalBytes > 250 {
		t.Fatalf("oversized unpinned read violated MaxBytes: %+v", st2)
	}

	// SetMaxBytes(0) evicts remaining clean unpinned entries.
	if err := mgr.SetMaxBytes(0); err != nil {
		t.Fatalf("SetMaxBytes(0): %v", err)
	}
	if st3 := mgr.Stats(); st3.TotalBytes != 0 || st3.EntryCount != 0 {
		t.Fatalf("expected 0 cached bytes after SetMaxBytes(0), got %+v", st3)
	}
}

func TestPinningPrefixIsolationAndPinSync(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/docs/a.txt", bytes.Repeat([]byte("1"), 120), "e1")
	fb.PutFile("/docs/sub/b.txt", bytes.Repeat([]byte("2"), 120), "e2")
	fb.PutFile("/docs2/x.txt", bytes.Repeat([]byte("3"), 120), "e3")

	// Budget 150B < 240B pinned docs; pinned files must survive!
	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), 150, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	// Blank strings must be ignored (never pin "/").
	if err := mgr.SetPinnedPaths([]string{"", "   ", `\Docs`, "/docs"}); err != nil {
		t.Fatalf("SetPinnedPaths: %v", err)
	}
	pins := mgr.PinnedPaths()
	if len(pins) != 1 || pins[0] != "/Docs" {
		t.Fatalf("PinnedPaths = %v, want [/Docs]", pins)
	}

	if err := mgr.PinSync(context.Background(), "/docs"); err != nil {
		t.Fatalf("PinSync: %v", err)
	}
	// Reading /docs2/x.txt (120B) puts total at 360 > 150; /docs2/x.txt is unpinned so it gets evicted,
	// while /docs/a.txt and /docs/sub/b.txt remain pinned and intact!
	buf := make([]byte, 120)
	if _, err := mgr.ReadFileAt(context.Background(), "/docs2/x.txt", buf, 0); err != nil {
		t.Fatalf("read /docs2/x.txt: %v", err)
	}
	st := mgr.Stats()
	if st.PinnedEntryCount != 2 || st.PinnedBytes != 240 || st.UnpinnedBytes != 0 {
		t.Fatalf("unexpected pin stats: %+v", st)
	}

	// ClearUnpinned preserves pinned entries.
	if err := mgr.ClearUnpinned(); err != nil {
		t.Fatalf("ClearUnpinned: %v", err)
	}
	fb.SetOnline(false)
	if _, err := mgr.ReadFileAt(context.Background(), "/docs/sub/b.txt", buf, 0); err != nil {
		t.Fatalf("expected pinned file readable offline after ClearUnpinned: %v", err)
	}
	if err := mgr.PinSync(context.Background(), "/docs"); !errors.Is(err, ErrOfflineUncached) {
		t.Fatalf("offline PinSync = %v, want ErrOfflineUncached", err)
	}
}

func TestSingleflightDeduplicationAndCancelIsolation(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	payload := bytes.Repeat([]byte("SF"), 256)
	fb.PutFile("/shared.bin", payload, "esf")

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fb.openHook = func(_ context.Context, _ string) {
		once.Do(func() { close(started) })
		<-release
	}

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	errCh1 := make(chan error, 1)
	go func() {
		_, err := mgr.ReadFileAt(ctx1, "/shared.bin", make([]byte, len(payload)), 0)
		errCh1 <- err
	}()

	<-started

	const readers = 5
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := make([]byte, len(payload))
			n, err := mgr.ReadFileAt(context.Background(), "/shared.bin", b, 0)
			if err != nil || n != len(payload) || !bytes.Equal(b, payload) {
				errs <- fmt.Errorf("reader got (%d, %v)", n, err)
				return
			}
			errs <- nil
		}()
	}

	// Cancel caller 1 while download is in-flight; sibling readers must still succeed!
	cancel1()
	if err := <-errCh1; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller err = %v, want context.Canceled", err)
	}

	close(release)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if fb.readCount.Load() != 1 {
		t.Fatalf("singleflight readCount = %d, want 1", fb.readCount.Load())
	}
}

func TestRealEngineCacheAndMockTGOfflineRead(t *testing.T) {
	store, eng, mtg := setupTestEngine(t)
	ctx := context.Background()

	docsDir, err := store.MkdirAll("/docs")
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	want := bytes.Repeat([]byte("TeleMount-Real-Engine-"), 200)
	if _, err := eng.UploadFile(ctx, docsDir.ID, "report.bin", "", bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if err := eng.Packer().FlushNow(ctx); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	dir := t.TempDir()
	mgr, err := Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), DefaultMaxBytes, store, eng)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got := make([]byte, len(want))
	n, err := mgr.ReadFileAt(ctx, "/docs/report.bin", got, 0)
	if err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("ReadFileAt = (%d, %v)", n, err)
	}
	_ = mgr.Close()

	// Turn mockTG offline and reopen with engine == nil -> must still read from disk cache!
	mtg.SetAuthorized(false)
	mgr2, err := Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), DefaultMaxBytes, store, nil)
	if err != nil {
		t.Fatalf("reopen offline: %v", err)
	}
	defer mgr2.Close()

	got2 := make([]byte, len(want))
	n, err = mgr2.ReadFileAt(ctx, "/docs/report.bin", got2, 0)
	if err != nil || n != len(want) || !bytes.Equal(got2, want) {
		t.Fatalf("offline ReadFileAt = (%d, %v)", n, err)
	}
	_ = path.Clean("/")
}

func TestConcurrentReadWhileEvictAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	data1 := bytes.Repeat([]byte("V1"), 500)
	data2 := bytes.Repeat([]byte("V2"), 500)
	fb.PutFile("/hot.bin", data1, "ev1")
	fb.PutFile("/other.bin", bytes.Repeat([]byte("OT"), 500), "eot")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), 1200, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()
	if _, err := mgr.ReadFileAt(ctx, "/hot.bin", make([]byte, len(data1)), 0); err != nil {
		t.Fatalf("seed hot.bin: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 30)
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := make([]byte, len(data1))
			n, err := mgr.ReadFileAt(ctx, "/hot.bin", b, 0)
			if err != nil || n != len(data1) {
				errCh <- fmt.Errorf("concurrent read failed: (%d, %v)", n, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Trigger overwrite & eviction concurrently with readers.
		s, err := mgr.StageOpenForWrite("/hot.bin", true)
		if err != nil {
			errCh <- err
			return
		}
		_ = os.WriteFile(s, data2, 0o644)
		if err := mgr.CommitFile(ctx, "/hot.bin", s, int64(len(data2)), 1700000000); err != nil {
			errCh <- err
		}
	}()
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestOpenNilStoreAndNilEngineDefaultsAndOrphanCleanup(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	dbPath := filepath.Join(dir, "desktop_cache.db")

	// Seed orphan files before Open().
	_ = os.MkdirAll(filepath.Join(cacheDir, "staging"), 0o755)
	_ = os.MkdirAll(filepath.Join(cacheDir, "blobs", "ab"), 0o755)
	_ = os.MkdirAll(filepath.Join(cacheDir, "queue"), 0o755)
	orphanStage := filepath.Join(cacheDir, "staging", "stale.tmp")
	orphanBlob := filepath.Join(cacheDir, "blobs", "ab", "ab00.dat")
	orphanQueue := filepath.Join(cacheDir, "queue", "op-orphan.dat")
	_ = os.WriteFile(orphanStage, []byte("s"), 0o644)
	_ = os.WriteFile(orphanBlob, []byte("b"), 0o644)
	_ = os.WriteFile(orphanQueue, []byte("q"), 0o644)

	// Open with store == nil, engine == nil, maxBytes == -1 -> DefaultMaxBytes.
	mgr, err := Open(cacheDir, dbPath, -1, nil, nil)
	if err != nil {
		t.Fatalf("Open(nil, nil): %v", err)
	}
	defer mgr.Close()

	if st := mgr.Stats(); st.MaxBytes != DefaultMaxBytes {
		t.Fatalf("MaxBytes = %d, want %d", st.MaxBytes, DefaultMaxBytes)
	}
	if err := mgr.SetMaxBytes(-1); err == nil {
		t.Fatal("expected error for SetMaxBytes(-1)")
	}
	for _, p := range []string{orphanStage, orphanBlob, orphanQueue} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan %s was not cleaned on Open", p)
		}
	}

	ctx := context.Background()
	if err := mgr.QueueMkdir(ctx, "/local"); err != nil {
		t.Fatalf("QueueMkdir: %v", err)
	}
	s, err := mgr.StageOpenForWrite("/local/note.txt", true)
	if err != nil {
		t.Fatalf("StageOpenForWrite: %v", err)
	}
	_ = os.WriteFile(s, []byte("pure-local"), 0o644)
	if err := mgr.CommitFile(ctx, "/local/note.txt", s, 10, 1700000000); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	out := make([]byte, 10)
	n, err := mgr.ReadFileAt(ctx, "/local/note.txt", out, 0)
	if err != nil || n != 10 || string(out) != "pure-local" {
		t.Fatalf("ReadFileAt pure-local = (%d, %q, %v)", n, string(out), err)
	}
}

func TestEvictPathAndPathStatusPreservesDirtyAndCloud(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/work/a.txt", bytes.Repeat([]byte("A"), 120), "e1")
	fb.PutFile("/work/sub/b.txt", bytes.Repeat([]byte("B"), 80), "e2")
	fb.PutFile("/other/c.txt", bytes.Repeat([]byte("C"), 60), "e3")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	ctx := context.Background()
	_, _ = mgr.ReadFileAt(ctx, "/work/a.txt", make([]byte, 120), 0)
	_, _ = mgr.ReadFileAt(ctx, "/work/sub/b.txt", make([]byte, 80), 0)
	_, _ = mgr.ReadFileAt(ctx, "/other/c.txt", make([]byte, 60), 0)

	// Queue one dirty offline file inside /work so we verify EvictPath NEVER drops unsynced writes.
	fb.SetOnline(false)
	stPath, err := mgr.StageOpenForWrite("/work/dirty.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(stPath, []byte("unsynced"), 0o600)
	if err := mgr.CommitFile(ctx, "/work/dirty.txt", stPath, 8, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	ps, err := mgr.PathStatus("/work")
	if err != nil {
		t.Fatal(err)
	}
	if ps.CachedFiles != 3 || ps.CachedBytes != 208 || ps.DirtyFiles != 1 {
		t.Fatalf("PathStatus(/work) before evict = %+v", ps)
	}

	freed, evicted, skippedDirty, err := mgr.EvictPath("/work")
	if err != nil {
		t.Fatal(err)
	}
	if freed != 200 || evicted != 2 || skippedDirty != 1 {
		t.Fatalf("EvictPath(/work) = (freed=%d, evicted=%d, skippedDirty=%d), want (200, 2, 1)", freed, evicted, skippedDirty)
	}

	// /work/a.txt is now cloud-only (offline read fails), while dirty file & /other/c.txt remain intact!
	if _, err := mgr.ReadFileAt(ctx, "/work/a.txt", make([]byte, 120), 0); !errors.Is(err, ErrOfflineUncached) {
		t.Fatalf("expected /work/a.txt evicted from local cache, got %v", err)
	}
	if _, err := mgr.ReadFileAt(ctx, "/work/dirty.txt", make([]byte, 8), 0); err != nil {
		t.Fatalf("dirty file lost on EvictPath: %v", err)
	}
	if _, err := mgr.ReadFileAt(ctx, "/other/c.txt", make([]byte, 60), 0); err != nil {
		t.Fatalf("unrelated /other/c.txt evicted: %v", err)
	}

	// Re-going online seamlessly fetches /work/a.txt back from cloud (cloud untouched!).
	fb.SetOnline(true)
	buf := make([]byte, 120)
	if n, err := mgr.ReadFileAt(ctx, "/work/a.txt", buf, 0); err != nil || n != 120 {
		t.Fatalf("cloud re-fetch after EvictPath failed: (%d, %v)", n, err)
	}
}

func TestOverlayStateAndOnChange(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	fb.PutFile("/Test/microg.apk", bytes.Repeat([]byte("M"), 128), "em")
	fb.PutFile("/CloudOnly/note.txt", []byte("cloud"), "ec")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	var changeCount atomic.Int32
	mgr.SetOnChange(func(...string) {
		changeCount.Add(1)
	})

	// Root and missing -> OverlayNone
	if got := mgr.OverlayState("/"); got != OverlayNone {
		t.Fatalf("OverlayState(/) = %c, want N", got)
	}
	if got := mgr.OverlayState("/does-not-exist.txt"); got != OverlayNone {
		t.Fatalf("OverlayState(missing) = %c, want N", got)
	}

	// Uncached remote file and folder -> OverlayCloud
	if got := mgr.OverlayState("/Test/microg.apk"); got != OverlayCloud {
		t.Fatalf("uncached file = %c, want O", got)
	}
	if got := mgr.OverlayState("/Test"); got != OverlayCloud {
		t.Fatalf("uncached dir = %c, want O", got)
	}

	// Reading caches file locally -> OverlayCached (both file & parent dir)
	ctx := context.Background()
	if _, err := mgr.ReadFileAt(ctx, "/Test/microg.apk", make([]byte, 128), 0); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/test/MICROG.APK"); got != OverlayCached {
		t.Fatalf("cached file = %c, want C", got)
	}
	if got := mgr.OverlayState("/Test"); got != OverlayCached {
		t.Fatalf("dir with cached file = %c, want C", got)
	}

	// Pinning an already-cached file -> OverlayPinned
	if err := mgr.SetPinnedPaths([]string{"/Test/microg.apk"}); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/Test/microg.apk"); got != OverlayPinned {
		t.Fatalf("pinned cached file = %c, want P", got)
	}

	// Pinning an uncached file -> OverlaySyncing until PinSync finishes -> OverlayPinned
	if err := mgr.SetPinnedPaths([]string{"/Test/microg.apk", "/CloudOnly/note.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/CloudOnly/note.txt"); got != OverlaySyncing {
		t.Fatalf("pinned uncached file = %c, want S", got)
	}
	if err := mgr.PinSync(ctx, "/CloudOnly/note.txt"); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/CloudOnly/note.txt"); got != OverlayPinned {
		t.Fatalf("pinned synced file = %c, want P", got)
	}

	// Unpinning + EvictPath -> back to OverlayCloud
	if err := mgr.SetPinnedPaths(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := mgr.EvictPath("/Test/microg.apk"); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/Test/microg.apk"); got != OverlayCloud {
		t.Fatalf("evicted file = %c, want O", got)
	}

	// Offline dirty write -> OverlaySyncing until flushed -> OverlayCached
	fb.SetOnline(false)
	stPath, err := mgr.StageOpenForWrite("/Test/new.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(stPath, []byte("hello"), 0o600)
	if err := mgr.CommitFile(ctx, "/Test/new.txt", stPath, 5, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/Test/new.txt"); got != OverlaySyncing {
		t.Fatalf("dirty file = %c, want S", got)
	}
	if got := mgr.OverlayState("/Test"); got != OverlaySyncing {
		t.Fatalf("dir with dirty file = %c, want S", got)
	}

	fb.SetOnline(true)
	if err := mgr.FlushQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mgr.OverlayState("/Test/new.txt"); got != OverlayCached {
		t.Fatalf("flushed file = %c, want C", got)
	}
	if changeCount.Load() == 0 {
		t.Fatal("expected onChange callback to be triggered on cache/pin/queue transitions")
	}
}

func TestLargeCloudFileStreamsOnDemandWithoutFullDownload(t *testing.T) {
	store, eng, mtg := setupTestEngine(t)
	ctx := context.Background()

	moviesDir, err := store.MkdirAll("/movies")
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 5 MiB file (>= DefaultStreamThreshold 4 MiB, 10+ Telegram 512 KiB chunks).
	payload := make([]byte, 5<<20)
	for i := range payload {
		payload[i] = byte((i*17 + i/257) % 251)
	}
	if _, err := eng.UploadFile(ctx, moviesDir.ID, "demo.mp4", "video/mp4", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if err := eng.Packer().FlushNow(ctx); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	dir := t.TempDir()
	mgr, err := Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), DefaultMaxBytes, store, eng)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mgr.Close()

	// Simulate VLC opening MP4: read header (offset 0) and moov atom near EOF via reusable ReadStream.
	rs := mgr.OpenReadStream("/movies/demo.mp4")
	defer rs.Close()

	head := make([]byte, 4096)
	if n, err := rs.ReadAt(ctx, head, 0); err != nil || n != len(head) || !bytes.Equal(head, payload[:4096]) {
		t.Fatalf("header read = (%d, %v)", n, err)
	}
	tailOff := int64(len(payload) - 8192)
	tail := make([]byte, 4096)
	if n, err := rs.ReadAt(ctx, tail, tailOff); err != nil || n != len(tail) || !bytes.Equal(tail, payload[tailOff:tailOff+4096]) {
		t.Fatalf("tail seek read = (%d, %v)", n, err)
	}

	mtg.mu.Lock()
	fetches := mtg.fetches
	mtg.mu.Unlock()
	if fetches < 2 || fetches > 3 {
		t.Fatalf("expected on-demand range stream (2..3 TG chunk fetches for head+tail), got %d fetches", fetches)
	}
	if st := mgr.Stats(); st.EntryCount != 0 || st.TotalBytes != 0 {
		t.Fatalf("expected streamed cloud file not to dump full blob into disk cache, got %+v", st)
	}
	if ov := mgr.OverlayState("/movies/demo.mp4"); ov != OverlayCloud {
		t.Fatalf("OverlayState = %c, want %c", ov, OverlayCloud)
	}

	// Explicit PinSync still downloads into local disk cache, after which ReadStream serves offline!
	if err := mgr.SetPinnedPaths([]string{"/movies/demo.mp4"}); err != nil {
		t.Fatalf("SetPinnedPaths: %v", err)
	}
	if err := mgr.PinSync(ctx, "/movies/demo.mp4"); err != nil {
		t.Fatalf("PinSync: %v", err)
	}
	mtg.SetAuthorized(false)
	eng.Cache().Clear()

	midOff := int64(2<<20 + 123)
	mid := make([]byte, 4096)
	if n, err := rs.ReadAt(ctx, mid, midOff); err != nil || n != len(mid) || !bytes.Equal(mid, payload[midOff:midOff+4096]) {
		t.Fatalf("offline read after PinSync = (%d, %v)", n, err)
	}
}

func TestReadStreamReusesSingleBackendReaderAcrossSeeksAndETagInvalidation(t *testing.T) {
	dir := t.TempDir()
	fb := newFakeBackend(true)
	payload1 := bytes.Repeat([]byte("S"), 1024)
	payload2 := bytes.Repeat([]byte("T"), 1024)
	fb.PutFile("/video.mkv", payload1, "etag-1")

	mgr, err := OpenWithBackend(filepath.Join(dir, "cache"), filepath.Join(dir, "cache.db"), DefaultMaxBytes, nil, fb)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer mgr.Close()
	mgr.streamThreshold = 512

	rs := mgr.OpenReadStream("/video.mkv")
	defer rs.Close()

	ctx := context.Background()
	buf := make([]byte, 64)
	for _, off := range []int64{0, 512, 960, 128} {
		n, err := rs.ReadAt(ctx, buf, off)
		if err != nil || n != 64 || !bytes.Equal(buf, payload1[off:off+64]) {
			t.Fatalf("ReadAt(%d) = (%d, %v)", off, n, err)
		}
	}
	if fb.readCount.Load() != 1 {
		t.Fatalf("expected ReadStream to open backend reader once across 4 seeks, got %d", fb.readCount.Load())
	}

	// Remote ETag change must transparently re-open the stream reader with fresh content.
	fb.PutFile("/video.mkv", payload2, "etag-2")
	if n, err := rs.ReadAt(ctx, buf, 0); err != nil || n != 64 || !bytes.Equal(buf, payload2[:64]) {
		t.Fatalf("ReadAt after ETag change = (%d, %v)", n, err)
	}
	if fb.readCount.Load() != 2 {
		t.Fatalf("expected re-open after ETag change (readCount=2), got %d", fb.readCount.Load())
	}
}


