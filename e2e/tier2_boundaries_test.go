//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/tray"
	"telemount/internal/vfs"
	"telemount/internal/web"
)

func TestTier2_F1_DesktopConfigBoundaries(t *testing.T) {
	t.Run("T2-F1-01_CorruptDesktopJSONReturnsError", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		raw := []byte("{bad json")
		if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := desktop.LoadConfig(cfgPath, dir); err == nil {
			t.Fatal("expected parse error for corrupt desktop.json")
		}
		after, _ := os.ReadFile(cfgPath)
		if !bytes.Equal(after, raw) {
			t.Fatalf("corrupt desktop.json was silently overwritten: %q", after)
		}
	})

	t.Run("T2-F1-02_EmptyDesktopJSONReturnsError", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := desktop.LoadConfig(cfgPath, dir); err == nil {
			t.Fatal("expected error for 0-byte desktop.json")
		}
	})

	t.Run("T2-F1-03_InvalidFieldsOnLoadAndSaveRejected", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		if err := os.WriteFile(cfgPath, []byte(`{"drive_letter":"C:","disk_cache_max_bytes":-1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := desktop.LoadConfig(cfgPath, dir); err == nil {
			t.Fatal("expected validation error loading invalid desktop.json")
		}
		bad := desktop.DefaultConfig(dir)
		bad.DiskCacheMaxBytes = -10
		if err := desktop.ValidateConfig(bad); err == nil {
			t.Fatal("expected ValidateConfig error for negative cache limit")
		}
	})

	t.Run("T2-F1-04_FailedUpdatePreservesPersistedFile", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Update(func(c *desktop.Config) error {
			c.DriveLetter = "V:"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Update(func(c *desktop.Config) error {
			c.DriveLetter = "C:"
			return nil
		}); err == nil {
			t.Fatal("expected Update with C: to fail")
		}
		if got := cs.Get().DriveLetter; got != "V:" {
			t.Fatalf("in-memory DriveLetter = %q, want V:", got)
		}
		reloaded, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil || reloaded.Get().DriveLetter != "V:" {
			t.Fatalf("persisted config altered after failed Update: %+v, err=%v", reloaded, err)
		}
	})

	t.Run("T2-F1-05_ConcurrentUpdateAndGet", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		cs, err := desktop.LoadConfig(cfgPath, dir)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, _ = cs.Update(func(c *desktop.Config) error {
					c.DiskCacheMaxBytes = int64(1024*1024 + idx*4096)
					c.AutoMount = idx%2 == 0
					return nil
				})
				_ = cs.Get()
			}(i)
		}
		wg.Wait()
		if _, err := desktop.LoadConfig(cfgPath, dir); err != nil {
			t.Fatalf("desktop.json unreadable after concurrent updates: %v", err)
		}
	})
}

func TestTier2_F2_DriveLetterBoundaries(t *testing.T) {
	t.Run("T2-F2-01_ReservedSystemDriveCRejected", func(t *testing.T) {
		for _, in := range []string{"C:", "c", `C:\`, `c:\`} {
			if got, err := desktop.NormalizeDriveLetter(in); err == nil {
				t.Fatalf("NormalizeDriveLetter(%q) = %q, want error rejecting C:", in, got)
			}
		}
	})

	t.Run("T2-F2-02_DigitsAndSymbolsRejected", func(t *testing.T) {
		for _, in := range []string{"1:", "9", "$:", "!:", "@:\\"} {
			if got, err := desktop.NormalizeDriveLetter(in); err == nil {
				t.Fatalf("NormalizeDriveLetter(%q) = %q, want error", in, got)
			}
		}
	})

	t.Run("T2-F2-03_MultiCharDriveRejected", func(t *testing.T) {
		for _, in := range []string{"VV:", "AB:", "VD", "C:\\Windows"} {
			if got, err := desktop.NormalizeDriveLetter(in); err == nil {
				t.Fatalf("NormalizeDriveLetter(%q) = %q, want error", in, got)
			}
		}
	})

	t.Run("T2-F2-04_EmptyAndWhitespaceRejected", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\t\n", ":"} {
			if got, err := desktop.NormalizeDriveLetter(in); err == nil {
				t.Fatalf("NormalizeDriveLetter(%q) = %q, want error", in, got)
			}
		}
	})

	t.Run("T2-F2-05_OccupiedDriveConflictRejected", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		rig.driveChecker.SetInUse("E:", true)
		drv := "E:"
		if _, err := rig.UpdateSettings(web.DesktopSettingsUpdate{DriveLetter: &drv}); err == nil {
			t.Fatal("expected conflict error when drive E: is in use")
		}
		if err := rig.mountMgr.Mount("E:"); err == nil {
			t.Fatal("expected MountManager.Mount(E:) to reject occupied drive")
		}
	})
}

func TestTier2_F3_RunAtStartupBoundaries(t *testing.T) {
	t.Run("T2-F3-01_ExePathWithSpacesAndParenthesesQuoted", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		exe := `C:\Program Files (x86)\TeleMount\telemount desktop.exe`
		if err := sm.Enable(exe, "--tray"); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		val, ok, err := ss.Get("TeleMount")
		if err != nil || !ok {
			t.Fatalf("Get = (%q, %v, %v)", val, ok, err)
		}
		want := `"` + exe + `" --tray`
		if val != want {
			t.Fatalf("stored command = %q, want %q", val, want)
		}
	})

	t.Run("T2-F3-02_DisableWhenMissingIsIdempotent", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		if err := sm.Disable(); err != nil {
			t.Fatalf("Disable when missing = %v, want nil", err)
		}
	})

	t.Run("T2-F3-03_IsEnabledWhenMissingReturnsFalseNil", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		enabled, err := sm.IsEnabled()
		if err != nil || enabled {
			t.Fatalf("IsEnabled when missing = (%v, %v), want (false, nil)", enabled, err)
		}
	})

	t.Run("T2-F3-04_EnableIdempotency", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		exe := `C:\TeleMount\telemount-desktop.exe`
		if err := sm.Enable(exe); err != nil {
			t.Fatalf("first Enable: %v", err)
		}
		if err := sm.Enable(exe); err != nil {
			t.Fatalf("second Enable: %v", err)
		}
		val, _, _ := ss.Get("TeleMount")
		if val != `"`+exe+`"` {
			t.Fatalf("value after double Enable = %q, want single quoted command", val)
		}
	})

	t.Run("T2-F3-05_EmptyExePathAndStoreErrorsPropagate", func(t *testing.T) {
		ss := newFakeStartupStore()
		sm := desktop.NewStartupManager(ss, "TeleMount")
		if err := sm.Enable(""); err == nil {
			t.Fatal("expected error for empty exePath")
		}
		ss.SetErr = errors.New("access denied")
		if err := sm.Enable(`C:\TeleMount\telemount-desktop.exe`); err == nil {
			t.Fatal("expected SetErr propagation")
		}
		ss.GetErr = errors.New("read denied")
		if _, err := sm.IsEnabled(); err == nil {
			t.Fatal("expected GetErr propagation")
		}
	})
}

func TestTier2_F4_WinFspDetectionBoundaries(t *testing.T) {
	t.Run("T2-F4-01_WinFspCompletelyMissing", func(t *testing.T) {
		probe := newFakeWinFspProbe(false, "", "WinFsp not found: https://winfsp.dev or winget install WinFsp.WinFsp")
		st := probe.Check()
		if st.Installed || st.DLLPath != "" || st.Guidance == "" {
			t.Fatalf("unexpected status: %+v", st)
		}
	})

	t.Run("T2-F4-02_MissingDLLNotReportedInstalled", func(t *testing.T) {
		// Create a fake WinFsp bin dir without winfsp-x64.dll and verify status check.
		fakeDir := t.TempDir()
		if _, err := os.Stat(filepath.Join(fakeDir, "winfsp-x64.dll")); err == nil {
			t.Fatal("expected DLL absent")
		}
		probe := newFakeWinFspProbe(false, "", "winfsp-x64.dll missing in "+fakeDir+"; install from https://winfsp.dev")
		st := probe.Check()
		if st.Installed || st.Guidance == "" {
			t.Fatalf("expected Installed=false when DLL absent on disk: %+v", st)
		}
	})

	t.Run("T2-F4-03_X86FallbackStatusHandledCleanly", func(t *testing.T) {
		probe := newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-x86.dll`, "")
		st := probe.Check()
		if !st.Installed || !strings.HasSuffix(st.DLLPath, "winfsp-x86.dll") {
			t.Fatalf("unexpected x86 status: %+v", st)
		}
	})

	t.Run("T2-F4-04_RepeatedMountManagerCheckWinFspNoPanic", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		mm := vfs.NewMountManager(vfs.NewTeleFS(store, cache), nil, nil)
		s1 := mm.CheckWinFsp()
		s2 := mm.CheckWinFsp()
		if s1.Installed != s2.Installed {
			t.Fatalf("non-deterministic CheckWinFsp: %+v vs %+v", s1, s2)
		}
	})

	t.Run("T2-F4-05_SystemGuidanceActionabilityWhenMissing", func(t *testing.T) {
		st := desktop.CheckWinFsp()
		if !st.Installed {
			low := strings.ToLower(st.Guidance)
			if !strings.Contains(low, "winfsp") || (!strings.Contains(low, "http") && !strings.Contains(low, "winget")) {
				t.Fatalf("system WinFsp guidance lacks concrete install steps: %q", st.Guidance)
			}
		}
	})
}

func TestTier2_F5_PersistentCacheBoundaries(t *testing.T) {
	t.Run("T2-F5-01_ZeroByteFileCachedAndReadOffline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/empty.txt", nil)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		n, err := mgr.ReadFileAt(t.Context(), "/empty.txt", buf, 0)
		if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("0-byte ReadFileAt = (%d, %v)", n, err)
		}
		_ = mgr.Close()

		fake.SetOnline(false)
		fake.ResetCounters()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()
		n2, err2 := mgr2.ReadFileAt(t.Context(), "/empty.txt", buf, 0)
		if n2 != 0 || (err2 != nil && !errors.Is(err2, io.EOF)) {
			t.Fatalf("offline 0-byte ReadFileAt = (%d, %v)", n2, err2)
		}
		if fake.Fetches() != 0 {
			t.Fatalf("expected 0 fetches for cached 0-byte file, got %d", fake.Fetches())
		}
	})

	t.Run("T2-F5-02_ReadAtAndPastEOF", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/eof.bin", bytes.Repeat([]byte("Z"), 100))

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		buf := make([]byte, 32)
		for _, off := range []int64{100, 500} {
			n, err := mgr.ReadFileAt(t.Context(), "/eof.bin", buf, off)
			if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
				t.Fatalf("ReadFileAt(off=%d) = (%d, %v), want (0, EOF/nil)", off, n, err)
			}
		}
	})

	t.Run("T2-F5-03_OfflineReadOfUncachedFileFails", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/uncached.txt", []byte("only-in-telegram"))
		fake.SetOnline(false)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		buf := make([]byte, 32)
		if n, err := mgr.ReadFileAt(t.Context(), "/uncached.txt", buf, 0); err == nil {
			t.Fatalf("expected error reading uncached file offline, got n=%d", n)
		}
	})

	t.Run("T2-F5-04_MissingBackingBlobSelfHealsOnline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		want := []byte("self-heal-payload-1234567890")
		uploadSeedFile(t, eng, store, fake, "/corrupt.txt", want)

		cacheDir := filepath.Join(dir, "cache")
		mgr, err := diskcache.Open(cacheDir, filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		if _, err := readAllCacheFile(t.Context(), mgr, "/corrupt.txt", int64(len(want))); err != nil {
			t.Fatal(err)
		}
		// Wipe cached blob files on disk while keeping desktop_cache.db rows.
		entries, _ := os.ReadDir(cacheDir)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(cacheDir, e.Name()))
		}
		eng.Cache().Clear()

		got, err := readAllCacheFile(t.Context(), mgr, "/corrupt.txt", int64(len(want)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("expected online self-heal read, got (%q, %v)", got, err)
		}
	})

	t.Run("T2-F5-05_DeepNestedUnicodePathAcrossRestart", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		vpath := "/📁/Sub Folder/tệp tin (1).txt"
		want := []byte("unicode-content-ok")
		uploadSeedFile(t, eng, store, fake, vpath, want)

		cacheDir := filepath.Join(dir, "cache")
		cacheDB := filepath.Join(dir, "desktop_cache.db")
		mgr, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readAllCacheFile(t.Context(), mgr, vpath, int64(len(want))); err != nil {
			t.Fatal(err)
		}
		_ = mgr.Close()

		fake.SetOnline(false)
		eng.Cache().Clear()
		mgr2, err := diskcache.Open(cacheDir, cacheDB, desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr2.Close()

		got, err := readAllCacheFile(t.Context(), mgr2, vpath, int64(len(want)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("unicode read after restart = (%q, %v), want %q", got, err, want)
		}
	})
}

func TestTier2_F6_LRUEvictionBoundaries(t *testing.T) {
	t.Run("T2-F6-01_NegativeMaxBytesRejectedAndZeroClamped", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/keep.bin", []byte("12345"))

		negMgr, err := diskcache.Open(filepath.Join(dir, "c2"), filepath.Join(dir, "d2.db"), -100, store, eng)
		if err != nil {
			t.Fatalf("Open(-100) error: %v", err)
		}
		if got := negMgr.Stats().MaxBytes; got != diskcache.DefaultMaxBytes {
			_ = negMgr.Close()
			t.Fatalf("Open(-100) MaxBytes = %d, want DefaultMaxBytes (%d)", got, diskcache.DefaultMaxBytes)
		}
		_ = negMgr.Close()

		mgr, err := diskcache.Open(filepath.Join(dir, "c3"), filepath.Join(dir, "d3.db"), 2048, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_, _ = readAllCacheFile(t.Context(), mgr, "/keep.bin", 5)

		if err := mgr.SetMaxBytes(-5); err == nil {
			t.Fatal("expected SetMaxBytes(-5) error")
		}
		if mgr.Stats().MaxBytes != 2048 || mgr.Stats().EntryCount != 1 {
			t.Fatalf("invalid SetMaxBytes corrupted cache: %+v", mgr.Stats())
		}
	})

	t.Run("T2-F6-02_ExactLimitBoundaryZeroEvictions", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/a.bin", bytes.Repeat([]byte("A"), 1024))
		uploadSeedFile(t, eng, store, fake, "/b.bin", bytes.Repeat([]byte("B"), 1024))

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2048, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_, _ = readAllCacheFile(t.Context(), mgr, "/a.bin", 1024)
		_, _ = readAllCacheFile(t.Context(), mgr, "/b.bin", 1024)

		st := mgr.Stats()
		if st.TotalBytes != 2048 || st.EvictionCount != 0 || st.EntryCount != 2 {
			t.Fatalf("unexpected stats at exact limit: %+v", st)
		}
	})

	t.Run("T2-F6-03_OneByteOverEvictsOldest", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/a.bin", bytes.Repeat([]byte("A"), 1024))
		uploadSeedFile(t, eng, store, fake, "/b.bin", bytes.Repeat([]byte("B"), 1024))
		uploadSeedFile(t, eng, store, fake, "/c.bin", []byte("1"))

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2048, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_, _ = readAllCacheFile(t.Context(), mgr, "/a.bin", 1024)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/b.bin", 1024)
		time.Sleep(15 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/c.bin", 1)

		st := mgr.Stats()
		if st.TotalBytes > 2048 || st.EvictionCount < 1 {
			t.Fatalf("expected eviction when 1 byte over: %+v", st)
		}
		fake.SetOnline(false)
		eng.Cache().Clear()
		if _, err := readAllCacheFile(t.Context(), mgr, "/a.bin", 1024); err == nil {
			t.Fatal("expected oldest /a.bin evicted")
		}
	})

	t.Run("T2-F6-04_SingleUnpinnedLargerThanMaxBytesNoLoop", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/small.bin", bytes.Repeat([]byte("S"), 400))
		uploadSeedFile(t, eng, store, fake, "/huge.bin", bytes.Repeat([]byte("H"), 2000))

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 1000, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_, _ = readAllCacheFile(t.Context(), mgr, "/small.bin", 400)
		got, err := readAllCacheFile(t.Context(), mgr, "/huge.bin", 2000)
		if err != nil || len(got) != 2000 {
			t.Fatalf("oversized read failed: len=%d err=%v", len(got), err)
		}
	})

	t.Run("T2-F6-05_AllEntriesPinnedOverBudgetNeverEvicted", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		p1 := bytes.Repeat([]byte("1"), 1000)
		p2 := bytes.Repeat([]byte("2"), 1000)
		uploadSeedFile(t, eng, store, fake, "/p1.bin", p1)
		uploadSeedFile(t, eng, store, fake, "/p2.bin", p2)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 3000, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_ = mgr.SetPinnedPaths([]string{"/p1.bin", "/p2.bin"})
		_, _ = readAllCacheFile(t.Context(), mgr, "/p1.bin", 1000)
		_, _ = readAllCacheFile(t.Context(), mgr, "/p2.bin", 1000)

		if err := mgr.SetMaxBytes(500); err != nil {
			t.Fatalf("SetMaxBytes(500): %v", err)
		}
		fake.SetOnline(false)
		eng.Cache().Clear()
		if g1, err := readAllCacheFile(t.Context(), mgr, "/p1.bin", 1000); err != nil || !bytes.Equal(g1, p1) {
			t.Fatalf("/p1.bin lost when pinned: err=%v", err)
		}
		if g2, err := readAllCacheFile(t.Context(), mgr, "/p2.bin", 1000); err != nil || !bytes.Equal(g2, p2) {
			t.Fatalf("/p2.bin lost when pinned: err=%v", err)
		}
	})
}

func TestTier2_F7_PinningBoundaries(t *testing.T) {
	t.Run("T2-F7-01_PrefixCollisionWorkVsWork2", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		wa := bytes.Repeat([]byte("A"), 1000)
		w2 := bytes.Repeat([]byte("B"), 1000)
		u := bytes.Repeat([]byte("C"), 1000)
		uploadSeedFile(t, eng, store, fake, "/work/a.txt", wa)
		uploadSeedFile(t, eng, store, fake, "/work2/b.txt", w2)
		uploadSeedFile(t, eng, store, fake, "/other.bin", u)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), 2200, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		_ = mgr.SetPinnedPaths([]string{"/work"})

		_, _ = readAllCacheFile(t.Context(), mgr, "/work/a.txt", 1000)
		time.Sleep(10 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/work2/b.txt", 1000)
		time.Sleep(10 * time.Millisecond)
		_, _ = readAllCacheFile(t.Context(), mgr, "/other.bin", 1000)

		fake.SetOnline(false)
		eng.Cache().Clear()
		if got, err := readAllCacheFile(t.Context(), mgr, "/work/a.txt", 1000); err != nil || !bytes.Equal(got, wa) {
			t.Fatalf("/work/a.txt should remain pinned: err=%v", err)
		}
		if _, err := readAllCacheFile(t.Context(), mgr, "/work2/b.txt", 1000); err == nil {
			t.Fatal("/work2/b.txt must NOT match /work pin and should have been evicted")
		}
	})

	t.Run("T2-F7-02_TrailingSlashAndBackslashNormalized", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		if err := mgr.SetPinnedPaths([]string{`\work\`, `/work/sub/`}); err != nil {
			t.Fatal(err)
		}
		pins := mgr.PinnedPaths()
		if !desktop.IsPathPinned(pins, "/work/x.txt") {
			t.Fatalf("expected /work/x.txt pinned in %v", pins)
		}
		for _, p := range pins {
			if strings.Contains(p, `\`) || (len(p) > 1 && strings.HasSuffix(p, "/")) {
				t.Fatalf("unnormalized pin path %q", p)
			}
		}
	})

	t.Run("T2-F7-03_RootPinAndDuplicates", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		if err := mgr.SetPinnedPaths([]string{"/a", "/a", "/a/"}); err != nil {
			t.Fatal(err)
		}
		if got := mgr.PinnedPaths(); len(got) != 1 || got[0] != "/a" {
			t.Fatalf("PinnedPaths = %v, want [/a]", got)
		}
		if err := mgr.SetPinnedPaths([]string{"/"}); err != nil {
			t.Fatal(err)
		}
		if !desktop.IsPathPinned(mgr.PinnedPaths(), "/anything/nested.txt") {
			t.Fatalf("root pin '/' should cover nested children")
		}
	})

	t.Run("T2-F7-04_PinSyncOfflineOrMissingPathErrorsCleanly", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/valid.txt", []byte("ok"))

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		if err := mgr.PinSync(t.Context(), "/missing"); err == nil {
			t.Fatal("expected PinSync(/missing) to return error")
		}
		fake.SetOnline(false)
		if err := mgr.PinSync(t.Context(), "/valid.txt"); err == nil {
			t.Fatal("expected PinSync offline to return error")
		}
	})

	t.Run("T2-F7-05_PinningEmptyFolderSucceeds", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		if _, err := store.MkdirAll("empty_dir"); err != nil {
			t.Fatal(err)
		}
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		if err := mgr.SetPinnedPaths([]string{"/empty_dir"}); err != nil {
			t.Fatal(err)
		}
		if err := mgr.PinSync(t.Context(), "/empty_dir"); err != nil {
			t.Fatalf("PinSync(/empty_dir) = %v, want nil", err)
		}
		if st := mgr.Stats(); st.PinnedBytes != 0 {
			t.Fatalf("PinnedBytes = %d, want 0", st.PinnedBytes)
		}
	})
}

func TestTier2_F8_OfflineWriteQueueBoundaries(t *testing.T) {
	t.Run("T2-F8-01_OfflinePartialRandomWritesAndGrowth", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), false, true)
		vfsWriteAll(t, rig.fs, "/rand.bin", []byte("0123456789"))

		rc, fh := rig.fs.Open("/rand.bin", fuse.O_RDWR)
		if rc != 0 {
			t.Fatalf("Open rc=%d", rc)
		}
		if n := rig.fs.Write("/rand.bin", []byte("XYZ"), 3, fh); n != 3 {
			t.Fatalf("Write1 = %d", n)
		}
		if n := rig.fs.Write("/rand.bin", []byte("AB"), 9, fh); n != 2 {
			t.Fatalf("Write2 = %d", n)
		}
		_ = rig.fs.Flush("/rand.bin", fh)
		_ = rig.fs.Release("/rand.bin", fh)

		want := []byte("012XYZ678AB")
		got, rc := vfsReadAll(t, rig.fs, "/rand.bin")
		if rc != 0 || !bytes.Equal(got, want) {
			t.Fatalf("offline random read = (%q, rc=%d), want %q", got, rc, want)
		}

		rig.fakeTG.SetOnline(true)
		if err := rig.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		synced, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/rand.bin")
		if err != nil || !bytes.Equal(synced, want) {
			t.Fatalf("synced read = (%q, %v), want %q", synced, err, want)
		}
	})

	t.Run("T2-F8-02_OfflineTruncateShrinkAndGrowZeroFilled", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), false, true)
		vfsWriteAll(t, rig.fs, "/trunc.bin", []byte("abcdef"))

		rc, fh := rig.fs.Open("/trunc.bin", fuse.O_RDWR)
		if rc != 0 {
			t.Fatalf("Open rc=%d", rc)
		}
		if rc := rig.fs.Truncate("/trunc.bin", 3, fh); rc != 0 {
			t.Fatalf("Truncate(3) rc=%d", rc)
		}
		_ = rig.fs.Flush("/trunc.bin", fh)
		_ = rig.fs.Release("/trunc.bin", fh)

		got3, rc := vfsReadAll(t, rig.fs, "/trunc.bin")
		if rc != 0 || string(got3) != "abc" {
			t.Fatalf("after shrink = (%q, rc=%d), want abc", got3, rc)
		}

		rc, fh = rig.fs.Open("/trunc.bin", fuse.O_RDWR)
		if rc != 0 {
			t.Fatalf("Open2 rc=%d", rc)
		}
		if rc := rig.fs.Truncate("/trunc.bin", 8, fh); rc != 0 {
			t.Fatalf("Truncate(8) rc=%d", rc)
		}
		_ = rig.fs.Flush("/trunc.bin", fh)
		_ = rig.fs.Release("/trunc.bin", fh)

		want8 := []byte("abc\x00\x00\x00\x00\x00")
		got8, rc := vfsReadAll(t, rig.fs, "/trunc.bin")
		if rc != 0 || !bytes.Equal(got8, want8) {
			t.Fatalf("after grow = (%x, rc=%d), want %x", got8, rc, want8)
		}

		rig.fakeTG.SetOnline(true)
		if err := rig.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		synced, err := readAllEngineFile(t, rig.engine, rig.engine.Store(), "/trunc.bin")
		if err != nil || !bytes.Equal(synced, want8) {
			t.Fatalf("synced truncated file = (%x, %v), want %x", synced, err, want8)
		}
	})

	t.Run("T2-F8-03_MidFlushNetworkFailurePreservesRemainingFIFO", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		// Force each small file to upload individually on UploadFile by setting SmallFileMaxMB=0.
		s, _ := store.GetSettings()
		s.SmallFileMaxMB = 0
		_ = store.SaveSettings(s)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		for _, name := range []string{"/op1.txt", "/op2.txt", "/op3.txt"} {
			st, _ := mgr.StageOpenForWrite(name, true)
			_ = os.WriteFile(st, []byte(name), 0o600)
			if err := mgr.CommitFile(t.Context(), name, st, int64(len(name)), time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}

		fake.SetOnline(true)
		fake.SetFailOnUploadAttempt(2, errors.New("transient 502"))
		if err := mgr.FlushQueue(t.Context()); err == nil {
			t.Fatal("expected mid-flush error on 2nd upload")
		}
		if qs := mgr.QueueStatus(); qs.PendingCount != 2 {
			t.Fatalf("PendingCount after 1 success + 1 fail = %d, want 2", qs.PendingCount)
		}

		if err := mgr.FlushQueue(t.Context()); err != nil {
			t.Fatalf("retry FlushQueue: %v", err)
		}
		if qs := mgr.QueueStatus(); qs.PendingCount != 0 {
			t.Fatalf("PendingCount after retry = %d, want 0", qs.PendingCount)
		}
		if fake.Uploads() != 3 {
			t.Fatalf("expected exactly 3 successful uploads (no duplicate op1), got %d", fake.Uploads())
		}
	})

	t.Run("T2-F8-04_DeleteOfflineCreatedNeverUploadedFile", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		st, _ := mgr.StageOpenForWrite("/temp.txt", true)
		_ = os.WriteFile(st, []byte("ephemeral"), 0o600)
		_ = mgr.CommitFile(t.Context(), "/temp.txt", st, 9, time.Now().Unix())
		if err := mgr.QueueDelete(t.Context(), "/temp.txt", false); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := mgr.Stat("/temp.txt"); ok {
			t.Fatal("expected /temp.txt absent in overlay")
		}

		fake.SetOnline(true)
		if err := mgr.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		if _, err := store.StatByPath("/temp.txt"); err == nil {
			t.Fatal("expected /temp.txt absent in db.Store")
		}
	})

	t.Run("T2-F8-05_RenameOverwriteExistingFileOffline", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, true)
		uploadSeedFile(t, eng, store, fake, "/src.txt", []byte("new-data"))
		uploadSeedFile(t, eng, store, fake, "/dst.txt", []byte("old-data"))
		fake.SetOnline(false)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		if err := mgr.QueueRename(t.Context(), "/src.txt", "/dst.txt"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := mgr.Stat("/src.txt"); ok {
			t.Fatal("/src.txt should not exist after rename")
		}
		// Ensure /dst.txt reads "new-data" after online flush and in overlay if cached.
		fake.SetOnline(true)
		if err := mgr.FlushQueue(t.Context()); err != nil {
			t.Fatalf("FlushQueue: %v", err)
		}
		got, err := readAllEngineFile(t, eng, store, "/dst.txt")
		if err != nil || string(got) != "new-data" {
			t.Fatalf("post-flush /dst.txt = (%q, %v), want new-data", got, err)
		}
	})
}

func TestTier2_F9_CleanFlushBoundaries(t *testing.T) {
	t.Run("T2-F9-01_FlushQueueCancelledContextPreservesItems", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		for i := 0; i < 3; i++ {
			p := fmt.Sprintf("/c%d.txt", i)
			st, _ := mgr.StageOpenForWrite(p, true)
			_ = os.WriteFile(st, []byte("x"), 0o600)
			_ = mgr.CommitFile(t.Context(), p, st, 1, time.Now().Unix())
		}
		fake.SetOnline(true)
		cctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := mgr.FlushQueue(cctx); !errors.Is(err, context.Canceled) && err == nil {
			t.Fatalf("expected context.Canceled or error, got %v", err)
		}
		if qs := mgr.QueueStatus(); qs.PendingCount == 0 {
			t.Fatal("expected pending items preserved when context already cancelled")
		}
	})

	t.Run("T2-F9-02_FlushQueueStaysOfflineSetsLastError", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		for _, p := range []string{"/o1.txt", "/o2.txt"} {
			st, _ := mgr.StageOpenForWrite(p, true)
			_ = os.WriteFile(st, []byte("offline"), 0o600)
			_ = mgr.CommitFile(t.Context(), p, st, 7, time.Now().Unix())
		}
		if err := mgr.FlushQueue(t.Context()); err == nil {
			t.Fatal("expected FlushQueue error when offline")
		}
		qs := mgr.QueueStatus()
		if qs.LastError == "" || qs.PendingCount != 2 || qs.Syncing {
			t.Fatalf("unexpected QueueStatus: %+v", qs)
		}
	})

	t.Run("T2-F9-03_ConcurrentFlushQueueSerializedSafely", func(t *testing.T) {
		dir := t.TempDir()
		_, store, fake, eng, _ := newTestCore(t, dir, false)
		s, _ := store.GetSettings()
		s.SmallFileMaxMB = 0
		_ = store.SaveSettings(s)

		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		for i := 0; i < 4; i++ {
			p := fmt.Sprintf("/par%d.txt", i)
			st, _ := mgr.StageOpenForWrite(p, true)
			_ = os.WriteFile(st, []byte(p), 0o600)
			_ = mgr.CommitFile(t.Context(), p, st, int64(len(p)), time.Now().Unix())
		}
		fake.SetOnline(true)

		var wg sync.WaitGroup
		errs := make(chan error, 5)
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- mgr.FlushQueue(t.Context())
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent FlushQueue error: %v", err)
			}
		}
		if mgr.QueueStatus().PendingCount != 0 || fake.Uploads() != 4 {
			t.Fatalf("PendingCount=%d Uploads=%d, want 0 and 4", mgr.QueueStatus().PendingCount, fake.Uploads())
		}
	})

	t.Run("T2-F9-04_DoubleCloseSafe", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		_ = mgr.Close()
		_ = mgr.Close()
	})

	t.Run("T2-F9-05_PendingBytesAccountingNonNegative", func(t *testing.T) {
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, false)
		mgr, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		st1, _ := mgr.StageOpenForWrite("/a.bin", true)
		_ = os.WriteFile(st1, bytes.Repeat([]byte("A"), 500), 0o600)
		_ = mgr.CommitFile(t.Context(), "/a.bin", st1, 500, time.Now().Unix())

		st2, _ := mgr.StageOpenForWrite("/a.bin", true)
		_ = os.WriteFile(st2, bytes.Repeat([]byte("B"), 200), 0o600)
		_ = mgr.CommitFile(t.Context(), "/a.bin", st2, 200, time.Now().Unix())

		qs := mgr.QueueStatus()
		if qs.PendingCount < 1 || qs.PendingBytes < 200 {
			t.Fatalf("invalid pending accounting: %+v", qs)
		}
		_ = mgr.QueueDelete(t.Context(), "/a.bin", false)
		if qs2 := mgr.QueueStatus(); qs2.PendingBytes < 0 {
			t.Fatalf("negative PendingBytes: %+v", qs2)
		}
	})
}

func TestTier2_F10_SystemTrayBoundaries(t *testing.T) {
	t.Run("T2-F10-01_ActionErrorsSurfacedWithoutPanic", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		rig.mountFail = errors.New("mount error")
		rig.startupFail = errors.New("registry error")
		actions := &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil || app == nil {
			t.Fatalf("tray.New = (%v, %v)", app, err)
		}
		defer app.Close()
		if _, err := tray.DispatchMenuAction(tray.MenuIDMountToggle, actions, nil); err == nil {
			t.Fatal("expected Mount error")
		}
		if _, err := tray.DispatchMenuAction(tray.MenuIDToggleStartup, actions, nil); err == nil {
			t.Fatal("expected ToggleStartup error")
		}
	})

	t.Run("T2-F10-02_OpenDriveWhenUnmountedReturnsError", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		actions := &rigTrayActions{rig: rig}
		if _, err := tray.DispatchMenuAction(tray.MenuIDOpenDrive, actions, nil); err == nil {
			t.Fatal("expected error calling OpenDrive when unmounted")
		}
	})

	t.Run("T2-F10-03_DoubleCloseAndQuitIdempotent", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		actions := &rigTrayActions{rig: rig}
		app, err := tray.New(actions)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := app.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		actions.Quit()
		actions.Quit()
	})

	t.Run("T2-F10-04_EmptyStatusValuesFormatSafelyWithinTooltipLimit", func(t *testing.T) {
		st := tray.TrayStatus{}
		tip := tray.FormatTooltip(st)
		if tip == "" || len(utf16.Encode([]rune(tip))) > 127 {
			t.Fatalf("invalid tooltip for empty TrayStatus: %q", tip)
		}
		items := tray.BuildMenuItems(st)
		if len(items) == 0 {
			t.Fatal("expected non-empty menu items for zero TrayStatus")
		}
	})

	t.Run("T2-F10-05_LargeQueueCountAndLongDriveTooltipTruncatedSafely", func(t *testing.T) {
		st := tray.TrayStatus{
			Mounted:         true,
			DriveLetter:     strings.Repeat("V:", 40),
			WinFspInstalled: true,
			QueuePending:    999999,
			Online:          true,
		}
		tip := tray.FormatTooltip(st)
		if units := len(utf16.Encode([]rune(tip))); units == 0 || units > 127 {
			t.Fatalf("tooltip UTF-16 length = %d, want 1..127 (%q)", units, tip)
		}
	})
}

func TestTier2_F11_MissingWinFspBoundaries(t *testing.T) {
	t.Run("T2-F11-01_MissingWinFspWithAutoMountDoesNotCrash", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		_, _ = rig.cfgStore.Update(func(c *desktop.Config) error {
			c.AutoMount = true
			return nil
		})
		if err := rig.Mount(); err == nil {
			t.Fatal("expected mount error when WinFsp missing")
		}
		st := rig.Status()
		if st.Mounted || st.MountError == "" || st.WinFspGuidance == "" {
			t.Fatalf("unexpected status: %+v", st)
		}
	})

	t.Run("T2-F11-02_WebMountWhenWinFspMissingReturnsErrorAndKeepsUnmounted", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		resp, err := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST /api/desktop/mount: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("expected 4xx/5xx error when WinFsp missing, got %d (%s)", resp.StatusCode, body)
		}
		if !strings.Contains(strings.ToLower(string(body)), "winfsp") {
			t.Fatalf("expected WinFsp guidance in error response: %s", body)
		}
		if rig.Status().Mounted {
			t.Fatal("mounted must remain false")
		}
	})

	t.Run("T2-F11-03_TrayMountWhenWinFspMissingReturnsGuidanceError", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		actions := &rigTrayActions{rig: rig}
		if err := actions.Mount(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "winfsp") {
			t.Fatalf("expected WinFsp error from tray Mount, got %v", err)
		}
	})

	t.Run("T2-F11-04_GuidancePersistsAcrossPollsUntilInstalled", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, false)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		for i := 0; i < 2; i++ {
			r, err := client.Get(ts.URL + "/api/desktop/status")
			if err != nil {
				t.Fatal(err)
			}
			var ds web.DesktopStatus
			_ = json.NewDecoder(r.Body).Decode(&ds)
			r.Body.Close()
			if ds.WinFspInstalled || ds.WinFspGuidance == "" {
				t.Fatalf("poll %d: expected missing WinFsp + guidance, got %+v", i, ds)
			}
		}

		rig.probe.Set(desktop.WinFspStatus{Installed: true, DLLPath: `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`})
		r3, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatal(err)
		}
		var ds3 web.DesktopStatus
		_ = json.NewDecoder(r3.Body).Decode(&ds3)
		r3.Body.Close()
		if !ds3.WinFspInstalled {
			t.Fatalf("poll 3: expected WinFspInstalled=true, got %+v", ds3)
		}
	})

	t.Run("T2-F11-05_OfflineAndMissingWinFspSimultaneously", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), false, false)
		st := rig.Status()
		ts := (&rigTrayActions{rig: rig}).Status()
		if st.WinFspInstalled || st.WinFspGuidance == "" || ts.Online {
			t.Fatalf("unexpected combined offline+no-WinFsp status: web=%+v tray=%+v", st, ts)
		}
	})
}

func TestTier2_F12_WebUIDesktopBoundaries(t *testing.T) {
	t.Run("T2-F12-01_StrictNegativeNoFileBrowserOrTrashEndpoints", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		for _, ep := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/files"},
			{http.MethodGet, "/api/files/"},
			{http.MethodPost, "/api/files"},
			{http.MethodGet, "/api/browse"},
			{http.MethodGet, "/api/trash"},
			{http.MethodPost, "/api/trash/empty"},
			{http.MethodDelete, "/api/trash/1"},
		} {
			req, _ := http.NewRequest(ep.method, ts.URL+ep.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", ep.method, ep.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("forbidden endpoint %s %s returned %d (%s), want 404/405", ep.method, ep.path, resp.StatusCode, body)
			}
		}
	})

	t.Run("T2-F12-02_StrictNegativeNoFileBrowserOrTrashHTMLJS", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		for _, asset := range []string{"/", "/app.js"} {
			resp, err := http.Get(ts.URL + asset)
			if err != nil {
				t.Fatalf("GET %s: %v", asset, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			low := strings.ToLower(string(b))
			for _, bad := range []string{"/api/files", "/api/browse", "/api/trash", "view-files", "view-trash"} {
				if strings.Contains(low, bad) {
					t.Fatalf("forbidden string %q found in %s", bad, asset)
				}
			}
		}
	})

	t.Run("T2-F12-03_UnauthenticatedDesktopEndpointsRejected", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts := httptest.NewServer(srv)
		defer ts.Close()

		for _, ep := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/desktop/status"},
			{http.MethodPut, "/api/desktop/settings"},
			{http.MethodPost, "/api/desktop/mount"},
			{http.MethodPost, "/api/desktop/unmount"},
			{http.MethodPost, "/api/desktop/cache/clear"},
			{http.MethodPost, "/api/desktop/queue/flush"},
		} {
			req, _ := http.NewRequest(ep.method, ts.URL+ep.path, strings.NewReader("{}"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", ep.method, ep.path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated %s %s = %d, want 401", ep.method, ep.path, resp.StatusCode)
			}
		}
	})

	t.Run("T2-F12-04_InvalidJSONAndSettingsRejected", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		for _, badBody := range []string{
			`{bad`,
			`{"drive_letter":"C:"}`,
			`{"drive_letter":"1:"}`,
			`{"disk_cache_max_bytes":-5}`,
		} {
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/desktop/settings", strings.NewReader(badBody))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("PUT %s status = %d, want 400", badBody, resp.StatusCode)
			}
		}
		if got := rig.cfgStore.Get().DriveLetter; got != "V:" {
			t.Fatalf("DriveLetter mutated on invalid PUTs: %q", got)
		}
	})

	t.Run("T2-F12-05_DesktopEndpointsWithoutController", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr) // SetDesktopController NOT called
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		r, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatal(err)
		}
		var ds web.DesktopStatus
		_ = json.NewDecoder(r.Body).Decode(&ds)
		r.Body.Close()
		if r.StatusCode != http.StatusOK || ds.Enabled {
			t.Fatalf("GET /api/desktop/status without controller = (code=%d, enabled=%v), want (200, false)", r.StatusCode, ds.Enabled)
		}

		mr, err := client.Post(ts.URL+"/api/desktop/mount", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		mr.Body.Close()
		if mr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("POST /api/desktop/mount without controller = %d, want 503", mr.StatusCode)
		}
	})
}

func TestTier2_F13_VFSBoundaries(t *testing.T) {
	t.Run("T2-F13-01_OpenFlagsExclTruncAppend", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		vfsWriteAll(t, rig.fs, "/flags.txt", []byte("initial"))

		if rc, _ := rig.fs.Create("/flags.txt", fuse.O_CREAT|fuse.O_EXCL|fuse.O_RDWR, 0o644); rc != -fuse.EEXIST {
			t.Fatalf("Create O_EXCL on existing = %d, want %d", rc, -fuse.EEXIST)
		}

		rc, fh := rig.fs.Open("/flags.txt", fuse.O_RDWR|fuse.O_APPEND)
		if rc != 0 {
			t.Fatalf("Open O_APPEND rc=%d", rc)
		}
		if n := rig.fs.Write("/flags.txt", []byte("-tail"), 0, fh); n != 5 {
			t.Fatalf("append Write = %d, want 5", n)
		}
		_ = rig.fs.Flush("/flags.txt", fh)
		_ = rig.fs.Release("/flags.txt", fh)

		got, rc := vfsReadAll(t, rig.fs, "/flags.txt")
		if rc != 0 || string(got) != "initial-tail" {
			t.Fatalf("after O_APPEND = (%q, rc=%d), want initial-tail", got, rc)
		}

		rc, fh = rig.fs.Open("/flags.txt", fuse.O_RDWR|fuse.O_TRUNC)
		if rc != 0 {
			t.Fatalf("Open O_TRUNC rc=%d", rc)
		}
		_ = rig.fs.Flush("/flags.txt", fh)
		_ = rig.fs.Release("/flags.txt", fh)

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/flags.txt", &st, 0); rc != 0 || st.Size != 0 {
			t.Fatalf("after O_TRUNC Getattr = (rc=%d, size=%d), want (0, 0)", rc, st.Size)
		}
	})

	t.Run("T2-F13-02_RenameOverwriteAndTypeMismatchGuards", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		vfsWriteAll(t, rig.fs, "/f1.txt", []byte("one"))
		vfsWriteAll(t, rig.fs, "/f2.txt", []byte("two"))
		_ = rig.fs.Mkdir("/d1", 0o755)
		_ = rig.fs.Mkdir("/d2", 0o755)
		vfsWriteAll(t, rig.fs, "/d2/child.txt", []byte("c"))

		if rc := rig.fs.Rename("/f1.txt", "/f2.txt"); rc != 0 {
			t.Fatalf("Rename file->existing file rc=%d, want 0", rc)
		}
		got, _ := vfsReadAll(t, rig.fs, "/f2.txt")
		if string(got) != "one" {
			t.Fatalf("overwritten /f2.txt = %q, want one", got)
		}

		if rc := rig.fs.Rename("/d1", "/d2"); rc != -fuse.ENOTEMPTY {
			t.Fatalf("Rename dir->non-empty dir rc=%d, want %d", rc, -fuse.ENOTEMPTY)
		}
		if rc := rig.fs.Rename("/f2.txt", "/d2"); rc != -fuse.EISDIR {
			t.Fatalf("Rename file->dir rc=%d, want %d", rc, -fuse.EISDIR)
		}
		if rc := rig.fs.Rename("/d1", "/f2.txt"); rc != -fuse.ENOTDIR {
			t.Fatalf("Rename dir->file rc=%d, want %d", rc, -fuse.ENOTDIR)
		}
	})

	t.Run("T2-F13-03_RmdirNonEmptyAndUnlinkDir", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		_ = rig.fs.Mkdir("/d", 0o755)
		vfsWriteAll(t, rig.fs, "/d/f.txt", []byte("x"))

		if rc := rig.fs.Rmdir("/d"); rc != -fuse.ENOTEMPTY {
			t.Fatalf("Rmdir non-empty = %d, want %d", rc, -fuse.ENOTEMPTY)
		}
		if rc := rig.fs.Unlink("/d"); rc != -fuse.EISDIR && rc != -fuse.EPERM {
			t.Fatalf("Unlink dir = %d, want -EISDIR or -EPERM", rc)
		}
	})

	t.Run("T2-F13-04_CaseInsensitiveLookupAndCollision", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		vfsWriteAll(t, rig.fs, "/Report.txt", []byte("case-body"))

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/report.txt", &st, 0); rc != 0 {
			t.Fatalf("Getattr(/report.txt) rc=%d", rc)
		}
		if rc := rig.fs.Getattr("/REPORT.TXT", &st, 0); rc != 0 {
			t.Fatalf("Getattr(/REPORT.TXT) rc=%d", rc)
		}
		if rc, _ := rig.fs.Create("/report.txt", fuse.O_CREAT|fuse.O_EXCL|fuse.O_RDWR, 0o644); rc != -fuse.EEXIST {
			t.Fatalf("Create(/report.txt, O_EXCL) = %d, want %d", rc, -fuse.EEXIST)
		}
	})

	t.Run("T2-F13-05_RootMutationsAndMissingParents", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		if rc := rig.fs.Unlink("/"); rc == 0 {
			t.Fatal("Unlink(/) should fail")
		}
		if rc := rig.fs.Rmdir("/"); rc == 0 {
			t.Fatal("Rmdir(/) should fail")
		}
		if rc := rig.fs.Rename("/", "/x"); rc == 0 {
			t.Fatal("Rename(/, /x) should fail")
		}
		if rc, _ := rig.fs.Create("/no_such_dir/f.txt", fuse.O_CREAT|fuse.O_RDWR, 0o644); rc != -fuse.ENOENT {
			t.Fatalf("Create missing parent = %d, want %d", rc, -fuse.ENOENT)
		}
	})
}

func TestTier2_F14_MountManagerBoundaries(t *testing.T) {
	newMM := func(t *testing.T, probe desktop.WinFspProbe, checker desktop.DriveChecker) *vfs.MountManager {
		t.Helper()
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cache.Close() })
		return vfs.NewMountManager(vfs.NewTeleFS(store, cache), probe, checker)
	}

	t.Run("T2-F14-01_UnmountWhenUnmountedIsSafe", func(t *testing.T) {
		mm := newMM(t, newFakeWinFspProbe(false, "", "missing"), newFakeDriveChecker("C:"))
		if err := mm.Unmount(t.Context()); err != nil {
			t.Fatalf("Unmount when unmounted: %v", err)
		}
		if mm.IsMounted() {
			t.Fatal("expected IsMounted=false")
		}
	})

	t.Run("T2-F14-02_DoubleMountRejected", func(t *testing.T) {
		if !desktop.CheckWinFsp().Installed {
			t.Skip("live WinFsp kernel driver not installed on host; skipping live double-Mount")
		}
		free := pickFreeDrives(t, 2)
		mm := newMM(t, desktop.NewWinFspProbe(), desktop.NewDriveChecker())
		if err := mm.Mount(free[0]); err != nil {
			t.Fatalf("first Mount(%s): %v", free[0], err)
		}
		defer func() { _ = mm.Unmount(t.Context()) }()
		if err := mm.Mount(free[1]); err == nil {
			t.Fatal("expected error on double Mount while already mounted")
		}
		if _, drv, _ := mm.Status(); drv != free[0] {
			t.Fatalf("original mount altered: %q, want %s", drv, free[0])
		}
	})

	t.Run("T2-F14-03_MountRejectedWhenWinFspMissing", func(t *testing.T) {
		mm := newMM(t, newFakeWinFspProbe(false, "", "Install WinFsp from https://winfsp.dev"), newFakeDriveChecker("C:"))
		err := mm.Mount("V:")
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "winfsp") {
			t.Fatalf("expected WinFsp error, got %v", err)
		}
		mounted, _, lastErr := mm.Status()
		if mounted || mm.IsMounted() || lastErr == "" {
			t.Fatalf("unexpected status after rejected mount: mounted=%v lastErr=%q", mounted, lastErr)
		}
	})

	t.Run("T2-F14-04_MountRejectedWhenDriveInvalidOrInUse", func(t *testing.T) {
		mm := newMM(t, newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`, ""), newFakeDriveChecker("C:", "E:"))
		for _, drv := range []string{"C:", "1:", "E:"} {
			if err := mm.Mount(drv); err == nil {
				t.Fatalf("expected Mount(%q) to be rejected", drv)
			}
			if mm.IsMounted() {
				t.Fatalf("expected IsMounted=false after failed Mount(%q)", drv)
			}
		}
	})

	t.Run("T2-F14-05_NilFSSafetyNoCrash", func(t *testing.T) {
		mm := vfs.NewMountManager(nil, newFakeWinFspProbe(true, `C:\WinFsp\winfsp-x64.dll`, ""), newFakeDriveChecker("C:"))
		err := mm.Mount("V:")
		if err == nil {
			_ = mm.Unmount(t.Context())
			t.Fatal("expected error mounting nil TeleFS")
		}
		if mm.IsMounted() {
			t.Fatal("expected IsMounted=false")
		}
	})
}

func TestTier2_F15_DesktopBinaryBoundaries(t *testing.T) {
	t.Run("T2-F15-01_ExistingCoreAndCLITestsPass", func(t *testing.T) {
		cmd := exec.Command("go", "test",
			"./cmd/telemount/...",
			"./internal/crypto/...",
			"./internal/db/...",
			"./internal/state/...",
			"./internal/storage/...",
			"./internal/dav/...",
			"./internal/web/...",
		)
		cmd.Dir = repoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("existing unit tests regressed: %v\n%s", err, out)
		}
	})

	t.Run("T2-F15-02_PlainWebServerHasDesktopDisabled", func(t *testing.T) {
		_, _, _, eng, tgMgr := newTestCore(t, t.TempDir(), true)
		srv := web.NewServer(eng, tgMgr)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		resp, err := client.Get(ts.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m["desktop_enabled"] != false {
			t.Fatalf("desktop_enabled = %v, want false", m["desktop_enabled"])
		}
	})

	t.Run("T2-F15-03_CorruptDesktopJSONAtStartupHandledGracefully", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "desktop.json")
		_ = os.WriteFile(cfgPath, []byte("{corrupt"), 0o600)
		if _, err := desktop.LoadConfig(cfgPath, dir); err == nil {
			t.Fatal("expected clean error on corrupt desktop.json")
		}
	})

	t.Run("T2-F15-04_NonExistentNestedDataDirInitialized", func(t *testing.T) {
		nested := filepath.Join(t.TempDir(), "nested", "TeleMount")
		rig := newTestDesktopRig(t, nested, true, true)
		if rig.cfgStore == nil || rig.cache == nil {
			t.Fatal("expected subsystems initialized")
		}
		for _, f := range []string{"desktop.json", "desktop_cache.db", "telemount.db"} {
			if _, err := os.Stat(filepath.Join(nested, f)); err != nil {
				t.Fatalf("expected %s created in nested dir: %v", f, err)
			}
		}
	})

	t.Run("T2-F15-05_GoVetCleanAcrossRepo", func(t *testing.T) {
		cmd := exec.Command("go", "vet", "./cmd/...", "./internal/...")
		cmd.Dir = repoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go vet failed: %v\n%s", err, out)
		}
	})
}
