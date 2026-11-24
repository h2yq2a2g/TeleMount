package tray

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

type mockTrayActions struct {
	status        TrayStatus
	mountCalled   int
	unmountCalled int
	openDrvCalled int
	openWebCalled int
	toggleCalled  int
	quitCalled    int

	mountErr   error
	unmountErr error
	openDrvErr error
	openWebErr error
	toggleErr  error
}

func (m *mockTrayActions) Status() TrayStatus { return m.status }
func (m *mockTrayActions) Mount() error       { m.mountCalled++; return m.mountErr }
func (m *mockTrayActions) Unmount() error     { m.unmountCalled++; return m.unmountErr }
func (m *mockTrayActions) OpenDrive() error   { m.openDrvCalled++; return m.openDrvErr }
func (m *mockTrayActions) OpenWebUI() error   { m.openWebCalled++; return m.openWebErr }
func (m *mockTrayActions) ToggleStartup() (bool, error) {
	m.toggleCalled++
	if m.toggleErr != nil {
		return m.status.RunAtStartup, m.toggleErr
	}
	m.status.RunAtStartup = !m.status.RunAtStartup
	return m.status.RunAtStartup, nil
}
func (m *mockTrayActions) Quit() { m.quitCalled++ }

type notifyRecord struct {
	title   string
	message string
	isError bool
	dialog  bool
}

type mockNotifier struct {
	records   []notifyRecord
	dialogErr error
	notifyErr error
}

func (n *mockNotifier) Notify(title, message string, isError bool) error {
	n.records = append(n.records, notifyRecord{title: title, message: message, isError: isError})
	return n.notifyErr
}

func (n *mockNotifier) ShowWarningDialog(title, message string) error {
	n.records = append(n.records, notifyRecord{title: title, message: message, dialog: true})
	return n.dialogErr
}

func findMenuItem(items []MenuItem, id uint16) (MenuItem, bool) {
	for _, it := range items {
		if !it.Separator && it.ID == id {
			return it, true
		}
	}
	return MenuItem{}, false
}

func TestBuildMenuItemsStates(t *testing.T) {
	t.Run("mounted online with startup checked", func(t *testing.T) {
		items := BuildMenuItems(TrayStatus{
			Mounted:         true,
			DriveLetter:     "v:",
			WinFspInstalled: true,
			RunAtStartup:    true,
			QueuePending:    3,
			Online:          true,
		})

		st, ok := findMenuItem(items, MenuIDStatus)
		if !ok || st.Enabled || !strings.Contains(st.Label, "Mounted") || !strings.Contains(st.Label, "Online") || !strings.Contains(st.Label, "3 queued") {
			t.Fatalf("unexpected status item: %+v", st)
		}
		mt, _ := findMenuItem(items, MenuIDMountToggle)
		if !mt.Enabled || !strings.Contains(mt.Label, "Unmount Drive (V:)") {
			t.Fatalf("unexpected mount toggle when mounted: %+v", mt)
		}
		od, _ := findMenuItem(items, MenuIDOpenDrive)
		if !od.Enabled || !strings.Contains(od.Label, "V:") {
			t.Fatalf("expected OpenDrive enabled when mounted: %+v", od)
		}
		su, _ := findMenuItem(items, MenuIDToggleStartup)
		if !su.Enabled || !su.Checked {
			t.Fatalf("expected RunAtStartup checked: %+v", su)
		}
		q, _ := findMenuItem(items, MenuIDQuit)
		if !q.Enabled {
			t.Fatalf("expected Quit enabled: %+v", q)
		}
	})

	t.Run("unmounted with WinFsp missing", func(t *testing.T) {
		items := BuildMenuItems(TrayStatus{
			Mounted:         false,
			DriveLetter:     "",
			WinFspInstalled: false,
			Online:          false,
		})

		st, _ := findMenuItem(items, MenuIDStatus)
		if !strings.Contains(st.Label, "WinFsp Missing") || !strings.Contains(st.Label, "Offline") {
			t.Fatalf("unexpected status item for missing WinFsp: %+v", st)
		}
		mt, _ := findMenuItem(items, MenuIDMountToggle)
		if mt.Enabled || !strings.Contains(mt.Label, "Mount Drive (V:)") {
			t.Fatalf("expected Mount disabled when WinFsp missing: %+v", mt)
		}
		od, _ := findMenuItem(items, MenuIDOpenDrive)
		if od.Enabled {
			t.Fatalf("expected OpenDrive disabled when unmounted: %+v", od)
		}
		web, _ := findMenuItem(items, MenuIDOpenWebUI)
		if !web.Enabled {
			t.Fatalf("expected OpenWebUI enabled even when unmounted: %+v", web)
		}
	})
}

func TestFormatTooltipAndLengthCap(t *testing.T) {
	tip := FormatTooltip(TrayStatus{
		Mounted:         true,
		DriveLetter:     "Z:",
		WinFspInstalled: true,
		Online:          true,
		QueuePending:    5,
	})
	if !strings.Contains(tip, "Z:") || !strings.Contains(tip, "Mounted") || !strings.Contains(tip, "5 pending") {
		t.Fatalf("unexpected tooltip: %q", tip)
	}

	missingTip := FormatTooltip(TrayStatus{WinFspInstalled: false})
	if !strings.Contains(missingTip, "WinFsp missing") {
		t.Fatalf("unexpected WinFsp missing tooltip: %q", missingTip)
	}

	longInput := strings.Repeat("🚀", 80) // 160 UTF-16 units (> 127)
	capped := truncateUTF16Units(longInput, 127)
	units := utf16.Encode([]rune(capped))
	if len(units) > 127 || len(units) == 0 {
		t.Fatalf("expected UTF-16 unit length <= 127 without splitting surrogate, got %d", len(units))
	}
}

func TestCheckAndNotifyWinFsp(t *testing.T) {
	notifier := &mockNotifier{}
	if ok := CheckAndNotifyWinFsp(TrayStatus{WinFspInstalled: true}, notifier); !ok {
		t.Fatal("expected true when WinFsp is installed")
	}
	if len(notifier.records) != 0 {
		t.Fatalf("expected no notification when WinFsp is installed, got %+v", notifier.records)
	}

	if ok := CheckAndNotifyWinFsp(TrayStatus{WinFspInstalled: false}, notifier); ok {
		t.Fatal("expected false when WinFsp is missing")
	}
	if len(notifier.records) != 1 || !notifier.records[0].dialog {
		t.Fatalf("expected 1 warning dialog record, got %+v", notifier.records)
	}
	msg := notifier.records[0].message
	if !strings.Contains(msg, "winfsp.dev") || !strings.Contains(msg, "winget install WinFsp.WinFsp") {
		t.Fatalf("guidance missing winfsp.dev or winget command: %q", msg)
	}

	// Fallback to Notify when ShowWarningDialog errors.
	fallbackNotifier := &mockNotifier{dialogErr: errors.New("headless")}
	CheckAndNotifyWinFsp(TrayStatus{WinFspInstalled: false}, fallbackNotifier)
	if len(fallbackNotifier.records) != 2 || !fallbackNotifier.records[1].isError {
		t.Fatalf("expected fallback error Notify after dialog error, got %+v", fallbackNotifier.records)
	}
}

func TestDispatchMenuAction(t *testing.T) {
	if _, err := DispatchMenuAction(MenuIDQuit, nil, nil); !errors.Is(err, ErrNilActions) {
		t.Fatalf("expected ErrNilActions, got %v", err)
	}

	t.Run("mount when WinFsp missing notifies guidance", func(t *testing.T) {
		act := &mockTrayActions{status: TrayStatus{WinFspInstalled: false}}
		n := &mockNotifier{}
		quit, err := DispatchMenuAction(MenuIDMountToggle, act, n)
		if quit || !errors.Is(err, ErrWinFspMissing) {
			t.Fatalf("expected ErrWinFspMissing, got quit=%v err=%v", quit, err)
		}
		if act.mountCalled != 0 || len(n.records) != 1 {
			t.Fatalf("expected no Mount call and 1 guidance dialog, mountCalled=%d records=%+v", act.mountCalled, n.records)
		}
	})

	t.Run("mount, unmount, open drive, open webui, toggle startup, quit", func(t *testing.T) {
		act := &mockTrayActions{status: TrayStatus{WinFspInstalled: true}}
		n := &mockNotifier{}

		if quit, err := DispatchMenuAction(MenuIDMountToggle, act, n); err != nil || quit || act.mountCalled != 1 {
			t.Fatalf("mount failed: quit=%v err=%v count=%d", quit, err, act.mountCalled)
		}
		act.status.Mounted = true
		if quit, err := DispatchMenuAction(MenuIDMountToggle, act, n); err != nil || quit || act.unmountCalled != 1 {
			t.Fatalf("unmount failed: quit=%v err=%v count=%d", quit, err, act.unmountCalled)
		}
		if quit, err := DispatchMenuAction(MenuIDOpenDrive, act, n); err != nil || quit || act.openDrvCalled != 1 {
			t.Fatalf("open drive failed: quit=%v err=%v count=%d", quit, err, act.openDrvCalled)
		}
		if quit, err := DispatchMenuAction(MenuIDOpenWebUI, act, n); err != nil || quit || act.openWebCalled != 1 {
			t.Fatalf("open webui failed: quit=%v err=%v count=%d", quit, err, act.openWebCalled)
		}
		if quit, err := DispatchMenuAction(MenuIDToggleStartup, act, n); err != nil || quit || act.toggleCalled != 1 || !act.status.RunAtStartup {
			t.Fatalf("toggle startup failed: quit=%v err=%v count=%d", quit, err, act.toggleCalled)
		}
		if quit, err := DispatchMenuAction(MenuIDQuit, act, n); err != nil || !quit || act.quitCalled != 1 {
			t.Fatalf("quit failed: quit=%v err=%v count=%d", quit, err, act.quitCalled)
		}
	})

	t.Run("action error notifies user", func(t *testing.T) {
		wantErr := errors.New("drive busy")
		act := &mockTrayActions{
			status:     TrayStatus{WinFspInstalled: true, Mounted: true},
			unmountErr: wantErr,
		}
		n := &mockNotifier{}
		quit, err := DispatchMenuAction(MenuIDMountToggle, act, n)
		if quit || !errors.Is(err, wantErr) {
			t.Fatalf("expected %v, got quit=%v err=%v", wantErr, quit, err)
		}
		if len(n.records) != 1 || !n.records[0].isError || !strings.Contains(n.records[0].message, "drive busy") {
			t.Fatalf("expected error notification, got %+v", n.records)
		}
	})
}

func TestOpenDrivePathAndURLValidation(t *testing.T) {
	for _, bad := range []string{"", "1:", "VV:", "C:\\Windows", "V:foo", ":"} {
		if err := OpenDrivePath(bad); !errors.Is(err, ErrInvalidDriveLetter) {
			t.Errorf("OpenDrivePath(%q): expected ErrInvalidDriveLetter, got %v", bad, err)
		}
	}
	for _, badURL := range []string{"", "file:///C:/Windows/calc.exe", "javascript:alert(1)", "http://", "not-a-url"} {
		if err := OpenBrowserURL(badURL); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("OpenBrowserURL(%q): expected ErrInvalidURL, got %v", badURL, err)
		}
	}
}

func TestBuildTrayIconDIB(t *testing.T) {
	dib := BuildTrayIconDIB(32)
	const expectedLen = 40 + (32*32*4) + (4 * 32) // 4264 bytes
	if len(dib) != expectedLen {
		t.Fatalf("expected DIB length %d, got %d", expectedLen, len(dib))
	}
	if biSize := binary.LittleEndian.Uint32(dib[0:4]); biSize != 40 {
		t.Fatalf("expected biSize=40, got %d", biSize)
	}
	if w := int32(binary.LittleEndian.Uint32(dib[4:8])); w != 32 {
		t.Fatalf("expected biWidth=32, got %d", w)
	}
	if h := int32(binary.LittleEndian.Uint32(dib[8:12])); h != 64 {
		t.Fatalf("expected biHeight=64 (XOR+AND), got %d", h)
	}
	if bpp := binary.LittleEndian.Uint16(dib[14:16]); bpp != 32 {
		t.Fatalf("expected biBitCount=32, got %d", bpp)
	}
	// Corner pixel (0,0) is outside rounded badge -> alpha 0 and AND mask bit 1.
	if alpha := dib[40+3]; alpha != 0 {
		t.Fatalf("expected transparent corner pixel alpha=0, got %d", alpha)
	}
	andOff := 40 + 32*32*4
	if dib[andOff]&0x80 == 0 {
		t.Fatalf("expected AND mask MSB=1 for transparent corner pixel")
	}
	// Center pixel (16,16) is opaque.
	centerAlpha := dib[40+(16*32+16)*4+3]
	if centerAlpha != 255 {
		t.Fatalf("expected opaque center pixel alpha=255, got %d", centerAlpha)
	}
}

func TestNewAppNilActions(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrNilActions) {
		t.Fatalf("expected ErrNilActions, got %v", err)
	}
}
