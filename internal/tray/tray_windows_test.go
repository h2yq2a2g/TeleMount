//go:build windows

package tray

import (
	"runtime"
	"testing"
	"unicode/utf16"
	"unsafe"
)

func TestWin32StructSizes(t *testing.T) {
	is64 := unsafe.Sizeof(uintptr(0)) == 8
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if !is64 {
			t.Fatal("expected 8-byte pointers on 64-bit arch")
		}
	}

	wantNID := uintptr(956)
	wantWC := uintptr(48)
	wantMSG := uintptr(28)
	if is64 {
		wantNID = 976
		wantWC = 80
		wantMSG = 48
	}

	if got := unsafe.Sizeof(notifyIconData{}); got != wantNID {
		t.Fatalf("notifyIconData size=%d, want %d", got, wantNID)
	}
	if got := unsafe.Sizeof(wndClassExW{}); got != wantWC {
		t.Fatalf("wndClassExW size=%d, want %d", got, wantWC)
	}
	if got := unsafe.Sizeof(msg{}); got != wantMSG {
		t.Fatalf("msg size=%d, want %d", got, wantMSG)
	}
	if got := unsafe.Sizeof(point{}); got != 8 {
		t.Fatalf("point size=%d, want 8", got)
	}
}

func TestCopyUTF16Safety(t *testing.T) {
	var buf [6]uint16
	copyUTF16(buf[:], "hello world")
	if buf[5] != 0 {
		t.Fatalf("expected NUL terminator at last slot, got %d", buf[5])
	}
	if got := utf16.Decode(buf[:5]); string(got) != "hello" {
		t.Fatalf("expected truncated string 'hello', got %q", string(got))
	}

	// Surrogate pair ("🚀" = 2 UTF-16 units) must not be split across boundary.
	var sbuf [4]uint16 // 3 code units + 1 NUL
	copyUTF16(sbuf[:], "ab🚀")
	if sbuf[3] != 0 || sbuf[2] != 0 {
		t.Fatalf("expected surrogate pair omitted cleanly without splitting, got %v", sbuf)
	}
	if got := utf16.Decode(sbuf[:2]); string(got) != "ab" {
		t.Fatalf("expected 'ab', got %q", string(got))
	}
}
