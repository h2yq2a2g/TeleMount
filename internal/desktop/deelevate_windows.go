//go:build windows

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const noDeelevateEnv = "TELEMOUNT_NO_DEELEVATE"

var (
	modUser32Deelev       = windows.NewLazySystemDLL("user32.dll")
	procGetShellWindow    = modUser32Deelev.NewProc("GetShellWindow")
	procGetWindowThreadID = modUser32Deelev.NewProc("GetWindowThreadProcessId")
	modAdvapi32Deelev     = windows.NewLazySystemDLL("advapi32.dll")
	procCreateProcWithTok = modAdvapi32Deelev.NewProc("CreateProcessWithTokenW")
)

// RelaunchNonElevatedIfNeeded detects when TeleMount desktop was accidentally launched
// elevated (for example after an older auto-updater using "runas" or "Run as administrator"),
// re-launches itself under Explorer's non-elevated desktop token so WinFsp drives are visible
// in File Explorer, and returns relaunched=true when the caller should exit immediately.
func RelaunchNonElevatedIfNeeded(exePath string, args []string) (bool, error) {
	if os.Getenv(noDeelevateEnv) == "1" || strings.TrimSpace(exePath) == "" {
		return false, nil
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}

	hwnd, _, _ := procGetShellWindow.Call()
	if hwnd == 0 {
		return false, nil
	}
	var shellPID uint32
	_, _, _ = procGetWindowThreadID.Call(hwnd, uintptr(unsafe.Pointer(&shellPID)))
	if shellPID == 0 {
		return false, nil
	}

	hShellProc, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, shellPID)
	if err != nil {
		hShellProc, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, shellPID)
		if err != nil {
			return false, err
		}
	}
	defer windows.CloseHandle(hShellProc)

	var hShellTok windows.Token
	if err := windows.OpenProcessToken(hShellProc, windows.TOKEN_DUPLICATE, &hShellTok); err != nil {
		return false, err
	}
	defer hShellTok.Close()

	var hPrimary windows.Token
	const tokenAccess = windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY |
		windows.TOKEN_DUPLICATE | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID
	if err := windows.DuplicateTokenEx(hShellTok, tokenAccess, nil, windows.SecurityImpersonation, windows.TokenPrimary, &hPrimary); err != nil {
		return false, err
	}
	defer hPrimary.Close()

	// Do not loop when Explorer itself runs elevated (e.g. UAC disabled / built-in Administrator).
	if hPrimary.IsElevated() {
		return false, nil
	}

	fullArgs := make([]string, 0, len(args)+1)
	fullArgs = append(fullArgs, exePath)
	fullArgs = append(fullArgs, args...)
	cmdLine := windows.ComposeCommandLine(fullArgs)

	exePtr, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return false, err
	}
	cmdPtr, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return false, err
	}
	dirPtr, err := windows.UTF16PtrFromString(filepath.Dir(exePath))
	if err != nil {
		return false, err
	}

	envBlock := buildUTF16EnvBlock(append(os.Environ(), noDeelevateEnv+"=1"))
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation

	const createUnicodeEnvironment = 0x00000400
	r1, _, callErr := procCreateProcWithTok.Call(
		uintptr(hPrimary),
		0,
		uintptr(unsafe.Pointer(exePtr)),
		uintptr(unsafe.Pointer(cmdPtr)),
		createUnicodeEnvironment,
		uintptr(unsafe.Pointer(&envBlock[0])),
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r1 == 0 {
		return false, callErr
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
	return true, nil
}

func buildUTF16EnvBlock(vars []string) []uint16 {
	var out []uint16
	for _, kv := range vars {
		if kv == "" {
			continue
		}
		out = append(out, utf16.Encode([]rune(kv))...)
		out = append(out, 0)
	}
	if len(out) == 0 {
		out = append(out, 0)
	}
	out = append(out, 0)
	return out
}
