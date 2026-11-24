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

func TestDeelevateNoOpWhenNonElevatedAndEnvBlock(t *testing.T) {
	// With loop guard set, RelaunchNonElevatedIfNeeded must always be a safe no-op.
	t.Setenv(noDeelevateEnv, "1")
	if relaunched, err := RelaunchNonElevatedIfNeeded(`C:\Program Files\TeleMount\telemount.exe`, []string{"--minimized"}); err != nil || relaunched {
		t.Fatalf("RelaunchNonElevatedIfNeeded with guard = (%v, %v), want (false, nil)", relaunched, err)
	}
	blk := buildUTF16EnvBlock([]string{"A=1", "B=2"})
	// "A=1\0B=2\0\0" -> 3+1+3+1+1 = 9 UTF-16 code units, ending in double NUL.
	if len(blk) != 9 || blk[len(blk)-1] != 0 || blk[len(blk)-2] != 0 {
		t.Fatalf("unexpected UTF-16 env block: %v", blk)
	}
}
