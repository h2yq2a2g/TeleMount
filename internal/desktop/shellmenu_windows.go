//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const shellMenuKeyName = "TeleMount"

type shellMenuTarget struct {
	baseSubkey   string
	pathArg      string
	appliesToFn  func(string) string
}

var windowsShellMenuTargets = []shellMenuTarget{
	{baseSubkey: `Software\Classes\*\shell\` + shellMenuKeyName, pathArg: `%1`, appliesToFn: shellAppliesToForItems},
	{baseSubkey: `Software\Classes\Directory\shell\` + shellMenuKeyName, pathArg: `%1`, appliesToFn: shellAppliesToForItems},
	{baseSubkey: `Software\Classes\Directory\Background\shell\` + shellMenuKeyName, pathArg: `%V`, appliesToFn: shellAppliesToForBackground},
	{baseSubkey: `Software\Classes\Drive\shell\` + shellMenuKeyName, pathArg: `%1`, appliesToFn: shellAppliesToForItems},
}

// RegisterShellMenu installs the per-user Windows Explorer right-click submenu
// scoped strictly to driveLetter via Windows AppliesTo AQS filters (no admin required).
func RegisterShellMenu(exePath, driveLetter string) error {
	exe := strings.TrimSpace(exePath)
	if exe == "" {
		return errors.New("executable path required")
	}
	drv, err := NormalizeDriveLetter(driveLetter)
	if err != nil {
		return err
	}

	iconVal := fmt.Sprintf(`"%s",0`, exe)
	specs := DefaultShellMenuSpecs()

	for _, t := range windowsShellMenuTargets {
		// Clear previous subkeys first so old items never linger across upgrades.
		_ = deleteRegistryTree(registry.CURRENT_USER, t.baseSubkey)

		rootKey, _, err := registry.CreateKey(registry.CURRENT_USER, t.baseSubkey, registry.SET_VALUE|registry.CREATE_SUB_KEY)
		if err != nil {
			return fmt.Errorf("create shell key %s: %w", t.baseSubkey, err)
		}
		if err := rootKey.SetStringValue("MUIVerb", "TeleMount"); err != nil {
			_ = rootKey.Close()
			return err
		}
		if err := rootKey.SetStringValue("Icon", iconVal); err != nil {
			_ = rootKey.Close()
			return err
		}
		if err := rootKey.SetStringValue("SubCommands", ""); err != nil {
			_ = rootKey.Close()
			return err
		}
		if err := rootKey.SetStringValue("AppliesTo", t.appliesToFn(drv)); err != nil {
			_ = rootKey.Close()
			return err
		}
		_ = rootKey.Close()

		for _, sp := range specs {
			itemPath := t.baseSubkey + `\shell\` + sp.Key
			itemKey, _, err := registry.CreateKey(registry.CURRENT_USER, itemPath, registry.SET_VALUE|registry.CREATE_SUB_KEY)
			if err != nil {
				return fmt.Errorf("create submenu key %s: %w", itemPath, err)
			}
			_ = itemKey.SetStringValue("MUIVerb", sp.Label)
			_ = itemKey.SetStringValue("Icon", iconVal)
			_ = itemKey.Close()

			cmdKey, _, err := registry.CreateKey(registry.CURRENT_USER, itemPath+`\command`, registry.SET_VALUE)
			if err != nil {
				return fmt.Errorf("create command key %s: %w", itemPath, err)
			}
			cmdLine := fmt.Sprintf(`"%s" --shell-action=%s --path="%s"`, exe, sp.Action, t.pathArg)
			if err := cmdKey.SetStringValue("", cmdLine); err != nil {
				_ = cmdKey.Close()
				return err
			}
			_ = cmdKey.Close()
		}
	}
	return nil
}

// UnregisterShellMenu removes all TeleMount Explorer context menu keys for the current user.
func UnregisterShellMenu() error {
	var firstErr error
	for _, t := range windowsShellMenuTargets {
		if err := deleteRegistryTree(registry.CURRENT_USER, t.baseSubkey); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func deleteRegistryTree(root registry.Key, subkey string) error {
	k, err := registry.OpenKey(root, subkey, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := k.ReadSubKeyNames(-1)
	_ = k.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := deleteRegistryTree(root, subkey+`\`+name); err != nil {
			return err
		}
	}
	err = registry.DeleteKey(root, subkey)
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
