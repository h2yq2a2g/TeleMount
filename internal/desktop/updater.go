package desktop

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultGitHubAPIBase = "https://api.github.com"

var ErrUpdateCanceled = errors.New("update canceled by user")

type UpdateInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	Available      bool   `json:"available"`
	ReleaseURL     string `json:"release_url"`
	AssetName      string `json:"asset_name"`
	AssetURL       string `json:"asset_url"`
	ChecksumsURL   string `json:"checksums_url"`
}

// CompareSemver compares two semantic version strings (with optional leading "v" and pre-release suffix).
// Returns -1 if a < b, 0 if a == b, +1 if a > b. Non-semver strings like "dev" sort below any release.
func CompareSemver(a, b string) int {
	av, aPre, aOK := parseSemver(a)
	bv, bPre, bOK := parseSemver(b)
	if !aOK && !bOK {
		return 0
	}
	if !aOK {
		return -1
	}
	if !bOK {
		return 1
	}
	for i := 0; i < 3; i++ {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	// Release (no prerelease) > prerelease for same major.minor.patch.
	if aPre == "" && bPre != "" {
		return 1
	}
	if aPre != "" && bPre == "" {
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func parseSemver(raw string) ([3]int, string, bool) {
	var parts [3]int
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return parts, "", false
	}
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	pre := ""
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	segs := strings.Split(s, ".")
	if len(segs) < 2 || len(segs) > 3 {
		return parts, "", false
	}
	for i, seg := range segs {
		n, err := strconv.Atoi(seg)
		if err != nil || n < 0 {
			return parts, "", false
		}
		parts[i] = n
	}
	return parts, pre, true
}

// ParseSHA256Sums parses standard "<hex>  <filename>" checksum lines into filename -> lowercase hex.
func ParseSHA256Sums(content string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = sum
	}
	return out
}

// CheckLatestRelease queries GitHub Releases for repo and locates the Windows Setup installer + SHA256SUMS.txt.
func CheckLatestRelease(ctx context.Context, client *http.Client, apiBaseURL, repo, currentVersion string) (*UpdateInfo, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(apiBaseURL) == "" {
		apiBaseURL = DefaultGitHubAPIBase
	}
	endpoint := strings.TrimRight(apiBaseURL, "/") + "/repos/" + strings.Trim(repo, "/") + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "TeleMount-Updater/"+currentVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API returned HTTP %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release metadata: %w", err)
	}
	if rel.TagName == "" || rel.Draft {
		return &UpdateInfo{CurrentVersion: currentVersion}, nil
	}

	info := &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  rel.TagName,
		ReleaseURL:     rel.HTMLURL,
	}
	for _, a := range rel.Assets {
		low := strings.ToLower(a.Name)
		if strings.HasPrefix(low, "telemount-setup-") && strings.HasSuffix(low, ".exe") {
			info.AssetName = a.Name
			info.AssetURL = a.BrowserDownloadURL
		} else if low == "sha256sums.txt" {
			info.ChecksumsURL = a.BrowserDownloadURL
		}
	}

	// Only mark available if current is a real release older than latest (never auto-overwrite dev builds unless explicitly forced),
	// and both the Windows Setup installer and SHA256SUMS.txt exist.
	_, _, curValid := parseSemver(currentVersion)
	if curValid && CompareSemver(currentVersion, rel.TagName) < 0 && info.AssetURL != "" && info.ChecksumsURL != "" {
		info.Available = true
	}
	return info, nil
}

func validateDownloadURL(rawURL string, allowHTTPForLoopback bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid download url: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if allowHTTPForLoopback && u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS update URL: %s", rawURL)
	}
	if host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com") && !strings.HasSuffix(host, ".github.com") {
		return fmt.Errorf("refusing untrusted update host: %s", host)
	}
	return nil
}

// DownloadAndVerifyInstaller downloads SHA256SUMS.txt and the Setup .exe into destDir,
// verifies the exact SHA-256 digest before returning the local path, and deletes the file on mismatch.
func DownloadAndVerifyInstaller(ctx context.Context, client *http.Client, destDir string, info *UpdateInfo) (string, error) {
	return downloadAndVerifyInstaller(ctx, client, destDir, info, false)
}

func downloadAndVerifyInstaller(ctx context.Context, client *http.Client, destDir string, info *UpdateInfo, allowLoopbackHTTP bool) (string, error) {
	if info == nil || info.AssetName == "" || info.AssetURL == "" || info.ChecksumsURL == "" {
		return "", errors.New("incomplete release asset metadata (installer or SHA256SUMS.txt missing)")
	}
	if err := validateDownloadURL(info.ChecksumsURL, allowLoopbackHTTP); err != nil {
		return "", err
	}
	if err := validateDownloadURL(info.AssetURL, allowLoopbackHTTP); err != nil {
		return "", err
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}

	// 1. Fetch SHA256SUMS.txt first.
	sumReq, err := http.NewRequestWithContext(ctx, http.MethodGet, info.ChecksumsURL, nil)
	if err != nil {
		return "", err
	}
	sumResp, err := client.Do(sumReq)
	if err != nil {
		return "", fmt.Errorf("download SHA256SUMS.txt: %w", err)
	}
	sumBytes, err := io.ReadAll(io.LimitReader(sumResp.Body, 256*1024))
	sumResp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("read SHA256SUMS.txt: %w", err)
	}
	if sumResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download SHA256SUMS.txt: HTTP %d", sumResp.StatusCode)
	}
	sums := ParseSHA256Sums(string(sumBytes))
	wantHash, ok := sums[info.AssetName]
	if !ok {
		return "", fmt.Errorf("SHA256SUMS.txt missing checksum for %s", info.AssetName)
	}

	// Reuse already-downloaded installer if its SHA-256 matches exactly.
	finalPath := filepath.Join(destDir, filepath.Base(info.AssetName))
	if gotHash, err := fileSHA256Hex(finalPath); err == nil && subtle.ConstantTimeCompare([]byte(gotHash), []byte(wantHash)) == 1 {
		return finalPath, nil
	}

	// 2. Download installer to temp file while streaming SHA-256 hash.
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, info.AssetURL, nil)
	if err != nil {
		return "", err
	}
	dlResp, err := client.Do(dlReq)
	if err != nil {
		return "", fmt.Errorf("download installer: %w", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download installer: HTTP %d", dlResp.StatusCode)
	}

	tmp, err := os.CreateTemp(destDir, "update-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(dlResp.Body, 256*1024*1024)); err != nil {
		return "", fmt.Errorf("write installer: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(gotHash), []byte(wantHash)) != 1 {
		return "", fmt.Errorf("installer SHA-256 mismatch (got %s, want %s)", gotHash, wantHash)
	}
	if err := renameWithRetry(tmpPath, finalPath); err != nil {
		return "", err
	}
	success = true
	return finalPath, nil
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
