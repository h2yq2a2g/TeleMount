package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"telemount/internal/crypto"
	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/tg"
)

type mockTG struct {
	mu           sync.Mutex
	authorized   bool
	nextID       int
	docs         map[int]*mockDoc
	sourceRef    *mockTG // simulates Telegram cloud server-side copy from source channel
	tracker      *tg.Tracker
	fetches      int
	uploadCalls  int
	failUploadAt int // 1-based upload call to fail once
}

type mockDoc struct {
	info tg.ChannelDoc
	data []byte
}

func newMockTG() *mockTG {
	return &mockTG{
		authorized: true,
		nextID:     1,
		docs:       make(map[int]*mockDoc),
		tracker:    tg.NewTracker(),
	}
}

func (m *mockTG) IsAuthorized() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authorized
}
func (m *mockTG) Tracker() *tg.Tracker { return m.tracker }

func (m *mockTG) ForwardCopyDoc(_ context.Context, _, _ int64, srcMsgID int) (*tg.UploadedDoc, error) {
	if m.sourceRef == nil {
		return nil, io.EOF
	}
	m.sourceRef.mu.Lock()
	src, ok := m.sourceRef.docs[srcMsgID]
	var copiedData []byte
	var fileName, caption string
	if ok {
		copiedData = append([]byte(nil), src.data...)
		fileName = src.info.FileName
		caption = src.info.Caption
	}
	m.sourceRef.mu.Unlock()
	if !ok {
		return nil, io.EOF
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.docs[id] = &mockDoc{
		info: tg.ChannelDoc{
			MsgID:      id,
			FileName:   fileName,
			Caption:    caption,
			FileID:     int64(id * 900),
			AccessHash: int64(id * 999),
			FileRef:    []byte("backup-ref"),
			DCID:       4,
			Size:       int64(len(copiedData)),
		},
		data: copiedData,
	}
	return &tg.UploadedDoc{
		MsgID:      id,
		FileID:     int64(id * 900),
		AccessHash: int64(id * 999),
		FileRef:    []byte("backup-ref"),
		DCID:       4,
		Size:       int64(len(copiedData)),
	}, nil
}

func (m *mockTG) UploadBlob(_ context.Context, fileName, caption string, r io.Reader, _ int64, _ int, onProgress func(done int64)) (*tg.UploadedDoc, error) {
	m.mu.Lock()
	m.uploadCalls++
	shouldFail := m.failUploadAt > 0 && m.uploadCalls == m.failUploadAt
	if shouldFail {
		m.failUploadAt = 0
	}
	m.mu.Unlock()
	if shouldFail {
		return nil, errors.New("injected network drop during upload")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(int64(len(data)))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.docs[id] = &mockDoc{
		info: tg.ChannelDoc{
			MsgID:      id,
			FileName:   fileName,
			Caption:    caption,
			FileID:     int64(id * 100),
			AccessHash: int64(id * 200),
			FileRef:    []byte("ref"),
			DCID:       5,
			Size:       int64(len(data)),
		},
		data: data,
	}
	return &tg.UploadedDoc{
		MsgID:      id,
		FileID:     int64(id * 100),
		AccessHash: int64(id * 200),
		FileRef:    []byte("ref"),
		DCID:       5,
		Size:       int64(len(data)),
	}, nil
}

func (m *mockTG) FetchChunk(_ context.Context, loc tg.DocRef, offset int64, limit int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return append([]byte(nil), doc.data[offset:end]...), nil
}

func (m *mockTG) RefreshFileRef(_ context.Context, _ int) ([]byte, error) { return []byte("ref2"), nil }

func (m *mockTG) DeleteMessages(_ context.Context, msgIDs []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range msgIDs {
		delete(m.docs, id)
	}
	return nil
}

func (m *mockTG) ListChannelDocs(_ context.Context, prefix string, _ int) ([]tg.ChannelDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tg.ChannelDoc
	for _, d := range m.docs {
		if prefix == "" || strings.HasPrefix(d.info.FileName, prefix) {
			out = append(out, d.info)
		}
	}
	return out, nil
}

func (m *mockTG) DownloadSmallDoc(_ context.Context, doc tg.ChannelDoc) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.docs[doc.MsgID].data...), nil
}

func (m *mockTG) CheckMessages(_ context.Context, msgIDs []int) (map[int]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]int64, len(msgIDs))
	for _, id := range msgIDs {
		if d, ok := m.docs[id]; ok {
			out[id] = int64(len(d.data))
		}
	}
	return out, nil
}

func TestEngineMultiPartUploadSeekCacheAndBackupRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetAdmin("admin", "secret123")
	_ = st.SetChannel(1001, 2002, "Vault")
	salt, _ := crypto.NewSalt()
	mk := crypto.DeriveMasterKey("123456", salt)
	_ = st.SetMasterKey(mk, salt)

	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Set SmallFileMaxMB = 1 MiB here so the 1.5 MiB file exercises direct large-file streaming,
	// while the 130 KiB unknown-size stream exercises small-file RAM packing!
	s0, _ := store.GetSettings()
	s0.SmallFileMaxMB = 1
	_ = store.SaveSettings(s0)

	mock := newMockTG()
	eng := NewEngine(store, st, mock, dir)

	// Upload ~1.5 MiB known-size file (spans multiple 512 KiB TG chunks and 24 crypto blocks).
	payload := make([]byte, 1500*1024+777)
	if _, err := io.ReadFull(rand.Reader, payload); err != nil {
		t.Fatal(err)
	}

	node, err := eng.UploadFile(ctx, db.RootID, "video.mkv", "video/x-matroska", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("upload known-size: %v", err)
	}

	// Verify Telegram only saw random .bin ciphertext, never real filename or plaintext.
	for _, d := range mock.docs {
		if strings.Contains(d.info.FileName, "video") || bytes.Contains(d.data, payload[:256]) {
			t.Fatal("plaintext or filename leaked to Telegram!")
		}
	}

	// Seek to middle and read across 64KB and 512KB boundaries.
	fr, err := eng.OpenFile(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Close()

	seekOff := int64(600*1024 + 13)
	if _, err := fr.Seek(seekOff, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	gotSlice := make([]byte, 300*1024)
	n, err := io.ReadFull(fr, gotSlice)
	if err != nil || n != len(gotSlice) {
		t.Fatalf("range read failed: n=%d err=%v", n, err)
	}
	if !bytes.Equal(gotSlice, payload[seekOff:seekOff+int64(len(gotSlice))]) {
		t.Fatal("decrypted slice mismatch")
	}

	// Second read of the exact same range must hit RAM LRU cache (zero extra TG fetches).
	beforeFetches := mock.fetches
	gotSlice2 := make([]byte, len(gotSlice))
	if _, err := fr.ReadAt(gotSlice2, seekOff); err != nil {
		t.Fatal(err)
	}
	if mock.fetches != beforeFetches {
		t.Fatalf("expected RAM cache hit (0 fetches), got %d extra fetches", mock.fetches-beforeFetches)
	}
	if !bytes.Equal(gotSlice2, gotSlice) {
		t.Fatal("cached read mismatch")
	}

	// Live resize cache down to 1 MiB must evict immediately.
	eng.Cache().SetMaxBytes(1024 * 1024)
	if st := eng.Cache().Stats(); st.UsedBytes > 1024*1024 {
		t.Fatalf("cache did not evict to new max: used=%d", st.UsedBytes)
	}

	// Unknown-size stream upload (knownSize = -1) also works cleanly via tmpDir.
	streamData := bytes.Repeat([]byte("hello-stream-"), 10000)
	node2, err := eng.UploadFile(ctx, db.RootID, "stream.txt", "text/plain", bytes.NewReader(streamData), -1)
	if err != nil {
		t.Fatalf("upload unknown-size: %v", err)
	}
	fr2, _ := eng.OpenFile(ctx, node2)
	gotStream, err := io.ReadAll(fr2)
	fr2.Close()
	if err != nil || !bytes.Equal(gotStream, streamData) {
		t.Fatal("unknown-size upload round-trip mismatch")
	}

	// Backup DB to Telegram, wipe into fresh container, restore using passphrase "123456"!
	if _, err := eng.BackupDBToTelegram(ctx); err != nil {
		t.Fatalf("backup DB: %v", err)
	}

	dir2 := t.TempDir()
	st2, _ := state.Load(dir2)
	_ = st2.SetAdmin("newadmin", "newpass123")
	_ = st2.SetChannel(1001, 2002, "Vault")
	store2, _ := db.Open(filepath.Join(dir2, "telemount.db"))
	defer store2.Close()
	eng2 := NewEngine(store2, st2, mock, dir2)

	if err := eng2.RestoreDBFromTelegram(ctx, 0, "wrong-key"); err == nil {
		t.Fatal("expected restore with wrong passphrase to fail")
	}
	if err := eng2.RestoreDBFromTelegram(ctx, 0, "123456"); err != nil {
		t.Fatalf("restore with right passphrase failed: %v", err)
	}
	restoredNode, err := store2.StatByPath("/video.mkv")
	if err != nil || restoredNode.Size != int64(len(payload)) {
		t.Fatalf("restored node mismatch: %+v err=%v", restoredNode, err)
	}
	fr3, err := eng2.OpenFile(ctx, restoredNode)
	if err != nil {
		t.Fatal(err)
	}
	defer fr3.Close()
	fullRestored, err := io.ReadAll(fr3)
	if err != nil || !bytes.Equal(fullRestored, payload) {
		t.Fatal("decrypted file after fresh-machine DB restore mismatch!")
	}
}

func TestHybridPackCoalescingDeleteSafetyAndCompaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetAdmin("admin", "secret123")
	_ = st.SetChannel(1001, 2002, "Vault")
	salt, _ := crypto.NewSalt()
	_ = st.SetMasterKey(crypto.DeriveMasterKey("123456", salt), salt)

	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Set long idle timeout so we explicitly verify pre-flush RAM reading first!
	cfg, _ := store.GetSettings()
	cfg.SmallFileMaxMB = 128
	cfg.PackMaxMB = 1024
	cfg.PackIdleSec = 300
	if err := store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	mock := newMockTG()
	eng := NewEngine(store, st, mock, dir)

	// Upload 6 small files (~90 KiB each -> multi-block AES-GCM) — should stay in 1 RAM Pack (0 TG uploads yet!).
	filesData := make([][]byte, 6)
	nodes := make([]*db.Node, 6)
	for i := 0; i < 6; i++ {
		b := make([]byte, 90*1024+i*333)
		if _, err := io.ReadFull(rand.Reader, b); err != nil {
			t.Fatal(err)
		}
		filesData[i] = b
		n, err := eng.UploadFile(ctx, db.RootID, "small_"+string(rune('a'+i))+".bin", "application/octet-stream", bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatalf("upload small file %d: %v", i, err)
		}
		nodes[i] = n
	}

	mock.mu.Lock()
	preFlushDocs := len(mock.docs)
	mock.mu.Unlock()
	if preFlushDocs != 0 {
		t.Fatalf("expected 0 Telegram uploads before pack flush, got %d", preFlushDocs)
	}
	if stg := eng.Packer().Status(); stg.BufferedFiles != 6 {
		t.Fatalf("expected 6 buffered files in RAM pack, got %+v", stg)
	}

	// Read file #3 BEFORE flush — must read & decrypt directly from RAM buffer!
	rPre, err := eng.OpenFile(ctx, nodes[3])
	if err != nil {
		t.Fatal(err)
	}
	gotPre, err := io.ReadAll(rPre)
	rPre.Close()
	if err != nil || !bytes.Equal(gotPre, filesData[3]) {
		t.Fatal("pre-flush RAM pack read mismatch!")
	}

	// Flush RAM pack -> all 6 files must land in EXACTLY 1 Telegram message!
	if err := eng.Packer().FlushNow(ctx); err != nil {
		t.Fatalf("FlushNow failed: %v", err)
	}
	mock.mu.Lock()
	postFlushDocs := len(mock.docs)
	mock.mu.Unlock()
	if postFlushDocs != 1 {
		t.Fatalf("expected 6 small files to coalesce into 1 Telegram message, got %d", postFlushDocs)
	}

	// Read & mid-file seek on file #4 AFTER flush (exercises PackOffset + 64KB block seek + ChunkCache).
	rPost, err := eng.OpenFile(ctx, nodes[4])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rPost.Seek(70000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(rPost)
	rPost.Close()
	if err != nil || !bytes.Equal(tail, filesData[4][70000:]) {
		t.Fatal("post-flush seek read mismatch!")
	}

	// Permanently delete 4 of the 6 files (~66% dead space) -> shared Telegram pack MUST NOT be deleted yet!
	for _, i := range []int{0, 1, 2, 3} {
		if err := eng.DeletePermanent(ctx, nodes[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	_ = eng.FlushPendingDeletes(ctx)
	mock.mu.Lock()
	afterPartialDel := len(mock.docs)
	mock.mu.Unlock()
	if afterPartialDel != 1 {
		t.Fatalf("shared pack message was prematurely deleted while 2 files were still alive! docs=%d", afterPartialDel)
	}

	// Surviving file #5 must still read intact!
	rSurv, _ := eng.OpenFile(ctx, nodes[5])
	gotSurv, err := io.ReadAll(rSurv)
	rSurv.Close()
	if err != nil || !bytes.Equal(gotSurv, filesData[5]) {
		t.Fatal("surviving file corrupted after sibling delete!")
	}

	// Run Duplicati-style CompactPacks -> reclaims the 4 dead files into a new smaller Pack and deletes old Pack!
	rep, err := eng.Packer().CompactPacks(ctx)
	if err != nil {
		t.Fatalf("CompactPacks failed: %v", err)
	}
	if rep.CompactedPacks != 1 || rep.CreatedPacks != 1 || rep.RelocatedFiles != 2 || rep.ReclaimedBytes <= 0 {
		t.Fatalf("unexpected CompactReport: %+v", rep)
	}

	// Verify surviving files #4 and #5 still read 100% identically from the compacted Pack!
	for _, i := range []int{4, 5} {
		n, err := store.GetNode(nodes[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		rf, err := eng.OpenFile(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rf)
		rf.Close()
		if err != nil || !bytes.Equal(got, filesData[i]) {
			t.Fatalf("file %d corrupted after compaction!", i)
		}
	}

	// Finally delete the last 2 surviving files -> shared pack now has 0 references and is deleted automatically!
	for _, i := range []int{4, 5} {
		_ = eng.DeletePermanent(ctx, nodes[i].ID)
	}
	_ = eng.FlushPendingDeletes(ctx)
	mock.mu.Lock()
	finalDocs := len(mock.docs)
	mock.mu.Unlock()
	if finalDocs != 0 {
		t.Fatalf("expected 0 Telegram docs after all files in pack deleted, got %d", finalDocs)
	}
}

func TestMultiAccountMirrorFailoverAndDisasterRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := state.Load(dir)
	_ = st.SetAdmin("admin", "secret123")
	_ = st.SetChannel(1001, 2002, "PrimaryVault")
	salt, _ := crypto.NewSalt()
	_ = st.SetMasterKey(crypto.DeriveMasterKey("123456", salt), salt)

	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	primaryTG := newMockTG()
	primaryTG.nextID = 50 // distinct msg IDs from backup channel (starts at 1) to verify ID remapping!
	eng := NewEngine(store, st, primaryTG, dir)

	// 1. Upload 2 files BEFORE any backup account exists.
	dataA := bytes.Repeat([]byte("alpha-data-block-"), 5000)
	dataB := bytes.Repeat([]byte("bravo-data-block-"), 6000)
	nodeA, err := eng.UploadFile(ctx, db.RootID, "a.bin", "application/octet-stream", bytes.NewReader(dataA), int64(len(dataA)))
	if err != nil {
		t.Fatal(err)
	}
	nodeB, err := eng.UploadFile(ctx, db.RootID, "b.bin", "application/octet-stream", bytes.NewReader(dataB), int64(len(dataB)))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Attach Backup Account #1 with its own isolated channel (9009) and run retroactive SyncAllReplicas!
	backupTG := newMockTG()
	backupTG.sourceRef = primaryTG
	eng.Pool().SetTestBackend("acc-bak1", backupTG)
	_ = st.UpsertBackupAccount(state.BackupAccountConfig{
		ID:                      "acc-bak1",
		OwnChannelID:            9009,
		OwnChannelAccessHash:    8888,
		OwnChannelTitle:         "BackupVault",
		SourceChannelID:         1001,
		SourceChannelAccessHash: 7777,
	})

	rep, err := eng.SyncAllReplicas(ctx, "")
	if err != nil || rep.SyncedBlobs != 1 {
		t.Fatalf("expected 1 pack blob synced to backup account, got %+v err=%v", rep, err)
	}

	// Wait briefly for the async .tmdb DB backup mirror triggered inside SyncAllReplicas->BackupDBToTelegram.
	_ = eng.MirrorMsg(ctx, 51, true)

	// 3. CATASTROPHE: Primary account is banned AND Primary Channel is completely wiped!
	primaryTG.mu.Lock()
	primaryTG.authorized = false
	primaryTG.docs = make(map[int]*mockDoc)
	primaryTG.mu.Unlock()
	eng.Cache().Clear()

	// Automatic Read Failover must seamlessly serve both files from Backup Account #1!
	frA, err := eng.OpenFile(ctx, nodeA)
	if err != nil {
		t.Fatalf("OpenFile during failover: %v", err)
	}
	gotA, err := io.ReadAll(frA)
	frA.Close()
	if err != nil || !bytes.Equal(gotA, dataA) {
		t.Fatalf("failover read A failed: err=%v match=%v", err, bytes.Equal(gotA, dataA))
	}

	// 4. TOTAL APOCALYPSE: Fresh VPS + restore .tmdb directly from Backup Account #1's channel (9009)!
	dir2 := t.TempDir()
	st2, _ := state.Load(dir2)
	_ = st2.SetAdmin("admin2", "pass1234")
	_ = st2.SetChannel(9009, 8888, "BackupVault") // logged into Backup Account #1 and selected BackupVault!
	store2, _ := db.Open(filepath.Join(dir2, "telemount.db"))
	defer store2.Close()
	eng2 := NewEngine(store2, st2, backupTG, dir2)

	if err := eng2.RestoreDBFromTelegram(ctx, 0, "123456"); err != nil {
		t.Fatalf("fresh VPS restore from Backup Channel failed: %v", err)
	}

	restoredB, err := store2.StatByPath("/b.bin")
	if err != nil {
		t.Fatalf("StatByPath /b.bin after backup-channel restore: %v", err)
	}
	frB, err := eng2.OpenFile(ctx, restoredB)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := io.ReadAll(frB)
	frB.Close()
	if err != nil || !bytes.Equal(gotB, dataB) {
		t.Fatalf("decrypted file B after fresh VPS restore from Backup Channel mismatch: id=%d err=%v", nodeB.ID, err)
	}
}

func TestFileReaderSequentialReadAheadAndCleanClose(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	salt, _ := crypto.NewSalt()
	key := crypto.DeriveMasterKey("secret-pass", salt)
	_ = st.SetAdmin("admin", "pass1234")
	_ = st.SetMasterKey(key, salt)
	_ = st.SetChannel(1001, 2002, "StreamChan")
	mock := newMockTG()
	eng := NewEngine(store, st, mock, filepath.Join(dir, "tmp"))

	// 4 MiB file = ~8 aligned 512 KiB Telegram chunks.
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte((i * 29) % 253)
	}
	ctx := context.Background()
	node, err := eng.UploadFile(ctx, db.RootID, "movie.mkv", "video/x-matroska", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if err := eng.Packer().FlushNow(ctx); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	fr, err := eng.OpenFile(ctx, node)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// Read first 1.5 MiB sequentially (chunks 0, 1, 2) -> triggers 4-chunk async read-ahead (chunks 3..6).
	buf := make([]byte, 1536*1024)
	if n, err := io.ReadFull(fr, buf); err != nil || n != len(buf) || !bytes.Equal(buf, payload[:len(buf)]) {
		_ = fr.Close()
		t.Fatalf("sequential read failed: n=%d err=%v", n, err)
	}

	// Wait briefly for read-ahead goroutines to populate ChunkCache beyond the 3 chunks read.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.Cache().Stats().Entries >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entries := eng.Cache().Stats().Entries; entries < 5 {
		_ = fr.Close()
		t.Fatalf("expected sequential read-ahead to populate >= 5 cached chunks, got %d", entries)
	}
	if err := fr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDBBackupRetainsRollingBackups(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	salt, _ := crypto.NewSalt()
	key := crypto.DeriveMasterKey("secret-pass", salt)
	_ = st.SetAdmin("admin", "pass1234")
	_ = st.SetMasterKey(key, salt)
	_ = st.SetChannel(1001, 2002, "RollingBackupChan")
	mock := newMockTG()
	eng := NewEngine(store, st, mock, filepath.Join(dir, "tmp"))
	defer eng.Close()

	ctx := context.Background()
	var uploadedIDs []int
	for i := 0; i < DBBackupKeepCount+2; i++ {
		d, err := eng.BackupDBToTelegram(ctx)
		if err != nil {
			t.Fatalf("BackupDBToTelegram #%d: %v", i+1, err)
		}
		uploadedIDs = append(uploadedIDs, d.MsgID)
	}

	backups, err := eng.ListChannelBackups(ctx)
	if err != nil {
		t.Fatalf("ListChannelBackups: %v", err)
	}
	if len(backups) != DBBackupKeepCount {
		t.Fatalf("expected %d rolling DB backups, got %d (%+v)", DBBackupKeepCount, len(backups), backups)
	}
	wantLatest := uploadedIDs[len(uploadedIDs)-1]
	if backups[0].MsgID != wantLatest {
		t.Fatalf("expected newest backup msg #%d first, got #%d", wantLatest, backups[0].MsgID)
	}
}

func TestBackupDBFailClosedWhenPackFlushFails(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	st, _ := state.Load(dir)
	salt, _ := crypto.NewSalt()
	_ = st.SetAdmin("admin", "pass1234")
	_ = st.SetMasterKey(crypto.DeriveMasterKey("secret-pass", salt), salt)
	_ = st.SetChannel(1001, 2002, "FailClosedChan")

	cfg, _ := store.GetSettings()
	cfg.PackIdleSec = 300
	_ = store.SaveSettings(cfg)

	mock := newMockTG()
	eng := NewEngine(store, st, mock, filepath.Join(dir, "tmp"))
	defer eng.Close()

	ctx := context.Background()
	data := bytes.Repeat([]byte("staged-small-"), 512)
	if _, err := eng.UploadFile(ctx, db.RootID, "staged.txt", "text/plain", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	// Next upload (pack flush inside BackupDBToTelegram) fails -> BackupDBToTelegram MUST fail closed!
	mock.mu.Lock()
	mock.failUploadAt = mock.uploadCalls + 1
	mock.mu.Unlock()

	if _, err := eng.BackupDBToTelegram(ctx); err == nil {
		t.Fatal("expected BackupDBToTelegram to fail closed when staged pack flush fails")
	}
	backups, err := eng.ListChannelBackups(ctx)
	if err != nil {
		t.Fatalf("ListChannelBackups: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("expected 0 DB backups uploaded after pack flush failure, got %d", len(backups))
	}
}

func TestVerifyVaultAndSyncContinuesPastMissingMessage(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	st, _ := state.Load(dir)
	salt, _ := crypto.NewSalt()
	_ = st.SetAdmin("admin", "pass1234")
	_ = st.SetMasterKey(crypto.DeriveMasterKey("secret-pass", salt), salt)
	_ = st.SetChannel(1001, 2002, "PrimaryChan")

	cfg, _ := store.GetSettings()
	cfg.SmallFileMaxMB = 0 // upload each file as its own message
	_ = store.SaveSettings(cfg)

	primaryTG := newMockTG()
	eng := NewEngine(store, st, primaryTG, filepath.Join(dir, "tmp"))
	defer eng.Close()

	ctx := context.Background()
	n1, err := eng.UploadFile(ctx, db.RootID, "f1.bin", "application/octet-stream", bytes.NewReader([]byte("file-one-content")), 16)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := eng.UploadFile(ctx, db.RootID, "f2.bin", "application/octet-stream", bytes.NewReader([]byte("file-two-content")), 16)
	if err != nil {
		t.Fatal(err)
	}

	// Healthy vault scrub.
	vrep, err := eng.VerifyVault(ctx)
	if err != nil {
		t.Fatalf("VerifyVault: %v", err)
	}
	if !vrep.SQLiteOK || vrep.CheckedBlobs != 2 || vrep.HealthyBlobs != 2 || len(vrep.MissingOnPrimary) != 0 {
		t.Fatalf("unexpected healthy VerifyReport: %+v", vrep)
	}

	// Simulate message 1 vanishing on Primary before SyncAllReplicas runs.
	p1, _ := store.GetFileParts(n1.ID)
	p2, _ := store.GetFileParts(n2.ID)
	_ = primaryTG.DeleteMessages(ctx, []int{p1[0].TGMsgID})

	backupTG := newMockTG()
	backupTG.sourceRef = primaryTG
	eng.Pool().SetTestBackend("acc-bak1", backupTG)
	_ = st.UpsertBackupAccount(state.BackupAccountConfig{
		ID:                      "acc-bak1",
		OwnChannelID:            9009,
		OwnChannelAccessHash:    8888,
		OwnChannelTitle:         "BackupChan",
		SourceChannelID:         1001,
		SourceChannelAccessHash: 7777,
	})

	srep, err := eng.SyncAllReplicas(ctx, "acc-bak1")
	if err != nil {
		t.Fatalf("SyncAllReplicas: %v", err)
	}
	if srep.SyncedBlobs != 1 || len(srep.Errors) == 0 {
		t.Fatalf("expected SyncAllReplicas to skip missing msg #%d and still replicate msg #%d, got %+v", p1[0].TGMsgID, p2[0].TGMsgID, srep)
	}

	// Delete message 2 on primary as well -> VerifyVault must report 2 missing, with msg 2 marked recoverable!
	_ = primaryTG.DeleteMessages(ctx, []int{p2[0].TGMsgID})
	vrep2, err := eng.VerifyVault(ctx)
	if err != nil {
		t.Fatalf("VerifyVault after loss: %v", err)
	}
	if len(vrep2.MissingOnPrimary) != 2 || len(vrep2.RecoverableBlobs) != 1 || vrep2.RecoverableBlobs[0] != p2[0].TGMsgID {
		t.Fatalf("unexpected degraded VerifyReport: %+v", vrep2)
	}
}

func TestChunkCacheGetOrFetchCancellationIsolation(t *testing.T) {
	t.Parallel()
	cache := NewChunkCache(1024 * 1024)
	release := make(chan struct{})
	started := make(chan struct{})
	var calls int
	var mu sync.Mutex

	fetch := func() ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release
		return []byte("chunk-bytes"), nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	errChA := make(chan error, 1)
	go func() {
		_, err := cache.GetOrFetch(ctxA, 7, 0, fetch)
		errChA <- err
	}()
	<-started

	resChB := make(chan []byte, 1)
	errChB := make(chan error, 1)
	go func() {
		b, err := cache.GetOrFetch(context.Background(), 7, 0, fetch)
		resChB <- b
		errChB <- err
	}()
	time.Sleep(15 * time.Millisecond)

	// Cancelling waiter A must unblock A immediately without poisoning waiter B.
	cancelA()
	if err := <-errChA; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter A err = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-errChB; err != nil {
		t.Fatalf("waiter B err = %v, want nil", err)
	}
	if got := string(<-resChB); got != "chunk-bytes" {
		t.Fatalf("waiter B data = %q, want %q", got, "chunk-bytes")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected 1 singleflight fetch, got %d", calls)
	}
}

func TestResumableUploadKeyDetectsMiddleChange(t *testing.T) {
	t.Parallel()
	a := bytes.Repeat([]byte("A"), 512*1024)
	b := append([]byte(nil), a...)
	b[len(b)/2] = 'Z' // same head, tail, and length; middle differs

	k1, ok1 := computeResumableUploadKey(1, "disk.img", int64(len(a)), 10<<20, bytes.NewReader(a))
	k2, ok2 := computeResumableUploadKey(1, "disk.img", int64(len(b)), 10<<20, bytes.NewReader(b))
	if !ok1 || !ok2 {
		t.Fatalf("expected keys computed: ok1=%v ok2=%v", ok1, ok2)
	}
	if k1 == k2 {
		t.Fatal("expected upload key to differ when middle of file changes")
	}
}

func TestMultiPartUploadCheckpointResumeAndCrossPartReadAhead(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	salt, _ := crypto.NewSalt()
	key := crypto.DeriveMasterKey("secret-pass", salt)
	_ = st.SetAdmin("admin", "pass1234")
	_ = st.SetMasterKey(key, salt)
	_ = st.SetChannel(1001, 2002, "ResumeChan")

	cfg, _ := store.GetSettings()
	cfg.SmallFileMaxMB = 0
	cfg.PartSizeMB = 10
	if err := store.SaveSettings(cfg); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	mock := newMockTG()
	mock.failUploadAt = 2 // fail on part #2 of 2!
	eng := NewEngine(store, st, mock, filepath.Join(dir, "tmp"))

	// 11 MiB file -> Part 0 (10 MiB) + Part 1 (1 MiB).
	payload := make([]byte, 11<<20)
	for i := range payload {
		payload[i] = byte((i*31 + i/1021) % 251)
	}
	ctx := context.Background()

	// Attempt 1: Part 0 uploads & checkpoints; Part 1 fails -> Part 0 must NOT be deleted!
	if _, err := eng.UploadFile(ctx, db.RootID, "big.bin", "application/octet-stream", bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Fatal("expected first UploadFile attempt to fail on part #2")
	}
	mock.mu.Lock()
	docsAfterFail := len(mock.docs)
	callsAfterFail := mock.uploadCalls
	mock.mu.Unlock()
	if docsAfterFail != 1 || callsAfterFail != 2 {
		t.Fatalf("expected part 0 preserved in TG (docs=1, calls=2), got docs=%d calls=%d", docsAfterFail, callsAfterFail)
	}
	refs, err := store.AllReferencedMsgIDs()
	if err != nil || len(refs) != 1 {
		t.Fatalf("expected checkpointed part protected in AllReferencedMsgIDs, got %v (err=%v)", refs, err)
	}

	// Attempt 2: resumes from checkpoint and uploads ONLY Part 1!
	node, err := eng.UploadFile(ctx, db.RootID, "big.bin", "application/octet-stream", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("resumed UploadFile: %v", err)
	}
	mock.mu.Lock()
	totalCalls := mock.uploadCalls
	totalDocs := len(mock.docs)
	mock.mu.Unlock()
	if totalCalls != 3 || totalDocs != 2 {
		t.Fatalf("expected resume to upload only part 1 (totalCalls=3, totalDocs=2), got calls=%d docs=%d", totalCalls, totalDocs)
	}

	// Verify decrypted content intact and sequential read across the 10 MiB part boundary triggers cross-part read-ahead.
	fr, err := eng.OpenFile(ctx, node)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fr.Close()

	// Read sequentially starting 1.5 MiB before the 10 MiB boundary up to 10 MiB (crossing chunks 17, 18, 19 of part 0).
	startOff := int64(10<<20) - int64(1536*1024)
	buf := make([]byte, 1536*1024)
	if n, err := fr.ReadAt(buf, startOff); err != nil || n != len(buf) || !bytes.Equal(buf, payload[startOff:startOff+int64(len(buf))]) {
		t.Fatalf("pre-boundary read = (%d, %v)", n, err)
	}
	// Part 1 chunk 0 should be prefetched asynchronously across the part boundary!
	parts, _ := store.GetFileParts(node.ID)
	if len(parts) != 2 {
		t.Fatalf("expected 2 file parts, got %d", len(parts))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.Cache().HasOrInflight(parts[1].TGMsgID, 0) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !eng.Cache().HasOrInflight(parts[1].TGMsgID, 0) {
		t.Fatal("expected cross-part read-ahead to prefetch part 1 chunk 0")
	}

	// Verify tail bytes in part 1 decrypt cleanly too.
	tail := make([]byte, 4096)
	tailOff := int64(len(payload) - 4096)
	if n, err := fr.ReadAt(tail, tailOff); err != nil || n != len(tail) || !bytes.Equal(tail, payload[tailOff:]) {
		t.Fatalf("part 1 tail read = (%d, %v)", n, err)
	}
}


