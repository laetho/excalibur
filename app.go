package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
	nats "github.com/nats-io/nats.go"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type pendingEdit struct{ Name, JSON string }
type pendingResult struct {
	Edit *pendingEdit
	Err  error
}

type collabOutboxEntry struct {
	SourceID   string `json:"sourceId"`
	Bucket     string `json:"bucket"`
	DocumentID string `json:"documentId"`
	Kind       string `json:"kind,omitempty"`
	MessageID  string `json:"messageId"`
	BlobID     string `json:"blobId,omitempty"`
	Data       string `json:"data"`
	Order      uint64 `json:"order,omitempty"`
}

type collabOutboxRecord struct {
	Op    string             `json:"op"`
	Key   string             `json:"key,omitempty"`
	Entry *collabOutboxEntry `json:"entry,omitempty"`
}

const (
	minApplicationZoom     = 0.5
	maxApplicationZoom     = 2.0
	defaultApplicationZoom = 1.0
	applicationZoomStep    = 0.1
)

type App struct {
	ctx        context.Context
	wailsApp   *application.App
	window     application.Window
	mu         sync.Mutex
	store      *Store
	pending    sync.Map
	thumbnails sync.Map
	// thumbMu guards thumbCache, the in-memory Morgan le Fayl preview cache
	// keyed by path and invalidated by mtime+size. It is separate from mu so a
	// slow renderer round-trip does not stall other store operations.
	thumbMu       sync.Mutex
	thumbCache    map[string]thumbCacheEntry
	subMu         sync.Mutex
	subscriptions map[string]*nats.Subscription
	// Durable collaboration messages are acknowledged only after the renderer
	// confirms it received and validated the Wails event. This closes the final
	// event-loss window between Go and the browser.
	collabPending  map[string]*nats.Msg
	outboxMu       sync.Mutex
	outboxPath     string
	collabOutbox   map[string]collabOutboxEntry
	outboxOrder    uint64
	outboxOps      uint64
	outboxWake     chan struct{}
	outboxWorkers  sync.WaitGroup
	shutdownReady  atomic.Bool
	shutdownSaving atomic.Bool
	requestID      atomic.Uint64
	watcherMu      sync.Mutex
	watcher        *fsnotify.Watcher
	fsHandling     atomic.Bool
	stop           chan struct{}
}

func newApp() *App {
	return &App{
		subscriptions: map[string]*nats.Subscription{},
		collabPending: map[string]*nats.Msg{},
		collabOutbox:  map[string]collabOutboxEntry{},
		thumbCache:    map[string]thumbCacheEntry{},
		outboxWake:    make(chan struct{}, 1),
		stop:          make(chan struct{}),
	}
}

func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.ctx = ctx
	config, err := os.UserConfigDir()
	if err != nil {
		config = "."
	}
	home, _ := os.UserHomeDir()
	folder := boardsFolder(home)
	store, err := newStore(preferencesPath(config), folder)
	if err != nil {
		return fmt.Errorf("initialise store: %w", err)
	}
	a.mu.Lock()
	a.store = store
	applicationZoom := store.prefs.ApplicationZoom
	a.mu.Unlock()
	a.outboxPath = filepath.Join(filepath.Dir(store.prefsPath), "collab-outbox.json")
	if err := a.loadCollabOutbox(); err != nil {
		return fmt.Errorf("load pending collaboration updates: %w", err)
	}
	if a.window != nil {
		a.window.SetZoom(applicationZoom)
	}
	a.startWatcher(store.localFolders)
	go a.remoteRefreshLoop()
	go a.refreshBucketsOnStartup()
	if os.Getenv("EXCALIBUR_DEBUG_NATS") != "" {
		go a.natsStatsLoop()
	}
	a.outboxWorkers.Add(1)
	go func() {
		defer a.outboxWorkers.Done()
		a.collabOutboxLoop()
	}()
	return nil
}

func (a *App) ServiceShutdown() error {
	// The renderer has already been torn down when service shutdown runs, so a
	// canvas round-trip here can only time out. The WindowClosing hook performs
	// the durability barrier while the renderer is still alive.
	close(a.stop)
	a.outboxWorkers.Wait()
	a.watcherMu.Lock()
	if a.watcher != nil {
		_ = a.watcher.Close()
		a.watcher = nil
	}
	a.watcherMu.Unlock()
	a.subMu.Lock()
	for _, subscription := range a.subscriptions {
		_ = subscription.Unsubscribe()
	}
	a.subscriptions = map[string]*nats.Subscription{}
	a.collabPending = map[string]*nats.Msg{}
	a.subMu.Unlock()
	a.mu.Lock()
	if a.store != nil {
		a.store.close()
	}
	a.mu.Unlock()
	return nil
}

func (a *App) prepareShutdown() error {
	if a.shutdownReady.Load() {
		return nil
	}
	if err := a.savePending(); err != nil {
		return err
	}
	a.shutdownReady.Store(true)
	return nil
}

func (a *App) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d-%x", prefix, os.Getpid(), a.requestID.Add(1))
}
func (a *App) emit(name string, payload any) {
	if a.wailsApp != nil {
		a.wailsApp.Event.Emit(name, payload)
	}
}
func (a *App) snapshotLocked() StoreSnapshot { return a.store.snapshot() }
func (a *App) emitSnapshotLocked()           { a.emit("store-changed", a.snapshotLocked()) }

func (a *App) GetState() (StoreSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return StoreSnapshot{}, errors.New("store is not ready")
	}
	return a.store.snapshot(), nil
}

func (a *App) CanvasReady() error {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return errors.New("store is not ready")
	}
	// CanvasReady is emitted only after the renderer has installed its canvas
	// event listeners. Reopen the persisted selection here instead of asking the
	// shell to navigate to it during startup, which would incorrectly run the
	// pending-save barrier before a canvas has been opened.
	err := a.openSelectedLocked()
	a.mu.Unlock()
	go a.refreshDetails()
	return err
}

func (a *App) CanvasChange(name, jsonText string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.saveBoard(name, jsonText); err != nil {
		return err
	}
	id := name + fileExtension
	modified := time.Now().Unix()
	if board := a.store.board(id); board != nil {
		modified = board.Modified.Unix()
	}
	a.emit("board-modified", map[string]any{"id": id, "modified": modified})
	go a.refreshDetails()
	return nil
}

func (a *App) CanvasView(name string, scrollX, scrollY, zoom float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.saveViewport(name, Viewport{ScrollX: scrollX, ScrollY: scrollY, Zoom: zoom})
}
func (a *App) CanvasLibrary(jsonText string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store.saveLibrary(jsonText)
}

func (a *App) OpenLink(rawURL string) {
	scheme := strings.ToLower(strings.SplitN(rawURL, ":", 2)[0])
	if scheme == "http" || scheme == "https" || scheme == "mailto" {
		_ = a.wailsApp.Browser.OpenURL(rawURL)
	}
}

func (a *App) ShowAlert(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(message) > 500 {
		message = message[:500]
	}
	a.store.alerts = append(a.store.alerts, Notice{ID: a.nextID("notice"), Title: "Excalibur", Message: message})
	a.emitSnapshotLocked()
}

func (a *App) Log(level, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	log.Printf("canvas %s: %s", level, message)
}

func (a *App) PendingResult(requestID string, name, jsonText, errorMessage *string) {
	value := (*pendingEdit)(nil)
	if name != nil && jsonText != nil {
		value = &pendingEdit{Name: *name, JSON: *jsonText}
	}
	result := pendingResult{Edit: value}
	if errorMessage != nil && *errorMessage != "" {
		result.Err = errors.New(*errorMessage)
	}
	if found, ok := a.pending.LoadAndDelete(requestID); ok {
		found.(chan pendingResult) <- result
	}
}

func (a *App) ThumbnailResult(requestID string, dataURL *string) {
	var data string
	if dataURL != nil {
		data = *dataURL
	}
	if found, ok := a.thumbnails.LoadAndDelete(requestID); ok {
		found.(chan string) <- data
	}
}

func (a *App) takePending() (*pendingEdit, error) {
	id := a.nextID("pending")
	result := make(chan pendingResult, 1)
	a.pending.Store(id, result)
	a.emit("canvas:take-pending", map[string]any{"requestId": id})
	select {
	case value := <-result:
		return value.Edit, value.Err
	case <-time.After(30 * time.Second):
		a.pending.Delete(id)
		return nil, errors.New("timed out while saving changes")
	}
}

func (a *App) savePending() error {
	// Navigation is a durability barrier for every board. Local boards return a
	// complete JSON scene; collaborative boards return nil only after their final
	// scene has been durably appended to the local outbox. Remote delivery remains
	// asynchronous, so the common path does not wait on the network.
	pending, err := a.takePending()
	if err != nil {
		return err
	}
	if pending == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		return a.store.saveBoard(pending.Name, pending.JSON)
	}
	return nil
}

func (a *App) openSelectedLocked() error {
	board := a.store.board(a.store.selection)
	if board == nil {
		return nil
	}
	data, snapshotSequence, err := a.store.readBoardState(*board)
	if err != nil {
		return err
	}
	a.store.known[board.ID] = append([]byte(nil), data...)
	var view *Viewport
	if saved, ok := a.store.prefs.Viewports[a.store.viewportKey(board.ID)]; ok {
		copy := saved
		view = &copy
	}
	a.emit("canvas:open", map[string]any{
		"name":             board.Name,
		"json":             string(data),
		"view":             view,
		"snapshotSequence": strconv.FormatUint(snapshotSequence, 10),
	})
	library, err := a.store.library()
	if err == nil {
		a.emit("canvas:set-library", map[string]any{"json": library})
		a.watchSharedLibraryLocked()
	}
	return err
}

// watchSharedLibraryLocked pushes library changes made by other clients to the
// renderer while a board of the active NATS server is open.
func (a *App) watchSharedLibraryLocked() {
	if a.store.isLocal() {
		return
	}
	client, err := a.store.clientFor(a.store.activeSource)
	if err != nil {
		return
	}
	sourceID := a.store.activeSource
	err = client.watchLibrary(func(data []byte) {
		a.mu.Lock()
		active := a.store != nil && a.store.activeSource == sourceID
		a.mu.Unlock()
		if active {
			text := string(data)
			a.emit("canvas:set-library", map[string]any{"json": &text})
		}
	})
	if err != nil {
		log.Printf("watch shared library: %v", err)
	}
}

func (a *App) SelectBoard(id string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store.board(id) == nil {
		return errors.New("board not found")
	}
	a.store.setSelection(id)
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}

func (a *App) CreateBoard(name string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.createBoard(name); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}

func (a *App) RenameBoard(id, name string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	wasOpen := a.store.selection == id
	if err := a.store.renameBoard(id, name); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	if wasOpen {
		a.emit("canvas:rename", map[string]any{"name": strings.TrimSpace(name)})
	}
	return nil
}

func (a *App) DuplicateBoard(id string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.duplicateBoard(id); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}
func (a *App) TrashBoard(id string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.trashBoard(id); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) ImportPaths(paths []string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.importPaths(paths); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}
func (a *App) ImportBoardData(name, jsonText string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.importBoardData(name, []byte(jsonText)); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}

func (a *App) PickFile(extensions []string) (*PickFileResult, error) {
	dialog := a.wailsApp.Dialog.OpenFile()
	if len(extensions) > 0 {
		dialog.AddFilter("Files", "*."+strings.Join(extensions, ";*."))
	}
	path, err := dialog.PromptForSingleSelection()
	if err != nil || path == "" {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &PickFileResult{Name: filepath.Base(path), DataURL: "data:" + mimeForPath(path) + ";base64," + base64.StdEncoding.EncodeToString(data)}, nil
}

func (a *App) SaveFile(fileName, dataURL string) error {
	path, err := a.wailsApp.Dialog.SaveFile().SetFilename(fileName).PromptForSingleSelection()
	if err != nil || path == "" {
		return err
	}
	data, err := decodeDataURL(dataURL)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (a *App) ChooseImport() error {
	paths, err := a.wailsApp.Dialog.OpenFile().AddFilter("Excalidraw", "*.excalidraw").PromptForMultipleSelection()
	if err != nil || len(paths) == 0 {
		return err
	}
	return a.ImportPaths(paths)
}

// AddLocalFolder lets the user pick a folder of .excalidraw boards, adds it to
// the sidebar and opens it.
func (a *App) AddLocalFolder() error {
	folder, err := a.wailsApp.Dialog.OpenFile().SetTitle("Add Boards Folder").CanChooseFiles(false).CanChooseDirectories(true).CanCreateDirectories(true).PromptForSingleSelection()
	if err != nil || folder == "" {
		return err
	}
	return a.addLocalFolderAt(folder)
}

// AddLocalFolderPath adds a boards folder from a typed path, for power users and
// for desktops whose native picker cannot select a directory. A leading ~ is
// expanded, surrounding quotes are ignored, and a missing folder is created.
func (a *App) AddLocalFolderPath(path string) error {
	folder, err := normalizeFolderPath(path)
	if err != nil {
		return err
	}
	return a.addLocalFolderAt(folder)
}

// normalizeFolderPath turns a typed folder location into a clean absolute path.
// Quotes pasted from a file manager are ignored and ~ expands to the home folder.
func normalizeFolderPath(path string) (string, error) {
	path = strings.Trim(strings.TrimSpace(path), `"'`)
	if path == "" {
		return "", errors.New("enter a folder path")
	}
	// File managers copy locations as file:// URIs.
	if strings.HasPrefix(path, "file://") {
		raw := strings.TrimPrefix(path, "file://")
		if decoded, err := url.PathUnescape(raw); err == nil {
			path = decoded
		} else {
			path = raw
		}
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("use an absolute path, for example /home/you/boards")
	}
	return filepath.Clean(path), nil
}

// addLocalFolderAt adds and activates a boards folder, then starts watching it.
func (a *App) addLocalFolderAt(folder string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	if err := a.store.addLocalFolder(folder); err != nil {
		a.mu.Unlock()
		return err
	}
	folders := append([]string(nil), a.store.localFolders...)
	a.emitSnapshotLocked()
	a.mu.Unlock()
	a.startWatcher(folders)
	return nil
}

// RemoveLocalFolder removes a folder from the sidebar. Its boards stay on disk.
func (a *App) RemoveLocalFolder(folder string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	err := a.store.removeLocalFolder(folder)
	folders := append([]string(nil), a.store.localFolders...)
	// Even when switching to the remaining folder fails, the folder is gone from the list.
	a.emitSnapshotLocked()
	a.mu.Unlock()
	a.startWatcher(folders)
	return err
}

// ChooseNatsCredentialsFile lets the user pick a .creds file and imports a
// private copy into the app's config directory, returning the copy's path.
// Reading the original again later (for example at boot) can fail on macOS,
// where access to folders such as Downloads is only granted for the moment the
// file is chosen. The credential contents and private NKey seed never enter
// the webview.
func (a *App) ChooseNatsCredentialsFile() (string, error) {
	path, err := a.wailsApp.Dialog.OpenFile().
		SetTitle("Choose NATS Credentials File").
		AddFilter("NATS credentials", "*.creds").
		PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	a.mu.Lock()
	store := a.store
	a.mu.Unlock()
	if store == nil {
		return "", errors.New("store is not ready")
	}
	return importCredentialsFile(path, store.credentialsDir())
}

func revealPath(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", "-R", path)
	case "windows":
		command = exec.Command("explorer", "/select,"+path)
	default:
		command = exec.Command("xdg-open", filepath.Dir(path))
	}
	return command.Start()
}

func (a *App) Reveal(id *string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	boardID := a.store.selection
	if id != nil {
		boardID = *id
	}
	board := a.store.board(boardID)
	if board == nil || !a.store.isLocal() {
		return nil
	}
	return revealPath(board.Path)
}

// RevealFolder shows a local boards folder in the file manager.
func (a *App) RevealFolder(folder string) error {
	a.mu.Lock()
	known := slices.Contains(a.store.localFolders, folder)
	a.mu.Unlock()
	if !known {
		return errors.New("unknown local folder")
	}
	return revealPath(filepath.Join(folder, "."))
}

func (a *App) SetSort(order string) error {
	if order != "name" && order != "dateEdited" {
		return errors.New("invalid sort order")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.prefs.SortOrder = order
	a.store.savePrefs()
	a.emitSnapshotLocked()
	return nil
}

// TogglePinnedGroup pins a folder or bucket so its boards stay visible while
// another group is active.
func (a *App) TogglePinnedGroup(sourceID, groupID string) error {
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(groupID) == "" {
		return errors.New("a pinned group needs a source and a group")
	}
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.togglePinnedGroup(sourceID, groupID)
	a.emitSnapshotLocked()
	return nil
}

// SetGroupState remembers how a folder or bucket looks in the sidebar: whether
// its find box is open, what is typed in it, and whether its list is folded.
func (a *App) SetGroupState(patch GroupStatePatch) error {
	if strings.TrimSpace(patch.Source) == "" || strings.TrimSpace(patch.Group) == "" {
		return errors.New("a group state needs a source and a group")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.applyGroupState(patch)
	return nil
}

func (a *App) SetShowThumbnails(show bool) {
	a.mu.Lock()
	a.store.prefs.ShowThumbnails = &show
	a.store.savePrefs()
	a.emitSnapshotLocked()
	a.mu.Unlock()
	if show {
		go a.refreshDetails()
	}
}
func (a *App) SetCanvasAppearance(appearance string) error {
	if appearance != "system" && appearance != "light" && appearance != "dark" {
		return errors.New("invalid appearance")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.prefs.CanvasAppearance = appearance
	a.store.savePrefs()
	a.emitSnapshotLocked()
	return nil
}

const maxCollaborationNicknameLength = 40

func normalizeCollaborationNickname(nickname string) (string, error) {
	nickname = strings.TrimSpace(nickname)
	if utf8.RuneCountInString(nickname) > maxCollaborationNicknameLength {
		return "", fmt.Errorf("nickname must be %d characters or fewer", maxCollaborationNicknameLength)
	}
	for _, r := range nickname {
		if unicode.IsControl(r) {
			return "", errors.New("nickname cannot contain control characters")
		}
	}
	return nickname, nil
}

// SetCollaborationNickname sets the name shown beside this client's cursor in
// NATS collaboration sessions. An empty value keeps the anonymous peer label.
func (a *App) SetCollaborationNickname(nickname string) error {
	nickname, err := normalizeCollaborationNickname(nickname)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.prefs.CollabNickname = nickname
	a.store.savePrefs()
	a.emitSnapshotLocked()
	return nil
}

// maxCollaborationAvatarRunes bounds the avatar emoji. A single emoji can be
// several code points (skin tone, ZWJ sequences, flags), so allow a few.
const maxCollaborationAvatarRunes = 12

var collaborationColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func normalizeCollaborationAppearance(color, avatar string) (string, string, error) {
	color = strings.ToLower(strings.TrimSpace(color))
	if color != "" && !collaborationColorPattern.MatchString(color) {
		return "", "", errors.New("color must be a #rrggbb value")
	}
	avatar = strings.TrimSpace(avatar)
	if utf8.RuneCountInString(avatar) > maxCollaborationAvatarRunes {
		return "", "", errors.New("avatar must be a single emoji")
	}
	for _, r := range avatar {
		// Emoji are never ASCII, letters, digits, whitespace or control characters.
		if r < 0x80 || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", "", errors.New("avatar must be a single emoji")
		}
	}
	return color, avatar, nil
}

// SetCollaborationAppearance sets the cursor color and the emoji avatar other
// people see for this client. Empty values mean an automatic color and no avatar.
// With avatarOnly, others see just the avatar instead of the avatar and name.
func (a *App) SetCollaborationAppearance(color, avatar string, avatarOnly bool) error {
	color, avatar, err := normalizeCollaborationAppearance(color, avatar)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store.prefs.CollabColor = color
	a.store.prefs.CollabAvatar = avatar
	a.store.prefs.CollabAvatarOnly = avatarOnly
	a.store.savePrefs()
	a.emitSnapshotLocked()
	return nil
}

func validApplicationZoom(zoom float64) bool {
	return !math.IsNaN(zoom) && !math.IsInf(zoom, 0) && zoom >= minApplicationZoom && zoom <= maxApplicationZoom
}

// SetApplicationZoom scales the whole webview UI, not the Excalidraw scene.
func (a *App) SetApplicationZoom(zoom float64) error {
	if !validApplicationZoom(zoom) {
		return fmt.Errorf("application zoom must be between %.0f%% and %.0f%%", minApplicationZoom*100, maxApplicationZoom*100)
	}
	// Avoid accumulating floating-point noise after repeated +/- steps.
	zoom = math.Round(zoom*100) / 100
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return errors.New("store is not ready")
	}
	a.store.prefs.ApplicationZoom = zoom
	a.store.savePrefs()
	a.emitSnapshotLocked()
	a.mu.Unlock()
	if a.window != nil {
		a.window.SetZoom(zoom)
	}
	return nil
}

func (a *App) ChangeApplicationZoom(delta float64) error {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return errors.New("store is not ready")
	}
	zoom := a.store.prefs.ApplicationZoom
	a.mu.Unlock()
	zoom = math.Max(minApplicationZoom, math.Min(maxApplicationZoom, zoom+delta))
	return a.SetApplicationZoom(zoom)
}

func (a *App) ResetApplicationZoom() error {
	return a.SetApplicationZoom(defaultApplicationZoom)
}
func (a *App) DismissAlert() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.store.alerts) > 0 {
		a.store.alerts = a.store.alerts[1:]
	}
	a.emitSnapshotLocked()
}

func (a *App) AddNatsServer(config NatsServerConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.addNatsServer(config); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}
func (a *App) UpdateNatsServer(config NatsServerConfig, keepPassword bool) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.updateNatsServer(config, keepPassword); err != nil {
		return err
	}
	a.clearNatsSubscriptions(config.ID)
	a.emitSnapshotLocked()
	return nil
}
func (a *App) RemoveNatsServer(id string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.removeNatsServer(id); err != nil {
		return err
	}
	a.clearNatsSubscriptions(id)
	a.emitSnapshotLocked()
	return nil
}

func (a *App) clearNatsSubscriptions(id string) {
	prefix := "nats:" + id + "\x00"
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for key, subscription := range a.subscriptions {
		if strings.HasPrefix(key, prefix) {
			_ = subscription.Unsubscribe()
			delete(a.subscriptions, key)
		}
	}
}
func (a *App) AddBucket(sourceID, bucket string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.addBucket(sourceID, bucket); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

// DeleteBucket permanently removes an empty NATS bucket. The backend verifies
// emptiness immediately before deletion; the menu's state is only advisory.
func (a *App) DeleteBucket(sourceID, bucket string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	if err := a.ensureNoCollabOutboxBucket(sourceID, bucket); err != nil {
		return err
	}
	a.mu.Lock()
	if err := a.store.deleteBucket(sourceID, bucket); err != nil {
		a.mu.Unlock()
		return err
	}
	a.emitSnapshotLocked()
	a.mu.Unlock()
	a.clearNatsBucketSubscriptions(sourceID, bucket)
	return nil
}

func (a *App) clearNatsBucketSubscriptions(sourceID, bucket string) {
	prefix := sourceID + "\x00" + bucket + "\x00"
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for key, subscription := range a.subscriptions {
		if strings.HasPrefix(key, prefix) {
			_ = subscription.Unsubscribe()
			delete(a.subscriptions, key)
		}
	}
	for key := range a.collabPending {
		if strings.HasPrefix(key, prefix) {
			delete(a.collabPending, key)
		}
	}
}
func (a *App) ActivateSource(sourceID, groupID string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.activateSource(sourceID, groupID); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

// RefreshBuckets re-lists the buckets on every configured NATS server and
// rescans the active source so the sidebar reflects the current state. Any
// server that could not be reached is reported in the returned error.
func (a *App) RefreshBuckets() error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	err := a.store.refreshBuckets()
	if scanErr := a.store.scan(); scanErr != nil {
		err = errors.Join(err, scanErr)
	}
	a.emitSnapshotLocked()
	return err
}
func (a *App) CopyBoardToSource(boardID, targetSource, targetGroup string) error {
	if err := a.savePending(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.copyBoardToSource(boardID, targetSource, targetGroup); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

// CollabSend sends ephemeral presence and live scene previews over Core NATS.
// Durable drawing updates use CollabPublish, so this traffic never enters the
// replay log or local outbox.
func (a *App) CollabSend(sourceID, bucket, documentID, data string) error {
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if !validDocumentID(documentID) {
		return errors.New("invalid collaboration document id")
	}
	return client.conn.Publish(collabPresenceSubject(bucket, documentID), []byte(data))
}

// CollabPublish appends a versioned Excalidraw scene to the board's durable
// JetStream log. The publish acknowledgement sequence is used for snapshots.
func collabOutboxKey(entry collabOutboxEntry) string {
	if entry.Kind == "blob" {
		return entry.SourceID + "\x00" + entry.Bucket + "\x00" + entry.DocumentID + "\x00blob\x00" + entry.BlobID
	}
	return entry.SourceID + "\x00" + entry.Bucket + "\x00" + entry.DocumentID + "\x00" + entry.MessageID
}

func collabOutboxDocumentKey(entry collabOutboxEntry) string {
	return entry.SourceID + "\x00" + entry.Bucket + "\x00" + entry.DocumentID
}

func (a *App) collabBlobPath(entry collabOutboxEntry) string {
	sum := sha256.Sum256([]byte(collabOutboxKey(entry)))
	return filepath.Join(filepath.Dir(a.outboxPath), "collab-blobs", fmt.Sprintf("%x.json", sum[:16]))
}

func (a *App) loadCollabOutbox() error {
	a.outboxMu.Lock()
	defer a.outboxMu.Unlock()
	a.collabOutbox = map[string]collabOutboxEntry{}
	data, err := os.ReadFile(a.outboxPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Migrate the original whole-file JSON array on first load. New writes use an
	// append-only journal so an offline backlog does not rewrite and fsync every
	// older scene for each enqueue/acknowledgement.
	var legacy []collabOutboxEntry
	if json.Unmarshal(data, &legacy) == nil {
		for i := range legacy {
			if legacy[i].Order == 0 {
				a.outboxOrder++
				legacy[i].Order = a.outboxOrder
			}
			a.loadCollabOutboxEntryLocked(legacy[i])
		}
		if err := a.validateCollabBlobFilesLocked(); err != nil {
			return err
		}
		return a.compactCollabOutboxLocked()
	}

	journal := string(data)
	if strings.TrimSpace(journal) == "" {
		return nil
	}
	// A crash can leave only the final append torn. Every earlier newline-ended
	// record was fsynced independently and remains authoritative.
	if !strings.HasSuffix(journal, "\n") {
		if last := strings.LastIndexByte(journal, '\n'); last >= 0 {
			journal = journal[:last+1]
		} else {
			// With no complete newline-terminated record, the entire journal is the
			// one torn append. The old array format was already attempted above.
			return nil
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(journal))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var record collabOutboxRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("invalid collaboration outbox journal: %w", err)
		}
		a.outboxOps++
		switch record.Op {
		case "put":
			if record.Entry != nil {
				a.loadCollabOutboxEntryLocked(*record.Entry)
			}
		case "del":
			delete(a.collabOutbox, record.Key)
		default:
			return fmt.Errorf("invalid collaboration outbox operation %q", record.Op)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return a.validateCollabBlobFilesLocked()
}

func (a *App) validateCollabBlobFilesLocked() error {
	for _, entry := range a.collabOutbox {
		if entry.Kind != "blob" {
			continue
		}
		if info, err := os.Stat(a.collabBlobPath(entry)); err != nil {
			return fmt.Errorf("pending collaboration blob %q is unavailable: %w", entry.BlobID, err)
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("pending collaboration blob %q is not a file", entry.BlobID)
		}
	}
	return nil
}

func (a *App) loadCollabOutboxEntryLocked(entry collabOutboxEntry) {
	if entry.SourceID == "" || !validDocumentID(entry.DocumentID) ||
		(entry.Kind == "blob" && entry.BlobID == "") ||
		(entry.Kind != "blob" && entry.MessageID == "") {
		return
	}
	if entry.Order == 0 {
		a.outboxOrder++
		entry.Order = a.outboxOrder
	} else if entry.Order > a.outboxOrder {
		a.outboxOrder = entry.Order
	}
	a.collabOutbox[collabOutboxKey(entry)] = entry
}

func (a *App) appendCollabOutboxRecordLocked(record collabOutboxRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.outboxPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(a.outboxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		a.outboxOps++
	}
	return err
}

// compactCollabOutboxLocked periodically bounds journal replay without putting
// whole-backlog rewrites on the hot path.
func (a *App) compactCollabOutboxLocked() error {
	entries := make([]collabOutboxEntry, 0, len(a.collabOutbox))
	for _, entry := range a.collabOutbox {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Order == entries[j].Order {
			return collabOutboxKey(entries[i]) < collabOutboxKey(entries[j])
		}
		return entries[i].Order < entries[j].Order
	})
	var data []byte
	for i := range entries {
		record, err := json.Marshal(collabOutboxRecord{Op: "put", Entry: &entries[i]})
		if err != nil {
			return err
		}
		data = append(data, record...)
		data = append(data, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(a.outboxPath), 0o755); err != nil {
		return err
	}
	if err := atomicWrite(a.outboxPath, data); err != nil {
		return err
	}
	a.outboxOps = uint64(len(entries))
	return nil
}

func (a *App) maybeCompactCollabOutboxLocked() {
	threshold := uint64(len(a.collabOutbox)*4 + 256)
	if a.outboxOps > threshold {
		if err := a.compactCollabOutboxLocked(); err != nil {
			log.Printf("could not compact collaboration outbox journal: %v", err)
		}
	}
}

func (a *App) enqueueCollabOutbox(entry collabOutboxEntry) error {
	a.outboxMu.Lock()
	defer a.outboxMu.Unlock()
	key := collabOutboxKey(entry)
	previous, existed := a.collabOutbox[key]
	if existed {
		if previous.SourceID != entry.SourceID || previous.Bucket != entry.Bucket || previous.DocumentID != entry.DocumentID || previous.Kind != entry.Kind || previous.MessageID != entry.MessageID || previous.BlobID != entry.BlobID || previous.Data != entry.Data {
			return errors.New("collaboration message id reused with different data")
		}
		return nil
	}
	a.outboxOrder++
	entry.Order = a.outboxOrder
	if err := a.appendCollabOutboxRecordLocked(collabOutboxRecord{Op: "put", Entry: &entry}); err != nil {
		a.outboxOrder--
		return err
	}
	a.collabOutbox[key] = entry
	a.maybeCompactCollabOutboxLocked()
	return nil
}

// ensureNoCollabOutboxBucket prevents deleting a bucket while this process has
// durable updates that have not reached JetStream yet.
func (a *App) ensureNoCollabOutboxBucket(sourceID, bucket string) error {
	a.outboxMu.Lock()
	defer a.outboxMu.Unlock()
	for _, entry := range a.collabOutbox {
		if entry.SourceID == sourceID && entry.Bucket == bucket {
			return errors.New("bucket has collaboration changes waiting to be saved")
		}
	}
	return nil
}

func (a *App) removeCollabOutbox(entry collabOutboxEntry) {
	a.outboxMu.Lock()
	key := collabOutboxKey(entry)
	if _, exists := a.collabOutbox[key]; !exists {
		a.outboxMu.Unlock()
		return
	}
	if err := a.appendCollabOutboxRecordLocked(collabOutboxRecord{Op: "del", Key: key}); err != nil {
		// Retain the entry in memory and retry. Removing it without a durable delete
		// would let the UI overclaim remote synchronization before restart safety.
		log.Printf("could not record collaboration outbox acknowledgement: %v", err)
		a.outboxMu.Unlock()
		return
	}
	delete(a.collabOutbox, key)
	a.maybeCompactCollabOutboxLocked()
	pending := a.collabOutboxCountLocked(entry.DocumentID)
	a.outboxMu.Unlock()
	if entry.Kind == "blob" {
		if err := os.Remove(a.collabBlobPath(entry)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("could not remove replicated collaboration blob: %v", err)
		}
	}
	a.emit("canvas:collab-outbox", map[string]any{"documentId": entry.DocumentID, "pending": pending})
}

func (a *App) collabOutboxCountLocked(documentID string) int {
	count := 0
	for _, entry := range a.collabOutbox {
		if entry.DocumentID == documentID {
			count++
		}
	}
	return count
}

func (a *App) flushCollabOutbox() {
	a.outboxMu.Lock()
	entries := make([]collabOutboxEntry, 0, len(a.collabOutbox))
	for _, entry := range a.collabOutbox {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Order == entries[j].Order {
			return collabOutboxKey(entries[i]) < collabOutboxKey(entries[j])
		}
		return entries[i].Order < entries[j].Order
	})
	a.outboxMu.Unlock()
	// Documents are independent and may replicate in parallel, but each document
	// must preserve durable append order. In particular, do not publish a newer
	// background update after an older update for that document failed.
	groups := make([][]collabOutboxEntry, 0)
	groupIndex := make(map[string]int)
	for _, entry := range entries {
		key := collabOutboxDocumentKey(entry)
		index, ok := groupIndex[key]
		if !ok {
			index = len(groups)
			groupIndex[key] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], entry)
	}
	const maxParallelDocuments = 4
	sem := make(chan struct{}, maxParallelDocuments)
	var workers sync.WaitGroup
	for _, group := range groups {
		group := group
		workers.Add(1)
		go func() {
			defer workers.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a.flushCollabOutboxDocument(group)
		}()
	}
	workers.Wait()
}

func (a *App) flushCollabOutboxDocument(entries []collabOutboxEntry) {
	if len(entries) == 0 {
		return
	}
	a.mu.Lock()
	store := a.store
	var client *natsClient
	var err error
	if store != nil {
		client, err = store.clientFor(entries[0].SourceID)
	}
	a.mu.Unlock()
	if store == nil || err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Kind == "blob" {
			data, readErr := os.ReadFile(a.collabBlobPath(entry))
			if readErr != nil {
				log.Printf("could not read pending collaboration blob: %v", readErr)
				return
			}
			if err = client.writeCollabFile(entry.Bucket, entry.DocumentID, entry.BlobID, data); err != nil {
				return
			}
		} else if _, err = client.publishCollabUpdate(entry.Bucket, entry.DocumentID, entry.MessageID, []byte(entry.Data)); err != nil {
			return
		}
		a.removeCollabOutbox(entry)
	}
}

func (a *App) collabOutboxLoop() {
	a.flushCollabOutbox()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.flushCollabOutbox()
		case <-a.outboxWake:
			a.flushCollabOutbox()
		case <-a.stop:
			return
		}
	}
}

func (a *App) CollabPublish(sourceID, bucket, documentID, messageID, data string) (string, error) {
	if sourceID == localSourceID {
		return "0", nil
	}
	entry := collabOutboxEntry{SourceID: sourceID, Bucket: bucket, DocumentID: documentID, MessageID: messageID, Data: data}
	if !validDocumentID(documentID) || messageID == "" {
		return "", errors.New("invalid collaboration update")
	}
	if err := a.enqueueCollabOutbox(entry); err != nil {
		return "", fmt.Errorf("could not persist collaboration update: %w", err)
	}
	a.outboxMu.Lock()
	pending := a.collabOutboxCountLocked(documentID)
	a.outboxMu.Unlock()
	a.emit("canvas:collab-outbox", map[string]any{"documentId": documentID, "pending": pending})
	// The durable local append is the navigation barrier. Wake the background
	// replicator, but never make board switching wait for the remote NATS RTT.
	// The renderer advances snapshot sequence only from its ordered subscription,
	// so it does not depend on a publish acknowledgement here.
	select {
	case a.outboxWake <- struct{}{}:
	default:
	}
	return "0", nil
}

// CollabPutFiles stores immutable JSON blobs outside the JetStream update log,
// avoiding the server's per-message payload limit. Values are normally
// Excalidraw files; an oversized scene batch uses the same blob channel.
func (a *App) CollabPutFiles(sourceID, bucket, documentID, filesJSON string) error {
	if sourceID == localSourceID {
		return nil
	}
	if sourceID == "" || !validDocumentID(documentID) {
		return errors.New("invalid collaboration files")
	}
	var files map[string]json.RawMessage
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return fmt.Errorf("invalid collaboration files: %w", err)
	}
	for fileID, data := range files {
		if fileID == "" || len(data) == 0 {
			return errors.New("invalid collaboration file")
		}
		entry := collabOutboxEntry{SourceID: sourceID, Bucket: bucket, DocumentID: documentID, Kind: "blob", BlobID: fileID}
		path := a.collabBlobPath(entry)
		if existing, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(existing, data) {
				return errors.New("collaboration blob key reused with different data")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("could not create collaboration blob outbox: %w", err)
		} else if err := atomicWrite(path, data); err != nil {
			return fmt.Errorf("could not persist collaboration blob: %w", err)
		}
		if err := a.enqueueCollabOutbox(entry); err != nil {
			return err
		}
	}
	a.outboxMu.Lock()
	pending := a.collabOutboxCountLocked(documentID)
	a.outboxMu.Unlock()
	a.emit("canvas:collab-outbox", map[string]any{"documentId": documentID, "pending": pending})
	select {
	case a.outboxWake <- struct{}{}:
	default:
	}
	return nil
}

// CollabGetFiles returns requested collaboration blobs as a JSON object.
func (a *App) CollabGetFiles(sourceID, bucket, documentID string, fileIDs []string) (string, error) {
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return "{}", nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	files := make(map[string]json.RawMessage, len(fileIDs))
	for _, fileID := range fileIDs {
		data, err := client.readCollabFile(bucket, documentID, fileID)
		if err != nil {
			return "", err
		}
		if data != nil {
			files[fileID] = data
		}
	}
	data, err := json.Marshal(files)
	return string(data), err
}

// CollabJoin subscribes after the immutable snapshot already loaded by the
// canvas. It returns snapshot data only when a newer compaction raced the board
// switch, avoiding duplicate large snapshot transfers on high-latency links.
func (a *App) CollabJoin(sourceID, bucket, documentID, subscriberID, baselineSequenceText string) (CollabJoinResult, error) {
	if sourceID == localSourceID {
		return CollabJoinResult{}, nil
	}
	if !validDocumentID(documentID) {
		return CollabJoinResult{}, errors.New("invalid collaboration document id")
	}
	if subscriberID == "" {
		return CollabJoinResult{}, errors.New("invalid collaboration subscriber id")
	}
	baselineSequence, err := strconv.ParseUint(baselineSequenceText, 10, 64)
	if err != nil {
		return CollabJoinResult{}, errors.New("invalid collaboration baseline sequence")
	}
	key := sourceID + "\x00" + bucket + "\x00" + documentID + "\x00" + subscriberID
	a.mu.Lock()
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return CollabJoinResult{}, err
	}
	var snapshotData []byte
	snapshotSequence := baselineSequence
	if snapshotSequence == 0 {
		snapshotData, snapshotSequence, err = client.latestCollabSnapshot(bucket, documentID)
		if err != nil {
			return CollabJoinResult{}, err
		}
	}
	a.outboxMu.Lock()
	pendingLocalUpdates := a.collabOutboxCountLocked(documentID)
	a.outboxMu.Unlock()
	result := CollabJoinResult{
		SnapshotSequence:      strconv.FormatUint(snapshotSequence, 10),
		ReplayThroughSequence: strconv.FormatUint(snapshotSequence, 10),
		PendingLocalUpdates:   pendingLocalUpdates,
	}
	if snapshotData != nil {
		text := string(snapshotData)
		result.Snapshot = &text
	}
	a.subMu.Lock()
	// Serialize subscription creation for this process. Holding subMu here is
	// intentional: otherwise two overlapping joins can both pass the check,
	// create consumers, and emit every replayed update twice.
	defer a.subMu.Unlock()
	if a.subscriptions[key] != nil {
		return result, nil
	}
	stream, err := client.ensureCollabStream(bucket)
	if err != nil {
		return CollabJoinResult{}, err
	}
	opts := []nats.SubOpt{nats.BindStream(stream), nats.ManualAck(), nats.AckExplicit()}
	if snapshotSequence > 0 {
		opts = append(opts, nats.StartSequence(snapshotSequence+1))
	} else {
		opts = append(opts, nats.DeliverAll())
	}
	updates, err := client.js.Subscribe(collabUpdateSubject(bucket, documentID), func(msg *nats.Msg) {
		metadata, metadataErr := msg.Metadata()
		if metadataErr != nil {
			_ = msg.Nak()
			return
		}
		sequence := strconv.FormatUint(metadata.Sequence.Stream, 10)
		// Register before emitting: a fast renderer may acknowledge immediately.
		a.subMu.Lock()
		if a.subscriptions[key] == nil {
			a.subMu.Unlock()
			_ = msg.Nak()
			return
		}
		a.collabPending[key+"\x00"+sequence] = msg
		a.subMu.Unlock()
		a.emit("canvas:collab", map[string]any{
			"documentId":   documentID,
			"subscriberId": subscriberID,
			"data":         string(msg.Data),
			"sequence":     sequence,
			"delivery":     strconv.FormatUint(metadata.Sequence.Consumer, 10),
		})
	}, opts...)
	if err != nil {
		client.invalidateCollabStream(bucket)
		return CollabJoinResult{}, err
	}
	client.trackSub(updates)
	presence, err := client.conn.Subscribe(collabPresenceSubject(bucket, documentID), func(msg *nats.Msg) {
		a.emit("canvas:collab", map[string]any{"documentId": documentID, "subscriberId": subscriberID, "data": string(msg.Data), "sequence": "0", "delivery": "0"})
	})
	if err != nil {
		_ = updates.Unsubscribe()
		return CollabJoinResult{}, err
	}
	client.trackSub(presence)
	// Close the snapshot-discovery/stream-compaction race. If a compactor wrote a
	// newer snapshot after our first lookup (and possibly purged older covered
	// updates), return that snapshot. Already emitted replay events are buffered by
	// the renderer and reconciled after this baseline, so duplicates are harmless.
	latestData, latestSequence, err := client.collabSnapshotAfter(bucket, documentID, snapshotSequence)
	if err != nil {
		_ = updates.Unsubscribe()
		_ = presence.Unsubscribe()
		return CollabJoinResult{}, err
	}
	if latestSequence > snapshotSequence {
		result.SnapshotSequence = strconv.FormatUint(latestSequence, 10)
		if latestData != nil {
			text := string(latestData)
			result.Snapshot = &text
		}
	}
	a.subscriptions[key] = updates
	a.subscriptions[key+"\x00presence"] = presence
	if last, lastErr := client.js.GetLastMsg(stream, collabUpdateSubject(bucket, documentID)); lastErr == nil {
		result.ReplayThroughSequence = strconv.FormatUint(last.Sequence, 10)
	} else if !errors.Is(lastErr, nats.ErrMsgNotFound) {
		_ = updates.Unsubscribe()
		_ = presence.Unsubscribe()
		delete(a.subscriptions, key)
		delete(a.subscriptions, key+"\x00presence")
		return CollabJoinResult{}, lastErr
	}
	return result, nil
}

// CollabAck confirms that the renderer received and validated one durable
// update. JetStream redelivers it if the Wails event was lost or the renderer
// disappeared before reaching this call.
func (a *App) CollabAck(sourceID, bucket, documentID, subscriberID, sequence string) error {
	if sourceID == localSourceID {
		return nil
	}
	if !validDocumentID(documentID) || subscriberID == "" {
		return errors.New("invalid collaboration acknowledgement")
	}
	if value, err := strconv.ParseUint(sequence, 10, 64); err != nil || value == 0 {
		return errors.New("invalid collaboration sequence")
	}
	key := sourceID + "\x00" + bucket + "\x00" + documentID + "\x00" + subscriberID + "\x00" + sequence
	a.subMu.Lock()
	msg := a.collabPending[key]
	delete(a.collabPending, key)
	a.subMu.Unlock()
	if msg == nil {
		// Duplicate renderer acknowledgements are idempotent.
		return nil
	}
	// The renderer has already validated and applied this update before calling
	// us. AckSync adds one NATS round trip to every replayed scene, turning a
	// minute of retained edits into hundreds of serial network RTTs on load.
	// A normal ACK is safe here: if its protocol publish is lost, JetStream
	// redelivers the update and renderer reconciliation is idempotent.
	return msg.Ack()
}

// CollabLeave stops consuming a document while its canvas is closed. Without
// this, the old ephemeral consumer would keep acknowledging updates that no
// active frontend applies, and reopening the board could not replay them.
func (a *App) CollabLeave(sourceID, bucket, documentID, subscriberID string) error {
	if sourceID == localSourceID {
		return nil
	}
	key := sourceID + "\x00" + bucket + "\x00" + documentID + "\x00" + subscriberID
	a.subMu.Lock()
	updates := a.subscriptions[key]
	presence := a.subscriptions[key+"\x00presence"]
	delete(a.subscriptions, key)
	delete(a.subscriptions, key+"\x00presence")
	for pendingKey := range a.collabPending {
		if strings.HasPrefix(pendingKey, key+"\x00") {
			delete(a.collabPending, pendingKey)
		}
	}
	a.subMu.Unlock()
	var errs []error
	if updates != nil {
		errs = append(errs, updates.Unsubscribe())
	}
	if presence != nil {
		errs = append(errs, presence.Unsubscribe())
	}
	return errors.Join(errs...)
}

// CollabSnapshot writes an immutable, sequence-addressed JSON materialization.
// The lease is verified with CAS in this same backend call so a stale frontend
// cannot write after another compactor has taken over.
func (a *App) CollabSnapshot(sourceID, bucket, boardID, documentID, sequenceText, jsonText, owner string) error {
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || sequence == 0 {
		return errors.New("invalid collaboration snapshot sequence")
	}
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	owned, err := client.renewLock(bucket, documentID, owner, 30*time.Second)
	if err != nil {
		return err
	}
	if !owned {
		return errors.New("collaboration snapshot lease lost")
	}
	if sidecar, err := client.readObject(bucket, documentMetaName(boardID)); err != nil {
		return err
	} else if strings.TrimSpace(string(sidecar)) != documentID {
		return errors.New("collaboration board was renamed or deleted")
	}
	if deleted, err := client.readObject(bucket, collabDeletedName(documentID)); err != nil {
		return err
	} else if deleted != nil {
		return errors.New("collaboration board was deleted")
	}
	data := []byte(jsonText)
	snapshotName := collabSnapshotName(documentID, sequence, data)
	if _, err := client.writeCollabSnapshotObject(bucket, documentID, sequence, data); err != nil {
		return err
	}
	// Snapshot objects can be large. Re-fence immediately before touching the
	// mutable compatibility materialization rather than relying on the lease
	// check that happened before the ObjectStore upload.
	owned, err = client.renewLock(bucket, documentID, owner, 30*time.Second)
	if err != nil {
		return err
	}
	if !owned {
		_ = client.deleteObject(bucket, snapshotName)
		return errors.New("collaboration snapshot lease lost during upload")
	}
	// Keep the ordinary board object as a durable materialization for export,
	// thumbnails, and disaster recovery. Only the CAS-elected compactor writes it.
	if strings.HasPrefix(boardID, ".") || !strings.HasSuffix(boardID, fileExtension) {
		return errors.New("invalid collaboration board id")
	}
	if err := client.writeObject(bucket, boardID, data); err != nil {
		return err
	}
	// Close the check/write race with TrashBoard: if deletion won while this
	// materialization was in flight, remove the object we just recreated.
	if deleted, err := client.readObject(bucket, collabDeletedName(documentID)); err != nil {
		return err
	} else if deleted != nil {
		_ = client.deleteObject(bucket, snapshotName)
		_ = client.deleteObject(bucket, boardID)
		return errors.New("collaboration board was deleted")
	}
	if sidecar, err := client.readObject(bucket, documentMetaName(boardID)); err != nil {
		return err
	} else if strings.TrimSpace(string(sidecar)) != documentID {
		_ = client.deleteObject(bucket, snapshotName)
		_ = client.deleteObject(bucket, boardID)
		return errors.New("collaboration board was renamed or deleted")
	}
	// Activate only after every potentially slow upload/materialization and all
	// deletion/identity checks. Once indexed, this immutable object is never
	// deleted, so the snapshot pointer cannot dangle if the lease is lost later.
	owned, err = client.renewLock(bucket, documentID, owner, 30*time.Second)
	if err != nil {
		return err
	}
	if !owned {
		_ = client.deleteObject(bucket, snapshotName)
		return errors.New("collaboration snapshot lease lost before activation")
	}
	if err := client.activateCollabSnapshot(bucket, documentID, snapshotName, sequence); err != nil {
		return err
	}
	// Compact only through the previous snapshot, not the one just written. A
	// joining client may have discovered that previous snapshot immediately
	// before this write; retaining one interval guarantees its replay baseline.
	previous, err := client.previousCollabSnapshotSequence(bucket, documentID)
	if err != nil {
		log.Printf("could not inspect collaboration snapshots for compaction: %v", err)
	} else if previous > 0 {
		if err := client.purgeCollabUpdates(bucket, documentID, previous); err != nil {
			log.Printf("could not compact collaboration update log: %v", err)
		}
	}
	return nil
}

// CollabAcquireLock tries to become the snapshot compactor for a document. Live
// update publishing does not depend on this lease.
func (a *App) CollabAcquireLock(sourceID, bucket, documentID, owner string, ttlSeconds int) (bool, error) {
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return true, nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return false, err
	}
	return client.acquireLock(bucket, documentID, owner, time.Duration(ttlSeconds)*time.Second)
}

// CollabRenewLock extends the leader lease. Returns false when this client no
// longer owns it (it should immediately stop writing the bucket).
func (a *App) CollabRenewLock(sourceID, bucket, documentID, owner string, ttlSeconds int) (bool, error) {
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return true, nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return false, err
	}
	return client.renewLock(bucket, documentID, owner, time.Duration(ttlSeconds)*time.Second)
}

// CollabReleaseLock drops the leader lease, but only if this client still owns
// it. Safe to call when not the owner.
func (a *App) CollabReleaseLock(sourceID, bucket, documentID, owner string) error {
	a.mu.Lock()
	if sourceID == localSourceID {
		a.mu.Unlock()
		return nil
	}
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return client.releaseLock(bucket, documentID, owner)
}

func (a *App) SetWindowTitle(title string) {
	if a.window != nil {
		a.window.SetTitle(title)
	}
}
func (a *App) ToggleFullscreen() {
	if a.window != nil {
		a.window.ToggleFullscreen()
	}
}

func (a *App) refreshDetails() {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return
	}
	boards := append([]Board(nil), a.store.boards...)
	show := *a.store.prefs.ShowThumbnails
	a.mu.Unlock()
	for _, board := range boards {
		a.mu.Lock()
		current := a.store.board(board.ID)
		var data []byte
		var err error
		var stamp string
		skip := false
		if current != nil {
			stamp = fmt.Sprintf("%d:%d", board.Modified.UnixNano(), board.Size)
			// Re-reading every board costs an object-store fetch (and one ordered
			// consumer) per board. A background board only needs that when its
			// object changed; the open board is always refreshed because
			// collaboration snapshots do not touch the board object itself.
			skip = !a.store.detailsStale(board, stamp)
			if !skip {
				data, err = a.store.readBoard(*current)
			}
		}
		a.mu.Unlock()
		if skip || err != nil || data == nil {
			continue
		}
		texts := searchableTexts(data)
		thumbnail := ""
		if show {
			thumbnail = a.renderThumbnail(string(data))
		}
		a.mu.Lock()
		if a.store.board(board.ID) != nil {
			detail := a.store.details[board.ID]
			detail.Texts = texts
			if thumbnail != "" {
				detail.Thumbnail = thumbnail
			}
			a.store.details[board.ID] = detail
			a.store.detailStamp[board.ID] = stamp
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	if a.store != nil {
		a.emitSnapshotLocked()
	}
	a.mu.Unlock()
}

func (a *App) renderThumbnail(jsonText string) string {
	id := a.nextID("thumbnail")
	result := make(chan string, 1)
	a.thumbnails.Store(id, result)
	a.emit("canvas:render-thumbnail", map[string]any{"requestId": id, "json": jsonText, "maxSize": 240})
	select {
	case value := <-result:
		return value
	case <-time.After(10 * time.Second):
		a.thumbnails.Delete(id)
		return ""
	}
}

func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".bmp":
		return "image/bmp"
	case ".avif":
		return "image/avif"
	case ".excalidraw", ".excalidrawlib", ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// boardsFolder returns a usable default boards folder. It prefers
// ~/Documents/Whiteboards but falls back to ~/Whiteboards when the preferred
// location is blocked. On macOS, Documents/Desktop/Downloads are protected by
// TCC (filesystem access), and a self-signed dev build is denied silently and
// without a prompt, which breaks both writing and watching boards.
func boardsFolder(home string) string {
	for _, folder := range []string{
		filepath.Join(home, "Documents", "Whiteboards"),
		filepath.Join(home, "Whiteboards"),
	} {
		if boardsFolderUsable(folder) {
			return folder
		}
	}
	return filepath.Join(home, "Documents", "Whiteboards")
}

// boardsFolderUsable reports whether folder can be created, written to, and
// watched. A folder that passes the write probe but fails to be watched is not
// usable in practice, so both are checked.
func boardsFolderUsable(folder string) bool {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return false
	}
	probe, err := os.CreateTemp(folder, ".wprobe-*")
	if err != nil {
		return false
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return true // cannot determine; don't penalize a working folder
	}
	defer w.Close()
	return w.Add(folder) == nil
}

// startWatcher watches every local boards folder, replacing any previous
// watcher. A change rescans the active folder; changes elsewhere are cheap no-ops.
func (a *App) startWatcher(folders []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watch boards folders: %v", err)
		return
	}
	for _, folder := range folders {
		if err = watcher.Add(folder); err != nil {
			log.Printf("watch boards folder %s: %v", folder, err)
		}
	}
	a.watcherMu.Lock()
	old := a.watcher
	a.watcher = watcher
	a.watcherMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go func() {
		for {
			select {
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				if a.fsHandling.CompareAndSwap(false, true) {
					go a.handleFilesystemChange()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("boards folder watcher: %v", err)
			case <-a.stop:
				return
			}
		}
	}()
}

func (a *App) handleFilesystemChange() {
	defer a.fsHandling.Store(false)
	time.Sleep(150 * time.Millisecond)
	a.mu.Lock()
	if a.store == nil || !a.store.isLocal() {
		a.mu.Unlock()
		return
	}
	selected := a.store.selection
	oldKnown := append([]byte(nil), a.store.known[selected]...)
	_ = a.store.scan()
	board := a.store.board(selected)
	if board == nil {
		a.emitSnapshotLocked()
		a.mu.Unlock()
		return
	}
	diskData, err := a.store.readBoard(*board)
	if err != nil || string(diskData) == string(oldKnown) {
		a.emitSnapshotLocked()
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	pending, pendingErr := a.takePending()
	if pendingErr != nil {
		log.Printf("could not collect pending edits before external file merge: %v", pendingErr)
		// Do not reopen the file from disk when the renderer could not provide its
		// pending state. Doing so would replace potentially newer in-memory edits
		// with the externally modified file—the same data-loss class that the
		// navigation durability barrier is designed to prevent.
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil || !a.store.isLocal() || a.store.selection != selected {
		return
	}
	board = a.store.board(selected)
	if board == nil {
		return
	}
	diskData, err = a.store.readBoard(*board)
	if err != nil {
		return
	}
	if pending != nil && pending.JSON != string(diskData) && pending.JSON != string(oldKnown) {
		name := a.store.uniqueName(board.Name + " conflict " + time.Now().Format("2006-01-02 150405"))
		id := name + fileExtension
		if err = a.store.writeBoard(id, []byte(pending.JSON), true); err == nil {
			a.store.known[id] = []byte(pending.JSON)
		}
	}
	a.store.known[selected] = append([]byte(nil), diskData...)
	_ = a.store.scan()
	a.emitSnapshotLocked()
	_ = a.openSelectedLocked()
	go a.refreshDetails()
}

func (a *App) remoteRefreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.mu.Lock()
			if a.store != nil {
				// Surface a dropped connection even while the board list cannot
				// be refreshed: reconnects are unlimited, so a silent retry
				// would look like an app that quietly stopped working.
				if a.store.syncConnectionNotice() {
					a.emitSnapshotLocked()
				}
			}
			if a.store != nil && !a.store.isLocal() {
				before := a.store.selection
				if err := a.store.scan(); err == nil {
					a.emitSnapshotLocked()
					if before != "" && a.store.selection == "" {
						a.emit("canvas:open", map[string]any{"name": "", "json": emptyBoardJSON, "view": nil})
					}
				}
			}
			a.mu.Unlock()
		case <-a.stop:
			return
		}
	}
}

// natsStatsLoop logs connection and subscription counters every half minute. It
// runs only when EXCALIBUR_DEBUG_NATS is set, and exists to attribute delivered
// messages to subscriptions while diagnosing collaboration traffic.
func (a *App) natsStatsLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		var clients []*natsClient
		if a.store != nil {
			clients = a.store.clients()
		}
		a.mu.Unlock()
		for _, client := range clients {
			client.logStats()
		}
		select {
		case <-ticker.C:
		case <-a.stop:
			return
		}
	}
}

// refreshBucketsOnStartup re-lists the buckets on every configured NATS server
// shortly after launch so the sidebar shows buckets that appeared while the app
// was closed. It runs in the background so startup is never blocked by a slow or
// unreachable server; failures are logged rather than surfaced as alerts.
func (a *App) refreshBucketsOnStartup() {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return
	}
	err := a.store.refreshBuckets()
	if scanErr := a.store.scan(); scanErr != nil {
		err = errors.Join(err, scanErr)
	}
	a.emitSnapshotLocked()
	a.mu.Unlock()
	if err != nil {
		log.Printf("bucket refresh on startup: %v", err)
	}
}
