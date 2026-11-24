//go:build !windows

package tray

import "context"

type osTrayState struct{}

func (a *App) runOS(context.Context) error            { return ErrUnsupportedPlatform }
func (a *App) closeOS() error                         { return ErrUnsupportedPlatform }
func (a *App) refreshOS() error                       { return ErrUnsupportedPlatform }
func (a *App) notifyOS(string, string, bool) error    { return ErrUnsupportedPlatform }
func showDialogOS(string, string) error               { return ErrUnsupportedPlatform }
func openDrivePathOS(string) error                    { return ErrUnsupportedPlatform }
func openBrowserURLOS(string) error                   { return ErrUnsupportedPlatform }
