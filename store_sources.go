package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (s *Store) importBoardData(fileName string, data []byte) error {
	base := filepath.Base(fileName)
	name := strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
	if name == "" {
		name = "Imported Board"
	}
	name = s.uniqueName(name)
	if err := s.writeBoard(name+fileExtension, data, true); err != nil {
		return err
	}
	if err := s.scan(); err != nil {
		return err
	}
	s.setSelection(name + fileExtension)
	return nil
}

func (s *Store) importPaths(paths []string) error {
	for _, path := range paths {
		if !strings.EqualFold(filepath.Ext(path), fileExtension) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err = s.importBoardData(filepath.Base(path), data); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addNatsServer(config NatsServerConfig) error {
	var err error
	config, err = normalizeNatsServerConfig(config)
	if err != nil {
		return err
	}
	for _, existing := range s.prefs.NatsServers {
		if existing.ID == config.ID {
			return errors.New("a server with that id already exists")
		}
	}
	client, err := connectNATS(config)
	if err != nil {
		return err
	}
	buckets, err := client.listBuckets()
	if err != nil {
		client.close()
		return err
	}
	config.Buckets = buckets
	s.natsClients[config.ID] = client
	s.prefs.NatsServers = append(s.prefs.NatsServers, config)
	s.savePrefs()
	return nil
}

// updateNatsServer verifies the replacement connection before changing the
// saved configuration. An empty password can retain the native-side secret so
// editing a name or URL never sends the old password to the webview.
func (s *Store) updateNatsServer(config NatsServerConfig, keepPassword bool) error {
	index := -1
	for i := range s.prefs.NatsServers {
		if s.prefs.NatsServers[i].ID == strings.TrimSpace(config.ID) {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("NATS server not found")
	}

	existing := s.prefs.NatsServers[index]
	if keepPassword && config.Password == "" {
		config.Password = existing.Password
	}
	client, err := connectNATS(config)
	if err != nil {
		return err
	}
	buckets, err := client.listBuckets()
	if err != nil {
		client.close()
		return err
	}
	config = client.config
	config.Buckets = buckets

	oldClient := s.natsClients[config.ID]
	oldSource, oldGroup, oldSelection := s.activeSource, s.activeGroup, s.selection
	oldBoards := s.boards
	s.prefs.NatsServers[index] = config
	s.natsClients[config.ID] = client
	if s.activeSource == "nats:"+config.ID {
		if !slices.Contains(buckets, s.activeGroup) {
			if len(buckets) > 0 {
				s.activeGroup = buckets[0]
			} else {
				s.activeSource, s.activeGroup = localSourceID, s.localFolder
			}
			s.selection = ""
		}
		if err = s.scan(); err != nil {
			s.prefs.NatsServers[index] = existing
			s.activeSource, s.activeGroup, s.selection, s.boards = oldSource, oldGroup, oldSelection, oldBoards
			if oldClient != nil {
				s.natsClients[config.ID] = oldClient
			} else {
				delete(s.natsClients, config.ID)
			}
			client.close()
			return err
		}
		s.prefs.ActiveSource, s.prefs.ActiveGroup, s.prefs.LastBoard = s.activeSource, s.activeGroup, s.selection
	}
	if oldClient != nil {
		oldClient.close()
	}
	s.savePrefs()
	return nil
}

func (s *Store) removeNatsServer(id string) error {
	index := -1
	for i := range s.prefs.NatsServers {
		if s.prefs.NatsServers[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	if client := s.natsClients[id]; client != nil {
		client.close()
		delete(s.natsClients, id)
	}
	s.prefs.NatsServers = append(s.prefs.NatsServers[:index], s.prefs.NatsServers[index+1:]...)
	if s.activeSource == "nats:"+id {
		s.activeSource, s.activeGroup, s.selection = localSourceID, s.localFolder, ""
		s.prefs.ActiveSource, s.prefs.ActiveGroup, s.prefs.LastBoard = localSourceID, s.localFolder, ""
		if err := s.scan(); err != nil {
			return err
		}
	}
	s.savePrefs()
	return nil
}

func (s *Store) addBucket(sourceID, bucket string) error {
	bucket = strings.TrimSpace(bucket)
	if problem := bucketNameProblem(bucket); problem != "" {
		return errors.New(problem)
	}
	client, err := s.clientFor(sourceID)
	if err != nil {
		return err
	}
	if _, err = client.objectStore(bucket); err != nil {
		return err
	}
	id := strings.TrimPrefix(sourceID, "nats:")
	for i := range s.prefs.NatsServers {
		server := &s.prefs.NatsServers[i]
		if server.ID != id {
			continue
		}
		for _, existing := range server.Buckets {
			if existing == bucket {
				return nil
			}
		}
		server.Buckets = append(server.Buckets, bucket)
		s.savePrefs()
		return nil
	}
	return fmt.Errorf("unknown NATS source %q", sourceID)
}

// refreshBuckets re-lists the object stores on every configured NATS server and
// updates the saved bucket list. When the active source is a server whose
// active bucket disappeared, it falls back to the first remaining bucket (or to
// the local folder when none are left) and clears the selection. It never scans
// — callers scan afterwards so the active group is validated against the new
// list. Errors are collected per server and joined.
func (s *Store) refreshBuckets() error {
	var errs []error
	changed := false
	for i := range s.prefs.NatsServers {
		server := &s.prefs.NatsServers[i]
		client, err := s.clientFor("nats:" + server.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", server.Name, err))
			continue
		}
		buckets, err := client.listBuckets()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", server.Name, err))
			continue
		}
		if !slices.Equal(buckets, server.Buckets) {
			server.Buckets = buckets
			changed = true
		}
		if s.activeSource == "nats:"+server.ID && !slices.Contains(buckets, s.activeGroup) {
			if len(buckets) > 0 {
				s.activeGroup = buckets[0]
			} else {
				s.activeSource, s.activeGroup = localSourceID, s.localFolder
			}
			s.selection = ""
			s.prefs.ActiveSource, s.prefs.ActiveGroup, s.prefs.LastBoard = s.activeSource, s.activeGroup, s.selection
			changed = true
		}
	}
	if changed {
		s.savePrefs()
	}
	return errors.Join(errs...)
}

func (s *Store) activateSource(sourceID, groupID string) error {
	if sourceID == localSourceID {
		if groupID == "" {
			groupID = s.localFolder
		}
		if groupID != s.localFolder {
			return errors.New("unknown local folder")
		}
	} else {
		id := strings.TrimPrefix(sourceID, "nats:")
		found := false
		for _, server := range s.prefs.NatsServers {
			if server.ID != id {
				continue
			}
			for _, bucket := range server.Buckets {
				if bucket == groupID {
					found = true
					break
				}
			}
		}
		if !found {
			return errors.New("unknown NATS bucket")
		}
	}
	s.activeSource, s.activeGroup, s.selection = sourceID, groupID, ""
	s.prefs.ActiveSource, s.prefs.ActiveGroup, s.prefs.LastBoard = sourceID, groupID, ""
	if err := s.scan(); err != nil {
		return err
	}
	s.savePrefs()
	return nil
}

func (s *Store) copyBoardToSource(boardID, targetSource, targetGroup string) error {
	board := s.board(boardID)
	if board == nil {
		return errors.New("board not found")
	}
	data, err := s.readBoard(*board)
	if err != nil {
		return err
	}
	name := board.Name
	if targetSource == localSourceID {
		path := filepath.Join(targetGroup, name+fileExtension)
		for n := 2; ; n++ {
			if _, e := os.Lstat(path); os.IsNotExist(e) {
				break
			}
			path = filepath.Join(targetGroup, fmt.Sprintf("%s %d%s", name, n, fileExtension))
		}
		if err = atomicWrite(path, data); err != nil {
			return err
		}
	} else {
		client, e := s.clientFor(targetSource)
		if e != nil {
			return e
		}
		store, e := client.objectStore(targetGroup)
		if e != nil {
			return e
		}
		candidate := name + fileExtension
		for n := 2; ; n++ {
			if _, e = store.GetInfo(candidate); e != nil {
				break
			}
			candidate = fmt.Sprintf("%s %d%s", name, n, fileExtension)
		}
		if _, err = store.PutBytes(candidate, data); err != nil {
			return err
		}
	}
	return s.activateSource(targetSource, targetGroup)
}

func (s *Store) changeFolder(folder string) error {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	s.localFolder, s.prefs.BoardsFolder = folder, folder
	s.activeSource, s.activeGroup, s.selection = localSourceID, folder, ""
	s.prefs.ActiveSource, s.prefs.ActiveGroup, s.prefs.LastBoard = localSourceID, folder, ""
	s.savePrefs()
	return s.scan()
}
