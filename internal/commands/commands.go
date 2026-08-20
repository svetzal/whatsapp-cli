package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vicentereig/whatsapp-cli/internal/client"
	"github.com/vicentereig/whatsapp-cli/internal/output"
	"github.com/vicentereig/whatsapp-cli/internal/store"
	"github.com/vicentereig/whatsapp-cli/internal/types"
	"go.mau.fi/whatsmeow/types/events"
)

type App struct {
	client          WAClient
	store           MessageStore
	version         string
	storeDir        string
	mediaDownloader func(ctx context.Context, info store.MessageDownloadInfo, targetPath string) (int64, error)
	mediaWorker     *mediaDownloadWorker
}

// NewApp creates a new App with production dependencies.
func NewApp(storeDir, version string) (*App, error) {
	cli, err := client.NewWAClient(storeDir)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(storeDir, "messages.db")
	st, err := store.NewMessageStore(dbPath)
	if err != nil {
		return nil, err
	}

	// Attach LID resolver for transparent LID<->phone JID mapping
	whatsmeowDBPath := filepath.Join(storeDir, "whatsapp.db")
	if lidResolver, err := store.NewLIDResolver(whatsmeowDBPath); err == nil {
		st.SetLIDResolver(lidResolver)
	}

	app := &App{
		client:   cli,
		store:    st,
		version:  resolveVersion(version, gitDescribe),
		storeDir: storeDir,
	}
	app.mediaDownloader = app.downloadMediaWithClient
	return app, nil
}

// NewAppWithDeps creates a new App with injected dependencies for testing.
func NewAppWithDeps(client WAClient, store MessageStore, storeDir, version string) *App {
	app := &App{
		client:   client,
		store:    store,
		version:  version,
		storeDir: storeDir,
	}
	return app
}

func (a *App) Close() {
	if a.mediaWorker != nil {
		a.mediaWorker.Stop()
	}
	if a.client != nil {
		a.client.Disconnect()
	}
	if a.store != nil {
		a.store.Close()
	}
}

func (a *App) Auth(ctx context.Context) string {
	if a.client.IsAuthenticated() {
		return output.Success(map[string]interface{}{
			"authenticated": true,
			"message":       "Already authenticated",
		})
	}

	if err := a.client.Authenticate(ctx); err != nil {
		return output.Error(err)
	}

	return output.Success(map[string]interface{}{
		"authenticated": true,
		"message":       "Successfully authenticated",
	})
}

func (a *App) ListMessages(chatJID *string, query *string, limit, page int) string {
	messages, err := a.store.ListMessages(store.ListMessagesParams{
		ChatJID: chatJID,
		Query:   query,
		Limit:   limit,
		Page:    page,
	})
	if err != nil {
		return output.Error(err)
	}

	return output.Success(messages)
}

func (a *App) SearchContacts(query string) string {
	contacts, err := a.store.SearchContacts(query)
	if err != nil {
		return output.Error(err)
	}

	return output.Success(contacts)
}

func (a *App) ListChats(query *string, limit, page int) string {
	chats, err := a.store.ListChats(store.ListChatsParams{
		Query: query,
		Limit: limit,
		Page:  page,
	})
	if err != nil {
		return output.Error(err)
	}

	return output.Success(chats)
}

// recipientToJID normalizes a recipient string to a full JID.
func recipientToJID(recipient string) string {
	if strings.Contains(recipient, "@") {
		return recipient
	}
	return recipient + "@s.whatsapp.net"
}

func (a *App) SendMessage(ctx context.Context, recipient, message string) string {
	if err := a.client.Connect(ctx); err != nil {
		return output.Error(err)
	}

	msgID, err := a.client.SendMessage(ctx, recipient, message)
	if err != nil {
		return output.Error(err)
	}

	timestamp := time.Now()
	chatJID := recipientToJID(recipient)

	chatName := a.client.ResolveChatName(ctx, chatJID, nil)
	if chatName == "" {
		chatName = recipient
	}

	if err := a.store.StoreChat(chatJID, chatName, timestamp); err != nil {
		return output.Error(fmt.Errorf("storing chat: %w", err))
	}
	if err := a.store.StoreMessage(
		msgID, chatJID, "me", message, timestamp, true,
		"", "", "", "", "",
		nil, nil, nil, 0,
	); err != nil {
		return output.Error(fmt.Errorf("storing message: %w", err))
	}

	return output.Success(map[string]interface{}{
		"sent":      true,
		"id":        msgID,
		"recipient": recipient,
		"message":   message,
	})
}

// quotedParticipant resolves the JID a quote must name as the author of the
// message being quoted.
//
// The store records an outbound message's sender as "me", and an inbound one's
// as a bare user part with no server. A bare part cannot be assumed to be a
// phone JID: since the LID migration an inbound sender is often a LID user id,
// and appending @s.whatsapp.net to one addresses a different account. In a
// one-to-one chat the author is the chat itself, which sidesteps the guess. A
// group has many authors, so there the stored sender is all we have.
func (a *App) quotedParticipant(quoted store.MessageDownloadInfo, chatJID string) (string, error) {
	if quoted.IsFromMe || quoted.Sender == "me" {
		own := a.client.OwnJID()
		if own == "" {
			return "", errors.New("cannot quote our own message: this device is not paired")
		}
		return own, nil
	}

	if !strings.HasSuffix(chatJID, "@g.us") {
		return chatJID, nil
	}

	if quoted.Sender == "" {
		return "", fmt.Errorf("cannot quote message %s: no sender recorded", quoted.ID)
	}
	if !strings.Contains(quoted.Sender, "@") {
		return "", fmt.Errorf(
			"cannot quote message %s: sender %q has no server and may be a LID or a phone number",
			quoted.ID, quoted.Sender,
		)
	}
	return quoted.Sender, nil
}

// SendReply sends a text message quoting an earlier one. The quoted message is
// read from the local store, so the id must belong to a message this store has
// already synced.
func (a *App) SendReply(ctx context.Context, recipient, message, replyToID string) string {
	chatJID := recipientToJID(recipient)
	quotedMsg, err := a.store.GetMessageForDownload(replyToID, &chatJID)
	if err != nil {
		return output.Error(fmt.Errorf("looking up message %s to reply to: %w", replyToID, err))
	}

	quotedSender, err := a.quotedParticipant(quotedMsg, chatJID)
	if err != nil {
		return output.Error(err)
	}

	if err := a.client.Connect(ctx); err != nil {
		return output.Error(err)
	}

	msgID, err := a.client.SendReply(ctx, recipient, message, types.QuotedContext{
		ID:     quotedMsg.ID,
		Sender: quotedSender,
		Text:   quotedMsg.Content,
	})
	if err != nil {
		return output.Error(err)
	}

	timestamp := time.Now()

	chatName := a.client.ResolveChatName(ctx, chatJID, nil)
	if chatName == "" {
		chatName = recipient
	}

	if err := a.store.StoreChat(chatJID, chatName, timestamp); err != nil {
		return output.Error(fmt.Errorf("storing chat: %w", err))
	}
	if err := a.store.StoreMessage(
		msgID, chatJID, "me", message, timestamp, true,
		"", "", "", "", "",
		nil, nil, nil, 0,
	); err != nil {
		return output.Error(fmt.Errorf("storing message: %w", err))
	}

	return output.Success(map[string]interface{}{
		"sent":        true,
		"id":          msgID,
		"recipient":   recipient,
		"message":     message,
		"in_reply_to": quotedMsg.ID,
		"quoted_text": quotedMsg.Content,
	})
}

func (a *App) SendImage(ctx context.Context, recipient, imagePath, caption string) string {
	if err := a.client.Connect(ctx); err != nil {
		return output.Error(err)
	}

	msgID, err := a.client.SendImageMessage(ctx, recipient, imagePath, caption)
	if err != nil {
		return output.Error(err)
	}

	timestamp := time.Now()
	chatJID := recipientToJID(recipient)

	chatName := a.client.ResolveChatName(ctx, chatJID, nil)
	if chatName == "" {
		chatName = recipient
	}

	content := caption
	if content == "" {
		content = "[Image]"
	}

	if err := a.store.StoreChat(chatJID, chatName, timestamp); err != nil {
		return output.Error(fmt.Errorf("storing chat: %w", err))
	}
	if err := a.store.StoreMessage(
		msgID, chatJID, "me", content, timestamp, true,
		"image", filepath.Base(imagePath), "", "", "",
		nil, nil, nil, 0,
	); err != nil {
		return output.Error(fmt.Errorf("storing message: %w", err))
	}

	return output.Success(map[string]interface{}{
		"sent":      true,
		"id":        msgID,
		"recipient": recipient,
		"image":     imagePath,
		"caption":   caption,
	})
}

func (a *App) DownloadMedia(ctx context.Context, messageID string, chatJID *string, outputPath string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return output.Error(fmt.Errorf("message ID is required"))
	}

	info, err := a.store.GetMessageForDownload(messageID, chatJID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return output.Error(fmt.Errorf("message %s not found", messageID))
		}
		return output.Error(err)
	}

	if strings.TrimSpace(info.MediaType) == "" || strings.TrimSpace(info.DirectPath) == "" || len(info.MediaKey) == 0 {
		return output.Error(fmt.Errorf("message %s has no downloadable media", messageID))
	}

	targetPath, bytesWritten, downloadedAt, err := a.downloadMediaAndPersist(ctx, info, outputPath)
	if err != nil {
		return output.Error(err)
	}

	response := map[string]interface{}{
		"message_id":    messageID,
		"chat_jid":      info.ChatJID,
		"path":          targetPath,
		"bytes":         bytesWritten,
		"media_type":    info.MediaType,
		"mime_type":     info.MimeType,
		"downloaded_at": downloadedAt.Format(time.RFC3339Nano),
	}
	if info.ChatName != nil && *info.ChatName != "" {
		response["chat_name"] = *info.ChatName
	}
	return output.Success(response)
}

func (a *App) resolveOutputPath(info store.MessageDownloadInfo, requested string) (string, error) {
	filename := sanitizeFilename(filenameFor(info))
	if filename == "" {
		filename = "file"
	}

	if strings.TrimSpace(requested) != "" {
		cleaned := requested
		if !filepath.IsAbs(cleaned) {
			if abs, err := filepath.Abs(cleaned); err == nil {
				cleaned = abs
			}
		}
		if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
			return filepath.Join(cleaned, filename), nil
		}
		if strings.HasSuffix(cleaned, string(os.PathSeparator)) {
			return filepath.Join(cleaned, filename), nil
		}
		return cleaned, nil
	}

	baseDir := filepath.Join(a.storeDir, "media", sanitizeSegment(info.ChatJID), sanitizeSegment(info.ID))
	if info.MediaType != "" {
		baseDir = filepath.Join(baseDir, sanitizeSegment(info.MediaType))
	}
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}
	return filepath.Join(baseDir, filename), nil
}

var pathReplacer = strings.NewReplacer(
	"/", "_",
	"\\", "_",
	":", "_",
	"@", "_",
	"?", "_",
	"*", "_",
	"<", "_",
	">", "_",
	"|", "_",
)

func sanitizeSegment(seg string) string {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return "unknown"
	}
	seg = pathReplacer.Replace(seg)
	seg = strings.ReplaceAll(seg, "..", "_")
	return seg
}

const maxFilenameLen = 200 // Leave room for directory path; most filesystems allow 255

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	name = pathReplacer.Replace(name)
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	name = strings.ReplaceAll(name, "..", "_")
	// Truncate if too long (preserve extension if possible)
	if len(name) > maxFilenameLen {
		ext := filepath.Ext(name)
		if len(ext) < 20 && len(ext) > 0 {
			base := name[:maxFilenameLen-len(ext)]
			name = base + ext
		} else {
			name = name[:maxFilenameLen]
		}
	}
	return name
}

func filenameFor(info store.MessageDownloadInfo) string {
	if trimmed := strings.TrimSpace(info.Filename); trimmed != "" {
		return trimmed
	}
	if ext := extensionForMime(info.MimeType); ext != "" {
		return info.ID + ext
	}
	switch strings.ToLower(strings.TrimSpace(info.MediaType)) {
	case "image":
		return info.ID + ".jpg"
	case "video":
		return info.ID + ".mp4"
	case "audio":
		return info.ID + ".ogg"
	case "document":
		return info.ID
	default:
		return info.ID
	}
}

func extensionForMime(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "" {
		return ""
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil {
		for _, ext := range exts {
			switch ext {
			case ".jpe":
				return ".jpg"
			default:
				if ext != "" {
					return ext
				}
			}
		}
	}
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}

func (a *App) downloadMediaWithClient(ctx context.Context, info store.MessageDownloadInfo, targetPath string) (int64, error) {
	if a.client == nil {
		return 0, fmt.Errorf("whatsapp client not initialized")
	}
	if err := a.client.Connect(ctx); err != nil {
		return 0, err
	}
	req := types.MediaDownloadRequest{
		DirectPath:    info.DirectPath,
		MediaKey:      info.MediaKey,
		FileSHA256:    info.FileSHA256,
		FileEncSHA256: info.FileEncSHA256,
		FileLength:    info.FileLength,
		MediaType:     info.MediaType,
		MimeType:      info.MimeType,
	}
	return a.client.DownloadMediaToFile(ctx, req, targetPath)
}

func (a *App) downloadMediaAndPersist(ctx context.Context, info store.MessageDownloadInfo, requestedPath string) (string, int64, time.Time, error) {
	finalPath, err := a.resolveOutputPath(info, requestedPath)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("failed to create destination directory: %w", err)
	}

	downloader := a.mediaDownloader
	if downloader == nil {
		downloader = a.downloadMediaWithClient
	}

	bytesWritten, err := downloader(ctx, info, finalPath)
	if err != nil {
		return "", 0, time.Time{}, err
	}

	now := time.Now().UTC()
	if err := a.store.MarkMediaDownloaded(info.ID, info.ChatJID, finalPath, now); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("failed to mark media downloaded: %w", err)
	}

	return finalPath, bytesWritten, now, nil
}

func (a *App) processMediaJob(ctx context.Context, job mediaJob) error {
	if a.store == nil {
		return fmt.Errorf("message store not initialized")
	}
	info, err := a.store.GetMessageForDownload(job.messageID, &job.chatJID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(info.DirectPath) == "" || len(info.MediaKey) == 0 {
		return nil
	}
	if info.LocalPath != nil {
		if _, err := os.Stat(*info.LocalPath); err == nil {
			return nil
		}
	}
	_, _, _, err = a.downloadMediaAndPersist(ctx, info, "")
	return err
}

type mediaJob struct {
	messageID string
	chatJID   string
}

type mediaDownloadWorker struct {
	app     *App
	workers int
	jobs    chan mediaJob
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// Error tracking
	mu             sync.Mutex
	expiredCount   int // 403/404/410 errors (media expired/deleted)
	otherErrors    int
	otherErrorMsgs []string // Keep first few for debugging
}

func newMediaDownloadWorker(app *App, workers int) *mediaDownloadWorker {
	if workers <= 0 {
		workers = 2
	}
	return &mediaDownloadWorker{
		app:     app,
		workers: workers,
		jobs:    make(chan mediaJob, workers*4),
	}
}

func (w *mediaDownloadWorker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.ctx, w.cancel = context.WithCancel(ctx)
	for i := 0; i < w.workers; i++ {
		w.wg.Add(1)
		go w.run()
	}
}

func (w *mediaDownloadWorker) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case job := <-w.jobs:
			if err := w.app.processMediaJob(w.ctx, job); err != nil {
				w.trackError(err)
			}
		}
	}
}

func (w *mediaDownloadWorker) trackError(err error) {
	errStr := err.Error()
	// Check for expected expired/deleted media errors
	isExpired := strings.Contains(errStr, "status code 403") ||
		strings.Contains(errStr, "status code 404") ||
		strings.Contains(errStr, "status code 410")

	w.mu.Lock()
	defer w.mu.Unlock()

	if isExpired {
		w.expiredCount++
	} else {
		w.otherErrors++
		// Keep first 5 other errors for debugging
		if len(w.otherErrorMsgs) < 5 {
			w.otherErrorMsgs = append(w.otherErrorMsgs, errStr)
		}
	}
}

func (w *mediaDownloadWorker) PrintSummary() {
	if w == nil {
		return
	}
	w.mu.Lock()
	expiredCount := w.expiredCount
	otherErrors := w.otherErrors
	otherErrorMsgs := w.otherErrorMsgs
	w.mu.Unlock()

	if expiredCount > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  Skipped %d expired/deleted media files (normal for old messages)\n", expiredCount)
	}
	if otherErrors > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  %d media downloads failed:\n", otherErrors)
		for _, msg := range otherErrorMsgs {
			fmt.Fprintf(os.Stderr, "   - %s\n", msg)
		}
		if otherErrors > len(otherErrorMsgs) {
			fmt.Fprintf(os.Stderr, "   ... and %d more\n", otherErrors-len(otherErrorMsgs))
		}
	}
}

func (w *mediaDownloadWorker) Enqueue(job mediaJob) {
	if w == nil || w.ctx == nil {
		return
	}
	select {
	case w.jobs <- job:
	case <-w.ctx.Done():
	default:
		go func() {
			select {
			case w.jobs <- job:
			case <-w.ctx.Done():
			}
		}()
	}
}

func (w *mediaDownloadWorker) Stop() {
	if w == nil {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// Watchdog tuning. Exposed as vars (not const) so tests can shorten them.
var (
	// syncWatchdogInterval is how often the sync watchdog checks connection state.
	syncWatchdogInterval = 30 * time.Second
	// syncWatchdogResetAfter is how long the connection can be down before the
	// watchdog forces a clean Disconnect+Connect cycle to break out of a wedged
	// auto-reconnect loop in whatsmeow.
	syncWatchdogResetAfter = 5 * time.Minute
)

// Stream-reclaim tuning. A StreamReplaced event means another session connected
// with this device's credentials, and the WhatsApp server tore down our stream
// (a linked device permits only one active websocket). Rather than exit, we wait
// a short backoff — letting a transient competitor such as a one-shot
// whatsapp-cli command finish and disconnect — then reconnect to reclaim the
// stream. A sliding-window attempt cap stops us from ping-ponging forever with a
// genuinely persistent competitor. Exposed as vars so tests can shorten them.
var (
	// streamReclaimBackoff is how long to wait after a StreamReplaced before
	// reconnecting, so a transient competitor can release the stream first.
	streamReclaimBackoff = 8 * time.Second
	// streamReclaimWindow is the sliding window over which reclaim attempts are
	// counted to detect a persistent competitor.
	streamReclaimWindow = 2 * time.Minute
	// streamReclaimMaxAttempts is how many reclaim attempts are allowed within
	// streamReclaimWindow before giving up and stopping the sync. Without this,
	// a session that keeps stealing the stream back would cause an endless
	// reconnect war (and risk a temporary ban).
	streamReclaimMaxAttempts = 3
)

// Sync connects to WhatsApp and continuously syncs messages to the database
func (a *App) Sync(ctx context.Context) string {
	messageCount := 0

	version := a.version
	if strings.TrimSpace(version) == "" {
		version = "unknown"
	}
	fmt.Fprintf(os.Stderr, "ℹ️  whatsapp-cli version: %s\n", version)

	// Wrap the caller context so a terminal event (LoggedOut) or the stream
	// reclaimer (after it gives up) can cancel the sync from inside.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	worker := newMediaDownloadWorker(a, 4)
	worker.Start(ctx)
	a.mediaWorker = worker
	defer func() {
		worker.Stop()
		worker.PrintSummary()
		if a.mediaWorker == worker {
			a.mediaWorker = nil
		}
	}()

	// StreamReplaced events are delivered from whatsmeow's goroutine; the
	// handler hands them off here so the (blocking) backoff-and-reconnect runs
	// on the reclaimer goroutine instead. Buffered+coalesced: one pending
	// reclaim is enough.
	reclaimCh := make(chan struct{}, 1)

	// Create event handler
	eventHandler := func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Extract message details
			details := client.HandleMessage(v)
			id := details.ID
			chatJID := details.ChatJID
			sender := details.Sender
			content := details.Content
			msgTime := details.Timestamp
			isFromMe := details.IsFromMe
			mediaType := ""
			filename := ""
			url := ""
			directPath := ""
			mimeType := ""
			var mediaKey, fileSHA256, fileEncSHA256 []byte
			var fileLength uint64

			if details.Media != nil {
				mediaType = details.Media.Type
				filename = details.Media.Filename
				url = details.Media.URL
				directPath = details.Media.DirectPath
				mimeType = details.Media.MimeType
				mediaKey = details.Media.MediaKey
				fileSHA256 = details.Media.FileSHA256
				fileEncSHA256 = details.Media.FileEncSHA256
				fileLength = details.Media.FileLength
			}

			chatName := a.client.ResolveChatName(ctx, chatJID, v)
			if chatName == "" && chatJID != "" {
				chatName = chatJID
			}

			// Store chat
			a.store.StoreChat(chatJID, chatName, msgTime)

			// Store message
			a.store.StoreMessage(
				id,
				chatJID,
				sender,
				content,
				msgTime,
				isFromMe,
				mediaType,
				filename,
				url,
				directPath,
				mimeType,
				mediaKey, fileSHA256, fileEncSHA256, fileLength,
			)

			if directPath != "" && len(mediaKey) > 0 {
				worker.Enqueue(mediaJob{messageID: id, chatJID: chatJID})
			}

			messageCount++
			fmt.Fprintf(os.Stderr, "\r💬 Synced %d messages...", messageCount)

		case *events.HistorySync:
			fmt.Fprintf(os.Stderr, "\n📜 Processing history sync (%d conversations)...\n", len(v.Data.Conversations))
			for _, conv := range v.Data.Conversations {
				chatJID := conv.GetID()
				chatName := conv.GetName()
				if chatName == "" {
					chatName = a.client.ResolveChatName(ctx, chatJID, nil)
					if chatName == "" {
						chatName = chatJID
					}
				}

				// Process messages in this conversation
				for _, msg := range conv.Messages {
					if msg.Message == nil {
						continue
					}

					histMsg := msg.Message
					msgID := histMsg.Key.GetID()
					sender := histMsg.Key.GetParticipant()
					if sender == "" {
						sender = histMsg.Key.GetRemoteJID()
					}
					isFromMe := histMsg.Key.GetFromMe()
					msgTimestamp := time.Unix(int64(histMsg.GetMessageTimestamp()), 0)

					// Extract content
					content := ""
					mediaType := ""
					filename := ""
					url := ""
					directPath := ""
					mimeType := ""
					var mediaKey, fileSHA256, fileEncSHA256 []byte
					var fileLength uint64

					switch {
					case histMsg.Message.GetConversation() != "":
						content = histMsg.Message.GetConversation()
					case histMsg.Message.GetExtendedTextMessage() != nil:
						extText := histMsg.Message.GetExtendedTextMessage()
						content = extText.GetText()
					case histMsg.Message.GetImageMessage() != nil:
						img := histMsg.Message.GetImageMessage()
						mediaType = "image"
						content = img.GetCaption()
						// Don't use caption as filename - it can be very long text
						url = img.GetURL()
						directPath = img.GetDirectPath()
						mimeType = img.GetMimetype()
						mediaKey = img.GetMediaKey()
						fileSHA256 = img.GetFileSHA256()
						fileEncSHA256 = img.GetFileEncSHA256()
						fileLength = img.GetFileLength()
					case histMsg.Message.GetVideoMessage() != nil:
						video := histMsg.Message.GetVideoMessage()
						mediaType = "video"
						content = video.GetCaption()
						// Don't use caption as filename - it can be very long text
						url = video.GetURL()
						directPath = video.GetDirectPath()
						mimeType = video.GetMimetype()
						mediaKey = video.GetMediaKey()
						fileSHA256 = video.GetFileSHA256()
						fileEncSHA256 = video.GetFileEncSHA256()
						fileLength = video.GetFileLength()
					case histMsg.Message.GetAudioMessage() != nil:
						audio := histMsg.Message.GetAudioMessage()
						mediaType = "audio"
						content = "[Audio]"
						url = audio.GetURL()
						directPath = audio.GetDirectPath()
						mimeType = audio.GetMimetype()
						mediaKey = audio.GetMediaKey()
						fileSHA256 = audio.GetFileSHA256()
						fileEncSHA256 = audio.GetFileEncSHA256()
						fileLength = audio.GetFileLength()
					case histMsg.Message.GetDocumentMessage() != nil:
						doc := histMsg.Message.GetDocumentMessage()
						mediaType = "document"
						content = doc.GetCaption()
						filename = doc.GetFileName()
						url = doc.GetURL()
						directPath = doc.GetDirectPath()
						mimeType = doc.GetMimetype()
						mediaKey = doc.GetMediaKey()
						fileSHA256 = doc.GetFileSHA256()
						fileEncSHA256 = doc.GetFileEncSHA256()
						fileLength = doc.GetFileLength()
					}

					// Store chat
					a.store.StoreChat(chatJID, chatName, msgTimestamp)

					// Store message
					a.store.StoreMessage(
						msgID,
						chatJID,
						sender,
						content,
						msgTimestamp,
						isFromMe,
						mediaType,
						filename,
						url,
						directPath,
						mimeType,
						mediaKey, fileSHA256, fileEncSHA256, fileLength,
					)

					if directPath != "" && len(mediaKey) > 0 {
						worker.Enqueue(mediaJob{messageID: msgID, chatJID: chatJID})
					}

					messageCount++
				}
			}
			fmt.Fprintf(os.Stderr, "\r💬 Synced %d messages...", messageCount)

		case *events.Connected:
			fmt.Fprintln(os.Stderr, "\n✓ Connected to WhatsApp")
			fmt.Fprintln(os.Stderr, "🔄 Listening for messages... (Press Ctrl+C to stop)")

		case *events.Disconnected:
			fmt.Fprintln(os.Stderr, "\n⚠ Disconnected from WhatsApp (auto-reconnect in progress)")

		case *events.KeepAliveTimeout:
			// Keepalive ping has not been answered. Whatsmeow will force a
			// reconnect after KeepAliveMaxFailTime (3min) of failures; surface
			// it now so the user knows the connection is degraded.
			fmt.Fprintf(os.Stderr, "\n⏱ Keepalive timeout (errors=%d, last success %s ago)\n",
				v.ErrorCount, time.Since(v.LastSuccess).Round(time.Second))

		case *events.KeepAliveRestored:
			fmt.Fprintln(os.Stderr, "\n✓ Keepalive restored")

		case *events.LoggedOut:
			// Terminal: the WhatsApp server invalidated this session.
			// Auto-reconnect will not recover this — the user must re-auth.
			fmt.Fprintf(os.Stderr, "\n✗ Logged out by WhatsApp (reason: %s). Run `whatsapp-cli auth` to re-authenticate.\n", v.Reason)
			cancel()

		case *events.StreamReplaced:
			// Another session connected with this device's credentials and the
			// server replaced our stream. Don't exit — signal the reclaimer to
			// back off and reconnect. A non-blocking send coalesces bursts: if a
			// reclaim is already pending, this event is dropped.
			fmt.Fprintln(os.Stderr, "\n⚠ Stream replaced — another session connected with this device. Attempting to reclaim...")
			select {
			case reclaimCh <- struct{}{}:
			default:
			}

		case *events.ConnectFailure:
			fmt.Fprintf(os.Stderr, "\n⚠ Connect failure (reason: %s)\n", v.Reason)

		case *events.TemporaryBan:
			fmt.Fprintf(os.Stderr, "\n⚠ Temporarily banned (code=%d, expire=%s)\n", v.Code, v.Expire)
		}
	}

	// Start syncing
	fmt.Fprintln(os.Stderr, "🚀 Starting WhatsApp sync...")
	if err := a.client.StartSync(ctx, eventHandler); err != nil {
		return output.Error(err)
	}

	// Watchdog: whatsmeow's internal auto-reconnect occasionally wedges
	// (growing backoff, stale state) and the sync silently stops delivering
	// messages while the process keeps running. Detect prolonged disconnects
	// and force a clean reconnect cycle.
	var bgWG sync.WaitGroup
	a.runSyncWatchdog(ctx, &bgWG)

	// Reclaimer: on StreamReplaced, back off and reconnect to take the stream
	// back, giving up only if a persistent competitor keeps stealing it.
	a.runStreamReclaimer(ctx, cancel, reclaimCh, &bgWG)

	// Wait for context cancellation (Ctrl+C, LoggedOut, reclaimer give-up)
	<-ctx.Done()

	// Drain background goroutines before returning so none outlive Sync.
	bgWG.Wait()

	fmt.Fprintf(os.Stderr, "\n\n✓ Sync completed. Total messages synced: %d\n", messageCount)

	return output.Success(map[string]interface{}{
		"synced":         true,
		"messages_count": messageCount,
	})
}

// runSyncWatchdog periodically checks the connection state and forces a clean
// reconnect if the client has been disconnected for longer than
// syncWatchdogResetAfter. Returns immediately; the watchdog runs in a
// goroutine that signals completion through wg before exiting.
func (a *App) runSyncWatchdog(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(syncWatchdogInterval)
		defer ticker.Stop()

		var disconnectedSince time.Time
		warned := false

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			if a.client.IsConnected() && a.client.IsLoggedIn() {
				if !disconnectedSince.IsZero() {
					disconnectedSince = time.Time{}
					warned = false
				}
				continue
			}

			if disconnectedSince.IsZero() {
				disconnectedSince = time.Now()
				continue
			}

			down := time.Since(disconnectedSince)
			if !warned && down >= syncWatchdogInterval*2 {
				fmt.Fprintf(os.Stderr, "\n⚠ Sync watchdog: client disconnected for %s\n", down.Round(time.Second))
				warned = true
			}

			if down < syncWatchdogResetAfter {
				continue
			}

			// Force a clean reconnect cycle. whatsmeow's internal auto-reconnect
			// can get wedged with growing backoff or stale state; calling
			// Disconnect() then Connect() resets it.
			fmt.Fprintf(os.Stderr, "\n⟳ Sync watchdog: forcing reconnect after %s offline\n", down.Round(time.Second))
			a.client.Disconnect()
			if err := a.client.Connect(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ Sync watchdog: reconnect attempt failed: %v\n", err)
				// Reset the timer so we don't hammer; next check is one
				// interval later, and another reset attempt is one
				// syncWatchdogResetAfter window away.
				disconnectedSince = time.Now()
				warned = false
				continue
			}
			// Successful reconnect; events.Connected handler will print "✓".
			disconnectedSince = time.Time{}
			warned = false
		}
	}()
}

// runStreamReclaimer reacts to StreamReplaced events delivered over reclaimCh.
// Each event triggers a backoff-then-reconnect to reclaim the WhatsApp stream
// from whatever connected with this device's credentials. It counts attempts in
// a sliding window: once streamReclaimMaxAttempts reclaims happen within
// streamReclaimWindow — the signature of a persistent competitor (WhatsApp
// Web/Desktop, or another process sharing this --store) — it cancels the sync
// instead of ping-ponging forever. Returns immediately; the goroutine signals
// completion through wg before exiting.
func (a *App) runStreamReclaimer(ctx context.Context, cancel context.CancelFunc, reclaimCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Timestamps of recent reclaim attempts, pruned to streamReclaimWindow.
		var attempts []time.Time

		for {
			select {
			case <-ctx.Done():
				return
			case <-reclaimCh:
			}

			// Drop attempts that have aged out of the window so isolated,
			// infrequent replacements (a transient one-shot command now and
			// then) never accumulate toward the give-up threshold.
			now := time.Now()
			kept := attempts[:0]
			for _, t := range attempts {
				if now.Sub(t) < streamReclaimWindow {
					kept = append(kept, t)
				}
			}
			attempts = kept

			if len(attempts) >= streamReclaimMaxAttempts {
				fmt.Fprintf(os.Stderr,
					"\n✗ Stream replaced %d times within %s — a persistent session keeps taking the connection "+
						"(WhatsApp Web/Desktop left open, or another whatsapp-cli process sharing this --store). "+
						"Stopping sync to avoid a reconnect war.\n",
					len(attempts), streamReclaimWindow.Round(time.Second))
				cancel()
				return
			}
			attempts = append(attempts, now)

			// Back off so a transient competitor (e.g. a one-shot whatsapp-cli
			// command) can finish and release the stream before we reclaim it.
			select {
			case <-ctx.Done():
				return
			case <-time.After(streamReclaimBackoff):
			}

			fmt.Fprintf(os.Stderr, "\n⟳ Reclaiming WhatsApp stream (attempt %d/%d within %s)...\n",
				len(attempts), streamReclaimMaxAttempts, streamReclaimWindow.Round(time.Second))
			a.client.Disconnect()
			if err := a.client.Connect(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ Reclaim reconnect failed: %v\n", err)
			}
		}
	}()
}

func resolveVersion(version string, describeFn func() (string, error)) string {
	if strings.TrimSpace(version) != "" && version != "dev" {
		return version
	}

	if describeFn != nil {
		if gitVersion, err := describeFn(); err == nil && strings.TrimSpace(gitVersion) != "" {
			return gitVersion
		}
	}

	if strings.TrimSpace(version) == "" {
		return "unknown"
	}
	return version
}

func gitDescribe() (string, error) {
	cmd := exec.Command("git", "describe", "--tags", "--dirty", "--always")
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
