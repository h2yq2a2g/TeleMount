//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"telemount/internal/db"
	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/state"
	"telemount/internal/vfs"
)

func TestTier1_F1_DesktopConfig(t *testing.T) {
	t.Run("T1-F1-01_DefaultInitWhenAbsent", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		got := cs.Get()
		if got.DriveLetter != "V:" {
			t.Fatalf("DriveLetter = %q, want V:", got.DriveLetter)
		}
		if got.DiskCacheMaxBytes != 10*1024*1024*1024 {
			t.Fatalf("DiskCacheMaxBytes = %d, want 10 GiB", got.DiskCacheMaxBytes)
		}
		wantCacheDir := filepath.Join(dir, "cache")
		if filepath.Clean(got.DiskCacheDir) != filepath.Clean(wantCacheDir) {
			t.Fatalf("DiskCacheDir = %q, want %q", got.DiskCacheDir, wantCacheDir)
		}
		if !strings.HasPrefix(got.WebAddr, "127.0.0.1:") {
			t.Fatalf("WebAddr = %q, want loopback 127.0.0.1:<port>", got.WebAddr)
		}
		if len(got.PinnedPaths) != 0 {
			t.Fatalf("PinnedPaths = %v, want empty", got.PinnedPaths)
		}
	})

	t.Run("T1-F1-02_SaveAndReloadAllFields", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		customCache := filepath.Join(dir, "custom-cache")
		saved, err := cs.Update(func(c *desktop.Config) error {
			c.DriveLetter = "Z:"
			c.DiskCacheMaxBytes = 512 << 20
			c.DiskCacheDir = customCache
			c.PinnedPaths = []string{"/docs", "/a/b.txt"}
			c.RunAtStartup = true
			c.AutoMount = false
			c.WebAddr = "127.0.0.1:9988"
			return nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("ReadFile(desktop.json): %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("desktop.json invalid JSON: %v", err)
		}

		cs2, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("reload LoadConfig: %v", err)
		}
		got := cs2.Get()
		if !reflect.DeepEqual(got, saved) {
			t.Fatalf("reloaded config %+v != saved %+v", got, saved)
		}
	})

	t.Run("T1-F1-03_AtomicPartialUpdatePreservesUntouched", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if _, err := cs.Update(func(c *desktop.Config) error {
			c.DriveLetter = "M:"
			c.RunAtStartup = true
			return nil
		}); err != nil {
			t.Fatalf("initial Update: %v", err)
		}
		if _, err := cs.Update(func(c *desktop.Config) error {
			c.DiskCacheMaxBytes = 256 << 20
			return nil
		}); err != nil {
			t.Fatalf("partial Update: %v", err)
		}
		cs2, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		got := cs2.Get()
		if got.DriveLetter != "M:" || !got.RunAtStartup || got.DiskCacheMaxBytes != 256<<20 {
			t.Fatalf("unexpected partial update state: %+v", got)
		}
	})

	t.Run("T1-F1-04_PinnedPathNormalization", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		updated, err := cs.Update(func(c *desktop.Config) error {
			c.PinnedPaths = []string{`\Work\Docs\`, "/media/photo.jpg", "//Work/Docs"}
			return nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if !desktop.IsPathPinned(updated.PinnedPaths, "/Work/Docs/sub/file.txt") {
			t.Fatalf("expected /Work/Docs/sub/file.txt to match pinned paths %v", updated.PinnedPaths)
		}
		if !desktop.IsPathPinned(updated.PinnedPaths, "/media/photo.jpg") {
			t.Fatalf("expected /media/photo.jpg to match pinned paths %v", updated.PinnedPaths)
		}
		for _, p := range updated.PinnedPaths {
			if strings.Contains(p, `\`) || (len(p) > 1 && strings.HasSuffix(p, "/")) {
				t.Fatalf("pin path not normalized: %q", p)
			}
		}
	})

	t.Run("T1-F1-05_IndependenceFromDBAndState", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		want, err := cs.Update(func(c *desktop.Config) error {
			c.DriveLetter = "T:"
			c.RunAtStartup = true
			c.PinnedPaths = []string{"/important"}
			return nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}

		st, err := state.Load(dir)
		if err != nil {
			t.Fatalf("state.Load: %v", err)
		}
		_ = st.SetAdmin("admin", "secret123")
		_ = st.SetChannel(42, 84, "Test")

		store, err := db.Open(filepath.Join(dir, "telemount.db"))
		if err != nil {
			t.Fatalf("db.Open: %v", err)
		}
		defer store.Close()
		s, err := store.GetSettings()
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		s.RAMCacheMB = 512
		if err := store.SaveSettings(s); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}

		cs2, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatalf("reload LoadConfig: %v", err)
		}
		if !reflect.DeepEqual(cs2.Get(), want) {
			t.Fatalf("desktop.json mutated by DB/state writes: got %+v, want %+v", cs2.Get(), want)
		}
	})
}

func TestTier1_F2_DriveLetter(t *testing.T) {
	t.Run("T1-F2-01_CanonicalUppercaseColon", func(t *testing.T) {
		got, err := desktop.NormalizeDriveLetter("V:")
		if err != nil || got != "V:" {
			t.Fatalf("NormalizeDriveLetter(V:) = (%q, %v), want (V:, nil)", got, err)
		}
	})

	t.Run("T1-F2-02_LowercaseBareLetter", func(t *testing.T) {
		got, err := desktop.NormalizeDriveLetter("v")
		if err != nil || got != "V:" {
			t.Fatalf("NormalizeDriveLetter(v) = (%q, %v), want (V:, nil)", got, err)
		}
	})

	t.Run("T1-F2-03_TrailingBackslash", func(t *testing.T) {
		got, err := desktop.NormalizeDriveLetter(`t:\`)
		if err != nil || got != "T:" {
			t.Fatalf("NormalizeDriveLetter(t:\\) = (%q, %v), want (T:, nil)", got, err)
		}
		got2, err := desktop.NormalizeDriveLetter(`T:\`)
		if err != nil || got2 != "T:" {
			t.Fatalf("NormalizeDriveLetter(T:\\) = (%q, %v), want (T:, nil)", got2, err)
		}
	})

	t.Run("T1-F2-04_WhitespacePadded", func(t *testing.T) {
		got, err := desktop.NormalizeDriveLetter("  x:  ")
		if err != nil || got != "X:" {
			t.Fatalf("NormalizeDriveLetter('  x:  ') = (%q, %v), want (X:, nil)", got, err)
		}
	})

	t.Run("T1-F2-05_DriveCheckerFreeAndValidateConfig", func(t *testing.T) {
		dc := newFakeDriveChecker("C:")
		inUse, err := dc.IsDriveInUse("V:")
		if err != nil || inUse {
			t.Fatalf("IsDriveInUse(V:) = (%v, %v), want (false, nil)", inUse, err)
		}
		cfg := desktop.DefaultConfig(t.TempDir())
		cfg.DriveLetter = "V:"
		if err := desktop.ValidateConfig(cfg); err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
	})
}

func TestTier1_F3_RunAtStartup(t *testing.T) {
	t.Run("T1-F3-01_EnableWithoutArgs", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		exe := `C:\TeleMount\telemount-desktop.exe`
		if err := sm.Enable(exe); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		val, ok, err := ss.Get("TeleMount")
		if err != nil || !ok {
			t.Fatalf("Store.Get = (%q, %v, %v)", val, ok, err)
		}
		want := `"` + exe + `"`
		if val != want {
			t.Fatalf("stored value = %q, want %q", val, want)
		}
		enabled, err := sm.IsEnabled()
		if err != nil || !enabled {
			t.Fatalf("IsEnabled = (%v, %v), want (true, nil)", enabled, err)
		}
	})

	t.Run("T1-F3-02_EnableWithArgs", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		exe := `C:\App\telemount-desktop.exe`
		cmd, err := sm.FormatCommand(exe, "--minimized", "--data", `D:\TD`)
		if err != nil {
			t.Fatalf("FormatCommand: %v", err)
		}
		if !strings.HasPrefix(cmd, `"`+exe+`"`) {
			t.Fatalf("FormatCommand = %q, expected double-quoted exe prefix", cmd)
		}
		if !strings.Contains(cmd, "--minimized") || !strings.Contains(cmd, "--data") {
			t.Fatalf("FormatCommand = %q, missing args outside quotes", cmd)
		}
		if err := sm.Enable(exe, "--minimized", "--data", `D:\TD`); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		stored, ok, err := ss.Get("TeleMount")
		if err != nil || !ok || stored != cmd {
			t.Fatalf("stored = (%q, %v, %v), want %q", stored, ok, err, cmd)
		}
	})

	t.Run("T1-F3-03_DisableRemovesEntry", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		if err := sm.Enable(`C:\TeleMount\telemount-desktop.exe`); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		if err := sm.Disable(); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		enabled, err := sm.IsEnabled()
		if err != nil || enabled {
			t.Fatalf("IsEnabled = (%v, %v), want (false, nil)", enabled, err)
		}
		if _, ok, _ := ss.Get("TeleMount"); ok {
			t.Fatalf("expected entry removed from store")
		}
	})

	t.Run("T1-F3-04_ReEnableOverwritesPath", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		if err := sm.Enable(`C:\Old\telemount-desktop.exe`); err != nil {
			t.Fatalf("Enable old: %v", err)
		}
		newExe := `D:\New\telemount-desktop.exe`
		if err := sm.Enable(newExe); err != nil {
			t.Fatalf("Enable new: %v", err)
		}
		val, ok, err := ss.Get("TeleMount")
		if err != nil || !ok || !strings.Contains(val, newExe) || strings.Contains(val, `C:\Old`) {
			t.Fatalf("stored = (%q, %v, %v), want only %q", val, ok, err, newExe)
		}
		enabled, err := sm.IsEnabled()
		if err != nil || !enabled {
			t.Fatalf("IsEnabled = (%v, %v), want (true, nil)", enabled, err)
		}
	})

	t.Run("T1-F3-05_MultipleAppNamesIsolated", func(t *testing.T) {
		ss := newFakeStartupStore()
		mgr1 := desktop.NewStartupManager(ss, "TeleMount")
		mgr2 := desktop.NewStartupManager(ss, "OtherApp")
		if err := mgr1.Enable(`C:\TeleMount\telemount-desktop.exe`); err != nil {
			t.Fatalf("mgr1.Enable: %v", err)
		}
		if err := mgr2.Enable(`C:\Other\other.exe`); err != nil {
			t.Fatalf("mgr2.Enable: %v", err)
		}
		if err := mgr2.Disable(); err != nil {
			t.Fatalf("mgr2.Disable: %v", err)
		}
		e1, err := mgr1.IsEnabled()
		if err != nil || !e1 {
			t.Fatalf("mgr1.IsEnabled = (%v, %v), want (true, nil)", e1, err)
		}
		e2, err := mgr2.IsEnabled()
		if err != nil || e2 {
			t.Fatalf("mgr2.IsEnabled = (%v, %v), want (false, nil)", e2, err)
		}
	})
}

func TestTier1_F4_WinFspDetection(t *testing.T) {
	t.Run("T1-F4-01_InstalledX64", func(t *testing.T) {
		probe := newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`, "")
		st := probe.Check()
		if !st.Installed || st.DLLPath == "" {
			t.Fatalf("unexpected status: %+v", st)
		}
	})

	t.Run("T1-F4-02_InstalledARM64", func(t *testing.T) {
		probe := newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-a64.dll`, "")
		st := probe.Check()
		if !st.Installed || !strings.HasSuffix(st.DLLPath, "winfsp-a64.dll") {
			t.Fatalf("unexpected status: %+v", st)
		}
	})

	t.Run("T1-F4-03_SystemProbeSafeNoPanic", func(t *testing.T) {
		sysProbe := desktop.NewWinFspProbe()
		if sysProbe == nil {
			t.Fatal("NewWinFspProbe returned nil")
		}
		st1 := sysProbe.Check()
		st2 := desktop.CheckWinFsp()
		if st1.Installed != st2.Installed {
			t.Fatalf("CheckWinFsp mismatch: %+v vs %+v", st1, st2)
		}
		if st1.Installed && st1.DLLPath == "" {
			t.Fatalf("installed WinFsp reported empty DLLPath: %+v", st1)
		}
		if !st1.Installed && st1.Guidance == "" {
			t.Fatalf("missing WinFsp reported empty Guidance: %+v", st1)
		}
	})

	t.Run("T1-F4-04_TransitionMissingToInstalled", func(t *testing.T) {
		probe := newFakeWinFspProbe(false, "", "Install WinFsp from https://winfsp.dev")
		s1 := probe.Check()
		if s1.Installed || s1.Guidance == "" {
			t.Fatalf("first Check = %+v, want Installed=false with guidance", s1)
		}
		probe.Set(desktop.WinFspStatus{
			Installed: true,
			DLLPath:   `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`,
		})
		s2 := probe.Check()
		if !s2.Installed || s2.DLLPath == "" {
			t.Fatalf("second Check = %+v, want Installed=true with DLLPath", s2)
		}
	})

	t.Run("T1-F4-05_MountManagerDelegatesCheckWinFsp", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("diskcache.Open: %v", err)
		}
		defer cache.Close()

		want := desktop.WinFspStatus{
			Installed: true,
			DLLPath:   `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`,
		}
		probe := newFakeWinFspProbe(want.Installed, want.DLLPath, want.Guidance)
		mm := vfs.NewMountManager(vfs.NewTeleFS(store, cache), probe, newFakeDriveChecker("C:"))
		got := mm.CheckWinFsp()
		if got != want {
			t.Fatalf("mm.CheckWinFsp() = %+v, want %+v", got, want)
		}
	})
}
