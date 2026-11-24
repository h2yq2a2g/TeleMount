//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	overlaySharedMagic   uint32 = 0x31534D54 // "TMS1"
	overlaySharedVersion uint32 = 1
	overlaySharedName           = `Local\TeleMountShellState`

	shcneUpdateDir   = 0x00001000
	shcneUpdateItem  = 0x00002000
	shcnfPathW       = 0x0005
	shcnfFlushNoWait = 0x2000

	pipeAccessDuplex    = 0x00000003
	pipeTypeMessage     = 0x00000004
	pipeReadmodeMessage = 0x00000002
	pipeWait            = 0x00000000
)

// sharedShellState matches TeleMountSharedState in shell-ext/telemount_shell.cpp byte-for-byte.
type sharedShellState struct {
	magic       uint32
	version     uint32
	generation  int32
	driveLetter uint16
	reserved    uint16
}

var (
	modShell32Overlay     = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify    = modShell32Overlay.NewProc("SHChangeNotify")
	modKernel32Overlay    = windows.NewLazySystemDLL("kernel32.dll")
	procProcessIdToSessId = modKernel32Overlay.NewProc("ProcessIdToSessionId")
)

func currentOverlayPipeName() string {
	var sid uint32
	pid := windows.GetCurrentProcessId()
	_, _, _ = procProcessIdToSessId.Call(uintptr(pid), uintptr(unsafe.Pointer(&sid)))
	return fmt.Sprintf(`\\.\pipe\TeleMountOverlay_%d`, sid)
}

// OverlayServer serves Explorer shell icon overlay queries over a session-local
// Windows Named Pipe and advertises the active drive letter & cache generation via
// Local\TeleMountShellState shared memory.
type OverlayServer struct {
	query     OverlayQueryFunc
	pipeName  string
	shmHandle windows.Handle
	shmView   uintptr
	shm       *sharedShellState
	stopEvent windows.Handle

	mu        sync.Mutex
	drive     string
	closed    bool
	wg        sync.WaitGroup
}

// StartOverlayServer creates the shared state mapping and starts the background
// Named Pipe server for telemount_shell.dll.
func StartOverlayServer(query OverlayQueryFunc) (*OverlayServer, error) {
	return startOverlayServerNamed(overlaySharedName, currentOverlayPipeName(), query)
}

func startOverlayServerNamed(shmName, pipeName string, query OverlayQueryFunc) (*OverlayServer, error) {
	if query == nil {
		query = func(string) byte { return 'N' }
	}
	namePtr, err := windows.UTF16PtrFromString(shmName)
	if err != nil {
		return nil, err
	}
	hMap, err := windows.CreateFileMapping(
		windows.InvalidHandle,
		nil,
		windows.PAGE_READWRITE,
		0,
		uint32(unsafe.Sizeof(sharedShellState{})),
		namePtr,
	)
	// x/sys/windows returns ERROR_ALREADY_EXISTS alongside a valid handle when explorer.exe
	// (telemount_shell.dll) still holds Local\TeleMountShellState open across restarts/upgrades.
	if hMap == 0 || (err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		if hMap != 0 {
			_ = windows.CloseHandle(hMap)
		}
		return nil, fmt.Errorf("create shared overlay state: %w", err)
	}
	view, err := windows.MapViewOfFile(hMap, windows.FILE_MAP_WRITE|windows.FILE_MAP_READ, 0, 0, unsafe.Sizeof(sharedShellState{}))
	if err != nil {
		_ = windows.CloseHandle(hMap)
		return nil, fmt.Errorf("map shared overlay state: %w", err)
	}

	// MapViewOfFile returns an OS-managed address outside the Go heap.
	shm := *(**sharedShellState)(unsafe.Pointer(&view))
	shm.magic = overlaySharedMagic
	shm.version = overlaySharedVersion
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&shm.driveLetter)), 0)
	atomic.AddInt32(&shm.generation, 1)

	stopEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.UnmapViewOfFile(view)
		_ = windows.CloseHandle(hMap)
		return nil, fmt.Errorf("create overlay stop event: %w", err)
	}

	s := &OverlayServer{
		query:     query,
		pipeName:  pipeName,
		shmHandle: hMap,
		shmView:   view,
		shm:       shm,
		stopEvent: stopEv,
	}

	// Create the first Named Pipe instance synchronously so callers never race with startup.
	firstPipe, err := s.createPipeInstance()
	if err != nil {
		_ = windows.CloseHandle(stopEv)
		_ = windows.UnmapViewOfFile(view)
		_ = windows.CloseHandle(hMap)
		return nil, fmt.Errorf("create overlay named pipe: %w", err)
	}

	s.wg.Add(1)
	go s.acceptLoop(firstPipe)
	return s, nil
}

func (s *OverlayServer) createPipeInstance() (windows.Handle, error) {
	pipeNamePtr, err := windows.UTF16PtrFromString(s.pipeName)
	if err != nil {
		return 0, err
	}
	return windows.CreateNamedPipe(
		pipeNamePtr,
		pipeAccessDuplex|windows.FILE_FLAG_OVERLAPPED,
		pipeTypeMessage|pipeReadmodeMessage|pipeWait,
		windows.PIPE_UNLIMITED_INSTANCES,
		64,
		4096,
		50,
		nil,
	)
}

// SetDrive sets or clears (when empty) the active mounted drive letter visible to telemount_shell.dll.
func (s *OverlayServer) SetDrive(driveLetter string) {
	if s == nil {
		return
	}
	var letter uint16
	var norm string
	if strings.TrimSpace(driveLetter) != "" {
		if d, err := NormalizeDriveLetter(driveLetter); err == nil {
			norm = d
			letter = uint16(d[0])
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.drive = norm
	if s.shm != nil {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&s.shm.driveLetter)), uint32(letter))
		atomic.AddInt32(&s.shm.generation, 1)
	}
	s.mu.Unlock()

	if norm != "" {
		shChangeNotifyPath(shcneUpdateDir, norm+`\`)
	}
}

// NotifyPaths invalidates telemount_shell.dll's in-memory cache and nudges Explorer
// to refresh icon overlays for the given virtual paths (or the whole drive when "/" or empty).
func (s *OverlayServer) NotifyPaths(paths ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	drv := s.drive
	if s.shm != nil {
		atomic.AddInt32(&s.shm.generation, 1)
	}
	s.mu.Unlock()

	if drv == "" {
		return
	}
	if len(paths) == 0 || len(paths) > 24 {
		shChangeNotifyPath(shcneUpdateDir, drv+`\`)
		return
	}

	notifiedDirs := make(map[string]struct{}, 8)
	for _, raw := range paths {
		vp := NormalizeVirtualPath(raw)
		if vp == "/" {
			shChangeNotifyPath(shcneUpdateDir, drv+`\`)
			return
		}
		if winPath, err := VirtualPathToWindows(drv, vp); err == nil {
			shChangeNotifyPath(shcneUpdateItem, winPath)
		}
		parent := path.Dir(vp)
		if parent == "." || parent == "" {
			parent = "/"
		}
		lowParent := strings.ToLower(parent)
		if _, seen := notifiedDirs[lowParent]; !seen {
			notifiedDirs[lowParent] = struct{}{}
			if winParent, err := VirtualPathToWindows(drv, parent); err == nil {
				shChangeNotifyPath(shcneUpdateItem, winParent)
				shChangeNotifyPath(shcneUpdateDir, winParent)
			}
		}
	}
}

// Close clears the active drive letter, stops the Named Pipe listener, and releases handles.
func (s *OverlayServer) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.drive = ""
	if s.shm != nil {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&s.shm.driveLetter)), 0)
		atomic.AddInt32(&s.shm.generation, 1)
	}
	s.mu.Unlock()

	_ = windows.SetEvent(s.stopEvent)
	s.wg.Wait()

	if s.stopEvent != 0 {
		_ = windows.CloseHandle(s.stopEvent)
		s.stopEvent = 0
	}
	if s.shmView != 0 {
		_ = windows.UnmapViewOfFile(s.shmView)
		s.shmView = 0
		s.shm = nil
	}
	if s.shmHandle != 0 {
		_ = windows.CloseHandle(s.shmHandle)
		s.shmHandle = 0
	}
	return nil
}

func (s *OverlayServer) acceptLoop(nextPipe windows.Handle) {
	defer s.wg.Done()

	for {
		if w, _ := windows.WaitForSingleObject(s.stopEvent, 0); w == windows.WAIT_OBJECT_0 {
			if nextPipe != 0 {
				_ = windows.CloseHandle(nextPipe)
			}
			return
		}

		var hPipe windows.Handle
		if nextPipe != 0 {
			hPipe = nextPipe
			nextPipe = 0
		} else {
			var err error
			hPipe, err = s.createPipeInstance()
			if err != nil {
				// Brief wait on stopEvent before retrying transient OS errors.
				if w, _ := windows.WaitForSingleObject(s.stopEvent, 200); w == windows.WAIT_OBJECT_0 {
					return
				}
				continue
			}
		}

		connEv, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			_ = windows.CloseHandle(hPipe)
			return
		}
		ov := windows.Overlapped{HEvent: connEv}

		err = windows.ConnectNamedPipe(hPipe, &ov)
		connected := false
		if err == nil || err == windows.ERROR_PIPE_CONNECTED {
			connected = true
		} else if err == windows.ERROR_IO_PENDING {
			handles := []windows.Handle{connEv, s.stopEvent}
			w, werr := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
			if werr == nil && w == windows.WAIT_OBJECT_0 {
				var transferred uint32
				if windows.GetOverlappedResult(hPipe, &ov, &transferred, false) == nil {
					connected = true
				}
			} else {
				_ = windows.CancelIoEx(hPipe, &ov)
				_ = windows.CloseHandle(connEv)
				_ = windows.CloseHandle(hPipe)
				return
			}
		}
		_ = windows.CloseHandle(connEv)

		if !connected {
			_ = windows.CloseHandle(hPipe)
			continue
		}

		// Pre-create the next listening instance before handing off hPipe so the pipe never disappears between calls.
		if np, cerr := s.createPipeInstance(); cerr == nil {
			nextPipe = np
		}

		s.wg.Add(1)
		go func(h windows.Handle) {
			defer s.wg.Done()
			s.handleClient(h)
		}(hPipe)
	}
}

func (s *OverlayServer) handleClient(h windows.Handle) {
	defer func() {
		_ = windows.FlushFileBuffers(h)
		_ = windows.DisconnectNamedPipe(h)
		_ = windows.CloseHandle(h)
	}()

	var buf [4096]byte
	n, err := overlappedPipeRead(h, buf[:], 100)
	if err != nil || n == 0 {
		return
	}
	vpath := strings.TrimSpace(string(buf[:n]))
	state := byte('N')
	if vpath != "" {
		state = s.query(vpath)
	}
	if state == 0 {
		state = 'N'
	}
	_, _ = overlappedPipeWrite(h, []byte{state}, 100)
}

func overlappedPipeRead(h windows.Handle, buf []byte, timeoutMs uint32) (int, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)

	ov := windows.Overlapped{HEvent: ev}
	var done uint32
	err = windows.ReadFile(h, buf, &done, &ov)
	if err == nil || err == windows.ERROR_MORE_DATA {
		return int(done), nil
	}
	if err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	w, werr := windows.WaitForSingleObject(ev, timeoutMs)
	if werr != nil || w != windows.WAIT_OBJECT_0 {
		_ = windows.CancelIoEx(h, &ov)
		return 0, syscall.ETIMEDOUT
	}
	if err := windows.GetOverlappedResult(h, &ov, &done, false); err != nil && err != windows.ERROR_MORE_DATA {
		return 0, err
	}
	return int(done), nil
}

func overlappedPipeWrite(h windows.Handle, buf []byte, timeoutMs uint32) (int, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)

	ov := windows.Overlapped{HEvent: ev}
	var done uint32
	err = windows.WriteFile(h, buf, &done, &ov)
	if err == nil {
		return int(done), nil
	}
	if err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	w, werr := windows.WaitForSingleObject(ev, timeoutMs)
	if werr != nil || w != windows.WAIT_OBJECT_0 {
		_ = windows.CancelIoEx(h, &ov)
		return 0, syscall.ETIMEDOUT
	}
	if err := windows.GetOverlappedResult(h, &ov, &done, false); err != nil {
		return 0, err
	}
	return int(done), nil
}

func shChangeNotifyPath(eventID uint32, winPath string) {
	if winPath == "" {
		return
	}
	p, err := windows.UTF16PtrFromString(winPath)
	if err != nil {
		return
	}
	_, _, _ = procSHChangeNotify.Call(
		uintptr(eventID),
		uintptr(shcnfPathW|shcnfFlushNoWait),
		uintptr(unsafe.Pointer(p)),
		0,
	)
}
