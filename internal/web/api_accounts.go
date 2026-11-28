package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"telemount/internal/storage"
	"telemount/internal/tg"
)

func newAccountID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "acc-" + hex.EncodeToString(b)
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	snap := s.engine.State().Snapshot()
	pSnap := s.tgMgr.Tracker().Snapshot()
	totalBlobs, syncedByAcc, _ := s.engine.Store().ReplicaCountsByAccount()

	type backupView struct {
		ID              string       `json:"id"`
		Connected       bool         `json:"connected"`
		ConnectionState string       `json:"connection_state"`
		Authorized      bool         `json:"authorized"`
		User            *tg.UserInfo `json:"user,omitempty"`
		OwnChannelID    int64        `json:"own_channel_id"`
		OwnChannelTitle string       `json:"own_channel_title"`
		SyncedBlobs     int64        `json:"synced_blobs"`
		TotalBlobs      int64        `json:"total_blobs"`
		CreatedAt       int64        `json:"created_at"`
	}

	backups := make([]backupView, 0, len(snap.BackupAccounts))
	for _, cfg := range snap.BackupAccounts {
		bv := backupView{
			ID:              cfg.ID,
			ConnectionState: "disconnected",
			OwnChannelID:    cfg.OwnChannelID,
			OwnChannelTitle: cfg.OwnChannelTitle,
			SyncedBlobs:     syncedByAcc[cfg.ID],
			TotalBlobs:      totalBlobs,
			CreatedAt:       cfg.CreatedAt,
		}
		if bm, ok := s.engine.Pool().GetManager(cfg.ID); ok {
			ts := bm.Tracker().Snapshot()
			bv.Connected = ts.Connected
			bv.ConnectionState = ts.ConnectionState
			bv.Authorized = ts.Authorized
			bv.User = ts.User
		}
		backups = append(backups, bv)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"primary": map[string]any{
			"connected":        pSnap.Connected,
			"connection_state": pSnap.ConnectionState,
			"authorized":       pSnap.Authorized,
			"user":             pSnap.User,
			"channel_id":       snap.ChannelID,
			"channel_title":    snap.ChannelTitle,
			"total_blobs":      totalBlobs,
		},
		"backups": backups,
	})
}

func (s *Server) handleStartBackupAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		APIID   int    `json:"api_id"`
		APIHash string `json:"api_hash"`
	}
	_ = readJSON(r, &req)

	snap := s.engine.State().Snapshot()
	apiID := req.APIID
	apiHash := strings.TrimSpace(req.APIHash)
	if apiID <= 0 || apiHash == "" {
		apiID = snap.TGAPIID
		apiHash = snap.TGAPIHash
	}
	if apiID <= 0 || apiHash == "" {
		writeErr(w, http.StatusBadRequest, "Telegram API ID / API Hash not configured")
		return
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newAccountID()
	} else if !tg.IsValidAccountID(id) {
		writeErr(w, http.StatusBadRequest, "invalid backup account id")
		return
	}

	bm := s.engine.Pool().GetOrCreateManager(id)
	if bm == nil {
		writeErr(w, http.StatusBadRequest, "invalid backup account id")
		return
	}
	if err := bm.Start(s.baseContext(), apiID, apiHash); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"authorized": bm.IsAuthorized(),
	})
}

func (s *Server) handleBackupQRStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	if err := bm.StartQRLogin(s.baseContext()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := bm.QRStatus()
		if st.QRDataURL != "" || st.Status != "pending" {
			writeJSON(w, http.StatusOK, st)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeJSON(w, http.StatusOK, bm.QRStatus())
}

func (s *Server) handleBackupQRStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"qr":         bm.QRStatus(),
		"authorized": bm.IsAuthorized(),
	})
}

func (s *Server) handleBackupPhoneCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := bm.SendPhoneCode(r.Context(), req.Phone); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleBackupPhoneVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := bm.SignInCode(r.Context(), req.Code)
	if errors.Is(err, tg.ErrPasswordNeeded) {
		writeJSON(w, http.StatusOK, map[string]any{"need_password": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authorized": true})
}

func (s *Server) handleBackupPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := bm.SignInPassword(r.Context(), req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authorized": true})
}

// handleFinalizeBackupAccount automatically invites the newly authorized Backup Account to read Primary Channel,
// creates a dedicated private Backup Channel owned by the Backup Account, and starts background full sync.
func (s *Server) handleFinalizeBackupAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm, ok := s.engine.Pool().GetManager(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "backup session not found")
		return
	}
	snap := s.engine.State().Snapshot()
	cfg, err := storage.LinkBackupAccount(r.Context(), s.engine.State(), s.tgMgr, bm, id, snap.TGAPIID, snap.TGAPIHash)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Kick off retroactive zero-bandwidth sync of all existing blobs + fresh .tmdb DB backup!
	go func() {
		ctx, cancel := context.WithTimeout(s.baseContext(), 30*time.Minute)
		defer cancel()
		if _, err := s.engine.SyncAllReplicas(ctx, id); err != nil {
			slog.Warn("initial backup account sync failed", "account_id", id, "err", err)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"account": cfg,
	})
}

func (s *Server) handleSyncAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	_ = readJSON(r, &req)
	rep, err := s.engine.SyncAllReplicas(r.Context(), strings.TrimSpace(req.AccountID))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handlePromoteBackupAccount promotes a Backup Account (and its independent Private Channel) to become the new Primary Account.
func (s *Server) handlePromoteBackupAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !tg.IsValidAccountID(id) {
		writeErr(w, http.StatusBadRequest, "invalid backup account id")
		return
	}
	if _, ok := s.engine.State().GetBackupAccount(id); !ok {
		writeErr(w, http.StatusNotFound, "backup account not found")
		return
	}

	var req struct {
		Force bool `json:"force"`
	}
	_ = readJSON(r, &req)
	force := req.Force || r.URL.Query().Get("force") == "true"

	if !force {
		unrep, err := s.engine.Store().CountUnreplicatedMsgIDs(id)
		if err != nil {
			writeInternalErr(w, "count_unreplicated", err)
			return
		}
		if unrep > 0 {
			writeErr(w, http.StatusConflict, fmt.Sprintf("backup account %s is missing %d unreplicated blob(s); run Sync first or pass force=true", id, unrep))
			return
		}
	}

	// 1. Remap SQLite file_parts, packs, and remaining replica mappings to this backup account's message IDs.
	remapped, err := s.engine.Store().PromoteAccountReplicasToPrimary(id)
	if err != nil {
		writeInternalErr(w, "promote_remap", err)
		return
	}

	// 2. Stop both live sessions so tg_session_<id>.json can be swapped cleanly into tg_session.json.
	s.tgMgr.Stop()
	s.engine.Pool().Remove(r.Context(), id, false)

	promoted, err := s.engine.State().PromoteBackupState(id)
	if err != nil {
		writeInternalErr(w, "promote_state", err)
		return
	}

	// 3. Start the new Primary session on s.tgMgr and clear chunk cache.
	s.engine.Cache().Clear()
	snap := s.engine.State().Snapshot()
	s.tgMgr.SetChannel(promoted.OwnChannelID, promoted.OwnChannelAccessHash)
	if err := s.tgMgr.Start(s.baseContext(), snap.TGAPIID, snap.TGAPIHash); err != nil {
		writeErr(w, http.StatusBadGateway, "Promoted backup account but reconnect failed: "+err.Error())
		return
	}

	// 4. Re-link any other remaining Backup Accounts (#2, #3...) to read from the newly promoted Primary Channel!
	go func() {
		ctx, cancel := context.WithTimeout(s.baseContext(), 2*time.Minute)
		defer cancel()
		for _, other := range s.engine.State().BackupAccounts() {
			if bm, ok := s.engine.Pool().GetManager(other.ID); ok && bm.IsAuthorized() {
				if _, err := storage.LinkBackupAccount(ctx, s.engine.State(), s.tgMgr, bm, other.ID, other.APIID, other.APIHash); err != nil {
					slog.Warn("re-link remaining backup account failed", "account_id", other.ID, "err", err)
				}
			}
		}
		if _, err := s.engine.BackupDBToTelegram(ctx); err != nil {
			slog.Warn("post-promote db backup failed", "err", err)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"remapped_parts": remapped,
		"new_channel":    promoted.OwnChannelTitle,
	})
}

func (s *Server) handleDeleteBackupAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !tg.IsValidAccountID(id) {
		writeErr(w, http.StatusBadRequest, "invalid backup account id")
		return
	}
	s.engine.Pool().Remove(r.Context(), id, true)
	_ = s.engine.State().RemoveBackupAccount(id)
	_ = s.engine.Store().DeleteAccountReplicas(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
