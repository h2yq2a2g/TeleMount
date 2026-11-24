package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.2", "v1.0.2", 0},
		{"v1.10.0", "v1.2.0", 1},
		{"1.0.3-rc1", "1.0.3", -1},
		{"dev", "v1.0.0", -1},
		{"dev", "dev", 0},
	}
	for _, tc := range cases {
		if got := CompareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckAndVerifyUpdateFlow(t *testing.T) {
	installerBytes := []byte("MZ-fake-telemount-setup-payload-v1.0.3")
	sumBytes := sha256.Sum256(installerBytes)
	validHash := hex.EncodeToString(sumBytes[:])
	tamper := false

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/h2yq2a2g/TeleMount/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.3",
				"html_url": "https://github.com/h2yq2a2g/TeleMount/releases/tag/v1.0.3",
				"assets": []map[string]string{
					{"name": "TeleMount-Setup-v1.0.3-windows-amd64.exe", "browser_download_url": ts.URL + "/dl/setup.exe"},
					{"name": "SHA256SUMS.txt", "browser_download_url": ts.URL + "/dl/SHA256SUMS.txt"},
				},
			})
		case "/dl/SHA256SUMS.txt":
			_, _ = w.Write([]byte(validHash + "  TeleMount-Setup-v1.0.3-windows-amd64.exe\n"))
		case "/dl/setup.exe":
			if tamper {
				_, _ = w.Write([]byte("tampered-binary"))
				return
			}
			_, _ = w.Write(installerBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	ctx := context.Background()

	// 1. Older version detects update available.
	info, err := CheckLatestRelease(ctx, ts.Client(), ts.URL, "h2yq2a2g/TeleMount", "v1.0.2")
	if err != nil {
		t.Fatalf("CheckLatestRelease: %v", err)
	}
	if !info.Available || info.LatestVersion != "v1.0.3" || info.AssetName != "TeleMount-Setup-v1.0.3-windows-amd64.exe" {
		t.Fatalf("unexpected UpdateInfo: %+v", info)
	}

	// 2. Already up-to-date reports Available=false.
	same, err := CheckLatestRelease(ctx, ts.Client(), ts.URL, "h2yq2a2g/TeleMount", "v1.0.3")
	if err != nil || same.Available {
		t.Fatalf("expected Available=false when up to date, got (%+v, %v)", same, err)
	}

	// 3. Dev build reports Available=false (does not auto-overwrite local dev run).
	devInfo, err := CheckLatestRelease(ctx, ts.Client(), ts.URL, "h2yq2a2g/TeleMount", "dev")
	if err != nil || devInfo.Available {
		t.Fatalf("expected Available=false for dev version, got (%+v, %v)", devInfo, err)
	}

	// 4. Tampered download MUST be rejected and cleaned up.
	tamper = true
	destDir := t.TempDir()
	if _, err := downloadAndVerifyInstaller(ctx, ts.Client(), destDir, info, true); err == nil {
		t.Fatal("expected SHA-256 mismatch error on tampered installer")
	}
	if _, err := os.Stat(filepath.Join(destDir, info.AssetName)); !os.IsNotExist(err) {
		t.Fatalf("tampered file must not be left in destDir: err=%v", err)
	}

	// 5. Valid download succeeds and verifies SHA-256.
	tamper = false
	localPath, err := downloadAndVerifyInstaller(ctx, ts.Client(), destDir, info, true)
	if err != nil {
		t.Fatalf("downloadAndVerifyInstaller valid: %v", err)
	}
	got, err := os.ReadFile(localPath)
	if err != nil || string(got) != string(installerBytes) {
		t.Fatalf("downloaded file mismatch: (%q, %v)", got, err)
	}

	// 6. Untrusted non-GitHub host rejected in production mode.
	badInfo := *info
	badInfo.AssetURL = "https://evil.example.com/setup.exe"
	if _, err := DownloadAndVerifyInstaller(ctx, ts.Client(), destDir, &badInfo); err == nil {
		t.Fatal("expected untrusted host to be rejected")
	}
}
