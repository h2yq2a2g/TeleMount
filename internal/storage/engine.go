package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"telemount/internal/crypto"
	"telemount/internal/db"
	"telemount/internal/state"
	"telemount/internal/tg"
)

const (
	DBBackupPrefix       = "telemount-db-backup-"
	legacyDBBackupPrefix = "teledrive-db-backup-"
	DBBackupKeepCount    = 5
)

var (
	ErrNotReady = errors.New("telemount is not fully configured yet")
)

type Engine struct {
	store  *db.Store
	state  *state.Manager
	tg     tg.Backend
	pool   *tg.Pool
	cache  *ChunkCache
	packer *Packer
	tmpDir string

	backupMu sync.Mutex
	syncMu   sync.Mutex
}

func NewEngine(store *db.Store, st *state.Manager, tgBackend tg.Backend, tmpDir string) *Engine {
	cacheMB := 2048
	if settings, err := store.GetSettings(); err == nil && settings.RAMCacheMB > 0 {
		cacheMB = settings.RAMCacheMB
	}
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	e := &Engine{
		store:  store,
		state:  st,
		tg:     tgBackend,
		pool:   tg.NewPool(st.DataDir()),
		cache:  NewChunkCache(int64(cacheMB) * 1024 * 1024),
		tmpDir: tmpDir,
	}
	e.packer = newPacker(e)
	return e
}

func (e *Engine) Store() *db.Store     { return e.store }
func (e *Engine) State() *state.Manager  { return e.state }
func (e *Engine) Pool() *tg.Pool       { return e.pool }
func (e *Engine) SetPool(p *tg.Pool)   { e.pool = p }
func (e *Engine) Cache() *ChunkCache   { return e.cache }
func (e *Engine) Packer() *Packer      { return e.packer }
func (e *Engine) TmpDir() string       { return e.tmpDir }

func (e *Engine) IsReady() bool {
	return e.state.HasAdmin() && e.state.HasChannel() && e.state.HasMasterKey() && e.hasAnyAuthorizedAccount()
}

func (e *Engine) ApplySettings(s db.Settings) error {
	if err := e.store.SaveSettings(s); err != nil {
		return err
	}
	e.cache.SetMaxBytes(int64(s.RAMCacheMB) * 1024 * 1024)
	return nil
}

// StartBackgroundTasks periodically flushes pending Telegram deletes and runs automatic encrypted DB backups.
func (e *Engine) StartBackgroundTasks(ctx context.Context) {
	go func() {
		delTicker := time.NewTicker(2 * time.Minute)
		bakTicker := time.NewTicker(1 * time.Minute)
		syncTicker := time.NewTicker(5 * time.Minute)
		defer delTicker.Stop()
		defer bakTicker.Stop()
		defer syncTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-delTicker.C:
				if e.IsReady() {
					_ = e.FlushPendingDeletes(ctx)
					e.FlushPendingReplicaDeletes(ctx)
				}
			case <-syncTicker.C:
				if e.IsReady() && len(e.state.BackupAccounts()) > 0 {
					_, _ = e.SyncAllReplicas(ctx, "")
				}
			case <-bakTicker.C:
				if !e.IsReady() {
					continue
				}
				settings, err := e.store.GetSettings()
				if err != nil || settings.BackupIntervalMin <= 0 {
					continue
				}
				lastChange := e.store.LastChangeUnix()
				if lastChange <= settings.LastBackupAt {
					continue
				}
				if time.Since(time.Unix(settings.LastBackupAt, 0)) >= time.Duration(settings.BackupIntervalMin)*time.Minute {
					// Flush any staged small-file pack before taking a DB backup so the backup contains real Telegram message IDs!
					_ = e.packer.FlushNow(ctx)
					_, _ = e.BackupDBToTelegram(ctx)
				}
			}
		}
	}()
}

// UploadFile implements Hybrid Pack storage:
//   - Small files (< SmallFileMaxMB, default 128 MiB): encrypted in RAM and coalesced into a Pack (flushed at PackMaxMB or PackIdleSec).
//   - Large files (>= SmallFileMaxMB): streamed directly to Telegram in parts of PartSizeMB (default 1 GiB) with zero RAM staging wait.
func (e *Engine) UploadFile(ctx context.Context, parentID int64, name, mimeType string, r io.Reader, knownSize int64) (*db.Node, error) {
	if !e.IsReady() {
		return nil, ErrNotReady
	}
	masterKey, _, ok := e.state.MasterKey()
	if !ok {
		return nil, ErrNotReady
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(path.Ext(name))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}

	settings, err := e.store.GetSettings()
	if err != nil {
		return nil, err
	}
	partSize := int64(settings.PartSizeMB) * 1024 * 1024
	if partSize <= 0 {
		partSize = 1024 * 1024 * 1024
	}
	smallMax := int64(settings.SmallFileMaxMB) * 1024 * 1024
	packMax := int64(settings.PackMaxMB) * 1024 * 1024
	if packMax <= 0 {
		packMax = 1024 * 1024 * 1024
	}
	idleDur := time.Duration(settings.PackIdleSec) * time.Second
	threads := settings.UploadThreads
	if threads <= 0 {
		threads = 4
	}

	// Empty 0-byte file needs no Telegram blob at all.
	if knownSize == 0 {
		node, replaced, err := e.store.UpsertFileWithParts(parentID, name, 0, mimeType, nil)
		if err == nil && len(replaced) > 0 {
			go e.deleteBestEffort(replaced)
		}
		return node, err
	}

	// Unknown stream length (e.g. chunked WebDAV PUT): probe up to smallMax bytes in RAM.
	// If EOF arrives before smallMax, we know its exact size and can pack it directly!
	if knownSize < 0 && smallMax > 0 {
		var probe bytes.Buffer
		n, err := io.CopyN(&probe, r, smallMax)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if errors.Is(err, io.EOF) {
			if n == 0 {
				node, replaced, uerr := e.store.UpsertFileWithParts(parentID, name, 0, mimeType, nil)
				if uerr == nil && len(replaced) > 0 {
					go e.deleteBestEffort(replaced)
				}
				return node, uerr
			}
			return e.uploadSmallPacked(ctx, parentID, name, mimeType, bytes.NewReader(probe.Bytes()), n, masterKey, packMax, idleDur)
		}
		// Stream is >= smallMax: stitch probed prefix back onto r and proceed through large-file streaming path.
		r = io.MultiReader(bytes.NewReader(probe.Bytes()), r)
	}

	// Known-size small file (< smallMax): coalesce into RAM Pack!
	if knownSize > 0 && smallMax > 0 && knownSize < smallMax {
		return e.uploadSmallPacked(ctx, parentID, name, mimeType, r, knownSize, masterKey, packMax, idleDur)
	}

	// Large file (>= smallMax or packing disabled): stream directly to Telegram in PartSizeMB parts.
	trID := e.tg.Tracker().StartTransfer("upload", name, knownSize)
	defer e.tg.Tracker().EndTransfer(trID)

	var parts []db.FilePart
	var uploadedMsgIDs []int
	var totalPlain int64
	var completedBytes int64

	cleanupUploaded := func() {
		if len(uploadedMsgIDs) > 0 {
			_ = e.tg.DeleteMessages(context.Background(), uploadedMsgIDs)
		}
	}

	if knownSize > 0 {
		remaining := knownSize
		for remaining > 0 {
			thisPlain := remaining
			if thisPlain > partSize {
				thisPlain = partSize
			}
			thisCipher := crypto.CipherSize(thisPlain)
			partBase := completedBytes

			lr := &exactReader{r: io.LimitReader(r, thisPlain), want: thisPlain}
			encReader, hdr, err := crypto.NewEncryptReader(lr, masterKey)
			if err != nil {
				cleanupUploaded()
				return nil, err
			}

			blobName := randomBlobName()
			doc, err := e.tg.UploadBlob(ctx, blobName, "", encReader, thisCipher, threads, func(done int64) {
				if done > thisPlain {
					done = thisPlain
				}
				e.tg.Tracker().UpdateTransfer(trID, partBase+done)
			})
			if err != nil {
				cleanupUploaded()
				return nil, err
			}
			if lr.read != thisPlain {
				cleanupUploaded()
				_ = e.tg.DeleteMessages(context.Background(), []int{doc.MsgID})
				return nil, io.ErrUnexpectedEOF
			}

			uploadedMsgIDs = append(uploadedMsgIDs, doc.MsgID)
			parts = append(parts, db.FilePart{
				PartIndex:    len(parts),
				TGMsgID:      doc.MsgID,
				TGFileID:     doc.FileID,
				TGAccessHash: doc.AccessHash,
				TGFileRef:    doc.FileRef,
				TGDCID:       doc.DCID,
				PlainSize:    thisPlain,
				CipherSize:   thisCipher,
				PartHeader:   append([]byte(nil), hdr[:]...),
			})
			completedBytes += thisPlain
			totalPlain += thisPlain
			remaining -= thisPlain
		}
	} else {
		for {
			tmpFile, err := os.CreateTemp(e.tmpDir, "td-part-*")
			if err != nil {
				cleanupUploaded()
				return nil, err
			}
			tmpPath := tmpFile.Name()
			n, copyErr := io.CopyN(tmpFile, r, partSize)
			if copyErr != nil && !errors.Is(copyErr, io.EOF) {
				tmpFile.Close()
				os.Remove(tmpPath)
				cleanupUploaded()
				return nil, copyErr
			}
			if n == 0 {
				tmpFile.Close()
				os.Remove(tmpPath)
				break
			}
			if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				cleanupUploaded()
				return nil, err
			}

			thisCipher := crypto.CipherSize(n)
			partBase := completedBytes
			encReader, hdr, err := crypto.NewEncryptReader(tmpFile, masterKey)
			if err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				cleanupUploaded()
				return nil, err
			}

			blobName := randomBlobName()
			doc, err := e.tg.UploadBlob(ctx, blobName, "", encReader, thisCipher, threads, func(done int64) {
				if done > n {
					done = n
				}
				e.tg.Tracker().UpdateTransfer(trID, partBase+done)
			})
			tmpFile.Close()
			os.Remove(tmpPath)
			if err != nil {
				cleanupUploaded()
				return nil, err
			}

			uploadedMsgIDs = append(uploadedMsgIDs, doc.MsgID)
			parts = append(parts, db.FilePart{
				PartIndex:    len(parts),
				TGMsgID:      doc.MsgID,
				TGFileID:     doc.FileID,
				TGAccessHash: doc.AccessHash,
				TGFileRef:    doc.FileRef,
				TGDCID:       doc.DCID,
				PlainSize:    n,
				CipherSize:   thisCipher,
				PartHeader:   append([]byte(nil), hdr[:]...),
			})
			completedBytes += n
			totalPlain += n
			if errors.Is(copyErr, io.EOF) {
				break
			}
		}
	}

	node, replaced, err := e.store.UpsertFileWithParts(parentID, name, totalPlain, mimeType, parts)
	if err != nil {
		cleanupUploaded()
		return nil, err
	}
	if len(replaced) > 0 {
		go e.deleteBestEffort(replaced)
	}
	for _, p := range parts {
		if p.TGMsgID > 0 {
			e.MirrorMsgAsync(p.TGMsgID, false)
		}
	}
	return node, nil
}

func (e *Engine) uploadSmallPacked(
	ctx context.Context,
	parentID int64,
	name, mimeType string,
	r io.Reader,
	plainSize int64,
	masterKey []byte,
	packMaxBytes int64,
	idleDur time.Duration,
) (*db.Node, error) {
	lr := &exactReader{r: io.LimitReader(r, plainSize), want: plainSize}
	encReader, hdr, err := crypto.NewEncryptReader(lr, masterKey)
	if err != nil {
		return nil, err
	}
	cipherLen := crypto.CipherSize(plainSize)
	var cipherBuf bytes.Buffer
	cipherBuf.Grow(int(cipherLen))
	if _, err := io.Copy(&cipherBuf, encReader); err != nil {
		return nil, err
	}
	if lr.read != plainSize {
		return nil, io.ErrUnexpectedEOF
	}
	return e.packer.StageSmallFile(ctx, parentID, name, mimeType, plainSize, append([]byte(nil), hdr[:]...), cipherBuf.Bytes(), packMaxBytes, idleDur)
}

type exactReader struct {
	r    io.Reader
	want int64
	read int64
}

func (er *exactReader) Read(p []byte) (int, error) {
	n, err := er.r.Read(p)
	er.read += int64(n)
	return n, err
}

func (e *Engine) deleteBestEffort(msgIDs []int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.tg.DeleteMessages(ctx, msgIDs); err == nil {
		_ = e.store.ClearPendingDeletes(msgIDs)
	}
}

func (e *Engine) FlushPendingDeletes(ctx context.Context) error {
	mids, err := e.store.PendingDeletes()
	if err != nil || len(mids) == 0 {
		return err
	}
	if err := e.tg.DeleteMessages(ctx, mids); err != nil {
		return err
	}
	return e.store.ClearPendingDeletes(mids)
}

func (e *Engine) DeleteToTrash(nodeID int64) error {
	return e.store.SoftDelete(nodeID)
}

func (e *Engine) DeletePermanent(ctx context.Context, nodeID int64) error {
	mids, err := e.store.PermanentDelete(nodeID)
	if err != nil {
		return err
	}
	if len(mids) > 0 && e.tg.IsAuthorized() {
		go e.deleteBestEffort(mids)
	}
	return nil
}

func (e *Engine) EmptyTrash(ctx context.Context) error {
	mids, err := e.store.EmptyTrash()
	if err != nil {
		return err
	}
	if len(mids) > 0 && e.tg.IsAuthorized() {
		go e.deleteBestEffort(mids)
	}
	return nil
}

func (e *Engine) RenameOrMove(ctx context.Context, nodeID, newParentID int64, newName string) error {
	replaced, err := e.store.RenameOrMove(nodeID, newParentID, newName)
	if err != nil {
		return err
	}
	if len(replaced) > 0 && e.tg.IsAuthorized() {
		go e.deleteBestEffort(replaced)
	}
	return nil
}

// FileReader provides O(1) random-access seekable plaintext reading across both multi-part large files
// and packed small files (serving seamlessly from RAM if unflushed or from Telegram + ChunkCache once flushed).
type FileReader struct {
	ctx        context.Context
	engine     *Engine
	node       *db.Node
	parts      []db.FilePart
	partStarts []int64
	decryptors []*crypto.PartDecryptor
	pos        int64
	trID       string
	closed     bool

	cachedPartIdx  int
	cachedBlockIdx uint64
	cachedPlain    []byte
	hasCachedBlock bool
	cipherScratch  []byte

	prefetchCtx    context.Context
	prefetchCancel context.CancelFunc
	prefetchWg     sync.WaitGroup
	lastPartIdx    int
	lastChunkIdx   int64
	seqChunks      int
}

func (e *Engine) OpenFile(ctx context.Context, node *db.Node) (*FileReader, error) {
	if node == nil || node.IsDir {
		return nil, db.ErrNotFound
	}
	masterKey, _, ok := e.state.MasterKey()
	if !ok {
		return nil, ErrNotReady
	}
	parts, err := e.store.GetFileParts(node.ID)
	if err != nil {
		return nil, err
	}

	starts := make([]int64, len(parts))
	decs := make([]*crypto.PartDecryptor, len(parts))
	var acc int64
	for i := range parts {
		starts[i] = acc
		acc += parts[i].PlainSize
		dec, err := crypto.NewPartDecryptor(masterKey, parts[i].PartHeader)
		if err != nil {
			return nil, fmt.Errorf("init decryptor part %d: %w", i, err)
		}
		decs[i] = dec
	}

	trID := ""
	if node.Size > 0 {
		trID = e.tg.Tracker().StartTransfer("download", node.Name, node.Size)
	}
	prefetchCtx, prefetchCancel := context.WithCancel(ctx)

	return &FileReader{
		ctx:            ctx,
		engine:         e,
		node:           node,
		parts:          parts,
		partStarts:     starts,
		decryptors:     decs,
		trID:           trID,
		cachedPartIdx:  -1,
		cipherScratch:  make([]byte, crypto.CipherBlockSize),
		prefetchCtx:    prefetchCtx,
		prefetchCancel: prefetchCancel,
		lastPartIdx:    -1,
		lastChunkIdx:   -1,
	}, nil
}

func (fr *FileReader) Close() error {
	if !fr.closed {
		fr.closed = true
		if fr.prefetchCancel != nil {
			fr.prefetchCancel()
		}
		fr.prefetchWg.Wait()
		if fr.trID != "" {
			fr.engine.tg.Tracker().EndTransfer(fr.trID)
		}
	}
	return nil
}

func (fr *FileReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = fr.pos + offset
	case io.SeekEnd:
		target = fr.node.Size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if target < 0 {
		return 0, errors.New("negative seek position")
	}
	fr.pos = target
	return fr.pos, nil
}

func (fr *FileReader) Read(p []byte) (int, error) {
	n, err := fr.ReadAt(p, fr.pos)
	fr.pos += int64(n)
	return n, err
}

func (fr *FileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= fr.node.Size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	totalRead := 0
	curOff := off
	for totalRead < len(p) && curOff < fr.node.Size {
		partIdx := fr.findPart(curOff)
		if partIdx < 0 {
			break
		}
		part := &fr.parts[partIdx]
		relOff := curOff - fr.partStarts[partIdx]
		blockIdx := uint64(relOff / crypto.PlainBlockSize)
		inBlockOff := int(relOff % crypto.PlainBlockSize)

		var plain []byte
		if fr.hasCachedBlock && fr.cachedPartIdx == partIdx && fr.cachedBlockIdx == blockIdx {
			plain = fr.cachedPlain
		} else {
			cOff, cLen, err := crypto.BlockCipherBounds(part.PlainSize, blockIdx)
			if err != nil {
				return totalRead, err
			}
			if int(cLen) > cap(fr.cipherScratch) {
				fr.cipherScratch = make([]byte, cLen)
			}
			cipherBuf := fr.cipherScratch[:cLen]
			fr.recordAndPrefetch(partIdx, part, cOff)
			if err := fr.engine.readPartCipherRaw(fr.ctx, part, cOff, cipherBuf); err != nil {
				return totalRead, err
			}
			decPlain, err := fr.decryptors[partIdx].DecryptBlock(blockIdx, cipherBuf, fr.cachedPlain[:0])
			if err != nil {
				return totalRead, err
			}
			fr.cachedPartIdx = partIdx
			fr.cachedBlockIdx = blockIdx
			fr.cachedPlain = decPlain
			fr.hasCachedBlock = true
			plain = decPlain
		}

		if inBlockOff >= len(plain) {
			break
		}
		n := copy(p[totalRead:], plain[inBlockOff:])
		totalRead += n
		curOff += int64(n)
		if fr.trID != "" {
			fr.engine.tg.Tracker().AddTransferBytes(fr.trID, int64(n))
		}
	}

	if totalRead == 0 {
		return 0, io.EOF
	}
	if curOff >= fr.node.Size && totalRead < len(p) {
		return totalRead, io.EOF
	}
	return totalRead, nil
}

func (fr *FileReader) findPart(off int64) int {
	idx := sort.Search(len(fr.partStarts), func(i int) bool {
		return fr.partStarts[i]+fr.parts[i].PlainSize > off
	})
	if idx >= len(fr.parts) {
		return -1
	}
	return idx
}

func (fr *FileReader) recordAndPrefetch(partIdx int, part *db.FilePart, cipherOff int64) {
	if part.TGMsgID <= 0 || part.CipherSize <= 0 || fr.prefetchCtx.Err() != nil {
		return
	}
	chunkIdx := (part.PackOffset + cipherOff) / tg.TGChunkSize
	if partIdx == fr.lastPartIdx && chunkIdx == fr.lastChunkIdx {
		return
	}
	if partIdx == fr.lastPartIdx && chunkIdx == fr.lastChunkIdx+1 {
		fr.seqChunks++
	} else {
		fr.seqChunks = 0
	}
	fr.lastPartIdx = partIdx
	fr.lastChunkIdx = chunkIdx

	// Start 4-chunk (2 MiB) async read-ahead once 3 consecutive chunks confirm sequential streaming.
	if fr.seqChunks < 2 || fr.engine.cache.MaxBytes() < 8*tg.TGChunkSize {
		return
	}
	maxChunk := (part.PackOffset + part.CipherSize - 1) / tg.TGChunkSize
	for ahead := int64(1); ahead <= 4; ahead++ {
		nextIdx := chunkIdx + ahead
		if nextIdx > maxChunk {
			break
		}
		if fr.engine.cache.HasOrInflight(part.TGMsgID, nextIdx) {
			continue
		}
		partCopy := *part
		fr.prefetchWg.Add(1)
		go func(p db.FilePart, idx int64) {
			defer fr.prefetchWg.Done()
			_, _ = fr.engine.fetchPartChunk(fr.prefetchCtx, &p, idx)
		}(partCopy, nextIdx)
	}
}

// readPartCipherRaw reads ciphertext bytes [cipherOff : cipherOff+len(dst)] of a FilePart.
// If the part belongs to an in-RAM Pack not yet flushed to Telegram (or still in memPacks), it serves directly from RAM;
// otherwise it reads from PackOffset + cipherOff in the Telegram message via the aligned 512 KiB ChunkCache.
func (e *Engine) readPartCipherRaw(ctx context.Context, part *db.FilePart, cipherOff int64, dst []byte) error {
	if part.PackID != "" {
		if n, inMem := e.packer.ReadSlice(part.PackID, part.PackOffset+cipherOff, dst); inMem {
			if n < len(dst) {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		// If pack flushed between OpenFile() and this read, reload fresh TGMsgID/FileID from SQLite once!
		if part.TGMsgID == 0 {
			freshParts, err := e.store.GetFileParts(part.NodeID)
			if err != nil {
				return err
			}
			for _, fp := range freshParts {
				if fp.ID == part.ID || fp.PartIndex == part.PartIndex {
					*part = fp
					break
				}
			}
			if part.TGMsgID == 0 {
				return errors.New("staged pack data unavailable")
			}
		}
	}

	filled := 0
	pos := part.PackOffset + cipherOff
	for filled < len(dst) {
		chunkIdx := pos / tg.TGChunkSize
		inChunkOff := int(pos % tg.TGChunkSize)

		chunkData, err := e.fetchPartChunk(ctx, part, chunkIdx)
		if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
			// Another reader cancelled its own prefetch for this chunk; retry once with our live ctx.
			chunkData, err = e.fetchPartChunk(ctx, part, chunkIdx)
		}
		if err != nil {
			return err
		}
		if inChunkOff >= len(chunkData) {
			return io.ErrUnexpectedEOF
		}
		n := copy(dst[filled:], chunkData[inChunkOff:])
		filled += n
		pos += int64(n)
	}
	return nil
}

func (e *Engine) fetchPartChunk(ctx context.Context, part *db.FilePart, chunkIdx int64) ([]byte, error) {
	return e.cache.GetOrFetch(part.TGMsgID, chunkIdx, func() ([]byte, error) {
		offset := chunkIdx * tg.TGChunkSize
		return e.fetchChunkWithFailover(ctx, part, offset)
	})
}

// BackupDBToTelegram flushes any open RAM Pack first so the snapshot contains real Telegram message IDs,
// then gzips & encrypts the SQLite DB to the storage channel.
func (e *Engine) BackupDBToTelegram(ctx context.Context) (*tg.UploadedDoc, error) {
	if !e.IsReady() {
		return nil, ErrNotReady
	}
	masterKey, kdfSalt, ok := e.state.MasterKey()
	if !ok {
		return nil, ErrNotReady
	}

	_ = e.packer.FlushNow(ctx)

	e.backupMu.Lock()
	defer e.backupMu.Unlock()

	rawSQLite, err := e.store.SnapshotBytes(e.tmpDir)
	if err != nil {
		return nil, err
	}
	enc, err := crypto.EncryptDBBackup(rawSQLite, masterKey, kdfSalt)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s%s.tmdb", DBBackupPrefix, time.Now().UTC().Format("20060102-150405"))
	doc, err := e.tg.UploadBlob(ctx, name, "TeleMount encrypted SQLite metadata backup", bytes.NewReader(enc), int64(len(enc)), 2, nil)
	if err != nil {
		return nil, err
	}
	_ = e.store.RecordBackup(doc.MsgID, time.Now().Unix())
	e.MirrorMsgAsync(doc.MsgID, true)

	if docs, err := e.tg.ListChannelDocs(ctx, DBBackupPrefix, 50); err == nil && len(docs) > DBBackupKeepCount {
		sort.Slice(docs, func(i, j int) bool { return docs[i].MsgID > docs[j].MsgID })
		var oldIDs []int
		for _, d := range docs[DBBackupKeepCount:] {
			oldIDs = append(oldIDs, d.MsgID)
		}
		_ = e.tg.DeleteMessages(ctx, oldIDs)
	}
	return doc, nil
}

func isBackupFileName(name string) bool {
	return strings.HasPrefix(name, DBBackupPrefix) || strings.HasPrefix(name, legacyDBBackupPrefix)
}

func (e *Engine) ListChannelBackups(ctx context.Context) ([]tg.ChannelDoc, error) {
	docs, err := e.tg.ListChannelDocs(ctx, DBBackupPrefix, 30)
	if err != nil {
		return nil, err
	}
	// Also fall back to legacy TeleDrive backups when restoring an existing pre-rename channel.
	if len(docs) == 0 {
		if legacyDocs, lerr := e.tg.ListChannelDocs(ctx, legacyDBBackupPrefix, 30); lerr == nil && len(legacyDocs) > 0 {
			docs = append(docs, legacyDocs...)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].MsgID > docs[j].MsgID })
	return docs, nil
}

func (e *Engine) RestoreDBFromTelegram(ctx context.Context, msgID int, passphrase string) error {
	if !e.tg.IsAuthorized() || !e.state.HasChannel() {
		return errors.New("connect Telegram and select a channel first")
	}
	if passphrase == "" {
		return errors.New("encryption key is required")
	}
	backups, err := e.ListChannelBackups(ctx)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		return errors.New("no TeleMount DB backup found in this channel")
	}
	target := backups[0]
	if msgID > 0 {
		found := false
		for _, b := range backups {
			if b.MsgID == msgID {
				target = b
				found = true
				break
			}
		}
		if !found {
			return errors.New("requested backup message not found")
		}
	}

	blob, err := e.tg.DownloadSmallDoc(ctx, target)
	if err != nil {
		return fmt.Errorf("download backup: %w", err)
	}
	sqliteBytes, masterKey, kdfSalt, err := crypto.DecryptDBBackup(blob, passphrase)
	if err != nil {
		return err
	}
	if err := e.store.RestoreFromBytes(sqliteBytes, e.tmpDir); err != nil {
		return err
	}
	// If restoring on a new VPS using a Backup Account's channel, automatically remap file_parts & packs to that channel's replica IDs!
	_, _ = e.store.RemapChannelReplicasOnRestore(e.state.Snapshot().ChannelID)
	if err := e.state.SetMasterKey(masterKey, kdfSalt); err != nil {
		return err
	}
	e.cache.Clear()
	if settings, err := e.store.GetSettings(); err == nil && settings.RAMCacheMB > 0 {
		e.cache.SetMaxBytes(int64(settings.RAMCacheMB) * 1024 * 1024)
	}
	return nil
}

type OrphanReport struct {
	ScannedDocs int             `json:"scanned_docs"`
	Orphans     []tg.ChannelDoc `json:"orphans"`
	TotalBytes  int64           `json:"total_bytes"`
}

func (e *Engine) ScanOrphans(ctx context.Context) (*OrphanReport, error) {
	if !e.IsReady() {
		return nil, ErrNotReady
	}
	_ = e.packer.FlushNow(ctx)

	docs, err := e.tg.ListChannelDocs(ctx, "", 10000)
	if err != nil {
		return nil, err
	}
	referenced, err := e.store.AllReferencedMsgIDs()
	if err != nil {
		return nil, err
	}

	rep := &OrphanReport{
		ScannedDocs: len(docs),
		Orphans:     make([]tg.ChannelDoc, 0),
	}
	for _, d := range docs {
		if isBackupFileName(d.FileName) {
			continue
		}
		if !strings.HasSuffix(d.FileName, ".bin") {
			continue
		}
		if _, ok := referenced[d.MsgID]; !ok {
			rep.Orphans = append(rep.Orphans, d)
			rep.TotalBytes += d.Size
		}
	}
	return rep, nil
}

func (e *Engine) CleanOrphans(ctx context.Context, msgIDs []int) (int, error) {
	if !e.IsReady() {
		return 0, ErrNotReady
	}
	referenced, err := e.store.AllReferencedMsgIDs()
	if err != nil {
		return 0, err
	}
	safe := make([]int, 0, len(msgIDs))
	for _, mid := range msgIDs {
		if _, ok := referenced[mid]; !ok {
			safe = append(safe, mid)
		}
	}
	if len(safe) == 0 {
		return 0, nil
	}
	if err := e.tg.DeleteMessages(ctx, safe); err != nil {
		return 0, err
	}
	_ = e.store.ClearPendingDeletes(safe)
	return len(safe), nil
}

func randomBlobName() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) + ".bin"
}
