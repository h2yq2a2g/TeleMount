//go:build windows

package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"telemount/internal/db"
	"telemount/internal/diskcache"
)

const (
	defaultBlockSize   uint64 = 4096
	defaultTotalBlocks uint64 = (1024 * 1024 * 1024 * 1024) / defaultBlockSize // 1 TiB virtual cloud drive capacity
)

type openHandle struct {
	mu         sync.Mutex
	id         uint64
	path       string
	flags      int
	writable   bool
	appendMode bool
	stagedPath string
	stagedFile *os.File
	readStream *diskcache.ReadStream
	dirty      bool
	mtime      int64
}

// TeleFS implements cgofuse FileSystemInterface (+ FileSystemGetpath for Windows case-insensitivity)
// on top of TeleMount's persistent diskcache.Manager and SQLite store.
type TeleFS struct {
	fuse.FileSystemBase

	store *db.Store
	cache *diskcache.Manager

	handlesMu sync.RWMutex
	handles   map[uint64]*openHandle
	nextFH    atomic.Uint64

	initMu   sync.Mutex
	initCh   chan struct{}
	initDone bool
}

var (
	_ fuse.FileSystemInterface = (*TeleFS)(nil)
	_ fuse.FileSystemGetpath   = (*TeleFS)(nil)
)

func NewTeleFS(store *db.Store, cache *diskcache.Manager) *TeleFS {
	return &TeleFS{
		store:   store,
		cache:   cache,
		handles: make(map[uint64]*openHandle),
	}
}

func (f *TeleFS) resetInitSignal() chan struct{} {
	f.initMu.Lock()
	defer f.initMu.Unlock()
	f.initCh = make(chan struct{})
	f.initDone = false
	return f.initCh
}

func (f *TeleFS) Init() {
	f.initMu.Lock()
	if f.initCh != nil && !f.initDone {
		f.initDone = true
		close(f.initCh)
	}
	f.initMu.Unlock()
}

func (f *TeleFS) Destroy() {
	f.handlesMu.Lock()
	all := make([]*openHandle, 0, len(f.handles))
	for id, h := range f.handles {
		all = append(all, h)
		delete(f.handles, id)
	}
	f.handlesMu.Unlock()

	for _, h := range all {
		h.mu.Lock()
		if h.dirty {
			_ = f.flushHandleLocked(h)
		}
		f.cleanupHandleFilesLocked(h)
		h.mu.Unlock()
	}
}

func (f *TeleFS) getHandle(fh uint64) *openHandle {
	if fh == 0 {
		return nil
	}
	f.handlesMu.RLock()
	h := f.handles[fh]
	f.handlesMu.RUnlock()
	return h
}

func (f *TeleFS) findOpenDirtyByPath(normPath string) *openHandle {
	wantLower := strings.ToLower(normPath)
	f.handlesMu.RLock()
	defer f.handlesMu.RUnlock()
	for _, h := range f.handles {
		if h.writable && strings.EqualFold(h.path, wantLower) {
			return h
		}
	}
	return nil
}

func (f *TeleFS) registerHandle(h *openHandle) uint64 {
	id := f.nextFH.Add(1)
	h.id = id
	f.handlesMu.Lock()
	f.handles[id] = h
	f.handlesMu.Unlock()
	return id
}

func (f *TeleFS) unregisterHandle(fh uint64) *openHandle {
	f.handlesMu.Lock()
	h := f.handles[fh]
	delete(f.handles, fh)
	f.handlesMu.Unlock()
	return h
}

func (f *TeleFS) ensureStagedLocked(h *openHandle, truncate bool) error {
	if h.stagedFile != nil {
		if truncate {
			if err := h.stagedFile.Truncate(0); err != nil {
				return err
			}
			h.dirty = true
		}
		return nil
	}
	stPath, err := f.cache.StageOpenForWrite(h.path, truncate)
	if err != nil {
		return err
	}
	osf, err := os.OpenFile(stPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		_ = os.Remove(stPath)
		return err
	}
	h.stagedPath = stPath
	h.stagedFile = osf
	if h.readStream != nil {
		_ = h.readStream.Close()
		h.readStream = nil
	}
	return nil
}

func (f *TeleFS) flushHandleLocked(h *openHandle) int {
	if !h.writable || !h.dirty {
		return 0
	}
	var size int64
	if h.stagedFile != nil {
		if err := h.stagedFile.Sync(); err != nil {
			return mapErr(err)
		}
		fi, err := h.stagedFile.Stat()
		if err != nil {
			return mapErr(err)
		}
		size = fi.Size()
		if err := h.stagedFile.Close(); err != nil {
			h.stagedFile = nil
			return mapErr(err)
		}
		h.stagedFile = nil
	} else if h.stagedPath != "" {
		if fi, err := os.Stat(h.stagedPath); err == nil {
			size = fi.Size()
		}
	}

	stPath := h.stagedPath
	h.stagedPath = ""
	mtime := h.mtime
	if mtime <= 0 {
		mtime = time.Now().Unix()
	}

	if err := f.cache.CommitFile(context.Background(), h.path, stPath, size, mtime); err != nil {
		if stPath != "" {
			_ = os.Remove(stPath)
		}
		return mapErr(err)
	}
	h.dirty = false
	return 0
}

func (f *TeleFS) cleanupHandleFilesLocked(h *openHandle) {
	if h.readStream != nil {
		_ = h.readStream.Close()
		h.readStream = nil
	}
	if h.stagedFile != nil {
		_ = h.stagedFile.Close()
		h.stagedFile = nil
	}
	if h.stagedPath != "" {
		_ = os.Remove(h.stagedPath)
		h.stagedPath = ""
	}
}

func (f *TeleFS) Statfs(_ string, stat *fuse.Statfs_t) int {
	if stat == nil {
		return -fuse.EINVAL
	}
	var usedBytes int64
	if f.store != nil {
		if st, err := f.store.Stats(); err == nil && st != nil {
			usedBytes = st.TotalPlainSize
		}
	}
	usedBlocks := uint64(0)
	if usedBytes > 0 {
		usedBlocks = (uint64(usedBytes) + defaultBlockSize - 1) / defaultBlockSize
	}
	freeBlocks := defaultTotalBlocks
	if usedBlocks < defaultTotalBlocks {
		freeBlocks = defaultTotalBlocks - usedBlocks
	} else {
		freeBlocks = 1024
	}
	stat.Bsize = defaultBlockSize
	stat.Frsize = defaultBlockSize
	stat.Blocks = defaultTotalBlocks
	stat.Bfree = freeBlocks
	stat.Bavail = freeBlocks
	stat.Files = 1_000_000
	stat.Ffree = 999_000
	stat.Favail = 999_000
	stat.Namemax = 255
	return 0
}

func (f *TeleFS) Getpath(p string, fh uint64) (int, string) {
	if h := f.getHandle(fh); h != nil {
		h.mu.Lock()
		cp := h.path
		h.mu.Unlock()
		return 0, cp
	}
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return 0, "/"
	}
	if ov, found, err := f.cache.Stat(norm); err != nil {
		return mapErr(err), ""
	} else if found {
		return 0, ov.Path
	}
	if h := f.findOpenDirtyByPath(norm); h != nil {
		h.mu.Lock()
		cp := h.path
		h.mu.Unlock()
		return 0, cp
	}
	return -fuse.ENOENT, ""
}

func (f *TeleFS) Getattr(p string, stat *fuse.Stat_t, fh uint64) int {
	if stat == nil {
		return -fuse.EINVAL
	}
	norm := diskcache.NormalizePath(p)

	if h := f.getHandle(fh); h != nil {
		h.mu.Lock()
		if h.writable && h.dirty && h.stagedFile != nil {
			if fi, err := h.stagedFile.Stat(); err == nil {
				fillStat(stat, false, fi.Size(), h.mtime, h.mtime)
				h.mu.Unlock()
				return 0
			}
		}
		hp := h.path
		h.mu.Unlock()
		norm = hp
	} else if h := f.findOpenDirtyByPath(norm); h != nil {
		h.mu.Lock()
		if h.dirty && h.stagedFile != nil {
			if fi, err := h.stagedFile.Stat(); err == nil {
				fillStat(stat, false, fi.Size(), h.mtime, h.mtime)
				h.mu.Unlock()
				return 0
			}
		}
		h.mu.Unlock()
	}

	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	fillStat(stat, ov.IsDir, ov.Size, ov.Mtime, ov.Ctime)
	return 0
}

func fillStat(stat *fuse.Stat_t, isDir bool, size, mtime, ctime int64) {
	now := time.Now().Unix()
	if mtime <= 0 {
		mtime = now
	}
	if ctime <= 0 {
		ctime = mtime
	}
	*stat = fuse.Stat_t{}
	if isDir {
		stat.Mode = fuse.S_IFDIR | 0o755
		stat.Nlink = 2
		stat.Size = 0
	} else {
		stat.Mode = fuse.S_IFREG | 0o644
		stat.Nlink = 1
		stat.Size = size
		stat.Blocks = (size + 511) / 512
	}
	stat.Blksize = int64(defaultBlockSize)
	stat.Atim = fuse.Timespec{Sec: mtime}
	stat.Mtim = fuse.Timespec{Sec: mtime}
	stat.Ctim = fuse.Timespec{Sec: ctime}
	stat.Birthtim = fuse.Timespec{Sec: ctime}
}

func (f *TeleFS) Opendir(p string) (int, uint64) {
	norm := diskcache.NormalizePath(p)
	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	if !found {
		return -fuse.ENOENT, ^uint64(0)
	}
	if !ov.IsDir {
		return -fuse.ENOTDIR, ^uint64(0)
	}
	return 0, f.registerHandle(&openHandle{path: ov.Path})
}

func (f *TeleFS) Readdir(p string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, _ int64, fh uint64) int {
	dirPath := diskcache.NormalizePath(p)
	if h := f.getHandle(fh); h != nil {
		h.mu.Lock()
		dirPath = h.path
		h.mu.Unlock()
	}
	children, err := f.cache.ListDir(dirPath)
	if err != nil {
		return mapErr(err)
	}

	var dirStat fuse.Stat_t
	fillStat(&dirStat, true, 0, time.Now().Unix(), time.Now().Unix())
	if !fill(".", &dirStat, 0) {
		return 0
	}
	if !fill("..", &dirStat, 0) {
		return 0
	}

	seen := make(map[string]struct{}, len(children))
	for _, ch := range children {
		name := path.Base(ch.Path)
		seen[strings.ToLower(name)] = struct{}{}
		var st fuse.Stat_t
		fillStat(&st, ch.IsDir, ch.Size, ch.Mtime, ch.Ctime)
		if !fill(name, &st, 0) {
			return 0
		}
	}

	// Include newly created but not-yet-flushed handles inside this directory so Windows apps see them immediately.
	dirLower := strings.ToLower(dirPath)
	f.handlesMu.RLock()
	var extra []*openHandle
	for _, h := range f.handles {
		if h.writable {
			extra = append(extra, h)
		}
	}
	f.handlesMu.RUnlock()

	for _, h := range extra {
		h.mu.Lock()
		if h.dirty && h.stagedFile != nil {
			parent := path.Dir(h.path)
			if parent == "." || parent == "" {
				parent = "/"
			}
			if strings.EqualFold(parent, dirLower) {
				name := path.Base(h.path)
				if _, ok := seen[strings.ToLower(name)]; !ok {
					seen[strings.ToLower(name)] = struct{}{}
					var sz int64
					if fi, err := h.stagedFile.Stat(); err == nil {
						sz = fi.Size()
					}
					var st fuse.Stat_t
					fillStat(&st, false, sz, h.mtime, h.mtime)
					h.mu.Unlock()
					if !fill(name, &st, 0) {
						return 0
					}
					continue
				}
			}
		}
		h.mu.Unlock()
	}
	return 0
}

func (f *TeleFS) Releasedir(_ string, fh uint64) int {
	f.unregisterHandle(fh)
	return 0
}

func (f *TeleFS) Mkdir(p string, _ uint32) int {
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return -fuse.EEXIST
	}
	parentPath := parentDir(norm)
	pov, pfound, err := f.cache.Stat(parentPath)
	if err != nil {
		return mapErr(err)
	}
	if !pfound {
		return -fuse.ENOENT
	}
	if !pov.IsDir {
		return -fuse.ENOTDIR
	}
	if _, exists, err := f.cache.Stat(norm); err != nil {
		return mapErr(err)
	} else if exists {
		return -fuse.EEXIST
	}
	if err := f.cache.QueueMkdir(context.Background(), norm); err != nil {
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Rmdir(p string) int {
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return -fuse.EBUSY
	}
	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	if !ov.IsDir {
		return -fuse.ENOTDIR
	}
	children, err := f.cache.ListDir(ov.Path)
	if err != nil {
		return mapErr(err)
	}
	if len(children) > 0 {
		return -fuse.ENOTEMPTY
	}
	if err := f.cache.QueueDelete(context.Background(), ov.Path, true); err != nil {
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Unlink(p string) int {
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return -fuse.EISDIR
	}
	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	if ov.IsDir {
		return -fuse.EISDIR
	}
	if err := f.cache.QueueDelete(context.Background(), ov.Path, false); err != nil {
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Rename(oldpath, newpath string) int {
	oldNorm := diskcache.NormalizePath(oldpath)
	newNorm := diskcache.NormalizePath(newpath)
	if oldNorm == "/" || newNorm == "/" {
		return -fuse.EINVAL
	}

	// Flush any active dirty handle on oldpath so the renamed target gets the latest written bytes.
	if h := f.findOpenDirtyByPath(oldNorm); h != nil {
		h.mu.Lock()
		if h.dirty {
			if rc := f.flushHandleLocked(h); rc != 0 {
				h.mu.Unlock()
				return rc
			}
		}
		h.path = newNorm
		if h.readStream != nil {
			_ = h.readStream.Close()
			h.readStream = f.cache.OpenReadStream(newNorm)
		}
		h.mu.Unlock()
	}

	srcOv, srcFound, err := f.cache.Stat(oldNorm)
	if err != nil {
		return mapErr(err)
	}
	if !srcFound {
		return -fuse.ENOENT
	}

	// Prevent moving a directory into itself or one of its descendants.
	if srcOv.IsDir {
		srcLower := strings.ToLower(srcOv.Path)
		newLower := strings.ToLower(newNorm)
		if strings.HasPrefix(newLower, srcLower+"/") {
			return -fuse.EINVAL
		}
	}

	dstParentPath := parentDir(newNorm)
	dstParentOv, dstParentFound, err := f.cache.Stat(dstParentPath)
	if err != nil {
		return mapErr(err)
	}
	if !dstParentFound {
		return -fuse.ENOENT
	}
	if !dstParentOv.IsDir {
		return -fuse.ENOTDIR
	}

	sameCaseInsensitive := strings.EqualFold(srcOv.Path, newNorm)
	if !sameCaseInsensitive {
		dstOv, dstFound, err := f.cache.Stat(newNorm)
		if err != nil {
			return mapErr(err)
		}
		if dstFound {
			if !srcOv.IsDir && dstOv.IsDir {
				return -fuse.EISDIR
			}
			if srcOv.IsDir && !dstOv.IsDir {
				return -fuse.ENOTDIR
			}
			if srcOv.IsDir && dstOv.IsDir {
				children, cerr := f.cache.ListDir(dstOv.Path)
				if cerr != nil {
					return mapErr(cerr)
				}
				if len(children) > 0 {
					return -fuse.ENOTEMPTY
				}
			}
		}
	}

	if err := f.cache.QueueRename(context.Background(), srcOv.Path, newNorm); err != nil {
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Create(p string, flags int, _ uint32) (int, uint64) {
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return -fuse.EISDIR, ^uint64(0)
	}
	parentPath := parentDir(norm)
	pov, pfound, err := f.cache.Stat(parentPath)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	if !pfound {
		return -fuse.ENOENT, ^uint64(0)
	}
	if !pov.IsDir {
		return -fuse.ENOTDIR, ^uint64(0)
	}

	ov, exists, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	if exists {
		if ov.IsDir {
			return -fuse.EISDIR, ^uint64(0)
		}
		if flags&fuse.O_EXCL != 0 {
			return -fuse.EEXIST, ^uint64(0)
		}
		norm = ov.Path
	} else if f.findOpenDirtyByPath(norm) != nil && flags&fuse.O_EXCL != 0 {
		return -fuse.EEXIST, ^uint64(0)
	}

	stPath, err := f.cache.StageOpenForWrite(norm, true)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	osf, err := os.OpenFile(stPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		_ = os.Remove(stPath)
		return mapErr(err), ^uint64(0)
	}

	h := &openHandle{
		path:       norm,
		flags:      flags,
		writable:   true,
		appendMode: flags&fuse.O_APPEND != 0,
		stagedPath: stPath,
		stagedFile: osf,
		dirty:      true,
		mtime:      time.Now().Unix(),
	}
	return 0, f.registerHandle(h)
}

func (f *TeleFS) Open(p string, flags int) (int, uint64) {
	norm := diskcache.NormalizePath(p)
	if norm == "/" {
		return -fuse.EISDIR, ^uint64(0)
	}
	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	if !found {
		return -fuse.ENOENT, ^uint64(0)
	}
	if ov.IsDir {
		return -fuse.EISDIR, ^uint64(0)
	}

	accMode := flags & fuse.O_ACCMODE
	writable := accMode == fuse.O_WRONLY || accMode == fuse.O_RDWR || flags&fuse.O_TRUNC != 0 || flags&fuse.O_APPEND != 0
	if !writable {
		return 0, f.registerHandle(&openHandle{
			path:       ov.Path,
			flags:      flags,
			writable:   false,
			readStream: f.cache.OpenReadStream(ov.Path),
			mtime:      ov.Mtime,
		})
	}

	trunc := flags&fuse.O_TRUNC != 0
	if !trunc {
		// Defer staging copy until Write/Truncate so O_RDWR probes on large files do not duplicate gigabytes on disk.
		return 0, f.registerHandle(&openHandle{
			path:       ov.Path,
			flags:      flags,
			writable:   true,
			appendMode: flags&fuse.O_APPEND != 0,
			readStream: f.cache.OpenReadStream(ov.Path),
			dirty:      false,
			mtime:      ov.Mtime,
		})
	}
	stPath, err := f.cache.StageOpenForWrite(ov.Path, true)
	if err != nil {
		return mapErr(err), ^uint64(0)
	}
	osf, err := os.OpenFile(stPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		_ = os.Remove(stPath)
		return mapErr(err), ^uint64(0)
	}

	h := &openHandle{
		path:       ov.Path,
		flags:      flags,
		writable:   true,
		appendMode: flags&fuse.O_APPEND != 0,
		stagedPath: stPath,
		stagedFile: osf,
		dirty:      true,
		mtime:      time.Now().Unix(),
	}
	return 0, f.registerHandle(h)
}

func (f *TeleFS) Read(p string, buff []byte, ofst int64, fh uint64) int {
	if ofst < 0 {
		return -fuse.EINVAL
	}
	if len(buff) == 0 {
		return 0
	}

	targetPath := diskcache.NormalizePath(p)
	var rs *diskcache.ReadStream
	if h := f.getHandle(fh); h != nil {
		h.mu.Lock()
		if h.stagedFile != nil {
			n, err := h.stagedFile.ReadAt(buff, ofst)
			h.mu.Unlock()
			if err != nil && !errors.Is(err, io.EOF) {
				return mapErr(err)
			}
			return n
		}
		targetPath = h.path
		rs = h.readStream
		h.mu.Unlock()
	}

	var (
		n   int
		err error
	)
	if rs != nil {
		n, err = rs.ReadAt(context.Background(), buff, ofst)
	} else {
		n, err = f.cache.ReadFileAt(context.Background(), targetPath, buff, ofst)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return mapErr(err)
	}
	return n
}

func (f *TeleFS) Write(p string, buff []byte, ofst int64, fh uint64) int {
	if ofst < 0 {
		return -fuse.EINVAL
	}
	h := f.getHandle(fh)
	if h == nil || !h.writable {
		return -fuse.EBADF
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if err := f.ensureStagedLocked(h, false); err != nil {
		return mapErr(err)
	}
	writeOff := ofst
	if h.appendMode {
		fi, err := h.stagedFile.Stat()
		if err != nil {
			return mapErr(err)
		}
		writeOff = fi.Size()
	}
	n, err := h.stagedFile.WriteAt(buff, writeOff)
	if err != nil {
		return mapErr(err)
	}
	h.dirty = true
	h.mtime = time.Now().Unix()
	return n
}

func (f *TeleFS) Truncate(p string, size int64, fh uint64) int {
	if size < 0 {
		return -fuse.EINVAL
	}
	if h := f.getHandle(fh); h != nil {
		if !h.writable {
			return -fuse.EBADF
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := f.ensureStagedLocked(h, size == 0); err != nil {
			return mapErr(err)
		}
		if err := h.stagedFile.Truncate(size); err != nil {
			return mapErr(err)
		}
		h.dirty = true
		h.mtime = time.Now().Unix()
		return 0
	}

	norm := diskcache.NormalizePath(p)
	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	if ov.IsDir {
		return -fuse.EISDIR
	}
	stPath, err := f.cache.StageOpenForWrite(ov.Path, size == 0)
	if err != nil {
		return mapErr(err)
	}
	if err := os.Truncate(stPath, size); err != nil {
		_ = os.Remove(stPath)
		return mapErr(err)
	}
	if err := f.cache.CommitFile(context.Background(), ov.Path, stPath, size, time.Now().Unix()); err != nil {
		_ = os.Remove(stPath)
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Flush(_ string, fh uint64) int {
	h := f.getHandle(fh)
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return f.flushHandleLocked(h)
}

func (f *TeleFS) Release(_ string, fh uint64) int {
	h := f.unregisterHandle(fh)
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rc := 0
	if h.dirty {
		rc = f.flushHandleLocked(h)
	}
	f.cleanupHandleFilesLocked(h)
	return rc
}

func (f *TeleFS) Fsync(_ string, _ bool, fh uint64) int {
	return f.Flush("", fh)
}

func (f *TeleFS) Fsyncdir(_ string, _ bool, _ uint64) int {
	return 0
}

func parseUtimens(tmsp []fuse.Timespec) (mtime int64, apply bool) {
	mtime = time.Now().Unix()
	if len(tmsp) >= 2 {
		if tmsp[1].Nsec == fuse.UTIME_OMIT {
			return 0, false
		}
		if tmsp[1].Nsec != fuse.UTIME_NOW {
			mtime = tmsp[1].Sec
		}
	} else if len(tmsp) == 1 {
		if tmsp[0].Nsec == fuse.UTIME_OMIT {
			return 0, false
		}
		if tmsp[0].Nsec != fuse.UTIME_NOW {
			mtime = tmsp[0].Sec
		}
	}
	return mtime, true
}

func (f *TeleFS) Utimens(p string, tmsp []fuse.Timespec) int {
	norm := diskcache.NormalizePath(p)
	// Explorer sets source mtime via SetFileTime before CloseHandle on newly created files.
	if h := f.findOpenDirtyByPath(norm); h != nil {
		h.mu.Lock()
		if h.dirty {
			if mtime, apply := parseUtimens(tmsp); apply {
				h.mtime = mtime
			}
			h.mu.Unlock()
			return 0
		}
		h.mu.Unlock()
	}

	ov, found, err := f.cache.Stat(norm)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}

	mtime, apply := parseUtimens(tmsp)
	if !apply {
		return 0
	}

	if err := f.cache.QueueUtimens(context.Background(), ov.Path, mtime); err != nil {
		return mapErr(err)
	}
	return 0
}

func (f *TeleFS) Chmod(p string, _ uint32) int {
	_, found, err := f.cache.Stat(diskcache.NormalizePath(p))
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	return 0
}

func (f *TeleFS) Chown(p string, _, _ uint32) int {
	_, found, err := f.cache.Stat(diskcache.NormalizePath(p))
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	return 0
}

func (f *TeleFS) Access(p string, _ uint32) int {
	_, found, err := f.cache.Stat(diskcache.NormalizePath(p))
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return -fuse.ENOENT
	}
	return 0
}

func parentDir(normPath string) string {
	d := path.Dir(normPath)
	if d == "." || d == "" {
		return "/"
	}
	return d
}

func mapErr(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, db.ErrNotFound):
		return -fuse.ENOENT
	case errors.Is(err, os.ErrExist), errors.Is(err, db.ErrAlreadyExist):
		return -fuse.EEXIST
	case errors.Is(err, db.ErrNotDir):
		return -fuse.ENOTDIR
	case errors.Is(err, os.ErrPermission):
		return -fuse.EACCES
	case errors.Is(err, os.ErrInvalid), errors.Is(err, db.ErrInvalidName), errors.Is(err, db.ErrInvalidMove):
		return -fuse.EINVAL
	case errors.Is(err, diskcache.ErrOfflineUncached):
		return -fuse.EIO
	default:
		return -fuse.EIO
	}
}
