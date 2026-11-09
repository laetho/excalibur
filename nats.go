package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type natsClient struct {
	config NatsServerConfig
	conn   *nats.Conn
	js     nats.JetStreamContext
	jsm    jetstream.JetStream
}

func connectNATS(config NatsServerConfig) (*natsClient, error) {
	var err error
	config, err = normalizeNatsServerConfig(config)
	if err != nil {
		return nil, err
	}
	options := []nats.Option{nats.Name("Excalibur"), nats.Timeout(5 * time.Second)}
	if config.CredentialsFile != "" {
		options = append(options, nats.UserCredentials(config.CredentialsFile))
	} else if config.Username != "" || config.Password != "" {
		options = append(options, nats.UserInfo(config.Username, config.Password))
	}
	nc, err := nats.Connect(config.URL, options...)
	if err != nil {
		return nil, fmt.Errorf("could not connect to %q (%s): %w", config.Name, config.URL, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("JetStream is unavailable on %q: %w", config.Name, err)
	}
	jsm, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("could not initialise JetStream on %q: %w", config.Name, err)
	}
	return &natsClient{config: config, conn: nc, js: js, jsm: jsm}, nil
}

// normalizeNatsServerConfig validates authentication before a connection is
// attempted. A credentials file is the complete NATS authentication method, so
// username/password values are discarded rather than combining two methods.
func normalizeNatsServerConfig(config NatsServerConfig) (NatsServerConfig, error) {
	config.ID = strings.TrimSpace(config.ID)
	config.Name = strings.TrimSpace(config.Name)
	config.URL = strings.TrimSpace(config.URL)
	config.Username = strings.TrimSpace(config.Username)
	config.CredentialsFile = strings.TrimSpace(config.CredentialsFile)
	if config.ID == "" || config.Name == "" || config.URL == "" {
		return config, fmt.Errorf("server name and URL are required")
	}
	if config.CredentialsFile == "" {
		return config, nil
	}
	path, err := filepath.Abs(config.CredentialsFile)
	if err != nil {
		return config, fmt.Errorf("could not resolve NATS credentials file %q: %w", config.CredentialsFile, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return config, fmt.Errorf("could not access NATS credentials file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return config, fmt.Errorf("NATS credentials path %q is not a file", path)
	}
	config.CredentialsFile = path
	config.Username = ""
	config.Password = ""
	return config, nil
}

func (c *natsClient) close() { c.conn.Close() }

func (c *natsClient) objectStore(bucket string) (nats.ObjectStore, error) {
	store, err := c.js.ObjectStore(bucket)
	if err == nil {
		return store, nil
	}
	store, err = c.js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket})
	if err != nil {
		return nil, fmt.Errorf("could not create bucket %q on %s: %w", bucket, c.config.Name, err)
	}
	return store, nil
}

// listBuckets returns the object-store bucket names on the server. It uses the
// jetstream package's lister, which trims the internal "OBJ_" stream prefix and
// surfaces any error encountered while paging through the stores.
func (c *natsClient) listBuckets() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var names []string
	lister := c.jsm.ObjectStoreNames(ctx)
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Error(); err != nil {
		return nil, fmt.Errorf("could not list buckets on %q: %w", c.config.Name, err)
	}
	sort.Strings(names)
	return names, nil
}

func (c *natsClient) listObjects(bucket string) ([]*nats.ObjectInfo, error) {
	store, err := c.objectStore(bucket)
	if err != nil {
		return nil, err
	}
	objects, err := store.List()
	if err != nil && err != nats.ErrNoObjectsFound {
		return nil, err
	}
	return objects, nil
}

func (c *natsClient) readObject(bucket, name string) ([]byte, error) {
	store, err := c.objectStore(bucket)
	if err != nil {
		return nil, err
	}
	data, err := store.GetBytes(name)
	if err == nats.ErrObjectNotFound {
		return nil, nil
	}
	return data, err
}

func (c *natsClient) writeObject(bucket, name string, data []byte) error {
	store, err := c.objectStore(bucket)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(name, data)
	return err
}

// writeObjectMerged writes a scene to the object store, first merging it with
// the current contents by element id so concurrent edits to different elements
// are not lost to a last-writer-wins overwrite. If the object does not exist
// yet (or cannot be read), the data is written as-is.
func (c *natsClient) writeObjectMerged(bucket, name string, data []byte) error {
	store, err := c.objectStore(bucket)
	if err != nil {
		return err
	}
	current, err := store.GetBytes(name)
	if err == nats.ErrObjectNotFound || len(current) == 0 {
		_, err = store.PutBytes(name, data)
		return err
	}
	if err != nil {
		return err
	}
	merged, err := mergeScenes(current, data)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(name, merged)
	return err
}

// elementID extracts the "id" field from a serialized Excalidraw element.
func elementID(raw json.RawMessage) string {
	var element struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &element); err != nil {
		return ""
	}
	return element.ID
}

// mergeScenes unions two serialized Excalidraw scenes. The newer scene wins for
// overlapping element ids and for all other top-level fields; elements that only
// exist in the current scene are kept in their original relative order, and
// files are unioned by key. If either scene cannot be parsed, the newer scene is
// returned unchanged (a plain write).
func mergeScenes(current, newer []byte) ([]byte, error) {
	var currentScene, newerScene map[string]json.RawMessage
	if err := json.Unmarshal(current, &currentScene); err != nil {
		return newer, nil
	}
	if err := json.Unmarshal(newer, &newerScene); err != nil {
		return newer, nil
	}
	merged := make(map[string]json.RawMessage, len(newerScene))
	for key, value := range newerScene {
		merged[key] = value
	}
	var currentElements, newerElements []json.RawMessage
	if err := json.Unmarshal(currentScene["elements"], &currentElements); err == nil &&
		json.Unmarshal(newerScene["elements"], &newerElements) == nil {
		merged["elements"] = unionElements(currentElements, newerElements)
	}
	var currentFiles, newerFiles map[string]json.RawMessage
	if err := json.Unmarshal(currentScene["files"], &currentFiles); err == nil &&
		json.Unmarshal(newerScene["files"], &newerFiles) == nil {
		for key, value := range currentFiles {
			if _, present := newerFiles[key]; !present {
				newerFiles[key] = value
			}
		}
		if files, err := json.Marshal(newerFiles); err == nil {
			merged["files"] = files
		}
	}
	return json.Marshal(merged)
}

// unionElements merges two element lists by id: newer wins collisions,
// current-only elements are kept in their original relative order, and
// newer-only elements are appended.
func unionElements(current, newer []json.RawMessage) []byte {
	newerByID := make(map[string]json.RawMessage, len(newer))
	for _, element := range newer {
		newerByID[elementID(element)] = element
	}
	seen := make(map[string]bool, len(current)+len(newer))
	merged := make([]json.RawMessage, 0, len(current)+len(newer))
	for _, element := range current {
		id := elementID(element)
		seen[id] = true
		if replacement, present := newerByID[id]; present {
			merged = append(merged, replacement)
		} else {
			merged = append(merged, element)
		}
	}
	for _, element := range newer {
		id := elementID(element)
		if !seen[id] {
			seen[id] = true
			merged = append(merged, element)
		}
	}
	// Marshaling a []json.RawMessage of valid JSON cannot fail; fall back to the
	// newer list on the off chance it does.
	if result, err := json.Marshal(merged); err == nil {
		return result
	}
	result, _ := json.Marshal(newer)
	return result
}

func (c *natsClient) deleteObject(bucket, name string) error {
	store, err := c.objectStore(bucket)
	if err != nil {
		return err
	}
	return store.Delete(name)
}

var subjectUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func collabSubject(bucket, board string) string {
	return "wb.collab." + subjectUnsafe.ReplaceAllString(bucket, "-") + "." + subjectUnsafe.ReplaceAllString(board, "-")
}

func bucketNameProblem(name string) string {
	switch {
	case name == "":
		return "Enter a bucket name."
	case len(name) > 64:
		return "That bucket name is too long."
	case strings.ContainsAny(name, ". ") || subjectUnsafe.MatchString(name):
		return "Bucket names can only contain letters, digits, ‘-’ and ‘_’."
	default:
		return ""
	}
}
