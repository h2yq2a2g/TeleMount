package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
)

func TestSetupAdminAuthGuardsAndKeyInit(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tgMgr := tg.NewManager(dir)
	eng := storage.NewEngine(store, st, tgMgr, dir)
	srv := NewServer(eng, tgMgr)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Embedded Web UI index is served at /.
	resp, err := http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / failed: status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Protected API must reject before admin setup.
	resp, _ = http.Get(ts.URL + "/api/settings")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before admin setup, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// First-boot admin creation succeeds and sets session cookie.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err = client.Post(ts.URL+"/api/setup/admin", "application/json",
		strings.NewReader(`{"username":"admin","password":"webpassword123"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup admin failed: status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Second call to /api/setup/admin must be permanently blocked (403).
	resp, _ = client.Post(ts.URL+"/api/setup/admin", "application/json",
		strings.NewReader(`{"username":"hacker","password":"password123"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on duplicate admin setup, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unauthenticated client gets 401 on protected endpoint and minimal /api/status.
	resp, _ = http.Get(ts.URL + "/api/settings")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session cookie, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/api/status")
	var statusBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&statusBody)
	resp.Body.Close()
	if statusBody["has_admin"] != true || statusBody["authenticated"] != false {
		t.Fatalf("unexpected unauthenticated /api/status: %+v", statusBody)
	}
	if _, leaked := statusBody["setup_step"]; leaked {
		t.Fatal("setup_step should not be exposed to unauthenticated callers once admin exists")
	}

	// Short passphrase (< 8 chars) must be rejected.
	resp, err = client.Post(ts.URL+"/api/setup/key/init", "application/json",
		bytes.NewBufferString(`{"passphrase":"123456"}`))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for short passphrase, got status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Authenticated admin can hash and initialize custom passphrase (>= 8 chars) via Argon2id.
	resp, err = client.Post(ts.URL+"/api/setup/key/init", "application/json",
		bytes.NewBufferString(`{"passphrase":"passphrase123"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("init key failed: status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if !st.HasMasterKey() {
		t.Fatal("expected master key to be derived and stored")
	}

	// Re-initializing master key must be rejected (409).
	resp, _ = client.Post(ts.URL+"/api/setup/key/init", "application/json",
		bytes.NewBufferString(`{"passphrase":"anotherpass123"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate key init, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSecurityHeadersAccountIDValidationPromoteGuardAndLoginRateLimit(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tgMgr := tg.NewManager(dir)
	eng := storage.NewEngine(store, st, tgMgr, dir)
	defer eng.Close()
	srv := NewServer(eng, tgMgr)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Security headers must be present on Web UI / API responses.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %+v", resp.Header)
	}

	client := setupAndLoginAdmin(t, ts)

	// Path traversal in backup account ID must be rejected with 400.
	_ = st.SetTelegramApp(12345, "hash")
	for _, badID := range []string{"../evil", "a/b", "a\\b", "foo.bar"} {
		body, _ := json.Marshal(map[string]any{"id": badID})
		r, err := client.Post(ts.URL+"/api/accounts/backup/start", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad account id %q: status=%d, want 400", badID, r.StatusCode)
		}
	}

	// Promote backup account must return 409 when unreplicated blobs remain.
	hdr := make([]byte, 32)
	copy(hdr, "TMP1")
	_, _, err = store.UpsertFileWithParts(db.RootID, "important.dat", 100, "application/octet-stream", []db.FilePart{
		{PartIndex: 0, TGMsgID: 77, TGFileID: 1, TGAccessHash: 2, TGDCID: 4, PlainSize: 100, CipherSize: 148, PartHeader: hdr},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertBackupAccount(state.BackupAccountConfig{ID: "acc-bak1", OwnChannelID: 999})

	r, err := client.Post(ts.URL+"/api/accounts/backup/acc-bak1/promote", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 when promoting backup with unreplicated blobs, got %d", r.StatusCode)
	}

	// Login rate limiting kicks in after 10 failed attempts from the same IP.
	for i := 0; i < 10; i++ {
		lr, err := http.Post(ts.URL+"/api/auth/login", "application/json",
			strings.NewReader(`{"username":"admin","password":"wrong-password"}`))
		if err != nil {
			t.Fatal(err)
		}
		lr.Body.Close()
		if lr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d, want 401", i+1, lr.StatusCode)
		}
	}
	lr, err := http.Post(ts.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"wrong-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after 10 failed logins, got %d", lr.StatusCode)
	}
}

func TestSelectChannelPreserves64BitAccessHash(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tgMgr := tg.NewManager(dir)
	eng := storage.NewEngine(store, st, tgMgr, dir)
	srv := NewServer(eng, tgMgr)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := setupAndLoginAdmin(t, ts)

	// AccessHash exceeding JS Number.MAX_SAFE_INTEGER (2^53-1 = 9007199254740991).
	const wantHash int64 = 7291847291847291843
	payload, err := json.Marshal(tg.ChannelInfo{ID: 2147483649, AccessHash: wantHash, Title: "Old Vault"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"access_hash":"7291847291847291843"`) {
		t.Fatalf("expected ChannelInfo.AccessHash serialized as JSON string, got %s", payload)
	}

	resp, err := client.Post(ts.URL+"/api/setup/channels/select", "application/json", bytes.NewReader(payload))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("select channel failed: status=%v err=%v", resp, err)
	}
	resp.Body.Close()

	if got := st.Snapshot().ChannelAccessHash; got != wantHash {
		t.Fatalf("ChannelAccessHash = %d, want %d (64-bit precision lost!)", got, wantHash)
	}

	// Clearing channel before key init succeeds so user can switch channels in Step 4.
	resp, err = client.Post(ts.URL+"/api/setup/channels/clear", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("clear channel failed: status=%v err=%v", resp, err)
	}
	resp.Body.Close()
	if st.HasChannel() {
		t.Fatal("expected channel cleared")
	}
}
