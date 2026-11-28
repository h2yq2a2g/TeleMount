package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/tg"
)

type batchForwarder interface {
	ForwardCopyDocsBatch(ctx context.Context, srcChannelID, srcAccessHash int64, srcMsgIDs []int) (map[int]*tg.UploadedDoc, error)
}

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
	e.spawnBg(func(bgCtx context.Context) {
		ctx, cancel := context.WithTimeout(bgCtx, 2*time.Minute)
		defer cancel()
		if err := e.MirrorMsg(ctx, primaryMsgID, isDBBackup); err != nil {
			e.recordBgError("mirror_msg", err)
		}
	})
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
			// Guard against fast delete/overwrite race: if primary blob was already unreferenced, clean up replica immediately.
			if stillRef, rerr := e.store.IsReferencedMsgID(primaryMsgID); rerr == nil && !stillRef {
				_ = backend.DeleteMessages(ctx, []int{repDoc.MsgID})
				continue
			}
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
			pruneOldBackupDocs(ctx, backend, repDoc.MsgID)
		}
	}
	return firstErr
}

func pruneOldBackupDocs(ctx context.Context, backend tg.Backend, newestMsgID int) {
	docs, err := backend.ListChannelDocs(ctx, DBBackupPrefix, 30)
	if err != nil {
		slog.Warn("list backup channel db backups failed", "err", err)
		return
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].MsgID > docs[j].MsgID })
	var oldIDs []int
	kept := 1
	for _, d := range docs {
		if d.MsgID == newestMsgID {
			continue
		}
		if kept < DBBackupKeepCount {
			kept++
			continue
		}
		oldIDs = append(oldIDs, d.MsgID)
	}
	if len(oldIDs) > 0 {
		if err := backend.DeleteMessages(ctx, oldIDs); err != nil {
			slog.Warn("prune old backup channel db backups failed", "count", len(oldIDs), "err", err)
		}
	}
}

func isFatalMirrorErr(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, tg.ErrNotAuthorized) ||
		errors.Is(err, tg.ErrNotConnected)
}

// SyncAllReplicas retroactively server-side copies all unreplicated packs/parts (and fresh .tmdb backup)
// to targetAccountID (or all backup accounts if targetAccountID == "").
func (e *Engine) SyncAllReplicas(ctx context.Context, targetAccountID string) (*SyncReport, error) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	if e.tg.IsAuthorized() {
		if err := e.packer.FlushNow(ctx); err != nil {
			slog.Warn("flush pack before replica sync failed", "err", err)
		}
	}

	primarySnap := e.state.Snapshot()
	if primarySnap.ChannelID == 0 {
		return nil, ErrNotReady
	}

	rep := &SyncReport{
		ByAccount: make(map[string]int),
	}

	needDBBackup := false
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

		recordCopied := func(mid int, doc *tg.UploadedDoc) error {
			if stillRef, rerr := e.store.IsReferencedMsgID(mid); rerr == nil && !stillRef {
				_ = backend.DeleteMessages(ctx, []int{doc.MsgID})
				return nil
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
				return err
			}
			rep.SyncedBlobs++
			rep.ByAccount[acc.ID]++
			return nil
		}

		// Use batch forward (50 msgs/RPC) when supported, falling back per-message on partial errors so
		// one bad/missing message never blocks newer blobs from replicating.
		bf, hasBatch := backend.(batchForwarder)
		const batchSize = 50
		for i := 0; i < len(missing); {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			if hasBatch && len(missing)-i > 1 {
				end := i + batchSize
				if end > len(missing) {
					end = len(missing)
				}
				chunk := missing[i:end]
				copiedMap, berr := bf.ForwardCopyDocsBatch(ctx, acc.SourceChannelID, acc.SourceChannelAccessHash, chunk)
				if berr == nil && len(copiedMap) > 0 {
					var unmatched []int
					for _, mid := range chunk {
						if doc, ok := copiedMap[mid]; ok {
							if uerr := recordCopied(mid, doc); uerr != nil {
								rep.Errors = append(rep.Errors, uerr.Error())
							}
						} else {
							unmatched = append(unmatched, mid)
						}
					}
					for _, mid := range unmatched {
						doc, serr := backend.ForwardCopyDoc(ctx, acc.SourceChannelID, acc.SourceChannelAccessHash, mid)
						if serr != nil {
							rep.Errors = append(rep.Errors, fmt.Sprintf("%s msg#%d: %v", acc.ID, mid, serr))
							if isFatalMirrorErr(serr) {
								i = len(missing)
								break
							}
							continue
						}
						if uerr := recordCopied(mid, doc); uerr != nil {
							rep.Errors = append(rep.Errors, uerr.Error())
						}
					}
					i = end
					continue
				}
				if berr != nil && isFatalMirrorErr(berr) {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", acc.ID, berr))
					break
				}
			}

			mid := missing[i]
			i++
			doc, err := backend.ForwardCopyDoc(ctx, acc.SourceChannelID, acc.SourceChannelAccessHash, mid)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s msg#%d: %v", acc.ID, mid, err))
				if isFatalMirrorErr(err) {
					break
				}
				continue
			}
			if err := recordCopied(mid, doc); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
			}
		}

		// Ensure this backup channel also holds at least one .tmdb DB backup even if no new blobs were synced.
		if docs, derr := backend.ListChannelDocs(ctx, DBBackupPrefix, 1); derr == nil && len(docs) == 0 {
			needDBBackup = true
		}
	}

	// Push an updated .tmdb backup (which includes the new blob_replicas rows) so every backup channel has a self-contained recovery snapshot.
	if (rep.SyncedBlobs > 0 || needDBBackup) && e.tg.IsAuthorized() {
		if _, err := e.BackupDBToTelegram(ctx); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("db backup: %v", err))
		}
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
func (e *Engine) fetchChunkWithFailover(ctx context.Context, part db.FilePart, offset int64) ([]byte, error) {
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
