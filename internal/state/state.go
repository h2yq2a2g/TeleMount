package state

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	stateFileName     = "state.json"
	masterKeyFileName = "master.key"
	sessionTTL        = 7 * 24 * time.Hour
)

type BackupAccountConfig struct {
	ID                      string `json:"id"`
	APIID                   int    `json:"api_id"`
	APIHash                 string `json:"api_hash"`
	OwnChannelID            int64  `json:"own_channel_id"`
	OwnChannelAccessHash    int64  `json:"own_channel_access_hash"`
	OwnChannelTitle         string `json:"own_channel_title"`
	SourceChannelID         int64  `json:"source_channel_id"`
	SourceChannelAccessHash int64  `json:"source_channel_access_hash"`
	CreatedAt               int64  `json:"created_at"`
}

type LocalState struct {
	AdminUser         string                `json:"admin_user"`
	AdminPassHash     string                `json:"admin_pass_hash"`
	SessionSecret     string                `json:"session_secret"`
	TGAPIID           int                   `json:"tg_api_id"`
	TGAPIHash         string                `json:"tg_api_hash"`
	ChannelID         int64                 `json:"channel_id"`
	ChannelAccessHash int64                 `json:"channel_access_hash"`
	ChannelTitle      string                `json:"channel_title"`
	SetupCompleted    bool                  `json:"setup_completed"`
	Language          string                `json:"language,omitempty"`
	BackupAccounts    []BackupAccountConfig `json:"backup_accounts,omitempty"`
}

type masterKeyFile struct {
	MasterKeyHex string `json:"master_key_hex"`
	KDFSaltHex   string `json:"kdf_salt_hex"`
}

type Manager struct {
	mu        sync.RWMutex
	dataDir   string
	state     LocalState
	masterKey []byte
	kdfSalt   []byte
}

func Load(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	m := &Manager{dataDir: dataDir}

	if b, err := os.ReadFile(filepath.Join(dataDir, stateFileName)); err == nil {
		if err := json.Unmarshal(b, &m.state); err != nil {
			return nil, fmt.Errorf("parse %s: %w", stateFileName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if m.state.SessionSecret == "" {
		sec := make([]byte, 32)
		if _, err := rand.Read(sec); err != nil {
			return nil, err
		}
		m.state.SessionSecret = hex.EncodeToString(sec)
		if err := m.saveStateLocked(); err != nil {
			return nil, err
		}
	}

	if b, err := os.ReadFile(filepath.Join(dataDir, masterKeyFileName)); err == nil {
		var kf masterKeyFile
		if err := json.Unmarshal(b, &kf); err != nil {
			return nil, fmt.Errorf("parse %s: %w", masterKeyFileName, err)
		}
		mk, err := hex.DecodeString(kf.MasterKeyHex)
		if err != nil || len(mk) != 32 {
			return nil, errors.New("corrupted master.key")
		}
		salt, err := hex.DecodeString(kf.KDFSaltHex)
		if err != nil || len(salt) != 16 {
			return nil, errors.New("corrupted kdf salt in master.key")
		}
		m.masterKey = mk
		m.kdfSalt = salt
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return m, nil
}

func (m *Manager) DataDir() string {
	return m.dataDir
}

func (m *Manager) Snapshot() LocalState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := m.state
	cp.BackupAccounts = append([]BackupAccountConfig(nil), m.state.BackupAccounts...)
	return cp
}

func (m *Manager) BackupAccounts() []BackupAccountConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]BackupAccountConfig(nil), m.state.BackupAccounts...)
}

func (m *Manager) GetBackupAccount(id string) (BackupAccountConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.state.BackupAccounts {
		if a.ID == id {
			return a, true
		}
	}
	return BackupAccountConfig{}, false
}

func (m *Manager) UpsertBackupAccount(cfg BackupAccountConfig) error {
	if cfg.ID == "" {
		return errors.New("backup account id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	replaced := false
	for i := range m.state.BackupAccounts {
		if m.state.BackupAccounts[i].ID == cfg.ID {
			m.state.BackupAccounts[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		if cfg.CreatedAt == 0 {
			cfg.CreatedAt = time.Now().Unix()
		}
		m.state.BackupAccounts = append(m.state.BackupAccounts, cfg)
	}
	return m.saveStateLocked()
}

func (m *Manager) RemoveBackupAccount(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	filtered := m.state.BackupAccounts[:0]
	for _, a := range m.state.BackupAccounts {
		if a.ID != id {
			filtered = append(filtered, a)
		}
	}
	m.state.BackupAccounts = filtered
	return m.saveStateLocked()
}

// PromoteBackupState swaps the promoted backup account's session file and channel config into the Primary slot.
func (m *Manager) PromoteBackupState(id string) (BackupAccountConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, a := range m.state.BackupAccounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return BackupAccountConfig{}, errors.New("backup account not found")
	}
	promoted := m.state.BackupAccounts[idx]
	if promoted.OwnChannelID == 0 {
		return BackupAccountConfig{}, errors.New("backup account has no storage channel initialized")
	}

	backupSession := filepath.Join(m.dataDir, fmt.Sprintf("tg_session_%s.json", id))
	primarySession := filepath.Join(m.dataDir, "tg_session.json")
	if b, err := os.ReadFile(backupSession); err == nil {
		if err := writeFileAtomic(primarySession, b, 0o600); err != nil {
			return BackupAccountConfig{}, fmt.Errorf("swap session file: %w", err)
		}
		_ = os.Remove(backupSession)
	}

	if promoted.APIID > 0 && promoted.APIHash != "" {
		m.state.TGAPIID = promoted.APIID
		m.state.TGAPIHash = promoted.APIHash
	}
	m.state.ChannelID = promoted.OwnChannelID
	m.state.ChannelAccessHash = promoted.OwnChannelAccessHash
	m.state.ChannelTitle = promoted.OwnChannelTitle
	m.state.BackupAccounts = append(m.state.BackupAccounts[:idx], m.state.BackupAccounts[idx+1:]...)

	if err := m.saveStateLocked(); err != nil {
		return BackupAccountConfig{}, err
	}
	return promoted, nil
}

func (m *Manager) HasAdmin() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.AdminUser != "" && m.state.AdminPassHash != ""
}

func (m *Manager) SetAdmin(username, password string) error {
	username = strings.TrimSpace(username)
	if len(username) < 2 || len(password) < 6 {
		return errors.New("username must be >= 2 chars and password >= 6 chars")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.AdminUser = username
	m.state.AdminPassHash = string(hash)
	return m.saveStateLocked()
}

func (m *Manager) VerifyAdmin(username, password string) bool {
	m.mu.RLock()
	user := m.state.AdminUser
	hash := m.state.AdminPassHash
	m.mu.RUnlock()
	if user == "" || hash == "" || username != user {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (m *Manager) SetTelegramApp(apiID int, apiHash string) error {
	apiHash = strings.TrimSpace(apiHash)
	if apiID <= 0 || apiHash == "" {
		return errors.New("invalid telegram api_id or api_hash")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.TGAPIID = apiID
	m.state.TGAPIHash = apiHash
	return m.saveStateLocked()
}

func (m *Manager) SetChannel(id, accessHash int64, title string) error {
	if id == 0 {
		return errors.New("invalid channel id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.ChannelID = id
	m.state.ChannelAccessHash = accessHash
	m.state.ChannelTitle = title
	return m.saveStateLocked()
}

func (m *Manager) ClearChannel() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.ChannelID = 0
	m.state.ChannelAccessHash = 0
	m.state.ChannelTitle = ""
	return m.saveStateLocked()
}

func (m *Manager) MarkSetupCompleted() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.SetupCompleted = true
	return m.saveStateLocked()
}

// NormalizeLanguage returns "vi" for Vietnamese or "en" (default) for everything else.
func NormalizeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "vi", "vi-vn":
		return "vi"
	default:
		return "en"
	}
}

func (m *Manager) Language() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return NormalizeLanguage(m.state.Language)
}

func (m *Manager) SetLanguage(lang string) error {
	raw := strings.ToLower(strings.TrimSpace(lang))
	if raw != "en" && raw != "vi" {
		return errors.New("unsupported language: expected 'en' or 'vi'")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Language == raw {
		return nil
	}
	m.state.Language = raw
	return m.saveStateLocked()
}

func (m *Manager) HasChannel() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.ChannelID != 0
}

func (m *Manager) HasMasterKey() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.masterKey) == 32 && len(m.kdfSalt) == 16
}

func (m *Manager) MasterKey() (key []byte, salt []byte, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.masterKey) != 32 || len(m.kdfSalt) != 16 {
		return nil, nil, false
	}
	return append([]byte(nil), m.masterKey...), append([]byte(nil), m.kdfSalt...), true
}

func (m *Manager) SetMasterKey(key, salt []byte) error {
	if len(key) != 32 || len(salt) != 16 {
		return errors.New("invalid master key or salt length")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kf := masterKeyFile{
		MasterKeyHex: hex.EncodeToString(key),
		KDFSaltHex:   hex.EncodeToString(salt),
	}
	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.dataDir, masterKeyFileName), data, 0o600); err != nil {
		return err
	}
	m.masterKey = append([]byte(nil), key...)
	m.kdfSalt = append([]byte(nil), salt...)
	return nil
}

func (m *Manager) SignSession(username string) string {
	m.mu.RLock()
	sec := m.state.SessionSecret
	m.mu.RUnlock()
	exp := time.Now().Add(sessionTTL).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%d", username, exp)))
	mac := hmac.New(sha256.New, []byte(sec))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

func (m *Manager) VerifySession(token string) (string, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	m.mu.RLock()
	sec := m.state.SessionSecret
	admin := m.state.AdminUser
	m.mu.RUnlock()

	mac := hmac.New(sha256.New, []byte(sec))
	mac.Write([]byte(parts[0]))
	wantSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(wantSig)) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	fields := strings.SplitN(string(raw), "|", 2)
	if len(fields) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	if fields[0] == "" || fields[0] != admin {
		return "", false
	}
	return fields[0], true
}

func (m *Manager) saveStateLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(m.dataDir, stateFileName), data, 0o600)
}

func writeFileAtomic(target string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}
