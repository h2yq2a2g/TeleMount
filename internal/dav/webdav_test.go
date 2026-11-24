package dav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"telemount/internal/crypto"
	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
)

type memTG struct {
	mu      sync.Mutex
	next    int
	blobs   map[int][]byte
	tracker *tg.Tracker
}

func (m *memTG) IsAuthorized() bool   { return true }
func (m *memTG) Tracker() *tg.Tracker { return m.tracker }
func (m *memTG) UploadBlob(_ context.Context, _, _ string, r io.Reader, _ int64, _ int, _ func(int64)) (*tg.UploadedDoc, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := m.next
	m.blobs[id] = b
	return &tg.UploadedDoc{MsgID: id, FileID: int64(id), AccessHash: int64(id), DCID: 1, Size: int64(len(b))}, nil
}
func (m *memTG) FetchChunk(_ context.Context, loc tg.DocRef, off int64, limit int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.blobs[loc.MsgID]
	if off >= int64(len(b)) {
		return nil, nil
	}
	end := off + int64(limit)
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return append([]byte(nil), b[off:end]...), nil
}
func (m *memTG) RefreshFileRef(_ context.Context, _ int) ([]byte, error)            { return nil, nil }
func (m *memTG) ForwardCopyDoc(_ context.Context, _, _ int64, _ int) (*tg.UploadedDoc, error) {
	return nil, nil
}
func (m *memTG) DeleteMessages(_ context.Context, ids []int) error                  { m.mu.Lock(); defer m.mu.Unlock(); for _, id := range ids { delete(m.blobs, id) }; return nil }
func (m *memTG) ListChannelDocs(_ context.Context, _ string, _ int) ([]tg.ChannelDoc, error) { return nil, nil }
func (m *memTG) DownloadSmallDoc(_ context.Context, _ tg.ChannelDoc) ([]byte, error) { return nil, nil }

func TestWebDAVLifecycleAuthRangeAndPermanentDelete(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Load(dir)
	_ = st.SetAdmin("admin", "adminpass")
	_ = st.SetChannel(1, 2, "Vault")
	salt, _ := crypto.NewSalt()
	_ = st.SetMasterKey(crypto.DeriveMasterKey("123456", salt), salt)

	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.SetWebDAVCredentials("rclone", "davpass123")

	mock := &memTG{blobs: make(map[int][]byte), tracker: tg.NewTracker()}
	eng := storage.NewEngine(store, st, mock, dir)
	h := NewHandler("/dav", eng)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Unauthenticated request must fail with 401.
	resp, err := http.Get(ts.URL + "/dav/")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Admin web password must NOT authenticate WebDAV (credentials are strictly separate).
	req, _ := http.NewRequest("OPTIONS", ts.URL+"/dav/", nil)
	req.SetBasicAuth("admin", "adminpass")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for admin password on WebDAV, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	doReq := func(method, urlPath string, body []byte, hdrs map[string]string) *http.Response {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+urlPath, rdr)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("rclone", "davpass123")
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// MKCOL /dav/media
	r := doReq("MKCOL", "/dav/media", nil, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL status %d", r.StatusCode)
	}

	// PROPFIND /dav/ (Depth: 1) must advertise /dav/media/ as a DAV collection (directory), never a plain file.
	r = doReq("PROPFIND", "/dav/", nil, map[string]string{"Depth": "1"})
	rootXML, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusMultiStatus || !strings.Contains(string(rootXML), "/dav/media/") || !strings.Contains(string(rootXML), "collection") {
		t.Fatalf("root PROPFIND did not mark folder as collection: status=%d xml=%s", r.StatusCode, string(rootXML))
	}

	// PROPPATCH on folder /dav/media must not fail with 500/ErrExist.
	proppatchXML := []byte(`<?xml version="1.0" encoding="utf-8" ?><D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><Z:Win32LastModifiedTime xmlns:Z="urn:schemas-microsoft-com:">Mon, 23 Nov 2026 15:00:00 GMT</Z:Win32LastModifiedTime></D:prop></D:set></D:propertyupdate>`)
	r = doReq("PROPPATCH", "/dav/media", proppatchXML, map[string]string{"Content-Type": "application/xml"})
	r.Body.Close()
	if r.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH on folder failed: status=%d", r.StatusCode)
	}

	// PUT /dav/media/sample.bin (100 KiB)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 6400)
	r = doReq("PUT", "/dav/media/sample.bin", payload, map[string]string{"Content-Type": "application/octet-stream"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || r.Header.Get("ETag") == "" {
		t.Fatalf("PUT failed: status=%d etag=%q", r.StatusCode, r.Header.Get("ETag"))
	}

	// PROPFIND /dav/media
	r = doReq("PROPFIND", "/dav/media/", nil, map[string]string{"Depth": "1"})
	xmlBody, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusMultiStatus || !strings.Contains(string(xmlBody), "sample.bin") {
		t.Fatalf("PROPFIND failed: status=%d body=%s", r.StatusCode, string(xmlBody))
	}

	// GET with Range header (bytes=1000-1999)
	r = doReq("GET", "/dav/media/sample.bin", nil, map[string]string{"Range": "bytes=1000-1999"})
	part, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusPartialContent || !bytes.Equal(part, payload[1000:2000]) {
		t.Fatalf("Range GET failed: status=%d len=%d", r.StatusCode, len(part))
	}

	// MOVE /dav/media/sample.bin -> /dav/media/moved.bin
	r = doReq("MOVE", "/dav/media/sample.bin", nil, map[string]string{"Destination": ts.URL + "/dav/media/moved.bin"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated && r.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE failed: status=%d", r.StatusCode)
	}

	// DELETE /dav/media/moved.bin must permanently delete (NOT go to Trash!)
	r = doReq("DELETE", "/dav/media/moved.bin", nil, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE failed: status=%d", r.StatusCode)
	}
	trash, _ := store.ListTrash()
	if len(trash) != 0 {
		t.Fatalf("WebDAV DELETE should bypass Trash, found %d items in trash", len(trash))
	}
}
