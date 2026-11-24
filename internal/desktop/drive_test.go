package desktop

import (
	"errors"
	"testing"
)

func TestBitmaskDriveChecker(t *testing.T) {
	// Bits for C: (2) and V: (21) set.
	mask := uint32(1<<('C'-'A')) | uint32(1<<('V'-'A'))
	checker := &bitmaskDriveChecker{
		getMask: func() (uint32, error) { return mask, nil },
	}

	inUseInputs := []string{"C", "c:", `C:\`, "C:/", "v", "V:", `V:\`, "  v:/ "}
	for _, in := range inUseInputs {
		used, err := checker.IsDriveInUse(in)
		if err != nil {
			t.Fatalf("IsDriveInUse(%q) unexpected error: %v", in, err)
		}
		if !used {
			t.Errorf("IsDriveInUse(%q) = false, want true", in)
		}
	}

	freeInputs := []string{"A:", "B:", "D:", "Z:"}
	for _, in := range freeInputs {
		used, err := checker.IsDriveInUse(in)
		if err != nil {
			t.Fatalf("IsDriveInUse(%q) unexpected error: %v", in, err)
		}
		if used {
			t.Errorf("IsDriveInUse(%q) = true, want false", in)
		}
	}

	// Test every bit A-Z individually.
	for letter := byte('A'); letter <= 'Z'; letter++ {
		bitMask := uint32(1) << uint(letter-'A')
		c := &bitmaskDriveChecker{getMask: func() (uint32, error) { return bitMask, nil }}
		for probe := byte('A'); probe <= 'Z'; probe++ {
			got, err := c.IsDriveInUse(string(probe) + ":")
			if err != nil {
				t.Fatalf("probe %c: %v", probe, err)
			}
			want := probe == letter
			if got != want {
				t.Fatalf("bit %c probe %c: got %v, want %v", letter, probe, got, want)
			}
		}
	}
}

func TestBitmaskDriveCheckerValidationAndErrors(t *testing.T) {
	called := false
	sentinel := errors.New("win32 failure")
	checker := &bitmaskDriveChecker{
		getMask: func() (uint32, error) {
			called = true
			return 0, sentinel
		},
	}

	// Invalid drive syntax must fail before invoking getMask.
	invalid := []string{"", "  ", "1:", "AB:", `V:\foo`, "é:", "V//"}
	for _, in := range invalid {
		if _, err := checker.IsDriveInUse(in); err == nil {
			t.Errorf("IsDriveInUse(%q) expected error, got nil", in)
		}
	}
	if called {
		t.Fatal("getMask called for invalid drive syntax")
	}

	// Valid drive propagates getMask error instead of treating mask=0 as free.
	used, err := checker.IsDriveInUse("V:")
	if !errors.Is(err, sentinel) {
		t.Fatalf("IsDriveInUse(\"V:\") error = %v, want wrapped %v", err, sentinel)
	}
	if used {
		t.Fatal("IsDriveInUse(\"V:\") = true on error, want false")
	}
}
