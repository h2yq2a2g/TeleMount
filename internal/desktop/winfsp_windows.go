//go:build windows

package desktop

import (
	"os"
	"runtime"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// NewWinFspProbe returns the Windows WinFsp installation detector.
func NewWinFspProbe() WinFspProbe {
	return newWinFspProbeWithDeps(winfspDeps{
		goarch:         runtime.GOARCH,
		findDLL:        findWinFspDLL,
		readInstallDir: readWinFspInstallDir,
		fileExists:     isRegularFile,
	})
}

func findWinFspDLL(dllName string) (string, bool) {
	h, err := windows.LoadLibraryEx(dllName, 0, windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
	if err != nil || h == 0 {
		return "", false
	}
	defer windows.FreeLibrary(h)

	if path := moduleFileName(h); path != "" {
		return path, true
	}
	return dllName, true
}

func moduleFileName(h windows.Handle) string {
	size := uint32(1024)
	for size <= 32768 {
		buf := make([]uint16, size)
		n, err := windows.GetModuleFileName(h, &buf[0], size)
		if err != nil || n == 0 {
			return ""
		}
		if n < size {
			return windows.UTF16ToString(buf[:n])
		}
		size *= 2
	}
	return ""
}

func readWinFspInstallDir() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.READ|registry.WOW64_32KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()

	val, _, err := k.GetStringValue("InstallDir")
	if err != nil {
		return "", err
	}
	return val, nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
