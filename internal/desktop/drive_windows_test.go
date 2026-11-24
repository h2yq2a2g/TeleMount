//go:build windows

package desktop

import "testing"

func TestWindowsDriveCheckerSmoke(t *testing.T) {
	checker := NewDriveChecker()

	// System drive C: is expected to exist on Windows hosts.
	used, err := checker.IsDriveInUse("C:")
	if err != nil {
		t.Fatalf("IsDriveInUse(\"C:\") unexpected error: %v", err)
	}
	if !used {
		t.Fatal("IsDriveInUse(\"C:\") = false, want true on Windows")
	}

	if _, err := checker.IsDriveInUse("bad"); err == nil {
		t.Fatal("IsDriveInUse(\"bad\") expected error, got nil")
	}
}
