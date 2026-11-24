package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"telemount/internal/dav"
	"telemount/internal/storage"
	"telemount/internal/tg"
)

//go:embed static/*
var staticFS embed.FS

const sessionCookieName = "tm_session"

type Server struct {
	engine          *storage.Engine
	tgMgr           *tg.Manager
	dav             *dav.Handler
	startedAt       time.Time
	mux             *http.ServeMux
	desktopMu       sync.RWMutex
	desktopCtrl     DesktopController
	desktopIPCToken string
}

func NewServer(engine *storage.Engine, tgMgr *tg.Manager) *Server {
	s := &Server{
		engine:    engine,
		tgMgr:     tgMgr,
		dav:       dav.NewHandler("/dav", engine),
		startedAt: time.Now(),
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Route WebDAV traffic cleanly before standard mux matching (server/Portainer mode only).
	if r.URL.Path == "/dav" || strings.HasPrefix(r.URL.Path, "/dav/") {
		if s.desktop() != nil {
			http.NotFound(w, r)
			return
		}
		s.dav.ServeHTTP(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// Public auth & bootstrap endpoints
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("PUT /api/language", s.handleSetLanguage)
	s.mux.HandleFunc("POST /api/setup/admin", s.handleSetupAdmin)
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// Setup & Telegram management (authenticated admin)
	s.mux.HandleFunc("GET /api/setup/state", s.requireAdmin(s.handleSetupState))
	s.mux.HandleFunc("POST /api/setup/telegram/connect", s.requireAdmin(s.handleTGConnect))
	s.mux.HandleFunc("POST /api/setup/telegram/qr/start", s.requireAdmin(s.handleTGQRStart))
	s.mux.HandleFunc("GET /api/setup/telegram/qr/status", s.requireAdmin(s.handleTGQRStatus))
	s.mux.HandleFunc("POST /api/setup/telegram/phone/code", s.requireAdmin(s.handleTGPhoneCode))
	s.mux.HandleFunc("POST /api/setup/telegram/phone/verify", s.requireAdmin(s.handleTGPhoneVerify))
	s.mux.HandleFunc("POST /api/setup/telegram/password", s.requireAdmin(s.handleTGPassword))
	s.mux.HandleFunc("POST /api/setup/telegram/logout", s.requireAdmin(s.handleTGLogout))
	s.mux.HandleFunc("GET /api/setup/channels", s.requireAdmin(s.handleListChannels))
	s.mux.HandleFunc("POST /api/setup/channels/create", s.requireAdmin(s.handleCreateChannel))
	s.mux.HandleFunc("POST /api/setup/channels/select", s.requireAdmin(s.handleSelectChannel))
	s.mux.HandleFunc("POST /api/setup/channels/clear", s.requireAdmin(s.handleClearChannel))
	s.mux.HandleFunc("GET /api/setup/key/generate", s.requireAdmin(s.handleGenerateKey))
	s.mux.HandleFunc("POST /api/setup/key/init", s.requireAdmin(s.handleInitKey))
	s.mux.HandleFunc("POST /api/setup/key/restore", s.requireAdmin(s.handleRestoreKey))
	s.mux.HandleFunc("POST /api/setup/webdav", s.requireAdmin(s.handleSetupWebDAV))

	// Dashboard, Settings & Ops
	s.mux.HandleFunc("GET /api/dashboard", s.requireAdmin(s.handleDashboard))
	s.mux.HandleFunc("GET /api/settings", s.requireAdmin(s.handleGetSettings))
	s.mux.HandleFunc("PUT /api/settings", s.requireAdmin(s.handleSaveSettings))
	s.mux.HandleFunc("POST /api/settings/webdav", s.requireAdmin(s.handleUpdateWebDAV))
	s.mux.HandleFunc("POST /api/settings/admin-password", s.requireAdmin(s.handleChangeAdminPassword))
	s.mux.HandleFunc("POST /api/ops/cache/clear", s.requireAdmin(s.handleClearCache))
	s.mux.HandleFunc("POST /api/ops/packs/flush", s.requireReady(s.handleFlushPacks))
	s.mux.HandleFunc("POST /api/ops/packs/compact", s.requireReady(s.handleCompactPacks))
	s.mux.HandleFunc("POST /api/ops/backup", s.requireReady(s.handleManualBackup))
	s.mux.HandleFunc("GET /api/ops/backups", s.requireAdmin(s.handleListBackups))
	s.mux.HandleFunc("POST /api/ops/restore", s.requireAdmin(s.handleRestoreBackup))
	s.mux.HandleFunc("GET /api/ops/orphans", s.requireReady(s.handleScanOrphans))
	s.mux.HandleFunc("POST /api/ops/orphans/clean", s.requireReady(s.handleCleanOrphans))
	s.mux.HandleFunc("GET /api/ops/rclone-config", s.requireAdmin(s.handleRcloneConfig))

	// Desktop Virtual Drive & Disk Cache Controls (admin-protected)
	s.mux.HandleFunc("GET /api/desktop/status", s.requireAdmin(s.handleDesktopStatus))
	s.mux.HandleFunc("PUT /api/desktop/settings", s.requireAdmin(s.handleDesktopSettings))
	s.mux.HandleFunc("POST /api/desktop/mount", s.requireAdmin(s.handleDesktopMount))
	s.mux.HandleFunc("POST /api/desktop/unmount", s.requireAdmin(s.handleDesktopUnmount))
	s.mux.HandleFunc("POST /api/desktop/open", s.requireAdmin(s.handleDesktopOpen))
	s.mux.HandleFunc("POST /api/desktop/cache/clear", s.requireAdmin(s.handleDesktopCacheClear))
	s.mux.HandleFunc("POST /api/desktop/queue/flush", s.requireAdmin(s.handleDesktopQueueFlush))
	s.mux.HandleFunc("POST /api/desktop/update/check", s.requireAdmin(s.handleDesktopUpdateCheck))
	s.mux.HandleFunc("POST /api/desktop/update/apply", s.requireAdmin(s.handleDesktopUpdateApply))
	s.mux.HandleFunc("POST /api/desktop/shell-action", s.handleDesktopShellAction)

	// Multi-account Telegram Backup Mirroring
	s.mux.HandleFunc("GET /api/accounts", s.requireAdmin(s.handleListAccounts))
	s.mux.HandleFunc("POST /api/accounts/backup/start", s.requireAdmin(s.handleStartBackupAccount))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/qr/start", s.requireAdmin(s.handleBackupQRStart))
	s.mux.HandleFunc("GET /api/accounts/backup/{id}/qr/status", s.requireAdmin(s.handleBackupQRStatus))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/phone/code", s.requireAdmin(s.handleBackupPhoneCode))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/phone/verify", s.requireAdmin(s.handleBackupPhoneVerify))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/password", s.requireAdmin(s.handleBackupPassword))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/finalize", s.requireAdmin(s.handleFinalizeBackupAccount))
	s.mux.HandleFunc("POST /api/accounts/sync", s.requireAdmin(s.handleSyncAccounts))
	s.mux.HandleFunc("POST /api/accounts/backup/{id}/promote", s.requireAdmin(s.handlePromoteBackupAccount))
	s.mux.HandleFunc("DELETE /api/accounts/backup/{id}", s.requireAdmin(s.handleDeleteBackupAccount))

	// Embedded Web UI static files (disable browser caching so upgrades immediately show updated UI)
	sub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(sub))
	s.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		fileServer.ServeHTTP(w, r)
	}))
}

func (s *Server) isAuthenticated(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return s.engine.State().VerifySession(c.Value)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, username string) {
	token := s.engine.State().SignSession(username)
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.engine.State().HasAdmin() {
			writeErr(w, http.StatusForbidden, "initial admin setup required")
			return
		}
		if _, ok := s.isAuthenticated(r); !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireReady(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if !s.engine.IsReady() {
			writeErr(w, http.StatusServiceUnavailable, "TeleMount setup is incomplete or Telegram is disconnected")
			return
		}
		next(w, r)
	})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
