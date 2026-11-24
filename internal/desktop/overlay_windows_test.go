//go:build windows

package desktop

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modKernel32Test      = windows.NewLazySystemDLL("kernel32.dll")
	procCallNamedPipeW   = modKernel32Test.NewProc("CallNamedPipeW")
	procOpenFileMappingW = modKernel32Test.NewProc("OpenFileMappingW")
)

func TestOverlayServerSharedMemoryAndPipe(t *testing.T) {
	pid := os.Getpid()
	shmName := fmt.Sprintf(`Local\TeleMountShellStateTest_%d`, pid)
	pipeName := fmt.Sprintf(`\\.\pipe\TeleMountOverlayTest_%d`, pid)

	var mu sync.RWMutex
	states := map[string]byte{
		"/test/pinned.apk": 'P',
		"/test/cached.apk": 'C',
		"/test/cloud.apk":  'O',
		"/test/dirty.apk":  'S',
	}

	srv, err := startOverlayServerNamed(shmName, pipeName, func(vp string) byte {
		mu.RLock()
		defer mu.RUnlock()
		if st, ok := states[vp]; ok {
			return st
		}
		return 'N'
	})
	if err != nil {
		t.Fatalf("startOverlayServerNamed: %v", err)
	}
	defer srv.Close()

	srv.SetDrive("V:")

	// Verify shared memory header written by OverlayServer matches telemount_shell.cpp layout.
	namePtr, _ := windows.UTF16PtrFromString(shmName)
	rMap, _, mapErr := procOpenFileMappingW.Call(windows.FILE_MAP_READ, 0, uintptr(unsafe.Pointer(namePtr)))
	if rMap == 0 {
		t.Fatalf("OpenFileMappingW: %v", mapErr)
	}
	hMap := windows.Handle(rMap)
	defer windows.CloseHandle(hMap)

	view, err := windows.MapViewOfFile(hMap, windows.FILE_MAP_READ, 0, 0, unsafe.Sizeof(sharedShellState{}))
	if err != nil {
		t.Fatalf("MapViewOfFile: %v", err)
	}
	defer windows.UnmapViewOfFile(view)

	hdr := *(**sharedShellState)(unsafe.Pointer(&view))
	if hdr.magic != overlaySharedMagic || hdr.version != overlaySharedVersion || hdr.driveLetter != 'V' {
		t.Fatalf("unexpected shared header: magic=0x%x ver=%d drive=%c", hdr.magic, hdr.version, hdr.driveLetter)
	}
	genBefore := hdr.generation

	// Verify Named Pipe CallNamedPipeW protocol for every state.
	pipePtr, _ := windows.UTF16PtrFromString(pipeName)
	queryPipe := func(vpath string) byte {
		req := []byte(vpath)
		var resp byte
		var read uint32
		r1, _, callErr := procCallNamedPipeW.Call(
			uintptr(unsafe.Pointer(pipePtr)),
			uintptr(unsafe.Pointer(&req[0])),
			uintptr(len(req)),
			uintptr(unsafe.Pointer(&resp)),
			1,
			uintptr(unsafe.Pointer(&read)),
			500,
		)
		if r1 == 0 || read != 1 {
			t.Fatalf("CallNamedPipeW(%q) failed: r1=%d read=%d err=%v", vpath, r1, read, callErr)
		}
		return resp
	}

	for path, want := range states {
		if got := queryPipe(path); got != want {
			t.Fatalf("pipe(%q) = %c, want %c", path, got, want)
		}
	}
	if got := queryPipe("/unknown.txt"); got != 'N' {
		t.Fatalf("pipe(/unknown.txt) = %c, want N", got)
	}

	// Verify NotifyPaths bumps shared generation counter so Explorer cache invalidates immediately.
	srv.NotifyPaths("/test/cloud.apk")
	if hdr.generation <= genBefore {
		t.Fatalf("expected generation to increment: before=%d after=%d", genBefore, hdr.generation)
	}

	// Verify Close zeroes driveLetter cleanly.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if hdr.driveLetter != 0 {
		t.Fatalf("expected driveLetter cleared to 0 after Close, got %c", hdr.driveLetter)
	}

	// Regression test: while explorer.exe (simulated by our open hMap handle) keeps the shared
	// mapping alive across restarts/auto-updates, a new OverlayServer must attach despite
	// CreateFileMapping returning ERROR_ALREADY_EXISTS and serve the Named Pipe cleanly.
	srv2, err := startOverlayServerNamed(shmName, pipeName, func(vp string) byte {
		if vp == "/Test" {
			return 'O'
		}
		return 'N'
	})
	if err != nil {
		t.Fatalf("restart startOverlayServerNamed with existing mapping failed: %v", err)
	}
	defer srv2.Close()
	srv2.SetDrive("G:")
	if hdr.driveLetter != 'G' {
		t.Fatalf("restarted server driveLetter = %c, want G", hdr.driveLetter)
	}
	if got := queryPipe("/Test"); got != 'O' {
		t.Fatalf("restarted pipe(/Test) = %c, want O", got)
	}
}

func TestShellOverlayDLLIntegration(t *testing.T) {
	dllPath := os.Getenv("TELEMOUNT_TEST_SHELL_DLL")
	verifierExe := os.Getenv("TELEMOUNT_TEST_SHELL_VERIFIER")
	if dllPath == "" || verifierExe == "" {
		t.Skip("set TELEMOUNT_TEST_SHELL_DLL and TELEMOUNT_TEST_SHELL_VERIFIER to run live COM DLL verification")
	}

	var mu sync.RWMutex
	states := map[string]byte{
		"/test/pinned.apk":  'P',
		"/test/cached.apk":  'C',
		"/test/cloud.apk":   'O',
		"/test/syncing.apk": 'S',
	}

	srv, err := StartOverlayServer(func(vp string) byte {
		mu.RLock()
		defer mu.RUnlock()
		if st, ok := states[strings.ToLower(vp)]; ok {
			return st
		}
		return 'N'
	})
	if err != nil {
		t.Fatalf("StartOverlayServer: %v", err)
	}
	defer srv.Close()
	srv.SetDrive("V:")

	cmd := exec.Command(verifierExe, dllPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	sawPass := false
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		t.Log("[DLL]", line)
		if line == "PHASE1_OK" {
			mu.Lock()
			states["/test/cloud.apk"] = 'P'
			mu.Unlock()
			srv.NotifyPaths("/Test/cloud.apk")
			_, _ = stdin.Write([]byte("\n"))
		} else if line == "COM_DLL_VERIFY_PASSED" {
			sawPass = true
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("COM verifier exited with error: %v", err)
	}
	if !sawPass {
		t.Fatal("missing COM_DLL_VERIFY_PASSED marker")
	}
}
