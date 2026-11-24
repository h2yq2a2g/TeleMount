//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/vfs"
	"telemount/internal/web"
)

func TestTier3_CrossFeatureCombinations(t *testing.T) {
	t.Run("T3-01_MissingWinFspTrayStatusAndWebGuidance", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		traySt := (&rigTrayActions{rig: rig}).Status()
		if traySt.WinFspInstalled || traySt.WinFspGuidance == "" {
			t.Fatalf("tray status = %+v, want missing WinFsp + guidance", traySt)
		}

		r, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatal(err)
		}
		var webSt web.DesktopStatus
		_ = json.NewDecoder(r.Body).Decode(&webSt)
		r.Body.Close()
		if webSt.WinFspInstalled || webSt.WinFspGuidance == "" {
			t.Fatalf("web status = %+v, want missing WinFsp + guidance", webSt)
		}

		mr, err := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		mr.Body.Close()
		if mr.StatusCode < 400 {
			t.Fatalf("mount status = %d, want error when WinFsp missing", mr.StatusCode)
		}
	})

	t.Run("T3-02_ConfigDriveLetterConflictCheckAndMount", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		rig.driveChecker.SetInUse("E:", true)

		drvY := "Y:"
		if _, err := rig.UpdateSettings(web.DesktopSettingsUpdate{DriveLetter: &drvY}); err != nil {
			t.Fatalf("UpdateSettings(Y:): %v", err)
		}
		if err := rig.Mount(); err != nil {
			t.Fatalf("Mount(Y:): %v", err)
		}
		if st := rig.Status(); !st.Mounted || st.DriveLetter != "Y:" {
			t.Fatalf("unexpected mount status: %+v", st)
		}

		drvE := "E:"
		if _, err := rig.UpdateSettings(web.DesktopSettingsUpdate{DriveLetter: &drvE}); err == nil {
			t.Fatal("expected conflict error updating to occupied E:")
		}
		reloaded, err := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if err != nil || reloaded.Get().DriveLetter != "Y:" {
			t.Fatalf("desktop.json DriveLetter altered: %+v, err=%v", reloaded, err)
		}
	})

	t.Run("T3-03_OfflineWritePinLRUEvictionRestartReconnectFlush", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		pinData := bytes.Repeat([]byte("P"), 800)
		u1 := bytes.Repeat([]byte("1"), 800)
		u2 := bytes.Repeat([]byte("2"), 800)
		u3 := bytes.Repeat([]byte("3"), 800)
		uploadSeedFile(t, eng, store, fake, "/pinned/key.dat", pinData)
		uploadSeedFile(t, eng, store, fake, "/u1.bin", u1)
		uploadSeedFile(t, eng, store, fake, "/u2.bin", u2)
		uploadSeedFile(t, eng, store, fake, "/u3.bin", u3)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, 2000, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		_ = mgr.SetPinnedPaths([]string{"/pinned"})
		_, _ = readAllCacheFile(t.Context(), mgr, "/pinned/key.dat", 800)
		_, _ = readAllCacheFile(t.Context(), mgr, "/u1.bin", 800)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/u2.bin", 800)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/u3.bin", 800)

		fake.SetOnline(false)
		if err := mgr.QueueMkdir(t.Context(), "/dirty"); err != nil {
			t.Fatalf("QueueMkdir /dirty: %v", err)
		}
		stPath, err := mgr.StageOpenForWrite("/dirty/note.txt", true)
		if err != nil {
			t.Fatalf("StageOpenForWrite: %v", err)
		}
		dirtyData := []byte("dirty note across eviction and restart")
		_ = os.WriteFile(stPath, dirtyData, 0o600)
		if err := mgr.CommitFile(t.Context(), "/dirty/note.txt", stPath, int64(len(dirtyData)), time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		_ = mgr.Close()

		eng.Cache().Clear()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, 2000, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		_ = mgr2.SetPinnedPaths([]string{"/pinned"})

		if gotPin, err := readAllCacheFile(t.Context(), mgr2, "/pinned/key.dat", 800); err != nil || !bytes.Equal(gotPin, pinData) {
			t.Fatalf("pinned file lost: err=%v", err)
		}
		if gotDirty, err := readAllCacheFile(t.Context(), mgr2, "/dirty/note.txt", int64(len(dirtyData))); err != nil || !bytes.Equal(gotDirty, dirtyData) {
			t.Fatalf("dirty file lost: err=%v", err)
		}
		if _, err := readAllCacheFile(t.Context(), mgr2, "/u1.bin", 800); err == nil {
			t.Fatal("expected /u1.bin evicted by LRU")
		}

		fake.SetOnline(true)
		if err := mgr2.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		_ = eng.Packer().FlushNow(t.Context())
		synced, err := readAllEngineFile(t, eng, store, "/dirty/note.txt")
		if err != nil || !bytes.Equal(synced, dirtyData) {
			t.Fatalf("synced /dirty/note.txt = (%q, %v)", synced, err)
		}
	})

	t.Run("T3-04_RenameOverwriteDirtyCacheAndFIFOOrder", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/a.txt", []byte("A1"))
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/b.txt", []byte("B1"))

		rig.fakeTG.SetOnline(false)
		vfsWriteAll(t, rig.fs, "/a.txt", []byte("A2"))
		if rc := rig.fs.Rename("/a.txt", "/b.txt"); rc != 0 {
			t.Fatalf("Rename rc=%d", rc)
		}
		vfsWriteAll(t, rig.fs, "/c.txt", []byte("C1"))

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/a.txt", &st, 0); rc != -fuse.ENOENT {
			t.Fatalf("Getattr(/a.txt) = %d, want -ENOENT", rc)
		}
		gotB, rc := vfsReadAll(t, rig.fs, "/b.txt")
		if rc != 0 || string(gotB) != "A2" {
			t.Fatalf("offline VFS /b.txt = (%q, rc=%d), want A2", gotB, rc)
		}

		rig.fakeTG.SetOnline(true)
		if err := rig.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if _, err := rig.engine.Store().StatByPath("/a.txt"); err == nil {
			t.Fatal("/a.txt should not exist after flush")
		}
		if syncedB, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/b.txt"); err != nil || string(syncedB) != "A2" {
			t.Fatalf("synced /b.txt = (%q, %v), want A2", syncedB, err)
		}
		if syncedC, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/c.txt"); err != nil || string(syncedC) != "C1" {
			t.Fatalf("synced /c.txt = (%q, %v), want C1", syncedC, err)
		}
	})

	t.Run("T3-05_WebSettingsStartupToggleAndCacheShrink", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		for _, name := range []string{"/keep.bin", "/drop1.bin", "/drop2.bin"} {
			uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, name, bytes.Repeat([]byte("K"), 1024))
			_, _ = readAllCacheFile(t.Context(), rig.cache, name, 1024)
			time.Sleep(10 * time.Millisecond)
		}

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		body := `{"run_at_startup":true,"disk_cache_max_bytes":1500,"pinned_paths":["/keep.bin"]}`
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d", resp.StatusCode)
		}

		enabled, err := rig.startup.IsEnabled()
		if err != nil || !enabled {
			t.Fatalf("StartupManager.IsEnabled = (%v, %v), want true", enabled, err)
		}
		if cs := rig.cache.Stats(); cs.TotalBytes > 1500 {
			t.Fatalf("cache TotalBytes = %d, want <= 1500 after shrink", cs.TotalBytes)
		}
		reloaded, _ := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if !reloaded.Get().RunAtStartup || reloaded.Get().DiskCacheMaxBytes != 1500 {
			t.Fatalf("desktop.json mismatch: %+v", reloaded.Get())
		}
	})

	t.Run("T3-06_TMDBBackupRestoreIsolatesDesktopConfigAndCache", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		drv := "Z:"
		startup := true
		pins := []string{"/Keep"}
		if _, err := rig.UpdateSettings(web.DesktopSettingsUpdate{
			DriveLetter:  &drv,
			RunAtStartup: &startup,
			PinnedPaths:  &pins,
		}); err != nil {
			t.Fatal(err)
		}
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/vault.txt", []byte("vault-payload"))

		doc, err := rig.engine.BackupDBToTelegram(t.Context())
		if err != nil {
			t.Fatalf("BackupDBToTelegram: %v", err)
		}
		node, _ := rig.engine.Store().StatByPath("/vault.txt")
		if err := rig.engine.DeletePermanent(t.Context(), node.ID); err != nil {
			t.Fatal(err)
		}
		if err := rig.engine.RestoreDBFromTelegram(t.Context(), doc.MsgID, "e2e-passphrase"); err != nil {
			t.Fatalf("RestoreDBFromTelegram: %v", err)
		}

		if _, err := rig.engine.Store().StatByPath("/vault.txt"); err != nil {
			t.Fatalf("expected /vault.txt restored in telemount.db: %v", err)
		}
		reloaded, err := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := reloaded.Get(); got.DriveLetter != "Z:" || !got.RunAtStartup || len(got.PinnedPaths) != 1 || got.PinnedPaths[0] != "/Keep" {
			t.Fatalf("machine-local desktop.json affected by .tmdb restore: %+v", got)
		}
	})

	t.Run("T3-07_WebPinUpdatePinSyncAndClearUnpinned", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		spec := []byte("design-spec-v1")
		junk := []byte("temporary-junk-blob")
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/docs/spec.md", spec)
		uploadSeedFile(t, rig.engine, rig.engine.Store(), rig.fakeTG, "/tmp/junk.bin", junk)
		_, _ = readAllCacheFile(t.Context(), rig.cache, "/tmp/junk.bin", int64(len(junk)))

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(`{"pinned_paths":["/docs"]}`))
		req.Header.Set("Content-Type", "application/json")
		r1, err := client.Do(req)
		if err != nil || r1.StatusCode != http.StatusOK {
			t.Fatalf("PUT settings failed: %v / %v", r1, err)
		}
		r1.Body.Close()

		r2, err := client.Post(ts.URL+"/api/desktop/cache/clear", "application/json", strings.NewReader("{}"))
		if err != nil || r2.StatusCode != http.StatusOK {
			t.Fatalf("POST cache/clear failed: %v / %v", r2, err)
		}
		r2.Body.Close()

		rig.fakeTG.SetOnline(false)
		rig.engine.Cache().Clear()
		if got, err := readAllCacheFile(t.Context(), rig.cache, "/docs/spec.md", int64(len(spec))); err != nil || !bytes.Equal(got, spec) {
			t.Fatalf("pinned /docs/spec.md not readable offline: err=%v", err)
		}
		if _, err := readAllCacheFile(t.Context(), rig.cache, "/tmp/junk.bin", int64(len(junk))); err == nil {
			t.Fatal("expected unpinned /tmp/junk.bin cleared")
		}
	})

	t.Run("T3-08_OfflineVFSWritesAndWebQueueFlush", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), false, true)
		s, _ := rig.engine.Store().GetSettings()
		s.SmallFileMaxMB = 0
		_ = rig.engine.Store().SaveSettings(s)

		vfsWriteAll(t, rig.fs, "/w1.txt", []byte("one"))
		vfsWriteAll(t, rig.fs, "/w2.txt", []byte("two"))
		vfsWriteAll(t, rig.fs, "/w3.txt", []byte("three"))

		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		r1, _ := client.Get(ts.URL + "/api/desktop/status")
		var st1 web.DesktopStatus
		_ = json.NewDecoder(r1.Body).Decode(&st1)
		r1.Body.Close()
		if st1.QueuePendingCount != 3 {
			t.Fatalf("QueuePendingCount = %d, want 3", st1.QueuePendingCount)
		}

		rig.fakeTG.SetOnline(true)
		r2, err := client.Post(ts.URL+"/api/desktop/queue/flush", "application/json", strings.NewReader("{}"))
		if err != nil || r2.StatusCode != http.StatusOK {
			t.Fatalf("POST queue/flush failed: %v / %v", r2, err)
		}
		r2.Body.Close()

		r3, _ := client.Get(ts.URL + "/api/desktop/status")
		var st3 web.DesktopStatus
		_ = json.NewDecoder(r3.Body).Decode(&st3)
		r3.Body.Close()
		if st3.QueuePendingCount != 0 || rig.fakeTG.Uploads() != 3 {
			t.Fatalf("after flush: pending=%d uploads=%d, want 0 and 3", st3.QueuePendingCount, rig.fakeTG.Uploads())
		}
	})

	t.Run("T3-09_TrayUnmountAndCleanQuitFlushesQueue", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		actions := &rigTrayActions{rig: rig}
		if err := actions.Mount(); err != nil {
			t.Fatal(err)
		}
		rig.fakeTG.SetOnline(false)
		vfsWriteAll(t, rig.fs, "/tray-dirty.txt", []byte("dirty-before-quit"))

		rig.fakeTG.SetOnline(true)
		if err := actions.Unmount(); err != nil {
			t.Fatal(err)
		}
		actions.Quit()
		if actions.Status().Mounted || rig.cache.QueueStatus().PendingCount != 0 {
			t.Fatalf("expected unmounted and queue drained after Quit: %+v", actions.Status())
		}
	})

	t.Run("T3-10_VFSReadsDriveLRURecency", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		a := bytes.Repeat([]byte("A"), 1000)
		b := bytes.Repeat([]byte("B"), 1000)
		c := bytes.Repeat([]byte("C"), 1000)
		d := bytes.Repeat([]byte("D"), 1000)
		for name, data := range map[string][]byte{"/a.bin": a, "/b.bin": b, "/c.bin": c, "/d.bin": d} {
			uploadSeedFile(t, eng, store, fake, name, data)
		}
		cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 3200, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		fs := vfs.NewTeleFS(store, cache)

		for _, p := range []string{"/a.bin", "/b.bin", "/c.bin"} {
			time.Sleep(15 * time.Millisecond)
			if _, rc := vfsReadAll(t, fs, p); rc != 0 {
				t.Fatalf("vfsReadAll(%s) rc=%d", p, rc)
			}
		}
		// Refresh /a.bin at non-zero offset via TeleFS.Read.
		time.Sleep(15 * time.Millisecond)
		rc, fh := fs.Open("/a.bin", fuse.O_RDONLY)
		if rc != 0 {
			t.Fatalf("Open(/a.bin) rc=%d", rc)
		}
		buf := make([]byte, 64)
		if n := fs.Read("/a.bin", buf, 100, fh); n != 64 {
			t.Fatalf("Read(/a.bin, off=100) = %d", n)
		}
		_ = fs.Release("/a.bin", fh)

		time.Sleep(15 * time.Millisecond)
		if _, rc := vfsReadAll(t, fs, "/d.bin"); rc != 0 {
			t.Fatalf("vfsReadAll(/d.bin) rc=%d", rc)
		}

		fake.SetOnline(false)
		eng.Cache().Clear()
		if gotA, rc := vfsReadAll(t, fs, "/a.bin"); rc != 0 || !bytes.Equal(gotA, a) {
			t.Fatalf("refreshed /a.bin should remain cached offline: rc=%d", rc)
		}
		if _, rc := vfsReadAll(t, fs, "/b.bin"); rc == 0 {
			t.Fatal("expected LRU /b.bin evicted and unreadable offline")
		}
	})

	t.Run("T3-11_TrayToggleStartupPersistsToStoreAndConfig", func(t *testing.T) {
		dir := t.TempDir()
		rig := newTestDesktopRig(t, dir, true, true)
		actions := &rigTrayActions{rig: rig}

		on1, err := actions.ToggleStartup()
		if err != nil || !on1 {
			t.Fatalf("toggle 1 = (%v, %v)", on1, err)
		}
		s1, _ := rig.startup.IsEnabled()
		c1, _ := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if !s1 || !c1.Get().RunAtStartup {
			t.Fatalf("expected both store (%v) and config (%v) true", s1, c1.Get().RunAtStartup)
		}

		on2, err := actions.ToggleStartup()
		if err != nil || on2 {
			t.Fatalf("toggle 2 = (%v, %v)", on2, err)
		}
		s2, _ := rig.startup.IsEnabled()
		c2, _ := desktop.LoadConfig(filepath.Join(dir, "desktop.json"), dir)
		if s2 || c2.Get().RunAtStartup {
			t.Fatalf("expected both store (%v) and config (%v) false", s2, c2.Get().RunAtStartup)
		}
	})

	t.Run("T3-12_PinnedFolderCaseInsensitiveVFSOfflineEdit", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		_ = rig.cache.SetPinnedPaths([]string{"/Work"})
		_ = rig.fs.Mkdir("/Work", 0o755)
		vfsWriteAll(t, rig.fs, "/Work/Notes.txt", []byte("initial-notes"))

		rig.fakeTG.SetOnline(false)
		rc, fh := rig.fs.Open("/work/notes.txt", fuse.O_RDWR|fuse.O_TRUNC)
		if rc != 0 {
			t.Fatalf("case-insensitive Open(/work/notes.txt) rc=%d", rc)
		}
		edited := []byte("edited-case-insensitive")
		if n := rig.fs.Write("/work/notes.txt", edited, 0, fh); n != len(edited) {
			t.Fatalf("Write = %d", n)
		}
		_ = rig.fs.Flush("/work/notes.txt", fh)
		_ = rig.fs.Release("/work/notes.txt", fh)

		got, rc := vfsReadAll(t, rig.fs, "/Work/Notes.txt")
		if rc != 0 || !bytes.Equal(got, edited) {
			t.Fatalf("Read(/Work/Notes.txt) = (%q, rc=%d), want %q", got, rc, edited)
		}

		rig.fakeTG.SetOnline(true)
		if err := rig.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		synced, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/Work/Notes.txt")
		if err != nil || !bytes.Equal(synced, edited) {
			t.Fatalf("synced = (%q, %v), want %q", synced, err, edited)
		}
	})

	t.Run("T3-13_WinFspInstalledMidSessionAllowsWebMount", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		r1, _ := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		r1.Body.Close()
		if r1.StatusCode < 400 {
			t.Fatalf("first mount should fail when WinFsp missing, got %d", r1.StatusCode)
		}

		rig.probe.Set(desktop.WinFspStatus{Installed: true, DLLPath: `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`})
		r2, err := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("second mount status = %d, want 200", r2.StatusCode)
		}
		if st := rig.Status(); !st.Mounted || !st.WinFspInstalled {
			t.Fatalf("unexpected status after mid-session install: %+v", st)
		}
	})

	t.Run("T3-14_WebSetupWizardAndDesktopControllerCoexistence", func(t *testing.T) {
		freshRig := newTestDesktopRigFreshAdmin(t, t.TempDir())
		srv := web.NewServer(freshRig.engine, freshRig.tgMgr)
		srv.SetDesktopController(freshRig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}

		preResp, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatal(err)
		}
		preResp.Body.Close()
		if preResp.StatusCode != http.StatusForbidden {
			t.Fatalf("pre-admin GET /api/desktop/status = %d, want 403", preResp.StatusCode)
		}

		setupResp, err := client.Post(ts.URL+"/api/setup/admin", "application/json", strings.NewReader(`{"username":"admin","password":"webpassword123"}`))
		if err != nil {
			t.Fatal(err)
		}
		setupResp.Body.Close()
		if setupResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/setup/admin = %d, want 200", setupResp.StatusCode)
		}

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(`{"drive_letter":"X:"}`))
		req.Header.Set("Content-Type", "application/json")
		putResp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		putResp.Body.Close()
		if putResp.StatusCode != http.StatusOK {
			t.Fatalf("post-setup PUT /api/desktop/settings = %d, want 200", putResp.StatusCode)
		}
	})

	t.Run("T3-15_VFSTruncateAndUtimensOfflineRestartAndSync", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/edit.txt", []byte("0123456789"))

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		fs := vfs.NewTeleFS(store, mgr)

		// Cache /edit.txt locally while online before editing offline.
		if _, rc := vfsReadAll(t, fs, "/edit.txt"); rc != 0 {
			t.Fatalf("seed read /edit.txt rc=%d", rc)
		}

		fake.SetOnline(false)
		rc, fh := fs.Open("/edit.txt", fuse.O_RDWR)
		if rc != 0 {
			t.Fatalf("Open rc=%d", rc)
		}
		if rc := fs.Truncate("/edit.txt", 4, fh); rc != 0 {
			t.Fatalf("Truncate rc=%d", rc)
		}
		_ = fs.Flush("/edit.txt", fh)
		_ = fs.Release("/edit.txt", fh)
		const wantSec int64 = 1720000000
		if rc := fs.Utimens("/edit.txt", []fuse.Timespec{{Sec: wantSec}, {Sec: wantSec}}); rc != 0 {
			t.Fatalf("Utimens rc=%d", rc)
		}
		_ = mgr.Close()

		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		fs2 := vfs.NewTeleFS(store, mgr2)

		var st fuse.Stat_t
		if rc := fs2.Getattr("/edit.txt", &st, 0); rc != 0 || st.Size != 4 || st.Mtim.Sec != wantSec {
			t.Fatalf("post-restart Getattr = (rc=%d, size=%d, mtim=%d), want (0, 4, %d)", rc, st.Size, st.Mtim.Sec, wantSec)
		}
		got, rc := vfsReadAll(t, fs2, "/edit.txt")
		if rc != 0 || string(got) != "0123" {
			t.Fatalf("post-restart Read = (%q, rc=%d), want 0123", got, rc)
		}

		fake.SetOnline(true)
		if err := mgr2.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		_ = eng.Packer().FlushNow(t.Context())
		synced, err := readAllEngineFile(t, eng, store, "/edit.txt")
		if err != nil || string(synced) != "0123" {
			t.Fatalf("synced Read = (%q, %v), want 0123", synced, err)
		}
	})

	t.Run("T3-16_LiveDriveLetterChangeViaWebUIAndRemount", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		r1, _ := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		r1.Body.Close()

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(`{"drive_letter":"W:"}`))
		req.Header.Set("Content-Type", "application/json")
		r2, _ := client.Do(req)
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("PUT drive_letter=W: status = %d", r2.StatusCode)
		}

		r3, _ := client.Post(ts.URL+"/api/desktop/unmount", "application/json", strings.NewReader("{}"))
		r3.Body.Close()
		r4, _ := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		r4.Body.Close()
		if st := rig.Status(); !st.Mounted || st.DriveLetter != "W:" {
			t.Fatalf("expected remount on W:, got %+v", st)
		}
	})
}
