package diskcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"telemount/internal/db"
	"telemount/internal/storage"
)

type QueueOpType string

const (
	OpWrite   QueueOpType = "write"
	OpMkdir   QueueOpType = "mkdir"
	OpRename  QueueOpType = "rename"
	OpDelete  QueueOpType = "delete"
	OpUtimens QueueOpType = "utimens"
)

type QueueStatus struct {
	PendingCount int
	PendingBytes int64
	Syncing      bool
	LastError    string
	LastSyncedAt int64
}

type RemoteNode struct {
	ID    int64
	Path  string
	IsDir bool
	Size  int64
	Mtime int64
	Ctime int64
	ETag  string
}

type Backend interface {
	IsOnline() bool
	Stat(path string) (*RemoteNode, bool, error)
	ListDir(dirPath string) ([]RemoteNode, error)
	OpenReader(ctx context.Context, path string) (io.ReadCloser, int64, string, error)
	WriteFile(ctx context.Context, path string, r io.Reader, size int64, mtime int64) (*RemoteNode, error)
	Mkdir(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	Delete(ctx context.Context, path string, isDir bool) error
	SetMtime(ctx context.Context, path string, mtime int64) error
	FlushPending(ctx context.Context) error
}

type engineBackend struct {
	store  *db.Store
	engine *storage.Engine
}

func (e *engineBackend) IsOnline() bool {
	return e.engine != nil && e.engine.IsReady()
}

func (e *engineBackend) resolveNode(p string) (*db.Node, string, error) {
	if e.store == nil {
		return nil, "", db.ErrNotFound
	}
	clean := NormalizePath(p)
	n, err := e.store.StatByPath(clean)
	if err == nil {
		return n, clean, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return nil, "", err
	}
	if clean == "/" {
		root, rerr := e.store.GetNode(db.RootID)
		return root, "/", rerr
	}
	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	curID := db.RootID
	var cur *db.Node
	actual := make([]string, 0, len(segs))
	for _, seg := range segs {
		children, cerr := e.store.ListChildren(curID)
		if cerr != nil {
			return nil, "", cerr
		}
		var matched *db.Node
		for i := range children {
			if children[i].Name == seg {
				matched = &children[i]
				break
			}
			if matched == nil && strings.EqualFold(children[i].Name, seg) {
				matched = &children[i]
			}
		}
		if matched == nil {
			return nil, "", db.ErrNotFound
		}
		actual = append(actual, matched.Name)
		cur = matched
		curID = matched.ID
	}
	return cur, "/" + strings.Join(actual, "/"), nil
}

func (e *engineBackend) Stat(p string) (*RemoteNode, bool, error) {
	if e.store == nil {
		return nil, false, nil
	}
	n, actualPath, err := e.resolveNode(p)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &RemoteNode{
		ID:    n.ID,
		Path:  actualPath,
		IsDir: n.IsDir,
		Size:  n.Size,
		Mtime: n.UpdatedAt,
		Ctime: n.CreatedAt,
		ETag:  n.ETag,
	}, true, nil
}

func (e *engineBackend) ListDir(dirPath string) ([]RemoteNode, error) {
	if e.store == nil {
		return nil, nil
	}
	dir, actualDir, err := e.resolveNode(dirPath)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if !dir.IsDir {
		return nil, os.ErrInvalid
	}
	children, err := e.store.ListChildren(dir.ID)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteNode, 0, len(children))
	for _, c := range children {
		childPath := path.Join(actualDir, c.Name)
		if actualDir == "/" {
			childPath = "/" + c.Name
		}
		out = append(out, RemoteNode{
			ID:    c.ID,
			Path:  childPath,
			IsDir: c.IsDir,
			Size:  c.Size,
			Mtime: c.UpdatedAt,
			Ctime: c.CreatedAt,
			ETag:  c.ETag,
		})
	}
	return out, nil
}

func (e *engineBackend) OpenReader(ctx context.Context, p string) (io.ReadCloser, int64, string, error) {
	if !e.IsOnline() {
		return nil, 0, "", ErrOfflineUncached
	}
	n, _, err := e.resolveNode(p)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, 0, "", os.ErrNotExist
		}
		return nil, 0, "", err
	}
	if n.IsDir {
		return nil, 0, "", os.ErrInvalid
	}
	fr, err := e.engine.OpenFile(ctx, n)
	if err != nil {
		return nil, 0, "", err
	}
	return fr, n.Size, n.ETag, nil
}

func (e *engineBackend) WriteFile(ctx context.Context, p string, r io.Reader, size int64, _ int64) (*RemoteNode, error) {
	if !e.IsOnline() {
		return nil, storage.ErrNotReady
	}
	clean := NormalizePath(p)
	if clean == "/" {
		return nil, os.ErrInvalid
	}
	dirPath, baseName := path.Split(clean)
	parent, err := e.store.MkdirAll(dirPath)
	if err != nil {
		return nil, err
	}
	node, err := e.engine.UploadFile(ctx, parent.ID, baseName, "", r, size)
	if err != nil {
		return nil, err
	}
	return &RemoteNode{
		ID:    node.ID,
		Path:  clean,
		IsDir: node.IsDir,
		Size:  node.Size,
		Mtime: node.UpdatedAt,
		Ctime: node.CreatedAt,
		ETag:  node.ETag,
	}, nil
}

func (e *engineBackend) Mkdir(_ context.Context, p string) error {
	if !e.IsOnline() {
		return storage.ErrNotReady
	}
	clean := NormalizePath(p)
	if clean == "/" {
		return nil
	}
	_, err := e.store.MkdirAll(clean)
	return err
}

func (e *engineBackend) Rename(ctx context.Context, oldPath, newPath string) error {
	if !e.IsOnline() {
		return storage.ErrNotReady
	}
	oldNode, _, err := e.resolveNode(oldPath)
	if err != nil {
		return err
	}
	cleanNew := NormalizePath(newPath)
	if cleanNew == "/" {
		return os.ErrInvalid
	}
	newParentPath, newBase := path.Split(cleanNew)
	newParent, err := e.store.MkdirAll(newParentPath)
	if err != nil {
		return err
	}
	return e.engine.RenameOrMove(ctx, oldNode.ID, newParent.ID, newBase)
}

func (e *engineBackend) Delete(ctx context.Context, p string, _ bool) error {
	if !e.IsOnline() {
		return storage.ErrNotReady
	}
	node, _, err := e.resolveNode(p)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return err
	}
	return e.engine.DeletePermanent(ctx, node.ID)
}

func (e *engineBackend) SetMtime(_ context.Context, p string, _ int64) error {
	if !e.IsOnline() {
		return storage.ErrNotReady
	}
	_, _, err := e.resolveNode(p)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	return err
}

func (e *engineBackend) FlushPending(ctx context.Context) error {
	if !e.IsOnline() {
		return storage.ErrNotReady
	}
	return e.engine.Packer().FlushNow(ctx)
}

func validateBaseName(normPath string) (string, error) {
	if normPath == "/" {
		return "", os.ErrInvalid
	}
	_, base := path.Split(normPath)
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, "/\\\x00") {
		return "", os.ErrInvalid
	}
	return base, nil
}

func subtreeBounds(lowerPath string) (prefixStart, prefixEnd string) {
	return lowerPath + "/", lowerPath + "0"
}

func isSubtreeMatch(candidateLower, rootLower string) bool {
	return candidateLower == rootLower || strings.HasPrefix(candidateLower, rootLower+"/")
}

// StageOpenForWrite materializes an existing cached/remote file into a writable staging file
// (or creates an empty staging file when truncate=true or creating a new file).
func (m *Manager) StageOpenForWrite(p string, truncate bool) (string, error) {
	normPath := NormalizePath(p)
	if _, err := validateBaseName(normPath); err != nil {
		return "", err
	}

	m.stateMu.Lock()
	parentPath := parentOfNormalized(normPath)
	parentOv, parentFound, err := m.statLocked(parentPath)
	if err != nil {
		m.stateMu.Unlock()
		return "", err
	}
	if !parentFound {
		m.stateMu.Unlock()
		return "", os.ErrNotExist
	}
	if !parentOv.IsDir {
		m.stateMu.Unlock()
		return "", os.ErrInvalid
	}
	ov, found, err := m.statLocked(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return "", err
	}
	if found && ov.IsDir {
		m.stateMu.Unlock()
		return "", os.ErrInvalid
	}

	var localBlobKey string
	var localSize int64
	if !truncate && found && ov.Size > 0 {
		pathLower := strings.ToLower(normPath)
		if ov.Dirty {
			if row, ok, _ := m.getOverlayRow(pathLower); ok && row.BlobKey != "" && m.blobValid(row.BlobKey, ov.Size) {
				localBlobKey = row.BlobKey
				localSize = ov.Size
			} else if ce, ok, _ := m.getCacheEntry(pathLower); ok && ce.BlobKey != "" && m.blobValid(ce.BlobKey, ov.Size) {
				localBlobKey = ce.BlobKey
				localSize = ov.Size
			}
		} else if ce, ok, _ := m.getCacheEntry(pathLower); ok && !ce.Dirty && m.blobValid(ce.BlobKey, ce.Size) {
			stale := false
			if m.backend != nil {
				if rn, exists, serr := m.backend.Stat(normPath); serr == nil {
					if !exists || rn.IsDir || ce.RemoteETag != rn.ETag || ce.Size != rn.Size || (ce.RemoteNodeID != 0 && rn.ID != 0 && ce.RemoteNodeID != rn.ID) {
						stale = true
					}
				}
			}
			if !stale {
				localBlobKey = ce.BlobKey
				localSize = ce.Size
			}
		}
	}
	m.stateMu.Unlock()

	if err := ensureCacheDirs(m.cacheDir); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(m.stagingDir(), "wr-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if truncate || !found || ov.Size == 0 {
		if err := tmp.Close(); err != nil {
			return "", err
		}
		success = true
		return tmpPath, nil
	}

	if localBlobKey != "" && localSize > 0 {
		m.acquireBlobReader(localBlobKey)
		src, oerr := os.Open(m.blobPath(localBlobKey))
		if oerr == nil {
			buf := make([]byte, 256*1024)
			_, cerr := io.CopyBuffer(tmp, src, buf)
			_ = src.Close()
			m.releaseBlobReader(localBlobKey)
			if cerr != nil {
				return "", cerr
			}
			if err := tmp.Sync(); err != nil {
				return "", err
			}
			if err := tmp.Close(); err != nil {
				return "", err
			}
			success = true
			return tmpPath, nil
		}
		m.releaseBlobReader(localBlobKey)
	}

	// File exists remotely with size > 0 and is not cached locally.
	if m.backend == nil || !m.backend.IsOnline() {
		return "", ErrOfflineUncached
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	rc, _, _, err := m.backend.OpenReader(context.Background(), normPath)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(out, rc, buf); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	success = true
	return tmpPath, nil
}

// CommitFile durably records a staged file write in the offline queue, cache, and overlay,
// and attempts an immediate flush when online.
func (m *Manager) CommitFile(ctx context.Context, p string, stagedPath string, size int64, mtime int64) error {
	normPath := NormalizePath(p)
	baseName, err := validateBaseName(normPath)
	if err != nil {
		return err
	}

	if size < 0 {
		if stagedPath == "" {
			return os.ErrInvalid
		}
		fi, serr := os.Stat(stagedPath)
		if serr != nil {
			return serr
		}
		size = fi.Size()
	}
	if mtime <= 0 {
		mtime = time.Now().Unix()
	}

	// Pre-validate parent & target before moving staged files.
	m.stateMu.Lock()
	parentPath := parentOfNormalized(normPath)
	parentOv, parentFound, err := m.statLocked(parentPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if !parentFound {
		m.stateMu.Unlock()
		return os.ErrNotExist
	}
	if !parentOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}
	existingOv, existingFound, err := m.statLocked(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if existingFound && existingOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}
	m.stateMu.Unlock()

	var payloadFile, blobKey string
	if size > 0 {
		if stagedPath == "" {
			return os.ErrInvalid
		}
		payloadFile = fmt.Sprintf("op-%s.dat", newHexID())
		qDest := m.queuePayloadPath(payloadFile)
		if err := renameWithRetry(stagedPath, qDest); err != nil {
			return err
		}
		blobKey = newHexID()
		if err := copyFileAtomic(qDest, m.blobPath(blobKey)); err != nil {
			removeWithRetry(qDest)
			return err
		}
	} else if stagedPath != "" {
		removeWithRetry(stagedPath)
	}

	cleanupNewFiles := true
	defer func() {
		if cleanupNewFiles {
			if payloadFile != "" {
				removeWithRetry(m.queuePayloadPath(payloadFile))
			}
			if blobKey != "" {
				removeWithRetry(m.blobPath(blobKey))
			}
		}
	}()

	m.stateMu.Lock()
	// Re-validate under stateMu before committing SQLite tx.
	parentOv, parentFound, err = m.statLocked(parentPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if !parentFound || !parentOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}
	existingOv, existingFound, err = m.statLocked(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if existingFound && existingOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}
	ctime := mtime
	if existingFound && existingOv.Ctime > 0 {
		ctime = existingOv.Ctime
	}

	pathLower := strings.ToLower(normPath)
	parentLower := strings.ToLower(parentPath)
	nowNS := time.Now().UnixNano()
	accessNS := m.nextAccessNS()
	pinnedInt := boolToInt(m.isPinnedLocked(normPath))

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO write_queue (op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns)
		 VALUES (?, ?, '', ?, ?, ?, 0, 'pending', ?)`,
		string(OpWrite), normPath, payloadFile, size, mtime, nowNS,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	opID, err := res.LastInsertId()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}

	var oldCacheBlob, oldOverlayBlob string
	_ = tx.QueryRow(`SELECT blob_key FROM cache_entries WHERE path_lower = ?`, pathLower).Scan(&oldCacheBlob)
	_ = tx.QueryRow(`SELECT blob_key FROM overlay_nodes WHERE path_lower = ?`, pathLower).Scan(&oldOverlayBlob)

	if _, err := tx.Exec(`
		INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, 0, 1, ?, ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET
			path=excluded.path, parent_lower=excluded.parent_lower, name=excluded.name,
			is_dir=0, size=excluded.size, mtime=excluded.mtime, ctime=excluded.ctime,
			deleted=0, dirty=1, blob_key=excluded.blob_key, last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
	`, pathLower, normPath, parentLower, baseName, size, mtime, ctime, blobKey, opID, nowNS); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO cache_entries (path_lower, path, blob_key, size, mtime, last_accessed_ns, dirty, pinned, remote_node_id, remote_etag, last_op_id, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, 0, '', ?, ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET
			path=excluded.path, blob_key=excluded.blob_key, size=excluded.size, mtime=excluded.mtime,
			last_accessed_ns=excluded.last_accessed_ns, dirty=1, pinned=excluded.pinned,
			remote_node_id=0, remote_etag='', last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
	`, pathLower, normPath, blobKey, size, mtime, accessNS, pinnedInt, opID, nowNS, nowNS); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO node_meta (path_lower, path, mtime, etag)
		VALUES (?, ?, ?, '')
		ON CONFLICT(path_lower) DO UPDATE SET path=excluded.path, mtime=excluded.mtime, etag=''
	`, pathLower, normPath, mtime); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	cleanupNewFiles = false
	m.stateMu.Unlock()

	m.scheduleBlobDeletes([]string{oldCacheBlob, oldOverlayBlob})
	m.notifyChange(normPath)

	if m.backend != nil && m.backend.IsOnline() {
		_ = m.FlushQueue(ctx)
	}
	return nil
}

// QueueMkdir durably records a directory creation in the offline queue and overlay.
func (m *Manager) QueueMkdir(ctx context.Context, p string) error {
	normPath := NormalizePath(p)
	baseName, err := validateBaseName(normPath)
	if err != nil {
		return err
	}

	m.stateMu.Lock()
	parentPath := parentOfNormalized(normPath)
	parentOv, parentFound, err := m.statLocked(parentPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if !parentFound {
		m.stateMu.Unlock()
		return os.ErrNotExist
	}
	if !parentOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}
	if _, exists, err := m.statLocked(normPath); err != nil {
		m.stateMu.Unlock()
		return err
	} else if exists {
		m.stateMu.Unlock()
		return os.ErrExist
	}

	now := time.Now().Unix()
	nowNS := time.Now().UnixNano()
	pathLower := strings.ToLower(normPath)
	parentLower := strings.ToLower(parentPath)

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO write_queue (op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns)
		 VALUES (?, ?, '', '', 0, ?, 1, 'pending', ?)`,
		string(OpMkdir), normPath, now, nowNS,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	opID, err := res.LastInsertId()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
		VALUES (?, ?, ?, ?, 1, 0, ?, ?, 0, 1, '', ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET
			path=excluded.path, parent_lower=excluded.parent_lower, name=excluded.name,
			is_dir=1, size=0, mtime=excluded.mtime, ctime=excluded.ctime,
			deleted=0, dirty=1, blob_key='', last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
	`, pathLower, normPath, parentLower, baseName, now, now, opID, nowNS); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	m.stateMu.Unlock()
	m.notifyChange(normPath)

	if m.backend != nil && m.backend.IsOnline() {
		_ = m.FlushQueue(ctx)
	}
	return nil
}

// collectLiveSubtreeLocked returns the node at rootPath plus all live descendants in the merged view.
func (m *Manager) collectLiveSubtreeLocked(rootPath string) ([]NodeOverlay, error) {
	ov, found, err := m.statLocked(rootPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	out := []NodeOverlay{*ov}
	if !ov.IsDir {
		return out, nil
	}
	queue := []string{ov.Path}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		children, err := m.listDirLocked(dir)
		if err != nil {
			return nil, err
		}
		for _, ch := range children {
			out = append(out, ch)
			if ch.IsDir {
				queue = append(queue, ch.Path)
			}
		}
	}
	return out, nil
}

// QueueRename durably records a file or directory rename/move in the offline queue, overlay, and cache.
func (m *Manager) QueueRename(ctx context.Context, oldPath, newPath string) error {
	normOld := NormalizePath(oldPath)
	normNew := NormalizePath(newPath)
	if _, err := validateBaseName(normOld); err != nil {
		return err
	}
	if _, err := validateBaseName(normNew); err != nil {
		return err
	}
	oldLower := strings.ToLower(normOld)
	newLower := strings.ToLower(normNew)
	if normOld == normNew {
		return nil
	}
	if strings.HasPrefix(newLower, oldLower+"/") {
		return db.ErrInvalidMove
	}

	m.stateMu.Lock()
	newParent := parentOfNormalized(normNew)
	newParentOv, newParentFound, err := m.statLocked(newParent)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if !newParentFound {
		m.stateMu.Unlock()
		return os.ErrNotExist
	}
	if !newParentOv.IsDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}

	subtree, err := m.collectLiveSubtreeLocked(normOld)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	srcRoot := subtree[0]

	var dstSubtree []NodeOverlay
	if oldLower != newLower {
		dstOv, dstFound, err := m.statLocked(normNew)
		if err != nil {
			m.stateMu.Unlock()
			return err
		}
		if dstFound {
			if dstOv.IsDir != srcRoot.IsDir {
				m.stateMu.Unlock()
				return os.ErrInvalid
			}
			if dstOv.IsDir {
				children, cerr := m.listDirLocked(normNew)
				if cerr != nil {
					m.stateMu.Unlock()
					return cerr
				}
				if len(children) > 0 {
					m.stateMu.Unlock()
					return os.ErrInvalid
				}
			}
			dstSubtree, _ = m.collectLiveSubtreeLocked(normNew)
		}
	}

	nowNS := time.Now().UnixNano()
	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO write_queue (op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns)
		 VALUES (?, ?, ?, '', 0, 0, ?, 'pending', ?)`,
		string(OpRename), normOld, normNew, boolToInt(srcRoot.IsDir), nowNS,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	opID, err := res.LastInsertId()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}

	var blobsToDelete []string

	// Tombstone overwritten destination if any.
	for _, dn := range dstSubtree {
		dLower := strings.ToLower(dn.Path)
		dParentLower := strings.ToLower(parentOfNormalized(dn.Path))
		_, dBase := path.Split(dn.Path)
		if _, err := tx.Exec(`
			INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?, 1, 1, '', ?, ?)
			ON CONFLICT(path_lower) DO UPDATE SET
				deleted=1, dirty=1, blob_key='', last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
		`, dLower, dn.Path, dParentLower, dBase, boolToInt(dn.IsDir), dn.Mtime, dn.Ctime, opID, nowNS); err != nil {
			m.stateMu.Unlock()
			return err
		}
		var bKey string
		if err := tx.QueryRow(`SELECT blob_key FROM cache_entries WHERE path_lower = ?`, dLower).Scan(&bKey); err == nil && bKey != "" {
			blobsToDelete = append(blobsToDelete, bKey)
		}
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE path_lower = ?`, dLower); err != nil {
			m.stateMu.Unlock()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM node_meta WHERE path_lower = ?`, dLower); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	// Capture existing cache_entries under old subtree so we re-key them accurately.
	oldPrefixStart, oldPrefixEnd := subtreeBounds(oldLower)
	ceRows, err := tx.Query(
		`SELECT path, path_lower, blob_key, size, mtime, last_accessed_ns, dirty, remote_node_id, remote_etag, created_at_ns, updated_at_ns
		 FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		oldLower, oldPrefixStart, oldPrefixEnd,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	var movedEntries []cacheEntry
	for ceRows.Next() {
		var ce cacheEntry
		var dirtyInt int
		if err := ceRows.Scan(&ce.Path, &ce.PathLower, &ce.BlobKey, &ce.Size, &ce.Mtime, &ce.LastAccessedNS, &dirtyInt, &ce.RemoteNodeID, &ce.RemoteETag, &ce.CreatedAtNS, &ce.UpdatedAtNS); err != nil {
			_ = ceRows.Close()
			m.stateMu.Unlock()
			return err
		}
		ce.Dirty = dirtyInt != 0
		movedEntries = append(movedEntries, ce)
	}
	if err := ceRows.Close(); err != nil {
		m.stateMu.Unlock()
		return err
	}

	cacheBlobByLower := make(map[string]string, len(movedEntries))
	for _, ce := range movedEntries {
		cacheBlobByLower[ce.PathLower] = ce.BlobKey
	}

	// Capture existing node_meta under old subtree.
	nmRows, err := tx.Query(
		`SELECT path, path_lower, mtime, etag FROM node_meta WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		oldLower, oldPrefixStart, oldPrefixEnd,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	type nmRow struct {
		path, pathLower, etag string
		mtime                 int64
	}
	var movedMeta []nmRow
	for nmRows.Next() {
		var r nmRow
		if err := nmRows.Scan(&r.path, &r.pathLower, &r.mtime, &r.etag); err != nil {
			_ = nmRows.Close()
			m.stateMu.Unlock()
			return err
		}
		movedMeta = append(movedMeta, r)
	}
	if err := nmRows.Close(); err != nil {
		m.stateMu.Unlock()
		return err
	}

	// Materialize moved nodes and tombstone old paths.
	for _, n := range subtree {
		nLower := strings.ToLower(n.Path)
		rel := ""
		if len(n.Path) > len(normOld) {
			rel = n.Path[len(normOld):]
		}
		mappedPath := normNew + rel
		mappedLower := strings.ToLower(mappedPath)
		mappedParentLower := strings.ToLower(parentOfNormalized(mappedPath))
		_, mappedBase := path.Split(mappedPath)

		blobKey := cacheBlobByLower[nLower]
		if blobKey == "" {
			_ = tx.QueryRow(`SELECT blob_key FROM overlay_nodes WHERE path_lower = ?`, nLower).Scan(&blobKey)
		}

		if oldLower != newLower {
			nParentLower := strings.ToLower(parentOfNormalized(n.Path))
			_, nBase := path.Split(n.Path)
			if _, err := tx.Exec(`
				INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
				VALUES (?, ?, ?, ?, ?, 0, ?, ?, 1, 1, '', ?, ?)
				ON CONFLICT(path_lower) DO UPDATE SET
					deleted=1, dirty=1, blob_key='', last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
			`, nLower, n.Path, nParentLower, nBase, boolToInt(n.IsDir), n.Mtime, n.Ctime, opID, nowNS); err != nil {
				m.stateMu.Unlock()
				return err
			}
		}

		if _, err := tx.Exec(`
			INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 1, ?, ?, ?)
			ON CONFLICT(path_lower) DO UPDATE SET
				path=excluded.path, parent_lower=excluded.parent_lower, name=excluded.name,
				is_dir=excluded.is_dir, size=excluded.size, mtime=excluded.mtime, ctime=excluded.ctime,
				deleted=0, dirty=1, blob_key=excluded.blob_key, last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
		`, mappedLower, mappedPath, mappedParentLower, mappedBase, boolToInt(n.IsDir), n.Size, n.Mtime, n.Ctime, blobKey, opID, nowNS); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	// Re-key cache_entries under old subtree -> new subtree.
	if _, err := tx.Exec(
		`DELETE FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		oldLower, oldPrefixStart, oldPrefixEnd,
	); err != nil {
		m.stateMu.Unlock()
		return err
	}
	for _, ce := range movedEntries {
		rel := ""
		if len(ce.Path) > len(normOld) {
			rel = ce.Path[len(normOld):]
		}
		mappedPath := normNew + rel
		mappedLower := strings.ToLower(mappedPath)
		pinnedInt := boolToInt(m.isPinnedLocked(mappedPath))
		if _, err := tx.Exec(`
			INSERT INTO cache_entries (path_lower, path, blob_key, size, mtime, last_accessed_ns, dirty, pinned, remote_node_id, remote_etag, last_op_id, created_at_ns, updated_at_ns)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path_lower) DO UPDATE SET
				path=excluded.path, blob_key=excluded.blob_key, size=excluded.size, mtime=excluded.mtime,
				last_accessed_ns=excluded.last_accessed_ns, dirty=1, pinned=excluded.pinned,
				remote_node_id=excluded.remote_node_id, remote_etag=excluded.remote_etag, last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
		`, mappedLower, mappedPath, ce.BlobKey, ce.Size, ce.Mtime, ce.LastAccessedNS, pinnedInt, ce.RemoteNodeID, ce.RemoteETag, opID, ce.CreatedAtNS, nowNS); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	// Re-key node_meta.
	if _, err := tx.Exec(
		`DELETE FROM node_meta WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		oldLower, oldPrefixStart, oldPrefixEnd,
	); err != nil {
		m.stateMu.Unlock()
		return err
	}
	for _, rm := range movedMeta {
		rel := ""
		if len(rm.path) > len(normOld) {
			rel = rm.path[len(normOld):]
		}
		mappedPath := normNew + rel
		mappedLower := strings.ToLower(mappedPath)
		if _, err := tx.Exec(`
			INSERT INTO node_meta (path_lower, path, mtime, etag) VALUES (?, ?, ?, ?)
			ON CONFLICT(path_lower) DO UPDATE SET path=excluded.path, mtime=excluded.mtime, etag=excluded.etag
		`, mappedLower, mappedPath, rm.mtime, rm.etag); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	m.stateMu.Unlock()

	m.scheduleBlobDeletes(blobsToDelete)
	m.notifyChange(normOld, normNew)

	if m.backend != nil && m.backend.IsOnline() {
		_ = m.FlushQueue(ctx)
	}
	return nil
}

// QueueDelete durably records a file or directory deletion in the offline queue, overlay, and cache.
func (m *Manager) QueueDelete(ctx context.Context, p string, isDir bool) error {
	normPath := NormalizePath(p)
	if _, err := validateBaseName(normPath); err != nil {
		return err
	}

	m.stateMu.Lock()
	subtree, err := m.collectLiveSubtreeLocked(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if subtree[0].IsDir != isDir {
		m.stateMu.Unlock()
		return os.ErrInvalid
	}

	pathLower := strings.ToLower(normPath)
	prefixStart, prefixEnd := subtreeBounds(pathLower)
	nowNS := time.Now().UnixNano()

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO write_queue (op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns)
		 VALUES (?, ?, '', '', 0, 0, ?, 'pending', ?)`,
		string(OpDelete), normPath, boolToInt(isDir), nowNS,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	opID, err := res.LastInsertId()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}

	for _, n := range subtree {
		nLower := strings.ToLower(n.Path)
		nParentLower := strings.ToLower(parentOfNormalized(n.Path))
		_, nBase := path.Split(n.Path)
		if _, err := tx.Exec(`
			INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?, 1, 1, '', ?, ?)
			ON CONFLICT(path_lower) DO UPDATE SET
				deleted=1, dirty=1, blob_key='', last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
		`, nLower, n.Path, nParentLower, nBase, boolToInt(n.IsDir), n.Mtime, n.Ctime, opID, nowNS); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	// Ensure any existing overlay rows under this path are also marked deleted with opID.
	if _, err := tx.Exec(`
		UPDATE overlay_nodes SET deleted = 1, dirty = 1, blob_key = '', last_op_id = ?, updated_at_ns = ?
		WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)
	`, opID, nowNS, pathLower, prefixStart, prefixEnd); err != nil {
		m.stateMu.Unlock()
		return err
	}

	var blobsToDelete []string
	rows, err := tx.Query(
		`SELECT blob_key FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		pathLower, prefixStart, prefixEnd,
	)
	if err == nil {
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err == nil && k != "" {
				blobsToDelete = append(blobsToDelete, k)
			}
		}
		_ = rows.Close()
	}

	if _, err := tx.Exec(
		`DELETE FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		pathLower, prefixStart, prefixEnd,
	); err != nil {
		m.stateMu.Unlock()
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM node_meta WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
		pathLower, prefixStart, prefixEnd,
	); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	m.stateMu.Unlock()

	m.scheduleBlobDeletes(blobsToDelete)
	m.notifyChange(normPath)

	if m.backend != nil && m.backend.IsOnline() {
		_ = m.FlushQueue(ctx)
	}
	return nil
}

// QueueUtimens durably updates a file or directory's modification timestamp in the overlay and queue.
func (m *Manager) QueueUtimens(ctx context.Context, p string, mtime int64) error {
	normPath := NormalizePath(p)
	baseName, err := validateBaseName(normPath)
	if err != nil {
		return err
	}

	m.stateMu.Lock()
	ov, found, err := m.statLocked(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	if !found {
		m.stateMu.Unlock()
		return os.ErrNotExist
	}

	pathLower := strings.ToLower(normPath)
	parentLower := strings.ToLower(parentOfNormalized(normPath))
	nowNS := time.Now().UnixNano()

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO write_queue (op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns)
		 VALUES (?, ?, '', '', 0, ?, ?, 'pending', ?)`,
		string(OpUtimens), normPath, mtime, boolToInt(ov.IsDir), nowNS,
	)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	opID, err := res.LastInsertId()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}

	var existingBlob, existingETag string
	_ = tx.QueryRow(`SELECT blob_key, remote_etag FROM cache_entries WHERE path_lower = ?`, pathLower).Scan(&existingBlob, &existingETag)
	if existingBlob == "" {
		_ = tx.QueryRow(`SELECT blob_key FROM overlay_nodes WHERE path_lower = ?`, pathLower).Scan(&existingBlob)
	}

	if _, err := tx.Exec(`
		INSERT INTO overlay_nodes (path_lower, path, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 1, ?, ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET
			mtime=excluded.mtime, deleted=0, dirty=1, last_op_id=excluded.last_op_id, updated_at_ns=excluded.updated_at_ns
	`, pathLower, ov.Path, parentLower, baseName, boolToInt(ov.IsDir), ov.Size, mtime, ov.Ctime, existingBlob, opID, nowNS); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if _, err := tx.Exec(`UPDATE cache_entries SET mtime = ?, last_op_id = ? WHERE path_lower = ?`, mtime, opID, pathLower); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO node_meta (path_lower, path, mtime, etag) VALUES (?, ?, ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET path=excluded.path, mtime=excluded.mtime, etag=excluded.etag
	`, pathLower, ov.Path, mtime, existingETag); err != nil {
		m.stateMu.Unlock()
		return err
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	m.stateMu.Unlock()
	m.notifyChange(normPath)

	if m.backend != nil && m.backend.IsOnline() {
		_ = m.FlushQueue(ctx)
	}
	return nil
}

type appliedWrite struct {
	op          queueOpRow
	remote      *RemoteNode
	pathLower   string
	preservedMt int64
}

// FlushQueue replays queued offline operations in strict FIFO order while online.
func (m *Manager) FlushQueue(ctx context.Context) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	m.statusMu.Lock()
	m.syncing = true
	m.statusMu.Unlock()
	defer func() {
		m.statusMu.Lock()
		m.syncing = false
		m.statusMu.Unlock()
	}()

	var deferredWrites []appliedWrite
	flushDeferredWrites := func() error {
		if len(deferredWrites) == 0 {
			return nil
		}
		if err := m.backend.FlushPending(ctx); err != nil {
			return err
		}
		m.stateMu.Lock()
		tx, err := m.db.Begin()
		if err != nil {
			m.stateMu.Unlock()
			return err
		}
		defer tx.Rollback()

		var payloads []string
		for _, w := range deferredWrites {
			if _, err := tx.Exec(`DELETE FROM write_queue WHERE id = ?`, w.op.ID); err != nil {
				m.stateMu.Unlock()
				return err
			}
			if _, err := tx.Exec(`DELETE FROM overlay_nodes WHERE last_op_id <= ?`, w.op.ID); err != nil {
				m.stateMu.Unlock()
				return err
			}
			if w.remote != nil {
				if _, err := tx.Exec(
					`UPDATE cache_entries SET remote_node_id = ?, remote_etag = ?, dirty = 0 WHERE path_lower = ? AND last_op_id <= ?`,
					w.remote.ID, w.remote.ETag, w.pathLower, w.op.ID,
				); err != nil {
					m.stateMu.Unlock()
					return err
				}
				if _, err := tx.Exec(
					`UPDATE node_meta SET etag = ? WHERE path_lower = ?`,
					w.remote.ETag, w.pathLower,
				); err != nil {
					m.stateMu.Unlock()
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE cache_entries SET dirty = 0 WHERE dirty = 1 AND last_op_id <= ?`, w.op.ID); err != nil {
				m.stateMu.Unlock()
				return err
			}
			if w.op.PayloadFile != "" {
				payloads = append(payloads, w.op.PayloadFile)
			}
		}
		if err := tx.Commit(); err != nil {
			m.stateMu.Unlock()
			return err
		}
		_ = m.evictToBudgetLocked()
		m.stateMu.Unlock()

		changedPaths := make([]string, 0, len(deferredWrites))
		for _, w := range deferredWrites {
			changedPaths = append(changedPaths, w.op.Path)
		}
		for _, pf := range payloads {
			removeWithRetry(m.queuePayloadPath(pf))
		}
		m.statusMu.Lock()
		m.lastSyncedAt = time.Now().Unix()
		m.lastError = ""
		m.statusMu.Unlock()
		deferredWrites = nil
		m.notifyChange(changedPaths...)
		return nil
	}

	var lastProcessedID int64
	for {
		if err := ctx.Err(); err != nil {
			_ = flushDeferredWrites()
			m.setLastError(err.Error())
			return err
		}
		if m.backend == nil || !m.backend.IsOnline() {
			_ = flushDeferredWrites()
			err := ErrOfflineUncached
			if m.backend == nil {
				err = errors.New("offline: no backend configured")
			} else {
				err = storage.ErrNotReady
			}
			m.setLastError(err.Error())
			return err
		}

		op, found, err := m.nextPendingOp(lastProcessedID)
		if err != nil {
			_ = flushDeferredWrites()
			m.setLastError(err.Error())
			return err
		}
		if !found {
			break
		}

		// Flush any buffered Packer writes before running a non-write op so FIFO retirement stays exact.
		if op.OpType != OpWrite && len(deferredWrites) > 0 {
			if ferr := flushDeferredWrites(); ferr != nil {
				m.setLastError(ferr.Error())
				return ferr
			}
		}

		prevStatus := op.Status
		if err := m.markOpApplying(op.ID); err != nil {
			_ = flushDeferredWrites()
			m.setLastError(err.Error())
			return err
		}

		if op.OpType == OpWrite {
			rn, werr := m.execWriteOp(ctx, op)
			if werr != nil {
				_ = flushDeferredWrites()
				m.setLastError(werr.Error())
				return werr
			}
			deferredWrites = append(deferredWrites, appliedWrite{
				op:          op,
				remote:      rn,
				pathLower:   strings.ToLower(op.Path),
				preservedMt: op.Mtime,
			})
			lastProcessedID = op.ID
			continue
		}

		if err := m.execNonWriteOp(ctx, op, prevStatus); err != nil {
			m.setLastError(err.Error())
			return err
		}

		if err := m.retireSingleOp(op); err != nil {
			m.setLastError(err.Error())
			return err
		}
		lastProcessedID = op.ID
		m.statusMu.Lock()
		m.lastSyncedAt = time.Now().Unix()
		m.lastError = ""
		m.statusMu.Unlock()
	}

	if err := flushDeferredWrites(); err != nil {
		m.setLastError(err.Error())
		return err
	}
	return nil
}

func (m *Manager) setLastError(msg string) {
	m.statusMu.Lock()
	m.lastError = msg
	m.statusMu.Unlock()
}

func (m *Manager) nextPendingOp(afterID int64) (queueOpRow, bool, error) {
	var op queueOpRow
	var opType string
	var isDirInt int
	err := m.db.QueryRow(
		`SELECT id, op_type, path, new_path, payload_file, size, mtime, is_dir, status, created_at_ns
		 FROM write_queue WHERE id > ? ORDER BY id ASC LIMIT 1`,
		afterID,
	).Scan(&op.ID, &opType, &op.Path, &op.NewPath, &op.PayloadFile, &op.Size, &op.Mtime, &isDirInt, &op.Status, &op.CreatedAtNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return op, false, nil
		}
		return op, false, err
	}
	op.OpType = QueueOpType(opType)
	op.IsDir = isDirInt != 0
	return op, true, nil
}

func (m *Manager) markOpApplying(opID int64) error {
	_, err := m.db.Exec(`UPDATE write_queue SET status = 'applying' WHERE id = ?`, opID)
	return err
}

func (m *Manager) execWriteOp(ctx context.Context, op queueOpRow) (*RemoteNode, error) {
	if op.Size == 0 {
		return m.backend.WriteFile(ctx, op.Path, strings.NewReader(""), 0, op.Mtime)
	}
	f, err := os.Open(m.queuePayloadPath(op.PayloadFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return m.backend.WriteFile(ctx, op.Path, f, op.Size, op.Mtime)
}

func (m *Manager) execNonWriteOp(ctx context.Context, op queueOpRow, prevStatus string) error {
	switch op.OpType {
	case OpMkdir:
		err := m.backend.Mkdir(ctx, op.Path)
		if err != nil && prevStatus == "applying" && errors.Is(err, db.ErrAlreadyExist) {
			if rn, ok, serr := m.backend.Stat(op.Path); serr == nil && ok && rn.IsDir {
				return nil
			}
		}
		return err
	case OpRename:
		err := m.backend.Rename(ctx, op.Path, op.NewPath)
		if err != nil && prevStatus == "applying" && (errors.Is(err, db.ErrNotFound) || errors.Is(err, os.ErrNotExist)) {
			_, oldFound, _ := m.backend.Stat(op.Path)
			_, newFound, _ := m.backend.Stat(op.NewPath)
			if !oldFound && newFound {
				return nil
			}
		}
		return err
	case OpDelete:
		err := m.backend.Delete(ctx, op.Path, op.IsDir)
		if err != nil && (errors.Is(err, db.ErrNotFound) || errors.Is(err, os.ErrNotExist)) {
			return nil
		}
		return err
	case OpUtimens:
		err := m.backend.SetMtime(ctx, op.Path, op.Mtime)
		if err != nil && prevStatus == "applying" && (errors.Is(err, db.ErrNotFound) || errors.Is(err, os.ErrNotExist)) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown queue op type: %s", op.OpType)
	}
}

func (m *Manager) retireSingleOp(op queueOpRow) error {
	// Refresh remote metadata for renamed/utimens entries before clearing dirty flag so ETag stays aligned.
	type metaRefresh struct {
		pathLower string
		nodeID    int64
		etag      string
	}
	var refreshes []metaRefresh
	if op.OpType == OpRename {
		newLower := strings.ToLower(op.NewPath)
		pStart, pEnd := subtreeBounds(newLower)
		rows, err := m.db.Query(
			`SELECT path, path_lower FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
			newLower, pStart, pEnd,
		)
		if err == nil {
			var paths []struct{ p, pl string }
			for rows.Next() {
				var p, pl string
				if err := rows.Scan(&p, &pl); err == nil {
					paths = append(paths, struct{ p, pl string }{p, pl})
				}
			}
			_ = rows.Close()
			for _, item := range paths {
				if rn, ok, serr := m.backend.Stat(item.p); serr == nil && ok {
					refreshes = append(refreshes, metaRefresh{pathLower: item.pl, nodeID: rn.ID, etag: rn.ETag})
				}
			}
		}
	} else if op.OpType == OpUtimens {
		if rn, ok, serr := m.backend.Stat(op.Path); serr == nil && ok {
			refreshes = append(refreshes, metaRefresh{pathLower: strings.ToLower(op.Path), nodeID: rn.ID, etag: rn.ETag})
		}
	}

	err := func() error {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()

		tx, err := m.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if _, err := tx.Exec(`DELETE FROM write_queue WHERE id = ?`, op.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM overlay_nodes WHERE last_op_id <= ?`, op.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE cache_entries SET dirty = 0 WHERE dirty = 1 AND last_op_id <= ?`, op.ID); err != nil {
			return err
		}
		for _, r := range refreshes {
			if _, err := tx.Exec(
				`UPDATE cache_entries SET remote_node_id = ?, remote_etag = ? WHERE path_lower = ? AND last_op_id <= ?`,
				r.nodeID, r.etag, r.pathLower, op.ID,
			); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE node_meta SET etag = ? WHERE path_lower = ?`, r.etag, r.pathLower); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return m.evictToBudgetLocked()
	}()
	if err != nil {
		return err
	}
	if op.NewPath != "" {
		m.notifyChange(op.Path, op.NewPath)
	} else {
		m.notifyChange(op.Path)
	}
	return nil
}

// StartSyncLoop starts a background goroutine that flushes queued ops while online.
func (m *Manager) StartSyncLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	m.syncMu.Lock()
	if m.closed || m.syncStarted {
		m.syncMu.Unlock()
		return
	}
	m.syncStarted = true
	m.syncWg.Add(1)
	m.syncMu.Unlock()

	go func() {
		defer m.syncWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-m.closeCh:
				return
			case <-ticker.C:
				if m.backend != nil && m.backend.IsOnline() && m.QueueStatus().PendingCount > 0 {
					_ = m.FlushQueue(ctx)
				}
			}
		}
	}()
}

// QueueStatus returns current offline write queue depth and sync status.
func (m *Manager) QueueStatus() QueueStatus {
	var cnt sql.NullInt64
	var bytes sql.NullInt64
	_ = m.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM write_queue`).Scan(&cnt, &bytes)

	m.statusMu.Lock()
	syncing := m.syncing
	lastErr := m.lastError
	lastSynced := m.lastSyncedAt
	m.statusMu.Unlock()

	return QueueStatus{
		PendingCount: int(cnt.Int64),
		PendingBytes: bytes.Int64,
		Syncing:      syncing,
		LastError:    lastErr,
		LastSyncedAt: lastSynced,
	}
}

func sortOverlays(items []NodeOverlay) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Path) < strings.ToLower(items[j].Path)
	})
}
