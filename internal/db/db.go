package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrAlreadyExist = errors.New("already exists")
	ErrNotDir       = errors.New("not a directory")
	ErrInvalidMove  = errors.New("cannot move directory into itself")
	ErrInvalidName  = errors.New("invalid name")
)

const RootID int64 = 1

type Node struct {
	ID           int64  `json:"id"`
	ParentID     *int64 `json:"parent_id"`
	Name         string `json:"name"`
	IsDir        bool   `json:"is_dir"`
	Size         int64  `json:"size"`
	MimeType     string `json:"mime_type"`
	ETag         string `json:"etag"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
	DeletedAt    *int64 `json:"deleted_at,omitempty"`
	OrigParentID *int64 `json:"orig_parent_id,omitempty"`
	OrigName     string `json:"orig_name,omitempty"`
}

type NodeWithPath struct {
	Node
	Path string `json:"path"`
}

type FilePart struct {
	ID           int64  `json:"id"`
	NodeID       int64  `json:"node_id"`
	PartIndex    int    `json:"part_index"`
	TGMsgID      int    `json:"tg_msg_id"`
	TGFileID     int64  `json:"tg_file_id"`
	TGAccessHash int64  `json:"tg_access_hash"`
	TGFileRef    []byte `json:"-"`
	TGDCID       int    `json:"tg_dc_id"`
	PlainSize    int64  `json:"plain_size"`
	CipherSize   int64  `json:"cipher_size"`
	PartHeader   []byte `json:"-"`
	PackID       string `json:"pack_id,omitempty"`
	PackOffset   int64  `json:"pack_offset,omitempty"`
}

type PackInfo struct {
	PackID    string `json:"pack_id"`
	TGMsgID   int    `json:"tg_msg_id"`
	TotalSize int64  `json:"total_size"`
	LiveSize  int64  `json:"live_size"`
	DeadSize  int64  `json:"dead_size"`
	LiveFiles int64  `json:"live_files"`
	CreatedAt int64  `json:"created_at"`
}

type PackSummary struct {
	PackCount      int64 `json:"pack_count"`
	TotalPackBytes int64 `json:"total_pack_bytes"`
	LivePackBytes  int64 `json:"live_pack_bytes"`
	DeadPackBytes  int64 `json:"dead_pack_bytes"`
}

type RelocatedPart struct {
	PartID     int64
	NewOffset  int64
}

type BlobReplica struct {
	AccountID    string `json:"account_id"`
	ChannelID    int64  `json:"channel_id"`
	PrimaryMsgID int    `json:"primary_msg_id"`
	ReplicaMsgID int    `json:"replica_msg_id"`
	TGFileID     int64  `json:"tg_file_id"`
	TGAccessHash int64  `json:"tg_access_hash"`
	TGFileRef    []byte `json:"-"`
	TGDCID       int    `json:"tg_dc_id"`
	CreatedAt    int64  `json:"created_at"`
}

type Settings struct {
	PartSizeMB        int    `json:"part_size_mb"`
	SmallFileMaxMB    int    `json:"small_file_max_mb"`
	PackMaxMB         int    `json:"pack_max_mb"`
	PackIdleSec       int    `json:"pack_idle_sec"`
	RAMCacheMB        int    `json:"ram_cache_mb"`
	BackupIntervalMin int    `json:"backup_interval_min"`
	UploadThreads     int    `json:"upload_threads"`
	WebDAVUser        string `json:"webdav_user"`
	HasWebDAVPass     bool   `json:"has_webdav_pass"`
	LastBackupAt      int64  `json:"last_backup_at"`
	LastBackupMsgID   int    `json:"last_backup_msg_id"`
}

type Stats struct {
	FileCount      int64       `json:"file_count"`
	FolderCount    int64       `json:"folder_count"`
	TotalPlainSize int64       `json:"total_plain_size"`
	TotalCipher    int64       `json:"total_cipher_size"`
	PartCount      int64       `json:"part_count"`
	TrashCount     int64       `json:"trash_count"`
	TrashSize      int64       `json:"trash_size"`
	Packs          PackSummary `json:"packs"`
}

type Store struct {
	mu         sync.RWMutex
	dbPath     string
	conn       *sql.DB
	lastChange atomic.Int64
}

func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	conn, err := openConn(dbPath)
	if err != nil {
		return nil, err
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}
	s := &Store{dbPath: dbPath, conn: conn}
	s.lastChange.Store(time.Now().Unix())
	return s, nil
}

func openConn(dbPath string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// Single connection eliminates SQLITE_BUSY under concurrent WebDAV + Web UI requests.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 10000;",
		"PRAGMA cache_size = -16000;",
		"PRAGMA wal_autocheckpoint = 1000;",
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			conn.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}
	return conn, nil
}

func migrate(conn *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	parent_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	is_dir INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	mime_type TEXT NOT NULL DEFAULT '',
	etag TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	deleted_at INTEGER,
	orig_parent_id INTEGER,
	orig_name TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_active_sibling
	ON nodes(parent_id, name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_nodes_parent_del ON nodes(parent_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_nodes_deleted ON nodes(deleted_at);
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);

CREATE TABLE IF NOT EXISTS file_parts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	part_index INTEGER NOT NULL,
	tg_msg_id INTEGER NOT NULL,
	tg_file_id INTEGER NOT NULL,
	tg_access_hash INTEGER NOT NULL,
	tg_file_ref BLOB,
	tg_dc_id INTEGER NOT NULL,
	plain_size INTEGER NOT NULL,
	cipher_size INTEGER NOT NULL,
	part_header BLOB NOT NULL,
	pack_id TEXT NOT NULL DEFAULT '',
	pack_offset INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_file_parts_node_idx ON file_parts(node_id, part_index);
CREATE INDEX IF NOT EXISTS idx_file_parts_msg ON file_parts(tg_msg_id);

CREATE TABLE IF NOT EXISTS packs (
	pack_id TEXT PRIMARY KEY,
	tg_msg_id INTEGER NOT NULL DEFAULT 0,
	total_size INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_packs_msg ON packs(tg_msg_id);

CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_deletes (
	tg_msg_id INTEGER PRIMARY KEY,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS blob_replicas (
	account_id TEXT NOT NULL,
	channel_id INTEGER NOT NULL DEFAULT 0,
	primary_msg_id INTEGER NOT NULL,
	replica_msg_id INTEGER NOT NULL,
	tg_file_id INTEGER NOT NULL,
	tg_access_hash INTEGER NOT NULL,
	tg_file_ref BLOB,
	tg_dc_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, primary_msg_id)
);
CREATE INDEX IF NOT EXISTS idx_blob_replicas_primary ON blob_replicas(primary_msg_id);
CREATE INDEX IF NOT EXISTS idx_blob_replicas_channel ON blob_replicas(channel_id);

CREATE TABLE IF NOT EXISTS pending_replica_deletes (
	account_id TEXT NOT NULL,
	replica_msg_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (account_id, replica_msg_id)
);

CREATE TABLE IF NOT EXISTS upload_checkpoints (
	upload_key TEXT NOT NULL,
	part_index INTEGER NOT NULL,
	tg_msg_id INTEGER NOT NULL,
	tg_file_id INTEGER NOT NULL,
	tg_access_hash INTEGER NOT NULL,
	tg_file_ref BLOB,
	tg_dc_id INTEGER NOT NULL,
	plain_size INTEGER NOT NULL,
	cipher_size INTEGER NOT NULL,
	part_header BLOB NOT NULL,
	total_size INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (upload_key, part_index)
);
CREATE INDEX IF NOT EXISTS idx_upload_checkpoints_msg ON upload_checkpoints(tg_msg_id);
`
	if _, err := conn.Exec(schema); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}

	// Backward-compatible column upgrades for existing databases.
	_, _ = conn.Exec(`ALTER TABLE file_parts ADD COLUMN pack_id TEXT NOT NULL DEFAULT ''`)
	_, _ = conn.Exec(`ALTER TABLE file_parts ADD COLUMN pack_offset INTEGER NOT NULL DEFAULT 0`)
	_, _ = conn.Exec(`CREATE INDEX IF NOT EXISTS idx_file_parts_pack ON file_parts(pack_id)`)

	now := time.Now().Unix()
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO nodes (id, parent_id, name, is_dir, size, mime_type, etag, created_at, updated_at)
		VALUES (1, NULL, '', 1, 0, '', 'root', ?, ?)`, now, now); err != nil {
		return err
	}

	defaults := map[string]string{
		"part_size_mb":        "1024",
		"small_file_max_mb":   "128",
		"pack_max_mb":         "1024",
		"pack_idle_sec":       "10",
		"ram_cache_mb":        "2048",
		"backup_interval_min": "60",
		"upload_threads":      "4",
		"webdav_user":         "",
		"webdav_pass_hash":    "",
		"last_backup_at":      "0",
		"last_backup_msg_id":  "0",
	}
	for k, v := range defaults {
		if _, err := conn.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Store) LastChangeUnix() int64 {
	return s.lastChange.Load()
}

func (s *Store) markChanged() {
	s.lastChange.Store(time.Now().Unix())
}

func newETag() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return ErrInvalidName
	}
	return nil
}

func cleanVirtualPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean("/" + p)
	return p
}

func scanNode(row interface{ Scan(dest ...any) error }) (*Node, error) {
	var n Node
	var parentID, deletedAt, origParentID sql.NullInt64
	var isDir int
	err := row.Scan(
		&n.ID, &parentID, &n.Name, &isDir, &n.Size, &n.MimeType, &n.ETag,
		&n.CreatedAt, &n.UpdatedAt, &deletedAt, &origParentID, &n.OrigName,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n.IsDir = isDir != 0
	if parentID.Valid {
		v := parentID.Int64
		n.ParentID = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Int64
		n.DeletedAt = &v
	}
	if origParentID.Valid {
		v := origParentID.Int64
		n.OrigParentID = &v
	}
	return &n, nil
}

const nodeCols = `id, parent_id, name, is_dir, size, mime_type, etag, created_at, updated_at, deleted_at, orig_parent_id, orig_name`

func (s *Store) GetNode(id int64) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

func (s *Store) StatByPath(p string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statByPathLocked(p)
}

func (s *Store) statByPathLocked(p string) (*Node, error) {
	clean := cleanVirtualPath(p)
	if clean == "/" {
		return scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, RootID))
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	curID := RootID
	var cur *Node
	for _, seg := range parts {
		n, err := scanNode(s.conn.QueryRow(
			`SELECT `+nodeCols+` FROM nodes WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
			curID, seg,
		))
		if err != nil {
			return nil, err
		}
		cur = n
		curID = n.ID
	}
	return cur, nil
}

func (s *Store) ResolveParent(p string) (*Node, string, error) {
	clean := cleanVirtualPath(p)
	if clean == "/" {
		return nil, "", ErrInvalidName
	}
	dir, base := path.Split(clean)
	if err := validateName(base); err != nil {
		return nil, "", err
	}
	parent, err := s.StatByPath(dir)
	if err != nil {
		return nil, "", err
	}
	if !parent.IsDir {
		return nil, "", ErrNotDir
	}
	return parent, base, nil
}

func (s *Store) NodePath(id int64) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodePathLocked(id)
}

func (s *Store) nodePathLocked(id int64) (string, error) {
	if id == RootID {
		return "/", nil
	}
	var segs []string
	cur := id
	for cur != RootID {
		var name string
		var parentID sql.NullInt64
		err := s.conn.QueryRow(`SELECT name, parent_id FROM nodes WHERE id = ?`, cur).Scan(&name, &parentID)
		if err != nil {
			return "", err
		}
		segs = append(segs, name)
		if !parentID.Valid {
			break
		}
		cur = parentID.Int64
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/"), nil
}

func (s *Store) ListChildren(parentID int64) ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(
		`SELECT `+nodeCols+` FROM nodes WHERE parent_id = ? AND deleted_at IS NULL ORDER BY is_dir DESC, name COLLATE NOCASE ASC`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Store) Mkdir(parentID int64, name string) (*Node, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mkdirLocked(parentID, name)
}

func (s *Store) mkdirLocked(parentID int64, name string) (*Node, error) {
	parent, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, parentID))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir {
		return nil, ErrNotDir
	}
	now := time.Now().Unix()
	etag := newETag()
	res, err := s.conn.Exec(
		`INSERT INTO nodes (parent_id, name, is_dir, size, mime_type, etag, created_at, updated_at) VALUES (?, ?, 1, 0, '', ?, ?, ?)`,
		parentID, name, etag, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrAlreadyExist
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.markChanged()
	return scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

func (s *Store) MkdirAll(p string) (*Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := cleanVirtualPath(p)
	if clean == "/" {
		return scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, RootID))
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	curID := RootID
	var cur *Node
	for _, seg := range parts {
		if err := validateName(seg); err != nil {
			return nil, err
		}
		n, err := scanNode(s.conn.QueryRow(
			`SELECT `+nodeCols+` FROM nodes WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
			curID, seg,
		))
		if errors.Is(err, ErrNotFound) {
			n, err = s.mkdirLocked(curID, seg)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else if !n.IsDir {
			return nil, ErrNotDir
		}
		cur = n
		curID = n.ID
	}
	return cur, nil
}

// releaseUnreferencedMsgIDsTx checks candidate Telegram message IDs after file_parts deletion
// and queues ONLY those messages that have zero remaining file_parts referencing them (protecting shared Hybrid Packs).
func releaseUnreferencedMsgIDsTx(tx *sql.Tx, candidates []int, now int64) ([]int, error) {
	seen := make(map[int]struct{})
	var releasable []int
	for _, mid := range candidates {
		if mid <= 0 {
			continue
		}
		if _, dup := seen[mid]; dup {
			continue
		}
		seen[mid] = struct{}{}

		var refCount int
		if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM file_parts WHERE tg_msg_id = ?) + (SELECT COUNT(*) FROM upload_checkpoints WHERE tg_msg_id = ?)`, mid, mid).Scan(&refCount); err != nil {
			return nil, err
		}
		if refCount == 0 {
			releasable = append(releasable, mid)
			if _, err := tx.Exec(`DELETE FROM packs WHERE tg_msg_id = ?`, mid); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO pending_deletes (tg_msg_id, created_at) VALUES (?, ?)`, mid, now); err != nil {
				return nil, err
			}
			// Queue corresponding mirrored blobs on backup accounts for cleanup too.
			if _, err := tx.Exec(`
				INSERT OR IGNORE INTO pending_replica_deletes (account_id, replica_msg_id, created_at)
				SELECT account_id, replica_msg_id, ? FROM blob_replicas WHERE primary_msg_id = ?`,
				now, mid,
			); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`DELETE FROM blob_replicas WHERE primary_msg_id = ?`, mid); err != nil {
				return nil, err
			}
		}
	}
	return releasable, nil
}

// UpsertFileWithParts atomically creates or replaces a file node and its parts.
// Only Telegram message IDs with zero remaining references are returned for deletion.
func (s *Store) UpsertFileWithParts(parentID int64, name string, size int64, mimeType string, parts []FilePart) (*Node, []int, error) {
	if err := validateName(name); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	parent, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, parentID))
	if err != nil {
		return nil, nil, err
	}
	if !parent.IsDir {
		return nil, nil, ErrNotDir
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	etag := newETag()
	var candidateMsgIDs []int
	var nodeID int64

	existing, err := scanNode(tx.QueryRow(
		`SELECT `+nodeCols+` FROM nodes WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
		parentID, name,
	))
	if err == nil {
		if existing.IsDir {
			return nil, nil, ErrAlreadyExist
		}
		rows, err := tx.Query(`SELECT tg_msg_id FROM file_parts WHERE node_id = ? AND tg_msg_id > 0`, existing.ID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var mid int
			if err := rows.Scan(&mid); err != nil {
				rows.Close()
				return nil, nil, err
			}
			candidateMsgIDs = append(candidateMsgIDs, mid)
		}
		rows.Close()

		if _, err := tx.Exec(`DELETE FROM file_parts WHERE node_id = ?`, existing.ID); err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(
			`UPDATE nodes SET size = ?, mime_type = ?, etag = ?, updated_at = ? WHERE id = ?`,
			size, mimeType, etag, now, existing.ID,
		); err != nil {
			return nil, nil, err
		}
		nodeID = existing.ID
	} else if errors.Is(err, ErrNotFound) {
		res, err := tx.Exec(
			`INSERT INTO nodes (parent_id, name, is_dir, size, mime_type, etag, created_at, updated_at) VALUES (?, ?, 0, ?, ?, ?, ?, ?)`,
			parentID, name, size, mimeType, etag, now, now,
		)
		if err != nil {
			return nil, nil, err
		}
		nodeID, _ = res.LastInsertId()
	} else {
		return nil, nil, err
	}

	for i, p := range parts {
		if _, err := tx.Exec(`
			INSERT INTO file_parts (node_id, part_index, tg_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, plain_size, cipher_size, part_header, pack_id, pack_offset)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, i, p.TGMsgID, p.TGFileID, p.TGAccessHash, p.TGFileRef, p.TGDCID, p.PlainSize, p.CipherSize, p.PartHeader, p.PackID, p.PackOffset,
		); err != nil {
			return nil, nil, err
		}
	}

	releasable, err := releaseUnreferencedMsgIDsTx(tx, candidateMsgIDs, now)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	s.markChanged()
	node, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, nodeID))
	return node, releasable, err
}

func (s *Store) GetFileParts(nodeID int64) ([]FilePart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		SELECT id, node_id, part_index, tg_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, plain_size, cipher_size, part_header, pack_id, pack_offset
		FROM file_parts WHERE node_id = ? ORDER BY part_index ASC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FilePart, 0)
	for rows.Next() {
		var p FilePart
		if err := rows.Scan(
			&p.ID, &p.NodeID, &p.PartIndex, &p.TGMsgID, &p.TGFileID, &p.TGAccessHash,
			&p.TGFileRef, &p.TGDCID, &p.PlainSize, &p.CipherSize, &p.PartHeader, &p.PackID, &p.PackOffset,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePartFileRef(partID int64, fileRef []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`UPDATE file_parts SET tg_file_ref = ? WHERE id = ?`, fileRef, partID)
	return err
}

func (s *Store) UpdateMsgFileRef(tgMsgID int, fileRef []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`UPDATE file_parts SET tg_file_ref = ? WHERE tg_msg_id = ?`, fileRef, tgMsgID)
	return err
}

// FinalizePackUpload attaches Telegram document metadata to all file_parts belonging to packID and records the Pack.
// Returns the number of live file_parts bound to this pack (0 if every file in the pack was deleted before flush).
func (s *Store) FinalizePackUpload(packID string, msgID int, fileID, accessHash int64, fileRef []byte, dcID int, totalSize int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE file_parts
		SET tg_msg_id = ?, tg_file_id = ?, tg_access_hash = ?, tg_file_ref = ?, tg_dc_id = ?
		WHERE pack_id = ?`,
		msgID, fileID, accessHash, fileRef, dcID, packID,
	)
	if err != nil {
		return 0, err
	}
	bound, _ := res.RowsAffected()
	now := time.Now().Unix()
	if bound > 0 {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO packs (pack_id, tg_msg_id, total_size, created_at)
			VALUES (?, ?, ?, ?)`,
			packID, msgID, totalSize, now,
		); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO pending_deletes (tg_msg_id, created_at) VALUES (?, ?)`, msgID, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.markChanged()
	return bound, nil
}

func (s *Store) CountLivePartsInPack(packID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	err := s.conn.QueryRow(`SELECT COUNT(*) FROM file_parts WHERE pack_id = ?`, packID).Scan(&n)
	return n, err
}

func (s *Store) ListPacks() ([]PackInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		SELECT p.pack_id, p.tg_msg_id, p.total_size,
		       COALESCE(SUM(fp.cipher_size), 0) AS live_size,
		       COUNT(fp.id) AS live_files,
		       p.created_at
		FROM packs p
		LEFT JOIN file_parts fp ON fp.pack_id = p.pack_id
		WHERE p.tg_msg_id > 0
		GROUP BY p.pack_id
		ORDER BY p.created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackInfo
	for rows.Next() {
		var pi PackInfo
		if err := rows.Scan(&pi.PackID, &pi.TGMsgID, &pi.TotalSize, &pi.LiveSize, &pi.LiveFiles, &pi.CreatedAt); err != nil {
			return nil, err
		}
		if pi.TotalSize > pi.LiveSize {
			pi.DeadSize = pi.TotalSize - pi.LiveSize
		}
		out = append(out, pi)
	}
	return out, rows.Err()
}

func (s *Store) ListPartsByPack(packID string) ([]FilePart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		SELECT id, node_id, part_index, tg_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, plain_size, cipher_size, part_header, pack_id, pack_offset
		FROM file_parts WHERE pack_id = ? ORDER BY pack_offset ASC`, packID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FilePart
	for rows.Next() {
		var p FilePart
		if err := rows.Scan(
			&p.ID, &p.NodeID, &p.PartIndex, &p.TGMsgID, &p.TGFileID, &p.TGAccessHash,
			&p.TGFileRef, &p.TGDCID, &p.PlainSize, &p.CipherSize, &p.PartHeader, &p.PackID, &p.PackOffset,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CommitCompactedPack atomically repoints relocated file_parts to a newly uploaded compacted Pack
// and marks any emptied source packs for Telegram deletion.
func (s *Store) CommitCompactedPack(newPackID string, msgID int, fileID, accessHash int64, fileRef []byte, dcID int, totalSize int64, relocated []RelocatedPart, sourcePackIDs []string) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	var updated int64
	for _, rp := range relocated {
		res, err := tx.Exec(`
			UPDATE file_parts
			SET pack_id = ?, pack_offset = ?, tg_msg_id = ?, tg_file_id = ?, tg_access_hash = ?, tg_file_ref = ?, tg_dc_id = ?
			WHERE id = ?`,
			newPackID, rp.NewOffset, msgID, fileID, accessHash, fileRef, dcID, rp.PartID,
		)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		updated += n
	}

	if updated > 0 {
		if _, err := tx.Exec(`INSERT INTO packs (pack_id, tg_msg_id, total_size, created_at) VALUES (?, ?, ?, ?)`,
			newPackID, msgID, totalSize, now); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO pending_deletes (tg_msg_id, created_at) VALUES (?, ?)`, msgID, now); err != nil {
			return nil, err
		}
	}

	var oldMsgs []int
	for _, spID := range sourcePackIDs {
		var oldMsg int
		if err := tx.QueryRow(`SELECT tg_msg_id FROM packs WHERE pack_id = ?`, spID).Scan(&oldMsg); err == nil && oldMsg > 0 {
			oldMsgs = append(oldMsgs, oldMsg)
		}
	}
	releasable, err := releaseUnreferencedMsgIDsTx(tx, oldMsgs, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.markChanged()
	return releasable, nil
}

func (s *Store) RenameOrMove(nodeID, newParentID int64, newName string) ([]int, error) {
	if nodeID == RootID {
		return nil, ErrInvalidMove
	}
	if err := validateName(newName); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	node, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID))
	if err != nil {
		return nil, err
	}
	dstParent, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, newParentID))
	if err != nil {
		return nil, err
	}
	if !dstParent.IsDir {
		return nil, ErrNotDir
	}

	if node.IsDir {
		cur := newParentID
		for cur != RootID {
			if cur == nodeID {
				return nil, ErrInvalidMove
			}
			var p sql.NullInt64
			if err := s.conn.QueryRow(`SELECT parent_id FROM nodes WHERE id = ?`, cur).Scan(&p); err != nil {
				return nil, err
			}
			if !p.Valid {
				break
			}
			cur = p.Int64
		}
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	var replacedMsgIDs []int
	if existing, err := scanNode(tx.QueryRow(
		`SELECT `+nodeCols+` FROM nodes WHERE parent_id = ? AND name = ? AND deleted_at IS NULL AND id != ?`,
		newParentID, newName, nodeID,
	)); err == nil {
		mids, err := collectSubtreeMsgIDsTx(tx, existing.ID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, existing.ID); err != nil {
			return nil, err
		}
		replacedMsgIDs, err = releaseUnreferencedMsgIDsTx(tx, mids, now)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE nodes SET parent_id = ?, name = ?, etag = ?, updated_at = ? WHERE id = ?`,
		newParentID, newName, newETag(), now, nodeID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.markChanged()
	return replacedMsgIDs, nil
}

func (s *Store) SoftDelete(nodeID int64) error {
	if nodeID == RootID {
		return ErrInvalidMove
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	node, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID))
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	var origParent any
	if node.ParentID != nil {
		origParent = *node.ParentID
	}
	_, err = s.conn.Exec(
		`UPDATE nodes SET deleted_at = ?, orig_parent_id = ?, orig_name = ?, updated_at = ? WHERE id = ?`,
		now, origParent, node.Name, now, nodeID,
	)
	if err == nil {
		s.markChanged()
	}
	return err
}

func (s *Store) ListTrash() ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(
		`SELECT `+nodeCols+` FROM nodes WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Store) RestoreFromTrash(nodeID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, err := scanNode(s.conn.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ? AND deleted_at IS NOT NULL`, nodeID))
	if err != nil {
		return err
	}

	targetParent := RootID
	if node.OrigParentID != nil {
		if ok, err := s.isActiveNodeLocked(*node.OrigParentID); err == nil && ok {
			targetParent = *node.OrigParentID
		}
	}

	targetName := node.OrigName
	if targetName == "" {
		targetName = node.Name
	}

	candidate := targetName
	for i := 1; ; i++ {
		var exists int
		err := s.conn.QueryRow(
			`SELECT 1 FROM nodes WHERE parent_id = ? AND name = ? AND deleted_at IS NULL`,
			targetParent, candidate,
		).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		ext := path.Ext(targetName)
		base := strings.TrimSuffix(targetName, ext)
		candidate = fmt.Sprintf("%s (restored %d)%s", base, i, ext)
	}

	now := time.Now().Unix()
	_, err = s.conn.Exec(
		`UPDATE nodes SET parent_id = ?, name = ?, deleted_at = NULL, orig_parent_id = NULL, orig_name = '', etag = ?, updated_at = ? WHERE id = ?`,
		targetParent, candidate, newETag(), now, nodeID,
	)
	if err == nil {
		s.markChanged()
	}
	return err
}

func (s *Store) isActiveNodeLocked(id int64) (bool, error) {
	if id == RootID {
		return true, nil
	}
	cur := id
	for cur != RootID {
		var parentID, deletedAt sql.NullInt64
		var isDir int
		err := s.conn.QueryRow(`SELECT parent_id, is_dir, deleted_at FROM nodes WHERE id = ?`, cur).Scan(&parentID, &isDir, &deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if deletedAt.Valid || (cur == id && isDir == 0) {
			return false, nil
		}
		if !parentID.Valid {
			return false, nil
		}
		cur = parentID.Int64
	}
	return true, nil
}

// PermanentDelete removes a node and its entire subtree from SQLite and queues only unreferenced Telegram message IDs.
func (s *Store) PermanentDelete(nodeID int64) ([]int, error) {
	if nodeID == RootID {
		return nil, ErrInvalidMove
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	candidates, err := collectSubtreeMsgIDsTx(tx, nodeID)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return nil, ErrNotFound
	}
	releasable, err := releaseUnreferencedMsgIDsTx(tx, candidates, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.markChanged()
	return releasable, nil
}

func (s *Store) EmptyTrash() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.Query(`SELECT id FROM nodes WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var candidates []int
	for _, id := range ids {
		mids, err := collectSubtreeMsgIDsTx(tx, id)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, mids...)
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	releasable, err := releaseUnreferencedMsgIDsTx(tx, candidates, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.markChanged()
	return releasable, nil
}

func collectSubtreeMsgIDsTx(tx *sql.Tx, rootID int64) ([]int, error) {
	rows, err := tx.Query(`
		WITH RECURSIVE sub(id) AS (
			SELECT id FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.id FROM nodes n JOIN sub s ON n.parent_id = s.id
		)
		SELECT fp.tg_msg_id FROM file_parts fp JOIN sub s ON fp.node_id = s.id WHERE fp.tg_msg_id > 0`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mids []int
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		mids = append(mids, mid)
	}
	return mids, rows.Err()
}

func (s *Store) PendingDeletes() ([]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`SELECT tg_msg_id FROM pending_deletes ORDER BY created_at ASC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mids []int
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		mids = append(mids, mid)
	}
	return mids, rows.Err()
}

func (s *Store) ClearPendingDeletes(msgIDs []int) error {
	if len(msgIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, mid := range msgIDs {
		if _, err := tx.Exec(`DELETE FROM pending_deletes WHERE tg_msg_id = ?`, mid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AllReferencedMsgIDs() (map[int]struct{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[int]struct{})
	rows, err := s.conn.Query(`
		SELECT tg_msg_id FROM file_parts WHERE tg_msg_id > 0
		UNION SELECT tg_msg_id FROM packs WHERE tg_msg_id > 0
		UNION SELECT tg_msg_id FROM upload_checkpoints WHERE tg_msg_id > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		out[mid] = struct{}{}
	}
	return out, rows.Err()
}

func (s *Store) GetUploadCheckpoint(uploadKey string) ([]FilePart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		SELECT part_index, tg_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, plain_size, cipher_size, part_header
		FROM upload_checkpoints WHERE upload_key = ? ORDER BY part_index ASC`, uploadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FilePart
	for rows.Next() {
		var p FilePart
		if err := rows.Scan(&p.PartIndex, &p.TGMsgID, &p.TGFileID, &p.TGAccessHash, &p.TGFileRef, &p.TGDCID, &p.PlainSize, &p.CipherSize, &p.PartHeader); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveUploadCheckpointPart(uploadKey string, totalSize int64, p FilePart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`
		INSERT OR REPLACE INTO upload_checkpoints
			(upload_key, part_index, tg_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, plain_size, cipher_size, part_header, total_size, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uploadKey, p.PartIndex, p.TGMsgID, p.TGFileID, p.TGAccessHash, p.TGFileRef, p.TGDCID, p.PlainSize, p.CipherSize, p.PartHeader, totalSize, time.Now().Unix(),
	)
	return err
}

func (s *Store) ClearUploadCheckpoint(uploadKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`DELETE FROM upload_checkpoints WHERE upload_key = ?`, uploadKey)
	return err
}

func (s *Store) PruneStaleUploadCheckpoints(maxAge time.Duration) ([]int, error) {
	cutoff := time.Now().Add(-maxAge).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT tg_msg_id FROM upload_checkpoints WHERE updated_at < ? AND tg_msg_id > 0`, cutoff)
	if err != nil {
		return nil, err
	}
	var candidates []int
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, mid)
	}
	rows.Close()
	if len(candidates) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`DELETE FROM upload_checkpoints WHERE updated_at < ?`, cutoff); err != nil {
		return nil, err
	}
	releasable, err := releaseUnreferencedMsgIDsTx(tx, candidates, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return releasable, nil
}

func (s *Store) Search(query string, limit int) ([]NodeWithPath, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []NodeWithPath{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.conn.Query(`
		WITH RECURSIVE active(id) AS (
			SELECT id FROM nodes WHERE id = 1
			UNION ALL
			SELECT n.id FROM nodes n JOIN active a ON n.parent_id = a.id WHERE n.deleted_at IS NULL
		)
		SELECT `+nodeCols+` FROM nodes
		WHERE id IN (SELECT id FROM active) AND id != 1 AND name LIKE ? ESCAPE '\'
		ORDER BY is_dir DESC, name COLLATE NOCASE ASC
		LIMIT ?`,
		"%"+escapeLike(query)+"%", limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	rows.Close()

	res := make([]NodeWithPath, 0, len(nodes))
	for _, n := range nodes {
		p, err := s.nodePathLocked(n.ID)
		if err != nil {
			continue
		}
		res = append(res, NodeWithPath{Node: n, Path: p})
	}
	return res, nil
}

func escapeLike(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	q = strings.ReplaceAll(q, `_`, `\_`)
	return q
}

func (s *Store) Stats() (*Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st Stats

	err := s.conn.QueryRow(`
		WITH RECURSIVE active(id) AS (
			SELECT id FROM nodes WHERE id = 1
			UNION ALL
			SELECT n.id FROM nodes n JOIN active a ON n.parent_id = a.id WHERE n.deleted_at IS NULL
		)
		SELECT
			COALESCE(SUM(CASE WHEN is_dir = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 1 AND id != 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 0 THEN size ELSE 0 END), 0)
		FROM nodes WHERE id IN (SELECT id FROM active)`).
		Scan(&st.FileCount, &st.FolderCount, &st.TotalPlainSize)
	if err != nil {
		return nil, err
	}

	if err := s.conn.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cipher_size), 0) FROM file_parts`).
		Scan(&st.PartCount, &st.TotalCipher); err != nil {
		return nil, err
	}

	if err := s.conn.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM nodes WHERE deleted_at IS NOT NULL`).
		Scan(&st.TrashCount, &st.TrashSize); err != nil {
		return nil, err
	}

	if err := s.conn.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(total_size), 0) FROM packs WHERE tg_msg_id > 0`).
		Scan(&st.Packs.PackCount, &st.Packs.TotalPackBytes); err != nil {
		return nil, err
	}
	if err := s.conn.QueryRow(`
		SELECT COALESCE(SUM(fp.cipher_size), 0)
		FROM file_parts fp JOIN packs p ON fp.pack_id = p.pack_id
		WHERE p.tg_msg_id > 0`).
		Scan(&st.Packs.LivePackBytes); err != nil {
		return nil, err
	}
	if st.Packs.TotalPackBytes > st.Packs.LivePackBytes {
		st.Packs.DeadPackBytes = st.Packs.TotalPackBytes - st.Packs.LivePackBytes
	}
	return &st, nil
}

func (s *Store) GetSettings() (Settings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	kv := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Settings{}, err
		}
		kv[k] = v
	}
	st := Settings{
		PartSizeMB:        parseIntDefault(kv["part_size_mb"], 1024),
		SmallFileMaxMB:    parseIntDefault(kv["small_file_max_mb"], 128),
		PackMaxMB:         parseIntDefault(kv["pack_max_mb"], 1024),
		PackIdleSec:       parseIntDefault(kv["pack_idle_sec"], 10),
		RAMCacheMB:        parseIntDefault(kv["ram_cache_mb"], 2048),
		BackupIntervalMin: parseIntDefault(kv["backup_interval_min"], 60),
		UploadThreads:     parseIntDefault(kv["upload_threads"], 4),
		WebDAVUser:        kv["webdav_user"],
		HasWebDAVPass:     kv["webdav_pass_hash"] != "",
		LastBackupAt:      parseInt64Default(kv["last_backup_at"], 0),
		LastBackupMsgID:   parseIntDefault(kv["last_backup_msg_id"], 0),
	}
	return st, nil
}

func (s *Store) SaveSettings(st Settings) error {
	if st.PartSizeMB < 10 || st.PartSizeMB > 1950 {
		return errors.New("part_size_mb must be between 10 and 1950")
	}
	if st.SmallFileMaxMB < 0 || st.SmallFileMaxMB > 1024 {
		return errors.New("small_file_max_mb must be between 0 and 1024")
	}
	if st.PackMaxMB < 16 || st.PackMaxMB > 1950 {
		return errors.New("pack_max_mb must be between 16 and 1950")
	}
	if st.SmallFileMaxMB > 0 && st.PackMaxMB < st.SmallFileMaxMB {
		return errors.New("pack_max_mb must be >= small_file_max_mb")
	}
	if st.PackIdleSec < 1 || st.PackIdleSec > 3600 {
		return errors.New("pack_idle_sec must be between 1 and 3600")
	}
	if st.RAMCacheMB < 16 || st.RAMCacheMB > 65536 {
		return errors.New("ram_cache_mb must be between 16 and 65536")
	}
	if st.BackupIntervalMin < 0 || st.BackupIntervalMin > 10080 {
		return errors.New("backup_interval_min must be between 0 and 10080")
	}
	if st.UploadThreads < 1 || st.UploadThreads > 16 {
		return errors.New("upload_threads must be between 1 and 16")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pairs := map[string]string{
		"part_size_mb":        strconv.Itoa(st.PartSizeMB),
		"small_file_max_mb":   strconv.Itoa(st.SmallFileMaxMB),
		"pack_max_mb":         strconv.Itoa(st.PackMaxMB),
		"pack_idle_sec":       strconv.Itoa(st.PackIdleSec),
		"ram_cache_mb":        strconv.Itoa(st.RAMCacheMB),
		"backup_interval_min": strconv.Itoa(st.BackupIntervalMin),
		"upload_threads":      strconv.Itoa(st.UploadThreads),
	}
	for k, v := range pairs {
		if _, err := s.conn.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	s.markChanged()
	return nil
}

func (s *Store) SetWebDAVCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	if len(username) < 2 || len(password) < 4 {
		return errors.New("webdav username must be >= 2 chars and password >= 4 chars")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 11)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.conn.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('webdav_user', ?)`, username); err != nil {
		return err
	}
	if _, err := s.conn.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('webdav_pass_hash', ?)`, string(hash)); err != nil {
		return err
	}
	s.markChanged()
	return nil
}

func (s *Store) VerifyWebDAV(username, password string) bool {
	s.mu.RLock()
	var user, hash string
	_ = s.conn.QueryRow(`SELECT value FROM settings WHERE key = 'webdav_user'`).Scan(&user)
	_ = s.conn.QueryRow(`SELECT value FROM settings WHERE key = 'webdav_pass_hash'`).Scan(&hash)
	s.mu.RUnlock()
	if user == "" || hash == "" || username != user {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (s *Store) RecordBackup(msgID int, at int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.conn.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('last_backup_at', ?)`, strconv.FormatInt(at, 10)); err != nil {
		return err
	}
	_, err := s.conn.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('last_backup_msg_id', ?)`, strconv.Itoa(msgID))
	return err
}

func (s *Store) SnapshotBytes(tmpDir string) ([]byte, error) {
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	snapPath := filepath.Join(tmpDir, fmt.Sprintf("telemount-snap-%s.db", newETag()))
	defer os.Remove(snapPath)

	s.mu.Lock()
	_, err := s.conn.Exec(`VACUUM INTO ?`, snapPath)
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("sqlite vacuum into: %w", err)
	}
	return os.ReadFile(snapPath)
}

func (s *Store) RestoreFromBytes(raw []byte, tmpDir string) error {
	if len(raw) < 100 || string(raw[:16]) != "SQLite format 3\x00" {
		return errors.New("invalid sqlite backup image")
	}
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	verifyPath := filepath.Join(tmpDir, fmt.Sprintf("telemount-verify-%s.db", newETag()))
	defer os.Remove(verifyPath)
	if err := os.WriteFile(verifyPath, raw, 0o600); err != nil {
		return err
	}
	vconn, err := openConn(verifyPath)
	if err != nil {
		return fmt.Errorf("open candidate backup: %w", err)
	}
	var rootExists int
	err = vconn.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = 1`).Scan(&rootExists)
	vconn.Close()
	if err != nil || rootExists != 1 {
		return errors.New("backup sqlite is missing telemount schema")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	_ = os.Remove(s.dbPath + "-wal")
	_ = os.Remove(s.dbPath + "-shm")

	stagePath := s.dbPath + ".restore"
	if err := os.WriteFile(stagePath, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(stagePath, s.dbPath); err != nil {
		_ = os.Remove(stagePath)
		return err
	}

	conn, err := openConn(s.dbPath)
	if err != nil {
		return err
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return err
	}
	s.conn = conn
	s.markChanged()
	return nil
}

func (s *Store) UpsertBlobReplica(r BlobReplica) error {
	if r.AccountID == "" || r.PrimaryMsgID <= 0 || r.ReplicaMsgID <= 0 {
		return errors.New("invalid blob replica")
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`
		INSERT OR REPLACE INTO blob_replicas
		(account_id, channel_id, primary_msg_id, replica_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AccountID, r.ChannelID, r.PrimaryMsgID, r.ReplicaMsgID, r.TGFileID, r.TGAccessHash, r.TGFileRef, r.TGDCID, r.CreatedAt,
	)
	if err == nil {
		s.markChanged()
	}
	return err
}

func (s *Store) GetBlobReplicas(primaryMsgID int) ([]BlobReplica, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		SELECT account_id, channel_id, primary_msg_id, replica_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id, created_at
		FROM blob_replicas WHERE primary_msg_id = ?`, primaryMsgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobReplica
	for rows.Next() {
		var r BlobReplica
		if err := rows.Scan(&r.AccountID, &r.ChannelID, &r.PrimaryMsgID, &r.ReplicaMsgID, &r.TGFileID, &r.TGAccessHash, &r.TGFileRef, &r.TGDCID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateReplicaFileRef(accountID string, replicaMsgID int, fileRef []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(`UPDATE blob_replicas SET tg_file_ref = ? WHERE account_id = ? AND replica_msg_id = ?`, fileRef, accountID, replicaMsgID)
	return err
}

func (s *Store) ListUnreplicatedMsgIDs(accountID string) ([]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`
		WITH all_msgs(mid) AS (
			SELECT DISTINCT tg_msg_id FROM file_parts WHERE tg_msg_id > 0
			UNION
			SELECT DISTINCT tg_msg_id FROM packs WHERE tg_msg_id > 0
		)
		SELECT m.mid FROM all_msgs m
		LEFT JOIN blob_replicas br ON br.account_id = ? AND br.primary_msg_id = m.mid
		WHERE br.replica_msg_id IS NULL
		ORDER BY m.mid ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		out = append(out, mid)
	}
	return out, rows.Err()
}

func (s *Store) ReplicaCountsByAccount() (int64, map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	err := s.conn.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT DISTINCT tg_msg_id FROM file_parts WHERE tg_msg_id > 0
			UNION
			SELECT DISTINCT tg_msg_id FROM packs WHERE tg_msg_id > 0
		)`).Scan(&total)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.conn.Query(`
		WITH all_msgs(mid) AS (
			SELECT DISTINCT tg_msg_id FROM file_parts WHERE tg_msg_id > 0
			UNION
			SELECT DISTINCT tg_msg_id FROM packs WHERE tg_msg_id > 0
		)
		SELECT br.account_id, COUNT(*)
		FROM blob_replicas br JOIN all_msgs m ON m.mid = br.primary_msg_id
		GROUP BY br.account_id`)
	if err != nil {
		return total, nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var acc string
		var cnt int64
		if err := rows.Scan(&acc, &cnt); err != nil {
			return total, nil, err
		}
		counts[acc] = cnt
	}
	return total, counts, rows.Err()
}

func (s *Store) DeleteAccountReplicas(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.conn.Exec(`DELETE FROM pending_replica_deletes WHERE account_id = ?`, accountID)
	_, err := s.conn.Exec(`DELETE FROM blob_replicas WHERE account_id = ?`, accountID)
	if err == nil {
		s.markChanged()
	}
	return err
}

func (s *Store) PendingReplicaDeletes(accountID string) ([]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.conn.Query(`SELECT replica_msg_id FROM pending_replica_deletes WHERE account_id = ? LIMIT 500`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		out = append(out, mid)
	}
	return out, rows.Err()
}

func (s *Store) ClearPendingReplicaDeletes(accountID string, msgIDs []int) error {
	if len(msgIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, mid := range msgIDs {
		if _, err := tx.Exec(`DELETE FROM pending_replica_deletes WHERE account_id = ? AND replica_msg_id = ?`, accountID, mid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PromoteAccountReplicasToPrimary remaps all file_parts, packs, and remaining backup account mappings
// to use the promoted backup account's own channel message IDs and file references.
func (s *Store) PromoteAccountReplicasToPrimary(accountID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remapReplicasLocked(`account_id = ?`, accountID)
}

// RemapChannelReplicasOnRestore remaps file_parts and packs when restoring a .tmdb backup directly from a Backup Channel.
func (s *Store) RemapChannelReplicasOnRestore(channelID int64) (int64, error) {
	if channelID == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remapReplicasLocked(`channel_id = ?`, channelID)
}

func (s *Store) remapReplicasLocked(whereClause string, arg any) (int64, error) {
	rows, err := s.conn.Query(`
		SELECT account_id, primary_msg_id, replica_msg_id, tg_file_id, tg_access_hash, tg_file_ref, tg_dc_id
		FROM blob_replicas WHERE `+whereClause, arg)
	if err != nil {
		return 0, err
	}
	var reps []BlobReplica
	for rows.Next() {
		var r BlobReplica
		if err := rows.Scan(&r.AccountID, &r.PrimaryMsgID, &r.ReplicaMsgID, &r.TGFileID, &r.TGAccessHash, &r.TGFileRef, &r.TGDCID); err != nil {
			rows.Close()
			return 0, err
		}
		reps = append(reps, r)
	}
	rows.Close()
	if len(reps) == 0 {
		return 0, nil
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Offset message IDs temporarily via negative values so in-place swaps never collide if a primary_msg_id == another replica_msg_id!
	const negBase = -1000000000
	var remapped int64
	for _, r := range reps {
		tmpID := negBase - r.ReplicaMsgID
		res, err := tx.Exec(`
			UPDATE file_parts
			SET tg_msg_id = ?, tg_file_id = ?, tg_access_hash = ?, tg_file_ref = ?, tg_dc_id = ?
			WHERE tg_msg_id = ?`,
			tmpID, r.TGFileID, r.TGAccessHash, r.TGFileRef, r.TGDCID, r.PrimaryMsgID,
		)
		if err != nil {
			return 0, err
		}
		aff, _ := res.RowsAffected()
		remapped += aff
		if _, err := tx.Exec(`UPDATE packs SET tg_msg_id = ? WHERE tg_msg_id = ?`, tmpID, r.PrimaryMsgID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE blob_replicas SET primary_msg_id = ? WHERE account_id != ? AND primary_msg_id = ?`,
			tmpID, r.AccountID, r.PrimaryMsgID); err != nil {
			return 0, err
		}
	}

	if _, err := tx.Exec(`UPDATE file_parts SET tg_msg_id = -(tg_msg_id - ?) WHERE tg_msg_id < ?`, negBase, negBase); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE packs SET tg_msg_id = -(tg_msg_id - ?) WHERE tg_msg_id < ?`, negBase, negBase); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE blob_replicas SET primary_msg_id = -(primary_msg_id - ?) WHERE primary_msg_id < ?`, negBase, negBase); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM blob_replicas WHERE `+whereClause, arg); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.markChanged()
	return remapped, nil
}

func parseIntDefault(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func parseInt64Default(s string, def int64) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
