package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"path"
	"strings"
)

type DesktopStatus struct {
	Enabled              bool     `json:"enabled"`
	WinFspInstalled      bool     `json:"winfsp_installed"`
	WinFspGuidance       string   `json:"winfsp_guidance"`
	DriveLetter          string   `json:"drive_letter"`
	Mounted              bool     `json:"mounted"`
	MountError           string   `json:"mount_error"`
	RunAtStartup         bool     `json:"run_at_startup"`
	AutoMount            bool     `json:"auto_mount"`
	DiskCacheMaxBytes    int64    `json:"disk_cache_max_bytes"`
	DiskCacheBytes       int64    `json:"disk_cache_bytes"`
	DiskCachePinnedBytes int64    `json:"disk_cache_pinned_bytes"`
	DiskCacheEntries     int      `json:"disk_cache_entries"`
	PinnedPaths          []string `json:"pinned_paths"`
	QueuePendingCount    int      `json:"queue_pending_count"`
	QueuePendingBytes    int64    `json:"queue_pending_bytes"`
	QueueSyncing         bool     `json:"queue_syncing"`
	QueueLastError       string   `json:"queue_last_error"`
}

type DesktopSettingsUpdate struct {
	DriveLetter       *string   `json:"drive_letter"`
	DiskCacheMaxBytes *int64    `json:"disk_cache_max_bytes"`
	PinnedPaths       *[]string `json:"pinned_paths"`
	RunAtStartup      *bool     `json:"run_at_startup"`
	AutoMount         *bool     `json:"auto_mount"`
}

type DesktopController interface {
	Status() DesktopStatus
	UpdateSettings(DesktopSettingsUpdate) (DesktopStatus, error)
	Mount() error
	Unmount() error
	OpenDrive() error
	FlushQueue(context.Context) error
	ClearUnpinnedCache() error
}

type ShellActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// ShellActionHandler is an optional extension of DesktopController for Windows Explorer context menu actions.
type ShellActionHandler interface {
	HandleShellAction(ctx context.Context, action, virtualPath string) (ShellActionResult, error)
}

func (s *Server) SetDesktopController(ctrl DesktopController) {
	s.desktopMu.Lock()
	defer s.desktopMu.Unlock()
	s.desktopCtrl = ctrl
}

func (s *Server) SetDesktopIPCToken(token string) {
	s.desktopMu.Lock()
	defer s.desktopMu.Unlock()
	s.desktopIPCToken = token
}

func (s *Server) verifyDesktopIPCToken(r *http.Request) bool {
	s.desktopMu.RLock()
	want := s.desktopIPCToken
	s.desktopMu.RUnlock()
	if want == "" {
		return false
	}
	got := r.Header.Get("X-TeleMount-IPC-Token")
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) desktop() DesktopController {
	s.desktopMu.RLock()
	defer s.desktopMu.RUnlock()
	return s.desktopCtrl
}

func normalizeStatus(st DesktopStatus) DesktopStatus {
	if st.PinnedPaths == nil {
		st.PinnedPaths = []string{}
	}
	return st
}

func normalizeDriveLetter(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimRight(trimmed, `\/`)
	if len(trimmed) == 2 && trimmed[1] == ':' {
		trimmed = trimmed[:1]
	}
	if len(trimmed) != 1 {
		return "", errors.New("invalid drive letter: expected format like V:")
	}
	ch := trimmed[0]
	if ch >= 'a' && ch <= 'z' {
		ch -= 'a' - 'A'
	}
	if ch < 'A' || ch > 'Z' {
		return "", errors.New("invalid drive letter: expected A-Z")
	}
	if ch == 'C' {
		return "", errors.New("drive letter C: is reserved for the system drive")
	}
	return string(ch) + ":", nil
}

func normalizePinnedPaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			return nil, errors.New("pinned path must not be empty")
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return nil, errors.New("pinned path contains invalid control characters")
			}
		}
		p = strings.ReplaceAll(p, `\`, `/`)
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return nil, errors.New("pinned path must not contain '..'")
			}
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		cleaned := path.Clean(p)
		if _, exists := seen[cleaned]; !exists {
			seen[cleaned] = struct{}{}
			out = append(out, cleaned)
		}
	}
	return out, nil
}

func (s *Server) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeJSON(w, http.StatusOK, DesktopStatus{
			Enabled:     false,
			PinnedPaths: []string{},
		})
		return
	}
	writeJSON(w, http.StatusOK, normalizeStatus(ctrl.Status()))
}

func (s *Server) handleDesktopSettings(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	var req DesktopSettingsUpdate
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DriveLetter != nil {
		norm, err := normalizeDriveLetter(*req.DriveLetter)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.DriveLetter = &norm
	}
	if req.DiskCacheMaxBytes != nil && *req.DiskCacheMaxBytes <= 0 {
		writeErr(w, http.StatusBadRequest, "disk_cache_max_bytes must be greater than 0")
		return
	}
	if req.PinnedPaths != nil {
		norm, err := normalizePinnedPaths(*req.PinnedPaths)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.PinnedPaths = &norm
	}
	updated, err := ctrl.UpdateSettings(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, normalizeStatus(updated))
}

func (s *Server) handleDesktopMount(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	if err := ctrl.Mount(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": normalizeStatus(ctrl.Status()),
	})
}

func (s *Server) handleDesktopUnmount(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	if err := ctrl.Unmount(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": normalizeStatus(ctrl.Status()),
	})
}

func (s *Server) handleDesktopOpen(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	if err := ctrl.OpenDrive(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDesktopCacheClear(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	if err := ctrl.ClearUnpinnedCache(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": normalizeStatus(ctrl.Status()),
	})
}

func (s *Server) handleDesktopQueueFlush(w http.ResponseWriter, r *http.Request) {
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	if err := ctrl.FlushQueue(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": normalizeStatus(ctrl.Status()),
	})
}

func (s *Server) handleDesktopShellAction(w http.ResponseWriter, r *http.Request) {
	if !s.verifyDesktopIPCToken(r) {
		if !s.engine.State().HasAdmin() {
			writeErr(w, http.StatusForbidden, "initial admin setup required")
			return
		}
		if _, ok := s.isAuthenticated(r); !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
	}
	ctrl := s.desktop()
	if ctrl == nil {
		writeErr(w, http.StatusServiceUnavailable, "desktop mode is not enabled")
		return
	}
	handler, ok := ctrl.(ShellActionHandler)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "shell actions not supported")
		return
	}
	var req struct {
		Action string `json:"action"`
		Path   string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := handler.HandleShellAction(r.Context(), strings.ToLower(strings.TrimSpace(req.Action)), req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
