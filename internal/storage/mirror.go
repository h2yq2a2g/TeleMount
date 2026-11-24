package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/tg"
)

type SyncReport struct {
	SyncedBlobs int            `json:"synced_blobs"`
	ByAccount   map[string]int `json:"by_account"`
	Errors      []string       `json:"errors,omitempty"`
}

// MirrorMsgAsync schedules non-blocking server-side replication of a newly uploaded primary message to all active backup accounts.
func (e *Engine) MirrorMsgAsync(primaryMsgID int, isDBBackup bool) {
	if primaryMsgID <= 0 || len(e.state.BackupAccounts()) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e.MirrorMsg(ctx, primaryMsgID, isDBBackup)
	}()
}

// MirrorMsg performs zero-bandwidth server-side copy (DropAuthor: true) of primaryMsgID into every configured backup account's private channel.
func (e *Engine) MirrorMsg(ctx context.Context, primaryMsgID int, isDBBackup bool) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	primarySnap := e.state.Snapshot()
	backups := primarySnap.BackupAccounts
	if len(backups) == 0 || primarySnap.ChannelID == 0 {
		return nil
	}

	var existing map[string]bool
	if !isDBBackup {
		if reps, err := e.store.GetBlobReplicas(primaryMsgID); err == nil && len(reps) > 0 {
			existing = make(map[string]bool, len(reps))
			for _, r := range reps {
				existing[r.AccountID] = true
			}
		}
	}

	var firstErr error
	for _, acc := range backups {
		if existing[acc.ID] {
			continue
		}
		if acc.OwnChannelID == 0 || acc.SourceChannelID != primarySnap.ChannelID || acc.SourceChannelAccessHash == 0 {
			continue
		}
		backend, ok := e.pool.GetBackend(acc.ID)
		if !ok || !backend.IsAuthorized() {
			continue
		}

		repDoc, err := backend.ForwardCopyDoc(ctx, acc.SourceChannelID, acc.SourceChannelAccessHash, primaryMsgID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("backup %s: %w", acc.ID, err)
			}
			continue
		}

		if !isDBBackup {
			if err := e.store.UpsertBlobReplica(db.BlobReplica{
				AccountID:    acc.ID,
				ChannelID:    acc.OwnChannelID,
				PrimaryMsgID: primaryMsgID,
				ReplicaMsgID: repDoc.MsgID,
				TGFileID:     repDoc.FileID,
				TGAccessHash: repDoc.AccessHash,
				TGFileRef:    repDoc.FileRef,
				TGDCID:       repDoc.DCID,
			}); err != nil && firstErr == nil {
				firstErr = err
			}
		} else {
			// Prune old .tmdb backups on the backup account's own channel so only the latest snapshots remain.
			if docs, err := backend.ListChannelDocs(ctx, DBBackupPrefix, 30); err == nil && len(docs) > DBBackupKeepCount {
				sort.Slice(docs, func(i, j int) bool { return docs[i].MsgID > docs[j].MsgID })
				var oldIDs []int
				for _, d := range docs[DBBackupKeepCount:] {
					oldIDs = append(oldIDs, d.MsgID)
				}
				_ = backend.DeleteMessages(ctx, oldIDs)
			}
		}
	}
	return firstErr
}

// SyncAllReplicas retroactively server-side copies all unreplicated packs/parts (and fresh .tmdb backup)
// to targetAccountID (or all backup accounts if targetAccountID == "").
func (e *Engine) SyncAllReplicas(ctx context.Context, targetAccountID string) (*SyncReport, error) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	if e.tg.IsAuthorized() {
		_ = e.packer.FlushNow(ctx)
	}

	primarySnap := e.state.Snapshot()
	if primarySnap.ChannelID == 0 {
		return nil, ErrNotReady
	}

	rep := &SyncReport{
		ByAccount: make(map[string]int),
	}

	for _, acc := range primarySnap.BackupAccounts {
		if targetAccountID != "" && acc.ID != targetAccountID {
			continue
		}
		if acc.OwnChannelID == 0 || acc.SourceChannelID != primarySnap.ChannelID || acc.SourceChannelAccessHash == 0 {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: channel chưa liên kết nguồn hiện tại", acc.ID))
			continue
		}
		backend, ok := e.pool.GetBackend(acc.ID)
		if !ok || !backend.IsAuthorized() {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: chưa kết nối / chưa đăng nhập", acc.ID))
			continue
		}

		// Clean pending deleted replicas on this backup channel first.
		if delIDs, err := e.store.PendingReplicaDeletes(acc.ID); err == nil && len(delIDs) > 0 {
			if err := backend.DeleteMessages(ctx, delIDs); err == nil {
				_ = e.store.ClearPendingReplicaDeletes(acc.ID, delIDs)
			}
		}

		missing, err := e.store.ListUnreplicatedMsgIDs(acc.ID)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", acc.ID, err))
			continue
		}
		for _, mid := range missing {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			doc, err := backend.ForwardCopyDoc(ctx, acc.SourceChannelID, acc.SourceChannelAccessHash, mid)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s msg#%d: %v", acc.ID, mid, err))
				break
			}
			if err := e.store.UpsertBlobReplica(db.BlobReplica{
				AccountID:    acc.ID,
				ChannelID:    acc.OwnChannelID,
				PrimaryMsgID: mid,
				ReplicaMsgID: doc.MsgID,
				TGFileID:     doc.FileID,
				TGAccessHash: doc.AccessHash,
				TGFileRef:    doc.FileRef,
				TGDCID:       doc.DCID,
			}); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				break
			}
			rep.SyncedBlobs++
			rep.ByAccount[acc.ID]++
		}
	}

	// Push an updated .tmdb backup (which includes the new blob_replicas rows) so every backup channel has a self-contained recovery snapshot.
	if rep.SyncedBlobs > 0 && e.tg.IsAuthorized() {
		_, _ = e.BackupDBToTelegram(ctx)
	}
	return rep, nil
}

func (e *Engine) FlushPendingReplicaDeletes(ctx context.Context) {
	for _, acc := range e.state.BackupAccounts() {
		backend, ok := e.pool.GetBackend(acc.ID)
		if !ok || !backend.IsAuthorized() {
			continue
		}
		delIDs, err := e.store.PendingReplicaDeletes(acc.ID)
		if err != nil || len(delIDs) == 0 {
			continue
		}
		if err := backend.DeleteMessages(ctx, delIDs); err == nil {
			_ = e.store.ClearPendingReplicaDeletes(acc.ID, delIDs)
		}
	}
}

// fetchChunkWithFailover tries the Primary account first, and transparently falls back to any healthy Backup Account replica
// if the Primary account is offline, banned, or returns an error.
func (e *Engine) fetchChunkWithFailover(ctx context.Context, part *db.FilePart, offset int64) ([]byte, error) {
	var primaryErr error
	if e.tg.IsAuthorized() {
		ref := tg.DocRef{
			MsgID:      part.TGMsgID,
			FileID:     part.TGFileID,
			AccessHash: part.TGAccessHash,
			FileRef:    part.TGFileRef,
			DCID:       part.TGDCID,
		}
		data, err := e.tg.FetchChunk(ctx, ref, offset, tg.TGChunkSize)
		if errors.Is(err, tg.ErrFileRefExpired) {
			if fresh, rerr := e.tg.RefreshFileRef(ctx, part.TGMsgID); rerr == nil {
				part.TGFileRef = fresh
				_ = e.store.UpdateMsgFileRef(part.TGMsgID, fresh)
				ref.FileRef = fresh
				data, err = e.tg.FetchChunk(ctx, ref, offset, tg.TGChunkSize)
			}
		}
		if err == nil {
			return data, nil
		}
		primaryErr = err
	} else {
		primaryErr = tg.ErrNotAuthorized
	}

	// Automatic failover to mirrored Backup Accounts!
	replicas, err := e.store.GetBlobReplicas(part.TGMsgID)
	if err != nil || len(replicas) == 0 {
		return nil, primaryErr
	}

	for _, rep := range replicas {
		backend, ok := e.pool.GetBackend(rep.AccountID)
		if !ok || !backend.IsAuthorized() {
			continue
		}
		ref := tg.DocRef{
			MsgID:      rep.ReplicaMsgID,
			FileID:     rep.TGFileID,
			AccessHash: rep.TGAccessHash,
			FileRef:    rep.TGFileRef,
			DCID:       rep.TGDCID,
		}
		data, ferr := backend.FetchChunk(ctx, ref, offset, tg.TGChunkSize)
		if errors.Is(ferr, tg.ErrFileRefExpired) {
			if fresh, rerr := backend.RefreshFileRef(ctx, rep.ReplicaMsgID); rerr == nil {
				_ = e.store.UpdateReplicaFileRef(rep.AccountID, rep.ReplicaMsgID, fresh)
				ref.FileRef = fresh
				data, ferr = backend.FetchChunk(ctx, ref, offset, tg.TGChunkSize)
			}
		}
		if ferr == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("primary failed (%v) and no backup replica succeeded", primaryErr)
}

func (e *Engine) hasAnyAuthorizedAccount() bool {
	if e.tg.IsAuthorized() {
		return true
	}
	for _, acc := range e.state.BackupAccounts() {
		if b, ok := e.pool.GetBackend(acc.ID); ok && b.IsAuthorized() {
			return true
		}
	}
	return false
}

// LinkBackupAccount joins the newly authorized backup manager to the primary channel via a single-use invite
// and creates a dedicated private Backup Channel owned by the backup account.
func LinkBackupAccount(ctx context.Context, st *state.Manager, primaryMgr *tg.Manager, backupMgr *tg.Manager, accID string, apiID int, apiHash string) (*state.BackupAccountConfig, error) {
	if !primaryMgr.IsAuthorized() {
		return nil, errors.New("tài khoản Telegram chính đang không kết nối")
	}
	if !backupMgr.IsAuthorized() {
		return nil, errors.New("tài khoản Backup chưa đăng nhập xong")
	}

	pUser := primaryMgr.Tracker().Snapshot().User
	bUser := backupMgr.Tracker().Snapshot().User
	if pUser != nil && bUser != nil && pUser.ID != 0 && pUser.ID == bUser.ID {
		return nil, errors.New("không thể dùng trùng chính tài khoản Telegram chính làm tài khoản Backup")
	}

	snap := st.Snapshot()
	if snap.ChannelID == 0 {
		return nil, errors.New("chưa chọn Private Channel trên tài khoản chính")
	}

	// 1. Export single-use invite from Primary Channel and join from Backup Account.
	inviteLink, err := primaryMgr.ExportOneTimeInvite(ctx)
	if err != nil {
		return nil, err
	}
	srcInfo, err := backupMgr.JoinChannelByInvite(ctx, inviteLink, snap.ChannelID)
	if err != nil {
		return nil, err
	}

	// 2. Create a separate Private Channel owned exclusively by the Backup Account (if not already created).
	existing, _ := st.GetBackupAccount(accID)
	ownID := existing.OwnChannelID
	ownHash := existing.OwnChannelAccessHash
	ownTitle := existing.OwnChannelTitle
	if ownID == 0 {
		title := fmt.Sprintf("TeleMount Backup (%s)", snap.ChannelTitle)
		ownCh, err := backupMgr.CreatePrivateChannel(ctx, title)
		if err != nil {
			return nil, fmt.Errorf("create backup private channel: %w", err)
		}
		ownID = ownCh.ID
		ownHash = ownCh.AccessHash
		ownTitle = ownCh.Title
	}
	backupMgr.SetChannel(ownID, ownHash)

	cfg := state.BackupAccountConfig{
		ID:                      accID,
		APIID:                   apiID,
		APIHash:                 apiHash,
		OwnChannelID:            ownID,
		OwnChannelAccessHash:    ownHash,
		OwnChannelTitle:         ownTitle,
		SourceChannelID:         srcInfo.ID,
		SourceChannelAccessHash: srcInfo.AccessHash,
		CreatedAt:               existing.CreatedAt,
	}
	if err := st.UpsertBackupAccount(cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Ensure sync unused import guard if refactored.
var _ = sync.Mutex{}
