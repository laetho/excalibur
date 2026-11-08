package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	nats "github.com/nats-io/nats.go"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type pendingEdit struct{ Name, JSON string }

const (
	minApplicationZoom     = 0.5
	maxApplicationZoom     = 2.0
	defaultApplicationZoom = 1.0
	applicationZoomStep    = 0.1
)

type App struct {
	ctx           context.Context
	wailsApp      *application.App
	window        application.Window
	mu            sync.Mutex
	store         *Store
	pending       sync.Map
	thumbnails    sync.Map
	subMu         sync.Mutex
	subscriptions map[string]*nats.Subscription
	requestID     atomic.Uint64
	watcherMu     sync.Mutex
	watcher       *fsnotify.Watcher
	fsHandling    atomic.Bool
	stop          chan struct{}
}

func newApp() *App {
	return &App{subscriptions: map[string]*nats.Subscription{}, stop: make(chan struct{})}
}

func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.ctx = ctx
	config, err := os.UserConfigDir()
	if err != nil {
		config = "."
	}
	home, _ := os.UserHomeDir()
	folder := filepath.Join(home, "Documents", "Whiteboards")
	store, err := newStore(preferencesPath(config), folder)
	if err != nil {
		return fmt.Errorf("initialise store: %w", err)
	}
	a.mu.Lock()
	a.store = store
	applicationZoom := store.prefs.ApplicationZoom
	a.mu.Unlock()
	if a.window != nil {
		a.window.SetZoom(applicationZoom)
	}
	a.startWatcher(folder)
	go a.remoteRefreshLoop()
	return nil
}

func (a *App) ServiceShutdown() error {
	a.savePending()
	close(a.stop)
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
	a.subMu.Unlock()
	a.mu.Lock()
	if a.store != nil {
		a.store.close()
	}
	a.mu.Unlock()
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

func (a *App) CanvasReady() error { go a.refreshDetails(); return nil }

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

func (a *App) PendingResult(requestID string, name, jsonText *string) {
	value := (*pendingEdit)(nil)
	if name != nil && jsonText != nil {
		value = &pendingEdit{Name: *name, JSON: *jsonText}
	}
	if found, ok := a.pending.LoadAndDelete(requestID); ok {
		found.(chan *pendingEdit) <- value
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

func (a *App) takePending() *pendingEdit {
	id := a.nextID("pending")
	result := make(chan *pendingEdit, 1)
	a.pending.Store(id, result)
	a.emit("canvas:take-pending", map[string]any{"requestId": id})
	select {
	case value := <-result:
		return value
	case <-time.After(2 * time.Second):
		a.pending.Delete(id)
		return nil
	}
}

func (a *App) savePending() {
	pending := a.takePending()
	if pending == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		_ = a.store.saveBoard(pending.Name, pending.JSON)
	}
}

func (a *App) openSelectedLocked() error {
	board := a.store.board(a.store.selection)
	if board == nil {
		return nil
	}
	data, err := a.store.readBoard(*board)
	if err != nil {
		return err
	}
	a.store.known[board.ID] = append([]byte(nil), data...)
	var view *Viewport
	if saved, ok := a.store.prefs.Viewports[a.store.viewportKey(board.ID)]; ok {
		copy := saved
		view = &copy
	}
	a.emit("canvas:open", map[string]any{"name": board.Name, "json": string(data), "view": view})
	library, err := a.store.library()
	if err == nil {
		a.emit("canvas:set-library", map[string]any{"json": library})
	}
	return err
}

func (a *App) SelectBoard(id string) error {
	a.savePending()
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
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.createBoard(name); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}

func (a *App) RenameBoard(id, name string) error {
	a.savePending()
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
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.duplicateBoard(id); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}
func (a *App) TrashBoard(id string) error {
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.trashBoard(id); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) ImportPaths(paths []string) error {
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.importPaths(paths); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return a.openSelectedLocked()
}
func (a *App) ImportBoardData(name, jsonText string) error {
	a.savePending()
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

func (a *App) ChooseFolder() error {
	folder, err := a.wailsApp.Dialog.OpenFile().SetTitle("Choose Boards Folder").CanChooseFiles(false).CanChooseDirectories(true).PromptForSingleSelection()
	if err != nil || folder == "" {
		return err
	}
	a.savePending()
	a.mu.Lock()
	if err = a.store.changeFolder(folder); err != nil {
		a.mu.Unlock()
		return err
	}
	a.emitSnapshotLocked()
	a.mu.Unlock()
	a.startWatcher(folder)
	return nil
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
func (a *App) RevealFolder() error {
	a.mu.Lock()
	folder := a.store.localFolder
	a.mu.Unlock()
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
func (a *App) RemoveNatsServer(id string) error {
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.removeNatsServer(id); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
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
func (a *App) ActivateSource(sourceID, groupID string) error {
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.activateSource(sourceID, groupID); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}
func (a *App) CopyBoardToSource(boardID, targetSource, targetGroup string) error {
	a.savePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.copyBoardToSource(boardID, targetSource, targetGroup); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) CollabSend(boardID, data string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store.isLocal() {
		return nil
	}
	client, err := a.store.clientFor(a.store.activeSource)
	if err != nil {
		return err
	}
	return client.conn.Publish(collabSubject(a.store.activeGroup, boardID), []byte(data))
}

func (a *App) CollabJoin(sourceID, bucket, boardID string) error {
	if sourceID == localSourceID {
		return nil
	}
	key := sourceID + "\x00" + bucket + "\x00" + boardID
	a.subMu.Lock()
	if a.subscriptions[key] != nil {
		a.subMu.Unlock()
		return nil
	}
	a.subMu.Unlock()
	a.mu.Lock()
	client, err := a.store.clientFor(sourceID)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	sub, err := client.conn.Subscribe(collabSubject(bucket, boardID), func(msg *nats.Msg) {
		a.emit("canvas:collab", map[string]any{"boardId": boardID, "data": string(msg.Data)})
	})
	if err != nil {
		return err
	}
	a.subMu.Lock()
	a.subscriptions[key] = sub
	a.subMu.Unlock()
	return nil
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
		if current != nil {
			data, err = a.store.readBoard(*current)
		}
		a.mu.Unlock()
		if err != nil || data == nil {
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
	case ".excalidraw", ".excalidrawlib", ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func (a *App) startWatcher(folder string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watch boards folder: %v", err)
		return
	}
	if err = watcher.Add(folder); err != nil {
		_ = watcher.Close()
		log.Printf("watch boards folder: %v", err)
		return
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

	pending := a.takePending()
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
