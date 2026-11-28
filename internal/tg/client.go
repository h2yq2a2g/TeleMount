package tg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	// Telegram aligned download chunk size (512 KiB divides 1 MiB cleanly and satisfies upload.getFile rules).
	TGChunkSize = 512 * 1024
)

var (
	ErrNotConnected    = errors.New("telegram client not connected")
	ErrNotAuthorized   = errors.New("telegram account not authorized")
	ErrNoChannel       = errors.New("storage channel not selected")
	ErrPasswordNeeded  = errors.New("telegram 2FA password required")
	ErrFileRefExpired  = errors.New("telegram file reference expired")
)

type DocRef struct {
	MsgID      int
	FileID     int64
	AccessHash int64
	FileRef    []byte
	DCID       int
}

type UploadedDoc struct {
	MsgID      int
	FileID     int64
	AccessHash int64
	FileRef    []byte
	DCID       int
	Size       int64
}

type ChannelInfo struct {
	ID         int64  `json:"id"`
	AccessHash int64  `json:"access_hash,string"`
	Title      string `json:"title"`
}

type ChannelDoc struct {
	MsgID      int    `json:"msg_id"`
	FileName   string `json:"file_name"`
	Caption    string `json:"caption"`
	FileID     int64  `json:"file_id,string"`
	AccessHash int64  `json:"access_hash,string"`
	FileRef    []byte `json:"-"`
	DCID       int    `json:"dc_id"`
	Size       int64  `json:"size"`
	Date       int    `json:"date"`
}

type QRState struct {
	Status    string `json:"status"` // "idle", "pending", "password_required", "authorized", "error"
	QRDataURL string `json:"qr_data_url,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Backend is the minimal Telegram transport interface consumed by the storage engine.
type Backend interface {
	IsAuthorized() bool
	Tracker() *Tracker
	UploadBlob(ctx context.Context, fileName, caption string, r io.Reader, size int64, threads int, onProgress func(done int64)) (*UploadedDoc, error)
	ForwardCopyDoc(ctx context.Context, srcChannelID, srcAccessHash int64, srcMsgID int) (*UploadedDoc, error)
	FetchChunk(ctx context.Context, loc DocRef, offset int64, limit int) ([]byte, error)
	RefreshFileRef(ctx context.Context, msgID int) ([]byte, error)
	DeleteMessages(ctx context.Context, msgIDs []int) error
	ListChannelDocs(ctx context.Context, prefix string, limit int) ([]ChannelDoc, error)
	DownloadSmallDoc(ctx context.Context, doc ChannelDoc) ([]byte, error)
}

type Manager struct {
	dataDir     string
	sessionFile string
	tracker     *Tracker

	mu          sync.RWMutex
	apiID       int
	apiHash     string
	channelID   int64
	channelHash int64

	client     *telegram.Client
	dispatcher tg.UpdateDispatcher
	runCancel  context.CancelFunc
	runDone    chan struct{}
	readyCh    chan struct{}

	// Phone auth in-flight state
	phone         string
	phoneCodeHash string

	// QR login in-flight state
	qrMu     sync.Mutex
	qrCancel context.CancelFunc
	qrState  QRState
}

func NewManager(dataDir string) *Manager {
	return NewManagerWithSession(dataDir, "tg_session.json")
}

func NewManagerWithSession(dataDir, sessionFile string) *Manager {
	if sessionFile == "" {
		sessionFile = "tg_session.json"
	}
	return &Manager{
		dataDir:     dataDir,
		sessionFile: sessionFile,
		tracker:     NewTracker(),
		qrState:     QRState{Status: "idle"},
	}
}

func (m *Manager) Tracker() *Tracker {
	return m.tracker
}

func (m *Manager) SetChannel(id, accessHash int64) {
	m.mu.Lock()
	m.channelID = id
	m.channelHash = accessHash
	m.mu.Unlock()
}

func (m *Manager) channelPeer() (*tg.InputPeerChannel, *tg.InputChannel, error) {
	m.mu.RLock()
	id, hash := m.channelID, m.channelHash
	m.mu.RUnlock()
	if id == 0 {
		return nil, nil, ErrNoChannel
	}
	return &tg.InputPeerChannel{ChannelID: id, AccessHash: hash},
		&tg.InputChannel{ChannelID: id, AccessHash: hash}, nil
}

func (m *Manager) IsAuthorized() bool {
	return m.tracker.Snapshot().Authorized
}

// Start connects (or reconnects) the gotd MTProto client using the given Telegram API credentials.
func (m *Manager) Start(parentCtx context.Context, apiID int, apiHash string) error {
	if apiID <= 0 || strings.TrimSpace(apiHash) == "" {
		return errors.New("api_id and api_hash are required")
	}

	m.Stop()

	ctx, cancel := context.WithCancel(parentCtx)
	readyCh := make(chan struct{})
	runDone := make(chan struct{})
	dispatcher := tg.NewUpdateDispatcher()

	waiter := floodwait.NewWaiter().
		WithMaxWait(5 * time.Minute).
		WithMaxRetries(5).
		WithServerErrorRetries(3).
		WithCallback(func(_ context.Context, fw floodwait.FloodWait) {
			name := fw.Name
			if name == "" {
				name = fmt.Sprintf("0x%x", fw.TypeID)
			}
			m.tracker.RecordFloodWait(name, fw.Duration)
		})

	sessionPath := filepath.Join(m.dataDir, m.sessionFile)
	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		UpdateHandler:  dispatcher,
		FilePoolSize:   4,
		Middlewares:    []telegram.Middleware{waiter},
		OnConnectionState: func(cs telegram.ConnectionState) {
			connected := cs == telegram.ConnectionStateReady
			m.tracker.SetConnectionState(cs.String(), connected)
		},
	})

	m.mu.Lock()
	m.apiID = apiID
	m.apiHash = apiHash
	m.client = client
	m.dispatcher = dispatcher
	m.runCancel = cancel
	m.runDone = runDone
	m.readyCh = readyCh
	m.mu.Unlock()

	m.tracker.SetConnectionState("connecting", false)
	m.tracker.SetLastError("")

	var readyOnce sync.Once
	markReady := func() { readyOnce.Do(func() { close(readyCh) }) }

	go func() {
		defer close(runDone)
		defer markReady()
		defer m.tracker.SetConnectionState("disconnected", false)

		err := waiter.Run(ctx, func(wctx context.Context) error {
			return client.Run(wctx, func(rctx context.Context) error {
				m.tracker.SetConnectionState("ready", true)
				if st, err := client.Auth().Status(rctx); err == nil && st.Authorized {
					m.refreshSelf(rctx, client)
				} else {
					m.tracker.SetAuthorized(false, nil)
				}
				markReady()
				<-rctx.Done()
				return rctx.Err()
			})
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			m.tracker.SetLastError(err.Error())
		}
	}()

	select {
	case <-readyCh:
		if errMsg := m.tracker.Snapshot().LastError; errMsg != "" && !m.tracker.Snapshot().Connected {
			cancel()
			return errors.New(errMsg)
		}
		return nil
	case <-time.After(20 * time.Second):
		cancel()
		return errors.New("timed out connecting to Telegram")
	case <-parentCtx.Done():
		cancel()
		return parentCtx.Err()
	}
}

func (m *Manager) Stop() {
	m.cancelQR()
	m.mu.Lock()
	cancel := m.runCancel
	done := m.runDone
	m.runCancel = nil
	m.client = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	m.tracker.SetConnectionState("disconnected", false)
	m.tracker.SetAuthorized(false, nil)
}

func (m *Manager) getClient() (*telegram.Client, error) {
	m.mu.RLock()
	c := m.client
	m.mu.RUnlock()
	if c == nil {
		return nil, ErrNotConnected
	}
	return c, nil
}

func (m *Manager) refreshSelf(ctx context.Context, client *telegram.Client) {
	self, err := client.Self(ctx)
	if err != nil {
		return
	}
	m.tracker.SetAuthorized(true, &UserInfo{
		ID:        self.ID,
		FirstName: self.FirstName,
		LastName:  self.LastName,
		Username:  self.Username,
		Phone:     self.Phone,
	})
}

func (m *Manager) cancelQR() {
	m.qrMu.Lock()
	if m.qrCancel != nil {
		m.qrCancel()
		m.qrCancel = nil
	}
	m.qrMu.Unlock()
}

// StartQRLogin starts a background Telegram QR login session and renders the login token as an offline PNG data URL.
func (m *Manager) StartQRLogin(parentCtx context.Context) error {
	c, err := m.getClient()
	if err != nil {
		return err
	}
	m.cancelQR()

	m.mu.RLock()
	disp := m.dispatcher
	m.mu.RUnlock()

	qrCtx, cancel := context.WithTimeout(parentCtx, 3*time.Minute)
	m.qrMu.Lock()
	m.qrCancel = cancel
	m.qrState = QRState{Status: "pending"}
	m.qrMu.Unlock()

	loggedIn := qrlogin.OnLoginToken(disp)

	go func() {
		defer cancel()
		_, err := c.QR().Auth(qrCtx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
			png, err := qrcode.Encode(token.URL(), qrcode.Medium, 256)
			if err != nil {
				return err
			}
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			m.qrMu.Lock()
			m.qrState = QRState{
				Status:    "pending",
				QRDataURL: dataURL,
				ExpiresAt: token.Expires().Unix(),
			}
			m.qrMu.Unlock()
			return nil
		})
		if err != nil {
			if errors.Is(err, auth.ErrPasswordAuthNeeded) || isTGError(err, "SESSION_PASSWORD_NEEDED") {
				m.qrMu.Lock()
				m.qrState = QRState{Status: "password_required"}
				m.qrMu.Unlock()
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			m.qrMu.Lock()
			m.qrState = QRState{Status: "error", Error: err.Error()}
			m.qrMu.Unlock()
			return
		}
		m.refreshSelf(context.Background(), c)
		m.qrMu.Lock()
		m.qrState = QRState{Status: "authorized"}
		m.qrMu.Unlock()
	}()

	return nil
}

func (m *Manager) QRStatus() QRState {
	m.qrMu.Lock()
	defer m.qrMu.Unlock()
	return m.qrState
}

func (m *Manager) SendPhoneCode(ctx context.Context, phone string) error {
	c, err := m.getClient()
	if err != nil {
		return err
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errors.New("phone number is required")
	}
	sent, err := c.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return err
	}
	sc, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return errors.New("unexpected auth.sentCode response")
	}
	m.mu.Lock()
	m.phone = phone
	m.phoneCodeHash = sc.PhoneCodeHash
	m.mu.Unlock()
	return nil
}

func (m *Manager) SignInCode(ctx context.Context, code string) error {
	c, err := m.getClient()
	if err != nil {
		return err
	}
	m.mu.RLock()
	phone, hash := m.phone, m.phoneCodeHash
	m.mu.RUnlock()
	if phone == "" || hash == "" {
		return errors.New("send phone code first")
	}
	_, err = c.Auth().SignIn(ctx, phone, strings.TrimSpace(code), hash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) || isTGError(err, "SESSION_PASSWORD_NEEDED") {
		return ErrPasswordNeeded
	}
	if err != nil {
		return err
	}
	m.refreshSelf(ctx, c)
	return nil
}

func (m *Manager) SignInPassword(ctx context.Context, password string) error {
	c, err := m.getClient()
	if err != nil {
		return err
	}
	if _, err := c.Auth().Password(ctx, password); err != nil {
		return err
	}
	m.refreshSelf(ctx, c)
	m.qrMu.Lock()
	m.qrState = QRState{Status: "authorized"}
	m.qrMu.Unlock()
	return nil
}

func (m *Manager) Logout(ctx context.Context) error {
	if c, err := m.getClient(); err == nil {
		_, _ = c.LogOut(ctx)
	}
	m.Stop()
	_ = os.Remove(filepath.Join(m.dataDir, m.sessionFile))
	m.qrMu.Lock()
	m.qrState = QRState{Status: "idle"}
	m.qrMu.Unlock()
	return nil
}

// fetchDialogChannels paginates through the user's dialogs (up to maxPages*100) and returns deduplicated channels.
func (m *Manager) fetchDialogChannels(ctx context.Context, maxPages int) ([]*tg.Channel, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if maxPages <= 0 {
		maxPages = 10
	}

	seen := make(map[int64]struct{})
	var out []*tg.Channel
	var offsetDate, offsetID int
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}

	for page := 0; page < maxPages; page++ {
		dialogsRes, err := c.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      100,
		})
		if err != nil {
			return nil, err
		}

		var (
			chats    []tg.ChatClass
			users    []tg.UserClass
			dialogs  []tg.DialogClass
			messages []tg.MessageClass
			isSlice  bool
		)
		switch d := dialogsRes.(type) {
		case *tg.MessagesDialogs:
			chats, users, dialogs, messages = d.Chats, d.Users, d.Dialogs, d.Messages
		case *tg.MessagesDialogsSlice:
			chats, users, dialogs, messages = d.Chats, d.Users, d.Dialogs, d.Messages
			isSlice = true
		}

		channelByID := make(map[int64]*tg.Channel, len(chats))
		for _, ch := range chats {
			if channel, ok := ch.(*tg.Channel); ok {
				channelByID[channel.ID] = channel
				if _, dup := seen[channel.ID]; !dup {
					seen[channel.ID] = struct{}{}
					out = append(out, channel)
				}
			}
		}

		if !isSlice || len(dialogs) < 100 {
			break
		}
		nextDate, nextID, nextPeer, ok := nextDialogCursor(dialogs, messages, users, channelByID)
		if !ok || (nextDate == offsetDate && nextID == offsetID) {
			break
		}
		offsetDate, offsetID, offsetPeer = nextDate, nextID, nextPeer
	}
	return out, nil
}

func nextDialogCursor(dialogs []tg.DialogClass, messages []tg.MessageClass, users []tg.UserClass, channels map[int64]*tg.Channel) (int, int, tg.InputPeerClass, bool) {
	msgDate := make(map[int]int, len(messages))
	for _, mc := range messages {
		switch m := mc.(type) {
		case *tg.Message:
			msgDate[m.ID] = m.Date
		case *tg.MessageService:
			msgDate[m.ID] = m.Date
		}
	}
	userHash := make(map[int64]int64, len(users))
	for _, uc := range users {
		if u, ok := uc.(*tg.User); ok {
			userHash[u.ID] = u.AccessHash
		}
	}

	for i := len(dialogs) - 1; i >= 0; i-- {
		d, ok := dialogs[i].(*tg.Dialog)
		if !ok {
			continue
		}
		date := msgDate[d.TopMessage]
		switch p := d.Peer.(type) {
		case *tg.PeerChannel:
			if ch, ok := channels[p.ChannelID]; ok {
				return date, d.TopMessage, &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, true
			}
		case *tg.PeerChat:
			return date, d.TopMessage, &tg.InputPeerChat{ChatID: p.ChatID}, true
		case *tg.PeerUser:
			if h, ok := userHash[p.UserID]; ok {
				return date, d.TopMessage, &tg.InputPeerUser{UserID: p.UserID, AccessHash: h}, true
			}
		}
	}
	return 0, 0, nil, false
}

// ListPrivateChannels returns broadcast channels where the logged-in user is creator or admin.
func (m *Manager) ListPrivateChannels(ctx context.Context) ([]ChannelInfo, error) {
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	channels, err := m.fetchDialogChannels(ctx, 10)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelInfo, 0, len(channels))
	for _, channel := range channels {
		if !channel.Broadcast {
			continue
		}
		if !channel.Creator && channel.AdminRights.Zero() {
			continue
		}
		out = append(out, ChannelInfo{
			ID:         channel.ID,
			AccessHash: channel.AccessHash,
			Title:      channel.Title,
		})
	}
	return out, nil
}

// CreatePrivateChannel creates a new private broadcast channel for TeleMount storage.
func (m *Manager) CreatePrivateChannel(ctx context.Context, title string) (*ChannelInfo, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "TeleMount Storage"
	}
	upd, err := c.API().ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Broadcast: true,
		Title:     title,
		About:     "TeleMount encrypted storage channel",
	})
	if err != nil {
		return nil, err
	}
	var chats []tg.ChatClass
	switch u := upd.(type) {
	case *tg.Updates:
		chats = u.Chats
	case *tg.UpdatesCombined:
		chats = u.Chats
	}
	for _, ch := range chats {
		if channel, ok := ch.(*tg.Channel); ok {
			return &ChannelInfo{
				ID:         channel.ID,
				AccessHash: channel.AccessHash,
				Title:      channel.Title,
			}, nil
		}
	}
	return nil, errors.New("channel created but not found in response")
}

type progressFunc func(ctx context.Context, state uploader.ProgressState) error

func (f progressFunc) Chunk(ctx context.Context, state uploader.ProgressState) error {
	return f(ctx, state)
}

// UploadBlob streams encrypted bytes directly from RAM into Telegram using parallel 512 KiB MTProto parts.
func (m *Manager) UploadBlob(ctx context.Context, fileName, caption string, r io.Reader, size int64, threads int, onProgress func(done int64)) (*UploadedDoc, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	peer, _, err := m.channelPeer()
	if err != nil {
		return nil, err
	}
	if threads < 1 {
		threads = 4
	}

	u := uploader.NewUploader(c.API()).
		WithPartSize(uploader.MaximumPartSize).
		WithThreads(threads)
	if onProgress != nil {
		u = u.WithProgress(progressFunc(func(_ context.Context, st uploader.ProgressState) error {
			onProgress(st.Uploaded)
			return nil
		}))
	}

	up := uploader.NewUpload(fileName, r, size)
	inputFile, err := u.Upload(ctx, up)
	if err != nil {
		return nil, fmt.Errorf("telegram upload: %w", err)
	}

	rndID := randomInt64()
	media := &tg.InputMediaUploadedDocument{
		File:     inputFile,
		MimeType: "application/octet-stream",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: fileName},
		},
	}

	updates, err := c.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Silent:   true,
		Peer:     peer,
		Media:    media,
		Message:  caption,
		RandomID: rndID,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram sendMedia: %w", err)
	}

	doc, err := extractUploadedDoc(updates)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func extractUploadedDoc(updates tg.UpdatesClass) (*UploadedDoc, error) {
	var list []tg.UpdateClass
	switch u := updates.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShortSentMessage:
		if doc, ok := docFromMedia(u.ID, u.Media); ok {
			return doc, nil
		}
	}
	for _, upd := range list {
		switch m := upd.(type) {
		case *tg.UpdateNewChannelMessage:
			if msg, ok := m.Message.(*tg.Message); ok {
				if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
					return doc, nil
				}
			}
		case *tg.UpdateNewMessage:
			if msg, ok := m.Message.(*tg.Message); ok {
				if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
					return doc, nil
				}
			}
		}
	}
	return nil, errors.New("uploaded document metadata not found in Telegram response")
}

func docFromMedia(msgID int, media tg.MessageMediaClass) (*UploadedDoc, bool) {
	md, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	doc, ok := md.Document.(*tg.Document)
	if !ok {
		return nil, false
	}
	return &UploadedDoc{
		MsgID:      msgID,
		FileID:     doc.ID,
		AccessHash: doc.AccessHash,
		FileRef:    append([]byte(nil), doc.FileReference...),
		DCID:       doc.DCID,
		Size:       doc.Size,
	}, true
}

// FetchChunk downloads a single aligned chunk from Telegram, transparently handling DC migration.
func (m *Manager) FetchChunk(ctx context.Context, loc DocRef, offset int64, limit int) ([]byte, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}

	dcID := loc.DCID
	for attempt := 0; attempt < 2; attempt++ {
		var inv tg.Invoker = c
		if dcID > 0 {
			if fileInv, err := c.FileDC(ctx, dcID); err == nil {
				inv = fileInv
			}
		}
		api := tg.NewClient(inv)
		res, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise: true,
			Location: &tg.InputDocumentFileLocation{
				ID:            loc.FileID,
				AccessHash:    loc.AccessHash,
				FileReference: loc.FileRef,
			},
			Offset: offset,
			Limit:  limit,
		})
		if err != nil {
			if isTGError(err, "FILE_REFERENCE_EXPIRED") || isTGError(err, "FILE_REFERENCE_INVALID") {
				return nil, ErrFileRefExpired
			}
			if migrateDC, ok := tgerr.AsType(err, "FILE_MIGRATE"); ok && migrateDC.Argument > 0 && attempt == 0 {
				dcID = migrateDC.Argument
				continue
			}
			return nil, err
		}
		switch f := res.(type) {
		case *tg.UploadFile:
			return f.Bytes, nil
		default:
			return nil, fmt.Errorf("unexpected upload.getFile response %T", res)
		}
	}
	return nil, errors.New("telegram chunk fetch failed after DC migration")
}

// RefreshFileRef reloads a message from the storage channel to obtain a fresh Telegram file_reference.
func (m *Manager) RefreshFileRef(ctx context.Context, msgID int) ([]byte, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	_, ch, err := m.channelPeer()
	if err != nil {
		return nil, err
	}
	res, err := c.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: ch,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch mm := res.(type) {
	case *tg.MessagesChannelMessages:
		msgs = mm.Messages
	case *tg.MessagesMessages:
		msgs = mm.Messages
	case *tg.MessagesMessagesSlice:
		msgs = mm.Messages
	}
	for _, mc := range msgs {
		msg, ok := mc.(*tg.Message)
		if !ok || msg.ID != msgID {
			continue
		}
		if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
			return doc.FileRef, nil
		}
	}
	return nil, errors.New("message or document no longer exists on Telegram channel")
}

// DeleteMessages deletes channel messages in batches of 100.
func (m *Manager) DeleteMessages(ctx context.Context, msgIDs []int) error {
	if len(msgIDs) == 0 {
		return nil
	}
	c, err := m.getClient()
	if err != nil {
		return err
	}
	_, ch, err := m.channelPeer()
	if err != nil {
		return err
	}
	const batchSize = 100
	for i := 0; i < len(msgIDs); i += batchSize {
		end := i + batchSize
		if end > len(msgIDs) {
			end = len(msgIDs)
		}
		if _, err := c.API().ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: ch,
			ID:      msgIDs[i:end],
		}); err != nil {
			return err
		}
	}
	return nil
}

// ListChannelDocs returns documents in the storage channel matching prefix (or all if prefix == "").
// When prefix != "", it uses Telegram server-side MessagesSearch (O(1) RPC instead of scanning full channel history).
// When limit <= 0, all matching messages are returned without an artificial cap.
func (m *Manager) ListChannelDocs(ctx context.Context, prefix string, limit int) ([]ChannelDoc, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	peer, _, err := m.channelPeer()
	if err != nil {
		return nil, err
	}

	var out []ChannelDoc
	offsetID := 0
	for limit <= 0 || len(out) < limit {
		var (
			res tg.MessagesMessagesClass
			err error
		)
		if prefix != "" {
			res, err = c.API().MessagesSearch(ctx, &tg.MessagesSearchRequest{
				Peer:     peer,
				Q:        prefix,
				Filter:   &tg.InputMessagesFilterDocument{},
				OffsetID: offsetID,
				Limit:    100,
			})
		} else {
			res, err = c.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     peer,
				OffsetID: offsetID,
				Limit:    100,
			})
		}
		if err != nil {
			return nil, err
		}
		var msgs []tg.MessageClass
		switch h := res.(type) {
		case *tg.MessagesChannelMessages:
			msgs = h.Messages
		case *tg.MessagesMessages:
			msgs = h.Messages
		case *tg.MessagesMessagesSlice:
			msgs = h.Messages
		}
		if len(msgs) == 0 {
			break
		}
		lastID := 0
		for _, mc := range msgs {
			msg, ok := mc.(*tg.Message)
			if !ok {
				continue
			}
			lastID = msg.ID
			doc, ok := docFromMedia(msg.ID, msg.Media)
			if !ok {
				continue
			}
			fname := extractFileName(msg.Media)
			if prefix != "" && !strings.HasPrefix(fname, prefix) {
				continue
			}
			out = append(out, ChannelDoc{
				MsgID:      msg.ID,
				FileName:   fname,
				Caption:    msg.Message,
				FileID:     doc.FileID,
				AccessHash: doc.AccessHash,
				FileRef:    doc.FileRef,
				DCID:       doc.DCID,
				Size:       doc.Size,
				Date:       msg.Date,
			})
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		if lastID == 0 || lastID == offsetID || len(msgs) < 100 {
			break
		}
		offsetID = lastID
	}
	return out, nil
}

// CheckMessages verifies in batches of 100 which msgIDs currently exist as document messages in the storage channel
// and returns a map of msgID -> document byte size.
func (m *Manager) CheckMessages(ctx context.Context, msgIDs []int) (map[int]int64, error) {
	if len(msgIDs) == 0 {
		return map[int]int64{}, nil
	}
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	_, ch, err := m.channelPeer()
	if err != nil {
		return nil, err
	}

	found := make(map[int]int64, len(msgIDs))
	const batchSize = 100
	for i := 0; i < len(msgIDs); i += batchSize {
		end := i + batchSize
		if end > len(msgIDs) {
			end = len(msgIDs)
		}
		ids := make([]tg.InputMessageClass, 0, end-i)
		for _, mid := range msgIDs[i:end] {
			ids = append(ids, &tg.InputMessageID{ID: mid})
		}
		res, err := c.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: ch,
			ID:      ids,
		})
		if err != nil {
			return nil, err
		}
		var msgs []tg.MessageClass
		switch mm := res.(type) {
		case *tg.MessagesChannelMessages:
			msgs = mm.Messages
		case *tg.MessagesMessages:
			msgs = mm.Messages
		case *tg.MessagesMessagesSlice:
			msgs = mm.Messages
		}
		for _, mc := range msgs {
			msg, ok := mc.(*tg.Message)
			if !ok {
				continue
			}
			if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
				found[msg.ID] = doc.Size
			}
		}
	}
	return found, nil
}

const maxSmallDocBytes int64 = 512 * 1024 * 1024

// DownloadSmallDoc downloads an entire small document (e.g., encrypted DB backup) into RAM.
func (m *Manager) DownloadSmallDoc(ctx context.Context, doc ChannelDoc) ([]byte, error) {
	if doc.Size > maxSmallDocBytes {
		return nil, fmt.Errorf("document size %d exceeds small-doc limit (%d)", doc.Size, maxSmallDocBytes)
	}
	var buf bytes.Buffer
	if doc.Size > 0 && doc.Size <= 64*1024*1024 {
		buf.Grow(int(doc.Size))
	}
	ref := DocRef{
		MsgID:      doc.MsgID,
		FileID:     doc.FileID,
		AccessHash: doc.AccessHash,
		FileRef:    doc.FileRef,
		DCID:       doc.DCID,
	}
	var offset int64
	for {
		chunk, err := m.FetchChunk(ctx, ref, offset, TGChunkSize)
		if errors.Is(err, ErrFileRefExpired) {
			fresh, rerr := m.RefreshFileRef(ctx, doc.MsgID)
			if rerr != nil {
				return nil, rerr
			}
			ref.FileRef = fresh
			chunk, err = m.FetchChunk(ctx, ref, offset, TGChunkSize)
		}
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			break
		}
		if offset+int64(len(chunk)) > maxSmallDocBytes {
			return nil, fmt.Errorf("document exceeded small-doc limit (%d bytes)", maxSmallDocBytes)
		}
		buf.Write(chunk)
		offset += int64(len(chunk))
		if len(chunk) < TGChunkSize || (doc.Size > 0 && offset >= doc.Size) {
			break
		}
	}
	return buf.Bytes(), nil
}

// ExportOneTimeInvite creates a single-use, short-lived invite link for the active storage channel so a backup account can join.
func (m *Manager) ExportOneTimeInvite(ctx context.Context) (string, error) {
	c, err := m.getClient()
	if err != nil {
		return "", err
	}
	if !m.IsAuthorized() {
		return "", ErrNotAuthorized
	}
	peer, _, err := m.channelPeer()
	if err != nil {
		return "", err
	}
	req := &tg.MessagesExportChatInviteRequest{Peer: peer}
	req.SetUsageLimit(1)
	req.SetExpireDate(int(time.Now().Add(10 * time.Minute).Unix()))
	req.SetTitle("TeleMount Backup Sync")

	inv, err := c.API().MessagesExportChatInvite(ctx, req)
	if err != nil {
		return "", fmt.Errorf("export channel invite: %w", err)
	}
	exp, ok := inv.(*tg.ChatInviteExported)
	if !ok || exp.Link == "" {
		return "", errors.New("unexpected invite response from Telegram")
	}
	return exp.Link, nil
}

// JoinChannelByInvite joins a private channel via invite link/hash and returns this account's own ChannelInfo (including per-user AccessHash).
func (m *Manager) JoinChannelByInvite(ctx context.Context, linkOrHash string, expectedChannelID int64) (*ChannelInfo, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	hash := parseInviteHash(linkOrHash)
	if hash == "" {
		return nil, errors.New("invalid invite link")
	}

	upd, err := c.API().MessagesImportChatInvite(ctx, hash)
	if err != nil {
		if isTGError(err, "USER_ALREADY_PARTICIPANT") && expectedChannelID != 0 {
			return m.FindDialogChannel(ctx, expectedChannelID)
		}
		return nil, fmt.Errorf("join source channel: %w", err)
	}
	var chats []tg.ChatClass
	if joinOk, ok := upd.(*tg.MessagesChatInviteJoinResultOk); ok {
		switch u := joinOk.Updates.(type) {
		case *tg.Updates:
			chats = u.Chats
		case *tg.UpdatesCombined:
			chats = u.Chats
		}
	}
	for _, ch := range chats {
		if channel, ok := ch.(*tg.Channel); ok {
			return &ChannelInfo{
				ID:         channel.ID,
				AccessHash: channel.AccessHash,
				Title:      channel.Title,
			}, nil
		}
	}
	if expectedChannelID != 0 {
		return m.FindDialogChannel(ctx, expectedChannelID)
	}
	return nil, errors.New("joined channel not found in response")
}

// FindDialogChannel locates a broadcast channel (even as a non-admin member) in this account's dialogs.
func (m *Manager) FindDialogChannel(ctx context.Context, channelID int64) (*ChannelInfo, error) {
	channels, err := m.fetchDialogChannels(ctx, 10)
	if err != nil {
		return nil, err
	}
	for _, channel := range channels {
		if channel.ID == channelID {
			return &ChannelInfo{
				ID:         channel.ID,
				AccessHash: channel.AccessHash,
				Title:      channel.Title,
			}, nil
		}
	}
	return nil, fmt.Errorf("channel %d not found in account dialogs", channelID)
}

// ForwardCopyDoc performs a zero-bandwidth Telegram server-side copy (DropAuthor: true)
// of message srcMsgID from (srcChannelID, srcAccessHash) into this account's active own channel.
func (m *Manager) ForwardCopyDoc(ctx context.Context, srcChannelID, srcAccessHash int64, srcMsgID int) (*UploadedDoc, error) {
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	dstPeer, _, err := m.channelPeer()
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesForwardMessagesRequest{
		Silent:     true,
		DropAuthor: true,
		FromPeer:   &tg.InputPeerChannel{ChannelID: srcChannelID, AccessHash: srcAccessHash},
		ID:         []int{srcMsgID},
		RandomID:   []int64{randomInt64()},
		ToPeer:     dstPeer,
	}
	req.SetFlags()
	updates, err := c.API().MessagesForwardMessages(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("server-side copy msg %d: %w", srcMsgID, err)
	}
	return extractUploadedDoc(updates)
}

// ForwardCopyDocsBatch server-side copies up to 100 messages per RPC and returns srcMsgID -> UploadedDoc.
func (m *Manager) ForwardCopyDocsBatch(ctx context.Context, srcChannelID, srcAccessHash int64, srcMsgIDs []int) (map[int]*UploadedDoc, error) {
	if len(srcMsgIDs) == 0 {
		return map[int]*UploadedDoc{}, nil
	}
	c, err := m.getClient()
	if err != nil {
		return nil, err
	}
	if !m.IsAuthorized() {
		return nil, ErrNotAuthorized
	}
	dstPeer, _, err := m.channelPeer()
	if err != nil {
		return nil, err
	}

	rndToSrc := make(map[int64]int, len(srcMsgIDs))
	rndIDs := make([]int64, len(srcMsgIDs))
	for i, mid := range srcMsgIDs {
		r := randomInt64()
		for _, dup := rndToSrc[r]; dup; _, dup = rndToSrc[r] {
			r = randomInt64()
		}
		rndIDs[i] = r
		rndToSrc[r] = mid
	}

	req := &tg.MessagesForwardMessagesRequest{
		Silent:     true,
		DropAuthor: true,
		FromPeer:   &tg.InputPeerChannel{ChannelID: srcChannelID, AccessHash: srcAccessHash},
		ID:         srcMsgIDs,
		RandomID:   rndIDs,
		ToPeer:     dstPeer,
	}
	req.SetFlags()
	updates, err := c.API().MessagesForwardMessages(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("batch server-side copy (%d msgs): %w", len(srcMsgIDs), err)
	}
	return extractBatchUploadedDocs(updates, rndToSrc), nil
}

func extractBatchUploadedDocs(updates tg.UpdatesClass, rndToSrc map[int64]int) map[int]*UploadedDoc {
	var list []tg.UpdateClass
	switch u := updates.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	}

	newIDToSrc := make(map[int]int, len(rndToSrc))
	docsByNewID := make(map[int]*UploadedDoc, len(rndToSrc))
	for _, upd := range list {
		switch u := upd.(type) {
		case *tg.UpdateMessageID:
			if srcID, ok := rndToSrc[u.RandomID]; ok {
				newIDToSrc[u.ID] = srcID
			}
		case *tg.UpdateNewChannelMessage:
			if msg, ok := u.Message.(*tg.Message); ok {
				if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
					docsByNewID[msg.ID] = doc
				}
			}
		case *tg.UpdateNewMessage:
			if msg, ok := u.Message.(*tg.Message); ok {
				if doc, ok := docFromMedia(msg.ID, msg.Media); ok {
					docsByNewID[msg.ID] = doc
				}
			}
		}
	}

	out := make(map[int]*UploadedDoc, len(newIDToSrc))
	for newID, srcID := range newIDToSrc {
		if doc, ok := docsByNewID[newID]; ok {
			out[srcID] = doc
		}
	}
	return out
}

func parseInviteHash(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "+"); idx >= 0 {
		return strings.Trim(s[idx+1:], "/")
	}
	if idx := strings.Index(s, "joinchat/"); idx >= 0 {
		return strings.Trim(s[idx+len("joinchat/"):], "/")
	}
	return s
}

func extractFileName(media tg.MessageMediaClass) string {
	md, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return ""
	}
	doc, ok := md.Document.(*tg.Document)
	if !ok {
		return ""
	}
	for _, attr := range doc.Attributes {
		if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return fn.FileName
		}
	}
	return ""
}

func isTGError(err error, errType string) bool {
	if rpcErr, ok := tgerr.As(err); ok {
		return rpcErr.IsType(errType)
	}
	return false
}

func randomInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}
