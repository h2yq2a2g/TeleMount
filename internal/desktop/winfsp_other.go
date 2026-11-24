//go:build !windows

package desktop

type unsupportedWinFspProbe struct{}

// NewWinFspProbe returns a non-Windows WinFspProbe stub.
func NewWinFspProbe() WinFspProbe {
	return unsupportedWinFspProbe{}
}

func (unsupportedWinFspProbe) Check() WinFspStatus {
	return WinFspStatus{
		Installed: false,
		Guidance: "WinFsp virtual drive mounting is only supported on Windows. On Windows, install WinFsp from " +
			winFspReleaseURL + " or run: " + winFspWingetCommand,
	}
}
