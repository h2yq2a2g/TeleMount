package db

import (
	"path/filepath"
	"testing"
)

func TestStoreCRUDTrashSearchAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	docs, err := store.MkdirAll("/docs/2026")
	if err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 32)
	copy(hdr, "TMP1")

	file, replaced, err := store.UpsertFileWithParts(docs.ID, "report.pdf", 1234, "application/pdf", []FilePart{
		{PartIndex: 0, TGMsgID: 101, TGFileID: 555, TGAccessHash: 777, TGDCID: 5, PlainSize: 1234, CipherSize: 1282, PartHeader: hdr},
	})
	if err != nil || len(replaced) != 0 {
		t.Fatalf("upsert failed: %v replaced=%v", err, replaced)
	}

	// Overwriting same filename returns old message ID and records it in pending_deletes.
	_, replaced, err = store.UpsertFileWithParts(docs.ID, "report.pdf", 2048, "application/pdf", []FilePart{
		{PartIndex: 0, TGMsgID: 102, TGFileID: 556, TGAccessHash: 778, TGDCID: 5, PlainSize: 2048, CipherSize: 2096, PartHeader: hdr},
	})
	if err != nil || len(replaced) != 1 || replaced[0] != 101 {
		t.Fatalf("expected replaced [101], got %v err=%v", replaced, err)
	}

	// Search finds active file.
	found, err := store.Search("report", 10)
	if err != nil || len(found) != 1 || found[0].Path != "/docs/2026/report.pdf" {
		t.Fatalf("search unexpected: %+v err=%v", found, err)
	}

	// Soft-delete hides file from StatByPath & Search, appears in Trash.
	if err := store.SoftDelete(file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StatByPath("/docs/2026/report.pdf"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after soft delete, got %v", err)
	}
	found, _ = store.Search("report", 10)
	if len(found) != 0 {
		t.Fatalf("expected 0 search hits for trashed file, got %d", len(found))
	}
	trash, _ := store.ListTrash()
	if len(trash) != 1 {
		t.Fatalf("expected 1 trashed item, got %d", len(trash))
	}

	// Restore brings it back.
	if err := store.RestoreFromTrash(file.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.StatByPath("/docs/2026/report.pdf"); err != nil || n.Size != 2048 {
		t.Fatalf("expected restored file size 2048, got %+v err=%v", n, err)
	}

	// Snapshot + restore into a fresh store preserves data cleanly.
	snap, err := store.SnapshotBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	store2, err := Open(filepath.Join(dir, "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if err := store2.RestoreFromBytes(snap, dir); err != nil {
		t.Fatal(err)
	}
	if n, err := store2.StatByPath("/docs/2026/report.pdf"); err != nil || n.Size != 2048 {
		t.Fatalf("restored store missing file: %+v err=%v", n, err)
	}

	// PermanentDelete removes node and returns Telegram message IDs.
	mids, err := store2.PermanentDelete(docs.ID)
	if err != nil || len(mids) != 1 || mids[0] != 102 {
		t.Fatalf("expected permanent delete msg [102], got %v err=%v", mids, err)
	}
}

func TestRestoreFailureKeepsStoreUsableQuickCheckMtimeStatsAndPromote(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.QuickCheck(); err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}

	hdr := make([]byte, 32)
	copy(hdr, "TMP1")
	n1, _, err := store.UpsertFileWithParts(RootID, "a.bin", 512, "application/octet-stream", []FilePart{
		{PartIndex: 0, TGMsgID: 10, TGFileID: 100, TGAccessHash: 200, TGDCID: 4, PlainSize: 512, CipherSize: 560, PartHeader: hdr},
	})
	if err != nil {
		t.Fatal(err)
	}

	// SetNodeMtime persists custom mtime into SQLite.
	const wantMtime int64 = 1700000123
	if err := store.SetNodeMtime(n1.ID, wantMtime); err != nil {
		t.Fatalf("SetNodeMtime: %v", err)
	}
	gotNode, err := store.GetNode(n1.ID)
	if err != nil || gotNode.UpdatedAt != wantMtime {
		t.Fatalf("expected updated_at=%d, got %+v err=%v", wantMtime, gotNode, err)
	}

	// Stats cache returns consistent values and invalidates on write.
	st1, err := store.Stats()
	if err != nil || st1.FileCount != 1 {
		t.Fatalf("Stats #1: %+v err=%v", st1, err)
	}
	st2, err := store.Stats()
	if err != nil || st2.FileCount != 1 {
		t.Fatalf("Stats #2 (cached): %+v err=%v", st2, err)
	}
	_, _, err = store.UpsertFileWithParts(RootID, "b.bin", 256, "application/octet-stream", []FilePart{
		{PartIndex: 0, TGMsgID: 20, TGFileID: 101, TGAccessHash: 201, TGDCID: 4, PlainSize: 256, CipherSize: 304, PartHeader: hdr},
	})
	if err != nil {
		t.Fatal(err)
	}
	st3, err := store.Stats()
	if err != nil || st3.FileCount != 2 {
		t.Fatalf("Stats #3 after mutation: %+v err=%v", st3, err)
	}

	// ListReferencedBlobs + CountUnreplicatedMsgIDs + PromoteAccountReplicasToPrimary with colliding msg IDs (10->20, 20->10).
	blobs, err := store.ListReferencedBlobs()
	if err != nil || len(blobs) != 2 {
		t.Fatalf("ListReferencedBlobs: %+v err=%v", blobs, err)
	}
	unrep, err := store.CountUnreplicatedMsgIDs("acc-1")
	if err != nil || unrep != 2 {
		t.Fatalf("CountUnreplicatedMsgIDs = (%d, %v), want 2", unrep, err)
	}
	_ = store.UpsertBlobReplica(BlobReplica{AccountID: "acc-1", ChannelID: 99, PrimaryMsgID: 10, ReplicaMsgID: 20, TGFileID: 901, TGAccessHash: 902, TGDCID: 4})
	_ = store.UpsertBlobReplica(BlobReplica{AccountID: "acc-1", ChannelID: 99, PrimaryMsgID: 20, ReplicaMsgID: 10, TGFileID: 903, TGAccessHash: 904, TGDCID: 4})
	unrep, err = store.CountUnreplicatedMsgIDs("acc-1")
	if err != nil || unrep != 0 {
		t.Fatalf("CountUnreplicatedMsgIDs after full sync = (%d, %v), want 0", unrep, err)
	}
	remapped, err := store.PromoteAccountReplicasToPrimary("acc-1")
	if err != nil || remapped != 2 {
		t.Fatalf("PromoteAccountReplicasToPrimary = (%d, %v), want 2", remapped, err)
	}
	partsA, _ := store.GetFileParts(n1.ID)
	if len(partsA) != 1 || partsA[0].TGMsgID != 20 || partsA[0].TGFileID != 901 {
		t.Fatalf("expected swapped part for a.bin (msg 20, file 901), got %+v", partsA)
	}

	// Corrupt restore candidate MUST fail AND leave existing store connection valid (no nil conn panic).
	corrupt := append([]byte("SQLite format 3\x00"), make([]byte, 200)...)
	if err := store.RestoreFromBytes(corrupt, dir); err == nil {
		t.Fatal("expected corrupt RestoreFromBytes to fail")
	}
	if n, err := store.StatByPath("/a.bin"); err != nil || n.Size != 512 {
		t.Fatalf("store broken after failed restore: %+v err=%v", n, err)
	}
}
