package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultDriveLetter             = "V:"
	DefaultDiskCacheMaxBytes int64 = 10 * 1024 * 1024 * 1024 // 10 GiB
	DefaultWebAddr                 = "127.0.0.1:8080"
	DefaultConfigFileName          = "desktop.json"
)

// Config holds machine-local TeleMount desktop settings persisted separately
// from telemount.db so .tmdb backups never overwrite machine-local values.
type Config struct {
	DriveLetter       string   `json:"drive_letter"`
	DiskCacheMaxBytes int64    `json:"disk_cache_max_bytes"`
	DiskCacheDir      string   `json:"disk_cache_dir"`
	PinnedPaths       []string `json:"pinned_paths"`
	RunAtStartup      bool     `json:"run_at_startup"`
	AutoMount         bool     `json:"auto_mount"`
	WebAddr           string   `json:"web_addr"`
}

// DefaultConfig returns the default desktop configuration for the given dataDir.
func DefaultConfig(dataDir string) Config {
	return Config{
		DriveLetter:       DefaultDriveLetter,
		DiskCacheMaxBytes: DefaultDiskCacheMaxBytes,
		DiskCacheDir:      filepath.Join(dataDir, "cache"),
		PinnedPaths:       []string{},
		RunAtStartup:      false,
		AutoMount:         true,
		WebAddr:           DefaultWebAddr,
	}
}

// NormalizeDriveLetter parses a drive letter specification such as "v", "V:",
// "V:\\", or "V:/" into canonical "X:" form and rejects reserved drive "C:".
func NormalizeDriveLetter(s string) (string, error) {
	ch, err := parseDriveLetter(s)
	if err != nil {
		return "", err
	}
	drive := string(ch) + ":"
	if drive == "C:" {
		return "", errors.New(`drive letter "C:" is reserved for the Windows system drive`)
	}
	return drive, nil
}

// ValidateConfig checks all fields of cfg for validity.
func ValidateConfig(cfg Config) error {
	if _, err := NormalizeDriveLetter(cfg.DriveLetter); err != nil {
		return err
	}
	if cfg.DiskCacheMaxBytes <= 0 {
		return errors.New("disk_cache_max_bytes must be greater than 0")
	}
	cacheDir := strings.TrimSpace(cfg.DiskCacheDir)
	if cacheDir == "" {
		return errors.New("disk_cache_dir cannot be empty")
	}
	if strings.ContainsRune(cacheDir, 0) {
		return errors.New("disk_cache_dir contains invalid null byte")
	}
	if err := validateWebAddr(cfg.WebAddr); err != nil {
		return err
	}
	for _, p := range cfg.PinnedPaths {
		if strings.ContainsRune(p, 0) {
			return errors.New("pinned_paths contains invalid null byte")
		}
	}
	return nil
}

func validateWebAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("web_addr cannot be empty")
	}
	for _, r := range addr {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid web_addr %q: contains control character", addr)
		}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid web_addr %q: %w", addr, err)
	}
	if strings.ContainsAny(host, " \t\r\n/") {
		return fmt.Errorf("invalid web_addr host %q", host)
	}
	for _, r := range host {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid web_addr host %q", host)
		}
	}
	if portStr == "" {
		return fmt.Errorf("invalid web_addr port %q: must be 0-65535", portStr)
	}
	for i := 0; i < len(portStr); i++ {
		if portStr[i] < '0' || portStr[i] > '9' {
			return fmt.Errorf("invalid web_addr port %q: must be 0-65535", portStr)
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid web_addr port %q: must be 0-65535", portStr)
	}
	return nil
}

func normalizeAndValidateConfig(cfg Config) (Config, error) {
	// Validate NUL bytes in PinnedPaths before NormalizeVirtualPath strips them.
	for _, p := range cfg.PinnedPaths {
		if strings.ContainsRune(p, 0) {
			return Config{}, errors.New("pinned_paths contains invalid null byte")
		}
	}

	drive, err := NormalizeDriveLetter(cfg.DriveLetter)
	if err != nil {
		return Config{}, err
	}
	cfg.DriveLetter = drive
	cfg.DiskCacheDir = strings.TrimSpace(cfg.DiskCacheDir)
	cfg.WebAddr = strings.TrimSpace(cfg.WebAddr)

	normalizedPins := make([]string, 0, len(cfg.PinnedPaths))
	for _, raw := range cfg.PinnedPaths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		norm := NormalizeVirtualPath(raw)
		dup := false
		for _, prev := range normalizedPins {
			if strings.EqualFold(prev, norm) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		normalizedPins = append(normalizedPins, norm)
	}
	cfg.PinnedPaths = normalizedPins

	if err := ValidateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func cloneConfig(cfg Config) Config {
	cp := cfg
	cp.PinnedPaths = make([]string, len(cfg.PinnedPaths))
	copy(cp.PinnedPaths, cfg.PinnedPaths)
	return cp
}

// ConfigStore manages thread-safe loading, updating, and atomic persistence of Config.
type ConfigStore struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	cfg     Config
}

// LoadConfig loads a ConfigStore from path (or dataDir/desktop.json when path is empty).
// Missing files initialize cleanly from DefaultConfig(dataDir).
func LoadConfig(path, dataDir string) (*ConfigStore, error) {
	if strings.TrimSpace(path) == "" && strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("config path or dataDir required")
	}
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(dataDir, DefaultConfigFileName)
	}
	if strings.TrimSpace(dataDir) == "" {
		dataDir = filepath.Dir(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	cfg := DefaultConfig(dataDir)
	created := false
	b, err := os.ReadFile(path)
	if err == nil {
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("parse %s: empty file", path)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		created = true
	} else {
		return nil, err
	}

	norm, err := normalizeAndValidateConfig(cfg)
	if err != nil {
		return nil, err
	}
	if created {
		if err := writeJSONAtomic(path, norm); err != nil {
			return nil, err
		}
	}
	return &ConfigStore{
		path:    path,
		dataDir: dataDir,
		cfg:     cloneConfig(norm),
	}, nil
}

// Get returns a snapshot copy of the current configuration.
func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// Save validates, normalizes, and atomically writes cfg to disk.
// In-memory state is updated only after the disk write succeeds.
func (s *ConfigStore) Save(cfg Config) error {
	norm, err := normalizeAndValidateConfig(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeJSONAtomic(s.path, norm); err != nil {
		return err
	}
	s.cfg = norm
	return nil
}

// Update clones the current config, passes it to fn, validates and atomically persists
// the modified config, and returns a snapshot of the committed config.
func (s *ConfigStore) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneConfig(s.cfg)
	if fn != nil {
		if err := fn(&next); err != nil {
			return Config{}, err
		}
	}
	norm, err := normalizeAndValidateConfig(next)
	if err != nil {
		return Config{}, err
	}
	if err := writeJSONAtomic(s.path, norm); err != nil {
		return Config{}, err
	}
	s.cfg = norm
	return cloneConfig(norm), nil
}

func writeJSONAtomic(target string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".desktop-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := renameWithRetry(tmpPath, target); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func renameWithRetry(src, dst string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dst)
	}
	delay := 15 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
			if !isRetryableWindowsRenameErr(err) || attempt == 4 {
				return err
			}
		}
		time.Sleep(delay)
		delay *= 2
	}
	return lastErr
}

func isRetryableWindowsRenameErr(err error) bool {
	if os.IsPermission(err) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// ERROR_SHARING_VIOLATION (32), ERROR_LOCK_VIOLATION (33)
		return errno == 32 || errno == 33
	}
	return false
}

// NormalizeVirtualPath normalizes a TeleMount virtual path to a slash-cleaned
// rooted path (e.g. "\\Docs\\Work\\" -> "/Docs/Work"), preserving case.
func NormalizeVirtualPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\x00", "")
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean("/" + p)
}

// IsPathPinned reports whether virtualPath matches any pinned file or folder prefix
// in pinned (case-insensitively to match WinFsp case-insensitive mounts).
func IsPathPinned(pinned []string, virtualPath string) bool {
	if len(pinned) == 0 {
		return false
	}
	vp := NormalizeVirtualPath(virtualPath)
	vpSegs := strings.Split(strings.TrimPrefix(vp, "/"), "/")
	for _, raw := range pinned {
		if strings.ContainsRune(raw, 0) || strings.TrimSpace(raw) == "" {
			continue
		}
		pin := NormalizeVirtualPath(raw)
		if pin == "/" {
			return true
		}
		pinSegs := strings.Split(strings.TrimPrefix(pin, "/"), "/")
		if len(vpSegs) < len(pinSegs) {
			continue
		}
		matched := true
		for i := range pinSegs {
			if !strings.EqualFold(vpSegs[i], pinSegs[i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
