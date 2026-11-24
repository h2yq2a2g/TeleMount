//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/db"
	"telemount/internal/desktop"
	"telemount/internal/diskcache"
	"telemount/internal/vfs"
	"telemount/internal/web"
)

func TestTier1_F13_WinFspVFS(t *testing.T) {
	t.Run("T1-F13-01_DirectoryOpsMkdirReaddirGetattrRmdir", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		if rc := rig.fs.Mkdir("/projects", 0o755); rc != 0 {
			t.Fatalf("Mkdir(/projects) rc=%d", rc)
		}
		names, rc := vfsListNames(t, rig.fs, "/")
		if rc != 0 {
			t.Fatalf("Readdir(/) rc=%d", rc)
		}
		for _, want := range []string{".", "..", "projects"} {
			if !slices.Contains(names, want) {
				t.Fatalf("Readdir(/) = %v, missing %q", names, want)
			}
		}
		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/projects", &st, 0); rc != 0 {
			t.Fatalf("Getattr(/projects) rc=%d", rc)
		}
		if st.Mode&fuse.S_IFDIR == 0 {
			t.Fatalf("expected S_IFDIR in mode %o", st.Mode)
		}
		if rc := rig.fs.Rmdir("/projects"); rc != 0 {
			t.Fatalf("Rmdir(/projects) rc=%d", rc)
		}
		if rc := rig.fs.Getattr("/projects", &st, 0); rc != -fuse.ENOENT {
			t.Fatalf("post-Rmdir Getattr rc=%d, want %d", rc, -fuse.ENOENT)
		}
	})

	t.Run("T1-F13-02_CreateWriteFlushReleaseOpenRead", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		rc, fh := rig.fs.Create("/hello.txt", fuse.O_CREAT|fuse.O_RDWR, 0o644)
		if rc != 0 {
			t.Fatalf("Create rc=%d", rc)
		}
		msg := []byte("hello world")
		if n := rig.fs.Write("/hello.txt", msg, 0, fh); n != len(msg) {
			t.Fatalf("Write = %d, want %d", n, len(msg))
		}
		if rc := rig.fs.Flush("/hello.txt", fh); rc != 0 {
			t.Fatalf("Flush rc=%d", rc)
		}
		if rc := rig.fs.Release("/hello.txt", fh); rc != 0 {
			t.Fatalf("Release rc=%d", rc)
		}

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/hello.txt", &st, 0); rc != 0 || st.Size != int64(len(msg)) {
			t.Fatalf("Getattr = (rc=%d, size=%d), want (0, %d)", rc, st.Size, len(msg))
		}
		got, rc := vfsReadAll(t, rig.fs, "/hello.txt")
		if rc != 0 || !bytes.Equal(got, msg) {
			t.Fatalf("Read = (%q, rc=%d), want %q", got, rc, msg)
		}
	})

	t.Run("T1-F13-03_RandomOffsetReadWriteAndTruncate", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		initial := bytes.Repeat([]byte("a"), 100)
		vfsWriteAll(t, rig.fs, "/rand.bin", initial)

		rc, fh := rig.fs.Open("/rand.bin", fuse.O_RDWR)
		if rc != 0 {
			t.Fatalf("Open rc=%d", rc)
		}
		if n := rig.fs.Write("/rand.bin", []byte("ABCDE"), 20, fh); n != 5 {
			t.Fatalf("random Write = %d, want 5", n)
		}
		if rc := rig.fs.Truncate("/rand.bin", 50, fh); rc != 0 {
			t.Fatalf("Truncate(50) rc=%d", rc)
		}
		if rc := rig.fs.Flush("/rand.bin", fh); rc != 0 {
			t.Fatalf("Flush rc=%d", rc)
		}
		if rc := rig.fs.Release("/rand.bin", fh); rc != 0 {
			t.Fatalf("Release rc=%d", rc)
		}

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/rand.bin", &st, 0); rc != 0 || st.Size != 50 {
			t.Fatalf("Getattr after truncate = (rc=%d, size=%d), want (0, 50)", rc, st.Size)
		}
		got, rc := vfsReadAll(t, rig.fs, "/rand.bin")
		if rc != 0 || len(got) != 50 {
			t.Fatalf("Read after truncate = (len=%d, rc=%d)", len(got), rc)
		}
		if string(got[20:25]) != "ABCDE" {
			t.Fatalf("got[20:25] = %q, want ABCDE", got[20:25])
		}
	})

	t.Run("T1-F13-04_RenameAndUnlink", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		data := []byte("rename-and-unlink-payload")
		vfsWriteAll(t, rig.fs, "/a.txt", data)

		if rc := rig.fs.Rename("/a.txt", "/b.txt"); rc != 0 {
			t.Fatalf("Rename rc=%d", rc)
		}
		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/a.txt", &st, 0); rc != -fuse.ENOENT {
			t.Fatalf("old path Getattr rc=%d, want %d", rc, -fuse.ENOENT)
		}
		got, rc := vfsReadAll(t, rig.fs, "/b.txt")
		if rc != 0 || !bytes.Equal(got, data) {
			t.Fatalf("new path Read = (%q, rc=%d)", got, rc)
		}
		if rc := rig.fs.Unlink("/b.txt"); rc != 0 {
			t.Fatalf("Unlink rc=%d", rc)
		}
		if rc := rig.fs.Getattr("/b.txt", &st, 0); rc != -fuse.ENOENT {
			t.Fatalf("post-Unlink Getattr rc=%d, want %d", rc, -fuse.ENOENT)
		}
	})

	t.Run("T1-F13-05_StatfsUtimensChmodGetpathCaseInsensitive", func(t *testing.T) {
		rig := newTestDesktopRig(t, t.TempDir(), true, true)
		vfsWriteAll(t, rig.fs, "/f.txt", []byte("meta"))

		var sfs fuse.Statfs_t
		if rc := rig.fs.Statfs("/", &sfs); rc != 0 || sfs.Bsize == 0 || sfs.Blocks == 0 {
			t.Fatalf("Statfs = (rc=%d, %+v)", rc, sfs)
		}

		const wantSec int64 = 1711111111
		tmsp := []fuse.Timespec{{Sec: wantSec}, {Sec: wantSec}}
		if rc := rig.fs.Utimens("/f.txt", tmsp); rc != 0 {
			t.Fatalf("Utimens rc=%d", rc)
		}
		if rc := rig.fs.Chmod("/f.txt", 0o644); rc != 0 {
			t.Fatalf("Chmod rc=%d", rc)
		}

		var st fuse.Stat_t
		if rc := rig.fs.Getattr("/F.TXT", &st, 0); rc != 0 {
			t.Fatalf("case-insensitive Getattr(/F.TXT) rc=%d", rc)
		}
		if st.Mtim.Sec != wantSec {
			t.Fatalf("Mtim.Sec = %d, want %d", st.Mtim.Sec, wantSec)
		}

		rc, canon := rig.fs.Getpath("/F.TXT", 0)
		if rc != 0 || canon != "/f.txt" {
			t.Fatalf("Getpath(/F.TXT) = (%d, %q), want (0, /f.txt)", rc, canon)
		}
	})
}

func TestTier1_F14_MountManager(t *testing.T) {
	newMM := func(t *testing.T, probe desktop.WinFspProbe, checker desktop.DriveChecker) *vfs.MountManager {
		t.Helper()
		dir := t.TempDir()
		_, store, _, eng, _ := newTestCore(t, dir, true)
		cache, err := diskcache.Open(filepath.Join(dir, "cache"), filepath.Join(dir, "desktop_cache.db"), desktop.DefaultDiskCacheMaxBytes, store, eng)
		if err != nil {
			t.Fatalf("diskcache.Open: %v", err)
		}
		t.Cleanup(func() { _ = cache.Close() })
		return vfs.NewMountManager(vfs.NewTeleFS(store, cache), probe, checker)
	}

	t.Run("T1-F14-01_PreFlightCheckWinFspSucceedsWhenInstalled", func(t *testing.T) {
		probe := newFakeWinFspProbe(true, `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`, "")
		mm := newMM(t, probe, newFakeDriveChecker("C:"))
		st := mm.CheckWinFsp()
		if !st.Installed || st.DLLPath == "" {
			t.Fatalf("CheckWinFsp = %+v, want Installed=true", st)
		}
	})

	t.Run("T1-F14-02_MountTransitionsToMounted", func(t *testing.T) {
		if !desktop.CheckWinFsp().Installed {
			t.Skip("live WinFsp kernel driver not installed on host; skipping live Mount()")
		}
		drv1 := pickFreeDrives(t, 1)[0]
		mm := newMM(t, desktop.NewWinFspProbe(), desktop.NewDriveChecker())
		if err := mm.Mount(drv1); err != nil {
			t.Fatalf("Mount(%s): %v", drv1, err)
		}
		defer func() { _ = mm.Unmount(context.Background()) }()
		mounted, drv, lastErr := mm.Status()
		if !mm.IsMounted() || !mounted || drv != drv1 || lastErr != "" {
			t.Fatalf("Status = (%v, %q, %q), want (true, %s, '')", mounted, drv, lastErr, drv1)
		}
	})

	t.Run("T1-F14-03_MountNormalizesDriveLetter", func(t *testing.T) {
		if !desktop.CheckWinFsp().Installed {
			t.Skip("live WinFsp kernel driver not installed on host; skipping live Mount()")
		}
		drv1 := pickFreeDrives(t, 1)[0]
		raw := strings.ToLower(drv1[:1]) + `:\`
		mm := newMM(t, desktop.NewWinFspProbe(), desktop.NewDriveChecker())
		if err := mm.Mount(raw); err != nil {
			t.Fatalf("Mount(%s): %v", raw, err)
		}
		defer func() { _ = mm.Unmount(context.Background()) }()
		_, drv, _ := mm.Status()
		if drv != drv1 {
			t.Fatalf("Status drive = %q, want %s", drv, drv1)
		}
	})

	t.Run("T1-F14-04_CleanUnmountTransitionsToUnmounted", func(t *testing.T) {
		mm := newMM(t, newFakeWinFspProbe(false, "", "missing"), newFakeDriveChecker("C:"))
		if err := mm.Unmount(context.Background()); err != nil {
			t.Fatalf("Unmount when unmounted: %v", err)
		}
		if mm.IsMounted() {
			t.Fatal("expected IsMounted=false")
		}
		if !desktop.CheckWinFsp().Installed {
			return
		}
		drv1 := pickFreeDrives(t, 1)[0]
		mmLive := newMM(t, desktop.NewWinFspProbe(), desktop.NewDriveChecker())
		if err := mmLive.Mount(drv1); err != nil {
			t.Fatalf("Mount(%s): %v", drv1, err)
		}
		if err := mmLive.Unmount(context.Background()); err != nil {
			t.Fatalf("Unmount: %v", err)
		}
		if mmLive.IsMounted() {
			t.Fatal("expected IsMounted=false after Unmount")
		}
	})

	t.Run("T1-F14-05_RemountOnDifferentDriveAfterUnmount", func(t *testing.T) {
		if !desktop.CheckWinFsp().Installed {
			t.Skip("live WinFsp kernel driver not installed on host; skipping live Mount()")
		}
		free := pickFreeDrives(t, 2)
		mm := newMM(t, desktop.NewWinFspProbe(), desktop.NewDriveChecker())
		if err := mm.Mount(free[0]); err != nil {
			t.Fatalf("Mount(%s): %v", free[0], err)
		}
		if err := mm.Unmount(context.Background()); err != nil {
			t.Fatalf("Unmount: %v", err)
		}
		if err := mm.Mount(free[1]); err != nil {
			t.Fatalf("Mount(%s): %v", free[1], err)
		}
		defer func() { _ = mm.Unmount(context.Background()) }()
		mounted, drv, _ := mm.Status()
		if !mounted || drv != free[1] {
			t.Fatalf("Status = (%v, %q), want (true, %s)", mounted, drv, free[1])
		}
	})
}

func TestTier1_F15_DesktopAndCLIBinaries(t *testing.T) {
	t.Run("T1-F15-01_CLIBuildsCleanlyWithoutCGO", func(t *testing.T) {
		outBin := filepath.Join(t.TempDir(), "telemount.exe")
		cmd := exec.Command("go", "build", "-o", outBin, "./cmd/telemount")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build ./cmd/telemount failed: %v\n%s", err, out)
		}
		if info, err := os.Stat(outBin); err != nil || info.Size() == 0 {
			t.Fatalf("expected non-empty output binary, err=%v", err)
		}
	})

	t.Run("T1-F15-02_UnifiedBinaryBuildsForWindowsAndLinuxPortainerWithoutCGO", func(t *testing.T) {
		winBin := filepath.Join(t.TempDir(), "telemount.exe")
		cmdWin := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", winBin, "./cmd/telemount")
		cmdWin.Dir = repoRoot(t)
		cmdWin.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
		if out, err := cmdWin.CombinedOutput(); err != nil {
			t.Fatalf("go build windows ./cmd/telemount failed: %v\n%s", err, out)
		}
		if info, err := os.Stat(winBin); err != nil || info.Size() == 0 {
			t.Fatalf("expected non-empty windows binary, err=%v", err)
		}

		linuxBin := filepath.Join(t.TempDir(), "telemount-linux-arm64")
		cmdLinux := exec.Command("go", "build", "-o", linuxBin, "./cmd/telemount")
		cmdLinux.Dir = repoRoot(t)
		cmdLinux.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
		if out, err := cmdLinux.CombinedOutput(); err != nil {
			t.Fatalf("go build linux/arm64 ./cmd/telemount failed: %v\n%s", err, out)
		}
		if info, err := os.Stat(linuxBin); err != nil || info.Size() == 0 {
			t.Fatalf("expected non-empty linux binary, err=%v", err)
		}
	})

	t.Run("T1-F15-03_DesktopSubsystemBootstrapIsolatedDataDir", func(t *testing.T) {
		dataDir := t.TempDir()
		rig := newTestDesktopRig(t, dataDir, true, true)
		srv := web.NewServer(rig.engine, rig.tgMgr)
		srv.SetDesktopController(rig)
		ts, client := newAuthedWebClient(t, srv, "admin", "adminpass123")

		r1, err := client.Get(ts.URL + "/api/status")
		if err != nil {
			t.Fatalf("GET /api/status: %v", err)
		}
		var st map[string]any
		_ = json.NewDecoder(r1.Body).Decode(&st)
		r1.Body.Close()
		if st["desktop_enabled"] != true {
			t.Fatalf("desktop_enabled = %v, want true", st["desktop_enabled"])
		}

		r2, err := client.Get(ts.URL + "/api/desktop/status")
		if err != nil {
			t.Fatalf("GET /api/desktop/status: %v", err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("desktop status = %d, want 200", r2.StatusCode)
		}

		for _, fname := range []string{"desktop.json", "desktop_cache.db", "telemount.db"} {
			if _, err := os.Stat(filepath.Join(dataDir, fname)); err != nil {
				t.Fatalf("expected %s in isolated dataDir: %v", fname, err)
			}
		}
	})

	t.Run("T1-F15-04_LoopbackOnlyDefaultWebAddr", func(t *testing.T) {
		cfg := desktop.DefaultConfig(t.TempDir())
		host, port, err := net.SplitHostPort(cfg.WebAddr)
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", cfg.WebAddr, err)
		}
		if host != "127.0.0.1" || port == "" {
			t.Fatalf("WebAddr = %q, want 127.0.0.1:<port>", cfg.WebAddr)
		}
		if strings.Contains(cfg.WebAddr, "0.0.0.0") {
			t.Fatalf("WebAddr must not bind 0.0.0.0: %q", cfg.WebAddr)
		}
	})

	t.Run("T1-F15-05_CleanShutdownClosesDBCacheAndUnmounts", func(t *testing.T) {
		dataDir := t.TempDir()
		rig := newTestDesktopRig(t, dataDir, false, true)
		_ = rig.Mount()

		stPath, _ := rig.cache.StageOpenForWrite("/exit.txt", true)
		_ = os.WriteFile(stPath, []byte("exit-data"), 0o600)
		_ = rig.cache.CommitFile(t.Context(), "/exit.txt", stPath, 9, time.Now().Unix())
		rig.fakeTG.SetOnline(true)

		rig.Quit()
		if rig.Status().Mounted {
			t.Fatal("expected unmounted on Quit")
		}
		if err := rig.cache.Close(); err != nil {
			t.Fatalf("cache.Close: %v", err)
		}
		if err := rig.engine.Store().Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}

		// Verify DBs can be reopened cleanly (not locked or corrupted).
		reopenedDB, err := db.Open(filepath.Join(dataDir, "telemount.db"))
		if err != nil {
			t.Fatalf("reopen telemount.db: %v", err)
		}
		defer reopenedDB.Close()
		if _, err := reopenedDB.StatByPath("/exit.txt"); err != nil {
			t.Fatalf("expected /exit.txt flushed to telemount.db before shutdown: %v", err)
		}
	})
}
