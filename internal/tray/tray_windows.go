//go:build windows

package tray

import (
	"context"
	_ "embed"
	"errors"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32                  = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIconW     = shell32.NewProc("Shell_NotifyIconW")
	user32                   = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW     = user32.NewProc("RegisterClassExW")
	procUnregisterClassW     = user32.NewProc("UnregisterClassW")
	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procDefWindowProcW       = user32.NewProc("DefWindowProcW")
	procGetMessageW          = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessageW     = user32.NewProc("DispatchMessageW")
	procPostMessageW         = user32.NewProc("PostMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procRegisterWindowMsgW   = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu      = user32.NewProc("CreatePopupMenu")
	procAppendMenuW          = user32.NewProc("AppendMenuW")
	procDestroyMenu          = user32.NewProc("DestroyMenu")
	procGetCursorPos         = user32.NewProc("GetCursorPos")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procTrackPopupMenuEx     = user32.NewProc("TrackPopupMenuEx")
	procCreateIconFromResEx  = user32.NewProc("CreateIconFromResourceEx")
	procLoadIconW            = user32.NewProc("LoadIconW")
	procDestroyIcon          = user32.NewProc("DestroyIcon")
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandleW     = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmContextMenu = 0x007B
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000

	wmTrayCallback = wmApp + 1
	wmAppRefresh   = wmApp + 2
	wmAppClose     = wmApp + 3

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo  = 0x00000001
	niifError = 0x00000003

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfChecked   = 0x00000008
	mfSeparator = 0x00000800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	idiApplication         = 32512
	errorClassAlreadyExist = 1410
	trayIconID             = 1
)

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             windows.HWND
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeoutOrVer    uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         windows.GUID
	hBalloonIcon     uintptr
}

type osTrayState struct {
	mu                sync.Mutex
	hwnd              windows.HWND
	hIcon             uintptr
	ownIcon           bool
	hInstance         windows.Handle
	className         *uint16
	taskbarCreatedMsg uint32

	readyOnce      sync.Once
	readyCh        chan struct{}
	closeOnce      sync.Once
	started        atomic.Bool
	closeRequested atomic.Bool
	closed         atomic.Bool
	iconAdded      atomic.Bool
	actionCh       chan uint16
}

var (
	wndProcOnce     sync.Once
	wndProcCallback uintptr
	appsByHWND      sync.Map
)

func getWndProcCallback() uintptr {
	wndProcOnce.Do(func() {
		wndProcCallback = windows.NewCallback(wndProc)
	})
	return wndProcCallback
}

func copyUTF16(dst []uint16, s string) {
	if len(dst) == 0 {
		return
	}
	clear(dst)
	limit := len(dst) - 1
	pos := 0
	for _, r := range s {
		r1, r2 := utf16.EncodeRune(r)
		if r1 != 0xFFFD || r2 != 0xFFFD {
			if pos+2 > limit {
				break
			}
			dst[pos] = uint16(r1)
			dst[pos+1] = uint16(r2)
			pos += 2
			continue
		}
		if pos+1 > limit {
			break
		}
		if r > 0xFFFF {
			r = 0xFFFD
		}
		dst[pos] = uint16(r)
		pos++
	}
	dst[pos] = 0
}

func (a *App) ensureReadyChan() chan struct{} {
	a.os.mu.Lock()
	defer a.os.mu.Unlock()
	if a.os.readyCh == nil {
		a.os.readyCh = make(chan struct{})
	}
	return a.os.readyCh
}

func (a *App) signalReady() {
	ch := a.ensureReadyChan()
	a.os.readyOnce.Do(func() {
		close(ch)
	})
}

func (a *App) runOS(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	a.ensureReadyChan()
	a.os.started.Store(true)

	hInstRaw, _, _ := procGetModuleHandleW.Call(0)
	hInst := windows.Handle(hInstRaw)

	className, err := windows.UTF16PtrFromString("TeleMountTrayWindowClass")
	if err != nil {
		a.signalReady()
		return err
	}
	winTitle, err := windows.UTF16PtrFromString("TeleMount Tray")
	if err != nil {
		a.signalReady()
		return err
	}

	var wc wndClassExW
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	wc.lpfnWndProc = getWndProcCallback()
	wc.hInstance = hInst
	wc.lpszClassName = className

	ret, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		if errno, ok := callErr.(windows.Errno); !ok || errno != errorClassAlreadyExist {
			a.signalReady()
			return callErr
		}
	}

	hIcon, ownIcon := createTrayIcon()
	hwndRaw, _, createErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(winTitle)),
		0,
		0, 0, 0, 0,
		0, // top-level hidden window so TaskbarCreated broadcasts reach it
		0,
		uintptr(hInst),
		0,
	)
	hwnd := windows.HWND(hwndRaw)
	if hwnd == 0 {
		if ownIcon && hIcon != 0 {
			procDestroyIcon.Call(hIcon)
		}
		a.signalReady()
		return createErr
	}

	tbMsgName, _ := windows.UTF16PtrFromString("TaskbarCreated")
	tbMsgRaw, _, _ := procRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(tbMsgName)))

	actionCh := make(chan uint16, 16)

	a.os.mu.Lock()
	a.os.hwnd = hwnd
	a.os.hIcon = hIcon
	a.os.ownIcon = ownIcon
	a.os.hInstance = hInst
	a.os.className = className
	a.os.taskbarCreatedMsg = uint32(tbMsgRaw)
	a.os.actionCh = actionCh
	a.os.mu.Unlock()

	appsByHWND.Store(hwnd, a)
	defer func() {
		a.deleteTrayIcon()
		appsByHWND.Delete(hwnd)
		if ownIcon && hIcon != 0 {
			procDestroyIcon.Call(hIcon)
		}
		procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), uintptr(hInst))
		a.os.mu.Lock()
		a.os.hwnd = 0
		a.os.hIcon = 0
		a.os.mu.Unlock()
	}()

	if err := a.addTrayIcon(); err != nil {
		procDestroyWindow.Call(uintptr(hwnd))
		a.signalReady()
		return err
	}

	// Serialized non-blocking action dispatcher so Win32 message pump never stalls.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for id := range actionCh {
			quit, _ := DispatchMenuAction(id, a.actions, a)
			if quit {
				a.Close()
				return
			}
			_ = a.Refresh()
		}
	}()

	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = a.Close()
		case <-ctxDone:
		}
	}()

	a.signalReady()
	if a.os.closeRequested.Load() || ctx.Err() != nil {
		_ = a.Close()
	}

	var m msg
	for {
		r, _, gmErr := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		ri := int32(r)
		if ri == 0 {
			break
		}
		if ri < 0 {
			close(ctxDone)
			a.stopActionWorker(actionCh, workerDone)
			return gmErr
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	close(ctxDone)
	a.stopActionWorker(actionCh, workerDone)
	return nil
}

func (a *App) stopActionWorker(ch chan uint16, workerDone chan struct{}) {
	a.os.mu.Lock()
	a.os.actionCh = nil
	a.os.mu.Unlock()
	close(ch)
	<-workerDone
}

//go:embed icon_tray.png
var trayIconPNG []byte

func createTrayIcon() (uintptr, bool) {
	if len(trayIconPNG) > 0 {
		hIcon, _, _ := procCreateIconFromResEx.Call(
			uintptr(unsafe.Pointer(&trayIconPNG[0])),
			uintptr(len(trayIconPNG)),
			1,
			0x00030000,
			0,
			0,
			0,
		)
		if hIcon != 0 {
			return hIcon, true
		}
	}
	dib := BuildTrayIconDIB(32)
	hIcon, _, _ := procCreateIconFromResEx.Call(
		uintptr(unsafe.Pointer(&dib[0])),
		uintptr(len(dib)),
		1,
		0x00030000,
		0,
		0,
		0,
	)
	if hIcon != 0 {
		return hIcon, true
	}
	fallback, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
	return fallback, false
}

func (a *App) newNID() (notifyIconData, windows.HWND) {
	a.os.mu.Lock()
	hwnd := a.os.hwnd
	hIcon := a.os.hIcon
	a.os.mu.Unlock()

	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = trayIconID
	nid.hIcon = hIcon
	return nid, hwnd
}

func (a *App) addTrayIcon() error {
	nid, hwnd := a.newNID()
	if hwnd == 0 {
		return errors.New("tray window not initialized")
	}
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTrayCallback
	copyUTF16(nid.szTip[:], FormatTooltip(a.actions.Status()))

	ok, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if ok == 0 {
		return err
	}
	a.os.iconAdded.Store(true)
	return nil
}

func (a *App) deleteTrayIcon() {
	if !a.os.iconAdded.Swap(false) {
		return
	}
	nid, hwnd := a.newNID()
	if hwnd == 0 {
		return
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (a *App) refreshOS() error {
	if a.os.closed.Load() {
		return nil
	}
	a.os.mu.Lock()
	hwnd := a.os.hwnd
	a.os.mu.Unlock()
	if hwnd == 0 {
		return nil
	}
	procPostMessageW.Call(uintptr(hwnd), wmAppRefresh, 0, 0)
	return nil
}

func (a *App) applyRefresh() error {
	if !a.os.iconAdded.Load() {
		return nil
	}
	nid, hwnd := a.newNID()
	if hwnd == 0 {
		return nil
	}
	nid.uFlags = nifTip | nifIcon
	copyUTF16(nid.szTip[:], FormatTooltip(a.actions.Status()))
	ok, _, err := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	if ok == 0 {
		return err
	}
	return nil
}

func (a *App) notifyOS(title, message string, isError bool) error {
	if !a.os.iconAdded.Load() {
		return nil
	}
	nid, hwnd := a.newNID()
	if hwnd == 0 {
		return nil
	}
	nid.uFlags = nifInfo
	nid.dwInfoFlags = niifInfo
	if isError {
		nid.dwInfoFlags = niifError
	}
	copyUTF16(nid.szInfoTitle[:], title)
	copyUTF16(nid.szInfo[:], message)
	ok, _, err := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	if ok == 0 {
		return err
	}
	return nil
}

func (a *App) closeOS() error {
	a.os.closeRequested.Store(true)
	if !a.os.started.Load() {
		return nil
	}
	ch := a.ensureReadyChan()
	<-ch

	a.os.closeOnce.Do(func() {
		a.os.closed.Store(true)
		a.os.mu.Lock()
		hwnd := a.os.hwnd
		a.os.mu.Unlock()
		if hwnd != 0 {
			procPostMessageW.Call(uintptr(hwnd), wmAppClose, 0, 0)
		}
	})
	return nil
}

func (a *App) enqueueAction(id uint16) {
	if a.os.closed.Load() {
		return
	}
	a.os.mu.Lock()
	ch := a.os.actionCh
	if ch != nil {
		select {
		case ch <- id:
		default:
		}
	}
	a.os.mu.Unlock()
}

func (a *App) showPopupMenu(hwnd windows.HWND) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	items := BuildMenuItems(a.actions.Status())
	for _, it := range items {
		if it.Separator {
			procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if !it.Enabled {
			flags |= mfGrayed
		}
		if it.Checked {
			flags |= mfChecked
		}
		txt, err := windows.UTF16PtrFromString(it.Label)
		if err != nil {
			continue
		}
		procAppendMenuW.Call(hMenu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(txt)))
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(uintptr(hwnd))
	procTrackPopupMenuEx.Call(
		hMenu,
		tpmLeftAlign|tpmBottomAlign|tpmRightButton,
		uintptr(pt.x),
		uintptr(pt.y),
		uintptr(hwnd),
		0,
	)
	procPostMessageW.Call(uintptr(hwnd), wmNull, 0, 0)
}

func wndProc(hwnd windows.HWND, uMsg uint32, wParam, lParam uintptr) uintptr {
	raw, ok := appsByHWND.Load(hwnd)
	if !ok {
		r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
		return r
	}
	app := raw.(*App)

	if app.os.taskbarCreatedMsg != 0 && uMsg == app.os.taskbarCreatedMsg {
		app.os.iconAdded.Store(false)
		_ = app.addTrayIcon()
		return 0
	}

	switch uMsg {
	case wmTrayCallback:
		ev := uint32(lParam & 0xFFFF)
		switch ev {
		case wmLButtonUp:
			app.enqueueAction(MenuIDOpenWebUI)
		case wmRButtonUp, wmContextMenu:
			app.showPopupMenu(hwnd)
		}
		return 0
	case wmCommand:
		cmdID := uint16(wParam & 0xFFFF)
		app.enqueueAction(cmdID)
		return 0
	case wmAppRefresh:
		_ = app.applyRefresh()
		return 0
	case wmAppClose, wmClose:
		app.deleteTrayIcon()
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		app.deleteTrayIcon()
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
	return r
}

func showDialogOS(title, message string) error {
	tPtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	mPtr, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return err
	}
	_, err = windows.MessageBox(0, mPtr, tPtr, windows.MB_OK|windows.MB_ICONWARNING|windows.MB_SETFOREGROUND)
	return err
}

func openDrivePathOS(root string) error {
	return exec.Command("explorer.exe", root).Start()
}

func openBrowserURLOS(rawURL string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	urlPtr, err := windows.UTF16PtrFromString(rawURL)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, urlPtr, nil, nil, windows.SW_SHOWNORMAL)
}
