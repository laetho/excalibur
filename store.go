package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	prefsPath    string
	prefs        Preferences
	localFolders []string // never empty; the first is the default
	activeSource string
	activeGroup  string
	boards       []Board
	selection    string
	details      map[string]BoardDetails
	detailStamp  map[string]string // board revision the cached details were built from
	known        map[string][]byte
	knownStamp   map[string]string
	alerts       []Notice

	// clientsMu guards natsClients and closed. Every read and write of the map
	// takes it: clientFor runs from the outbox, refresh and collab paths, and a
	// concurrent map write would abort the process.
	clientsMu   sync.Mutex
	natsClients map[string]*natsClient
	closed      bool
	// dialNATS connects a server; tests replace it to observe dialling.
	dialNATS func(config NatsServerConfig) (*natsClient, error)

	// pinnedCache holds the last listing of each pinned group, keyed by
	// source+"\x00"+group, so the 2 s sidebar refresh does not re-list buckets.
	pinnedCache map[string]pinnedBoardsEntry
}

// preferencesPath migrates the former ExcaliWails preferences on first launch
// so the product rename does not discard existing folders, sources, or settings.
func preferencesPath(configDir string) string {
	path := filepath.Join(configDir, "excalibur", "prefs.json")
	if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return path
	}
	legacyPath := filepath.Join(configDir, "excaliwails", "prefs.json")
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return legacyPath
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return legacyPath
	}
	return path
}

// credentialsDir holds private copies of imported NATS credentials files.
func (s *Store) credentialsDir() string {
	return filepath.Join(filepath.Dir(s.prefsPath), "credentials")
}

func newStore(prefsPath, defaultFolder string) (*Store, error) {
	show := true
	s := &Store{
		prefsPath: prefsPath,
		prefs:     Preferences{Viewports: map[string]Viewport{}, SortOrder: "name", CanvasAppearance: "system", ShowThumbnails: &show},
		details:   map[string]BoardDetails{}, detailStamp: map[string]string{}, known: map[string][]byte{}, knownStamp: map[string]string{}, natsClients: map[string]*natsClient{}, dialNATS: connectNATS, pinnedCache: map[string]pinnedBoardsEntry{},
	}
	if data, err := os.ReadFile(prefsPath); err == nil {
		_ = json.Unmarshal(data, &s.prefs)
	}
	if s.prefs.Viewports == nil {
		s.prefs.Viewports = map[string]Viewport{}
	}
	if s.prefs.SortOrder == "" {
		s.prefs.SortOrder = "name"
	}
	if s.prefs.CanvasAppearance == "" {
		s.prefs.CanvasAppearance = "system"
	}
	if !validApplicationZoom(s.prefs.ApplicationZoom) {
		s.prefs.ApplicationZoom = defaultApplicationZoom
	}
	if s.prefs.AI.Model == "" {
		s.prefs.AI.Model = "dramallama"
	}
	if s.prefs.ShowThumbnails == nil {
		s.prefs.ShowThumbnails = &show
	}
	// Older preferences only have BoardsFolder.
	for _, folder := range append([]string{s.prefs.BoardsFolder}, s.prefs.LocalFolders...) {
		if folder != "" && !slices.Contains(s.localFolders, folder) {
			s.localFolders = append(s.localFolders, folder)
		}
	}
	if len(s.localFolders) == 0 {
		s.localFolders = []string{defaultFolder}
	}
	s.syncLocalFolderPrefs()
	s.activeSource, s.activeGroup = localSourceID, s.localFolders[0]
	if err := os.MkdirAll(s.localFolders[0], 0o755); err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(s.prefs.ActiveSource, "nats:") && s.prefs.ActiveGroup != "":
		s.activeSource, s.activeGroup = s.prefs.ActiveSource, s.prefs.ActiveGroup
	case s.prefs.ActiveSource == localSourceID && slices.Contains(s.localFolders, s.prefs.ActiveGroup):
		s.activeGroup = s.prefs.ActiveGroup
	}
	if err := s.scan(); err != nil {
		s.activeSource, s.activeGroup = localSourceID, s.localFolders[0]
		if localErr := s.scan(); localErr != nil {
			return nil, localErr
		}
		s.alerts = append(s.alerts, Notice{ID: "startup-source", Title: "NATS source unavailable", Message: err.Error()})
	}
	if s.prefs.LastBoard != "" && s.board(s.prefs.LastBoard) != nil {
		s.selection = s.prefs.LastBoard
	}
	return s, nil
}

func (s *Store) close() {
	s.clientsMu.Lock()
	s.closed = true
	clients := make([]*natsClient, 0, len(s.natsClients))
	for _, client := range s.natsClients {
		clients = append(clients, client)
	}
	s.natsClients = map[string]*natsClient{}
	s.clientsMu.Unlock()
	// Closing happens outside the lock so a slow connection teardown cannot
	// stall a request that is only reading the map.
	for _, client := range clients {
		client.close()
	}
}

func (s *Store) isLocal() bool { return s.activeSource == localSourceID }

var errStoreClosed = errors.New("store is closed")

// dial connects a server through the injectable seam.
func (s *Store) dial(config NatsServerConfig) (*natsClient, error) {
	if s.dialNATS == nil {
		return connectNATS(config)
	}
	return s.dialNATS(config)
}

// setClient and takeClient keep natsClients accesses behind clientsMu.
func (s *Store) setClient(id string, client *natsClient) {
	s.clientsMu.Lock()
	s.natsClients[id] = client
	s.clientsMu.Unlock()
}

func (s *Store) takeClient(id string) *natsClient {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	return s.natsClients[id]
}

// detailsStale reports whether cached board details must be rebuilt. Re-reading
// every board costs an object-store fetch and an ordered consumer, so a
// background board is only re-read when its object revision changed. The open
// board is always refreshed because collaboration snapshots do not touch the
// board object itself.
func (s *Store) detailsStale(board Board, stamp string) bool {
	cached, ok := s.detailStamp[board.ID]
	if ok && cached == stamp {
		return !s.isLocal() && board.ID == s.selection
	}
	return true
}

// clients returns the open connections. The slice is a snapshot; the
// connections themselves are safe for concurrent use.
func (s *Store) clients() []*natsClient {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	clients := make([]*natsClient, 0, len(s.natsClients))
	for _, client := range s.natsClients {
		clients = append(clients, client)
	}
	return clients
}

func (s *Store) deleteClient(id string) {
	s.clientsMu.Lock()
	delete(s.natsClients, id)
	s.clientsMu.Unlock()
}

// clientFor returns the connection for a source, dialling it on first use. A
// cached connection that has since closed is dropped and replaced, because a
// reconnect that exhausts its attempts leaves the client permanently closed and
// otherwise keeps serving it for the rest of the session.
func (s *Store) clientFor(sourceID string) (*natsClient, error) {
	id := strings.TrimPrefix(sourceID, "nats:")
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	if s.closed {
		return nil, errStoreClosed
	}
	if c := s.natsClients[id]; c != nil && c.conn != nil && !c.conn.IsClosed() {
		return c, nil
	}
	for _, config := range s.prefs.NatsServers {
		if config.ID == id {
			c, err := s.dial(config)
			if err != nil {
				return nil, err
			}
			s.natsClients[id] = c
			return c, nil
		}
	}
	return nil, fmt.Errorf("unknown NATS server %q", id)
}

const connectionNoticeID = "nats-connection"

// syncConnectionNotice keeps one notice per connected server in step with its
// connection state. Reconnecting is unlimited, so without this an unreachable
// cluster would only ever show up as publishes quietly failing. It reports
// whether any notice changed.
func (s *Store) syncConnectionNotice() bool {
	wanted := map[string]string{}
	for _, config := range s.prefs.NatsServers {
		client := s.takeClient(config.ID)
		if client == nil {
			continue // never dialled, so there is no state to report
		}
		if issue := client.connectionIssue(); issue != "" {
			wanted[connectionNoticeID+":"+config.ID] = fmt.Sprintf("Reconnecting to %s: %s", config.Name, issue)
		}
	}
	changed := false
	kept := s.alerts[:0]
	for _, notice := range s.alerts {
		if !strings.HasPrefix(notice.ID, connectionNoticeID) {
			kept = append(kept, notice)
			continue
		}
		message, stillDown := wanted[notice.ID]
		if !stillDown {
			changed = true
			continue
		}
		delete(wanted, notice.ID)
		if notice.Message != message {
			notice.Message = message
			changed = true
		}
		kept = append(kept, notice)
	}
	s.alerts = kept
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s.alerts = append(s.alerts, Notice{ID: id, Title: "NATS connection", Message: wanted[id]})
		changed = true
	}
	return changed
}

func (s *Store) scan() error {
	var boards []Board
	var err error
	if s.isLocal() {
		boards, err = listLocalBoards(s.activeGroup)
	} else {
		var client *natsClient
		if client, err = s.clientFor(s.activeSource); err == nil {
			boards, err = listRemoteBoards(client, s.activeGroup)
		}
	}
	if err != nil {
		return err
	}
	sort.Slice(boards, func(i, j int) bool { return strings.ToLower(boards[i].Name) < strings.ToLower(boards[j].Name) })
	s.boards = boards
	if s.selection != "" && s.board(s.selection) == nil {
		s.selection = ""
	}
	return nil
}

// listLocalBoards lists the .excalidraw files in one folder without activating it.
func listLocalBoards(folder string) ([]Board, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	boards := make([]Board, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), fileExtension) {
			continue
		}
		path := filepath.Join(folder, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		board := Board{ID: name, Name: strings.TrimSuffix(name, filepath.Ext(name)), Path: path}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !strings.EqualFold(filepath.Ext(target), fileExtension) {
				continue
			}
			targetInfo, err := os.Stat(target)
			if err != nil || !targetInfo.Mode().IsRegular() {
				continue
			}
			board.IsLink, board.Target, board.Modified, board.Size = true, target, targetInfo.ModTime(), uint64(targetInfo.Size())
		} else {
			if !info.Mode().IsRegular() {
				continue
			}
			board.Modified, board.Size = info.ModTime(), uint64(info.Size())
		}
		boards = append(boards, board)
	}
	return boards, nil
}

// listRemoteBoards lists the board objects in one bucket without activating it.
func listRemoteBoards(client *natsClient, bucket string) ([]Board, error) {
	objects, err := client.listObjects(bucket)
	if err != nil {
		return nil, err
	}
	boards := make([]Board, 0, len(objects))
	for _, object := range objects {
		if object == nil || object.Deleted || strings.HasPrefix(object.Name, ".") || !strings.HasSuffix(object.Name, fileExtension) {
			continue
		}
		documentID, err := client.documentID(bucket, object.Name)
		if err != nil {
			return nil, err
		}
		boards = append(boards, Board{ID: object.Name, Name: strings.TrimSuffix(object.Name, fileExtension), DocumentID: documentID, Modified: object.ModTime, Size: object.Size})
	}
	return boards, nil
}

// listGroupBoards lists one group's boards without activating it.
func (s *Store) listGroupBoards(sourceID, group string) ([]Board, error) {
	if sourceID == localSourceID {
		return listLocalBoards(group)
	}
	client, err := s.clientFor(sourceID)
	if err != nil {
		return nil, err
	}
	return listRemoteBoards(client, group)
}

func (s *Store) board(id string) *Board {
	for i := range s.boards {
		if s.boards[i].ID == id {
			return &s.boards[i]
		}
	}
	return nil
}

func (s *Store) readBoard(board Board) ([]byte, error) {
	data, _, err := s.readBoardState(board)
	return data, err
}

// readBoardState returns the materialized scene and the immutable collaboration
// snapshot sequence it came from. The sequence lets the renderer join replay
// after this exact baseline instead of downloading the same snapshot again.
func (s *Store) readBoardState(board Board) ([]byte, uint64, error) {
	if s.isLocal() {
		path := board.Path
		if board.Target != "" {
			path = board.Target
		}
		data, err := os.ReadFile(path)
		return data, 0, err
	}
	client, err := s.clientFor(s.activeSource)
	if err != nil {
		return nil, 0, err
	}
	if board.DocumentID != "" {
		if snapshot, sequence, snapshotErr := client.latestCollabSnapshot(s.activeGroup, board.DocumentID); snapshotErr != nil {
			return nil, 0, snapshotErr
		} else if snapshot != nil {
			return snapshot, sequence, nil
		}
	}
	data, err := client.readObject(s.activeGroup, board.ID)
	return data, 0, err
}

func (s *Store) writeBoard(id string, data []byte, createOnly bool) error {
	if s.isLocal() {
		path := filepath.Join(s.activeGroup, id)
		if board := s.board(id); board != nil && board.Target != "" {
			path = board.Target
		}
		if createOnly {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.Write(data)
			return err
		}
		return atomicWrite(path, data)
	}
	client, err := s.clientFor(s.activeSource)
	if err != nil {
		return err
	}
	return client.writeObject(s.activeGroup, id, data)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".excalibur-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *Store) savePrefs() {
	_ = os.MkdirAll(filepath.Dir(s.prefsPath), 0o755)
	data, _ := json.MarshalIndent(s.prefs, "", "  ")
	_ = atomicWrite(s.prefsPath, data)
}

func (s *Store) setSelection(id string) {
	s.selection = id
	s.prefs.LastBoard = id
	s.savePrefs()
}

func (s *Store) nameProblem(raw, excluding string) string {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "Enter a name."
	case strings.ContainsAny(name, "/:"):
		return "Names can’t contain ‘/’ or ‘:’."
	case strings.HasPrefix(name, "."):
		return "Names can’t start with a period."
	case len([]rune(name)) > 120:
		return "That name is too long."
	}
	for _, b := range s.boards {
		if b.ID != excluding && strings.EqualFold(b.Name, name) {
			return "A board with this name already exists."
		}
	}
	return ""
}

func (s *Store) uniqueName(base string) string {
	name := base
	for n := 2; s.nameProblem(name, "") != ""; n++ {
		name = fmt.Sprintf("%s %d", base, n)
	}
	return name
}

func (s *Store) createBoard(raw string) error {
	if problem := s.nameProblem(raw, ""); problem != "" {
		return errors.New(problem)
	}
	name := strings.TrimSpace(raw)
	id := name + fileExtension
	if err := s.writeBoard(id, []byte(emptyBoardJSON), true); err != nil {
		return err
	}
	if !s.isLocal() {
		client, err := s.clientFor(s.activeSource)
		if err != nil {
			return err
		}
		documentID, err := newDocumentID()
		if err != nil {
			return err
		}
		// Always overwrite a stale sidecar left by a previously deleted board of
		// the same name. A newly created board must never inherit old history.
		if err := client.setDocumentID(s.activeGroup, id, documentID); err != nil {
			return err
		}
	}
	s.known[id] = []byte(emptyBoardJSON)
	if err := s.scan(); err != nil {
		return err
	}
	s.setSelection(id)
	return nil
}

func (s *Store) saveBoard(name, scene string) error {
	id := name + fileExtension
	if s.board(id) == nil {
		return nil
	}
	data := []byte(scene)
	if string(s.known[id]) == scene {
		return nil
	}
	if err := s.writeBoard(id, data, false); err != nil {
		return err
	}
	s.known[id] = append([]byte(nil), data...)
	if board := s.board(id); board != nil {
		board.Modified = time.Now()
		board.Size = uint64(len(data))
	}
	return nil
}

func (s *Store) renameBoard(id, raw string) error {
	board := s.board(id)
	if board == nil {
		return nil
	}
	name := strings.TrimSpace(raw)
	if name == board.Name {
		return nil
	}
	if problem := s.nameProblem(name, id); problem != "" {
		return errors.New(problem)
	}
	newID := name + fileExtension
	if s.isLocal() {
		if err := os.Rename(board.Path, filepath.Join(s.activeGroup, newID)); err != nil {
			return err
		}
	} else {
		data, err := s.readBoard(*board)
		if err != nil {
			return err
		}
		client, err := s.clientFor(s.activeSource)
		if err != nil {
			return err
		}
		if err = client.writeObject(s.activeGroup, newID, data); err != nil {
			return err
		}
		if err = client.setDocumentID(s.activeGroup, newID, board.DocumentID); err != nil {
			return err
		}
		if err = client.deleteObject(s.activeGroup, id); err != nil {
			return err
		}
		_ = client.deleteObject(s.activeGroup, documentMetaName(id))
	}
	if detail, ok := s.details[id]; ok {
		delete(s.details, id)
		s.details[newID] = detail
	}
	if stamp, ok := s.detailStamp[id]; ok {
		delete(s.detailStamp, id)
		s.detailStamp[newID] = stamp
	}
	if known, ok := s.known[id]; ok {
		delete(s.known, id)
		s.known[newID] = known
	}
	wasSelected := s.selection == id
	if err := s.scan(); err != nil {
		return err
	}
	if wasSelected {
		s.setSelection(newID)
	}
	if v, ok := s.prefs.Viewports[s.viewportKey(id)]; ok {
		delete(s.prefs.Viewports, s.viewportKey(id))
		s.prefs.Viewports[s.viewportKey(newID)] = v
		s.savePrefs()
	}
	return nil
}

func (s *Store) duplicateBoard(id string) error {
	board := s.board(id)
	if board == nil {
		return nil
	}
	data, err := s.readBoard(*board)
	if err != nil {
		return err
	}
	name := s.uniqueName(board.Name + " copy")
	newID := name + fileExtension
	if err = s.writeBoard(newID, data, true); err != nil {
		return err
	}
	if !s.isLocal() {
		client, clientErr := s.clientFor(s.activeSource)
		if clientErr != nil {
			return clientErr
		}
		documentID, documentErr := newDocumentID()
		if documentErr != nil {
			return documentErr
		}
		if documentErr = client.setDocumentID(s.activeGroup, newID, documentID); documentErr != nil {
			return documentErr
		}
	}
	if v, ok := s.prefs.Viewports[s.viewportKey(id)]; ok {
		s.prefs.Viewports[s.viewportKey(newID)] = v
	}
	if err = s.scan(); err != nil {
		return err
	}
	s.setSelection(newID)
	return nil
}

func (s *Store) trashBoard(id string) error {
	board := s.board(id)
	if board == nil {
		return nil
	}
	if s.isLocal() {
		if err := moveToTrash(board.Path); err != nil {
			return err
		}
	} else {
		client, err := s.clientFor(s.activeSource)
		if err != nil {
			return err
		}
		// Write the document tombstone before deleting the visible object. A
		// compactor racing this operation checks the tombstone before and after its
		// materialization write, so it cannot resurrect the deleted board.
		if board.DocumentID != "" {
			if err = client.writeObject(s.activeGroup, collabDeletedName(board.DocumentID), []byte("deleted")); err != nil {
				return err
			}
		}
		if err = client.deleteObject(s.activeGroup, id); err != nil {
			return err
		}
		// Snapshots and stream history are immutable audit data, but removing the
		// sidecar makes them unreachable from any future board with this filename.
		_ = client.deleteObject(s.activeGroup, documentMetaName(id))
	}
	delete(s.known, id)
	delete(s.details, id)
	delete(s.prefs.Viewports, s.viewportKey(id))
	if s.selection == id {
		s.setSelection("")
	}
	return s.scan()
}

func moveToTrash(path string) error {
	home, _ := os.UserHomeDir()
	trash := filepath.Join(home, ".local", "share", "Trash", "files")
	if runtime.GOOS == "darwin" {
		trash = filepath.Join(home, ".Trash")
	}
	if runtime.GOOS == "windows" {
		return errors.New("moving files to the Recycle Bin is not supported by this build")
	}
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	base := filepath.Base(path)
	destination := filepath.Join(trash, base)
	for n := 2; ; n++ {
		if _, err := os.Lstat(destination); os.IsNotExist(err) {
			break
		}
		destination = filepath.Join(trash, fmt.Sprintf("%s %d%s", strings.TrimSuffix(base, filepath.Ext(base)), n, filepath.Ext(base)))
	}
	return os.Rename(path, destination)
}

func (s *Store) viewportKey(id string) string {
	if s.isLocal() {
		return filepath.Join(s.activeGroup, id)
	}
	return s.activeSource + ":" + s.activeGroup + "/" + id
}

func (s *Store) saveViewport(name string, view Viewport) {
	if math.IsNaN(view.ScrollX) || math.IsNaN(view.ScrollY) || math.IsNaN(view.Zoom) || math.IsInf(view.ScrollX, 0) || math.IsInf(view.ScrollY, 0) || math.IsInf(view.Zoom, 0) || view.Zoom <= 0 {
		return
	}
	id := name + fileExtension
	if s.board(id) == nil {
		return
	}
	s.prefs.Viewports[s.viewportKey(id)] = view
	s.savePrefs()
}

// library returns the active library: a file in the boards folder for local
// boards, or the one library shared by every bucket on the active NATS server.
func (s *Store) library() (*string, error) {
	var data []byte
	var err error
	if s.isLocal() {
		data, err = os.ReadFile(filepath.Join(s.activeGroup, libraryName))
		if os.IsNotExist(err) {
			return nil, nil
		}
	} else {
		client, e := s.clientFor(s.activeSource)
		if e != nil {
			return nil, e
		}
		data, err = client.readSharedLibrary()
		if err == nil && data == nil {
			// Libraries used to be stored per bucket. Adopt the first one found as the
			// server-wide library so existing shapes aren't lost.
			if legacy, legacyErr := client.readObject(s.activeGroup, libraryName); legacyErr == nil && legacy != nil {
				if client.writeSharedLibrary(legacy) == nil {
					data = legacy
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	text := string(data)
	return &text, nil
}

func (s *Store) saveLibrary(text string) error {
	if s.isLocal() {
		return atomicWrite(filepath.Join(s.activeGroup, libraryName), []byte(text))
	}
	client, err := s.clientFor(s.activeSource)
	if err != nil {
		return err
	}
	return client.writeSharedLibrary([]byte(text))
}

func searchableTexts(data []byte) []string {
	var scene struct {
		Elements []map[string]any `json:"elements"`
	}
	if json.Unmarshal(data, &scene) != nil {
		return []string{}
	}
	seen := map[string]bool{}
	result := []string{}
	for _, element := range scene.Elements {
		if deleted, _ := element["isDeleted"].(bool); deleted {
			continue
		}
		for _, key := range []string{"originalText", "text", "name", "link"} {
			text, _ := element[key].(string)
			text = strings.TrimSpace(text)
			if text != "" && !seen[text] {
				seen[text] = true
				result = append(result, text)
			}
			if key == "originalText" && text != "" {
				break
			}
		}
	}
	return result
}

func (s *Store) refreshTexts() {
	for _, board := range s.boards {
		data, err := s.readBoard(board)
		if err != nil {
			continue
		}
		detail := s.details[board.ID]
		detail.Texts = searchableTexts(data)
		s.details[board.ID] = detail
	}
}

// pinnedBoardsTTL bounds how often a pinned bucket is re-listed; the sidebar
// refreshes every couple of seconds and each listing is a round trip per board.
const pinnedBoardsTTL = 5 * time.Second

type pinnedBoardsEntry struct {
	at     time.Time
	boards []BoardInfo
}

// pinnedBoards lists the boards of the pinned groups other than the active one,
// so they stay visible while another folder or bucket is open. An unreachable
// server keeps showing its last known listing.
func (s *Store) pinnedBoards() []PinnedGroupInfo {
	out := make([]PinnedGroupInfo, 0, len(s.prefs.Pinned))
	for _, pin := range s.prefs.Pinned {
		if pin.Source == s.activeSource && pin.Group == s.activeGroup {
			// The active group already shows its boards, so only tell the sidebar
			// that it is pinned.
			out = append(out, PinnedGroupInfo{Source: pin.Source, Group: pin.Group, Boards: []BoardInfo{}})
			continue
		}
		key := pin.Source + "\x00" + pin.Group
		if entry, ok := s.pinnedCache[key]; ok && time.Since(entry.at) < pinnedBoardsTTL {
			out = append(out, PinnedGroupInfo{Source: pin.Source, Group: pin.Group, Boards: entry.boards})
			continue
		}
		boards, err := s.listGroupBoards(pin.Source, pin.Group)
		if err != nil {
			if entry, ok := s.pinnedCache[key]; ok {
				out = append(out, PinnedGroupInfo{Source: pin.Source, Group: pin.Group, Boards: entry.boards})
			}
			continue
		}
		infos := make([]BoardInfo, 0, len(boards))
		for _, board := range boards {
			info := BoardInfo{ID: board.ID, Name: board.Name, DocumentID: board.DocumentID, Modified: board.Modified.Unix(), IsLink: board.IsLink, Texts: []string{}}
			if board.Target != "" {
				target := board.Target
				info.Target = &target
			}
			infos = append(infos, info)
		}
		s.pinnedCache[key] = pinnedBoardsEntry{at: time.Now(), boards: infos}
		out = append(out, PinnedGroupInfo{Source: pin.Source, Group: pin.Group, Boards: infos})
	}
	return out
}

// togglePinnedGroup pins or unpins a folder or bucket and reports the new state.
func (s *Store) togglePinnedGroup(sourceID, group string) bool {
	for i, pin := range s.prefs.Pinned {
		if pin.Source == sourceID && pin.Group == group {
			s.prefs.Pinned = slices.Delete(s.prefs.Pinned, i, i+1)
			delete(s.pinnedCache, sourceID+"\x00"+group)
			s.savePrefs()
			return false
		}
	}
	s.prefs.Pinned = append(s.prefs.Pinned, PinnedGroup{Source: sourceID, Group: group})
	s.savePrefs()
	return true
}

// unpinGroup forgets a pin, e.g. when its folder or bucket is gone.
func (s *Store) unpinGroup(sourceID, group string) {
	for i, pin := range s.prefs.Pinned {
		if pin.Source == sourceID && pin.Group == group {
			s.prefs.Pinned = slices.Delete(s.prefs.Pinned, i, i+1)
			delete(s.pinnedCache, sourceID+"\x00"+group)
			return
		}
	}
}

// groupState is how a folder or bucket looks in the tree. A group the user has
// never touched shows its find box and an expanded list.
func (s *Store) groupState(sourceID, group string) GroupState {
	for _, state := range s.prefs.Tree {
		if state.Source == sourceID && state.Group == group {
			return state
		}
	}
	return GroupState{Source: sourceID, Group: group, Find: true}
}

// applyGroupState stores a change to a folder or bucket's tree state, and drops
// the entry once it is back to the default so old groups do not build up.
func (s *Store) applyGroupState(patch GroupStatePatch) {
	state := s.groupState(patch.Source, patch.Group)
	if patch.Query != nil {
		state.Query = *patch.Query
	}
	if patch.Find != nil {
		state.Find = *patch.Find
	}
	if patch.Folded != nil {
		state.Folded = *patch.Folded
	}
	state.Source, state.Group = patch.Source, patch.Group
	tree := make([]GroupState, 0, len(s.prefs.Tree)+1)
	for _, existing := range s.prefs.Tree {
		if existing.Source == patch.Source && existing.Group == patch.Group {
			continue
		}
		tree = append(tree, existing)
	}
	if state.Query != "" || !state.Find || state.Folded {
		tree = append(tree, state)
	}
	s.prefs.Tree = tree
	s.savePrefs()
}

// forgetGroup drops the tree state of a folder or bucket that is gone.
func (s *Store) forgetGroup(sourceID, group string) {
	for i, state := range s.prefs.Tree {
		if state.Source == sourceID && state.Group == group {
			s.prefs.Tree = slices.Delete(s.prefs.Tree, i, i+1)
			return
		}
	}
}

func (s *Store) snapshot() StoreSnapshot {
	boards := make([]BoardInfo, 0, len(s.boards))
	for _, board := range s.boards {
		var target *string
		if board.Target != "" {
			value := board.Target
			target = &value
		}
		detail := s.details[board.ID]
		texts := detail.Texts
		if texts == nil {
			texts = []string{}
		}
		var thumbnail *string
		if detail.Thumbnail != "" {
			value := detail.Thumbnail
			thumbnail = &value
		}
		boards = append(boards, BoardInfo{ID: board.ID, Name: board.Name, DocumentID: board.DocumentID, Modified: board.Modified.Unix(), IsLink: board.IsLink, Target: target, Texts: texts, Thumbnail: thumbnail})
	}
	var selection *string
	if s.selection != "" {
		value := s.selection
		selection = &value
	}
	localGroups := make([]GroupInfo, 0, len(s.localFolders))
	for _, folder := range s.localFolders {
		localGroups = append(localGroups, GroupInfo{ID: folder, Name: displayPath(folder), Kind: "folder"})
	}
	pinned := s.pinnedBoards()
	sources := []SourceInfo{{ID: localSourceID, Name: "Local", Kind: "local", Groups: localGroups}}
	natsServers := make([]NatsServerSettings, 0, len(s.prefs.NatsServers))
	for _, server := range s.prefs.NatsServers {
		groups := make([]GroupInfo, 0, len(server.Buckets))
		for _, bucket := range server.Buckets {
			groups = append(groups, GroupInfo{ID: bucket, Name: bucket, Kind: "bucket"})
		}
		sources = append(sources, SourceInfo{ID: "nats:" + server.ID, Name: server.Name, Kind: "nats", Groups: groups})
		natsServers = append(natsServers, NatsServerSettings{ID: server.ID, Name: server.Name, URL: server.URL, Username: server.Username, CredentialsFile: server.CredentialsFile, Buckets: append([]string(nil), server.Buckets...), HasPassword: server.Password != ""})
	}
	folder := ""
	if s.isLocal() {
		folder = displayPath(s.activeGroup)
	}
	ai := AISettings{Enabled: s.prefs.AI.Enabled, BaseURL: s.prefs.AI.BaseURL, Model: s.prefs.AI.Model, HasAPIKey: s.prefs.AI.APIKey != ""}
	return StoreSnapshot{Folder: folder, Boards: boards, Selection: selection, SortOrder: s.prefs.SortOrder, ShowThumbnails: *s.prefs.ShowThumbnails, CanvasAppearance: s.prefs.CanvasAppearance, ApplicationZoom: s.prefs.ApplicationZoom, CollabNickname: s.prefs.CollabNickname, CollabColor: s.prefs.CollabColor, CollabAvatar: s.prefs.CollabAvatar, CollabAvatarOnly: s.prefs.CollabAvatarOnly, Alerts: append([]Notice(nil), s.alerts...), IsAlertPresented: len(s.alerts) > 0, Sources: sources, NatsServers: natsServers, ActiveSource: s.activeSource, ActiveGroup: s.activeGroup, Pinned: pinned, Tree: append([]GroupState(nil), s.prefs.Tree...), AI: ai}
}

func displayPath(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

func decodeDataURL(value string) ([]byte, error) {
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return nil, errors.New("invalid data URL")
	}
	header, payload := value[:comma], value[comma+1:]
	if strings.Contains(header, ";base64") {
		return base64.StdEncoding.DecodeString(payload)
	}
	decoded, err := url.PathUnescape(payload)
	if err != nil {
		return nil, err
	}
	return []byte(decoded), nil
}
