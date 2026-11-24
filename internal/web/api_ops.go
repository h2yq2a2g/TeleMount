package web

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"telemount/internal/crypto"
	"telemount/internal/db"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats, _ := s.engine.Store().Stats()
	settings, _ := s.engine.Store().GetSettings()
	snap := s.engine.State().Snapshot()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	resp := map[string]any{
		"stats":           stats,
		"cache":           s.engine.Cache().Stats(),
		"packer":          s.engine.Packer().Status(),
		"telegram":        s.tgMgr.Tracker().Snapshot(),
		"settings":        settings,
		"channel":         map[string]any{"id": snap.ChannelID, "title": snap.ChannelTitle},
		"uptime_sec":      int64(time.Since(s.startedAt).Seconds()),
		"mem_alloc_bytes": mem.Alloc,
		"mem_sys_bytes":   mem.Sys,
		"ready":           s.engine.IsReady(),
	}
	if ctrl := s.desktop(); ctrl != nil {
		resp["desktop"] = normalizeStatus(ctrl.Status())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.engine.Store().GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req db.Settings
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.ApplySettings(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := s.engine.Store().GetSettings()
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleUpdateWebDAV(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.Store().SetWebDAVCredentials(req.Username, req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleChangeAdminPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewUsername     string `json:"new_username"`
		NewPassword     string `json:"new_password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	snap := s.engine.State().Snapshot()
	if !s.engine.State().VerifyAdmin(snap.AdminUser, req.CurrentPassword) {
		writeErr(w, http.StatusUnauthorized, "Mật khẩu hiện tại không đúng")
		return
	}
	newUser := strings.TrimSpace(req.NewUsername)
	if newUser == "" {
		newUser = snap.AdminUser
	}
	if err := s.engine.State().SetAdmin(newUser, req.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setSessionCookie(w, r, newUser)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	s.engine.Cache().Clear()
	runtime.GC()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cache": s.engine.Cache().Stats()})
}

func (s *Server) handleFlushPacks(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Packer().FlushNow(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "packer": s.engine.Packer().Status()})
}

func (s *Server) handleCompactPacks(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.Packer().CompactPacks(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleManualBackup(w http.ResponseWriter, r *http.Request) {
	doc, err := s.engine.BackupDBToTelegram(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"msg_id": doc.MsgID,
		"size":   doc.Size,
	})
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.engine.ListChannelBackups(r.Context())
	if err != nil {
		// Self-heal stale/truncated 64-bit AccessHash saved by older builds by refreshing from Telegram dialogs.
		snap := s.engine.State().Snapshot()
		if snap.ChannelID != 0 {
			if ch, ferr := s.tgMgr.FindDialogChannel(r.Context(), snap.ChannelID); ferr == nil && ch != nil && ch.AccessHash != 0 && ch.AccessHash != snap.ChannelAccessHash {
				title := snap.ChannelTitle
				if title == "" {
					title = ch.Title
				}
				_ = s.engine.State().SetChannel(ch.ID, ch.AccessHash, title)
				s.tgMgr.SetChannel(ch.ID, ch.AccessHash)
				backups, err = s.engine.ListChannelBackups(r.Context())
			}
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MsgID      int    `json:"msg_id"`
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.RestoreDBFromTelegram(r.Context(), req.MsgID, req.Passphrase); err != nil {
		if errors.Is(err, crypto.ErrDecryptFailed) {
			writeErr(w, http.StatusBadRequest, "Sai Encryption Key hoặc bản backup bị lỗi xác thực")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleScanOrphans(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.ScanOrphans(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleCleanOrphans(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MsgIDs []int `json:"msg_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	deleted, err := s.engine.CleanOrphans(r.Context(), req.MsgIDs)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) handleRcloneConfig(w http.ResponseWriter, r *http.Request) {
	settings, _ := s.engine.Store().GetSettings()
	user := settings.WebDAVUser
	if user == "" {
		user = "rclone"
	}

	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	extURL := fmt.Sprintf("%s://%s/dav/", scheme, r.Host)
	dockerURL := "http://telemount:8080/dav/"

	conf := fmt.Sprintf(`[telemount]
type = webdav
url = %s
vendor = other
user = %s
pass = <chạy: rclone obscure "MAT_KHAU_WEBDAV">
`, dockerURL, user)

	mountCmd := `rclone mount telemount: /mnt/telemount \
  --cache-dir /cache \
  --vfs-cache-mode writes \
  --vfs-cache-max-size 2G \
  --vfs-cache-max-age 5m \
  --dir-cache-time 30s \
  --allow-other`

	writeJSON(w, http.StatusOK, map[string]any{
		"internal_url": dockerURL,
		"external_url": extURL,
		"webdav_user":  user,
		"rclone_conf":  conf,
		"mount_cmd":    mountCmd,
	})
}
