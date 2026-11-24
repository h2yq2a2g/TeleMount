package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWinFspDLLNameMapping(t *testing.T) {
	cases := []struct {
		arch string
		want string
		ok   bool
	}{
		{"amd64", "winfsp-x64.dll", true},
		{"arm64", "winfsp-a64.dll", true},
		{"386", "winfsp-x86.dll", true},
		{"arm", "", false},
		{"riscv64", "", false},
		{"", "", false},
	}

	for _, tc := range cases {
		got, ok := winfspDLLName(tc.arch)
		if ok != tc.ok || got != tc.want {
			t.Errorf("winfspDLLName(%q) = (%q, %v), want (%q, %v)", tc.arch, got, ok, tc.want, tc.ok)
		}
	}

	status := newWinFspProbeWithDeps(winfspDeps{goarch: "riscv64"}).Check()
	if status.Installed {
		t.Fatal("unsupported arch reported Installed=true")
	}
	if !strings.Contains(status.Guidance, "riscv64") ||
		!strings.Contains(status.Guidance, winFspReleaseURL) ||
		!strings.Contains(status.Guidance, winFspWingetCommand) {
		t.Fatalf("unexpected unsupported arch guidance: %q", status.Guidance)
	}
}

func TestWinFspProbeFoundViaDLLSearch(t *testing.T) {
	regCalled := false
	probe := newWinFspProbeWithDeps(winfspDeps{
		goarch: "amd64",
		findDLL: func(dllName string) (string, bool) {
			if dllName != "winfsp-x64.dll" {
				t.Fatalf("unexpected dllName %q", dllName)
			}
			return `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`, true
		},
		readInstallDir: func() (string, error) {
			regCalled = true
			return "", errors.New("should not be called")
		},
	})

	status := probe.Check()
	if !status.Installed {
		t.Fatal("Installed = false, want true")
	}
	if status.DLLPath != `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll` {
		t.Fatalf("DLLPath = %q", status.DLLPath)
	}
	if status.Guidance != "" {
		t.Fatalf("Guidance = %q, want empty when installed", status.Guidance)
	}
	if regCalled {
		t.Fatal("registry fallback should not run when DLL search succeeds")
	}

	// Blank path from findDLL falls back to dllName cleanly.
	probeBlankPath := newWinFspProbeWithDeps(winfspDeps{
		goarch:  "arm64",
		findDLL: func(string) (string, bool) { return "   ", true },
	})
	statusBlank := probeBlankPath.Check()
	if !statusBlank.Installed || statusBlank.DLLPath != "winfsp-a64.dll" {
		t.Fatalf("unexpected status on blank findDLL path: %+v", statusBlank)
	}
}

func TestWinFspProbeRegistryFallback(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dllPath := filepath.Join(binDir, "winfsp-x64.dll")
	if err := os.WriteFile(dllPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	regularFileCheck := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && !info.IsDir()
	}

	// Trailing slash in InstallDir normalizes cleanly and finds DLL.
	probe := newWinFspProbeWithDeps(winfspDeps{
		goarch:  "amd64",
		findDLL: func(string) (string, bool) { return "", false },
		readInstallDir: func() (string, error) {
			return tmp + string(filepath.Separator), nil
		},
		fileExists: regularFileCheck,
	})
	status := probe.Check()
	if !status.Installed || status.DLLPath != dllPath || status.Guidance != "" {
		t.Fatalf("registry fallback got %+v, want Installed=true DLLPath=%q", status, dllPath)
	}

	// Missing DLL on disk under valid InstallDir -> Installed=false + guidance.
	emptyDir := t.TempDir()
	probeMissingFile := newWinFspProbeWithDeps(winfspDeps{
		goarch:         "amd64",
		findDLL:        func(string) (string, bool) { return "", false },
		readInstallDir: func() (string, error) { return emptyDir, nil },
		fileExists:     regularFileCheck,
	})
	missingStatus := probeMissingFile.Check()
	if missingStatus.Installed || missingStatus.DLLPath != "" {
		t.Fatalf("expected not installed when DLL missing, got %+v", missingStatus)
	}
	if !strings.Contains(missingStatus.Guidance, winFspReleaseURL) || !strings.Contains(missingStatus.Guidance, winFspWingetCommand) {
		t.Fatalf("missing actionable guidance: %q", missingStatus.Guidance)
	}

	// Directory named winfsp-x64.dll must NOT count as installed.
	dirAsDLL := filepath.Join(t.TempDir(), "bin", "winfsp-x64.dll")
	if err := os.MkdirAll(dirAsDLL, 0o700); err != nil {
		t.Fatal(err)
	}
	probeDirAsDLL := newWinFspProbeWithDeps(winfspDeps{
		goarch:         "amd64",
		findDLL:        func(string) (string, bool) { return "", false },
		readInstallDir: func() (string, error) { return filepath.Dir(filepath.Dir(dirAsDLL)), nil },
		fileExists:     regularFileCheck,
	})
	if st := probeDirAsDLL.Check(); st.Installed {
		t.Fatalf("directory at DLL path must not report Installed=true: %+v", st)
	}
}

func TestWinFspProbeMissingAndNilDeps(t *testing.T) {
	cases := []struct {
		name string
		deps winfspDeps
	}{
		{
			name: "zero deps",
			deps: winfspDeps{goarch: "amd64"},
		},
		{
			name: "blank install dir",
			deps: winfspDeps{
				goarch:         "386",
				findDLL:        func(string) (string, bool) { return "", false },
				readInstallDir: func() (string, error) { return "   ", nil },
				fileExists:     func(string) bool { return true },
			},
		},
		{
			name: "registry error",
			deps: winfspDeps{
				goarch:         "amd64",
				findDLL:        func(string) (string, bool) { return "", false },
				readInstallDir: func() (string, error) { return "", errors.New("not found") },
				fileExists:     func(string) bool { return true },
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newWinFspProbeWithDeps(tc.deps).Check()
			if st.Installed || st.DLLPath != "" {
				t.Fatalf("expected Installed=false and empty DLLPath, got %+v", st)
			}
			if !strings.Contains(st.Guidance, winFspReleaseURL) || !strings.Contains(st.Guidance, winFspWingetCommand) {
				t.Fatalf("guidance missing install instructions: %q", st.Guidance)
			}
		})
	}
}

func TestCheckWinFspHostSmoke(t *testing.T) {
	st := CheckWinFsp()
	if st.Installed {
		if st.DLLPath == "" || st.Guidance != "" {
			t.Fatalf("installed status invariant violated: %+v", st)
		}
	} else {
		if st.DLLPath != "" || !strings.Contains(st.Guidance, winFspReleaseURL) || !strings.Contains(st.Guidance, winFspWingetCommand) {
			t.Fatalf("not-installed status invariant violated: %+v", st)
		}
	}
}
