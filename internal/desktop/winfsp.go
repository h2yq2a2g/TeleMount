package desktop

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	winFspReleaseURL    = "https://winfsp.dev/rel/"
	winFspWingetCommand = "winget install WinFsp.WinFsp"
)

// WinFspStatus reports whether WinFsp is available for mounting, the resolved DLL path,
// and user-facing installation guidance when WinFsp is missing or unsupported.
type WinFspStatus struct {
	Installed bool   `json:"installed"`
	DLLPath   string `json:"dll_path,omitempty"`
	Guidance  string `json:"guidance,omitempty"`
}

// WinFspProbe checks whether WinFsp is installed on the host.
type WinFspProbe interface {
	Check() WinFspStatus
}

// CheckWinFsp probes the host for a usable WinFsp installation without panicking.
func CheckWinFsp() WinFspStatus {
	return NewWinFspProbe().Check()
}

type winfspDeps struct {
	goarch         string
	findDLL        func(dllName string) (string, bool)
	readInstallDir func() (string, error)
	fileExists     func(path string) bool
}

type winfspProbe struct {
	deps winfspDeps
}

func newWinFspProbeWithDeps(deps winfspDeps) WinFspProbe {
	return &winfspProbe{deps: deps}
}

func winfspDLLName(arch string) (string, bool) {
	switch arch {
	case "amd64":
		return "winfsp-x64.dll", true
	case "arm64":
		return "winfsp-a64.dll", true
	case "386":
		return "winfsp-x86.dll", true
	default:
		return "", false
	}
}

func (p *winfspProbe) Check() WinFspStatus {
	dllName, ok := winfspDLLName(p.deps.goarch)
	if !ok {
		return WinFspStatus{
			Installed: false,
			Guidance: fmt.Sprintf(
				"WinFsp is not supported on CPU architecture %q (supported: amd64, arm64, 386). Install WinFsp on a supported Windows architecture from %s or via: %s",
				p.deps.goarch, winFspReleaseURL, winFspWingetCommand,
			),
		}
	}

	if p.deps.findDLL != nil {
		if path, found := p.deps.findDLL(dllName); found {
			path = strings.TrimSpace(path)
			if path == "" {
				path = dllName
			}
			return WinFspStatus{
				Installed: true,
				DLLPath:   path,
			}
		}
	}

	if p.deps.readInstallDir != nil && p.deps.fileExists != nil {
		installDir, err := p.deps.readInstallDir()
		installDir = strings.TrimSpace(installDir)
		if err == nil && installDir != "" {
			candidate := filepath.Join(installDir, "bin", dllName)
			if p.deps.fileExists(candidate) {
				return WinFspStatus{
					Installed: true,
					DLLPath:   candidate,
				}
			}
		}
	}

	return WinFspStatus{
		Installed: false,
		Guidance: fmt.Sprintf(
			"WinFsp is not installed (%s not found). Install WinFsp from %s or run: %s, then restart TeleMount.",
			dllName, winFspReleaseURL, winFspWingetCommand,
		),
	}
}
