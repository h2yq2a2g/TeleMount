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

func TestShellMenuSpecsLocalizationAndNoStatusVerb(t *testing.T) {
	en := DefaultShellMenuSpecs()
	if len(en) != 3 {
		t.Fatalf("expected 3 English shell verbs (no status), got %d: %+v", len(en), en)
	}
	if en[0].Label != "Always keep on this device" || en[1].Label != "Free up space" || en[2].Label != "Sync now" {
		t.Fatalf("unexpected English shell menu labels: %+v", en)
	}
	vi := ShellMenuSpecs("vi")
	if len(vi) != 3 || vi[0].Action != "pin" || vi[1].Action != "free" || vi[2].Action != "sync" {
		t.Fatalf("unexpected Vietnamese shell menu specs: %+v", vi)
	}
	for _, sp := range append(en, vi...) {
		if sp.Action == "status" {
			t.Fatalf("status shell action must be removed, found %+v", sp)
		}
	}
}
