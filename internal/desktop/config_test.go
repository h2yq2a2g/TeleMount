package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "telemount")
	cfg := DefaultConfig(dataDir)

	if cfg.DriveLetter != "V:" {
		t.Fatalf("DriveLetter = %q, want V:", cfg.DriveLetter)
	}
	if cfg.DiskCacheMaxBytes != 10*1024*1024*1024 {
		t.Fatalf("DiskCacheMaxBytes = %d, want 10 GiB", cfg.DiskCacheMaxBytes)
	}
	if cfg.DiskCacheDir != filepath.Join(dataDir, "cache") {
		t.Fatalf("DiskCacheDir = %q, want %q", cfg.DiskCacheDir, filepath.Join(dataDir, "cache"))
	}
	if cfg.PinnedPaths == nil || len(cfg.PinnedPaths) != 0 {
		t.Fatalf("PinnedPaths = %#v, want non-nil empty slice", cfg.PinnedPaths)
	}
	if cfg.RunAtStartup {
		t.Fatal("RunAtStartup = true, want false")
	}
	if !cfg.AutoMount {
		t.Fatal("AutoMount = false, want true")
	}
	if cfg.WebAddr != "127.0.0.1:8080" {
		t.Fatalf("WebAddr = %q, want 127.0.0.1:8080", cfg.WebAddr)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig(DefaultConfig) unexpected error: %v", err)
	}
}

func TestNormalizeDriveLetter(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v", "V:"},
		{"V", "V:"},
		{"V:", "V:"},
		{"v:", "V:"},
		{`V:\`, "V:"},
		{"V:/", "V:"},
		{`  v:\ `, "V:"},
		{"a", "A:"},
		{"z:/", "Z:"},
	}
	for _, tc := range valid {
		got, err := NormalizeDriveLetter(tc.in)
		if err != nil {
			t.Errorf("NormalizeDriveLetter(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeDriveLetter(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"C",
		"c:",
		`C:\`,
		"C:/",
		"1",
		"1:",
		"AB",
		"AB:",
		"V::",
		"V/",
		`V\`,
		`V:\\`,
		`V:\foo`,
		":V",
		"é:",
	}
	for _, in := range invalid {
		got, err := NormalizeDriveLetter(in)
		if err == nil {
			t.Errorf("NormalizeDriveLetter(%q) = %q, want error", in, got)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	base := DefaultConfig(t.TempDir())

	validAddrs := []string{"127.0.0.1:8080", "127.0.0.1:0", "[::1]:8080", "localhost:8080", ":8080", "127.0.0.1:65535"}
	for _, addr := range validAddrs {
		cfg := base
		cfg.WebAddr = addr
		if err := ValidateConfig(cfg); err != nil {
			t.Errorf("ValidateConfig(WebAddr=%q) unexpected error: %v", addr, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty drive", func(c *Config) { c.DriveLetter = "" }},
		{"reserved C drive", func(c *Config) { c.DriveLetter = "C:" }},
		{"zero cache bytes", func(c *Config) { c.DiskCacheMaxBytes = 0 }},
		{"negative cache bytes", func(c *Config) { c.DiskCacheMaxBytes = -1 }},
		{"empty cache dir", func(c *Config) { c.DiskCacheDir = "   " }},
		{"nul cache dir", func(c *Config) { c.DiskCacheDir = "cache\x00dir" }},
		{"nul pinned path", func(c *Config) { c.PinnedPaths = []string{"/ok", "/bad\x00path"} }},
		{"empty web addr", func(c *Config) { c.WebAddr = "" }},
		{"missing port", func(c *Config) { c.WebAddr = "localhost" }},
		{"empty port", func(c *Config) { c.WebAddr = "127.0.0.1:" }},
		{"port too high", func(c *Config) { c.WebAddr = "127.0.0.1:99999" }},
		{"negative port", func(c *Config) { c.WebAddr = "127.0.0.1:-1" }},
		{"plus port", func(c *Config) { c.WebAddr = "127.0.0.1:+80" }},
		{"non-numeric port", func(c *Config) { c.WebAddr = "127.0.0.1:abc" }},
		{"scheme in web addr", func(c *Config) { c.WebAddr = "http://127.0.0.1:8080" }},
		{"nul in web addr host", func(c *Config) { c.WebAddr = "127.0.0.1\x00:8080" }},
		{"bell ctrl in web addr host", func(c *Config) { c.WebAddr = "127.0.0.1\a:8080" }},
		{"del ctrl in web addr host", func(c *Config) { c.WebAddr = "127.0.0.1\x7f:8080" }},
		{"nul in web addr port", func(c *Config) { c.WebAddr = "127.0.0.1:8080\x00" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatalf("ValidateConfig(%s) expected error, got nil", tc.name)
			}
		})
	}
}

func TestConfigStoreLoadErrorsAndPartialMerge(t *testing.T) {
	if _, err := LoadConfig("", ""); err == nil {
		t.Fatal("LoadConfig(\"\", \"\") expected error, got nil")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, DefaultConfigFileName)

	// Missing file loads defaults cleanly.
	store, err := LoadConfig("", dir)
	if err != nil {
		t.Fatalf("LoadConfig missing file unexpected error: %v", err)
	}
	got := store.Get()
	if got.DriveLetter != "V:" || !got.AutoMount || got.DiskCacheDir != filepath.Join(dir, "cache") {
		t.Fatalf("unexpected defaults loaded: %+v", got)
	}

	// Empty file errors.
	if err := os.WriteFile(cfgPath, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfgPath, dir); err == nil {
		t.Fatal("LoadConfig empty file expected error, got nil")
	}

	// Corrupt JSON errors.
	if err := os.WriteFile(cfgPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfgPath, dir); err == nil {
		t.Fatal("LoadConfig corrupt JSON expected error, got nil")
	}

	// Partial JSON merges onto defaults (preserving explicit false for auto_mount).
	partial := `{"drive_letter": "t:\\", "auto_mount": false, "pinned_paths": ["", "\\Docs\\Work\\", "/docs/work", "/Notes"]}`
	if err := os.WriteFile(cfgPath, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = LoadConfig(cfgPath, dir)
	if err != nil {
		t.Fatalf("LoadConfig partial JSON unexpected error: %v", err)
	}
	merged := store.Get()
	if merged.DriveLetter != "T:" {
		t.Fatalf("DriveLetter = %q, want T:", merged.DriveLetter)
	}
	if merged.AutoMount {
		t.Fatal("AutoMount = true, want false from partial JSON")
	}
	if merged.DiskCacheMaxBytes != DefaultDiskCacheMaxBytes {
		t.Fatalf("DiskCacheMaxBytes = %d, want default %d", merged.DiskCacheMaxBytes, DefaultDiskCacheMaxBytes)
	}
	if merged.WebAddr != DefaultWebAddr {
		t.Fatalf("WebAddr = %q, want default %q", merged.WebAddr, DefaultWebAddr)
	}
	if len(merged.PinnedPaths) != 2 || merged.PinnedPaths[0] != "/Docs/Work" || merged.PinnedPaths[1] != "/Notes" {
		t.Fatalf("PinnedPaths = %#v, want [/Docs/Work /Notes]", merged.PinnedPaths)
	}
}

func TestConfigStoreSaveUpdateJSONAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, DefaultConfigFileName)

	store, err := LoadConfig(cfgPath, dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Save valid normalized config and verify snake_case JSON keys on disk.
	toSave := DefaultConfig(dir)
	toSave.DriveLetter = "w"
	toSave.DiskCacheMaxBytes = 4096
	toSave.PinnedPaths = []string{`\Photos\2026\`, "/photos/2026", "   "}
	toSave.RunAtStartup = true
	toSave.AutoMount = false

	if err := store.Save(toSave); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("Unmarshal raw JSON: %v", err)
	}
	for _, key := range []string{"drive_letter", "disk_cache_max_bytes", "disk_cache_dir", "pinned_paths", "run_at_startup", "auto_mount", "web_addr"} {
		if _, ok := rawMap[key]; !ok {
			t.Errorf("missing expected snake_case JSON key %q in %s", key, string(raw))
		}
	}

	// Mutating returned Get() slice must not affect internal store state.
	snap := store.Get()
	snap.PinnedPaths[0] = "/mutated"
	if got := store.Get().PinnedPaths[0]; got != "/Photos/2026" {
		t.Fatalf("Get() returned aliased PinnedPaths slice: got %q", got)
	}

	// Failed Save (validation error or NUL byte) leaves memory & disk intact.
	bad := store.Get()
	bad.DriveLetter = "C:"
	if err := store.Save(bad); err == nil {
		t.Fatal("Save with reserved C: expected error")
	}
	badNul := store.Get()
	badNul.PinnedPaths = []string{"/bad\x00"}
	if err := store.Save(badNul); err == nil {
		t.Fatal("Save with NUL in PinnedPaths expected error")
	}
	if store.Get().DriveLetter != "W:" {
		t.Fatalf("in-memory DriveLetter changed after failed Save: %q", store.Get().DriveLetter)
	}

	// Failed Update leaves memory & disk intact.
	if _, err := store.Update(func(c *Config) error {
		c.DiskCacheMaxBytes = 0
		return nil
	}); err == nil {
		t.Fatal("Update with 0 cache bytes expected error")
	}
	reloaded, err := LoadConfig(cfgPath, dir)
	if err != nil {
		t.Fatalf("reload after failed Update: %v", err)
	}
	if reloaded.Get().DiskCacheMaxBytes != 4096 || reloaded.Get().DriveLetter != "W:" {
		t.Fatalf("disk config altered after failed Update: %+v", reloaded.Get())
	}

	// Failed write (target path is an existing directory) must not mutate in-memory config.
	badTarget := filepath.Join(dir, "dir_as_file")
	if err := os.MkdirAll(badTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	storeBadDisk := &ConfigStore{
		path:    badTarget,
		dataDir: dir,
		cfg:     DefaultConfig(dir),
	}
	next := DefaultConfig(dir)
	next.DriveLetter = "X:"
	if err := storeBadDisk.Save(next); err == nil {
		t.Fatal("Save over directory expected error")
	}
	if storeBadDisk.Get().DriveLetter != "V:" {
		t.Fatalf("in-memory config mutated despite disk failure: %q", storeBadDisk.Get().DriveLetter)
	}

	// Concurrent Get and Update calls should stay consistent without races.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if (idx+j)%2 == 0 {
					_ = store.Get()
				} else {
					if _, err := store.Update(func(c *Config) error {
						c.RunAtStartup = (idx+j)%3 == 0
						return nil
					}); err != nil {
						t.Errorf("concurrent Update error: %v", err)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// Ensure no lingering temp files remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".desktop-") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestNormalizeVirtualPathAndIsPathPinned(t *testing.T) {
	pathCases := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{"/", "/"},
		{"///", "/"},
		{".", "/"},
		{"..", "/"},
		{`\`, "/"},
		{`\Docs\Work\`, "/Docs/Work"},
		{"/docs/../secret", "/secret"},
		{"/../../etc/passwd", "/etc/passwd"},
		{"  /a/b/\x00c/  ", "/a/b/c"},
	}
	for _, tc := range pathCases {
		if got := NormalizeVirtualPath(tc.in); got != tc.want {
			t.Errorf("NormalizeVirtualPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Blank pin entries must NOT pin the root "/".
	if IsPathPinned([]string{"", "   "}, "/secret.txt") {
		t.Fatal("blank pin entries must not pin arbitrary files")
	}
	if IsPathPinned(nil, "/secret.txt") {
		t.Fatal("nil pins must return false")
	}
	// NUL-containing pin entries must be skipped and never normalize to root "/".
	for _, nulPin := range [][]string{{"\x00"}, {" \x00 "}, {"/bad\x00pin"}} {
		if IsPathPinned(nulPin, "/secret.txt") {
			t.Fatalf("IsPathPinned(%q, \"/secret.txt\") = true, want false", nulPin)
		}
		if IsPathPinned(nulPin, "/") {
			t.Fatalf("IsPathPinned(%q, \"/\") = true, want false", nulPin)
		}
	}

	// Explicit "/" pins everything.
	if !IsPathPinned([]string{"/"}, "/anything/here.txt") {
		t.Fatal(`explicit "/" pin must match all paths`)
	}

	// Unicode case-folding with differing UTF-8 byte lengths (U+212A Kelvin <-> k, U+017F Long S <-> s).
	unicodeCases := []struct {
		pinned []string
		path   string
		want   bool
	}{
		{[]string{"/\u212A"}, "/k", true},
		{[]string{"/\u212A"}, "/k/sub", true},
		{[]string{"/k"}, "/\u212A", true},
		{[]string{"/k"}, "/\u212A/sub", true},
		{[]string{"/\u017F"}, "/s/file.txt", true},
		{[]string{"/s"}, "/\u017F/file.txt", true},
		{[]string{"/\u212A"}, "/k2/sub", false},
		{[]string{"/s"}, "/\u017F2/file.txt", false},
	}
	for _, tc := range unicodeCases {
		if got := IsPathPinned(tc.pinned, tc.path); got != tc.want {
			t.Errorf("IsPathPinned(%q, %q) = %v, want %v", tc.pinned, tc.path, got, tc.want)
		}
	}

	pins := []string{`\Docs\Work\`, "/readme.txt"}
	pinCases := []struct {
		path string
		want bool
	}{
		{"/Docs/Work", true},
		{"/docs/work", true},
		{"/docs/work/a.pdf", true},
		{"/DOCS/WORK/sub/b.txt", true},
		{`\docs\work\sub\b.txt`, true},
		{"/docs/work2/a.pdf", false},
		{"/Docs/Work2", false},
		{"/readme.txt", true},
		{"/README.TXT", true},
		{"/readme.txt.bak", false},
		{"/other/file.txt", false},
	}
	for _, tc := range pinCases {
		if got := IsPathPinned(pins, tc.path); got != tc.want {
			t.Errorf("IsPathPinned(%v, %q) = %v, want %v", pins, tc.path, got, tc.want)
		}
	}
}

func TestConfigStorePinDeduplicationEqualFold(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadConfig(filepath.Join(dir, DefaultConfigFileName), dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	updated, err := store.Update(func(c *Config) error {
		c.PinnedPaths = []string{"/s", "/\u017F", "/\u212A/Sub", "/k/sub"}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.PinnedPaths) != 2 || updated.PinnedPaths[0] != "/s" || updated.PinnedPaths[1] != "/\u212A/Sub" {
		t.Fatalf("PinnedPaths = %#v, want [\"/s\", \"/\u212A/Sub\"]", updated.PinnedPaths)
	}
}
