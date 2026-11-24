//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/crypto"
	"telemount/internal/db"
	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
	"telemount/internal/tray"
	"telemount/internal/vfs"
	"telemount/internal/web"
)

func TestTier4_RealWorldScenarios(t *testing.T) {
	t.Run("T4-01_EditorAtomicTempFileRenameSaveOfflineRestartSync", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/notes/todo.txt", []byte("v1"))

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		fs := vfs.NewTeleFS(store, mgr)

		// Disconnect and execute VS Code / Notepad atomic temp-file-rename save pattern.
		fake.SetOnline(false)
		rc, fh := fs.Create("/notes/todo.txt.tmp~", fuse.O_CREAT|fuse.O_EXCL|fuse.O_RDWR, 0o644)
		if rc != 0 {
			t.Fatalf("Create tmp~ rc=%d", rc)
		}
		edited := []byte("v2 edited offline")
		if n := fs.Write("/notes/todo.txt.tmp~", edited, 0, fh); n != len(edited) {
			t.Fatalf("Write tmp~ = %d", n)
		}
		_ = fs.Flush("/notes/todo.txt.tmp~", fh)
		_ = fs.Release("/notes/todo.txt.tmp~", fh)
		if rc := fs.Rename("/notes/todo.txt.tmp~", "/notes/todo.txt"); rc != 0 {
			t.Fatalf("Rename overwrite rc=%d", rc)
		}

		names, rc := vfsListNames(t, fs, "/notes")
		if rc != 0 || slices.Contains(names, "todo.txt.tmp~") || !slices.Contains(names, "todo.txt") {
			t.Fatalf("unexpected /notes listing after atomic save: %v (rc=%d)", names, rc)
		}
		gotOffline, rc := vfsReadAll(t, fs, "/notes/todo.txt")
		if rc != 0 || !bytes.Equal(gotOffline, edited) {
			t.Fatalf("offline read = (%q, rc=%d), want %q", gotOffline, rc, edited)
		}
		_ = mgr.Close()

		// Restart offline and confirm persistence.
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		fs2 := vfs.NewTeleFS(store, mgr2)
		gotRestart, rc := vfsReadAll(t, fs2, "/notes/todo.txt")
		if rc != 0 || !bytes.Equal(gotRestart, edited) {
			t.Fatalf("post-restart offline read = (%q, rc=%d), want %q", gotRestart, rc, edited)
		}

		// Reconnect and flush.
		fake.SetOnline(true)
		if err := mgr2.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		_ = eng.Packer().FlushNow(t.Context())
		if mgr2.QueueStatus().PendingCount != 0 {
			t.Fatalf("PendingCount = %d, want 0", mgr2.QueueStatus().PendingCount)
		}
		if _, err := store.StatByPath("/notes/todo.txt.tmp~"); err == nil {
			t.Fatal("orphan .tmp~ found in db.Store")
		}
		synced, err := readAllEngineFile(t, eng, store, "/notes/todo.txt")
		if err != nil || !bytes.Equal(synced, edited) {
			t.Fatalf("synced /notes/todo.txt = (%q, %v), want %q", synced, err, edited)
		}
	})

	t.Run("T4-02_MediaAndDocsPinnedFolderSurvivesHeavyCacheChurnRestart", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		contract := bytes.Repeat([]byte("C"), 1024)
		budget := bytes.Repeat([]byte("B"), 1024)
		uploadSeedFile(t, eng, store, fake, "/Important/contract.pdf", contract)
		uploadSeedFile(t, eng, store, fake, "/Important/budget.xlsx", budget)
		for i := 1; i <= 6; i++ {
			name := "/Media/clip" + string(rune('0'+i)) + ".bin"
			uploadSeedFile(t, eng, store, fake, name, bytes.Repeat([]byte{byte('0' + i)}, 1024))
		}

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, 4096, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		_ = mgr.SetPinnedPaths([]string{"/Important"})
		if err := mgr.PinSync(t.Context(), "/Important"); err != nil {
			t.Fatalf("PinSync(/Important): %v", err)
		}

		fs := vfs.NewTeleFS(store, mgr)
		for i := 1; i <= 6; i++ {
			name := "/Media/clip" + string(rune('0'+i)) + ".bin"
			if _, rc := vfsReadAll(t, fs, name); rc != 0 {
				t.Fatalf("stream %s rc=%d", name, rc)
			}
		}
		if st := mgr.Stats(); st.TotalBytes > 4096 {
			t.Fatalf("cache TotalBytes = %d, exceeded budget 4096", st.TotalBytes)
		}
		_ = mgr.Close()

		fake.SetOnline(false)
		eng.Cache().Clear()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, 4096, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		_ = mgr2.SetPinnedPaths([]string{"/Important"})
		fs2 := vfs.NewTeleFS(store, mgr2)

		if gotC, rc := vfsReadAll(t, fs2, "/Important/contract.pdf"); rc != 0 || !bytes.Equal(gotC, contract) {
			t.Fatalf("pinned contract lost: rc=%d", rc)
		}
		if gotB, rc := vfsReadAll(t, fs2, "/Important/budget.xlsx"); rc != 0 || !bytes.Equal(gotB, budget) {
			t.Fatalf("pinned budget lost: rc=%d", rc)
		}
		if _, rc := vfsReadAll(t, fs2, "/Media/clip1.bin"); rc == 0 {
			t.Fatal("expected oldest unpinned /Media/clip1.bin evicted under churn")
		}
	})

	t.Run("T4-03_MissingWinFspGuidanceToSimulatedInstallToMount", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		_, _ = rig.cfgStore.Update(func(c *desktop.Config) error {
			c.AutoMount = true
			return nil
		})
		_ = rig.Mount() // AutoMount attempt fails cleanly without panic.

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")
		actions := &rigTrayActions{rig: rig}

		if actions.Status().WinFspInstalled || actions.Status().WinFspGuidance == "" {
			t.Fatalf("expected missing WinFsp guidance in tray")
		}
		mr, _ := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		mr.Body.Close()
		if mr.StatusCode < 400 {
			t.Fatalf("mount before WinFsp install should fail, got %d", mr.StatusCode)
		}

		// Simulate user installing WinFsp.
		rig.probe.Set(desktop.WinFspStatus{Installed: true, DLLPath: `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`})
		if err := actions.Mount(); err != nil {
			t.Fatalf("Mount after WinFsp install: %v", err)
		}
		r, _ := client.Get(ts.URL + "/api/desktop/status")
		var ds web.DesktopStatus
		_ = json.NewDecoder(r.Body).Decode(&ds)
		r.Body.Close()
		if !ds.Mounted || ds.DriveLetter != "V:" || !ds.WinFspInstalled {
			t.Fatalf("unexpected final status: %+v", ds)
		}
	})

	t.Run("T4-04_WebUIFullSetupManagementAndNegativeSurfaceCheck", func(t *testing.T) {
		rig := newTestDesktopRigFreshAdmin(t, t.TempDir())
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}

		// Step 1: Setup admin.
		rAdmin, _ := client.Post(ts.URL+"/api/setup/admin", "application/json", strings.NewReader(`{"username":"admin","password":"webpassword123"}`))
		rAdmin.Body.Close()
		if rAdmin.StatusCode != http.StatusOK {
			t.Fatalf("setup admin = %d", rAdmin.StatusCode)
		}

		// Step 2: Check /api/status.
		rStatus, _ := client.Get(ts.URL + "/api/status")
		var statusMap map[string]any
		_ = json.NewDecoder(rStatus.Body).Decode(&statusMap)
		rStatus.Body.Close()
		if statusMap["desktop_enabled"] != true {
			t.Fatalf("desktop_enabled = %v, want true", statusMap["desktop_enabled"])
		}

		// Step 3: Update settings.
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(`{"drive_letter":"Z:","disk_cache_max_bytes":52428800,"pinned_paths":["/Work"],"run_at_startup":true}`))
		req.Header.Set("Content-Type", "application/json")
		rPut, _ := client.Do(req)
		rPut.Body.Close()
		if rPut.StatusCode != http.StatusOK {
			t.Fatalf("PUT settings = %d", rPut.StatusCode)
		}

		// Step 4: Queue offline write, bring online, flush & clear unpinned via API.
		rig.fakeTG.SetOnline(false)
		if rc := rig.fs.Mkdir("/Work", 0o755); rc != 0 {
			t.Fatalf("Mkdir /Work rc=%d", rc)
		}
		vfsWriteAll(t, rig.fs, "/Work/doc.txt", []byte("work-doc"))
		rig.fakeTG.SetOnline(true)
		rFlush, _ := client.Post(ts.URL+"/api/desktop/queue/flush", "application/json", strings.NewReader("{}"))
		rFlush.Body.Close()
		rClear, _ := client.Post(ts.URL+"/api/desktop/cache/clear", "application/json", strings.NewReader("{}"))
		rClear.Body.Close()
		if rFlush.StatusCode != http.StatusOK || rClear.StatusCode != http.StatusOK {
			t.Fatalf("flush=%d clear=%d", rFlush.StatusCode, rClear.StatusCode)
		}

		// Step 5: Verify no file browser or trash endpoints/views exist.
		for _, badPath := range []string{"/api/files", "/api/browse", "/api/trash"} {
			rBad, _ := client.Get(ts.URL + badPath)
			rBad.Body.Close()
			if rBad.StatusCode != http.StatusNotFound && rBad.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("forbidden route %s returned %d", badPath, rBad.StatusCode)
			}
		}
		for _, asset := range []string{"/", "/app.js"} {
			rAsset, _ := client.Get(ts.URL + asset)
			b, _ := io.ReadAll(rAsset.Body)
			rAsset.Body.Close()
			low := strings.ToLower(string(b))
			if strings.Contains(low, "view-files") || strings.Contains(low, "view-trash") {
				t.Fatalf("forbidden view in %s", asset)
			}
		}
	})

	t.Run("T4-05_TrayDailyLifecycleMountOpenToggleUnmountQuit", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		actions := &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil || app == nil {
			t.Fatalf("tray.New = (%v, %v)", app, err)
		}
		defer app.Close()

		if err := actions.Mount(); err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if err := actions.OpenDrive(); err != nil {
			t.Fatalf("OpenDrive: %v", err)
		}
		if err := actions.OpenWebUI(); err != nil {
			t.Fatalf("OpenWebUI: %v", err)
		}
		enabled, err := actions.ToggleStartup()
		if err != nil || !enabled {
			t.Fatalf("ToggleStartup = (%v, %v)", enabled, err)
		}
		if cmd, ok, _ := rig.startupStore.Get("TeleMount"); !ok || !strings.HasPrefix(cmd, `"`) {
			t.Fatalf("startup command not quoted: %q (ok=%v)", cmd, ok)
		}

		rig.fakeTG.SetOnline(false)
		vfsWriteAll(t, rig.fs, "/tray-work.txt", []byte("sync-on-quit"))
		rig.fakeTG.SetOnline(true)

		if err := actions.Unmount(); err != nil {
			t.Fatalf("Unmount: %v", err)
		}
		quit, err := tray.DispatchMenuAction(tray.MenuIDQuit, actions, nil)
		if err != nil || !quit {
			t.Fatalf("DispatchMenuAction(MenuIDQuit) = (%v, %v)", quit, err)
		}
		_ = app.Close()

		if actions.Status().Mounted || rig.cache.QueueStatus().PendingCount != 0 {
			t.Fatalf("expected unmounted and flushed on quit: %+v", actions.Status())
		}
	})

	t.Run("T4-06_MultiFileOfflineSequencePreservesFIFOSyncOrder", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		s, _ := store.GetSettings()
		s.SmallFileMaxMB = 0
		_ = store.SaveSettings(s)
		uploadSeedFile(t, eng, store, fake, "/old.txt", []byte("old"))
		uploadSeedFile(t, eng, store, fake, "/keep.txt", []byte("k1"))

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		fs := vfs.NewTeleFS(store, mgr)

		fake.SetOnline(false)
		if rc := fs.Mkdir("/proj", 0o755); rc != 0 {
			t.Fatalf("Mkdir rc=%d", rc)
		}
		vfsWriteAll(t, fs, "/proj/a.txt", []byte("alpha"))
		vfsWriteAll(t, fs, "/keep.txt", []byte("k2"))
		if rc := fs.Rename("/proj/a.txt", "/proj/b.txt"); rc != 0 {
			t.Fatalf("Rename rc=%d", rc)
		}
		if rc := fs.Unlink("/old.txt"); rc != 0 {
			t.Fatalf("Unlink rc=%d", rc)
		}
		vfsWriteAll(t, fs, "/proj/c.txt", []byte("gamma"))
		_ = mgr.Close()

		// Restart offline and verify directory tree & contents.
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		fs2 := vfs.NewTeleFS(store, mgr2)

		var st fuse.Stat_t
		if rc := fs2.Getattr("/old.txt", &st, 0); rc != -fuse.ENOENT {
			t.Fatalf("expected /old.txt deleted offline, rc=%d", rc)
		}
		if g, _ := vfsReadAll(t, fs2, "/keep.txt"); string(g) != "k2" {
			t.Fatalf("/keep.txt offline = %q, want k2", g)
		}
		if g, _ := vfsReadAll(t, fs2, "/proj/b.txt"); string(g) != "alpha" {
			t.Fatalf("/proj/b.txt offline = %q, want alpha", g)
		}
		if g, _ := vfsReadAll(t, fs2, "/proj/c.txt"); string(g) != "gamma" {
			t.Fatalf("/proj/c.txt offline = %q, want gamma", g)
		}

		// Reconnect and flush.
		fake.SetOnline(true)
		fake.ResetCounters()
		if err := mgr2.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if _, err := store.StatByPath("/old.txt"); err == nil {
			t.Fatal("/old.txt still exists in db.Store")
		}
		for p, want := range map[string]string{"/keep.txt": "k2", "/proj/b.txt": "alpha", "/proj/c.txt": "gamma"} {
			got, err := readAllEngineFile(t, eng, store, p)
			if err != nil || string(got) != want {
				t.Fatalf("synced %s = (%q, %v), want %q", p, got, err, want)
			}
		}
		if len(fake.UploadHistory()) < 3 {
			t.Fatalf("expected >= 3 FIFO uploads recorded, got %d", len(fake.UploadHistory()))
		}
	})

	t.Run("T4-07_TMDBBackupRestoreLeavesDesktopLocalStateIntact", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		drv := "T:"
		startup := true
		pins := []string{"/Keep"}
		if _, err := rig.UpdateSettings(web.DesktopSettingsUpdate{
			DriveLetter:  &drv,
			RunAtStartup: &startup,
			PinnedPaths:  &pins,
		}); err != nil {
			t.Fatal(err)
		}
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/file1.txt", []byte("important-cloud-file"))
		doc, err := rig.engine.BackupDBToTelegram(t.Context())
		if err != nil {
			t.Fatalf("BackupDBToTelegram: %v", err)
		}

		// Simulate local SQLite metadata loss without wiping remote Telegram blobs.
		n, _ := rig.engine.Store().StatByPath("/file1.txt")
		if _, err := rig.engine.Store().PermanentDelete(n.ID); err != nil {
			t.Fatalf("local PermanentDelete: %v", err)
		}
		if err := rig.engine.RestoreDBFromTelegram(t.Context(), doc.MsgID, "e2e-passphrase"); err != nil {
			t.Fatalf("RestoreDBFromTelegram: %v", err)
		}

		got, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/file1.txt")
		if err != nil || string(got) != "important-cloud-file" {
			t.Fatalf("restored /file1.txt = (%q, %v)", got, err)
		}
		reloaded, err := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := reloaded.Get()
		if cfg.DriveLetter != "T:" || !cfg.RunAtStartup || !desktop.IsPathPinned(cfg.PinnedPaths, "/Keep/sub.txt") {
			t.Fatalf("desktop.json altered by .tmdb restore: %+v", cfg)
		}
		if enabled, err := rig.startup.IsEnabled(); err != nil || !enabled {
			t.Fatalf("StartupManager state altered: (%v, %v)", enabled, err)
		}
	})

	t.Run("T4-08_UnifiedBinaryWindowsAndLinuxNoCGO", func(t *testing.T) {
		root := repoRoot(t)
		binDir := t.TempDir()

		winCmd := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", filepath.Join(binDir, "telemount.exe"), "./cmd/telemount")
		winCmd.Dir = root
		winCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
		if out, err := winCmd.CombinedOutput(); err != nil {
			t.Fatalf("build windows cmd/telemount: %v\n%s", err, out)
		}

		linuxCmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "telemount-linux-arm64"), "./cmd/telemount")
		linuxCmd.Dir = root
		linuxCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
		if out, err := linuxCmd.CombinedOutput(); err != nil {
			t.Fatalf("build linux/arm64 cmd/telemount: %v\n%s", err, out)
		}

		testCmd := exec.Command("go", "test",
			"./cmd/telemount/...",
			"./internal/crypto/...",
			"./internal/db/...",
			"./internal/state/...",
			"./internal/storage/...",
			"./internal/dav/...",
			"./internal/web/...",
		)
		testCmd.Dir = root
		if out, err := testCmd.CombinedOutput(); err != nil {
			t.Fatalf("existing test suite regressed: %v\n%s", err, out)
		}

		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		plainSrv := web.NewServer(rig.engine, rig.tgMgr)
		tsPlain, cPlain := newAuthedWebClient(t, plainSrv, "admin", "adminpass123")
		r1, err := cPlain.Get(tsPlain.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		var m1 map[string]any
		_ = json.NewDecoder(r1.Body).Decode(&m1)
		r1.Body.Close()
		if m1["desktop_enabled"] != false {
			t.Fatalf("plain server desktop_enabled = %v, want false", m1["desktop_enabled"])
		}

		deskSrv := web.NewServer(rig.engine, rig.tgMgr)
		deskSrv.SetDesktopController(rig)
		tsDesk, cDesk := newAuthedWebClient(t, deskSrv, "admin", "adminpass123")
		r2, err := cDesk.Get(tsDesk.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		var m2 map[string]any
		_ = json.NewDecoder(r2.Body).Decode(&m2)
		r2.Body.Close()
		if m2["desktop_enabled"] != true {
			t.Fatalf("desktop server desktop_enabled = %v, want true", m2["desktop_enabled"])
		}
	})
}

// newTestDesktopRigFreshAdmin creates a rig whose state.Manager has NO admin yet
// (for testing pre-admin setup wizard + desktop controller coexistence).
func newTestDesktopRigFreshAdmin(t *testing.T, dataDir string) *testDesktopRig {
	t.Helper()
	st, err := state.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannel(1001, 2002, "E2EVault"); err != nil {
		t.Fatal(err)
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMasterKey(crypto.DeriveMasterKey("e2e-passphrase", salt), salt); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dataDir, "telemount.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fake := newFakeTG(true)
	eng := storage.NewEngine(store, st, fake, dataDir)
	tgMgr := tg.NewManager(dataDir)
	tgMgr.Tracker().SetAuthorized(true, &tg.UserInfo{ID: 100, Username: "e2e"})

	cfgStore, err := desktop.LoadConfig(filepath.Join(dataDir, "desktop.json"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgStore.Get()
	cache, err := diskcache.Open(cfg.DiskCacheDir, filepath.Join(dataDir, "desktop_cache.db"), cfg.DiskCacheMaxBytes, store, eng)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	ss := newFakeStartupStore()
	probe := newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`, "")
	dc := newFakeDriveChecker("C:")
	tfs := vfs.NewTeleFS(store, cache)
	return &testDesktopRig{
		dataDir:      dataDir,
		exePath:      `C:\Program Files\TeleMount\telemount-desktop.exe`,
		cfgStore:     cfgStore,
		startup:      desktop.NewStartupManager(ss, "TeleMount"),
		startupStore: ss,
		probe:        probe,
		driveChecker: dc,
		cache:        cache,
		fs:           tfs,
		mountMgr:     vfs.NewMountManager(tfs, probe, dc),
		fakeTG:       fake,
		engine:       eng,
		tgMgr:        tgMgr,
	}
}
