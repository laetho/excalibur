package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func (c *natsClient) deleteObject(bucket, name string) error {
	store, err := c.objectStore(bucket)
	if err != nil {
		return err
	}
	return store.Delete(name)
}

// Collaboration leader lease, stored as a JetStream KV entry per board. The KV
// compare-and-swap (expected revision) makes acquisition atomic: exactly one
// client can hold the lease, so only that client writes the board's bucket
// object (single-writer persistence). The lease expires after a TTL, so a
// crashed or suspended client is replaced by a follower.

const lockBucket = "excalibur-locks"

type lockValue struct {
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expiresAt"` // unix milliseconds
}

func lockKey(bucket, boardID string) string { return bucket + "/" + boardID }

func (c *natsClient) lockKV() (nats.KeyValue, error) {
	kv, err := c.js.KeyValue(lockBucket)
	if err == nil {
		return kv, nil
	}
	kv, err = c.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: lockBucket})
	if err != nil {
		return nil, fmt.Errorf("could not create lock bucket %q on %s: %w", lockBucket, c.config.Name, err)
	}
	return kv, nil
}

// acquireLock tries to become the leader for a board. It succeeds if the lock
// is free (or the current holder's lease has expired) and this client's CAS
// wins. Returns whether this client now owns the lease.
func (c *natsClient) acquireLock(bucket, boardID, owner string, ttl time.Duration) (bool, error) {
	kv, err := c.lockKV()
	if err != nil {
		return false, err
	}
	key := lockKey(bucket, boardID)
	value, err := json.Marshal(lockValue{Owner: owner, ExpiresAt: time.Now().Add(ttl).UnixMilli()})
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err = kv.Create(key, value)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return false, err
		}
		entry, err := kv.Get(key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue // released between Create and Get; retry Create
		}
		if err != nil {
			return false, err
		}
		var current lockValue
		if err := json.Unmarshal(entry.Value(), &current); err != nil {
			return false, nil
		}
		if current.ExpiresAt > time.Now().UnixMilli() {
			return false, nil // still held
		}
		_, err = kv.Update(key, value, entry.Revision())
		if err == nil {
			return true, nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			return false, nil // lost the CAS race to another client
		}
		return false, err
	}
	return false, nil
}

// renewLock extends the lease if this client still owns it. Returns false when
// the lease was lost (another client owns it, or it was released).
func (c *natsClient) renewLock(bucket, boardID, owner string, ttl time.Duration) (bool, error) {
	kv, err := c.lockKV()
	if err != nil {
		return false, err
	}
	key := lockKey(bucket, boardID)
	entry, err := kv.Get(key)
	if err != nil {
		return false, err
	}
	var current lockValue
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return false, nil
	}
	if current.Owner != owner {
		return false, nil
	}
	value, err := json.Marshal(lockValue{Owner: owner, ExpiresAt: time.Now().Add(ttl).UnixMilli()})
	if err != nil {
		return false, err
	}
	_, err = kv.Update(key, value, entry.Revision())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, nats.ErrKeyExists) {
		return false, nil
	}
	return false, err
}

// releaseLock removes the lease, but only if this client still owns it (so a
// stale releaser cannot clobber a lease that was renewed or taken over).
func (c *natsClient) releaseLock(bucket, boardID, owner string) error {
	kv, err := c.lockKV()
	if err != nil {
		return err
	}
	key := lockKey(bucket, boardID)
	entry, err := kv.Get(key)
	if err != nil {
		return err
	}
	var current lockValue
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return nil
	}
	if current.Owner != owner {
		return nil
	}
	return kv.Delete(key, nats.LastRevision(entry.Revision()))
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

const maxCredentialsFileSize = 64 << 10

// importCredentialsFile copies a NATS .creds file into dir (mode 0600) and
// returns the copy's path. The name includes a content hash, so importing the
// same file twice reuses one copy and a changed file gets a new one.
func importCredentialsFile(src, dir string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("could not access NATS credentials file %q: %w", src, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCredentialsFileSize {
		return "", fmt.Errorf("%q is not a valid NATS credentials file", src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("could not read NATS credentials file %q: %w", src, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	dst := filepath.Join(dir, hex.EncodeToString(sum[:4])+"-"+filepath.Base(src))
	if err := atomicWrite(dst, data); err != nil {
		return "", fmt.Errorf("could not store NATS credentials: %w", err)
	}
	return dst, nil
}
