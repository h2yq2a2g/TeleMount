package diskcache

import (
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"
)

// NormalizePath converts a virtual path to canonical POSIX form ("/a/b.txt" or "/").
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean("/" + p)
}

func parentOfNormalized(normPath string) string {
	if normPath == "/" {
		return "/"
	}
	dir := path.Dir(normPath)
	if dir == "." || dir == "" {
		return "/"
	}
	return dir
}

// IsPathPinned reports whether target matches any pinned path (exact match or directory prefix).
func IsPathPinned(pinned []string, target string) bool {
	if len(pinned) == 0 {
		return false
	}
	lt := strings.ToLower(NormalizePath(target))
	for _, pin := range pinned {
		if strings.TrimSpace(pin) == "" {
			continue
		}
		lp := strings.ToLower(NormalizePath(pin))
		if lp == "/" || lt == lp || strings.HasPrefix(lt, lp+"/") {
			return true
		}
	}
	return false
}

func (m *Manager) nextAccessNS() int64 {
	now := time.Now().UnixNano()
	for {
		prev := m.lastAccessNS.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if m.lastAccessNS.CompareAndSwap(prev, next) {
			return next
		}
	}
}

func (m *Manager) isPinnedLocked(normPath string) bool {
	return IsPathPinned(m.pinnedPaths, normPath)
}

// SetMaxBytes updates the cache budget and evicts unpinned clean entries if over budget.
func (m *Manager) SetMaxBytes(maxBytes int64) error {
	if maxBytes < 0 {
		return errors.New("maxBytes must be >= 0")
	}
	m.stateMu.Lock()
	m.maxBytes = maxBytes
	err := m.evictToBudgetLocked()
	m.stateMu.Unlock()
	if err == nil {
		m.notifyChange("/")
	}
	return err
}

// SetPinnedPaths replaces the pinned paths set, recomputes entry pin flags, and evicts if over budget.
func (m *Manager) SetPinnedPaths(paths []string) error {
	seen := make(map[string]struct{}, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		norm := NormalizePath(raw)
		lower := strings.ToLower(norm)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		normalized = append(normalized, norm)
	}

	m.stateMu.Lock()
	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM pinned_paths`); err != nil {
		m.stateMu.Unlock()
		return err
	}
	now := time.Now().Unix()
	for _, p := range normalized {
		if _, err := tx.Exec(
			`INSERT INTO pinned_paths (path_lower, path, created_at) VALUES (?, ?, ?)`,
			strings.ToLower(p), p, now,
		); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	rows, err := tx.Query(`SELECT path_lower, path, pinned FROM cache_entries`)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	type pinUpdate struct {
		pathLower string
		pinned    int
	}
	var updates []pinUpdate
	for rows.Next() {
		var pLower, p string
		var curPinned int
		if err := rows.Scan(&pLower, &p, &curPinned); err != nil {
			_ = rows.Close()
			m.stateMu.Unlock()
			return err
		}
		want := boolToInt(IsPathPinned(normalized, p))
		if want != curPinned {
			updates = append(updates, pinUpdate{pathLower: pLower, pinned: want})
		}
	}
	if err := rows.Close(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE cache_entries SET pinned = ? WHERE path_lower = ?`, u.pinned, u.pathLower); err != nil {
			m.stateMu.Unlock()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}

	m.pinnedPaths = normalized
	evErr := m.evictToBudgetLocked()
	m.stateMu.Unlock()
	if evErr != nil {
		return evErr
	}
	m.notifyChange("/")
	return nil
}

// PinnedPaths returns a copy of the configured pinned virtual paths.
func (m *Manager) PinnedPaths() []string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	out := make([]string, len(m.pinnedPaths))
	copy(out, m.pinnedPaths)
	return out
}

func (m *Manager) evictToBudgetLocked() error {
	if m.maxBytes < 0 {
		return nil
	}
	var totalBytes sql.NullInt64
	if err := m.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM cache_entries`).Scan(&totalBytes); err != nil {
		return err
	}
	curTotal := totalBytes.Int64
	if curTotal <= m.maxBytes {
		return nil
	}

	rows, err := m.db.Query(
		`SELECT path_lower, blob_key, size FROM cache_entries WHERE dirty = 0 AND pinned = 0 ORDER BY last_accessed_ns ASC`,
	)
	if err != nil {
		return err
	}
	type candidate struct {
		pathLower string
		blobKey   string
		size      int64
	}
	var toEvict []candidate
	for rows.Next() {
		if curTotal <= m.maxBytes {
			break
		}
		var c candidate
		if err := rows.Scan(&c.pathLower, &c.blobKey, &c.size); err != nil {
			_ = rows.Close()
			return err
		}
		toEvict = append(toEvict, c)
		curTotal -= c.size
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(toEvict) == 0 {
		return nil
	}

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var blobsToDelete []string
	var evictedCount int64
	for _, c := range toEvict {
		res, err := tx.Exec(`DELETE FROM cache_entries WHERE path_lower = ? AND dirty = 0 AND pinned = 0`, c.pathLower)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			evictedCount += n
			if c.blobKey != "" {
				blobsToDelete = append(blobsToDelete, c.blobKey)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if evictedCount > 0 {
		m.evictionCount.Add(evictedCount)
	}
	m.scheduleBlobDeletes(blobsToDelete)
	return nil
}

// ClearUnpinned removes all clean, unpinned entries from the disk cache while preserving pinned and dirty entries.
func (m *Manager) ClearUnpinned() error {
	m.stateMu.Lock()
	rows, err := m.db.Query(`SELECT path_lower, blob_key FROM cache_entries WHERE dirty = 0 AND pinned = 0`)
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	var keys []string
	var paths []string
	for rows.Next() {
		var pLower, bKey string
		if err := rows.Scan(&pLower, &bKey); err != nil {
			_ = rows.Close()
			m.stateMu.Unlock()
			return err
		}
		paths = append(paths, pLower)
		if bKey != "" {
			keys = append(keys, bKey)
		}
	}
	if err := rows.Close(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	if len(paths) == 0 {
		m.stateMu.Unlock()
		return nil
	}

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM cache_entries WHERE dirty = 0 AND pinned = 0`); err != nil {
		m.stateMu.Unlock()
		return err
	}
	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return err
	}
	m.stateMu.Unlock()
	m.scheduleBlobDeletes(keys)
	m.notifyChange("/")
	return nil
}

// OverlayState represents the OneDrive-style Explorer sync/offline icon overlay for a virtual path.
type OverlayState byte

const (
	OverlayNone    OverlayState = 'N' // drive root "/" or non-existent path
	OverlayPinned  OverlayState = 'P' // always keep offline on device (solid green checkmark)
	OverlayCached  OverlayState = 'C' // available offline in local disk cache (hollow green checkmark)
	OverlayCloud   OverlayState = 'O' // online-only on Telegram Cloud (blue cloud)
	OverlaySyncing OverlayState = 'S' // pending upload / dirty write / pinned file still downloading
)

// OverlayState returns the OneDrive-style status icon state for a file or folder path.
func (m *Manager) OverlayState(virtualPath string) OverlayState {
	norm := NormalizePath(virtualPath)
	if norm == "/" {
		return OverlayNone
	}

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	ov, found, err := m.statLocked(norm)
	if err != nil || !found {
		return OverlayNone
	}
	pathLower := strings.ToLower(ov.Path)
	pinned := m.isPinnedLocked(ov.Path)

	if !ov.IsDir {
		if ov.Dirty {
			return OverlaySyncing
		}
		ce, ceOk, _ := m.getCacheEntry(pathLower)
		if ceOk && ce.Dirty {
			return OverlaySyncing
		}
		cached := ceOk && !ce.Dirty && m.blobValid(ce.BlobKey, ce.Size)
		if pinned {
			if cached {
				return OverlayPinned
			}
			return OverlaySyncing
		}
		if cached {
			return OverlayCached
		}
		return OverlayCloud
	}

	if ov.Dirty {
		return OverlaySyncing
	}
	pStart, pEnd := subtreeBounds(pathLower)
	var dirtyOverlay int
	_ = m.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM overlay_nodes WHERE dirty = 1 AND deleted = 0 AND path_lower >= ? AND path_lower < ?)`,
		pStart, pEnd,
	).Scan(&dirtyOverlay)
	if dirtyOverlay != 0 {
		return OverlaySyncing
	}

	var cachedCnt, dirtyCnt sql.NullInt64
	_ = m.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN dirty = 1 THEN 1 ELSE 0 END), 0)
		FROM cache_entries
		WHERE path_lower >= ? AND path_lower < ?
	`, pStart, pEnd).Scan(&cachedCnt, &dirtyCnt)
	if dirtyCnt.Int64 > 0 || m.isPinSyncing(ov.Path) {
		return OverlaySyncing
	}
	if pinned {
		return OverlayPinned
	}
	if cachedCnt.Int64 > 0 {
		return OverlayCached
	}
	for _, pin := range m.pinnedPaths {
		if strings.HasPrefix(strings.ToLower(pin), pathLower+"/") {
			return OverlayCached
		}
	}
	return OverlayCloud
}

// PathCacheStatus summarizes local disk cache and pin state for a file or folder subtree.
type PathCacheStatus struct {
	Path        string
	Pinned      bool
	CachedFiles int
	CachedBytes int64
	DirtyFiles  int
}

// PathStatus returns cache and pin statistics for virtualPath (file, folder subtree, or "/").
func (m *Manager) PathStatus(virtualPath string) (PathCacheStatus, error) {
	norm := NormalizePath(virtualPath)
	pathLower := strings.ToLower(norm)

	m.stateMu.Lock()
	pinned := m.isPinnedLocked(norm)
	m.stateMu.Unlock()

	var cnt, total, dirty sql.NullInt64
	var err error
	if norm == "/" {
		err = m.db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(CASE WHEN dirty = 1 THEN 1 ELSE 0 END), 0)
			FROM cache_entries
		`).Scan(&cnt, &total, &dirty)
	} else {
		pStart, pEnd := subtreeBounds(pathLower)
		err = m.db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(SUM(CASE WHEN dirty = 1 THEN 1 ELSE 0 END), 0)
			FROM cache_entries
			WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)
		`, pathLower, pStart, pEnd).Scan(&cnt, &total, &dirty)
	}
	if err != nil {
		return PathCacheStatus{}, err
	}
	return PathCacheStatus{
		Path:        norm,
		Pinned:      pinned,
		CachedFiles: int(cnt.Int64),
		CachedBytes: total.Int64,
		DirtyFiles:  int(dirty.Int64),
	}, nil
}

// EvictPath removes local cached blobs for a file or folder subtree while keeping cloud data intact.
// Dirty (unsynced offline write) entries are preserved and counted in skippedDirty to prevent data loss.
func (m *Manager) EvictPath(virtualPath string) (freedBytes int64, evictedFiles int, skippedDirty int, err error) {
	norm := NormalizePath(virtualPath)
	pathLower := strings.ToLower(norm)

	m.stateMu.Lock()
	var rows *sql.Rows
	if norm == "/" {
		rows, err = m.db.Query(`SELECT path_lower, blob_key, size, dirty FROM cache_entries`)
	} else {
		pStart, pEnd := subtreeBounds(pathLower)
		rows, err = m.db.Query(
			`SELECT path_lower, blob_key, size, dirty FROM cache_entries WHERE path_lower = ? OR (path_lower >= ? AND path_lower < ?)`,
			pathLower, pStart, pEnd,
		)
	}
	if err != nil {
		m.stateMu.Unlock()
		return 0, 0, 0, err
	}

	var cleanPaths []string
	var blobKeys []string
	for rows.Next() {
		var pLower, bKey string
		var sz int64
		var dirtyInt int
		if scanErr := rows.Scan(&pLower, &bKey, &sz, &dirtyInt); scanErr != nil {
			_ = rows.Close()
			m.stateMu.Unlock()
			return 0, 0, 0, scanErr
		}
		if dirtyInt != 0 {
			skippedDirty++
			continue
		}
		cleanPaths = append(cleanPaths, pLower)
		freedBytes += sz
		if bKey != "" {
			blobKeys = append(blobKeys, bKey)
		}
	}
	if closeErr := rows.Close(); closeErr != nil {
		m.stateMu.Unlock()
		return 0, 0, 0, closeErr
	}
	if len(cleanPaths) == 0 {
		m.stateMu.Unlock()
		return 0, 0, skippedDirty, nil
	}

	tx, err := m.db.Begin()
	if err != nil {
		m.stateMu.Unlock()
		return 0, 0, skippedDirty, err
	}
	defer tx.Rollback()

	for _, pLower := range cleanPaths {
		res, execErr := tx.Exec(`DELETE FROM cache_entries WHERE path_lower = ? AND dirty = 0`, pLower)
		if execErr != nil {
			m.stateMu.Unlock()
			return 0, 0, skippedDirty, execErr
		}
		if n, _ := res.RowsAffected(); n > 0 {
			evictedFiles += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		m.stateMu.Unlock()
		return 0, 0, skippedDirty, err
	}
	m.stateMu.Unlock()
	m.scheduleBlobDeletes(blobKeys)
	if evictedFiles > 0 {
		m.notifyChange(norm)
	}
	return freedBytes, evictedFiles, skippedDirty, nil
}

// Stats returns current disk cache usage statistics.
func (m *Manager) Stats() CacheStats {
	m.stateMu.Lock()
	maxBytes := m.maxBytes
	m.stateMu.Unlock()

	var stats CacheStats
	stats.MaxBytes = maxBytes
	stats.EvictionCount = m.evictionCount.Load()

	var total, pinned, unpinned sql.NullInt64
	var cnt, pinnedCnt sql.NullInt64
	_ = m.db.QueryRow(`
		SELECT
			COALESCE(SUM(size), 0),
			COALESCE(SUM(CASE WHEN pinned = 1 THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pinned = 0 THEN size ELSE 0 END), 0),
			COUNT(*),
			COALESCE(SUM(CASE WHEN pinned = 1 THEN 1 ELSE 0 END), 0)
		FROM cache_entries
	`).Scan(&total, &pinned, &unpinned, &cnt, &pinnedCnt)

	stats.TotalBytes = total.Int64
	stats.PinnedBytes = pinned.Int64
	stats.UnpinnedBytes = unpinned.Int64
	stats.EntryCount = int(cnt.Int64)
	stats.PinnedEntryCount = int(pinnedCnt.Int64)
	return stats
}
