package desktop

import (
	"errors"
	"os"
	"testing"
)

type fakeStartupStore struct {
	entries   map[string]string
	getErr    error
	setErr    error
	deleteErr error
}

func newFakeStartupStore() *fakeStartupStore {
	return &fakeStartupStore{entries: make(map[string]string)}
}

func (f *fakeStartupStore) Get(name string) (string, bool, error) {
	if f.getErr != nil {
		return "", false, f.getErr
	}
	val, ok := f.entries[name]
	return val, ok, nil
}

func (f *fakeStartupStore) Set(name, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.entries[name] = value
	return nil
}

func (f *fakeStartupStore) Delete(name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.entries[name]; !ok {
		return os.ErrNotExist
	}
	delete(f.entries, name)
	return nil
}

func TestStartupManagerFormatCommand(t *testing.T) {
	mgr := NewStartupManager(newFakeStartupStore(), "TeleMount")

	cmd, err := mgr.FormatCommand(`  C:\Program Files\TeleMount\telemount-desktop.exe  `, "--minimized")
	if err != nil {
		t.Fatalf("FormatCommand unexpected error: %v", err)
	}
	want := `"C:\Program Files\TeleMount\telemount-desktop.exe" --minimized`
	if cmd != want {
		t.Fatalf("FormatCommand = %q, want %q", cmd, want)
	}

	cmdNoArgs, err := mgr.FormatCommand(`C:\TeleMount\app.exe`)
	if err != nil {
		t.Fatalf("FormatCommand no args unexpected error: %v", err)
	}
	if cmdNoArgs != `"C:\TeleMount\app.exe"` {
		t.Fatalf("FormatCommand no args = %q", cmdNoArgs)
	}

	cmdSpaceArg, err := mgr.FormatCommand(`C:\TeleMount\app.exe`, "--data-dir", `C:\Users\My User\TeleMount`)
	if err != nil {
		t.Fatalf("FormatCommand space arg unexpected error: %v", err)
	}
	wantSpace := `"C:\TeleMount\app.exe" --data-dir "C:\Users\My User\TeleMount"`
	if cmdSpaceArg != wantSpace {
		t.Fatalf("FormatCommand space arg = %q, want %q", cmdSpaceArg, wantSpace)
	}

	cmdTrailingSlash1, err := mgr.FormatCommand(`C:\TeleMount\app.exe`, "--data-dir", `C:\Users\My User\TeleMount\`, "--minimized")
	if err != nil {
		t.Fatalf("FormatCommand 1 trailing backslash unexpected error: %v", err)
	}
	wantTrailing1 := `"C:\TeleMount\app.exe" --data-dir "C:\Users\My User\TeleMount\\" --minimized`
	if cmdTrailingSlash1 != wantTrailing1 {
		t.Fatalf("FormatCommand 1 trailing backslash = %q, want %q", cmdTrailingSlash1, wantTrailing1)
	}

	cmdTrailingSlash2, err := mgr.FormatCommand(`C:\TeleMount\app.exe`, "--data-dir", `C:\Users\My User\TeleMount\\`, "--minimized")
	if err != nil {
		t.Fatalf("FormatCommand 2 trailing backslashes unexpected error: %v", err)
	}
	wantTrailing2 := `"C:\TeleMount\app.exe" --data-dir "C:\Users\My User\TeleMount\\\\" --minimized`
	if cmdTrailingSlash2 != wantTrailing2 {
		t.Fatalf("FormatCommand 2 trailing backslashes = %q, want %q", cmdTrailingSlash2, wantTrailing2)
	}

	invalidCases := []struct {
		name string
		exe  string
		args []string
	}{
		{"empty exe", "", nil},
		{"whitespace exe", "   ", nil},
		{"quote in exe", `C:\Bad"Path\app.exe`, nil},
		{"newline in exe", "C:\\app.exe\n", nil},
		{"nul in exe", "C:\\app.exe\x00", nil},
		{"tab in exe", "C:\\app\t.exe", nil},
		{"empty arg", `C:\app.exe`, []string{""}},
		{"whitespace arg", `C:\app.exe`, []string{"   "}},
		{"quote in arg", `C:\app.exe`, []string{`--foo="bar"`}},
		{"control char in arg", `C:\app.exe`, []string{"--bad\r"}},
	}
	for _, tc := range invalidCases {
		if _, err := mgr.FormatCommand(tc.exe, tc.args...); err == nil {
			t.Errorf("FormatCommand(%s) expected error, got nil", tc.name)
		}
	}
}

func TestStartupManagerLifecycleAndErrors(t *testing.T) {
	store := newFakeStartupStore()
	mgr := NewStartupManager(store, "TeleMount")

	enabled, err := mgr.IsEnabled()
	if err != nil {
		t.Fatalf("initial IsEnabled error: %v", err)
	}
	if enabled {
		t.Fatal("initial IsEnabled = true, want false")
	}

	// Disable when missing is idempotent even when store returns os.ErrNotExist.
	if err := mgr.Disable(); err != nil {
		t.Fatalf("initial Disable unexpected error: %v", err)
	}

	if err := mgr.Enable(`C:\Program Files\TeleMount\telemount-desktop.exe`, "--minimized"); err != nil {
		t.Fatalf("Enable error: %v", err)
	}
	if got := store.entries["TeleMount"]; got != `"C:\Program Files\TeleMount\telemount-desktop.exe" --minimized` {
		t.Fatalf("stored command = %q", got)
	}

	enabled, err = mgr.IsEnabled()
	if err != nil || !enabled {
		t.Fatalf("IsEnabled after Enable = (%v, %v), want (true, nil)", enabled, err)
	}

	// Whitespace-only registry value counts as disabled.
	store.entries["TeleMount"] = "   "
	enabled, err = mgr.IsEnabled()
	if err != nil || enabled {
		t.Fatalf("IsEnabled with blank entry = (%v, %v), want (false, nil)", enabled, err)
	}

	if err := mgr.Disable(); err != nil {
		t.Fatalf("Disable error: %v", err)
	}
	enabled, err = mgr.IsEnabled()
	if err != nil || enabled {
		t.Fatalf("IsEnabled after Disable = (%v, %v), want (false, nil)", enabled, err)
	}

	// Empty or control-character appName must fail cleanly without touching registry.
	for _, badName := range []string{"   ", "TeleMount\x00", "Tele\nDrive", "Tele\x7fDrive"} {
		badApp := NewStartupManager(store, badName)
		if err := badApp.Enable(`C:\app.exe`); err == nil {
			t.Fatalf("Enable with bad appName %q expected error", badName)
		}
		if err := badApp.Disable(); err == nil {
			t.Fatalf("Disable with bad appName %q expected error", badName)
		}
		if _, err := badApp.IsEnabled(); err == nil {
			t.Fatalf("IsEnabled with bad appName %q expected error", badName)
		}
	}

	// Store errors propagate.
	boom := errors.New("registry access denied")
	store.getErr = boom
	if _, err := mgr.IsEnabled(); !errors.Is(err, boom) {
		t.Fatalf("IsEnabled error = %v, want %v", err, boom)
	}
	store.getErr = nil

	store.setErr = boom
	if err := mgr.Enable(`C:\app.exe`); !errors.Is(err, boom) {
		t.Fatalf("Enable error = %v, want %v", err, boom)
	}
	store.setErr = nil

	store.deleteErr = boom
	if err := mgr.Disable(); !errors.Is(err, boom) {
		t.Fatalf("Disable error = %v, want %v", err, boom)
	}
}
