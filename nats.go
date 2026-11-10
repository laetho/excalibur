package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type natsClient struct {
	config   NatsServerConfig
	conn     *nats.Conn
	js       nats.JetStreamContext
	jsm      jetstream.JetStream
	streamMu sync.RWMutex
	streams  map[string]struct{}
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
	return &natsClient{config: config, conn: nc, js: js, jsm: jsm, streams: map[string]struct{}{}}, nil
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

// deleteBucket removes an Object Store only when it has no visible boards. The
// Object Store may still contain hidden CRDT metadata, snapshots, or blobs from
// boards that were previously deleted; those are part of the bucket and are
// intentionally removed with it. The per-bucket collaboration stream is removed
// as well so recreating a bucket with the same name starts with no retained log.
func (c *natsClient) deleteBucket(bucket string) error {
	store, err := c.js.ObjectStore(bucket)
	if err != nil {
		return fmt.Errorf("could not open bucket %q: %w", bucket, err)
	}
	objects, err := store.List()
	if err != nil && err != nats.ErrNoObjectsFound {
		return fmt.Errorf("could not inspect bucket %q: %w", bucket, err)
	}
	for _, object := range objects {
		if object != nil && !object.Deleted && !strings.HasPrefix(object.Name, ".") && strings.HasSuffix(object.Name, fileExtension) {
			return fmt.Errorf("bucket %q still contains boards", bucket)
		}
	}
	if err = c.js.DeleteObjectStore(bucket); err != nil {
		return fmt.Errorf("could not delete bucket %q: %w", bucket, err)
	}
	// The bucket deletion is the primary operation and cannot be rolled back. A
	// denied/failed best-effort history cleanup must not leave the now-deleted
	// bucket in application navigation.
	if err = c.js.DeleteStream(collabStreamName(bucket)); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		log.Printf("could not delete collaboration history for removed bucket %q: %v", bucket, err)
	}
	c.streamMu.Lock()
	delete(c.streams, collabStreamName(bucket))
	c.streamMu.Unlock()
	return nil
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

// A board's filename changes on rename, while its collaboration log must not.
// Store a stable document id in a hidden sidecar object. Legacy boards get a
// deterministic id on first scan; copies receive a fresh random id.
func documentMetaName(boardID string) string {
	sum := sha256.Sum256([]byte(boardID))
	return fmt.Sprintf(".crdt-document.%x", sum[:16])
}

func legacyDocumentID(bucket, boardID string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + boardID))
	return hex.EncodeToString(sum[:16])
}

func newDocumentID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func validDocumentID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (c *natsClient) documentID(bucket, boardID string) (string, error) {
	name := documentMetaName(boardID)
	data, err := c.readObject(bucket, name)
	if err != nil {
		return "", err
	}
	if id := strings.TrimSpace(string(data)); validDocumentID(id) {
		return id, nil
	}
	id := legacyDocumentID(bucket, boardID)
	if err := c.writeObject(bucket, name, []byte(id)); err != nil {
		return "", err
	}
	return id, nil
}

func (c *natsClient) setDocumentID(bucket, boardID, documentID string) error {
	if !validDocumentID(documentID) {
		return errors.New("invalid collaboration document id")
	}
	return c.writeObject(bucket, documentMetaName(boardID), []byte(documentID))
}

func collabBucketToken(bucket string) string {
	sum := sha256.Sum256([]byte(bucket))
	return hex.EncodeToString(sum[:8])
}

func collabStreamName(bucket string) string {
	return "EXCALIBUR_CRDT_" + strings.ToUpper(collabBucketToken(bucket))
}

func collabUpdateSubject(bucket, documentID string) string {
	return "wb.crdt.v2." + collabBucketToken(bucket) + "." + documentID + ".updates"
}

func collabPresenceSubject(bucket, documentID string) string {
	return "wb.crdt.v2." + collabBucketToken(bucket) + "." + documentID + ".presence"
}

func (c *natsClient) invalidateCollabStream(bucket string) {
	c.streamMu.Lock()
	delete(c.streams, collabStreamName(bucket))
	c.streamMu.Unlock()
}

func (c *natsClient) ensureCollabStream(bucket string) (string, error) {
	name := collabStreamName(bucket)
	c.streamMu.RLock()
	_, ready := c.streams[name]
	c.streamMu.RUnlock()
	if ready {
		return name, nil
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if _, ready = c.streams[name]; ready {
		return name, nil
	}
	if info, err := c.js.StreamInfo(name); err == nil {
		// Updates must not expire independently of per-document snapshot progress.
		// Older builds configured time/size limits which could silently discard the
		// only durable copy of an edit, so remove those limits when encountered.
		if info.Config.MaxAge != 0 || info.Config.MaxBytes != -1 {
			config := info.Config
			config.MaxAge = 0
			config.MaxBytes = -1
			if _, updateErr := c.js.UpdateStream(&config); updateErr != nil {
				return "", fmt.Errorf("could not make collaboration stream %q lossless: %w", name, updateErr)
			}
		}
		c.streams[name] = struct{}{}
		return name, nil
	} else if !errors.Is(err, nats.ErrStreamNotFound) {
		return "", err
	}
	_, err := c.js.AddStream(&nats.StreamConfig{
		Name:        name,
		Description: "Excalibur durable collaboration updates for " + bucket,
		Subjects:    []string{"wb.crdt.v2." + collabBucketToken(bucket) + ".*.updates"},
		Retention:   nats.LimitsPolicy,
		Storage:     nats.FileStorage,
		Discard:     nats.DiscardOld,
		MaxAge:      0,
		MaxBytes:    -1,
		Duplicates:  10 * time.Minute,
	})
	if err != nil {
		// Another client may have created the stream after our StreamInfo call.
		if _, infoErr := c.js.StreamInfo(name); infoErr == nil {
			c.streams[name] = struct{}{}
			return name, nil
		}
		return "", fmt.Errorf("could not create collaboration stream %q: %w", name, err)
	}
	c.streams[name] = struct{}{}
	return name, nil
}

func (c *natsClient) publishCollabUpdate(bucket, documentID, messageID string, data []byte) (uint64, error) {
	if !validDocumentID(documentID) {
		return 0, errors.New("invalid collaboration document id")
	}
	if _, err := c.ensureCollabStream(bucket); err != nil {
		return 0, err
	}
	msg := nats.NewMsg(collabUpdateSubject(bucket, documentID))
	msg.Data = data
	if messageID != "" {
		msg.Header.Set(nats.MsgIdHdr, messageID)
	}
	ack, err := c.js.PublishMsg(msg)
	if err != nil {
		// The stream may have been deleted or reconfigured outside this process.
		// Revalidate on the next retry rather than pinning a stale cache entry.
		c.invalidateCollabStream(bucket)
		return 0, err
	}
	return ack.Sequence, nil
}

// purgeCollabUpdates removes update-log entries covered by an older immutable
// snapshot. Callers deliberately keep the newest snapshot interval so a join
// that raced snapshot discovery can still replay from the previous snapshot.
func (c *natsClient) purgeCollabUpdates(bucket, documentID string, through uint64) error {
	if through == 0 || through == ^uint64(0) {
		return nil
	}
	stream, err := c.ensureCollabStream(bucket)
	if err != nil {
		return err
	}
	return c.js.PurgeStream(stream, &nats.StreamPurgeRequest{
		Sequence: through + 1,
		Subject:  collabUpdateSubject(bucket, documentID),
	})
}

func collabSnapshotName(documentID string, sequence uint64, data []byte) string {
	sum := sha256.Sum256(data)
	// Content-address the suffix so snapshots are immutable even if a stale
	// compactor races at the same stream sequence.
	return fmt.Sprintf(".crdt.%s.snapshot.%020d.%x", documentID, sequence, sum[:8])
}

type collabSnapshotIndex struct {
	Name     string `json:"name"`
	Sequence uint64 `json:"sequence"`
}

func collabSnapshotIndexKey(bucket, documentID string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + documentID))
	return fmt.Sprintf("snapshot.%x", sum[:16])
}

// updateCollabSnapshotIndex advances a CAS-protected max-sequence pointer. A
// stale compactor can finish late, but it can never regress this index.
func (c *natsClient) updateCollabSnapshotIndex(bucket, documentID string, index collabSnapshotIndex) error {
	kv, err := c.lockKV()
	if err != nil {
		return err
	}
	key := collabSnapshotIndexKey(bucket, documentID)
	value, _ := json.Marshal(index)
	for attempt := 0; attempt < 4; attempt++ {
		entry, err := kv.Get(key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			if _, err = kv.Create(key, value); err == nil {
				return nil
			}
			if errors.Is(err, nats.ErrKeyExists) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		var current collabSnapshotIndex
		if json.Unmarshal(entry.Value(), &current) == nil &&
			(current.Sequence > index.Sequence || (current.Sequence == index.Sequence && current.Name >= index.Name)) {
			return nil
		}
		if _, err = kv.Update(key, value, entry.Revision()); err == nil {
			return nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return err
		}
	}
	return errors.New("collaboration snapshot index CAS contention")
}

func (c *natsClient) readCollabSnapshotIndex(bucket, documentID string) (collabSnapshotIndex, error) {
	kv, err := c.lockKV()
	if err != nil {
		return collabSnapshotIndex{}, err
	}
	entry, err := kv.Get(collabSnapshotIndexKey(bucket, documentID))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return collabSnapshotIndex{}, nil
	}
	if err != nil {
		return collabSnapshotIndex{}, err
	}
	var index collabSnapshotIndex
	if err := json.Unmarshal(entry.Value(), &index); err != nil {
		return collabSnapshotIndex{}, err
	}
	return index, nil
}

func collabFileName(documentID, fileID string) string {
	sum := sha256.Sum256([]byte(fileID))
	return fmt.Sprintf(".crdt.%s.file.%x", documentID, sum[:16])
}

func collabDeletedName(documentID string) string {
	return ".crdt." + documentID + ".deleted"
}

func (c *natsClient) writeCollabFile(bucket, documentID, fileID string, data []byte) error {
	if !validDocumentID(documentID) || fileID == "" {
		return errors.New("invalid collaboration file")
	}
	name := collabFileName(documentID, fileID)
	existing, err := c.readObject(bucket, name)
	if err != nil {
		return err
	}
	if existing != nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("collaboration blob key already contains different data")
	}
	return c.writeObject(bucket, name, data)
}

func (c *natsClient) readCollabFile(bucket, documentID, fileID string) ([]byte, error) {
	if !validDocumentID(documentID) || fileID == "" {
		return nil, errors.New("invalid collaboration file")
	}
	return c.readObject(bucket, collabFileName(documentID, fileID))
}

// writeCollabSnapshotObject uploads an immutable candidate without making it
// discoverable. The caller must re-check its lease and all board deletion/
// identity fences before activating the index.
func (c *natsClient) writeCollabSnapshotObject(bucket, documentID string, sequence uint64, data []byte) (string, error) {
	if !validDocumentID(documentID) || sequence == 0 {
		return "", errors.New("invalid collaboration snapshot")
	}
	name := collabSnapshotName(documentID, sequence, data)
	if err := c.writeObject(bucket, name, data); err != nil {
		return "", err
	}
	return name, nil
}

// activateCollabSnapshot is the commit point. Callers must never delete this
// object after the CAS index can reference it.
func (c *natsClient) activateCollabSnapshot(bucket, documentID, name string, sequence uint64) error {
	if err := c.updateCollabSnapshotIndex(bucket, documentID, collabSnapshotIndex{Name: name, Sequence: sequence}); err != nil {
		return err
	}
	// Keep a small immutable rollback window rather than accumulating one object
	// per compaction forever. Durable updates after the newest snapshot remain in
	// JetStream and are replayed on join.
	objects, err := c.listObjects(bucket)
	if err != nil {
		return nil // the snapshot is durable; pruning is best-effort
	}
	prefix := ".crdt." + documentID + ".snapshot."
	names := make([]string, 0)
	for _, object := range objects {
		if object != nil && !object.Deleted && strings.HasPrefix(object.Name, prefix) {
			var objectSequence uint64
			if _, err := fmt.Sscanf(strings.TrimPrefix(object.Name, prefix), "%d", &objectSequence); err == nil {
				names = append(names, object.Name)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > 3 {
		for _, name := range names[3:] {
			_ = c.deleteObject(bucket, name)
		}
	}
	return nil
}

// writeCollabSnapshot is retained for internal callers and tests that do not
// participate in the frontend lease protocol. Production compaction uploads,
// re-fences, and activates explicitly in App.CollabSnapshot.
func (c *natsClient) writeCollabSnapshot(bucket, documentID string, sequence uint64, data []byte) error {
	name, err := c.writeCollabSnapshotObject(bucket, documentID, sequence, data)
	if err != nil {
		return err
	}
	return c.activateCollabSnapshot(bucket, documentID, name, sequence)
}

func (c *natsClient) latestCollabSnapshot(bucket, documentID string) ([]byte, uint64, error) {
	if !validDocumentID(documentID) {
		return nil, 0, errors.New("invalid collaboration document id")
	}
	prefix := ".crdt." + documentID + ".snapshot."
	// The CAS-protected pointer avoids scanning every object in a large board
	// bucket. Any missing/corrupt target falls back to immutable object discovery.
	if index, err := c.readCollabSnapshotIndex(bucket, documentID); err == nil && index.Sequence > 0 && strings.HasPrefix(index.Name, prefix) {
		var nameSequence uint64
		if _, parseErr := fmt.Sscanf(strings.TrimPrefix(index.Name, prefix), "%d", &nameSequence); parseErr == nil && nameSequence == index.Sequence {
			if data, readErr := c.readObject(bucket, index.Name); readErr == nil && data != nil {
				return data, index.Sequence, nil
			}
		}
	}
	objects, err := c.listObjects(bucket)
	if err != nil {
		return nil, 0, err
	}
	var latest uint64
	var latestName string
	for _, object := range objects {
		if object == nil || object.Deleted || !strings.HasPrefix(object.Name, prefix) {
			continue
		}
		var sequence uint64
		if _, err := fmt.Sscanf(strings.TrimPrefix(object.Name, prefix), "%d", &sequence); err == nil &&
			(sequence > latest || (sequence == latest && object.Name > latestName)) {
			latest, latestName = sequence, object.Name
		}
	}
	if latestName == "" {
		return nil, 0, nil
	}
	data, err := c.readObject(bucket, latestName)
	if err == nil && data != nil {
		_ = c.updateCollabSnapshotIndex(bucket, documentID, collabSnapshotIndex{Name: latestName, Sequence: latest})
	}
	return data, latest, err
}

// collabSnapshotAfter returns a snapshot only when it is newer than the
// renderer's already-loaded baseline. The normal indexed path is a single KV
// lookup and avoids retransferring a large unchanged snapshot on every board
// switch. Missing/corrupt indexes fall back to immutable snapshot discovery.
func (c *natsClient) collabSnapshotAfter(bucket, documentID string, after uint64) ([]byte, uint64, error) {
	if !validDocumentID(documentID) {
		return nil, 0, errors.New("invalid collaboration document id")
	}
	prefix := ".crdt." + documentID + ".snapshot."
	if index, err := c.readCollabSnapshotIndex(bucket, documentID); err == nil && index.Sequence > 0 && strings.HasPrefix(index.Name, prefix) {
		if index.Sequence <= after {
			return nil, after, nil
		}
		var nameSequence uint64
		if _, parseErr := fmt.Sscanf(strings.TrimPrefix(index.Name, prefix), "%d", &nameSequence); parseErr == nil && nameSequence == index.Sequence {
			if data, readErr := c.readObject(bucket, index.Name); readErr == nil && data != nil {
				return data, index.Sequence, nil
			}
		}
	}
	data, sequence, err := c.latestCollabSnapshot(bucket, documentID)
	if err != nil || sequence > after {
		return data, sequence, err
	}
	return nil, after, nil
}

// previousCollabSnapshotSequence returns the second-newest distinct snapshot
// sequence. Retaining one complete snapshot interval closes the discovery/purge
// race for clients joining while a newer snapshot is committed.
func (c *natsClient) previousCollabSnapshotSequence(bucket, documentID string) (uint64, error) {
	objects, err := c.listObjects(bucket)
	if err != nil {
		return 0, err
	}
	prefix := ".crdt." + documentID + ".snapshot."
	sequences := map[uint64]struct{}{}
	for _, object := range objects {
		if object == nil || object.Deleted || !strings.HasPrefix(object.Name, prefix) {
			continue
		}
		var sequence uint64
		if _, err := fmt.Sscanf(strings.TrimPrefix(object.Name, prefix), "%d", &sequence); err == nil {
			sequences[sequence] = struct{}{}
		}
	}
	ordered := make([]uint64, 0, len(sequences))
	for sequence := range sequences {
		ordered = append(ordered, sequence)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] > ordered[j] })
	if len(ordered) < 2 {
		return 0, nil
	}
	return ordered[1], nil
}

// Collaboration compaction lease, stored as a JetStream KV entry per document.
// Live updates are multi-writer and commutatively reconciled; CAS elects exactly
// one client to materialize immutable snapshots. A crashed compactor is replaced
// after the lease expires.

const lockBucket = "excalibur-locks"

type lockValue struct {
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expiresAt"` // unix milliseconds
}

// lockKey hashes the tuple instead of embedding bucket/board names directly.
// Board names may contain spaces, Unicode, or other characters rejected by the
// JetStream KV key grammar, while a SHA-256 hex digest is always valid.
func lockKey(bucket, boardID string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + boardID))
	return fmt.Sprintf("board.%x", sum)
}

func (c *natsClient) lockKV() (nats.KeyValue, error) {
	kv, err := c.js.KeyValue(lockBucket)
	if err == nil {
		return kv, nil
	}
	kv, err = c.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: lockBucket})
	if err != nil {
		// Another client may have created the shared KV stream between our lookup
		// and create. Resolve it once before surfacing a real permission/server error.
		if existing, lookupErr := c.js.KeyValue(lockBucket); lookupErr == nil {
			return existing, nil
		}
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
	for attempt := 0; attempt < 3; attempt++ {
		entry, err := kv.Get(key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			return false, nil
		}
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
			// The snapshot fence and periodic heartbeat can renew concurrently.
			// Re-read before declaring the lease lost: the competing CAS may have
			// been another renewal by this same owner.
			continue
		}
		return false, err
	}
	return false, nil
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
