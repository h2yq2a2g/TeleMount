package desktop

import "strings"

// OverlayQueryFunc maps a normalized virtual POSIX path ("/Folder/file.ext")
// to a single-byte OneDrive-style overlay state:
// 'P' (pinned offline), 'C' (cached locally), 'O' (cloud-only), 'S' (syncing), or 'N' (none).
type OverlayQueryFunc func(virtualPath string) byte

// VirtualPathToWindows joins driveLetter ("V:") and a virtual POSIX path ("/Test/a.apk")
// into a Windows path ("V:\Test\a.apk").
func VirtualPathToWindows(driveLetter, virtualPath string) (string, error) {
	drv, err := NormalizeDriveLetter(driveLetter)
	if err != nil {
		return "", err
	}
	vp := NormalizeVirtualPath(virtualPath)
	if vp == "/" {
		return drv + `\`, nil
	}
	return drv + strings.ReplaceAll(vp, "/", `\`), nil
}
