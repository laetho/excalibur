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
	"sort"
	"strings"
	"time"
)

type Store struct {
	prefsPath    string
	prefs        Preferences
	localFolder  string
	activeSource string
	activeGroup  string
	boards       []Board
	selection    string
	details      map[string]BoardDetails
	known        map[string][]byte
	knownStamp   map[string]string
	alerts       []Notice
	natsClients  map[string]*natsClient
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

func newStore(prefsPath, defaultFolder string) (*Store, error) {
	show := true
	s := &Store{
		prefsPath: prefsPath,
		prefs:     Preferences{Viewports: map[string]Viewport{}, SortOrder: "name", CanvasAppearance: "system", ShowThumbnails: &show},
		details:   map[string]BoardDetails{}, known: map[string][]byte{}, knownStamp: map[string]string{}, natsClients: map[string]*natsClient{},
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
	if s.prefs.BoardsFolder == "" {
		s.prefs.BoardsFolder = defaultFolder
	}
	s.localFolder = s.prefs.BoardsFolder
	s.activeSource, s.activeGroup = localSourceID, s.localFolder
	if err := os.MkdirAll(s.localFolder, 0o755); err != nil {
		return nil, err
	}
	if strings.HasPrefix(s.prefs.ActiveSource, "nats:") && s.prefs.ActiveGroup != "" {
		s.activeSource, s.activeGroup = s.prefs.ActiveSource, s.prefs.ActiveGroup
	}
	if err := s.scan(); err != nil {
		s.activeSource, s.activeGroup = localSourceID, s.localFolder
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
	for _, client := range s.natsClients {
		client.close()
	}
}

func (s *Store) isLocal() bool { return s.activeSource == localSourceID }

func (s *Store) clientFor(sourceID string) (*natsClient, error) {
	id := strings.TrimPrefix(sourceID, "nats:")
	if c := s.natsClients[id]; c != nil {
		return c, nil
	}
	for _, config := range s.prefs.NatsServers {
		if config.ID == id {
			c, err := connectNATS(config)
			if err != nil {
				return nil, err
			}
			s.natsClients[id] = c
			return c, nil
		}
	}
	return nil, fmt.Errorf("unknown NATS server %q", id)
}

func (s *Store) scan() error {
	var boards []Board
	if s.isLocal() {
		entries, err := os.ReadDir(s.activeGroup)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), fileExtension) {
				continue
			}
			path := filepath.Join(s.activeGroup, name)
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
	} else {
		client, err := s.clientFor(s.activeSource)
		if err != nil {
			return err
		}
		objects, err := client.listObjects(s.activeGroup)
		if err != nil {
			return err
		}
		for _, object := range objects {
			if object == nil || object.Deleted || strings.HasPrefix(object.Name, ".") || !strings.HasSuffix(object.Name, fileExtension) {
				continue
			}
			boards = append(boards, Board{ID: object.Name, Name: strings.TrimSuffix(object.Name, fileExtension), Modified: object.ModTime, Size: object.Size})
		}
	}
	sort.Slice(boards, func(i, j int) bool { return strings.ToLower(boards[i].Name) < strings.ToLower(boards[j].Name) })
	s.boards = boards
	if s.selection != "" && s.board(s.selection) == nil {
		s.selection = ""
	}
	return nil
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
	if s.isLocal() {
		path := board.Path
		if board.Target != "" {
			path = board.Target
		}
		return os.ReadFile(path)
	}
	client, err := s.clientFor(s.activeSource)
	if err != nil {
		return nil, err
	}
	return client.readObject(s.activeGroup, board.ID)
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
	return client.writeObjectMerged(s.activeGroup, id, data)
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
		if err = client.deleteObject(s.activeGroup, id); err != nil {
			return err
		}
	}
	if detail, ok := s.details[id]; ok {
		delete(s.details, id)
		s.details[newID] = detail
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
		if err = client.deleteObject(s.activeGroup, id); err != nil {
			return err
		}
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
		data, err = client.readObject(s.activeGroup, libraryName)
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
	return client.writeObject(s.activeGroup, libraryName, []byte(text))
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
		boards = append(boards, BoardInfo{ID: board.ID, Name: board.Name, Modified: board.Modified.Unix(), IsLink: board.IsLink, Target: target, Texts: texts, Thumbnail: thumbnail})
	}
	var selection *string
	if s.selection != "" {
		value := s.selection
		selection = &value
	}
	sources := []SourceInfo{{ID: localSourceID, Name: "Local", Kind: "local", Groups: []GroupInfo{{ID: s.localFolder, Name: displayPath(s.localFolder), Kind: "folder"}}}}
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
	return StoreSnapshot{Folder: folder, Boards: boards, Selection: selection, SortOrder: s.prefs.SortOrder, ShowThumbnails: *s.prefs.ShowThumbnails, CanvasAppearance: s.prefs.CanvasAppearance, ApplicationZoom: s.prefs.ApplicationZoom, CollabNickname: s.prefs.CollabNickname, Alerts: append([]Notice(nil), s.alerts...), IsAlertPresented: len(s.alerts) > 0, Sources: sources, NatsServers: natsServers, ActiveSource: s.activeSource, ActiveGroup: s.activeGroup, AI: ai}
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
