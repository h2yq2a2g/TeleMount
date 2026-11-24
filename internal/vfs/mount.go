//go:build windows

package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/desktop"
)

// MountManager controls the WinFsp cgofuse mount lifecycle for a TeleFS instance.
type MountManager struct {
	fs      *TeleFS
	probe   desktop.WinFspProbe
	checker desktop.DriveChecker

	mu       sync.Mutex
	host     *fuse.FileSystemHost
	mounted  bool
	drive    string
	lastErr  string
	doneCh   chan struct{}
}

func NewMountManager(fs *TeleFS, probe desktop.WinFspProbe, checker desktop.DriveChecker) *MountManager {
	if probe == nil {
		probe = desktop.NewWinFspProbe()
	}
	if checker == nil {
		checker = desktop.NewDriveChecker()
	}
	return &MountManager{
		fs:      fs,
		probe:   probe,
		checker: checker,
	}
}

func (m *MountManager) CheckWinFsp() desktop.WinFspStatus {
	return m.probe.Check()
}

func (m *MountManager) IsMounted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted
}

func (m *MountManager) Status() (mounted bool, drive string, lastErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted, m.drive, m.lastErr
}

func (m *MountManager) Mount(driveLetter string) error {
	m.mu.Lock()
	if m.mounted {
		drv := m.drive
		m.mu.Unlock()
		return fmt.Errorf("virtual drive is already mounted at %s", drv)
	}
	if m.fs == nil {
		m.lastErr = "virtual filesystem is not initialized"
		m.mu.Unlock()
		return errors.New("virtual filesystem is nil")
	}

	wf := m.probe.Check()
	if !wf.Installed {
		msg := wf.Guidance
		if msg == "" {
			msg = "WinFsp is not installed"
		}
		m.lastErr = msg
		m.mu.Unlock()
		return fmt.Errorf("WinFsp required: %s", msg)
	}

	drv, err := desktop.NormalizeDriveLetter(driveLetter)
	if err != nil {
		m.lastErr = err.Error()
		m.mu.Unlock()
		return err
	}

	inUse, err := m.checker.IsDriveInUse(drv)
	if err != nil {
		m.lastErr = err.Error()
		m.mu.Unlock()
		return err
	}
	if inUse {
		err = fmt.Errorf("drive letter %s is already in use", drv)
		m.lastErr = err.Error()
		m.mu.Unlock()
		return err
	}

	initCh := m.fs.resetInitSignal()
	host := fuse.NewFileSystemHost(m.fs)
	host.SetCapCaseInsensitive(true)
	host.SetCapReaddirPlus(true)

	doneCh := make(chan struct{})
	errCh := make(chan error, 1)

	m.host = host
	m.drive = drv
	m.doneCh = doneCh
	m.lastErr = ""
	m.mu.Unlock()

	go func() {
		defer close(doneCh)
		ok := host.Mount(drv, []string{
			"-o", "volname=TeleMount",
			"-o", "uid=-1",
			"-o", "gid=-1",
			"-o", "FileInfoTimeout=1000",
		})
		m.mu.Lock()
		m.mounted = false
		m.host = nil
		if !ok {
			m.lastErr = fmt.Sprintf("WinFsp failed to mount %s", drv)
			select {
			case errCh <- errors.New(m.lastErr):
			default:
			}
		}
		m.mu.Unlock()
	}()

	select {
	case <-initCh:
		m.mu.Lock()
		m.mounted = true
		m.lastErr = ""
		m.mu.Unlock()
		return nil
	case merr := <-errCh:
		return merr
	case <-doneCh:
		m.mu.Lock()
		msg := m.lastErr
		m.mu.Unlock()
		if msg == "" {
			msg = fmt.Sprintf("WinFsp mount exited unexpectedly on %s", drv)
		}
		return errors.New(msg)
	case <-time.After(10 * time.Second):
		host.Unmount()
		m.mu.Lock()
		m.mounted = false
		m.host = nil
		m.lastErr = fmt.Sprintf("timed out waiting for WinFsp mount on %s", drv)
		m.mu.Unlock()
		return errors.New("timed out waiting for WinFsp mount")
	}
}

func (m *MountManager) Unmount(ctx context.Context) error {
	m.mu.Lock()
	host := m.host
	doneCh := m.doneCh
	if host == nil {
		m.mounted = false
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	host.Unmount()

	if doneCh != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case <-doneCh:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("timed out waiting for WinFsp unmount")
		}
	}

	m.mu.Lock()
	m.mounted = false
	m.host = nil
	m.doneCh = nil
	m.mu.Unlock()
	return nil
}
