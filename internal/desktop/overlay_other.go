//go:build !windows

package desktop

// OverlayServer is a no-op stub on non-Windows platforms.
type OverlayServer struct{}

func StartOverlayServer(_ OverlayQueryFunc) (*OverlayServer, error) {
	return &OverlayServer{}, nil
}

func (*OverlayServer) SetDrive(_ string)        {}
func (*OverlayServer) NotifyPaths(_ ...string)  {}
func (*OverlayServer) Close() error             { return nil }
