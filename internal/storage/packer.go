package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"telemount/internal/db"
)

type memPack struct {
	id    string
	buf   []byte
	files int
}

type StagedPackStatus struct {
	BufferedBytes int64 `json:"buffered_bytes"`
	BufferedFiles int   `json:"buffered_files"`
	InFlightPacks int   `json:"inflight_packs"`
}

type CompactReport struct {
	CompactedPacks int   `json:"compacted_packs"`
	CreatedPacks   int   `json:"created_packs"`
	RelocatedFiles int   `json:"relocated_files"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
}

// Packer coalesces encrypted small files in RAM into larger Duplicati-style Pack blobs
// and flushes them to Telegram when reaching PackMaxMB or after PackIdleSec of inactivity.
type Packer struct {
	engine *Engine

	mu        sync.RWMutex
	current   *memPack
	memPacks  map[string]*memPack // holds open + currently-uploading packs so reads never miss
	idleTimer *time.Timer

	// Semaphore of capacity 1 bounds concurrent pack uploads so peak RAM never runs away under heavy bursts.
	flushSem  chan struct{}
	compactMu sync.Mutex
}

func newPacker(engine *Engine) *Packer {
	return &Packer{
		engine:   engine,
		memPacks: make(map[string]*memPack),
		flushSem: make(chan struct{}, 1),
	}
}

func newPackID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "pack-" + hex.EncodeToString(b)
}

func (p *Packer) Status() StagedPackStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var st StagedPackStatus
	if p.current != nil {
		st.BufferedBytes = int64(len(p.current.buf))
		st.BufferedFiles = p.current.files
	}
	inflight := len(p.memPacks)
	if p.current != nil && inflight > 0 {
		inflight--
	}
	st.InFlightPacks = inflight
	return st
}

// ReadSlice serves ciphertext directly from RAM for files whose Pack has not finished uploading to Telegram yet.
func (p *Packer) ReadSlice(packID string, offset int64, dst []byte) (int, bool) {
	p.mu.RLock()
	mp, ok := p.memPacks[packID]
	if !ok {
		p.mu.RUnlock()
		return 0, false
	}
	buf := mp.buf
	p.mu.RUnlock()

	if offset < 0 || offset >= int64(len(buf)) {
		return 0, true
	}
	n := copy(dst, buf[offset:])
	return n, true
}

// StageSmallFile appends an already-encrypted TMP1 file segment into the active RAM Pack,
// commits the SQLite node + file_part BEFORE any flush of that pack can execute,
// and triggers size-threshold or idle-timer flushing.
func (p *Packer) StageSmallFile(
	ctx context.Context,
	parentID int64,
	name, mimeType string,
	plainSize int64,
	partHeader []byte,
	cipherSeg []byte,
	packMaxBytes int64,
	idleDur time.Duration,
) (*db.Node, error) {
	segLen := int64(len(cipherSeg))

	// Detach previous pack if adding this segment would overflow packMaxBytes.
	p.mu.Lock()
	var overflowPack *memPack
	if p.current != nil && len(p.current.buf) > 0 && int64(len(p.current.buf))+segLen > packMaxBytes {
		overflowPack = p.detachCurrentLocked()
	}
	p.mu.Unlock()

	if overflowPack != nil {
		// Synchronous flush of the previous full pack provides clean RAM backpressure during bursts.
		if err := p.flushPack(ctx, overflowPack); err != nil {
			return nil, err
		}
	}

	p.mu.Lock()
	if p.current == nil {
		mp := &memPack{id: newPackID()}
		p.current = mp
		p.memPacks[mp.id] = mp
	}
	mp := p.current
	packID := mp.id
	packOffset := int64(len(mp.buf))
	mp.buf = append(mp.buf, cipherSeg...)
	mp.files++

	// Insert SQLite metadata while still holding p.mu (fast <0.1ms WAL commit) so FinalizePackUpload
	// is guaranteed to see every file_part belonging to packID.
	node, replaced, err := p.engine.store.UpsertFileWithParts(parentID, name, plainSize, mimeType, []db.FilePart{
		{
			PartIndex:  0,
			TGMsgID:    0, // filled in atomically when Pack flushes to Telegram
			PlainSize:  plainSize,
			CipherSize: segLen,
			PartHeader: partHeader,
			PackID:     packID,
			PackOffset: packOffset,
		},
	})
	if err != nil {
		// Roll back appended bytes if DB insert failed (e.g. name conflict with directory).
		mp.buf = mp.buf[:packOffset]
		mp.files--
		if len(mp.buf) == 0 {
			delete(p.memPacks, mp.id)
			p.current = nil
		}
		p.mu.Unlock()
		return nil, err
	}

	var readyToFlush *memPack
	if int64(len(mp.buf)) >= packMaxBytes {
		readyToFlush = p.detachCurrentLocked()
	} else {
		p.resetIdleTimerLocked(idleDur)
	}
	p.mu.Unlock()

	if len(replaced) > 0 {
		go p.engine.deleteBestEffort(replaced)
	}
	if readyToFlush != nil {
		if err := p.flushPack(ctx, readyToFlush); err != nil {
			return nil, err
		}
	}
	return node, nil
}

func (p *Packer) resetIdleTimerLocked(d time.Duration) {
	if d <= 0 {
		d = 10 * time.Second
	}
	if p.idleTimer != nil {
		p.idleTimer.Stop()
	}
	p.idleTimer = time.AfterFunc(d, func() {
		_ = p.FlushNow(context.Background())
	})
}

func (p *Packer) detachCurrentLocked() *memPack {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	mp := p.current
	p.current = nil
	return mp
}

// FlushNow seals the active open RAM pack (if any) and uploads it to Telegram immediately.
func (p *Packer) FlushNow(ctx context.Context) error {
	p.mu.Lock()
	mp := p.detachCurrentLocked()
	p.mu.Unlock()
	if mp == nil || len(mp.buf) == 0 {
		if mp != nil {
			p.removeMemPack(mp.id)
		}
		return nil
	}
	return p.flushPack(ctx, mp)
}

func (p *Packer) removeMemPack(id string) {
	p.mu.Lock()
	delete(p.memPacks, id)
	p.mu.Unlock()
}

func (p *Packer) flushPack(ctx context.Context, mp *memPack) error {
	select {
	case p.flushSem <- struct{}{}:
		defer func() { <-p.flushSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// Skip Telegram upload entirely if every file in this staged pack was already deleted/overwritten before flush!
	liveCount, err := p.engine.store.CountLivePartsInPack(mp.id)
	if err == nil && liveCount == 0 {
		p.removeMemPack(mp.id)
		return nil
	}

	settings, _ := p.engine.store.GetSettings()
	threads := settings.UploadThreads
	if threads <= 0 {
		threads = 4
	}

	totalSize := int64(len(mp.buf))
	blobName := mp.id + ".bin"
	trID := p.engine.tg.Tracker().StartTransfer("upload", fmt.Sprintf("Pack (%d files)", mp.files), totalSize)
	defer p.engine.tg.Tracker().EndTransfer(trID)

	doc, err := p.engine.tg.UploadBlob(ctx, blobName, "", bytes.NewReader(mp.buf), totalSize, threads, func(done int64) {
		p.engine.tg.Tracker().UpdateTransfer(trID, done)
	})
	if err != nil {
		// Keep mp in memPacks so reads continue from RAM, and schedule automatic background retry if network blipped.
		if ctx.Err() == nil {
			time.AfterFunc(15*time.Second, func() {
				_ = p.flushPack(context.Background(), mp)
			})
		}
		return fmt.Errorf("upload pack %s: %w", mp.id, err)
	}

	bound, err := p.engine.store.FinalizePackUpload(mp.id, doc.MsgID, doc.FileID, doc.AccessHash, doc.FileRef, doc.DCID, totalSize)
	if err != nil {
		return err
	}
	// Only evict from RAM AFTER SQLite file_parts point to the committed Telegram message ID!
	p.removeMemPack(mp.id)

	if bound == 0 {
		go p.engine.deleteBestEffort([]int{doc.MsgID})
	} else {
		p.engine.MirrorMsgAsync(doc.MsgID, false)
	}
	return nil
}

// CompactPacks merges under-filled packs and reclaims dead space from packs where files were deleted,
// copying already-encrypted TMP1 segments directly in RAM without re-encryption or SSD writes.
func (p *Packer) CompactPacks(ctx context.Context) (*CompactReport, error) {
	if !p.engine.IsReady() {
		return nil, ErrNotReady
	}
	p.compactMu.Lock()
	defer p.compactMu.Unlock()

	if err := p.FlushNow(ctx); err != nil {
		return nil, err
	}

	settings, err := p.engine.store.GetSettings()
	if err != nil {
		return nil, err
	}
	packMaxBytes := int64(settings.PackMaxMB) * 1024 * 1024
	if packMaxBytes <= 0 {
		packMaxBytes = 1024 * 1024 * 1024
	}
	threads := settings.UploadThreads
	if threads <= 0 {
		threads = 4
	}

	allPacks, err := p.engine.store.ListPacks()
	if err != nil {
		return nil, err
	}

	var fragmented []db.PackInfo
	var small []db.PackInfo
	rep := &CompactReport{}

	for _, pk := range allPacks {
		if pk.LiveFiles == 0 || pk.LiveSize == 0 {
			// Completely dead pack: clean up immediately.
			_, _ = p.engine.store.CommitCompactedPack("", 0, 0, 0, nil, 0, 0, nil, []string{pk.PackID})
			rep.CompactedPacks++
			rep.ReclaimedBytes += pk.TotalSize
			continue
		}
		wasteRatio := float64(pk.DeadSize) / float64(pk.TotalSize)
		if pk.DeadSize > 0 && (wasteRatio >= 0.25 || pk.DeadSize >= 32*1024*1024) {
			fragmented = append(fragmented, pk)
		} else if pk.TotalSize < packMaxBytes/2 {
			small = append(small, pk)
		}
	}

	candidates := append([]db.PackInfo{}, fragmented...)
	// Only include clean small packs if we have at least 2 packs overall to merge together.
	if len(fragmented)+len(small) >= 2 {
		candidates = append(candidates, small...)
	}
	if len(candidates) == 0 {
		_ = p.engine.FlushPendingDeletes(ctx)
		return rep, nil
	}

	// Group candidates into batches whose live bytes fit within packMaxBytes.
	var batch []db.PackInfo
	var batchLive int64
	flushBatch := func(b []db.PackInfo) error {
		if len(b) == 0 {
			return nil
		}
		// A single unfragmented small pack alone has nothing to merge with and 0 dead bytes to reclaim.
		if len(b) == 1 && b[0].DeadSize == 0 {
			return nil
		}

		var buf bytes.Buffer
		var relocated []db.RelocatedPart
		var srcIDs []string
		var oldTotal int64

		for _, pk := range b {
			srcIDs = append(srcIDs, pk.PackID)
			oldTotal += pk.TotalSize
			parts, err := p.engine.store.ListPartsByPack(pk.PackID)
			if err != nil {
				return err
			}
			for i := range parts {
				pt := &parts[i]
				newOff := int64(buf.Len())
				seg := make([]byte, pt.CipherSize)
				if err := p.engine.readPartCipherRaw(ctx, pt, 0, seg); err != nil {
					return fmt.Errorf("read live part %d from pack %s: %w", pt.ID, pk.PackID, err)
				}
				buf.Write(seg)
				relocated = append(relocated, db.RelocatedPart{PartID: pt.ID, NewOffset: newOff})
			}
		}

		if len(relocated) == 0 {
			return nil
		}

		newPackID := newPackID()
		newSize := int64(buf.Len())
		trID := p.engine.tg.Tracker().StartTransfer("upload", fmt.Sprintf("Compact (%d files)", len(relocated)), newSize)
		doc, err := p.engine.tg.UploadBlob(ctx, newPackID+".bin", "", bytes.NewReader(buf.Bytes()), newSize, threads, func(done int64) {
			p.engine.tg.Tracker().UpdateTransfer(trID, done)
		})
		p.engine.tg.Tracker().EndTransfer(trID)
		if err != nil {
			return err
		}

		oldMsgs, err := p.engine.store.CommitCompactedPack(newPackID, doc.MsgID, doc.FileID, doc.AccessHash, doc.FileRef, doc.DCID, newSize, relocated, srcIDs)
		if err != nil {
			_ = p.engine.tg.DeleteMessages(context.Background(), []int{doc.MsgID})
			return err
		}
		if len(oldMsgs) > 0 {
			go p.engine.deleteBestEffort(oldMsgs)
		}
		p.engine.MirrorMsgAsync(doc.MsgID, false)

		rep.CompactedPacks += len(b)
		rep.CreatedPacks++
		rep.RelocatedFiles += len(relocated)
		if oldTotal > newSize {
			rep.ReclaimedBytes += oldTotal - newSize
		}
		return nil
	}

	for _, pk := range candidates {
		if len(batch) > 0 && batchLive+pk.LiveSize > packMaxBytes {
			if err := flushBatch(batch); err != nil {
				return rep, err
			}
			batch = nil
			batchLive = 0
		}
		batch = append(batch, pk)
		batchLive += pk.LiveSize
	}
	if err := flushBatch(batch); err != nil {
		return rep, err
	}

	_ = p.engine.FlushPendingDeletes(ctx)
	return rep, nil
}

// Ensure io import used cleanly if needed.
var _ = io.EOF
