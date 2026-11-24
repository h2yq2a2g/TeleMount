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

	// Authenticated admin can hash and initialize custom passphrase like "123456" via Argon2id.
	resp, err = client.Post(ts.URL+"/api/setup/key/init", "application/json",
		bytes.NewBufferString(`{"passphrase":"123456"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("init key failed: status=%v err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if !st.HasMasterKey() {
		t.Fatal("expected master key to be derived and stored")
	}

	// Re-initializing master key must be rejected (409).
	resp, _ = client.Post(ts.URL+"/api/setup/key/init", "application/json",
		bytes.NewBufferString(`{"passphrase":"another"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate key init, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
