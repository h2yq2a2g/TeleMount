package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
)

type mockDesktopController struct {
	mu           sync.Mutex
	status       DesktopStatus
	updateErr    error
	mountErr     error
	unmountErr   error
	openErr      error
	flushErr     error
	clearErr     error
	mountCalls   int
	unmountCalls int
	openCalls    int
	flushCalls   int
	clearCalls   int
}

func newMockDesktopController() *mockDesktopController {
	return &mockDesktopController{
		status: DesktopStatus{
			Enabled:           true,
			WinFspInstalled:   true,
			DriveLetter:       "V:",
			DiskCacheMaxBytes: 10 * 1024 * 1024 * 1024,
			PinnedPaths:       []string{"/Documents"},
			AutoMount:         true,
		},
	}
}

func (m *mockDesktopController) Status() DesktopStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := m.status
	cp.PinnedPaths = append([]string(nil), m.status.PinnedPaths...)
	return cp
}

func (m *mockDesktopController) UpdateSettings(u DesktopSettingsUpdate) (DesktopStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.status, m.updateErr
	}
	if u.DriveLetter != nil {
		m.status.DriveLetter = *u.DriveLetter
	}
	if u.DiskCacheMaxBytes != nil {
		m.status.DiskCacheMaxBytes = *u.DiskCacheMaxBytes
	}
	if u.PinnedPaths != nil {
		m.status.PinnedPaths = append([]string(nil), (*u.PinnedPaths)...)
	}
	if u.RunAtStartup != nil {
		m.status.RunAtStartup = *u.RunAtStartup
	}
	if u.AutoMount != nil {
		m.status.AutoMount = *u.AutoMount
	}
	cp := m.status
	cp.PinnedPaths = append([]string(nil), m.status.PinnedPaths...)
	return cp, nil
}

func (m *mockDesktopController) Mount() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mountCalls++
	if m.mountErr != nil {
		m.status.MountError = m.mountErr.Error()
		return m.mountErr
	}
	m.status.Mounted = true
	m.status.MountError = ""
	return nil
}

func (m *mockDesktopController) Unmount() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmountCalls++
	if m.unmountErr != nil {
		return m.unmountErr
	}
	m.status.Mounted = false
	return nil
}

func (m *mockDesktopController) OpenDrive() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openCalls++
	return m.openErr
}

func (m *mockDesktopController) FlushQueue(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushCalls++
	if m.flushErr != nil {
		m.status.QueueLastError = m.flushErr.Error()
		return m.flushErr
	}
	m.status.QueuePendingCount = 0
	m.status.QueuePendingBytes = 0
	m.status.QueueLastError = ""
	return nil
}

func (m *mockDesktopController) ClearUnpinnedCache() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCalls++
	if m.clearErr != nil {
		return m.clearErr
	}
	m.status.DiskCacheBytes = m.status.DiskCachePinnedBytes
	return nil
}

func newTestWebServer(t *testing.T) (*Server, *state.Manager, *tg.Manager, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tgMgr := tg.NewManager(dir)
	eng := storage.NewEngine(store, st, tgMgr, dir)
	srv := NewServer(eng, tgMgr)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, st, tgMgr, ts
}

func setupAndLoginAdmin(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/setup/admin", "application/json",
		strings.NewReader(`{"username":"admin","password":"webpassword123"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup admin failed: status=%v err=%v", resp, err)
	}
	resp.Body.Close()
	return client
}

func TestDesktopDisabledDefaults(t *testing.T) {
	_, _, _, ts := newTestWebServer(t)

	// Unauthenticated /api/status before admin setup still exposes desktop_enabled: false.
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status failed: %v", err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["desktop_enabled"] != false {
		t.Fatalf("expected desktop_enabled=false, got %v", st["desktop_enabled"])
	}

	client := setupAndLoginAdmin(t, ts)

	resp, err = client.Get(ts.URL + "/api/desktop/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/desktop/status failed: %v", err)
	}
	var ds DesktopStatus
	_ = json.NewDecoder(resp.Body).Decode(&ds)
	resp.Body.Close()
	if ds.Enabled || ds.PinnedPaths == nil {
		t.Fatalf("expected disabled DesktopStatus with non-nil PinnedPaths, got %+v", ds)
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/desktop/settings", `{}`},
		{http.MethodPost, "/api/desktop/mount", ``},
		{http.MethodPost, "/api/desktop/unmount", ``},
		{http.MethodPost, "/api/desktop/open", ``},
		{http.MethodPost, "/api/desktop/cache/clear", ``},
		{http.MethodPost, "/api/desktop/queue/flush", ``},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s error: %v", tc.method, tc.path, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s expected 503 when disabled, got %d", tc.method, tc.path, r.StatusCode)
		}
	}

	// Skipping WebDAV must be rejected when desktop mode is disabled.
	resp, err = client.Post(ts.URL+"/api/setup/webdav", "application/json", strings.NewReader(`{"skip":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 skipping webdav when desktop disabled, got %d", resp.StatusCode)
	}
}

func TestDesktopAuthGuards(t *testing.T) {
	srv, _, _, ts := newTestWebServer(t)
	srv.SetDesktopController(newMockDesktopController())

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/desktop/status"},
		{http.MethodPut, "/api/desktop/settings"},
		{http.MethodPost, "/api/desktop/mount"},
		{http.MethodPost, "/api/desktop/unmount"},
		{http.MethodPost, "/api/desktop/open"},
		{http.MethodPost, "/api/desktop/cache/clear"},
		{http.MethodPost, "/api/desktop/queue/flush"},
	}

	// Before admin setup -> 403 Forbidden.
	for _, rt := range routes {
		req, _ := http.NewRequest(rt.method, ts.URL+rt.path, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("before admin %s %s: expected 403, got %d", rt.method, rt.path, resp.StatusCode)
		}
	}

	// Initialize admin, then query WITHOUT session cookie -> 401 Unauthorized.
	_ = setupAndLoginAdmin(t, ts)
	for _, rt := range routes {
		req, _ := http.NewRequest(rt.method, ts.URL+rt.path, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s %s: expected 401, got %d", rt.method, rt.path, resp.StatusCode)
		}
	}

	// Unauthenticated /api/status exposes desktop_enabled: true without leaking setup_step.
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status failed: %v", err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["desktop_enabled"] != true || st["has_admin"] != true || st["authenticated"] != false {
		t.Fatalf("unexpected unauthenticated /api/status: %+v", st)
	}
	if _, leaked := st["setup_step"]; leaked {
		t.Fatal("setup_step leaked to unauthenticated caller")
	}
}

func TestDesktopSetupWizardSkipsWebDAVAndDisablesDAVRoute(t *testing.T) {
	srv, st, tgMgr, ts := newTestWebServer(t)
	srv.SetDesktopController(newMockDesktopController())
	client := setupAndLoginAdmin(t, ts)

	if err := st.SetChannel(123456, 789, "Vault"); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(ts.URL+"/api/setup/key/init", "application/json", strings.NewReader(`{"passphrase":"secret"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("init key failed: %v", err)
	}
	resp.Body.Close()

	// On Windows Desktop mode, completing key setup finishes immediately (no WebDAV step).
	tgMgr.Tracker().SetAuthorized(true, &tg.UserInfo{ID: 42, Username: "alice"})
	resp, err = client.Get(ts.URL + "/api/setup/state")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/setup/state failed: %v", err)
	}
	var setupState map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&setupState)
	resp.Body.Close()
	if setupState["desktop_enabled"] != true || setupState["setup_step"] != "ready" || setupState["setup_completed"] != true {
		t.Fatalf("expected desktop_enabled=true, setup_step=ready, setup_completed=true; got %+v", setupState)
	}

	// WebDAV endpoint /dav/ is disabled in Desktop mode (404).
	davResp, err := client.Get(ts.URL + "/dav/")
	if err != nil {
		t.Fatal(err)
	}
	davResp.Body.Close()
	if davResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on /dav/ in desktop mode, got %d", davResp.StatusCode)
	}
}

func TestDesktopSettingsValidationAndUpdate(t *testing.T) {
	srv, _, _, ts := newTestWebServer(t)
	ctrl := newMockDesktopController()
	srv.SetDesktopController(ctrl)
	client := setupAndLoginAdmin(t, ts)

	putJSON := func(body string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		return resp, raw
	}

	// Valid normalization cases.
	for _, driveIn := range []string{"v", "v:\\", "V:/", "W:"} {
		resp, _ := putJSON(`{"drive_letter":"` + strings.ReplaceAll(driveIn, `\`, `\\`) + `"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for drive_letter %q, got %d", driveIn, resp.StatusCode)
		}
	}

	resp, _ := putJSON(`{
		"drive_letter": "z:\\",
		"disk_cache_max_bytes": 4096,
		"pinned_paths": ["Docs\\", "/Docs/", "Work/file.txt"],
		"run_at_startup": true,
		"auto_mount": false
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid settings update, got %d", resp.StatusCode)
	}
	got := ctrl.Status()
	if got.DriveLetter != "Z:" || got.DiskCacheMaxBytes != 4096 || !got.RunAtStartup || got.AutoMount {
		t.Fatalf("unexpected updated settings: %+v", got)
	}
	if len(got.PinnedPaths) != 2 || got.PinnedPaths[0] != "/Docs" || got.PinnedPaths[1] != "/Work/file.txt" {
		t.Fatalf("unexpected normalized pinned_paths: %+v", got.PinnedPaths)
	}

	// Invalid payloads must return 400.
	invalidPayloads := []string{
		`{not-json`,
		`{"drive_letter":""}`,
		`{"drive_letter":"C:"}`,
		`{"drive_letter":"c:\\"}`,
		`{"drive_letter":"1:"}`,
		`{"drive_letter":"VV"}`,
		`{"drive_letter":"V:foo"}`,
		`{"disk_cache_max_bytes":0}`,
		`{"disk_cache_max_bytes":-100}`,
		`{"pinned_paths":[""]}`,
		`{"pinned_paths":["../secret"]}`,
		`{"pinned_paths":["/bad\u0000path"]}`,
	}
	for _, bad := range invalidPayloads {
		r, _ := putJSON(bad)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %s: expected 400, got %d", bad, r.StatusCode)
		}
	}
}

func TestDesktopActionEndpointsAndDashboard(t *testing.T) {
	srv, _, _, ts := newTestWebServer(t)
	ctrl := newMockDesktopController()
	ctrl.status.QueuePendingCount = 2
	ctrl.status.QueuePendingBytes = 512
	ctrl.status.DiskCacheBytes = 1024
	ctrl.status.DiskCachePinnedBytes = 256
	srv.SetDesktopController(ctrl)
	client := setupAndLoginAdmin(t, ts)

	// Dashboard includes desktop status when enabled.
	resp, err := client.Get(ts.URL + "/api/dashboard")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/dashboard failed: %v", err)
	}
	var dash map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&dash)
	resp.Body.Close()
	if dash["desktop"] == nil {
		t.Fatal("expected desktop field in /api/dashboard response")
	}

	// Mount / Unmount / Open / Cache clear / Queue flush success paths.
	for _, p := range []string{
		"/api/desktop/mount",
		"/api/desktop/open",
		"/api/desktop/unmount",
		"/api/desktop/cache/clear",
		"/api/desktop/queue/flush",
	} {
		r, err := client.Post(ts.URL+p, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("POST %s expected 200, got %d", p, r.StatusCode)
		}
	}
	if ctrl.mountCalls != 1 || ctrl.unmountCalls != 1 || ctrl.openCalls != 1 || ctrl.clearCalls != 1 || ctrl.flushCalls != 1 {
		t.Fatalf("unexpected action call counts: mount=%d unmount=%d open=%d clear=%d flush=%d",
			ctrl.mountCalls, ctrl.unmountCalls, ctrl.openCalls, ctrl.clearCalls, ctrl.flushCalls)
	}
	if ctrl.Status().DiskCacheBytes != 256 || ctrl.Status().QueuePendingCount != 0 {
		t.Fatalf("expected cache cleared to pinned bytes and queue flushed, got %+v", ctrl.Status())
	}

	// Error propagation.
	ctrl.mountErr = errors.New("winfsp missing")
	ctrl.flushErr = errors.New("telegram offline")
	r1, _ := client.Post(ts.URL+"/api/desktop/mount", "application/json", nil)
	r1.Body.Close()
	if r1.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on mount error, got %d", r1.StatusCode)
	}
	r2, _ := client.Post(ts.URL+"/api/desktop/queue/flush", "application/json", nil)
	r2.Body.Close()
	if r2.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on queue flush error, got %d", r2.StatusCode)
	}
}

func TestNoFileBrowserOrTrashRoutesOrViews(t *testing.T) {
	srv, _, _, ts := newTestWebServer(t)
	srv.SetDesktopController(newMockDesktopController())
	client := setupAndLoginAdmin(t, ts)

	forbiddenRoutes := []string{
		"/api/files",
		"/api/files/1",
		"/api/browse",
		"/api/trash",
		"/api/trash/empty",
	}
	for _, route := range forbiddenRoutes {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			req, _ := http.NewRequest(method, ts.URL+route, strings.NewReader(`{}`))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("R3 violation: %s %s returned %d, expected 404", method, route, resp.StatusCode)
			}
		}
	}

	// Verify embedded static assets contain desktop control/WinFsp UI and zero file-browser/trash views.
	htmlBytes, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, err := fs.ReadFile(staticFS, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(htmlBytes) + "\n" + string(jsBytes)
	for _, forbidden := range []string{"/api/files", "/api/browse", "/api/trash", "file-browser", "trash-view"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("R3 violation: embedded UI contains forbidden token %q", forbidden)
		}
	}
	for _, required := range []string{
		"desktop-winfsp-banner",
		"winfsp.dev",
		"winget install WinFsp.WinFsp",
		"desktop-drive-card",
		"desktop-queue-card",
		"desktop-cache-card",
		"desktop-settings-card",
		"wizard-step-pill-webdav",
		"card-rclone-config",
		"card-webdav-settings",
	} {
		if !strings.Contains(combined, required) {
			t.Fatalf("embedded UI missing required desktop element/guidance %q", required)
		}
	}
}

type mockShellController struct {
	*mockDesktopController
	lastAction string
	lastPath   string
}

func (m *mockShellController) HandleShellAction(_ context.Context, action, virtualPath string) (ShellActionResult, error) {
	m.lastAction = action
	m.lastPath = virtualPath
	return ShellActionResult{OK: true, Message: action + ":" + virtualPath}, nil
}

func TestDesktopShellActionIPCTokenAuth(t *testing.T) {
	srv, _, _, ts := newTestWebServer(t)
	ctrl := &mockShellController{mockDesktopController: newMockDesktopController()}
	srv.SetDesktopController(ctrl)
	srv.SetDesktopIPCToken("secret-ipc-token-123")

	// Missing token before admin setup -> 403.
	resp, err := http.Post(ts.URL+"/api/desktop/shell-action", "application/json", strings.NewReader(`{"action":"pin","path":"/Docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without IPC token expected 403, got %d", resp.StatusCode)
	}

	// Valid IPC token bypasses admin cookie for local Explorer shell integration.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/desktop/shell-action", strings.NewReader(`{"action":"free","path":"/Docs/big.iso"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TeleMount-IPC-Token", "secret-ipc-token-123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with valid IPC token expected 200, got %d", resp.StatusCode)
	}
	if ctrl.lastAction != "free" || ctrl.lastPath != "/Docs/big.iso" {
		t.Fatalf("unexpected shell action dispatch: (%q, %q)", ctrl.lastAction, ctrl.lastPath)
	}
}
