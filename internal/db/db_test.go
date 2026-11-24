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
