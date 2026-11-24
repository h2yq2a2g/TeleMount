package desktop

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// StartupStore abstracts OS startup entry persistence (e.g., HKCU Run registry).
type StartupStore interface {
	Get(name string) (string, bool, error)
	Set(name, value string) error
	Delete(name string) error
}

// StartupManager manages enabling, disabling, and querying application auto-start.
type StartupManager struct {
	store   StartupStore
	appName string
}

// NewStartupManager creates a StartupManager using store (or NewWindowsStartupStore
// when store is nil) for the given application entry name.
func NewStartupManager(store StartupStore, appName string) *StartupManager {
	if store == nil {
		store = NewWindowsStartupStore()
	}
	return &StartupManager{
		store:   store,
		appName: appName,
	}
}

func (m *StartupManager) validate() error {
	if m == nil || m.store == nil {
		return errors.New("startup store is not initialized")
	}
	if strings.TrimSpace(m.appName) == "" {
		return errors.New("startup app name cannot be empty")
	}
	for _, r := range m.appName {
		if r < 0x20 || r == 0x7f {
			return errors.New("startup app name contains invalid control character")
		}
	}
	return nil
}

// FormatCommand formats exePath and optional args into a Windows Run-key command line,
// double-quoting exePath without escaping backslashes and placing args outside quotes.
func (m *StartupManager) FormatCommand(exePath string, args ...string) (string, error) {
	if err := validateCommandToken("executable path", exePath); err != nil {
		return "", err
	}
	exe := strings.TrimSpace(exePath)
	if exe == "" {
		return "", errors.New("executable path cannot be empty")
	}

	var b strings.Builder
	b.Grow(len(exe) + 2)
	b.WriteByte('"')
	b.WriteString(exe)
	b.WriteByte('"')

	for _, rawArg := range args {
		if strings.TrimSpace(rawArg) == "" {
			return "", errors.New("startup argument cannot be empty")
		}
		if err := validateCommandToken("argument", rawArg); err != nil {
			return "", err
		}
		b.WriteByte(' ')
		if strings.ContainsRune(rawArg, ' ') {
			b.WriteByte('"')
			b.WriteString(rawArg)
			// Double trailing backslashes before the closing quote per CommandLineToArgvW rules.
			for i := len(rawArg) - 1; i >= 0 && rawArg[i] == '\\'; i-- {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
		} else {
			b.WriteString(rawArg)
		}
	}
	return b.String(), nil
}

func validateCommandToken(label, s string) error {
	for _, r := range s {
		if r == '"' {
			return fmt.Errorf("%s cannot contain double quotes", label)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains invalid control character", label)
		}
	}
	return nil
}

// Enable registers the startup entry for exePath and optional args.
func (m *StartupManager) Enable(exePath string, args ...string) error {
	if err := m.validate(); err != nil {
		return err
	}
	cmd, err := m.FormatCommand(exePath, args...)
	if err != nil {
		return err
	}
	return m.store.Set(m.appName, cmd)
}

// Disable removes the startup entry if present (idempotent on os.ErrNotExist).
func (m *StartupManager) Disable() error {
	if err := m.validate(); err != nil {
		return err
	}
	err := m.store.Delete(m.appName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// IsEnabled reports whether a non-empty startup entry is currently configured.
func (m *StartupManager) IsEnabled() (bool, error) {
	if err := m.validate(); err != nil {
		return false, err
	}
	val, ok, err := m.store.Get(m.appName)
	if err != nil {
		return false, err
	}
	return ok && strings.TrimSpace(val) != "", nil
}
