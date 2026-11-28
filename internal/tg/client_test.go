package tg

import (
	"testing"
	"time"

	mt "github.com/gotd/td/tg"
)

func TestIsValidAccountIDAndPool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"generated id", "acc-1a2b3c4d", true},
		{"alnum underscore", "backup_01", true},
		{"empty", "", false},
		{"path traversal", "../etc", false},
		{"slash", "a/b", false},
		{"dot", "a.b", false},
		{"too long", "abcdefghijklmnopqrstuvwxyz0123456", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsValidAccountID(tc.id); got != tc.want {
				t.Fatalf("IsValidAccountID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}

	pool := NewPool(t.TempDir())
	if m := pool.GetOrCreateManager("../bad"); m != nil {
		t.Fatal("expected nil manager for invalid account id")
	}
	m1 := pool.GetOrCreateManager("acc-good1")
	m2 := pool.GetOrCreateManager("acc-good1")
	if m1 == nil || m1 != m2 {
		t.Fatal("expected Pool to cache and reuse manager for valid id")
	}
	pool.StopAll()
}

func TestParseInviteHash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plus link", "https://t.me/+AbCdEf123/", "AbCdEf123"},
		{"joinchat link", "https://t.me/joinchat/XYZ789", "XYZ789"},
		{"raw hash", "  rawHash  ", "rawHash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseInviteHash(tc.in); got != tc.want {
				t.Fatalf("parseInviteHash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractBatchUploadedDocsAndFileName(t *testing.T) {
	t.Parallel()
	media1 := &mt.MessageMediaDocument{
		Document: &mt.Document{
			ID:            1001,
			AccessHash:    2002,
			FileReference: []byte("ref1"),
			DCID:          4,
			Size:          512,
			Attributes:    []mt.DocumentAttributeClass{&mt.DocumentAttributeFilename{FileName: "blob1.bin"}},
		},
	}
	media2 := &mt.MessageMediaDocument{
		Document: &mt.Document{
			ID:            1002,
			AccessHash:    2003,
			FileReference: []byte("ref2"),
			DCID:          5,
			Size:          1024,
			Attributes:    []mt.DocumentAttributeClass{&mt.DocumentAttributeFilename{FileName: "blob2.bin"}},
		},
	}
	if fn := extractFileName(media1); fn != "blob1.bin" {
		t.Fatalf("extractFileName = %q, want blob1.bin", fn)
	}

	upds := &mt.Updates{
		Updates: []mt.UpdateClass{
			&mt.UpdateMessageID{ID: 501, RandomID: 11},
			&mt.UpdateMessageID{ID: 502, RandomID: 22},
			&mt.UpdateNewChannelMessage{Message: &mt.Message{ID: 501, Media: media1}},
			&mt.UpdateNewChannelMessage{Message: &mt.Message{ID: 502, Media: media2}},
		},
	}
	got := extractBatchUploadedDocs(upds, map[int64]int{11: 10, 22: 20})
	if len(got) != 2 || got[10].MsgID != 501 || got[10].FileID != 1001 || got[20].MsgID != 502 || got[20].Size != 1024 {
		t.Fatalf("unexpected batch docs: %+v", got)
	}
}

func TestTrackerSnapshotAndFloodRing(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.SetConnectionState("ready", true)
	tr.SetAuthorized(true, &UserInfo{ID: 42, Username: "alice"})

	id := tr.StartTransfer("upload", "file.bin", 1000)
	tr.AddTransferBytes(id, 400)
	for i := 0; i < 25; i++ {
		tr.RecordFloodWait("upload.saveBigFilePart", 2*time.Second)
	}
	snap := tr.Snapshot()
	if !snap.Connected || !snap.Authorized || snap.User == nil || snap.User.ID != 42 {
		t.Fatalf("unexpected tracker snapshot: %+v", snap)
	}
	if len(snap.ActiveTransfers) != 1 || snap.ActiveTransfers[0].Done != 400 {
		t.Fatalf("unexpected transfers: %+v", snap.ActiveTransfers)
	}
	if len(snap.RecentFloods) != 20 {
		t.Fatalf("expected recent floods capped at 20, got %d", len(snap.RecentFloods))
	}
	tr.EndTransfer(id)
	if len(tr.Snapshot().ActiveTransfers) != 0 {
		t.Fatal("expected transfer cleared after EndTransfer")
	}
}
