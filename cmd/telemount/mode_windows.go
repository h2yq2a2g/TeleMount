//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"telemount/internal/db"
	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/state"
	"telemount/internal/storage"
	"telemount/internal/tg"
	"telemount/internal/tray"
	"telemount/internal/version"
	"telemount/internal/vfs"
	"telemount/internal/web"
)

type desktopIPCInfo struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Drive string `json:"drive"`
}

func desktopIPCFilePath(dataDir string) string {
	return filepath.Join(dataDir, "desktop_ipc.json")
}

func generateIPCToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeDesktopIPCFile(dataDir string, info desktopIPCInfo) {
	b, err := json.Marshal(info)
	if err != nil {
		return
	}
	_ = os.WriteFile(desktopIPCFilePath(dataDir), b, 0o600)
}

func runShellAction(dataDir, action, rawPath string) {
	raw, err := os.ReadFile(desktopIPCFilePath(dataDir))
	if err != nil {
		log.Fatalf("TeleMount is not running or IPC unavailable: %v", err)
	}
	var ipc desktopIPCInfo
	if err := json.Unmarshal(raw, &ipc); err != nil || ipc.URL == "" || ipc.Token == "" {
		log.Fatalf("invalid TeleMount IPC state")
	}
	vpath, err := desktop.MapWindowsPathToVirtual(rawPath, ipc.Drive)
	if err != nil {
		log.Fatalf("invalid target path %q: %v", rawPath, err)
	}
	body, _ := json.Marshal(map[string]string{
		"action": strings.ToLower(strings.TrimSpace(action)),
		"path":   vpath,
	})
	req, err := http.NewRequest(http.MethodPost, ipc.URL+"/api/desktop/shell-action", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("build shell action request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TeleMount-IPC-Token", ipc.Token)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("send shell action: %v", err)
	}
	defer resp.Body.Close()
}

func defaultRuntimePaths() (dataDir, tmpDir, listenAddr string) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, "AppData", "Local")
		} else {
			base = "."
		}
	}
	root := filepath.Join(base, "TeleMount")
	return root, filepath.Join(root, "tmp"), desktop.DefaultWebAddr
}

func platformEarlyExit(headless bool) bool {
	if headless {
		return false
	}
	exePath, _ := os.Executable()
	// Release Windows builds require installation via TeleMount-Setup.exe (no portable mode).
	if version.Version != "dev" && exePath != "" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(exePath), "unins000.exe")); err != nil {
			_ = tray.NewDialogNotifier().ShowWarningDialog(
				"TeleMount — Installation Required",
				"TeleMount does not support portable execution.\nPlease install using TeleMount-Setup.exe for WinFsp, Explorer integration, and auto-updates.",
			)
			return true
		}
	}
	// WinFsp drives mounted by an elevated (Admin) process are invisible to standard File Explorer.
	// Automatically drop elevation back to Explorer's non-elevated user token before opening DB/locks.
	if relaunched, derr := desktop.RelaunchNonElevatedIfNeeded(exePath, os.Args[1:]); derr != nil {
		log.Printf("de-elevate warning: %v", derr)
	} else if relaunched {
		return true
	}
	return false
}

func runPlatform(
	appCtx context.Context,
	appCancel context.CancelFunc,
	dataDir, listenAddr string,
	headless, minimized bool,
	store *db.Store,
	st *state.Manager,
	tgMgr *tg.Manager,
	engine *storage.Engine,
	srv *web.Server,
) {
	if headless {
		runHeadlessWindows(appCancel, dataDir, listenAddr, engine, srv)
		return
	}

	exePath, _ := os.Executable()

	cfgStore, err := desktop.LoadConfig("", dataDir)
	if err != nil {
		log.Fatalf("load desktop config: %v", err)
	}
	cfg := cfgStore.Get()

	// Respect LISTEN_ADDR env override if explicitly set; otherwise use persisted WebAddr (default 127.0.0.1:8080).
	bindAddr := cfg.WebAddr
	if os.Getenv("LISTEN_ADDR") != "" {
		bindAddr = listenAddr
	}

	cache, err := diskcache.Open(
		cfg.DiskCacheDir,
		filepath.Join(dataDir, "desktop_cache.db"),
		cfg.DiskCacheMaxBytes,
		store,
		engine,
	)
	if err != nil {
		log.Fatalf("open persistent disk cache: %v", err)
	}
	defer cache.Close()

	if len(cfg.PinnedPaths) > 0 {
		_ = cache.SetPinnedPaths(cfg.PinnedPaths)
	}
	cache.StartSyncLoop(appCtx, 5*time.Second)

	probe := desktop.NewWinFspProbe()
	driveChecker := desktop.NewDriveChecker()
	startupMgr := desktop.NewStartupManager(nil, "TeleMount")

	tfs := vfs.NewTeleFS(store, cache)
	mountMgr := vfs.NewMountManager(tfs, probe, driveChecker)

	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		// Fall back to an ephemeral loopback port if default 127.0.0.1:8080 is already taken.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("listen loopback web ui: %v", err)
		}
	}
	webURL := "http://" + ln.Addr().String()

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ipcToken := generateIPCToken()
	srv.SetDesktopIPCToken(ipcToken)

	overlaySrv, oerr := desktop.StartOverlayServer(func(vp string) byte {
		return byte(cache.OverlayState(vp))
	})
	if oerr != nil {
		log.Printf("shell overlay server warning: %v", oerr)
	} else {
		cache.SetOnChange(overlaySrv.NotifyPaths)
	}

	rt := &desktopRuntime{
		appCancel:    appCancel,
		dataDir:      dataDir,
		exePath:      exePath,
		webURL:       webURL,
		ipcToken:     ipcToken,
		st:           st,
		cfgStore:     cfgStore,
		cache:        cache,
		mountMgr:     mountMgr,
		probe:        probe,
		driveChecker: driveChecker,
		startupMgr:   startupMgr,
		overlaySrv:   overlaySrv,
		engine:       engine,
		tgMgr:        tgMgr,
		httpSrv:      httpSrv,
	}
	srv.SetDesktopController(rt)
	rt.syncShellIntegration(cfg.DriveLetter)
	defer os.Remove(desktopIPCFilePath(dataDir))

	trayApp, err := tray.New(&trayActionsAdapter{rt: rt})
	if err != nil {
		log.Fatalf("init system tray: %v", err)
	}
	rt.setTray(trayApp)

	go func() {
		log.Printf("TeleMount desktop listening on %s (data: %s, cache: %s)", webURL, dataDir, cfg.DiskCacheDir)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server error: %v", err)
		}
	}()

	// Handle OS termination signals gracefully alongside Tray Quit.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sigCh:
			rt.Quit()
			_ = trayApp.Close()
		case <-appCtx.Done():
		}
	}()

	// Post-startup mount, WinFsp probe notification, and initial Web UI launch.
	go func() {
		time.Sleep(350 * time.Millisecond)
		stTray := rt.TrayStatus()
		if !tray.CheckAndNotifyWinFsp(stTray, trayApp) {
			log.Printf("WinFsp not installed: %s", stTray.WinFspGuidance)
		} else if cfgStore.Get().AutoMount {
			if err := rt.Mount(); err != nil {
				log.Printf("auto-mount warning: %v", err)
			}
		}
		_ = trayApp.Refresh()

		if !minimized && !st.Snapshot().SetupCompleted {
			_ = rt.OpenWebUI()
		}

		// Background GitHub Releases auto-update check (15s after launch, then every 6 hours).
		go rt.runAutoUpdateLoop(appCtx)

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				_ = trayApp.Refresh()
			}
		}
	}()

	if err := trayApp.Run(appCtx); err != nil {
		log.Printf("system tray exited with error: %v", err)
	}
	rt.shutdownOnce()
}

func runHeadlessWindows(
	appCancel context.CancelFunc,
	dataDir, listenAddr string,
	engine *storage.Engine,
	srv *web.Server,
) {
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		shutCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		if engine.IsReady() {
			_ = engine.Packer().FlushNow(shutCtx)
		}
		appCancel()
	}()

	log.Printf("TeleMount headless listening on %s (Web UI: /, WebDAV: /dav/, data: %s)", listenAddr, dataDir)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}

type desktopRuntime struct {
	appCancel    context.CancelFunc
	dataDir      string
	exePath      string
	webURL       string
	ipcToken     string
	st           *state.Manager
	cfgStore     *desktop.ConfigStore
	cache        *diskcache.Manager
	mountMgr     *vfs.MountManager
	probe        desktop.WinFspProbe
	driveChecker desktop.DriveChecker
	startupMgr   *desktop.StartupManager
	overlaySrv   *desktop.OverlayServer
	engine       *storage.Engine
	tgMgr        *tg.Manager
	httpSrv      *http.Server

	trayMu   sync.RWMutex
	trayApp  *tray.App
	quitOnce sync.Once

	updateMu       sync.Mutex
	updateInfo     *desktop.UpdateInfo
	updateErr      string
	updating       bool
	promptedVer    string
}

func (r *desktopRuntime) lang() string {
	if r.st != nil {
		return r.st.Language()
	}
	return "en"
}

func (r *desktopRuntime) tr(en, vi string) string {
	if r.lang() == "vi" {
		return vi
	}
	return en
}

func (r *desktopRuntime) OnLanguageChange(_ string) {
	r.syncShellIntegration(r.cfgStore.Get().DriveLetter)
	r.refreshTray()
}

func (r *desktopRuntime) syncShellIntegration(driveLetter string) {
	writeDesktopIPCFile(r.dataDir, desktopIPCInfo{
		URL:   r.webURL,
		Token: r.ipcToken,
		Drive: driveLetter,
	})
	if r.exePath != "" {
		if err := desktop.RegisterShellMenu(r.exePath, driveLetter, r.lang()); err != nil {
			log.Printf("register explorer shell menu warning: %v", err)
		}
	}
}

func (r *desktopRuntime) notifyTray(title, msg string, isErr bool) {
	r.trayMu.RLock()
	app := r.trayApp
	r.trayMu.RUnlock()
	if app != nil {
		_ = app.Notify(title, msg, isErr)
	}
}

func (r *desktopRuntime) setTray(app *tray.App) {
	r.trayMu.Lock()
	r.trayApp = app
	r.trayMu.Unlock()
}

func (r *desktopRuntime) refreshTray() {
	r.trayMu.RLock()
	app := r.trayApp
	r.trayMu.RUnlock()
	if app != nil {
		_ = app.Refresh()
	}
}

// web.DesktopController implementation:
func (r *desktopRuntime) Status() web.DesktopStatus {
	cfg := r.cfgStore.Get()
	wf := r.probe.Check()
	mounted, activeDrive, mountErr := r.mountMgr.Status()
	drive := cfg.DriveLetter
	mountedDrive := ""
	if mounted && activeDrive != "" {
		drive = activeDrive
		mountedDrive = activeDrive
	}
	cs := r.cache.Stats()
	qs := r.cache.QueueStatus()
	runAtStartup := cfg.RunAtStartup
	if enabled, err := r.startupMgr.IsEnabled(); err == nil {
		runAtStartup = enabled
	}

	r.updateMu.Lock()
	uInfo := r.updateInfo
	uErr := r.updateErr
	r.updateMu.Unlock()

	var updAvail bool
	var latestVer, relURL string
	if uInfo != nil {
		updAvail = uInfo.Available
		latestVer = uInfo.LatestVersion
		relURL = uInfo.ReleaseURL
	}

	return web.DesktopStatus{
		Enabled:              true,
		AppVersion:           version.Version,
		WinFspInstalled:      wf.Installed,
		WinFspGuidance:       wf.Guidance,
		DriveLetter:          drive,
		AvailableDrives:      desktop.ListAvailableDrives(r.driveChecker, mountedDrive),
		UsedDrives:           desktop.ListUsedDrives(r.driveChecker, mountedDrive),
		Mounted:              mounted,
		MountError:           mountErr,
		RunAtStartup:         runAtStartup,
		AutoMount:            cfg.AutoMount,
		AutoUpdate:           cfg.AutoUpdate,
		UpdateAvailable:      updAvail,
		LatestVersion:        latestVer,
		ReleaseURL:           relURL,
		UpdateError:          uErr,
		DiskCacheMaxBytes:    cfg.DiskCacheMaxBytes,
		DiskCacheBytes:       cs.TotalBytes,
		DiskCachePinnedBytes: cs.PinnedBytes,
		DiskCacheEntries:     cs.EntryCount,
		PinnedPaths:          r.cache.PinnedPaths(),
		QueuePendingCount:    qs.PendingCount,
		QueuePendingBytes:    qs.PendingBytes,
		QueueSyncing:         qs.Syncing,
		QueueLastError:       qs.LastError,
	}
}

func (r *desktopRuntime) UpdateSettings(u web.DesktopSettingsUpdate) (web.DesktopStatus, error) {
	wasMounted, oldDrive, _ := r.mountMgr.Status()
	oldCfg := r.cfgStore.Get()

	updated, err := r.cfgStore.Update(func(c *desktop.Config) error {
		if u.DriveLetter != nil {
			norm, err := desktop.NormalizeDriveLetter(*u.DriveLetter)
			if err != nil {
				return err
			}
			if !wasMounted || norm != oldDrive {
				inUse, err := r.driveChecker.IsDriveInUse(norm)
				if err != nil {
					return err
				}
				if inUse {
					return fmt.Errorf("drive letter %s is already in use", norm)
				}
			}
			c.DriveLetter = norm
		}
		if u.DiskCacheMaxBytes != nil {
			c.DiskCacheMaxBytes = *u.DiskCacheMaxBytes
		}
		if u.PinnedPaths != nil {
			c.PinnedPaths = append([]string(nil), (*u.PinnedPaths)...)
		}
		if u.RunAtStartup != nil {
			c.RunAtStartup = *u.RunAtStartup
		}
		if u.AutoMount != nil {
			c.AutoMount = *u.AutoMount
		}
		if u.AutoUpdate != nil {
			c.AutoUpdate = *u.AutoUpdate
		}
		return desktop.ValidateConfig(*c)
	})
	if err != nil {
		return r.Status(), err
	}

	if u.DiskCacheMaxBytes != nil {
		if err := r.cache.SetMaxBytes(updated.DiskCacheMaxBytes); err != nil {
			return r.Status(), err
		}
	}

	if u.PinnedPaths != nil {
		if err := r.cache.SetPinnedPaths(updated.PinnedPaths); err != nil {
			return r.Status(), err
		}
		if r.engine.IsReady() {
			go func(paths []string) {
				for _, p := range paths {
					_ = r.cache.PinSync(context.Background(), p)
				}
			}(append([]string(nil), updated.PinnedPaths...))
		}
	}

	if u.RunAtStartup != nil {
		if *u.RunAtStartup {
			if err := r.startupMgr.Enable(r.exePath, "--minimized"); err != nil {
				return r.Status(), err
			}
		} else {
			if err := r.startupMgr.Disable(); err != nil {
				return r.Status(), err
			}
		}
	}

	// If drive letter changed, update Explorer context menu filter & IPC state, and remount if active.
	if u.DriveLetter != nil && updated.DriveLetter != oldCfg.DriveLetter {
		r.syncShellIntegration(updated.DriveLetter)
		if wasMounted {
			r.overlaySrv.SetDrive("")
			_ = r.mountMgr.Unmount(context.Background())
			if err := r.mountMgr.Mount(updated.DriveLetter); err != nil {
				return r.Status(), err
			}
			r.overlaySrv.SetDrive(updated.DriveLetter)
		}
	}

	r.refreshTray()
	return r.Status(), nil
}

func (r *desktopRuntime) HandleShellAction(ctx context.Context, action, rawPath string) (web.ShellActionResult, error) {
	vpath := desktop.NormalizeVirtualPath(rawPath)
	switch action {
	case "pin":
		cfg := r.cfgStore.Get()
		pins := append([]string(nil), cfg.PinnedPaths...)
		if !desktop.IsPathPinned(pins, vpath) {
			pins = append(pins, vpath)
			if _, err := r.UpdateSettings(web.DesktopSettingsUpdate{PinnedPaths: &pins}); err != nil {
				r.notifyTray(r.tr("TeleMount — Pin failed", "TeleMount — Lỗi ghim offline"), err.Error(), true)
				return web.ShellActionResult{}, err
			}
		}
		if r.engine.IsReady() {
			r.notifyTray(
				r.tr("TeleMount — Always keep on device", "TeleMount — Giữ offline"),
				fmt.Sprintf(r.tr("Downloading for offline access: %s", "Đang tải lưu offline: %s"), vpath),
				false,
			)
			go func() {
				if err := r.cache.PinSync(context.Background(), vpath); err != nil {
					r.notifyTray(r.tr("TeleMount — Offline download failed", "TeleMount — Lỗi tải offline"), fmt.Sprintf("%s: %v", vpath, err), true)
					return
				}
				ps, _ := r.cache.PathStatus(vpath)
				r.notifyTray(
					r.tr("TeleMount — Kept on this device", "TeleMount — Đã giữ trên máy"),
					fmt.Sprintf(r.tr("%s (%d files · %s)", "%s (%d tệp · %s)"), vpath, ps.CachedFiles, fmtHumanBytes(ps.CachedBytes)),
					false,
				)
				r.refreshTray()
			}()
		} else {
			r.notifyTray(
				r.tr("TeleMount — Pinned for offline", "TeleMount — Đã ghim offline"),
				fmt.Sprintf(r.tr("%s (will download when Telegram is online)", "%s (sẽ tự tải khi Telegram online)"), vpath),
				false,
			)
		}
		return web.ShellActionResult{OK: true, Message: "pinned " + vpath}, nil

	case "free":
		// Remove exact path and any nested child pins under this folder.
		targetLower := strings.ToLower(vpath)
		updated, err := r.cfgStore.Update(func(c *desktop.Config) error {
			kept := make([]string, 0, len(c.PinnedPaths))
			for _, p := range c.PinnedPaths {
				normP := desktop.NormalizeVirtualPath(p)
				lowP := strings.ToLower(normP)
				if vpath == "/" || lowP == targetLower || strings.HasPrefix(lowP, targetLower+"/") {
					continue
				}
				kept = append(kept, normP)
			}
			c.PinnedPaths = kept
			return nil
		})
		if err != nil {
			r.notifyTray(r.tr("TeleMount — Free up space failed", "TeleMount — Lỗi giải phóng"), err.Error(), true)
			return web.ShellActionResult{}, err
		}
		_ = r.cache.SetPinnedPaths(updated.PinnedPaths)

		if desktop.IsPathPinned(updated.PinnedPaths, vpath) {
			msg := fmt.Sprintf(
				r.tr("A parent folder of %s is pinned offline — run Free up space on the parent folder.", "Thư mục cha của %s đang được ghim offline — hãy chọn Giải phóng dung lượng trên thư mục cha."),
				vpath,
			)
			r.notifyTray(r.tr("TeleMount — Pinned via parent folder", "TeleMount — Đang bị ghim từ thư mục cha"), msg, true)
			return web.ShellActionResult{OK: false, Message: msg}, nil
		}

		freed, evicted, skippedDirty, err := r.cache.EvictPath(vpath)
		if err != nil {
			r.notifyTray(r.tr("TeleMount — Free up space failed", "TeleMount — Lỗi giải phóng"), err.Error(), true)
			return web.ShellActionResult{}, err
		}
		var msg string
		switch {
		case evicted > 0 && skippedDirty > 0:
			msg = fmt.Sprintf(
				r.tr("Freed %s (%d files) locally; kept on Cloud. Preserved %d unsynced files.", "Đã giải phóng %s (%d tệp) khỏi máy, vẫn giữ trên Cloud. Giữ lại %d tệp chưa đồng bộ."),
				fmtHumanBytes(freed), evicted, skippedDirty,
			)
		case evicted > 0:
			msg = fmt.Sprintf(
				r.tr("Freed %s (%d files) from this device — safely kept on Telegram Cloud.", "Đã giải phóng %s (%d tệp) khỏi máy — dữ liệu vẫn giữ an toàn trên Telegram Cloud."),
				fmtHumanBytes(freed), evicted,
			)
		case skippedDirty > 0:
			msg = fmt.Sprintf(
				r.tr("Cannot free %s while %d files are pending upload to Telegram.", "Chưa thể giải phóng %s vì còn %d tệp chưa đồng bộ lên Telegram."),
				vpath, skippedDirty,
			)
		default:
			msg = fmt.Sprintf(
				r.tr("%s is already cloud-only (0 B on this device).", "%s hiện chỉ nằm trên Cloud (0 B trên máy)."),
				vpath,
			)
		}
		r.notifyTray(r.tr("TeleMount — Free up space", "TeleMount — Giải phóng dung lượng"), msg, skippedDirty > 0 && evicted == 0)
		r.refreshTray()
		return web.ShellActionResult{OK: true, Message: msg}, nil

	case "sync":
		if err := r.FlushQueue(ctx); err != nil {
			r.notifyTray(r.tr("TeleMount — Sync failed", "TeleMount — Lỗi đồng bộ"), err.Error(), true)
			return web.ShellActionResult{}, err
		}
		if r.engine.IsReady() && desktop.IsPathPinned(r.cfgStore.Get().PinnedPaths, vpath) {
			go func() {
				_ = r.cache.PinSync(context.Background(), vpath)
				r.refreshTray()
			}()
		}
		msg := fmt.Sprintf(r.tr("Synced: %s", "Đã đồng bộ xong: %s"), vpath)
		r.notifyTray(r.tr("TeleMount — Sync now", "TeleMount — Đồng bộ ngay"), msg, false)
		return web.ShellActionResult{OK: true, Message: msg}, nil

	default:
		return web.ShellActionResult{}, fmt.Errorf("unknown shell action %q", action)
	}
}

func fmtHumanBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(b) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func (r *desktopRuntime) Mount() error {
	drv := r.cfgStore.Get().DriveLetter
	err := r.mountMgr.Mount(drv)
	if err == nil {
		r.overlaySrv.SetDrive(drv)
	}
	r.refreshTray()
	return err
}

func (r *desktopRuntime) Unmount() error {
	r.overlaySrv.SetDrive("")
	err := r.mountMgr.Unmount(context.Background())
	r.refreshTray()
	return err
}

func (r *desktopRuntime) OpenDrive() error {
	mounted, drv, _ := r.mountMgr.Status()
	if !mounted {
		return errors.New(r.tr("virtual drive is not mounted", "ổ đĩa ảo chưa được mount"))
	}
	return tray.OpenDrivePath(drv)
}

func (r *desktopRuntime) FlushQueue(ctx context.Context) error {
	err := r.cache.FlushQueue(ctx)
	if err == nil && r.engine.IsReady() {
		_ = r.engine.Packer().FlushNow(ctx)
	}
	r.refreshTray()
	return err
}

func (r *desktopRuntime) ClearUnpinnedCache() error {
	err := r.cache.ClearUnpinned()
	r.refreshTray()
	return err
}

// tray.TrayActions implementation (reuses Mount/Unmount/OpenDrive above):
func (r *desktopRuntime) TrayStatus() tray.TrayStatus {
	st := r.Status()
	return tray.TrayStatus{
		Mounted:         st.Mounted,
		DriveLetter:     st.DriveLetter,
		WinFspInstalled: st.WinFspInstalled,
		WinFspGuidance:  st.WinFspGuidance,
		RunAtStartup:    st.RunAtStartup,
		QueuePending:    st.QueuePendingCount,
		Online:          r.engine.IsReady(),
		UpdateAvailable: st.UpdateAvailable,
		LatestVersion:   st.LatestVersion,
		Language:        r.lang(),
	}
}

func (r *desktopRuntime) runAutoUpdateLoop(ctx context.Context) {
	select {
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
		return
	}
	r.autoCheckOnce(ctx)

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.autoCheckOnce(ctx)
		}
	}
}

func (r *desktopRuntime) autoCheckOnce(ctx context.Context) {
	if _, err := r.CheckUpdate(ctx); err != nil {
		return
	}
	r.updateMu.Lock()
	info := r.updateInfo
	alreadyPrompted := info != nil && r.promptedVer == info.LatestVersion
	r.updateMu.Unlock()
	if info == nil || !info.Available || alreadyPrompted {
		return
	}

	// Pre-download and verify SHA-256 in the background so installation is instant.
	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := desktop.DownloadAndVerifyInstaller(dlCtx, nil, filepath.Join(r.dataDir, "updates"), info); err != nil {
		r.updateMu.Lock()
		r.updateErr = err.Error()
		r.updateMu.Unlock()
		return
	}

	r.updateMu.Lock()
	r.promptedVer = info.LatestVersion
	r.updateMu.Unlock()
	r.refreshTray()

	if r.cfgStore.Get().AutoUpdate {
		r.notifyTray(
			r.tr("TeleMount — Auto-Update", "TeleMount — Tự động cập nhật"),
			fmt.Sprintf(r.tr("Updating to version %s...", "Đang cập nhật lên phiên bản %s..."), info.LatestVersion),
			false,
		)
		if err := r.ApplyUpdate(ctx); err != nil && !errors.Is(err, desktop.ErrUpdateCanceled) {
			r.notifyTray(r.tr("TeleMount — Update failed", "TeleMount — Lỗi cập nhật"), err.Error(), true)
		}
	} else {
		r.notifyTray(
			r.tr("TeleMount — Update available", "TeleMount — Có bản cập nhật mới"),
			fmt.Sprintf(r.tr("Version %s is ready (current: %s).", "Phiên bản %s đã sẵn sàng (đang dùng %s)."), info.LatestVersion, version.Version),
			false,
		)
	}
}

func (r *desktopRuntime) CheckUpdate(ctx context.Context) (web.DesktopStatus, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := desktop.CheckLatestRelease(checkCtx, nil, desktop.DefaultGitHubAPIBase, version.GitHubRepo, version.Version)
	r.updateMu.Lock()
	if err != nil {
		r.updateErr = err.Error()
	} else {
		r.updateInfo = info
		r.updateErr = ""
	}
	r.updateMu.Unlock()
	r.refreshTray()
	return r.Status(), err
}

func (r *desktopRuntime) ApplyUpdate(ctx context.Context) error {
	r.updateMu.Lock()
	if r.updating {
		r.updateMu.Unlock()
		return errors.New(r.tr("an update is already in progress", "đang tiến hành cập nhật"))
	}
	info := r.updateInfo
	r.updating = true
	r.updateMu.Unlock()

	resetUpdating := true
	defer func() {
		if resetUpdating {
			r.updateMu.Lock()
			r.updating = false
			r.updateMu.Unlock()
		}
	}()

	if info == nil || !info.Available {
		fresh, err := desktop.CheckLatestRelease(ctx, nil, desktop.DefaultGitHubAPIBase, version.GitHubRepo, version.Version)
		if err != nil {
			return err
		}
		r.updateMu.Lock()
		r.updateInfo = fresh
		r.updateMu.Unlock()
		info = fresh
	}
	if !info.Available {
		return fmt.Errorf(r.tr("already on the latest version (%s)", "đang ở phiên bản mới nhất (%s)"), version.Version)
	}

	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	installerPath, err := desktop.DownloadAndVerifyInstaller(dlCtx, nil, filepath.Join(r.dataDir, "updates"), info)
	if err != nil {
		r.updateMu.Lock()
		r.updateErr = err.Error()
		r.updateMu.Unlock()
		return err
	}

	// Safely unmount and flush pending writes BEFORE launching the silent upgrade installer.
	wasMounted, prevDrive, _ := r.mountMgr.Status()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 30*time.Second)
	r.overlaySrv.SetDrive("")
	_ = r.mountMgr.Unmount(flushCtx)
	if r.engine.IsReady() {
		_ = r.cache.FlushQueue(flushCtx)
		_ = r.engine.Packer().FlushNow(flushCtx)
	}
	flushCancel()

	if err := desktop.LaunchElevatedInstaller(installerPath); err != nil {
		// If user declined UAC or launch failed, restore mount cleanly instead of quitting.
		if wasMounted && prevDrive != "" {
			if r.mountMgr.Mount(prevDrive) == nil {
				r.overlaySrv.SetDrive(prevDrive)
			}
		}
		r.refreshTray()
		return err
	}

	resetUpdating = false
	go func() {
		time.Sleep(250 * time.Millisecond)
		r.Quit()
		r.trayMu.RLock()
		app := r.trayApp
		r.trayMu.RUnlock()
		if app != nil {
			_ = app.Close()
		}
	}()
	return nil
}

func (r *desktopRuntime) TriggerUpdate() error {
	st, err := r.CheckUpdate(context.Background())
	if err != nil {
		return err
	}
	if !st.UpdateAvailable {
		r.notifyTray("TeleMount", fmt.Sprintf(r.tr("Already on the latest version (%s).", "Đang ở phiên bản mới nhất (%s)."), version.Version), false)
		return nil
	}
	r.notifyTray(
		r.tr("TeleMount — Updating", "TeleMount — Cập nhật"),
		fmt.Sprintf(r.tr("Downloading and verifying %s...", "Đang tải và xác thực bản %s..."), st.LatestVersion),
		false,
	)
	err = r.ApplyUpdate(context.Background())
	if errors.Is(err, desktop.ErrUpdateCanceled) {
		return nil
	}
	return err
}

func (r *desktopRuntime) OpenWebUI() error {
	return tray.OpenBrowserURL(r.webURL)
}

func (r *desktopRuntime) ToggleStartup() (bool, error) {
	cur := r.Status().RunAtStartup
	next := !cur
	st, err := r.UpdateSettings(web.DesktopSettingsUpdate{RunAtStartup: &next})
	return st.RunAtStartup, err
}

func (r *desktopRuntime) Quit() {
	r.shutdownOnce()
}

func (r *desktopRuntime) shutdownOnce() {
	r.quitOnce.Do(func() {
		log.Println("shutting down TeleMount desktop (unmounting drive and flushing pending writes)...")
		_ = os.Remove(desktopIPCFilePath(r.dataDir))
		_ = r.overlaySrv.Close()
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.mountMgr.Unmount(shutCtx)
		if r.engine.IsReady() {
			if err := r.cache.FlushQueue(shutCtx); err != nil {
				log.Printf("flush disk queue on shutdown warning: %v", err)
			}
			if err := r.engine.Packer().FlushNow(shutCtx); err != nil {
				log.Printf("flush pack on shutdown warning: %v", err)
			}
		}
		_ = r.httpSrv.Shutdown(shutCtx)
		r.appCancel()
	})
}

// Adapter so desktopRuntime can satisfy both web.DesktopController (Status() web.DesktopStatus)
// and tray.TrayActions (Status() tray.TrayStatus).
type trayActionsAdapter struct {
	rt *desktopRuntime
}

func (a *trayActionsAdapter) Status() tray.TrayStatus      { return a.rt.TrayStatus() }
func (a *trayActionsAdapter) Mount() error                 { return a.rt.Mount() }
func (a *trayActionsAdapter) Unmount() error               { return a.rt.Unmount() }
func (a *trayActionsAdapter) OpenDrive() error             { return a.rt.OpenDrive() }
func (a *trayActionsAdapter) OpenWebUI() error             { return a.rt.OpenWebUI() }
func (a *trayActionsAdapter) ToggleStartup() (bool, error) { return a.rt.ToggleStartup() }
func (a *trayActionsAdapter) TriggerUpdate() error         { return a.rt.TriggerUpdate() }
func (a *trayActionsAdapter) Quit()                        { a.rt.Quit() }
