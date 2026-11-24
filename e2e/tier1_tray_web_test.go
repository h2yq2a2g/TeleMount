//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"telemount/internal/tray"
	"telemount/internal/web"
)

func TestTier1_F10_SystemTray(t *testing.T) {
	t.Run("T1-F10-01_TrayStatusReflectsState", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		if err := rig.Mount(); err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if _, err := rig.ToggleStartup(); err != nil {
			t.Fatalf("ToggleStartup: %v", err)
		}
		// Queue 3 pending offline writes.
		rig.fakeTG.SetOnline(false)
		for _, p := range []string{"/t1.txt", "/t2.txt", "/t3.txt"} {
			st, err := rig.cache.StageOpenForWrite(p, true)
			if err != nil {
				t.Fatalf("Stage: %v", err)
			}
			_ = os.WriteFile(st, []byte("tray"), 0o600)
			_ = rig.cache.CommitFile(t.Context(), p, st, 4, time.Now().Unix())
		}
		rig.fakeTG.SetOnline(true)

		var actions tray.TrayActions = &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil || app == nil {
			t.Fatalf("tray.New = (%v, %v)", app, err)
		}
		defer app.Close()
		st := actions.Status()
		if !st.Mounted || st.DriveLetter != "V:" || !st.WinFspInstalled || !st.RunAtStartup || st.QueuePending != 3 || !st.Online {
			t.Fatalf("unexpected TrayStatus: %+v", st)
		}
		items := tray.BuildMenuItems(st)
		if len(items) == 0 {
			t.Fatal("expected non-empty tray menu items")
		}
		tip := tray.FormatTooltip(st)
		if !strings.Contains(tip, "V:") || !strings.Contains(tip, "3 pending") {
			t.Fatalf("unexpected tooltip: %q", tip)
		}
	})

	t.Run("T1-F10-02_MountAndUnmountDelegateCleanly", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		var actions tray.TrayActions = &rigTrayActions{rig: rig}
		if err := actions.Mount(); err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if !actions.Status().Mounted {
			t.Fatalf("expected Mounted=true")
		}
		if err := actions.Unmount(); err != nil {
			t.Fatalf("Unmount: %v", err)
		}
		if actions.Status().Mounted {
			t.Fatalf("expected Mounted=false after Unmount")
		}
	})

	t.Run("T1-F10-03_OpenDriveAndOpenWebUI", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		var actions tray.TrayActions = &rigTrayActions{rig: rig}
		if err := actions.Mount(); err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if err := actions.OpenDrive(); err != nil {
			t.Fatalf("OpenDrive: %v", err)
		}
		if err := actions.OpenWebUI(); err != nil {
			t.Fatalf("OpenWebUI: %v", err)
		}
		if rig.openDriveN != 1 || rig.openWebUIN != 1 {
			t.Fatalf("expected openDrive=1 openWebUI=1, got %d / %d", rig.openDriveN, rig.openWebUIN)
		}
	})

	t.Run("T1-F10-04_ToggleStartupFlipsState", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		var actions tray.TrayActions = &rigTrayActions{rig: rig}
		enabled1, err := actions.ToggleStartup()
		if err != nil || !enabled1 {
			t.Fatalf("first ToggleStartup = (%v, %v), want (true, nil)", enabled1, err)
		}
		enabled2, err := actions.ToggleStartup()
		if err != nil || enabled2 {
			t.Fatalf("second ToggleStartup = (%v, %v), want (false, nil)", enabled2, err)
		}
	})

	t.Run("T1-F10-05_CleanQuitFlushesAndExits", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), false, true)
		st, _ := rig.cache.StageOpenForWrite("/quit-flush.txt", true)
		_ = os.WriteFile(st, []byte("flush-on-quit"), 0o600)
		_ = rig.cache.CommitFile(t.Context(), "/quit-flush.txt", st, 13, time.Now().Unix())
		rig.fakeTG.SetOnline(true)

		var actions tray.TrayActions = &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil {
			t.Fatalf("tray.New: %v", err)
		}
		quit, err := tray.DispatchMenuAction(tray.MenuIDQuit, actions, nil)
		if err != nil || !quit {
			t.Fatalf("DispatchMenuAction(MenuIDQuit) = (%v, %v), want (true, nil)", quit, err)
		}
		if err := app.Close(); err != nil {
			t.Fatalf("app.Close: %v", err)
		}

		if rig.quitN < 1 {
			t.Fatalf("expected Quit callback invoked")
		}
		if qs := rig.cache.QueueStatus(); qs.PendingCount != 0 {
			t.Fatalf("expected pending queue drained on Quit, got %d", qs.PendingCount)
		}
	})
}

func TestTier1_F11_MissingWinFspGuidance(t *testing.T) {
	t.Run("T1-F11-01_MissingProbeReturnsActionableGuidance", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		wf := rig.probe.Check()
		if wf.Installed {
			t.Fatal("expected Installed=false")
		}
		low := strings.ToLower(wf.Guidance)
		if !strings.Contains(low, "winfsp") || (!strings.Contains(low, "winfsp.dev") && !strings.Contains(low, "winget")) {
			t.Fatalf("guidance not actionable: %q", wf.Guidance)
		}
	})

	t.Run("T1-F11-02_TraySurfacesMissingWinFspWithoutPanic", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		actions := &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil || app == nil {
			t.Fatalf("tray.New = (%v, %v)", app, err)
		}
		defer app.Close()
		st := actions.Status()
		if st.WinFspInstalled || st.WinFspGuidance == "" {
			t.Fatalf("expected missing WinFsp status & guidance in tray, got %+v", st)
		}
		if tray.CheckAndNotifyWinFsp(st, nil) {
			t.Fatal("expected CheckAndNotifyWinFsp=false when WinFsp missing")
		}
	})

	t.Run("T1-F11-03_WebStatusSurfacesMissingWinFspGuidance", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		resp, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatalf("GET /api/desktop/status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload["winfsp_installed"] != false {
			t.Fatalf("winfsp_installed = %v, want false", payload["winfsp_installed"])
		}
		g, _ := payload["winfsp_guidance"].(string)
		if strings.TrimSpace(g) == "" {
			t.Fatalf("expected non-empty winfsp_guidance in /api/desktop/status")
		}
	})

	t.Run("T1-F11-04_WebStaticAssetsHaveWinFspBannerHook", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		htmlResp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		htmlBytes, _ := io.ReadAll(htmlResp.Body)
		htmlResp.Body.Close()

		jsResp, err := http.Get(ts.URL + "/app.js")
		if err != nil {
			t.Fatalf("GET /app.js: %v", err)
		}
		jsBytes, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		combined := strings.ToLower(string(htmlBytes) + "\n" + string(jsBytes))
		if !strings.Contains(combined, "winfsp") {
			t.Fatalf("expected embedded UI assets to reference WinFsp guidance")
		}
	})

	t.Run("T1-F11-05_AppStartupContinuesWhenWinFspMissing", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		// Simulate AutoMount attempt failing gracefully without crashing.
		_ = rig.Mount()

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		resp, err := client.Get(ts.URL + "/api/status")
		if err != nil {
			t.Fatalf("GET /api/status: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 even when WinFsp missing", resp.StatusCode)
		}
		if (&rigTrayActions{rig: rig}).Status().WinFspInstalled {
			t.Fatalf("expected tray to stay alive and report WinFspInstalled=false")
		}
	})
}

func TestTier1_F12_WebUIDesktopAPI(t *testing.T) {
	t.Run("T1-F12-01_StatusReportsDesktopEnabledFlag", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		tgMgr := rig.tgMgr

		plainSrv := web.NewServer(rig.engine, tgMgr)
		tsPlain, clientPlain := newAuthedWebClient(t, plainSrv, "admin", "adminpass123")
		resp1, err := clientPlain.Get(tsPlain.URL + "/api/status")
		if err != nil {
			t.Fatalf("plain GET /api/status: %v", err)
		}
		var m1 map[string]any
		_ = json.NewDecoder(resp1.Body).Decode(&m1)
		resp1.Body.Close()
		if m1["desktop_enabled"] != false {
			t.Fatalf("plain server desktop_enabled = %v, want false", m1["desktop_enabled"])
		}

		deskSrv := web.NewServer(rig.engine, tgMgr)
		deskSrv.SetDesktopController(rig)
		tsDesk, clientDesk := newAuthedWebClient(t, deskSrv, "admin", "adminpass123")
		resp2, err := clientDesk.Get(tsDesk.URL + "/api/status")
		if err != nil {
			t.Fatalf("desktop GET /api/status: %v", err)
		}
		var m2 map[string]any
		_ = json.NewDecoder(resp2.Body).Decode(&m2)
		resp2.Body.Close()
		if m2["desktop_enabled"] != true {
			t.Fatalf("desktop server desktop_enabled = %v, want true", m2["desktop_enabled"])
		}
	})

	t.Run("T1-F12-02_DesktopStatusTelemetryFields", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		resp, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatalf("GET /api/desktop/status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, key := range []string{
			"enabled", "winfsp_installed", "drive_letter", "mounted",
			"run_at_startup", "disk_cache_max_bytes", "disk_cache_bytes",
			"pinned_paths", "queue_pending_count",
		} {
			if _, ok := got[key]; !ok {
				t.Fatalf("missing key %q in /api/desktop/status: %+v", key, got)
			}
		}
	})

	t.Run("T1-F12-03_UpdateDesktopSettings", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		body := `{"drive_letter":"W:","disk_cache_max_bytes":104857600,"pinned_paths":["/Work","/Docs"],"run_at_startup":true}`
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT /api/desktop/settings: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
		}

		getResp, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatalf("GET /api/desktop/status: %v", err)
		}
		defer getResp.Body.Close()
		var st web.DesktopStatus
		if err := json.NewDecoder(getResp.Body).Decode(&st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if st.DriveLetter != "W:" || st.DiskCacheMaxBytes != 104857600 || !st.RunAtStartup || len(st.PinnedPaths) != 2 {
			t.Fatalf("persisted status mismatch: %+v", st)
		}
	})

	t.Run("T1-F12-04_DesktopControlEndpoints", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		// Seed one cached unpinned file and one queued offline write.
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/unpinned.txt", []byte("cache-me"))
		_, _ = readAllCacheFile(t.Context(), rig.cache, "/unpinned.txt", 8)

		rig.fakeTG.SetOnline(false)
		stPath, _ := rig.cache.StageOpenForWrite("/queued.txt", true)
		_ = os.WriteFile(stPath, []byte("queued"), 0o600)
		_ = rig.cache.CommitFile(t.Context(), "/queued.txt", stPath, 6, time.Now().Unix())
		rig.fakeTG.SetOnline(true)

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		post := func(path string) {
			t.Helper()
			r, err := client.Post(ts.URL+path, "application/json", bytes.NewReader([]byte(`{}`)))
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("POST %s status = %d, want 200", path, r.StatusCode)
			}
		}

		post("/api/desktop/mount")
		if !rig.Status().Mounted {
			t.Fatal("expected mounted=true after POST /api/desktop/mount")
		}
		post("/api/desktop/unmount")
		if rig.Status().Mounted {
			t.Fatal("expected mounted=false after POST /api/desktop/unmount")
		}
		post("/api/desktop/cache/clear")
		// ClearUnpinned removes clean /unpinned.txt (8B) while preserving dirty /queued.txt (6B).
		if got := rig.cache.Stats().UnpinnedBytes; got != 6 {
			t.Fatalf("expected UnpinnedBytes=6 (dirty preserved) after cache/clear, got %d", got)
		}
		post("/api/desktop/queue/flush")
		if rig.cache.QueueStatus().PendingCount != 0 {
			t.Fatalf("expected PendingCount=0 after queue/flush, got %d", rig.cache.QueueStatus().PendingCount)
		}
		post("/api/desktop/cache/clear")
		if got := rig.cache.Stats().UnpinnedBytes; got != 0 {
			t.Fatalf("expected UnpinnedBytes=0 after flush + cache/clear, got %d", got)
		}
	})

	t.Run("T1-F12-05_EmbeddedUIDesktopPanelsAndNoFileBrowserOrTrash", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		htmlResp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		htmlBody, _ := io.ReadAll(htmlResp.Body)
		htmlResp.Body.Close()

		jsResp, err := http.Get(ts.URL + "/app.js")
		if err != nil {
			t.Fatalf("GET /app.js: %v", err)
		}
		jsBody, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		htmlStr := string(htmlBody)
		jsStr := string(jsBody)
		combined := htmlStr + "\n" + jsStr

		if !strings.Contains(combined, "/api/desktop/") {
			t.Fatalf("expected embedded Web UI to integrate /api/desktop/ endpoints")
		}
		for _, forbidden := range []string{"/api/files", "/api/browse", "/api/trash", "view-files", "view-trash"} {
			if strings.Contains(strings.ToLower(combined), forbidden) {
				t.Fatalf("forbidden file-browser/trash reference %q found in embedded UI", forbidden)
			}
		}
	})
}
