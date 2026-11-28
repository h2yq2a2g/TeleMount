package dav

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"telemount/internal/db"
	"telemount/internal/storage"
)

type ctxKey int

const (
	ctxKeyContentLength ctxKey = iota
	ctxKeyContentType
)

type authFailTracker struct {
	mu      sync.Mutex
	entries map[string][]time.Time
}

func (a *authFailTracker) isBlocked(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.entries[ip][:0]
	for _, ts := range a.entries[ip] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(a.entries, ip)
		return false
	}
	a.entries[ip] = kept
	return len(kept) >= 20
}

func (a *authFailTracker) recordFailure(ip string) {
	a.mu.Lock()
	if a.entries == nil {
		a.entries = make(map[string][]time.Time)
	}
	a.entries[ip] = append(a.entries[ip], time.Now())
	a.mu.Unlock()
}

func (a *authFailTracker) clear(ip string) {
	a.mu.Lock()
	delete(a.entries, ip)
	a.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

type Handler struct {
	engine   *storage.Engine
	dav      *webdav.Handler
	authFail authFailTracker
}

func NewHandler(prefix string, engine *storage.Engine) *Handler {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		prefix = "/dav"
	}
	return &Handler{
		engine: engine,
		dav: &webdav.Handler{
			Prefix:     prefix,
			FileSystem: &tgFS{engine: engine},
			LockSystem: webdav.NewMemLS(),
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.engine.IsReady() {
		http.Error(w, "TeleMount setup is not complete", http.StatusServiceUnavailable)
		return
	}

	ip := clientIP(r)
	if h.authFail.isBlocked(ip) {
		http.Error(w, "Too Many Failed Authentication Attempts", http.StatusTooManyRequests)
		return
	}

	user, pass, ok := r.BasicAuth()
	if !ok || !h.engine.Store().VerifyWebDAV(user, pass) {
		if ok {
			h.authFail.recordFailure(ip)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="TeleMount WebDAV"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.authFail.clear(ip)

	// Pass HTTP PUT Content-Length and Content-Type through context so OpenFile can stream directly on RAM.
	ctx := context.WithValue(r.Context(), ctxKeyContentLength, r.ContentLength)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		ctx = context.WithValue(ctx, ctxKeyContentType, ct)
	}
	h.dav.ServeHTTP(w, r.WithContext(ctx))
}

type tgFS struct {
	engine *storage.Engine
}

func mapDBErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, db.ErrAlreadyExist):
		return os.ErrExist
	case errors.Is(err, db.ErrNotDir), errors.Is(err, db.ErrInvalidName), errors.Is(err, db.ErrInvalidMove):
		return os.ErrInvalid
	default:
		return err
	}
}

func (f *tgFS) Mkdir(_ context.Context, name string, _ os.FileMode) error {
	parent, base, err := f.engine.Store().ResolveParent(name)
	if err != nil {
		return mapDBErr(err)
	}
	_, err = f.engine.Store().Mkdir(parent.ID, base)
	return mapDBErr(err)
}

func (f *tgFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	node, err := f.engine.Store().StatByPath(name)
	if err != nil {
		return nil, mapDBErr(err)
	}
	return &nodeFileInfo{node: node}, nil
}

// RemoveAll permanently deletes via Telegram because WebDAV DELETE bypasses Trash.
func (f *tgFS) RemoveAll(ctx context.Context, name string) error {
	node, err := f.engine.Store().StatByPath(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return mapDBErr(err)
	}
	if node.ID == db.RootID {
		return os.ErrPermission
	}
	return mapDBErr(f.engine.DeletePermanent(ctx, node.ID))
}

func (f *tgFS) Rename(ctx context.Context, oldName, newName string) error {
	oldNode, err := f.engine.Store().StatByPath(oldName)
	if err != nil {
		return mapDBErr(err)
	}
	newParent, newBase, err := f.engine.Store().ResolveParent(newName)
	if err != nil {
		return mapDBErr(err)
	}
	return mapDBErr(f.engine.RenameOrMove(ctx, oldNode.ID, newParent.ID, newBase))
}

func (f *tgFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	existing, statErr := f.engine.Store().StatByPath(name)

	// Allow PROPPATCH / read-write metadata opens on existing directories without treating them as file PUTs.
	if statErr == nil && existing.IsDir {
		if flag&(os.O_CREATE|os.O_TRUNC|os.O_WRONLY) != 0 {
			return nil, os.ErrExist
		}
		return &davDirFile{engine: f.engine, node: existing}, nil
	}

	isWrite := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0
	if isWrite {
		parent, base, err := f.engine.Store().ResolveParent(name)
		if err != nil {
			return nil, mapDBErr(err)
		}
		knownSize := int64(-1)
		if v, ok := ctx.Value(ctxKeyContentLength).(int64); ok {
			knownSize = v
		}
		mimeType, _ := ctx.Value(ctxKeyContentType).(string)
		return newDavWriteFile(ctx, f.engine, parent.ID, base, mimeType, knownSize), nil
	}

	if statErr != nil {
		return nil, mapDBErr(statErr)
	}
	return &davReadFile{ctx: ctx, engine: f.engine, node: existing}, nil
}

// nodeFileInfo implements os.FileInfo, webdav.ETager, and webdav.ContentTyper.
type nodeFileInfo struct {
	node *db.Node
}

func (fi *nodeFileInfo) Name() string {
	if fi.node.Name == "" {
		return "/"
	}
	return fi.node.Name
}
func (fi *nodeFileInfo) Size() int64 { return fi.node.Size }
func (fi *nodeFileInfo) Mode() fs.FileMode {
	if fi.node.IsDir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (fi *nodeFileInfo) ModTime() time.Time { return time.Unix(fi.node.UpdatedAt, 0) }
func (fi *nodeFileInfo) IsDir() bool        { return fi.node.IsDir }
func (fi *nodeFileInfo) Sys() any           { return nil }
func (fi *nodeFileInfo) ETag(_ context.Context) (string, error) {
	if fi.node.ETag == "" {
		return "", webdav.ErrNotImplemented
	}
	return `"` + fi.node.ETag + `"`, nil
}
func (fi *nodeFileInfo) ContentType(_ context.Context) (string, error) {
	if fi.node.IsDir {
		return "", webdav.ErrNotImplemented
	}
	if fi.node.MimeType == "" {
		return "application/octet-stream", nil
	}
	return fi.node.MimeType, nil
}

// davDirFile supports PROPFIND directory listing.
type davDirFile struct {
	engine   *storage.Engine
	node     *db.Node
	children []os.FileInfo
	loaded   bool
	offset   int
}

func (d *davDirFile) Close() error                       { return nil }
func (d *davDirFile) Read([]byte) (int, error)           { return 0, os.ErrInvalid }
func (d *davDirFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (d *davDirFile) Seek(int64, int) (int64, error)     { return 0, os.ErrInvalid }
func (d *davDirFile) Stat() (os.FileInfo, error)         { return &nodeFileInfo{node: d.node}, nil }
func (d *davDirFile) Readdir(count int) ([]os.FileInfo, error) {
	if !d.loaded {
		nodes, err := d.engine.Store().ListChildren(d.node.ID)
		if err != nil {
			return nil, err
		}
		d.children = make([]os.FileInfo, len(nodes))
		for i := range nodes {
			n := nodes[i]
			d.children[i] = &nodeFileInfo{node: &n}
		}
		d.loaded = true
	}
	if count <= 0 {
		rem := d.children[d.offset:]
		d.offset = len(d.children)
		return rem, nil
	}
	if d.offset >= len(d.children) {
		return nil, io.EOF
	}
	end := d.offset + count
	if end > len(d.children) {
		end = len(d.children)
	}
	res := d.children[d.offset:end]
	d.offset = end
	return res, nil
}

// davReadFile lazily opens the underlying encrypted FileReader on first Read so PROPFIND/HEAD avoid Telegram RPCs.
type davReadFile struct {
	ctx    context.Context
	engine *storage.Engine
	node   *db.Node
	pos    int64
	reader *storage.FileReader
}

func (f *davReadFile) ensureReader() error {
	if f.reader != nil {
		return nil
	}
	r, err := f.engine.OpenFile(f.ctx, f.node)
	if err != nil {
		return err
	}
	if f.pos > 0 {
		if _, err := r.Seek(f.pos, io.SeekStart); err != nil {
			r.Close()
			return err
		}
	}
	f.reader = r
	return nil
}

func (f *davReadFile) Read(p []byte) (int, error) {
	if err := f.ensureReader(); err != nil {
		return 0, err
	}
	n, err := f.reader.Read(p)
	f.pos += int64(n)
	return n, err
}

func (f *davReadFile) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = f.pos + offset
	case io.SeekEnd:
		target = f.node.Size + offset
	default:
		return 0, os.ErrInvalid
	}
	if target < 0 {
		return 0, os.ErrInvalid
	}
	f.pos = target
	if f.reader != nil {
		return f.reader.Seek(target, io.SeekStart)
	}
	return f.pos, nil
}

func (f *davReadFile) Write([]byte) (int, error)           { return 0, os.ErrPermission }
func (f *davReadFile) Readdir(int) ([]os.FileInfo, error)  { return nil, os.ErrInvalid }
func (f *davReadFile) Stat() (os.FileInfo, error)          { return &nodeFileInfo{node: f.node}, nil }
func (f *davReadFile) Close() error {
	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}

// davWriteFile streams WebDAV PUT bytes straight through an io.Pipe into Engine.UploadFile without SSD spooling.
type davWriteFile struct {
	name      string
	pw        *io.PipeWriter
	done      chan struct{}
	once      sync.Once
	node      *db.Node
	uploadErr error
}

func newDavWriteFile(ctx context.Context, engine *storage.Engine, parentID int64, name, mimeType string, knownSize int64) *davWriteFile {
	pr, pw := io.Pipe()
	wf := &davWriteFile{
		name: name,
		pw:   pw,
		done: make(chan struct{}),
	}
	go func() {
		defer close(wf.done)
		defer pr.Close()
		node, err := engine.UploadFile(ctx, parentID, name, mimeType, pr, knownSize)
		wf.node = node
		wf.uploadErr = err
		if err != nil {
			pr.CloseWithError(err)
		}
	}()
	return wf
}

func (w *davWriteFile) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *davWriteFile) finalize() error {
	w.once.Do(func() {
		_ = w.pw.Close()
		<-w.done
	})
	return w.uploadErr
}

// Stat finalizes the pipe upload so x/net/webdav's handlePut gets the real committed ETag and FileInfo.
func (w *davWriteFile) Stat() (os.FileInfo, error) {
	if err := w.finalize(); err != nil {
		return nil, err
	}
	if w.node == nil {
		return nil, os.ErrInvalid
	}
	return &nodeFileInfo{node: w.node}, nil
}

func (w *davWriteFile) Close() error {
	return w.finalize()
}

func (w *davWriteFile) Read([]byte) (int, error)          { return 0, os.ErrPermission }
func (w *davWriteFile) Seek(int64, int) (int64, error)    { return 0, os.ErrInvalid }
func (w *davWriteFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
