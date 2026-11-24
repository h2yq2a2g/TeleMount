package desktop

import "testing"

func TestMapWindowsPathToVirtual(t *testing.T) {
	cases := []struct {
		winPath string
		drive   string
		want    string
		wantErr bool
	}{
		{`V:\`, "V:", "/", false},
		{`v:\`, "V:", "/", false},
		{`V:`, "V:", "/", false},
		{`V:"`, "V:", "/", false},
		{`V:\"`, "V:", "/", false}, // Windows Explorer %V root trailing backslash before closing quote
		{`V:\Docs\report.pdf`, "V:", "/Docs/report.pdf", false},
		{`v:/Docs/Sub Folder/a.txt`, "V:", "/Docs/Sub Folder/a.txt", false},
		{`  "V:\Work\notes.md"  `, "V:", "/Work/notes.md", false},
		{`C:\Windows\System32`, "V:", "", true},
		{`W:\Docs\a.txt`, "V:", "", true},
		{`V:\..\secret`, "V:", "", true},
		{`V:relative.txt`, "V:", "", true},
		{``, "V:", "", true},
		{`V:\bad` + "\x00" + `file`, "V:", "", true},
	}
	for _, tc := range cases {
		got, err := MapWindowsPathToVirtual(tc.winPath, tc.drive)
		if tc.wantErr {
			if err == nil {
				t.Errorf("MapWindowsPathToVirtual(%q, %q) = %q, want error", tc.winPath, tc.drive, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("MapWindowsPathToVirtual(%q, %q) unexpected err: %v", tc.winPath, tc.drive, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MapWindowsPathToVirtual(%q, %q) = %q, want %q", tc.winPath, tc.drive, got, tc.want)
		}
	}
}

func TestShellAppliesToFilters(t *testing.T) {
	if got := shellAppliesToForItems("V:"); got != `System.ParsingPath:~< "V:\"` {
		t.Fatalf("items AppliesTo = %q", got)
	}
	if got := shellAppliesToForBackground("V:"); got != `System.ItemFolderPathDisplay:~< "V:\"` {
		t.Fatalf("background AppliesTo = %q", got)
	}
}
