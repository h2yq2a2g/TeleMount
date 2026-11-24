//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

type uploadRecord struct {
	MsgID    int
	FileName string
	Caption  string
	Size     int64
}

// fakeTG implements tg.Backend for controllable online/offline E2E simulation.
type fakeTG struct {
	mu           sync.Mutex
	authorized   bool
	nextID       int
	docs         map[int]tg.ChannelDoc
	blobs        map[int][]byte
	fetches      int
	uploads      int
	uploadLog    []uploadRecord
	failOnUpload int // 1-based upload attempt to fail once when > 0
	failErr      error
	tracker      *tg.Tracker
}

func newFakeTG(online bool) *fakeTG {
	tr := tg.NewTracker()
	if online {
		tr.SetConnectionState("ready", true)
		tr.SetAuthorized(true, &tg.UserInfo{ID: 100, Username: "e2e"})
	} else {
		tr.SetConnectionState("disconnected", false)
		tr.SetAuthorized(false, nil)
	}
	return &fakeTG{
		authorized: online,
		nextID:     1,
		docs:       make(map[int]tg.ChannelDoc),
		blobs:      make(map[int][]byte),
		tracker:    tr,
	}
}

func (f *fakeTG) SetOnline(online bool) {
	f.mu.Lock()
	f.authorized = online
	f.mu.Unlock()
	if online {
		f.tracker.SetConnectionState("ready", true)
		f.tracker.SetAuthorized(true, &tg.UserInfo{ID: 100, Username: "e2e"})
	} else {
		f.tracker.SetConnectionState("disconnected", false)
		f.tracker.SetAuthorized(false, nil)
	}
}

func (f *fakeTG) SetFailOnUploadAttempt(attempt int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnUpload = attempt
	f.failErr = err
}

func (f *fakeTG) Fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func (f *fakeTG) Uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads
}

func (f *fakeTG) UploadHistory() []uploadRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uploadRecord, len(f.uploadLog))
	copy(out, f.uploadLog)
	return out
}

func (f *fakeTG) ResetCounters() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = 0
	f.uploads = 0
}

func (f *fakeTG) IsAuthorized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorized
}

func (f *fakeTG) Tracker() *tg.Tracker { return f.tracker }

func (f *fakeTG) UploadBlob(_ context.Context, fileName, caption string, r io.Reader, _ int64, _ int, onProgress func(int64)) (*tg.UploadedDoc, error) {
	f.mu.Lock()
	online := f.authorized
	f.mu.Unlock()
	if !online {
		return nil, tg.ErrNotAuthorized
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(int64(len(data)))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	attempt := f.uploads + 1
	if f.failOnUpload > 0 && attempt == f.failOnUpload {
		f.failOnUpload = 0
		if f.failErr != nil {
			return nil, f.failErr
		}
		return nil, errors.New("simulated upload failure")
	}

	id := f.nextID
	f.nextID++
	f.uploads++
	f.blobs[id] = data
	f.docs[id] = tg.ChannelDoc{
		MsgID:      id,
		FileName:   fileName,
		Caption:    caption,
		FileID:     int64(id * 100),
		AccessHash: int64(id * 200),
		FileRef:    []byte("ref"),
		DCID:       5,
		Size:       int64(len(data)),
	}
	f.uploadLog = append(f.uploadLog, uploadRecord{
		MsgID:    id,
		FileName: fileName,
		Caption:  caption,
		Size:     int64(len(data)),
	})
	return &tg.UploadedDoc{
		MsgID:      id,
		FileID:     int64(id * 100),
		AccessHash: int64(id * 200),
		FileRef:    []byte("ref"),
		DCID:       5,
		Size:       int64(len(data)),
	}, nil
}

func (f *fakeTG) FetchChunk(_ context.Context, loc tg.DocRef, offset int64, limit int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authorized {
		return nil, tg.ErrNotAuthorized
	}
	f.fetches++
	data, ok := f.blobs[loc.MsgID]
	if !ok {
		return nil, errors.New("blob not found")
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + int64(limit)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return append([]byte(nil), data[offset:end]...), nil
}

func (f *fakeTG) ForwardCopyDoc(_ context.Context, _, _ int64, _ int) (*tg.UploadedDoc, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeTG) RefreshFileRef(_ context.Context, _ int) ([]byte, error) {
	return []byte("ref"), nil
}

func (f *fakeTG) DeleteMessages(_ context.Context, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.blobs, id)
		delete(f.docs, id)
	}
	return nil
}

func (f *fakeTG) ListChannelDocs(_ context.Context, prefix string, _ int) ([]tg.ChannelDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tg.ChannelDoc
	for _, d := range f.docs {
		if prefix == "" || strings.HasPrefix(d.FileName, prefix) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeTG) DownloadSmallDoc(_ context.Context, doc tg.ChannelDoc) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[doc.MsgID]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), b...), nil
}

func newTestCore(t *testing.T, dataDir string, online bool) (*state.Manager, *db.Store, *fakeTG, *storage.Engine, *tg.Manager) {
	t.Helper()
	st, err := state.Load(dataDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if err := st.SetAdmin("admin", "adminpass123"); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if err := st.SetChannel(1001, 2002, "E2EVault"); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	if err := st.SetMasterKey(crypto.DeriveMasterKey("e2e-passphrase", salt), salt); err != nil {
		t.Fatalf("SetMasterKey: %v", err)
	}
	if err := st.MarkSetupCompleted(); err != nil {
		t.Fatalf("MarkSetupCompleted: %v", err)
	}

	store, err := db.Open(filepath.Join(dataDir, "telemount.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fake := newFakeTG(online)
	eng := storage.NewEngine(store, st, fake, dataDir)
	tgMgr := tg.NewManager(dataDir)
	if online {
		tgMgr.Tracker().SetConnectionState("ready", true)
		tgMgr.Tracker().SetAuthorized(true, &tg.UserInfo{ID: 100, Username: "e2e"})
	}
	return st, store, fake, eng, tgMgr
}

func newAuthedWebClient(t *testing.T, srv *web.Server, user, pass string) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{
		"username": user,
		"password": pass,
	})
	resp, err := client.Post(ts.URL+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/auth/login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return ts, client
}

func uploadSeedFile(t *testing.T, eng *storage.Engine, store *db.Store, fake *fakeTG, virtualPath string, data []byte) *db.Node {
	t.Helper()
	ctx := context.Background()
	clean := strings.TrimPrefix(filepath.ToSlash(virtualPath), "/")
	dirPart := ""
	fileName := clean
	if idx := strings.LastIndex(clean, "/"); idx >= 0 {
		dirPart = clean[:idx]
		fileName = clean[idx+1:]
	}
	parentID := db.RootID
	if dirPart != "" {
		dirNode, err := store.MkdirAll(dirPart)
		if err != nil {
			t.Fatalf("MkdirAll(%q): %v", dirPart, err)
		}
		parentID = dirNode.ID
	}
	node, err := eng.UploadFile(ctx, parentID, fileName, "", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("UploadFile(%q): %v", virtualPath, err)
	}
	if err := eng.Packer().FlushNow(ctx); err != nil {
		t.Fatalf("Packer.FlushNow: %v", err)
	}
	eng.Cache().Clear()
	if fake != nil {
		fake.ResetCounters()
	}
	return node
}

func readAllEngineFile(t *testing.T, eng *storage.Engine, store *db.Store, virtualPath string) ([]byte, error) {
	t.Helper()
	node, err := store.StatByPath(virtualPath)
	if err != nil {
		return nil, err
	}
	eng.Cache().Clear()
	fr, err := eng.OpenFile(context.Background(), node)
	if err != nil {
		return nil, err
	}
	defer fr.Close()
	return io.ReadAll(fr)
}

func readAllCacheFile(ctx context.Context, mgr *diskcache.Manager, virtualPath string, size int64) ([]byte, error) {
	if size == 0 {
		buf := make([]byte, 16)
		n, err := mgr.ReadFileAt(ctx, virtualPath, buf, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return buf[:n], nil
	}
	out := make([]byte, size)
	var off int64
	for off < size {
		n, err := mgr.ReadFileAt(ctx, virtualPath, out[off:], off)
		off += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return out[:off], nil
}

// fakeStartupStore is an in-memory desktop.StartupStore double for E2E tests.
type fakeStartupStore struct {
	mu        sync.Mutex
	vals      map[string]string
	GetErr    error
	SetErr    error
	DeleteErr error
}

func newFakeStartupStore() *fakeStartupStore {
	return &fakeStartupStore{vals: make(map[string]string)}
}

func (s *fakeStartupStore) Get(name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.GetErr != nil {
		return "", false, s.GetErr
	}
	v, ok := s.vals[name]
	return v, ok, nil
}

func (s *fakeStartupStore) Set(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SetErr != nil {
		return s.SetErr
	}
	s.vals[name] = value
	return nil
}

func (s *fakeStartupStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DeleteErr != nil {
		return s.DeleteErr
	}
	delete(s.vals, name)
	return nil
}

// fakeDriveChecker is an in-memory desktop.DriveChecker double for E2E tests.
type fakeDriveChecker struct {
	mu    sync.Mutex
	inUse map[string]bool
	err   error
}

func newFakeDriveChecker(inUse ...string) *fakeDriveChecker {
	m := make(map[string]bool)
	for _, d := range inUse {
		m[strings.ToUpper(strings.TrimSpace(d))] = true
	}
	return &fakeDriveChecker{inUse: m}
}

func (c *fakeDriveChecker) SetInUse(drive string, used bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inUse[strings.ToUpper(strings.TrimSpace(drive))] = used
}

func (c *fakeDriveChecker) IsDriveInUse(drive string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	return c.inUse[strings.ToUpper(strings.TrimSpace(drive))], nil
}

// fakeWinFspProbe is a controllable desktop.WinFspProbe double for E2E tests.
type fakeWinFspProbe struct {
	mu     sync.Mutex
	status desktop.WinFspStatus
}

func newFakeWinFspProbe(installed bool, dllPath, guidance string) *fakeWinFspProbe {
	return &fakeWinFspProbe{
		status: desktop.WinFspStatus{
			Installed: installed,
			DLLPath:   dllPath,
			Guidance:  guidance,
		},
	}
}

func (p *fakeWinFspProbe) Set(st desktop.WinFspStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = st
}

func (p *fakeWinFspProbe) Check() desktop.WinFspStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// testDesktopRig wires real desktop/diskcache/vfs/web/tray components together
// with fakes strictly at external OS/Telegram boundaries.
type testDesktopRig struct {
	mu           sync.Mutex
	dataDir      string
	exePath      string
	cfgStore     *desktop.ConfigStore
	startup      *desktop.StartupManager
	startupStore *fakeStartupStore
	probe        *fakeWinFspProbe
	driveChecker *fakeDriveChecker
	cache        *diskcache.Manager
	fs           *vfs.TeleFS
	mountMgr     *vfs.MountManager
	fakeTG       *fakeTG
	engine       *storage.Engine
	tgMgr        *tg.Manager

	mounted     bool
	mountedDrv  string
	mountErr    string
	openDriveN  int
	openWebUIN  int
	quitN       int
	unmountN    int
	mountN      int
	mountFail   error
	startupFail error
}

func pickFreeDrives(t *testing.T, n int) []string {
	t.Helper()
	dc := desktop.NewDriveChecker()
	var out []string
	for ch := byte('Z'); ch >= 'D' && len(out) < n; ch-- {
		drv := string(ch) + ":"
		inUse, err := dc.IsDriveInUse(drv)
		if err == nil && !inUse {
			out = append(out, drv)
		}
	}
	if len(out) < n {
		t.Skipf("not enough free drive letters for live WinFsp test (need %d, found %d)", n, len(out))
	}
	return out
}

func newTestDesktopRig(t *testing.T, dataDir string, online bool, winFspInstalled bool) *testDesktopRig {
	t.Helper()
	_, store, fake, eng, tgMgr := newTestCore(t, dataDir, online)
	cfgPath := filepath.Join(dataDir, "desktop.json")
	cfgStore, err := desktop.LoadConfig(cfgPath, dataDir)
	if err != nil {
		t.Fatalf("desktop.LoadConfig: %v", err)
	}
	cfg := cfgStore.Get()
	cache, err := diskcache.Open(cfg.DiskCacheDir, filepath.Join(dataDir, "desktop_cache.db"), cfg.DiskCacheMaxBytes, store, eng)
	if err != nil {
		t.Fatalf("diskcache.Open: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	ss := newFakeStartupStore()
	sm := desktop.NewStartupManager(ss, "TeleMount")
	guidance := ""
	dllPath := `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`
	if !winFspInstalled {
		dllPath = ""
		guidance = "WinFsp is not installed. Download from https://winfsp.dev or run: winget install WinFsp.WinFsp"
	}
	probe := newFakeWinFspProbe(winFspInstalled, dllPath, guidance)
	dc := newFakeDriveChecker("C:")
	tfs := vfs.NewTeleFS(store, cache)
	mm := vfs.NewMountManager(tfs, probe, dc)

	return &testDesktopRig{
		dataDir:      dataDir,
		exePath:      `C:\Program Files\TeleMount\telemount-desktop.exe`,
		cfgStore:     cfgStore,
		startup:      sm,
		startupStore: ss,
		probe:        probe,
		driveChecker: dc,
		cache:        cache,
		fs:           tfs,
		mountMgr:     mm,
		fakeTG:       fake,
		engine:       eng,
		tgMgr:        tgMgr,
	}
}

func (r *testDesktopRig) Status() web.DesktopStatus {
	r.mu.Lock()
	mounted := r.mounted
	mountErr := r.mountErr
	r.mu.Unlock()

	cfg := r.cfgStore.Get()
	wf := r.probe.Check()
	cs := r.cache.Stats()
	qs := r.cache.QueueStatus()
	return web.DesktopStatus{
		Enabled:              true,
		WinFspInstalled:      wf.Installed,
		WinFspGuidance:       wf.Guidance,
		DriveLetter:          cfg.DriveLetter,
		Mounted:              mounted,
		MountError:           mountErr,
		RunAtStartup:         cfg.RunAtStartup,
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

func (r *testDesktopRig) UpdateSettings(u web.DesktopSettingsUpdate) (web.DesktopStatus, error) {
	var oldPins []string
	updated, err := r.cfgStore.Update(func(c *desktop.Config) error {
		oldPins = append([]string(nil), c.PinnedPaths...)
		if u.DriveLetter != nil {
			norm, err := desktop.NormalizeDriveLetter(*u.DriveLetter)
			if err != nil {
				return err
			}
			inUse, err := r.driveChecker.IsDriveInUse(norm)
			if err != nil {
				return err
			}
			if inUse {
				return errors.New("drive letter already in use: " + norm)
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
		for _, p := range updated.PinnedPaths {
			isOld := false
			for _, op := range oldPins {
				if desktop.NormalizeVirtualPath(op) == desktop.NormalizeVirtualPath(p) {
					isOld = true
					break
				}
			}
			if !isOld && r.engine.IsReady() {
				_ = r.cache.PinSync(context.Background(), p)
			}
		}
	}
	if u.RunAtStartup != nil {
		if *u.RunAtStartup {
			if err := r.startup.Enable(r.exePath, "--minimized"); err != nil {
				return r.Status(), err
			}
		} else {
			if err := r.startup.Disable(); err != nil {
				return r.Status(), err
			}
		}
	}
	return r.Status(), nil
}

func (r *testDesktopRig) Mount() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mountN++
	if r.mountFail != nil {
		r.mountErr = r.mountFail.Error()
		return r.mountFail
	}
	wf := r.probe.Check()
	if !wf.Installed {
		r.mountErr = wf.Guidance
		return errors.New("winfsp not installed: " + wf.Guidance)
	}
	cfg := r.cfgStore.Get()
	drv, err := desktop.NormalizeDriveLetter(cfg.DriveLetter)
	if err != nil {
		r.mountErr = err.Error()
		return err
	}
	inUse, err := r.driveChecker.IsDriveInUse(drv)
	if err != nil {
		r.mountErr = err.Error()
		return err
	}
	if inUse {
		err = errors.New("drive already in use: " + drv)
		r.mountErr = err.Error()
		return err
	}
	if desktop.CheckWinFsp().Installed {
		if realInUse, _ := desktop.NewDriveChecker().IsDriveInUse(drv); !realInUse {
			if err := r.mountMgr.Mount(drv); err != nil {
				r.mountErr = err.Error()
				return err
			}
		}
	}
	r.mounted = true
	r.mountedDrv = drv
	r.mountErr = ""
	return nil
}

func (r *testDesktopRig) Unmount() error {
	r.mu.Lock()
	r.unmountN++
	if r.mountMgr != nil && r.mountMgr.IsMounted() {
		_ = r.mountMgr.Unmount(context.Background())
	}
	r.mounted = false
	r.mountedDrv = ""
	r.mu.Unlock()
	return nil
}

func (r *testDesktopRig) OpenDrive() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.mounted {
		return errors.New("drive is not mounted")
	}
	r.openDriveN++
	return nil
}

func (r *testDesktopRig) OpenWebUI() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openWebUIN++
	return nil
}

func (r *testDesktopRig) ToggleStartup() (bool, error) {
	r.mu.Lock()
	if r.startupFail != nil {
		err := r.startupFail
		r.mu.Unlock()
		return false, err
	}
	r.mu.Unlock()

	cur := r.cfgStore.Get().RunAtStartup
	next := !cur
	if next {
		if err := r.startup.Enable(r.exePath, "--minimized"); err != nil {
			return false, err
		}
	} else {
		if err := r.startup.Disable(); err != nil {
			return false, err
		}
	}
	_, err := r.cfgStore.Update(func(c *desktop.Config) error {
		c.RunAtStartup = next
		return nil
	})
	return next, err
}

func (r *testDesktopRig) FlushQueue(ctx context.Context) error {
	err := r.cache.FlushQueue(ctx)
	if err == nil && r.engine.IsReady() {
		_ = r.engine.Packer().FlushNow(ctx)
	}
	return err
}

func (r *testDesktopRig) ClearUnpinnedCache() error {
	return r.cache.ClearUnpinned()
}

func (r *testDesktopRig) Quit() {
	_ = r.Unmount()
	_ = r.FlushQueue(context.Background())
	r.mu.Lock()
	r.quitN++
	r.mu.Unlock()
}

func (r *testDesktopRig) TrayStatus() tray.TrayStatus {
	st := r.Status()
	return tray.TrayStatus{
		Mounted:         st.Mounted,
		DriveLetter:     st.DriveLetter,
		WinFspInstalled: st.WinFspInstalled,
		WinFspGuidance:  st.WinFspGuidance,
		RunAtStartup:    st.RunAtStartup,
		QueuePending:    st.QueuePendingCount,
		Online:          r.fakeTG.IsAuthorized(),
	}
}

// rigTrayActions adapts testDesktopRig to tray.TrayActions (whose Status() returns tray.TrayStatus).
type rigTrayActions struct {
	rig *testDesktopRig
}

func (a *rigTrayActions) Status() tray.TrayStatus      { return a.rig.TrayStatus() }
func (a *rigTrayActions) Mount() error                 { return a.rig.Mount() }
func (a *rigTrayActions) Unmount() error               { return a.rig.Unmount() }
func (a *rigTrayActions) OpenDrive() error             { return a.rig.OpenDrive() }
func (a *rigTrayActions) OpenWebUI() error             { return a.rig.OpenWebUI() }
func (a *rigTrayActions) ToggleStartup() (bool, error) { return a.rig.ToggleStartup() }
func (a *rigTrayActions) Quit()                        { a.rig.Quit() }

// VFS helper functions for clean, genuine FUSE method calls.
func vfsWriteAll(t *testing.T, fs *vfs.TeleFS, path string, data []byte) {
	t.Helper()
	rc, fh := fs.Create(path, fuse.O_CREAT|fuse.O_RDWR|fuse.O_TRUNC, 0o644)
	if rc != 0 {
		t.Fatalf("Create(%q) rc=%d", path, rc)
	}
	if len(data) > 0 {
		wn := fs.Write(path, data, 0, fh)
		if wn != len(data) {
			t.Fatalf("Write(%q) = %d, want %d", path, wn, len(data))
		}
	}
	if rc := fs.Flush(path, fh); rc != 0 {
		t.Fatalf("Flush(%q) rc=%d", path, rc)
	}
	if rc := fs.Release(path, fh); rc != 0 {
		t.Fatalf("Release(%q) rc=%d", path, rc)
	}
}

func vfsReadAll(t *testing.T, fs *vfs.TeleFS, path string) ([]byte, int) {
	t.Helper()
	var st fuse.Stat_t
	if rc := fs.Getattr(path, &st, 0); rc != 0 {
		return nil, rc
	}
	rc, fh := fs.Open(path, fuse.O_RDONLY)
	if rc != 0 {
		return nil, rc
	}
	defer fs.Release(path, fh)

	out := make([]byte, st.Size)
	var off int64
	for off < st.Size {
		n := fs.Read(path, out[off:], off, fh)
		if n < 0 {
			return nil, n
		}
		if n == 0 {
			break
		}
		off += int64(n)
	}
	return out[:off], 0
}

func vfsListNames(t *testing.T, fs *vfs.TeleFS, dir string) ([]string, int) {
	t.Helper()
	rc, fh := fs.Opendir(dir)
	if rc != 0 {
		return nil, rc
	}
	defer fs.Releasedir(dir, fh)
	var names []string
	rc = fs.Readdir(dir, func(name string, _ *fuse.Stat_t, _ int64) bool {
		names = append(names, name)
		return true
	}, 0, fh)
	return names, rc
}

func mustWaitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out after %v: %s", timeout, msg)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod from %s", dir)
		}
		dir = parent
	}
}
