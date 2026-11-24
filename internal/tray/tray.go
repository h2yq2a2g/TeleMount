package tray

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"
)

var (
	ErrUnsupportedPlatform = errors.New("system tray is only supported on Windows")
	ErrWinFspMissing       = errors.New("WinFsp is not installed")
	ErrNilActions          = errors.New("tray actions must not be nil")
	ErrInvalidDriveLetter  = errors.New("invalid drive letter")
	ErrInvalidURL          = errors.New("invalid browser URL")
)

const DefaultWinFspGuidance = "WinFsp is required to mount the TeleMount virtual drive.\n\nInstall WinFsp from https://winfsp.dev or run:\n  winget install WinFsp.WinFsp"

const (
	MenuIDStatus        uint16 = 1001
	MenuIDMountToggle   uint16 = 1002
	MenuIDOpenDrive     uint16 = 1003
	MenuIDOpenWebUI     uint16 = 1004
	MenuIDToggleStartup uint16 = 1005
	MenuIDQuit          uint16 = 1006
)

type TrayStatus struct {
	Mounted         bool   `json:"mounted"`
	DriveLetter     string `json:"drive_letter"`
	WinFspInstalled bool   `json:"winfsp_installed"`
	WinFspGuidance  string `json:"winfsp_guidance"`
	RunAtStartup    bool   `json:"run_at_startup"`
	QueuePending    int    `json:"queue_pending"`
	Online          bool   `json:"online"`
}

type TrayActions interface {
	Status() TrayStatus
	Mount() error
	Unmount() error
	OpenDrive() error
	OpenWebUI() error
	ToggleStartup() (bool, error)
	Quit()
}

type Notifier interface {
	Notify(title, message string, isError bool) error
	ShowWarningDialog(title, message string) error
}

type MenuItem struct {
	ID        uint16
	Label     string
	Enabled   bool
	Checked   bool
	Separator bool
}

func FormatWinFspGuidance(custom string) string {
	trimmed := strings.TrimSpace(custom)
	if trimmed == "" {
		return DefaultWinFspGuidance
	}
	return trimmed
}

// CheckAndNotifyWinFsp alerts the user with actionable WinFsp install guidance if WinFsp is missing.
// Returns true when WinFsp is installed.
func CheckAndNotifyWinFsp(status TrayStatus, notifier Notifier) bool {
	if status.WinFspInstalled {
		return true
	}
	if notifier != nil {
		msg := FormatWinFspGuidance(status.WinFspGuidance)
		if err := notifier.ShowWarningDialog("TeleMount — WinFsp Required", msg); err != nil {
			_ = notifier.Notify("TeleMount — WinFsp Required", msg, true)
		}
	}
	return false
}

func displayDrive(drive string) string {
	d := strings.TrimSpace(drive)
	if d == "" {
		return "V:"
	}
	return strings.ToUpper(strings.TrimRight(d, `\/`))
}

func BuildMenuItems(status TrayStatus) []MenuItem {
	drive := displayDrive(status.DriveLetter)

	var stateParts []string
	if !status.WinFspInstalled {
		stateParts = append(stateParts, "WinFsp Missing")
	} else if status.Mounted {
		stateParts = append(stateParts, "Mounted")
	} else {
		stateParts = append(stateParts, "Unmounted")
	}
	if status.Online {
		stateParts = append(stateParts, "Online")
	} else {
		stateParts = append(stateParts, "Offline")
	}
	if status.QueuePending > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d queued", status.QueuePending))
	}

	mountLabel := fmt.Sprintf("Mount Drive (%s)", drive)
	mountEnabled := status.WinFspInstalled
	if status.Mounted {
		mountLabel = fmt.Sprintf("Unmount Drive (%s)", drive)
		mountEnabled = true
	}

	return []MenuItem{
		{
			ID:      MenuIDStatus,
			Label:   fmt.Sprintf("TeleMount (%s): %s", drive, strings.Join(stateParts, " | ")),
			Enabled: false,
		},
		{Separator: true},
		{
			ID:      MenuIDMountToggle,
			Label:   mountLabel,
			Enabled: mountEnabled,
		},
		{
			ID:      MenuIDOpenDrive,
			Label:   fmt.Sprintf("Open Virtual Drive (%s)", drive),
			Enabled: status.Mounted,
		},
		{
			ID:      MenuIDOpenWebUI,
			Label:   "Open Web UI",
			Enabled: true,
		},
		{Separator: true},
		{
			ID:      MenuIDToggleStartup,
			Label:   "Run at Windows Startup",
			Enabled: true,
			Checked: status.RunAtStartup,
		},
		{Separator: true},
		{
			ID:      MenuIDQuit,
			Label:   "Quit TeleMount",
			Enabled: true,
		},
	}
}

func FormatTooltip(status TrayStatus) string {
	drive := displayDrive(status.DriveLetter)
	var msg string
	switch {
	case !status.WinFspInstalled:
		msg = "TeleMount — WinFsp missing (see guidance)"
	case status.Mounted:
		msg = fmt.Sprintf("TeleMount (%s) — Mounted", drive)
	default:
		msg = fmt.Sprintf("TeleMount (%s) — Unmounted", drive)
	}
	if status.Online {
		msg += ", Online"
	} else {
		msg += ", Offline"
	}
	if status.QueuePending > 0 {
		msg += fmt.Sprintf(", %d pending", status.QueuePending)
	}
	return truncateUTF16Units(msg, 127)
}

func truncateUTF16Units(s string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	var units int
	for idx, r := range s {
		n := 1
		if _, r2 := utf16.EncodeRune(r); r2 != 0xFFFD {
			n = 2
		}
		if units+n > maxUnits {
			return s[:idx]
		}
		units += n
	}
	return s
}

func DispatchMenuAction(id uint16, actions TrayActions, notifier Notifier) (quit bool, err error) {
	if actions == nil {
		return false, ErrNilActions
	}
	notifyErr := func(e error) {
		if e != nil && notifier != nil {
			_ = notifier.Notify("TeleMount", e.Error(), true)
		}
	}

	switch id {
	case MenuIDStatus:
		return false, nil
	case MenuIDMountToggle:
		st := actions.Status()
		if st.Mounted {
			if err := actions.Unmount(); err != nil {
				notifyErr(err)
				return false, err
			}
			return false, nil
		}
		if !st.WinFspInstalled {
			CheckAndNotifyWinFsp(st, notifier)
			return false, ErrWinFspMissing
		}
		if err := actions.Mount(); err != nil {
			notifyErr(err)
			return false, err
		}
		return false, nil
	case MenuIDOpenDrive:
		if err := actions.OpenDrive(); err != nil {
			notifyErr(err)
			return false, err
		}
		return false, nil
	case MenuIDOpenWebUI:
		if err := actions.OpenWebUI(); err != nil {
			notifyErr(err)
			return false, err
		}
		return false, nil
	case MenuIDToggleStartup:
		if _, err := actions.ToggleStartup(); err != nil {
			notifyErr(err)
			return false, err
		}
		return false, nil
	case MenuIDQuit:
		actions.Quit()
		return true, nil
	default:
		return false, nil
	}
}

// OpenDrivePath opens the virtual drive root (e.g. "V:" -> "V:\") in Windows Explorer.
func OpenDrivePath(driveLetter string) error {
	trimmed := strings.TrimSpace(driveLetter)
	trimmed = strings.TrimRight(trimmed, `\/`)
	if len(trimmed) == 2 && trimmed[1] == ':' {
		trimmed = trimmed[:1]
	}
	if len(trimmed) != 1 {
		return fmt.Errorf("%w: %q", ErrInvalidDriveLetter, driveLetter)
	}
	ch := trimmed[0]
	if ch >= 'a' && ch <= 'z' {
		ch -= 'a' - 'A'
	}
	if ch < 'A' || ch > 'Z' {
		return fmt.Errorf("%w: %q", ErrInvalidDriveLetter, driveLetter)
	}
	return openDrivePathOS(string(ch) + `:\`)
}

// OpenBrowserURL opens an http/https URL in the user's default browser.
func OpenBrowserURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q", ErrInvalidURL, rawURL)
	}
	return openBrowserURLOS(u.String())
}

// BuildTrayIconDIB builds an RT_ICON-compatible 32-bpp ARGB DIB + 1-bpp AND mask.
func BuildTrayIconDIB(size int) []byte {
	if size <= 0 {
		size = 32
	}
	andStride := ((size + 31) / 32) * 4
	xorBytes := size * size * 4
	andBytes := andStride * size
	buf := make([]byte, 40+xorBytes+andBytes)

	// BITMAPINFOHEADER (40 bytes, little-endian). biHeight is 2*size for XOR + AND masks.
	binary.LittleEndian.PutUint32(buf[0:4], 40)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(int32(size)))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(int32(size*2)))
	binary.LittleEndian.PutUint16(buf[12:14], 1)
	binary.LittleEndian.PutUint16(buf[14:16], 32)
	binary.LittleEndian.PutUint32(buf[16:20], 0) // BI_RGB
	binary.LittleEndian.PutUint32(buf[20:24], uint32(xorBytes+andBytes))

	xorOff := 40
	andOff := 40 + xorBytes
	margin := size / 16
	if margin < 1 {
		margin = 1
	}
	radius := size / 5
	if radius < 2 {
		radius = 2
	}

	// Render bottom-up (y=0 in DIB is bottom row; top-down visual row is ty = size-1-y).
	for y := 0; y < size; y++ {
		ty := size - 1 - y
		for x := 0; x < size; x++ {
			b, g, r, a := iconPixel(x, ty, size, margin, radius)
			pixOff := xorOff + (y*size+x)*4
			buf[pixOff+0] = b
			buf[pixOff+1] = g
			buf[pixOff+2] = r
			buf[pixOff+3] = a
			if a == 0 {
				byteIdx := andOff + y*andStride + x/8
				buf[byteIdx] |= 1 << uint(7-(x%8))
			}
		}
	}
	return buf
}

func iconPixel(x, y, size, margin, radius int) (b, g, r, a byte) {
	if !inRoundedRect(x, y, margin, margin, size-margin-1, size-margin-1, radius) {
		return 0, 0, 0, 0
	}

	// White "T" glyph centered inside the blue badge.
	cx := size / 2
	top := size * 26 / 100
	barBot := size * 40 / 100
	stemBot := size * 76 / 100
	halfBar := size * 26 / 100
	halfStem := size * 9 / 100
	if halfStem < 1 {
		halfStem = 1
	}
	inTopBar := y >= top && y <= barBot && x >= cx-halfBar && x <= cx+halfBar
	inStem := y >= top && y <= stemBot && x >= cx-halfStem && x <= cx+halfStem
	if inTopBar || inStem {
		return 255, 255, 255, 255
	}

	// Subtle vertical blue gradient (#3B82F6 -> #1D4ED8).
	t := (y * 255) / size
	rr := byte(59 - (30*t)/255)
	gg := byte(130 - (52*t)/255)
	bb := byte(246 - (30*t)/255)
	return bb, gg, rr, 255
}

func inRoundedRect(x, y, x0, y0, x1, y1, r int) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	dx, dy := 0, 0
	if x < x0+r {
		dx = (x0 + r) - x
	} else if x > x1-r {
		dx = x - (x1 - r)
	}
	if y < y0+r {
		dy = (y0 + r) - y
	} else if y > y1-r {
		dy = y - (y1 - r)
	}
	return dx*dx+dy*dy <= r*r
}

type App struct {
	actions TrayActions
	os      osTrayState
}

func New(actions TrayActions) (*App, error) {
	if actions == nil {
		return nil, ErrNilActions
	}
	return &App{actions: actions}, nil
}

func (a *App) Run(ctx context.Context) error {
	return a.runOS(ctx)
}

func (a *App) Close() error {
	return a.closeOS()
}

func (a *App) Refresh() error {
	return a.refreshOS()
}

func (a *App) Notify(title, message string, isError bool) error {
	return a.notifyOS(title, message, isError)
}

func (a *App) ShowWarningDialog(title, message string) error {
	return showDialogOS(title, message)
}

type dialogNotifier struct{}

func NewDialogNotifier() Notifier {
	return dialogNotifier{}
}

func (dialogNotifier) Notify(title, message string, isError bool) error {
	if isError {
		return showDialogOS(title, message)
	}
	return nil
}

func (dialogNotifier) ShowWarningDialog(title, message string) error {
	return showDialogOS(title, message)
}
