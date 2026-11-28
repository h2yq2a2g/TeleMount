package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"telemount/internal/crypto"
	"telemount/internal/tg"
)

func (s *Server) computeSetupStep() string {
	st := s.engine.State()
	if !st.HasAdmin() {
		return "admin"
	}
	if !s.tgMgr.IsAuthorized() {
		return "telegram"
	}
	if !st.HasChannel() {
		return "channel"
	}
	if !st.HasMasterKey() {
		return "key"
	}
	// Windows Desktop mounts directly via WinFsp — no WebDAV setup step needed.
	if s.desktop() != nil {
		return "ready"
	}
	settings, err := s.engine.Store().GetSettings()
	if err != nil || !settings.HasWebDAVPass {
		return "webdav"
	}
	return "ready"
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	hasAdmin := s.engine.State().HasAdmin()
	user, authed := s.isAuthenticated(r)
	desktopEnabled := s.desktop() != nil
	lang := s.engine.State().Language()

	// Before login on an already-initialized instance, expose only minimal login-routing flags.
	if hasAdmin && !authed {
		writeJSON(w, http.StatusOK, map[string]any{
			"has_admin":       true,
			"authenticated":   false,
			"desktop_enabled": desktopEnabled,
			"language":        lang,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"has_admin":       hasAdmin,
		"authenticated":   authed,
		"admin_user":      user,
		"setup_step":      s.computeSetupStep(),
		"ready":           s.engine.IsReady(),
		"desktop_enabled": desktopEnabled,
		"language":        lang,
	})
}

func (s *Server) handleSetLanguage(w http.ResponseWriter, r *http.Request) {
	// Allow setting language during initial setup (before admin exists) or when authenticated as admin.
	if s.engine.State().HasAdmin() {
		if _, ok := s.isAuthenticated(r); !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
	}
	var req struct {
		Language string `json:"language"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.State().SetLanguage(req.Language); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lang := s.engine.State().Language()
	if ctrl := s.desktop(); ctrl != nil {
		if obs, ok := ctrl.(LanguageObserver); ok {
			obs.OnLanguageChange(lang)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "language": lang})
}

func (s *Server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	if s.engine.State().HasAdmin() {
		writeErr(w, http.StatusForbidden, "admin account already initialized")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.State().SetAdmin(req.Username, req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setSessionCookie(w, r, strings.TrimSpace(req.Username))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.engine.State().HasAdmin() {
		writeErr(w, http.StatusForbidden, "setup admin first")
		return
	}
	ip := remoteClientIP(r)
	if s.loginFails.isBlocked(ip) {
		writeErr(w, http.StatusTooManyRequests, "Too many failed login attempts; please wait a few minutes")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !s.engine.State().VerifyAdmin(strings.TrimSpace(req.Username), req.Password) {
		s.loginFails.recordFailure(ip)
		writeErr(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}
	s.loginFails.clear(ip)
	s.setSessionCookie(w, r, strings.TrimSpace(req.Username))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	snap := s.engine.State().Snapshot()
	tgSnap := s.tgMgr.Tracker().Snapshot()
	settings, _ := s.engine.Store().GetSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_step":        s.computeSetupStep(),
		"admin_user":        snap.AdminUser,
		"tg_api_configured": snap.TGAPIID > 0 && snap.TGAPIHash != "",
		"tg_api_id":         snap.TGAPIID,
		"tg_connected":      tgSnap.Connected,
		"tg_state":          tgSnap.ConnectionState,
		"tg_authorized":     tgSnap.Authorized,
		"tg_user":           tgSnap.User,
		"tg_last_error":     tgSnap.LastError,
		"channel_id":        snap.ChannelID,
		"channel_title":     snap.ChannelTitle,
		"has_master_key":    s.engine.State().HasMasterKey(),
		"webdav_user":       settings.WebDAVUser,
		"has_webdav_pass":   settings.HasWebDAVPass,
		"setup_completed":   snap.SetupCompleted,
		"desktop_enabled":   s.desktop() != nil,
	})
}

func (s *Server) handleTGConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIID   int    `json:"api_id"`
		APIHash string `json:"api_hash"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.State().SetTelegramApp(req.APIID, req.APIHash); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.tgMgr.Start(s.baseContext(), req.APIID, req.APIHash); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	snap := s.engine.State().Snapshot()
	if snap.ChannelID != 0 {
		s.tgMgr.SetChannel(snap.ChannelID, snap.ChannelAccessHash)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"authorized": s.tgMgr.IsAuthorized(),
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleTGQRStart(w http.ResponseWriter, r *http.Request) {
	if err := s.tgMgr.StartQRLogin(s.baseContext()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Give gotd a brief moment to mint the first QR token so the immediate response often has the PNG ready.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := s.tgMgr.QRStatus()
		if st.QRDataURL != "" || st.Status != "pending" {
			writeJSON(w, http.StatusOK, st)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeJSON(w, http.StatusOK, s.tgMgr.QRStatus())
}

func (s *Server) handleTGQRStatus(w http.ResponseWriter, r *http.Request) {
	st := s.tgMgr.QRStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"qr":         st,
		"authorized": s.tgMgr.IsAuthorized(),
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleTGPhoneCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.tgMgr.SendPhoneCode(r.Context(), req.Phone); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTGPhoneVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := s.tgMgr.SignInCode(r.Context(), req.Code)
	if errors.Is(err, tg.ErrPasswordNeeded) {
		writeJSON(w, http.StatusOK, map[string]any{"need_password": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"authorized": true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleTGPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.tgMgr.SignInPassword(r.Context(), req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"authorized": true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleTGLogout(w http.ResponseWriter, r *http.Request) {
	_ = s.tgMgr.Logout(r.Context())
	_ = s.engine.State().ClearChannel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "setup_step": s.computeSetupStep()})
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.tgMgr.ListPrivateChannels(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ch, err := s.tgMgr.CreatePrivateChannel(r.Context(), req.Title)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.engine.State().SetChannel(ch.ID, ch.AccessHash, ch.Title); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.tgMgr.SetChannel(ch.ID, ch.AccessHash)
	writeJSON(w, http.StatusOK, map[string]any{
		"channel":    ch,
		"backups":    []tg.ChannelDoc{},
		"setup_step": s.computeSetupStep(),
	})
}

func parseFlexInt64(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	if s == "" || s == "null" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func (s *Server) handleSelectChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID         int64           `json:"id"`
		AccessHash json.RawMessage `json:"access_hash"`
		Title      string          `json:"title"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	accessHash, err := parseFlexInt64(req.AccessHash)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid channel access_hash")
		return
	}
	// Resolve authoritative 64-bit AccessHash from Telegram dialogs when connected to avoid any client precision loss.
	if ch, ferr := s.tgMgr.FindDialogChannel(r.Context(), req.ID); ferr == nil && ch != nil && ch.AccessHash != 0 {
		accessHash = ch.AccessHash
		if req.Title == "" {
			req.Title = ch.Title
		}
	}
	if err := s.engine.State().SetChannel(req.ID, accessHash, req.Title); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.tgMgr.SetChannel(req.ID, accessHash)
	// Probe the selected channel for any existing TeleMount encrypted DB backups so user can restore immediately.
	backups, _ := s.engine.ListChannelBackups(r.Context())
	if backups == nil {
		backups = []tg.ChannelDoc{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"backups":    backups,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleClearChannel(w http.ResponseWriter, r *http.Request) {
	if s.engine.State().HasMasterKey() {
		writeErr(w, http.StatusConflict, "cannot clear channel after encryption key is initialized")
		return
	}
	if err := s.engine.State().ClearChannel(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.tgMgr.SetChannel(0, 0)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleGenerateKey(w http.ResponseWriter, r *http.Request) {
	pass, err := crypto.GenerateRandomPassphrase()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"passphrase": pass})
}

// handleInitKey hashes any user-provided passphrase via Argon2id (+ random 16-byte salt) to derive the 32-byte AES-256 master key.
func (s *Server) handleInitKey(w http.ResponseWriter, r *http.Request) {
	if s.engine.State().HasMasterKey() {
		writeErr(w, http.StatusConflict, "encryption key already configured")
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Passphrase) < 8 {
		writeErr(w, http.StatusBadRequest, "Encryption key must be at least 8 characters")
		return
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	masterKey := crypto.DeriveMasterKey(req.Passphrase, salt)
	if err := s.engine.State().SetMasterKey(masterKey, salt); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.finishDesktopSetupIfEnabled()
	// Upload initial encrypted DB backup carrying the kdfSalt header so disaster recovery works from day one.
	_, backupErr := s.engine.BackupDBToTelegram(r.Context())
	warn := ""
	if backupErr != nil {
		warn = backupErr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"backup_warning": warn,
		"setup_step":     s.computeSetupStep(),
	})
}

func (s *Server) handleRestoreKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
		MsgID      int    `json:"msg_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.engine.RestoreDBFromTelegram(r.Context(), req.MsgID, req.Passphrase); err != nil {
		if errors.Is(err, crypto.ErrDecryptFailed) {
			writeErr(w, http.StatusBadRequest, "Invalid encryption key or corrupted backup")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.finishDesktopSetupIfEnabled()
	stats, _ := s.engine.Store().Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"stats":      stats,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) handleSetupWebDAV(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Skip     bool   `json:"skip"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Skip {
		if s.desktop() == nil {
			writeErr(w, http.StatusBadRequest, "webdav setup cannot be skipped when desktop mode is disabled")
			return
		}
		if err := s.engine.State().MarkSetupCompleted(); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := s.engine.Store().SetWebDAVCredentials(req.Username, req.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.engine.State().MarkSetupCompleted()
	}
	// Refresh DB backup so restored instances also keep WebDAV credentials/state.
	if s.engine.IsReady() {
		go func() {
			ctx, cancel := context.WithTimeout(s.baseContext(), 30*time.Second)
			defer cancel()
			_, _ = s.engine.BackupDBToTelegram(ctx)
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"setup_step": s.computeSetupStep(),
	})
}

func (s *Server) finishDesktopSetupIfEnabled() {
	ctrl := s.desktop()
	if ctrl == nil {
		return
	}
	_ = s.engine.State().MarkSetupCompleted()
	st := ctrl.Status()
	if st.AutoMount && st.WinFspInstalled && !st.Mounted {
		go func() { _ = ctrl.Mount() }()
	}
}
