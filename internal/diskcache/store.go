package diskcache

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type cacheEntry struct {
	Path           string
	PathLower      string
	BlobKey        string
	Size           int64
	Mtime          int64
	LastAccessedNS int64
	Dirty          bool
	Pinned         bool
	RemoteNodeID   int64
	RemoteETag     string
	LastOpID       int64
	CreatedAtNS    int64
	UpdatedAtNS    int64
}

type overlayRow struct {
	Path        string
	PathLower   string
	ParentLower string
	Name        string
	IsDir       bool
	Size        int64
	Mtime       int64
	Ctime       int64
	Deleted     bool
	Dirty       bool
	BlobKey     string
	LastOpID    int64
	UpdatedAtNS int64
}

type queueOpRow struct {
	ID          int64
	OpType      QueueOpType
	Path        string
	NewPath     string
	PayloadFile string
	Size        int64
	Mtime       int64
	IsDir       bool
	Status      string
	CreatedAtNS int64
}

func openCacheDB(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	pragmas := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`PRAGMA temp_store = MEMORY;`,
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA busy_timeout = 10000;`,
		`PRAGMA cache_size = -16000;`,
		`PRAGMA wal_autocheckpoint = 1000;`,
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("apply pragma %s: %w", p, err)
		}
	}

	if err := initCacheSchema(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func initCacheSchema(conn *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS cache_entries (
		path_lower       TEXT PRIMARY KEY,
		path             TEXT NOT NULL,
		blob_key         TEXT NOT NULL DEFAULT '',
		size             INTEGER NOT NULL DEFAULT 0,
		mtime            INTEGER NOT NULL DEFAULT 0,
		last_accessed_ns INTEGER NOT NULL DEFAULT 0,
		dirty            INTEGER NOT NULL DEFAULT 0,
		pinned           INTEGER NOT NULL DEFAULT 0,
		remote_node_id   INTEGER NOT NULL DEFAULT 0,
		remote_etag      TEXT NOT NULL DEFAULT '',
		last_op_id       INTEGER NOT NULL DEFAULT 0,
		created_at_ns    INTEGER NOT NULL DEFAULT 0,
		updated_at_ns    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_cache_entries_lru  ON cache_entries(dirty, pinned, last_accessed_ns ASC);
	CREATE INDEX IF NOT EXISTS idx_cache_entries_blob ON cache_entries(blob_key);

	CREATE TABLE IF NOT EXISTS pinned_paths (
		path_lower TEXT PRIMARY KEY,
		path       TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS cache_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS overlay_nodes (
		path_lower    TEXT PRIMARY KEY,
		path          TEXT NOT NULL,
		parent_lower  TEXT NOT NULL,
		name          TEXT NOT NULL,
		is_dir        INTEGER NOT NULL DEFAULT 0,
		size          INTEGER NOT NULL DEFAULT 0,
		mtime         INTEGER NOT NULL DEFAULT 0,
		ctime         INTEGER NOT NULL DEFAULT 0,
		deleted       INTEGER NOT NULL DEFAULT 0,
		dirty         INTEGER NOT NULL DEFAULT 1,
		blob_key      TEXT NOT NULL DEFAULT '',
		last_op_id    INTEGER NOT NULL DEFAULT 0,
		updated_at_ns INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_overlay_parent ON overlay_nodes(parent_lower);
	CREATE INDEX IF NOT EXISTS idx_overlay_op     ON overlay_nodes(last_op_id);

	CREATE TABLE IF NOT EXISTS write_queue (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		op_type       TEXT NOT NULL,
		path          TEXT NOT NULL,
		new_path      TEXT NOT NULL DEFAULT '',
		payload_file  TEXT NOT NULL DEFAULT '',
		size          INTEGER NOT NULL DEFAULT 0,
		mtime         INTEGER NOT NULL DEFAULT 0,
		is_dir        INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL DEFAULT 'pending',
		created_at_ns INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS node_meta (
		path_lower TEXT PRIMARY KEY,
		path       TEXT NOT NULL,
		mtime      INTEGER NOT NULL DEFAULT 0,
		etag       TEXT NOT NULL DEFAULT ''
	);
	`
	if _, err := conn.Exec(schema); err != nil {
		return fmt.Errorf("init cache schema: %w", err)
	}
	return nil
}

func ensureCacheDirs(cacheDir string) error {
	for _, sub := range []string{"blobs", "queue", "staging"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return nil
}

func newHexID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m *Manager) blobPath(blobKey string) string {
	if len(blobKey) < 2 {
		return filepath.Join(m.cacheDir, "blobs", blobKey+".dat")
	}
	return filepath.Join(m.cacheDir, "blobs", blobKey[:2], blobKey+".dat")
}

func (m *Manager) queuePayloadPath(payloadFile string) string {
	return filepath.Join(m.cacheDir, "queue", payloadFile)
}

func (m *Manager) stagingDir() string {
	return filepath.Join(m.cacheDir, "staging")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func renameWithRetry(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	var lastErr error
	for i := 0; i < 6; i++ {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	// Fallback for cross-volume moves (e.g. stagedPath in a different drive/temp dir).
	if err := copyFileAtomic(src, dst); err != nil {
		return fmt.Errorf("rename (%v) and copy fallback failed: %w", lastErr, err)
	}
	_ = os.Remove(src)
	return nil
}

// linkOrCopyAtomic creates an immutable blob from an immutable queue payload.
// Hard-linking is O(1) with 0 extra disk bytes on NTFS/POSIX; falls back to copying on FAT32/exFAT or cross-volume.
func linkOrCopyAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFileAtomic(src, dst)
}

func copyFileAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(tmp, in, buf); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	for i := 0; i < 6; i++ {
		if err := os.Rename(tmpName, dst); err == nil {
			success = true
			return nil
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	return os.Rename(tmpName, dst)
}

func removeWithRetry(path string) {
	if path == "" {
		return
	}
	for i := 0; i < 5; i++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
}

func (m *Manager) acquireBlobReader(blobKey string) {
	if blobKey == "" {
		return
	}
	m.blobMu.Lock()
	m.activeBlobReaders[blobKey]++
	m.blobMu.Unlock()
}

func (m *Manager) releaseBlobReader(blobKey string) {
	if blobKey == "" {
		return
	}
	m.blobMu.Lock()
	cnt := m.activeBlobReaders[blobKey]
	if cnt <= 1 {
		delete(m.activeBlobReaders, blobKey)
		if m.deferredBlobDeletes[blobKey] {
			delete(m.deferredBlobDeletes, blobKey)
			m.blobMu.Unlock()
			m.deleteBlobIfUnreferenced(blobKey)
			return
		}
	} else {
		m.activeBlobReaders[blobKey] = cnt - 1
	}
	m.blobMu.Unlock()
}

func (m *Manager) scheduleBlobDeletes(blobKeys []string) {
	seen := make(map[string]struct{}, len(blobKeys))
	for _, k := range blobKeys {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		m.deleteBlobIfUnreferenced(k)
	}
}

func (m *Manager) deleteBlobIfUnreferenced(blobKey string) {
	if blobKey == "" {
		return
	}
	m.blobMu.Lock()
	if m.activeBlobReaders[blobKey] > 0 {
		m.deferredBlobDeletes[blobKey] = true
		m.blobMu.Unlock()
		return
	}
	m.blobMu.Unlock()

	var refs int
	if err := m.db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM cache_entries WHERE blob_key = ?) + (SELECT COUNT(*) FROM overlay_nodes WHERE blob_key = ?)`,
		blobKey, blobKey,
	).Scan(&refs); err != nil {
		return
	}
	if refs > 0 {
		return
	}
	removeWithRetry(m.blobPath(blobKey))
}

func (m *Manager) readBlobAt(blobKey string, size int64, buf []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative offset: %d", offset)
	}
	if size == 0 || offset >= size {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	m.acquireBlobReader(blobKey)
	defer m.releaseBlobReader(blobKey)

	f, err := os.Open(m.blobPath(blobKey))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return readFileSliceAt(f, size, buf, offset)
}

func readFileSliceAt(f *os.File, size int64, buf []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative offset: %d", offset)
	}
	if size == 0 || offset >= size {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	want := len(buf)
	if rem := size - offset; int64(want) > rem {
		want = int(rem)
	}
	total := 0
	for total < want {
		n, err := f.ReadAt(buf[total:want], offset+int64(total))
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				if total < len(buf) {
					return total, io.EOF
				}
				return total, nil
			}
			return total, err
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

func (m *Manager) blobValid(blobKey string, size int64) bool {
	if size == 0 {
		return true
	}
	if blobKey == "" {
		return false
	}
	fi, err := os.Stat(m.blobPath(blobKey))
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Size() >= size
}

func (m *Manager) cleanupOnOpen() error {
	// 1. Purge leftover ephemeral staging files.
	if entries, err := os.ReadDir(m.stagingDir()); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(m.stagingDir(), e.Name()))
		}
	}

	// 2. Prune clean cache_entries whose disk blob is missing/corrupt.
	rows, err := m.db.Query(`SELECT path_lower, blob_key, size, dirty FROM cache_entries WHERE size > 0`)
	if err != nil {
		return err
	}
	var missingClean []string
	for rows.Next() {
		var pLower, bKey string
		var sz int64
		var dirty int
		if err := rows.Scan(&pLower, &bKey, &sz, &dirty); err != nil {
			_ = rows.Close()
			return err
		}
		if dirty == 0 && !m.blobValid(bKey, sz) {
			missingClean = append(missingClean, pLower)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(missingClean) > 0 {
		tx, err := m.db.Begin()
		if err == nil {
			for _, pLower := range missingClean {
				_, _ = tx.Exec(`DELETE FROM cache_entries WHERE path_lower = ? AND dirty = 0`, pLower)
			}
			_ = tx.Commit()
		}
	}

	// 3. Collect all referenced blob keys and queue payload files.
	refBlobs := make(map[string]struct{})
	if bRows, err := m.db.Query(`SELECT blob_key FROM cache_entries WHERE blob_key != '' UNION SELECT blob_key FROM overlay_nodes WHERE blob_key != ''`); err == nil {
		for bRows.Next() {
			var k string
			if err := bRows.Scan(&k); err == nil && k != "" {
				refBlobs[k] = struct{}{}
			}
		}
		_ = bRows.Close()
	}
	refPayloads := make(map[string]struct{})
	if qRows, err := m.db.Query(`SELECT payload_file FROM write_queue WHERE payload_file != ''`); err == nil {
		for qRows.Next() {
			var f string
			if err := qRows.Scan(&f); err == nil && f != "" {
				refPayloads[f] = struct{}{}
			}
		}
		_ = qRows.Close()
	}

	// 4. Delete unreferenced blob files.
	blobsRoot := filepath.Join(m.cacheDir, "blobs")
	_ = filepath.WalkDir(blobsRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d == nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".dat") {
			key := strings.TrimSuffix(name, ".dat")
			if _, ok := refBlobs[key]; !ok {
				_ = os.Remove(p)
			}
		} else {
			_ = os.Remove(p)
		}
		return nil
	})

	// 5. Delete unreferenced queue payload files.
	queueRoot := filepath.Join(m.cacheDir, "queue")
	if entries, err := os.ReadDir(queueRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if _, ok := refPayloads[e.Name()]; !ok {
				_ = os.Remove(filepath.Join(queueRoot, e.Name()))
			}
		}
	}

	return nil
}

func (m *Manager) getCacheEntry(pathLower string) (*cacheEntry, bool, error) {
	var e cacheEntry
	var dirtyInt, pinnedInt int
	err := m.db.QueryRow(
		`SELECT path, path_lower, blob_key, size, mtime, last_accessed_ns, dirty, pinned, remote_node_id, remote_etag, last_op_id, created_at_ns, updated_at_ns
		 FROM cache_entries WHERE path_lower = ?`,
		pathLower,
	).Scan(
		&e.Path, &e.PathLower, &e.BlobKey, &e.Size, &e.Mtime, &e.LastAccessedNS,
		&dirtyInt, &pinnedInt, &e.RemoteNodeID, &e.RemoteETag, &e.LastOpID,
		&e.CreatedAtNS, &e.UpdatedAtNS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	e.Dirty = dirtyInt != 0
	e.Pinned = pinnedInt != 0
	return &e, true, nil
}

func (m *Manager) getOverlayRow(pathLower string) (*overlayRow, bool, error) {
	var r overlayRow
	var isDirInt, deletedInt, dirtyInt int
	err := m.db.QueryRow(
		`SELECT path, path_lower, parent_lower, name, is_dir, size, mtime, ctime, deleted, dirty, blob_key, last_op_id, updated_at_ns
		 FROM overlay_nodes WHERE path_lower = ?`,
		pathLower,
	).Scan(
		&r.Path, &r.PathLower, &r.ParentLower, &r.Name, &isDirInt, &r.Size,
		&r.Mtime, &r.Ctime, &deletedInt, &dirtyInt, &r.BlobKey, &r.LastOpID, &r.UpdatedAtNS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	r.IsDir = isDirInt != 0
	r.Deleted = deletedInt != 0
	r.Dirty = dirtyInt != 0
	return &r, true, nil
}

func (m *Manager) getNodeMetaMtime(pathLower string) (mtime int64, etag string, ok bool) {
	err := m.db.QueryRow(`SELECT mtime, etag FROM node_meta WHERE path_lower = ?`, pathLower).Scan(&mtime, &etag)
	if err != nil {
		return 0, "", false
	}
	return mtime, etag, true
}
