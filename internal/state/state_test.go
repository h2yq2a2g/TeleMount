package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerLifecycleAuthMasterKeyAndSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.HasAdmin() || m.HasChannel() || m.HasMasterKey() {
		t.Fatal("fresh manager should not have admin, channel, or master key")
	}

	if err := m.SetAdmin("a", "short"); err == nil {
		t.Fatal("expected short username/password rejected")
	}
	if err := m.SetAdmin("admin", "strongpass123"); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	if !m.VerifyAdmin("admin", "strongpass123") {
		t.Fatal("expected valid admin credentials accepted")
	}
	if m.VerifyAdmin("admin", "wrong") || m.VerifyAdmin("other", "strongpass123") {
		t.Fatal("expected invalid admin credentials rejected")
	}

	tok := m.SignSession("admin")
	if u, ok := m.VerifySession(tok); !ok || u != "admin" {
		t.Fatalf("VerifySession(valid) = (%q, %v), want (admin, true)", u, ok)
	}
	if _, ok := m.VerifySession(tok + "x"); ok {
		t.Fatal("expected tampered session token rejected")
	}
	if _, ok := m.VerifySession("malformed"); ok {
		t.Fatal("expected malformed session token rejected")
	}

	key := bytes.Repeat([]byte{0x42}, 32)
	salt := bytes.Repeat([]byte{0x24}, 16)
	if err := m.SetMasterKey(key[:16], salt); err == nil {
		t.Fatal("expected short master key rejected")
	}
	if err := m.SetMasterKey(key, salt); err != nil {
		t.Fatalf("SetMasterKey: %v", err)
	}

	if err := m.SetChannel(12345, 67890, "Vault"); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	if err := m.SetLanguage("vi"); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	if err := m.SetLanguage("fr"); err == nil {
		t.Fatal("expected unsupported language rejected")
	}

	// Reload from disk and verify persistence + defensive copies.
	m2, err := Load(dir)
	if err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	if !m2.HasAdmin() || !m2.HasChannel() || !m2.HasMasterKey() || m2.Language() != "vi" {
		t.Fatalf("reloaded state mismatch: snap=%+v", m2.Snapshot())
	}
	gotKey, gotSalt, ok := m2.MasterKey()
	if !ok || !bytes.Equal(gotKey, key) || !bytes.Equal(gotSalt, salt) {
		t.Fatal("reloaded master key/salt mismatch")
	}
	gotKey[0] ^= 0xff
	gotKey2, _, _ := m2.MasterKey()
	if !bytes.Equal(gotKey2, key) {
		t.Fatal("MasterKey did not return a defensive copy")
	}
}

func TestPromoteBackupStateSwapsSessionAndChannel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SetTelegramApp(111, "primary-hash")
	_ = m.SetChannel(1001, 2002, "Primary")

	if err := os.WriteFile(filepath.Join(dir, "tg_session.json"), []byte(`{"primary":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tg_session_acc-bak1.json"), []byte(`{"backup":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.UpsertBackupAccount(BackupAccountConfig{
		ID:                   "acc-bak1",
		APIID:                222,
		APIHash:              "backup-hash",
		OwnChannelID:         9009,
		OwnChannelAccessHash: 8888,
		OwnChannelTitle:      "BackupVault",
	}); err != nil {
		t.Fatal(err)
	}

	promoted, err := m.PromoteBackupState("acc-bak1")
	if err != nil {
		t.Fatalf("PromoteBackupState: %v", err)
	}
	if promoted.OwnChannelID != 9009 {
		t.Fatalf("unexpected promoted config: %+v", promoted)
	}

	snap := m.Snapshot()
	if snap.ChannelID != 9009 || snap.TGAPIID != 222 || len(snap.BackupAccounts) != 0 {
		t.Fatalf("unexpected state after promote: %+v", snap)
	}
	sessBytes, err := os.ReadFile(filepath.Join(dir, "tg_session.json"))
	if err != nil || string(sessBytes) != `{"backup":true}` {
		t.Fatalf("primary session not swapped: %q err=%v", string(sessBytes), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tg_session_acc-bak1.json")); !os.IsNotExist(err) {
		t.Fatalf("expected old backup session removed, got err=%v", err)
	}
}
