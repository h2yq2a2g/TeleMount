//go:build windows

package desktop

import (
	"reflect"
	"syscall"
	"testing"
	"unsafe"
)

func parseWindowsCommandLine(t *testing.T, cmd string) []string {
	t.Helper()
	ptr, err := syscall.UTF16PtrFromString(cmd)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", cmd, err)
	}
	var argc int32
	argv, err := syscall.CommandLineToArgv(ptr, &argc)
	if err != nil {
		t.Fatalf("CommandLineToArgv(%q): %v", cmd, err)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(argv)))

	got := make([]string, argc)
	for i := int32(0); i < argc; i++ {
		got[i] = syscall.UTF16ToString(argv[i][:])
	}
	return got
}

func TestStartupManagerFormatCommandWindowsRoundTrip(t *testing.T) {
	mgr := NewStartupManager(newFakeStartupStore(), "TeleMount")

	cases := []struct {
		name string
		exe  string
		args []string
	}{
		{
			name: "spaced exe and flag",
			exe:  `C:\Program Files\TeleMount\telemount-desktop.exe`,
			args: []string{"--minimized"},
		},
		{
			name: "spaced exe with trailing backslash",
			exe:  `C:\Program Files\TeleMount\`,
			args: []string{"--minimized"},
		},
		{
			name: "spaced arg with 1 trailing backslash before --minimized",
			exe:  `C:\TeleMount\telemount-desktop.exe`,
			args: []string{"--data-dir", `C:\Users\My User\TeleMount\`, "--minimized"},
		},
		{
			name: "spaced arg with 2 trailing backslashes before --minimized",
			exe:  `C:\TeleMount\telemount-desktop.exe`,
			args: []string{"--data-dir", `C:\Users\My User\TeleMount\\`, "--minimized"},
		},
		{
			name: "unc paths with spaces and trailing backslash",
			exe:  `\\fileserver\TeleMount Share\bin\telemount-desktop.exe`,
			args: []string{"--data-dir", `\\fileserver\Data Share\TeleMount\`, "--minimized"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := mgr.FormatCommand(tc.exe, tc.args...)
			if err != nil {
				t.Fatalf("FormatCommand error: %v", err)
			}
			got := parseWindowsCommandLine(t, cmd)
			want := append([]string{tc.exe}, tc.args...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("CommandLineToArgv(%q) = %#v, want %#v", cmd, got, want)
			}
		})
	}
}
