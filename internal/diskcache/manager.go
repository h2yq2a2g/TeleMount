package diskcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"telemount/internal/db"
	"telemount/internal/storage"
)

const DefaultMaxBytes int64 = 10 * 1024 * 1024 * 1024

var ErrOfflineUncached = errors.New("offline and file not cached")

type CacheStats struct {
	TotalBytes       int64
	PinnedBytes      int64
	UnpinnedBytes    int64
	MaxBytes         int64
	EntryCount       int
	PinnedEntryCount int
	EvictionCount    int64
}

type NodeOverlay struct {
	Path    string
	IsDir   bool
	Size    int64
	Mtime   int64
	Ctime   int64
	Deleted bool
	Dirty   bool
}

type Manager struct {
	cacheDir string
	dbPath   string
	db       *sql.DB
	store    *db.Store
	backend  Backend

	flushMu  sync.Mutex
	stateMu  sync.Mutex
	statusMu sync.Mutex
	syncMu   sync.Mutex

	maxBytes     int64
	pinnedPaths  []string
	lastAccessNS atomic.Int64

	blobMu              sync.Mutex
	activeBlobReaders   map[string]int
	deferredBlobDeletes map[string]bool

	sf singleflight.Group

	evictionCount atomic.Int64

	syncing      bool
	lastError    string
	lastSyncedAt int64

	syncStarted bool
	closed      bool
	closeCh     chan struct{}
	syncWg      sync.WaitGroup

	onChangeMu   sync.RWMutex
	onChange     func(paths ...string)
	pinSyncingMu sync.Mutex
	pinSyncing   map[string]int
}

// Open initializes a persistent disk cache and offline write queue using store and engine.
// If maxBytes < 0, DefaultMaxBytes is used. maxBytes == 0 is valid (0-byte unpinned cache budget).
// engine == nil runs cleanly in offline mode; store == nil runs in pure local cache/queue mode.
func Open(cacheDir, dbPath string, maxBytes int64, store *db.Store, engine *storage.Engine) (*Manager, error) {
	var backend Backend
	if store != nil || engine != nil {
		backend = &engineBackend{store: store, engine: engine}
	}
	return OpenWithBackend(cacheDir, dbPath, maxBytes, store, backend)
}

// OpenWithBackend initializes a Manager with an explicit Backend implementation for testing or custom transports.
func OpenWithBackend(cacheDir, dbPath string, maxBytes int64, store *db.Store, backend Backend) (*Manager, error) {
	if maxBytes < 0 {
		maxBytes = DefaultMaxBytes
	}
	if backend == nil && store != nil {
		backend = &engineBackend{store: store, engine: nil}
	}
	if err := ensureCacheDirs(cacheDir); err != nil {
		return nil, err
	}
	conn, err := openCacheDB(dbPath)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		cacheDir:            cacheDir,
		dbPath:              dbPath,
		db:                  conn,
		store:               store,
		backend:             backend,
		maxBytes:            maxBytes,
		activeBlobReaders:   make(map[string]int),
		deferredBlobDeletes: make(map[string]bool),
		closeCh:             make(chan struct{}),
		pinSyncing:          make(map[string]int),
	}

	// Seed strictly monotonic lastAccessNS from DB.
	var maxAccess sql.NullInt64
	if err := conn.QueryRow(`SELECT COALESCE(MAX(last_accessed_ns), 0) FROM cache_entries`).Scan(&maxAccess); err != nil {
		_ = conn.Close()
		return nil, err
	}
	m.lastAccessNS.Store(maxAccess.Int64)

	// Load persisted pinned paths.
	rows, err := conn.Query(`SELECT path FROM pinned_paths ORDER BY rowid ASC`)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil && strings.TrimSpace(p) != "" {
			m.pinnedPaths = append(m.pinnedPaths, NormalizePath(p))
		}
	}
	_ = rows.Close()

	if err := m.cleanupOnOpen(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	m.stateMu.Lock()
	err = m.evictToBudgetLocked()
	m.stateMu.Unlock()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	return m, nil
}

// SetOnChange registers a callback invoked after local cache, pin, or dirty state transitions.
// Passing nil clears the callback. The callback is always called without holding stateMu.
func (m *Manager) SetOnChange(fn func(paths ...string)) {
	m.onChangeMu.Lock()
	m.onChange = fn
	m.onChangeMu.Unlock()
}

func (m *Manager) notifyChange(paths ...string) {
	m.onChangeMu.RLock()
	fn := m.onChange
	m.onChangeMu.RUnlock()
	if fn != nil {
		fn(paths...)
	}
}

func (m *Manager) beginPinSync(normPath string) {
	key := strings.ToLower(NormalizePath(normPath))
	m.pinSyncingMu.Lock()
	m.pinSyncing[key]++
	m.pinSyncingMu.Unlock()
	m.notifyChange(normPath)
}

func (m *Manager) endPinSync(normPath string) {
	key := strings.ToLower(NormalizePath(normPath))
	m.pinSyncingMu.Lock()
	if n := m.pinSyncing[key]; n <= 1 {
		delete(m.pinSyncing, key)
	} else {
		m.pinSyncing[key] = n - 1
	}
	m.pinSyncingMu.Unlock()
	m.notifyChange(normPath)
}

func (m *Manager) isPinSyncing(normPath string) bool {
	lt := strings.ToLower(NormalizePath(normPath))
	m.pinSyncingMu.Lock()
	defer m.pinSyncingMu.Unlock()
	for k := range m.pinSyncing {
		if k == "/" || lt == k || strings.HasPrefix(lt, k+"/") {
			return true
		}
	}
	return false
}

// Close stops background sync loops and closes desktop_cache.db cleanly.
func (m *Manager) Close() error {
	m.syncMu.Lock()
	if m.closed {
		m.syncMu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closeCh)
	m.syncMu.Unlock()

	m.syncWg.Wait()
	return m.db.Close()
}

func (m *Manager) hasDeletedAncestorLocked(normPath string) (bool, error) {
	if normPath == "/" {
		return false, nil
	}
	cur := parentOfNormalized(normPath)
	for cur != "/" {
		ov, ok, err := m.getOverlayRow(strings.ToLower(cur))
		if err != nil {
			return false, err
		}
		if ok && (ov.Deleted || !ov.IsDir) {
			return true, nil
		}
		cur = parentOfNormalized(cur)
	}
	return false, nil
}

func (m *Manager) statLocked(normPath string) (*NodeOverlay, bool, error) {
	if normPath == "/" {
		return &NodeOverlay{Path: "/", IsDir: true}, true, nil
	}
	pathLower := strings.ToLower(normPath)

	if ov, ok, err := m.getOverlayRow(pathLower); err != nil {
		return nil, false, err
	} else if ok {
		if ov.Deleted {
			return nil, false, nil
		}
		ancDeleted, aerr := m.hasDeletedAncestorLocked(normPath)
		if aerr != nil {
			return nil, false, aerr
		}
		if ancDeleted {
			return nil, false, nil
		}
		return &NodeOverlay{
			Path:    ov.Path,
			IsDir:   ov.IsDir,
			Size:    ov.Size,
			Mtime:   ov.Mtime,
			Ctime:   ov.Ctime,
			Deleted: false,
			Dirty:   ov.Dirty,
		}, true, nil
	}

	ancDeleted, err := m.hasDeletedAncestorLocked(normPath)
	if err != nil {
		return nil, false, err
	}
	if ancDeleted {
		return nil, false, nil
	}

	if m.backend == nil {
		return nil, false, nil
	}
	rn, exists, err := m.backend.Stat(normPath)
	if err != nil || !exists {
		return nil, false, err
	}
	mtime := rn.Mtime
	if metaMt, metaETag, ok := m.getNodeMetaMtime(strings.ToLower(rn.Path)); ok {
		if metaETag == "" || rn.ETag == "" || metaETag == rn.ETag {
			mtime = metaMt
		}
	}
	return &NodeOverlay{
		Path:    rn.Path,
		IsDir:   rn.IsDir,
		Size:    rn.Size,
		Mtime:   mtime,
		Ctime:   rn.Ctime,
		Deleted: false,
		Dirty:   false,
	}, true, nil
}

// Stat resolves a virtual path by overlaying local dirty/tombstone state onto the backend metadata store.
func (m *Manager) Stat(p string) (*NodeOverlay, bool, error) {
	normPath := NormalizePath(p)
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.statLocked(normPath)
}

func (m *Manager) listDirLocked(dirPath string) ([]NodeOverlay, error) {
	normDir := NormalizePath(dirPath)
	ov, found, err := m.statLocked(normDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	if !ov.IsDir {
		return nil, os.ErrInvalid
	}

	merged := make(map[string]NodeOverlay)
	if m.backend != nil {
		remoteChildren, berr := m.backend.ListDir(ov.Path)
		if berr != nil && !errors.Is(berr, os.ErrNotExist) && !errors.Is(berr, db.ErrNotFound) {
			return nil, berr
		}
		for _, rn := range remoteChildren {
			rnLower := strings.ToLower(rn.Path)
			mtime := rn.Mtime
			if metaMt, metaETag, ok := m.getNodeMetaMtime(rnLower); ok {
				if metaETag == "" || rn.ETag == "" || metaETag == rn.ETag {
					mtime = metaMt
				}
			}
			merged[rnLower] = NodeOverlay{
				Path:  rn.Path,
				IsDir: rn.IsDir,
				Size:  rn.Size,
				Mtime: mtime,
				Ctime: rn.Ctime,
			}
		}
	}

	parentLower := strings.ToLower(ov.Path)
	rows, err := m.db.Query(
		`SELECT path, path_lower, is_dir, size, mtime, ctime, deleted, dirty
		 FROM overlay_nodes WHERE parent_lower = ?`,
		parentLower,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, pLower string
		var isDirInt, deletedInt, dirtyInt int
		var sz, mt, ct int64
		if err := rows.Scan(&p, &pLower, &isDirInt, &sz, &mt, &ct, &deletedInt, &dirtyInt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if deletedInt != 0 {
			delete(merged, pLower)
		} else {
			merged[pLower] = NodeOverlay{
				Path:    p,
				IsDir:   isDirInt != 0,
				Size:    sz,
				Mtime:   mt,
				Ctime:   ct,
				Deleted: false,
				Dirty:   dirtyInt != 0,
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make([]NodeOverlay, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	sortOverlays(out)
	return out, nil
}

// ListDir returns directory children merging backend entries with the local offline overlay.
func (m *Manager) ListDir(dirPath string) ([]NodeOverlay, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.listDirLocked(dirPath)
}

// ReadFileAt reads len(buf) bytes starting at offset, serving from dirty overlay -> clean disk cache -> online fetch.
func (m *Manager) ReadFileAt(ctx context.Context, p string, buf []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative offset: %d", offset)
	}
	normPath := NormalizePath(p)
	if normPath == "/" {
		return 0, os.ErrInvalid
	}
	pathLower := strings.ToLower(normPath)

	m.stateMu.Lock()
	// Check ancestor tombstone first.
	if ancDel, err := m.hasDeletedAncestorLocked(normPath); err != nil {
		m.stateMu.Unlock()
		return 0, err
	} else if ancDel {
		m.stateMu.Unlock()
		return 0, os.ErrNotExist
	}

	// Priority 1: overlay_nodes (tombstone or dirty local file).
	if ov, ok, err := m.getOverlayRow(pathLower); err != nil {
		m.stateMu.Unlock()
		return 0, err
	} else if ok {
		if ov.Deleted {
			m.stateMu.Unlock()
			return 0, os.ErrNotExist
		}
		if ov.IsDir {
			m.stateMu.Unlock()
			return 0, os.ErrInvalid
		}
		if ov.Dirty {
			blobKey := ov.BlobKey
			if blobKey == "" && ov.Size > 0 {
				if ce, ceOk, _ := m.getCacheEntry(pathLower); ceOk {
					blobKey = ce.BlobKey
				}
			}
			accessNS := m.nextAccessNS()
			_, _ = m.db.Exec(`UPDATE cache_entries SET last_accessed_ns = ? WHERE path_lower = ?`, accessNS, pathLower)
			size := ov.Size
			m.stateMu.Unlock()

			if size == 0 || offset >= size {
				return 0, io.EOF
			}
			if len(buf) == 0 {
				return 0, nil
			}
			if !m.blobValid(blobKey, size) {
				return 0, fmt.Errorf("dirty blob missing for %s", normPath)
			}
			return m.readBlobAt(blobKey, size, buf, offset)
		}
	}

	// Priority 2: clean cache_entries + staleness check.
	var staleBlobToDelete string
	var rn *RemoteNode
	var remoteFound bool
	if m.backend != nil {
		var serr error
		rn, remoteFound, serr = m.backend.Stat(normPath)
		if serr != nil {
			m.stateMu.Unlock()
			return 0, serr
		}
	}

	if ce, ceOk, err := m.getCacheEntry(pathLower); err != nil {
		m.stateMu.Unlock()
		return 0, err
	} else if ceOk && !ce.Dirty {
		stale := false
		if m.backend != nil {
			if !remoteFound {
				stale = true
			} else if rn.IsDir {
				_, _ = m.db.Exec(`DELETE FROM cache_entries WHERE path_lower = ? AND dirty = 0`, pathLower)
				staleBlobToDelete = ce.BlobKey
				m.stateMu.Unlock()
				m.scheduleBlobDeletes([]string{staleBlobToDelete})
				return 0, os.ErrInvalid
			} else {
				stale = ce.RemoteETag != rn.ETag || ce.Size != rn.Size || (ce.RemoteNodeID != 0 && rn.ID != 0 && ce.RemoteNodeID != rn.ID)
			}
		}
		if !stale && m.blobValid(ce.BlobKey, ce.Size) {
			accessNS := m.nextAccessNS()
			_, _ = m.db.Exec(`UPDATE cache_entries SET last_accessed_ns = ? WHERE path_lower = ?`, accessNS, pathLower)
			size := ce.Size
			blobKey := ce.BlobKey
			m.stateMu.Unlock()

			if size == 0 || offset >= size {
				return 0, io.EOF
			}
			if len(buf) == 0 {
				return 0, nil
			}
			return m.readBlobAt(blobKey, size, buf, offset)
		}
		// Stale or corrupt clean cache entry.
		_, _ = m.db.Exec(`DELETE FROM cache_entries WHERE path_lower = ? AND dirty = 0`, pathLower)
		staleBlobToDelete = ce.BlobKey
	}

	if m.backend == nil {
		m.stateMu.Unlock()
		m.scheduleBlobDeletes([]string{staleBlobToDelete})
		return 0, os.ErrNotExist
	}
	if !remoteFound {
		m.stateMu.Unlock()
		m.scheduleBlobDeletes([]string{staleBlobToDelete})
		return 0, os.ErrNotExist
	}
	if rn.IsDir {
		m.stateMu.Unlock()
		m.scheduleBlobDeletes([]string{staleBlobToDelete})
		return 0, os.ErrInvalid
	}
	isPinned := m.isPinnedLocked(rn.Path)
	maxBytes := m.maxBytes
	m.stateMu.Unlock()

	if staleBlobToDelete != "" {
		m.scheduleBlobDeletes([]string{staleBlobToDelete})
	}

	// Priority 3: online fetch.
	if !m.backend.IsOnline() {
		return 0, ErrOfflineUncached
	}

	// Unpinned file larger than maxBytes: serve without retaining in cache_entries so TotalBytes <= MaxBytes holds.
	if !isPinned && maxBytes >= 0 && rn.Size > maxBytes {
		return m.readOversizedUncached(ctx, rn.Path, rn.Size, buf, offset)
	}

	ce, err := m.ensureCachedSingleflight(ctx, rn.Path)
	if err != nil {
		return 0, err
	}
	if ce.Size == 0 || offset >= ce.Size {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	n, err := m.readBlobAt(ce.BlobKey, ce.Size, buf, offset)
	if errors.Is(err, os.ErrNotExist) {
		return m.readOversizedUncached(ctx, rn.Path, ce.Size, buf, offset)
	}
	return n, err
}

func (m *Manager) readOversizedUncached(ctx context.Context, normPath string, size int64, buf []byte, offset int64) (int, error) {
	if size == 0 || offset >= size {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	rc, remoteSize, _, err := m.backend.OpenReader(ctx, normPath)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	if remoteSize >= 0 {
		size = remoteSize
		if size == 0 || offset >= size {
			return 0, io.EOF
		}
	}

	if ra, ok := rc.(io.ReaderAt); ok {
		want := len(buf)
		if rem := size - offset; int64(want) > rem {
			want = int(rem)
		}
		total := 0
		for total < want {
			n, rerr := ra.ReadAt(buf[total:want], offset+int64(total))
			total += n
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					if total < len(buf) {
						return total, io.EOF
					}
					return total, nil
				}
				return total, rerr
			}
			if n == 0 {
				break
			}
		}
		if total < len(buf) {
			return total, io.EOF
		}
		return total, nil
	}

	if err := ensureCacheDirs(m.cacheDir); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(m.stagingDir(), "dl-uncached-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		removeWithRetry(tmpPath)
	}()

	cpBuf := make([]byte, 256*1024)
	written, err := io.CopyBuffer(tmp, rc, cpBuf)
	if err != nil {
		return 0, err
	}
	if remoteSize >= 0 && written != remoteSize {
		return 0, io.ErrUnexpectedEOF
	}
	return readFileSliceAt(tmp, written, buf, offset)
}

func (m *Manager) ensureCachedSingleflight(ctx context.Context, normPath string) (*cacheEntry, error) {
	sfKey := strings.ToLower(normPath)
	dlCtx := context.WithoutCancel(ctx)
	ch := m.sf.DoChan(sfKey, func() (any, error) {
		return m.downloadAndCommitClean(dlCtx, normPath)
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		ce, ok := res.Val.(*cacheEntry)
		if !ok || ce == nil {
			return nil, errors.New("unexpected empty singleflight result")
		}
		return ce, nil
	}
}

func (m *Manager) downloadAndCommitClean(ctx context.Context, normPath string) (*cacheEntry, error) {
	pathLower := strings.ToLower(normPath)

	// Re-check if already cleanly cached while waiting.
	m.stateMu.Lock()
	if ov, ok, err := m.getOverlayRow(pathLower); err == nil && ok {
		if ov.Deleted {
			m.stateMu.Unlock()
			return nil, os.ErrNotExist
		}
		if ov.Dirty {
			if ce, ceOk, _ := m.getCacheEntry(pathLower); ceOk {
				m.stateMu.Unlock()
				return ce, nil
			}
		}
	}
	if m.backend == nil {
		m.stateMu.Unlock()
		return nil, os.ErrNotExist
	}
	rn, exists, err := m.backend.Stat(normPath)
	if err != nil {
		m.stateMu.Unlock()
		return nil, err
	}
	if !exists {
		m.stateMu.Unlock()
		return nil, os.ErrNotExist
	}
	if rn.IsDir {
		m.stateMu.Unlock()
		return nil, os.ErrInvalid
	}
	if ce, ceOk, _ := m.getCacheEntry(pathLower); ceOk && !ce.Dirty {
		if ce.RemoteETag == rn.ETag && ce.Size == rn.Size && (ce.RemoteNodeID == 0 || rn.ID == 0 || ce.RemoteNodeID == rn.ID) && m.blobValid(ce.BlobKey, ce.Size) {
			accessNS := m.nextAccessNS()
			_, _ = m.db.Exec(`UPDATE cache_entries SET last_accessed_ns = ? WHERE path_lower = ?`, accessNS, pathLower)
			ce.LastAccessedNS = accessNS
			m.stateMu.Unlock()
			return ce, nil
		}
	}
	canonicalPath := rn.Path
	m.stateMu.Unlock()

	if !m.backend.IsOnline() {
		return nil, ErrOfflineUncached
	}

	rc, remoteSize, remoteETag, err := m.backend.OpenReader(ctx, canonicalPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var blobKey string
	var written int64
	if remoteSize != 0 {
		if err := ensureCacheDirs(m.cacheDir); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(m.stagingDir(), "dl-*.tmp")
		if err != nil {
			return nil, err
		}
		tmpPath := tmp.Name()
		committed := false
		defer func() {
			if !committed {
				_ = tmp.Close()
				removeWithRetry(tmpPath)
			}
		}()

		buf := make([]byte, 256*1024)
		written, err = io.CopyBuffer(tmp, rc, buf)
		if err != nil {
			return nil, err
		}
		if remoteSize >= 0 && written != remoteSize {
			return nil, io.ErrUnexpectedEOF
		}
		if err := tmp.Sync(); err != nil {
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		if written > 0 {
			blobKey = newHexID()
			if err := renameWithRetry(tmpPath, m.blobPath(blobKey)); err != nil {
				return nil, err
			}
		} else {
			removeWithRetry(tmpPath)
		}
		committed = true
	}

	if remoteETag == "" {
		remoteETag = rn.ETag
	}
	finalSize := written
	if remoteSize == 0 {
		finalSize = 0
	}

	m.stateMu.Lock()
	// Check if a local dirty write happened while we were downloading; never overwrite dirty entry!
	if ov, ok, _ := m.getOverlayRow(pathLower); ok && ov.Dirty {
		if ce, ceOk, _ := m.getCacheEntry(pathLower); ceOk {
			m.stateMu.Unlock()
			if blobKey != "" {
				removeWithRetry(m.blobPath(blobKey))
			}
			return ce, nil
		}
	}

	var oldBlobKey string
	_ = m.db.QueryRow(`SELECT blob_key FROM cache_entries WHERE path_lower = ?`, pathLower).Scan(&oldBlobKey)

	nowNS := time.Now().UnixNano()
	accessNS := m.nextAccessNS()
	pinned := m.isPinnedLocked(canonicalPath)
	pinnedInt := boolToInt(pinned)

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		if blobKey != "" {
			removeWithRetry(m.blobPath(blobKey))
		}
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO cache_entries (path_lower, path, blob_key, size, mtime, last_accessed_ns, dirty, pinned, remote_node_id, remote_etag, last_op_id, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(path_lower) DO UPDATE SET
			path=excluded.path, blob_key=excluded.blob_key, size=excluded.size, mtime=excluded.mtime,
			last_accessed_ns=excluded.last_accessed_ns, dirty=0, pinned=excluded.pinned,
			remote_node_id=excluded.remote_node_id, remote_etag=excluded.remote_etag, last_op_id=0, updated_at_ns=excluded.updated_at_ns
	`, pathLower, canonicalPath, blobKey, finalSize, rn.Mtime, accessNS, pinnedInt, rn.ID, remoteETag, nowNS, nowNS); err != nil {
		m.stateMu.Unlock()
		if blobKey != "" {
			removeWithRetry(m.blobPath(blobKey))
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		if blobKey != "" {
			removeWithRetry(m.blobPath(blobKey))
		}
		return nil, err
	}

	if err := m.evictToBudgetLocked(); err != nil {
		m.stateMu.Unlock()
		return nil, err
	}
	ce, _, err := m.getCacheEntry(pathLower)
	m.stateMu.Unlock()

	if oldBlobKey != "" && oldBlobKey != blobKey {
		m.scheduleBlobDeletes([]string{oldBlobKey})
	}
	m.notifyChange(canonicalPath)
	if ce == nil {
		ce = &cacheEntry{
			Path:           canonicalPath,
			PathLower:      pathLower,
			BlobKey:        blobKey,
			Size:           finalSize,
			Mtime:          rn.Mtime,
			LastAccessedNS: accessNS,
			Pinned:         pinned,
			RemoteNodeID:   rn.ID,
			RemoteETag:     remoteETag,
		}
	}
	return ce, err
}

// PinSync pre-fetches a pinned file or directory subtree into the disk cache while online.
func (m *Manager) PinSync(ctx context.Context, p string) error {
	if m.backend == nil || !m.backend.IsOnline() {
		return ErrOfflineUncached
	}
	normPath := NormalizePath(p)

	ov, found, err := m.Stat(normPath)
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	m.beginPinSync(ov.Path)
	defer m.endPinSync(ov.Path)
	if !ov.IsDir {
		if ov.Dirty {
			return nil
		}
		_, err := m.ensureCachedSingleflight(ctx, ov.Path)
		return err
	}

	// BFS walk across merged directory tree.
	queue := []string{ov.Path}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur := queue[0]
		queue = queue[1:]
		children, err := m.ListDir(cur)
		if err != nil {
			return err
		}
		for _, ch := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if ch.IsDir {
				queue = append(queue, ch.Path)
				continue
			}
			if ch.Dirty {
				continue
			}
			if _, err := m.ensureCachedSingleflight(ctx, ch.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Ensure path package is used.
var _ = path.Clean
