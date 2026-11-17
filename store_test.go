package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestLockKeyIsValidAndDeterministic(t *testing.T) {
	bucket := "team bucket/日本語"
	board := "Roadmap 2026 #1.excalidraw"
	wantHash := sha256.Sum256([]byte(bucket + "\x00" + board))
	want := fmt.Sprintf("board.%x", wantHash)

	got := lockKey(bucket, board)
	if got != want {
		t.Fatalf("lockKey() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`).MatchString(got) {
		t.Fatalf("lockKey() produced an invalid NATS KV key: %q", got)
	}
}

func TestLockKeySeparatesBucketAndBoard(t *testing.T) {
	if lockKey("a", "b/c") == lockKey("a/b", "c") {
		t.Fatal("lockKey() collided for distinct bucket/board tuples")
	}
}

func TestLibraryWatchConsumerIsScopedToOneInstance(t *testing.T) {
	first := libraryWatchConsumerName("0123456789abcdef")
	second := libraryWatchConsumerName("fedcba9876543210")
	if first == second {
		t.Fatal("library watch consumer names must differ per app run")
	}
	if first != "library-watch-0123456789abcdef" {
		t.Fatalf("libraryWatchConsumerName() = %q", first)
	}
	// JetStream rejects consumer names outside this alphabet.
	if !regexp.MustCompile(`^[-_a-zA-Z0-9]+$`).MatchString(first) {
		t.Fatalf("library watch consumer name is invalid: %q", first)
	}
}

func TestNewInstanceIDIsUniquePerRun(t *testing.T) {
	seen := map[string]struct{}{}
	for range 100 {
		id := newInstanceID()
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
			t.Fatalf("newInstanceID() = %q, want 16 hex characters", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("newInstanceID() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestLibraryWatchSubjectMatchesObjectStoreMeta(t *testing.T) {
	if got, want := libraryWatchSubject(libraryBucket), "$O."+libraryBucket+".M.>"; got != want {
		t.Fatalf("libraryWatchSubject() = %q, want %q", got, want)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	store, err := newStore(filepath.Join(root, "config", "prefs.json"), filepath.Join(root, "boards"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.close)
	return store
}

func TestPreferencesPathMigratesLegacyName(t *testing.T) {
	config := t.TempDir()
	legacyPath := filepath.Join(config, "excaliwails", "prefs.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const prefs = `{"sortOrder":"date"}`
	if err := os.WriteFile(legacyPath, []byte(prefs), 0o600); err != nil {
		t.Fatal(err)
	}

	path := preferencesPath(config)
	if want := filepath.Join(config, "excalibur", "prefs.json"); path != want {
		t.Fatalf("preferencesPath() = %q, want %q", path, want)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != prefs {
		t.Fatalf("migrated preferences = %q, %v", data, err)
	}
}

func TestLocalBoardLifecycle(t *testing.T) {
	store := testStore(t)
	if err := store.createBoard("Plan"); err != nil {
		t.Fatal(err)
	}
	if store.selection != "Plan.excalidraw" {
		t.Fatalf("selection = %q", store.selection)
	}
	scene := `{"type":"excalidraw","elements":[{"text":"hello"}]}`
	if err := store.saveBoard("Plan", scene); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(store.localFolders[0], "Plan.excalidraw")); err != nil || string(got) != scene {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := store.renameBoard("Plan.excalidraw", "Roadmap"); err != nil {
		t.Fatal(err)
	}
	if store.board("Roadmap.excalidraw") == nil {
		t.Fatal("renamed board missing")
	}
	if err := store.duplicateBoard("Roadmap.excalidraw"); err != nil {
		t.Fatal(err)
	}
	if store.board("Roadmap copy.excalidraw") == nil {
		t.Fatal("duplicate missing")
	}
}

func TestMultipleLocalFolders(t *testing.T) {
	store := testStore(t)
	first := store.localFolders[0]
	if err := store.createBoard("First"); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "more boards")
	if err := store.addLocalFolder(second); err != nil {
		t.Fatal(err)
	}
	if store.activeSource != localSourceID || store.activeGroup != second || len(store.boards) != 0 {
		t.Fatalf("after add: active %s/%s with %d boards", store.activeSource, store.activeGroup, len(store.boards))
	}
	if err := store.addLocalFolder(second + string(filepath.Separator)); err != nil || len(store.localFolders) != 2 {
		t.Fatalf("re-adding a folder = %v, folders %v", err, store.localFolders)
	}
	if err := store.createBoard("Second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, "Second.excalidraw")); err != nil {
		t.Fatalf("board not written to the active folder: %v", err)
	}
	if err := store.activateSource(localSourceID, first); err != nil || store.board("First.excalidraw") == nil {
		t.Fatalf("switching back = %v, boards %v", err, store.boards)
	}
	if err := store.activateSource(localSourceID, filepath.Join(t.TempDir(), "unknown")); err == nil {
		t.Fatal("an unknown folder was activated")
	}
	snapshot := store.snapshot()
	if groups := snapshot.Sources[0].Groups; len(groups) != 2 || groups[1].ID != second {
		t.Fatalf("local groups = %#v", groups)
	}

	// The list survives a restart, along with the active folder; BoardsFolder stays the first one.
	if err := store.activateSource(localSourceID, second); err != nil {
		t.Fatal(err)
	}
	reopened, err := newStore(store.prefsPath, filepath.Join(t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reopened.localFolders, []string{first, second}) || reopened.activeGroup != second || reopened.prefs.BoardsFolder != first {
		t.Fatalf("reopened folders %v, active %s, boardsFolder %s", reopened.localFolders, reopened.activeGroup, reopened.prefs.BoardsFolder)
	}

	// Removing the active folder falls back to the first; the last one can't go.
	if err := store.removeLocalFolder(second); err != nil {
		t.Fatal(err)
	}
	if store.activeGroup != first || len(store.localFolders) != 1 {
		t.Fatalf("after remove: active %s, folders %v", store.activeGroup, store.localFolders)
	}
	if _, err := os.Stat(filepath.Join(second, "Second.excalidraw")); err != nil {
		t.Fatalf("removing a folder must not delete its boards: %v", err)
	}
	if err := store.removeLocalFolder(first); err == nil {
		t.Fatal("the last folder was removed")
	}
}

func TestLegacyBoardsFolderMigrates(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	prefsPath := filepath.Join(root, "config", "prefs.json")
	if err := os.MkdirAll(filepath.Dir(prefsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefsPath, []byte(`{"boardsFolder":`+strconv.Quote(legacy)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newStore(prefsPath, filepath.Join(root, "default"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.localFolders, []string{legacy}) || store.activeGroup != legacy {
		t.Fatalf("folders %v, active %s", store.localFolders, store.activeGroup)
	}
}

func TestValidationAndSearchText(t *testing.T) {
	store := testStore(t)
	if got := store.nameProblem("a/b", ""); got == "" {
		t.Fatal("slash should be rejected")
	}
	data := []byte(`{"elements":[{"originalText":"Alpha"},{"text":"Beta"},{"text":"Beta"},{"text":"gone","isDeleted":true}]}`)
	if got, want := searchableTexts(data), []string{"Alpha", "Beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDataURLAndCollaborationSubjects(t *testing.T) {
	got, err := decodeDataURL("data:text/plain,hello%20world")
	if err != nil || string(got) != "hello world" {
		t.Fatalf("decode = %q, %v", got, err)
	}
	documentID := legacyDocumentID("team.bucket", "A board.excalidraw")
	if !validDocumentID(documentID) {
		t.Fatalf("invalid document id %q", documentID)
	}
	if got := collabUpdateSubject("team.bucket", documentID); !strings.HasSuffix(got, "."+documentID+".updates") {
		t.Fatalf("update subject = %q", got)
	}
	if collabUpdateSubject("team.bucket", documentID) == collabPresenceSubject("team.bucket", documentID) {
		t.Fatal("durable updates and ephemeral presence share a subject")
	}
}

func TestCollaborationObjectNamesAreHiddenAndStable(t *testing.T) {
	documentID := legacyDocumentID("bucket", "Board.excalidraw")
	if same := legacyDocumentID("bucket", "Board.excalidraw"); same != documentID {
		t.Fatalf("legacy document id changed: %q != %q", same, documentID)
	}
	if other := legacyDocumentID("bucket/Board", ".excalidraw"); other == documentID {
		t.Fatal("document tuple hashing is ambiguous")
	}
	snapshot := collabSnapshotName(documentID, 42, []byte("scene"))
	if !strings.HasPrefix(snapshot, ".crdt."+documentID+".snapshot.00000000000000000042.") {
		t.Fatalf("snapshot name = %q", snapshot)
	}
	if a, b := collabFileName(documentID, "file-a"), collabFileName(documentID, "file-b"); a == b || !strings.HasPrefix(a, ".crdt.") {
		t.Fatalf("file names are not hidden/distinct: %q %q", a, b)
	}
	if deleted := collabDeletedName(documentID); deleted != ".crdt."+documentID+".deleted" {
		t.Fatalf("deletion tombstone name = %q", deleted)
	}
}

func TestCollaborationOutboxSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	entry := collabOutboxEntry{
		SourceID:   "source",
		Bucket:     "bucket",
		DocumentID: legacyDocumentID("bucket", "Board.excalidraw"),
		MessageID:  "peer:1",
		Data:       `{"type":"scene"}`,
	}
	first := newApp()
	first.outboxPath = path
	if err := first.enqueueCollabOutbox(entry); err != nil {
		t.Fatal(err)
	}
	second := newApp()
	second.outboxPath = path
	if err := second.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	if got, ok := second.collabOutbox[collabOutboxKey(entry)]; !ok || got.SourceID != entry.SourceID || got.Bucket != entry.Bucket || got.DocumentID != entry.DocumentID || got.MessageID != entry.MessageID || got.Data != entry.Data || got.Order == 0 {
		t.Fatalf("reloaded outbox entry = %#v, present=%v", got, ok)
	}
	second.removeCollabOutbox(entry)
	third := newApp()
	third.outboxPath = path
	if err := third.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	if len(third.collabOutbox) != 0 {
		t.Fatalf("removed outbox was reloaded: %#v", third.collabOutbox)
	}
}

func TestCollaborationOutboxMigratesLegacyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	documentID := legacyDocumentID("bucket", "Board.excalidraw")
	legacy := []collabOutboxEntry{
		{SourceID: "source", Bucket: "bucket", DocumentID: documentID, MessageID: "peer:1", Data: `{"type":"scene","n":1}`},
		{SourceID: "source", Bucket: "bucket", DocumentID: documentID, MessageID: "peer:2", Data: `{"type":"scene","n":2}`},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	app := newApp()
	app.outboxPath = path
	if err := app.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	first := app.collabOutbox[collabOutboxKey(legacy[0])]
	second := app.collabOutbox[collabOutboxKey(legacy[1])]
	if first.Order == 0 || second.Order <= first.Order {
		t.Fatalf("migrated order = %d, %d", first.Order, second.Order)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated) == 0 || migrated[0] != '{' || !strings.HasSuffix(string(migrated), "\n") {
		t.Fatalf("legacy outbox was not rewritten as a JSONL journal: %q", migrated)
	}
}

func TestCollaborationOutboxIgnoresOnlyTornFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	documentID := legacyDocumentID("bucket", "Board.excalidraw")
	entry := collabOutboxEntry{SourceID: "source", Bucket: "bucket", DocumentID: documentID, MessageID: "peer:1", Data: `{"type":"scene"}`, Order: 1}
	record, err := json.Marshal(collabOutboxRecord{Op: "put", Entry: &entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(record, '\n'), []byte(`{"op":"put","entry":`)...), 0o600); err != nil {
		t.Fatal(err)
	}

	app := newApp()
	app.outboxPath = path
	if err := app.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	if got := app.collabOutbox[collabOutboxKey(entry)]; got.Data != entry.Data || got.Order != entry.Order {
		t.Fatalf("complete journal record was not recovered: %#v", got)
	}

	if err := os.WriteFile(path, []byte(`{"op":"put"`), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := newApp()
	empty.outboxPath = path
	if err := empty.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	if len(empty.collabOutbox) != 0 {
		t.Fatalf("torn-only journal recovered phantom entries: %#v", empty.collabOutbox)
	}
}

func TestEmptyCollaborationOutboxJournalReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	app.outboxPath = path
	if err := app.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
}

func TestCollabPublishIsDurableBeforeReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	documentID := legacyDocumentID("bucket", "Board.excalidraw")
	app := newApp()
	app.outboxPath = path

	sequence, err := app.CollabPublish("source", "bucket", documentID, "peer:1", `{"type":"scene"}`)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != "0" {
		t.Fatalf("local durability sequence = %q, want 0", sequence)
	}
	select {
	case <-app.outboxWake:
	default:
		t.Fatal("CollabPublish did not wake the background replicator")
	}

	restarted := newApp()
	restarted.outboxPath = path
	if err := restarted.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	key := collabOutboxKey(collabOutboxEntry{
		SourceID:   "source",
		Bucket:     "bucket",
		DocumentID: documentID,
		MessageID:  "peer:1",
	})
	entry, ok := restarted.collabOutbox[key]
	if !ok || entry.Data != `{"type":"scene"}` {
		t.Fatalf("durable publication = %#v, present=%v", entry, ok)
	}
}

func TestCollaborationBlobsAreDurableBeforeReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collab-outbox.json")
	documentID := legacyDocumentID("bucket", "Board.excalidraw")
	app := newApp()
	app.outboxPath = path
	files := `{"file:a":{"id":"a","dataURL":"data:image/png;base64,AA=="},"scene:peer:1":{"type":"scene","elements":[]}}`
	if err := app.CollabPutFiles("source", "bucket", documentID, files); err != nil {
		t.Fatal(err)
	}
	if len(app.collabOutbox) != 2 {
		t.Fatalf("blob outbox length = %d, want 2", len(app.collabOutbox))
	}

	restarted := newApp()
	restarted.outboxPath = path
	if err := restarted.loadCollabOutbox(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.collabOutbox) != 2 {
		t.Fatalf("reloaded blob outbox length = %d, want 2", len(restarted.collabOutbox))
	}
	for _, entry := range restarted.collabOutbox {
		if entry.Kind != "blob" || entry.BlobID == "" {
			t.Fatalf("invalid blob outbox entry: %#v", entry)
		}
		if data, err := os.ReadFile(restarted.collabBlobPath(entry)); err != nil || len(data) == 0 {
			t.Fatalf("blob payload was not persisted: size=%d err=%v", len(data), err)
		}
		restarted.removeCollabOutbox(entry)
		if _, err := os.Stat(restarted.collabBlobPath(entry)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replicated blob payload still exists: %v", err)
		}
	}
}

func TestPendingResultPropagatesRendererFailure(t *testing.T) {
	app := newApp()
	result := make(chan struct {
		edit *pendingEdit
		err  error
	}, 1)
	go func() {
		edit, err := app.takePending()
		result <- struct {
			edit *pendingEdit
			err  error
		}{edit: edit, err: err}
	}()

	var requestID string
	deadline := time.Now().Add(time.Second)
	for requestID == "" && time.Now().Before(deadline) {
		app.pending.Range(func(key, _ any) bool {
			requestID, _ = key.(string)
			return false
		})
		if requestID == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if requestID == "" {
		t.Fatal("takePending did not register its request")
	}
	message := "could not persist final collaboration update"
	app.PendingResult(requestID, nil, nil, &message)

	got := <-result
	if got.edit != nil {
		t.Fatalf("pending edit = %#v, want nil", got.edit)
	}
	if got.err == nil || got.err.Error() != message {
		t.Fatalf("pending error = %v, want %q", got.err, message)
	}
}

func TestServiceShutdownDoesNotWaitForDestroyedRenderer(t *testing.T) {
	app := newApp()
	started := time.Now()
	if err := app.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ServiceShutdown took %v; renderer round-trip must happen before window close", elapsed)
	}
}

// resolveTestNATS starts a throwaway JetStream server when url is "spawn".
func resolveTestNATS(t *testing.T, url string) string {
	t.Helper()
	if url != "spawn" {
		return url
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	logFile, err := os.Create(filepath.Join(t.TempDir(), "nats-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("nats-server", "--jetstream", "--addr", "127.0.0.1", "--port", strconv.Itoa(port), "--store_dir", t.TempDir())
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
	})
	return fmt.Sprintf("nats://127.0.0.1:%d", port)
}

func TestSharedLibrarySyncsAcrossBuckets(t *testing.T) {
	serverURL := os.Getenv("EXCALIBUR_TEST_NATS_URL")
	if serverURL == "" {
		t.Skip("set EXCALIBUR_TEST_NATS_URL to a server URL or 'spawn' to run the JetStream integration test")
	}
	serverURL = resolveTestNATS(t, serverURL)
	var clients [2]*natsClient
	for i := range clients {
		id := fmt.Sprintf("lib-%d", i)
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if clients[i], err = connectNATS(NatsServerConfig{ID: id, Name: id, URL: serverURL}); err == nil {
				break
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(clients[i].close)
	}
	changes := make(chan string, 4)
	if err := clients[1].watchLibrary(func(data []byte) { changes <- string(data) }); err != nil {
		t.Fatal(err)
	}
	if err := clients[0].watchLibrary(func(data []byte) { changes <- "own:" + string(data) }); err != nil {
		t.Fatal(err)
	}
	if err := clients[0].writeObject("bucket-a", libraryName, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	store.natsClients["lib-0"] = clients[0]
	store.activeSource, store.activeGroup = "nats:lib-0", "bucket-a"
	library, err := store.library()
	if err != nil || library == nil || *library != "legacy" {
		t.Fatalf("legacy library not adopted: %v, %v", library, err)
	}
	if err := store.saveLibrary("shared"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"legacy", "shared"} {
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("watcher saw %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("library change %q did not reach the other client", want)
		}
	}
	store.activeGroup = "bucket-b"
	if library, err = store.library(); err != nil || library == nil || *library != "shared" {
		t.Fatalf("library in another bucket = %v, %v", library, err)
	}
	select {
	case got := <-changes:
		t.Fatalf("unexpected library change %q (own writes must not be echoed)", got)
	case <-time.After(300 * time.Millisecond):
	}
	if buckets, err := clients[0].listBuckets(); err != nil || slices.Contains(buckets, libraryBucket) {
		t.Fatalf("library bucket leaked into bucket list: %v, %v", buckets, err)
	}
}

func TestCollaborationJetStreamDurability(t *testing.T) {
	serverURL := os.Getenv("EXCALIBUR_TEST_NATS_URL")
	if serverURL == "" {
		t.Skip("set EXCALIBUR_TEST_NATS_URL to a server URL or 'spawn' to run the JetStream integration test")
	}
	serverURL = resolveTestNATS(t, serverURL)
	var client *natsClient
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client, err = connectNATS(NatsServerConfig{ID: "test", Name: "test", URL: serverURL})
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("connect to integration server: %v", err)
	}
	defer client.close()
	secondClient, err := connectNATS(NatsServerConfig{ID: "test-2", Name: "test-2", URL: serverURL})
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.close()
	thirdClient, err := connectNATS(NatsServerConfig{ID: "test-3", Name: "test-3", URL: serverURL})
	if err != nil {
		t.Fatal(err)
	}
	defer thirdClient.close()
	bucket := fmt.Sprintf("test-%d", time.Now().UnixNano())
	documentID, err := newDocumentID()
	if err != nil {
		t.Fatal(err)
	}
	secondDocumentID, err := newDocumentID()
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot fencing and the periodic heartbeat can renew the same owner at
	// the same instant. A CAS conflict caused by that same owner must be retried,
	// not reported as leadership loss.
	const leaseOwner = "integration-owner"
	if won, err := client.acquireLock(bucket, documentID, leaseOwner, 30*time.Second); err != nil || !won {
		t.Fatalf("acquire integration lease: won=%v err=%v", won, err)
	}
	for round := 0; round < 20; round++ {
		start := make(chan struct{})
		results := make(chan bool, 2)
		errorsCh := make(chan error, 2)
		for _, lockClient := range []*natsClient{client, secondClient} {
			go func(c *natsClient) {
				<-start
				ok, err := c.renewLock(bucket, documentID, leaseOwner, 30*time.Second)
				results <- ok
				errorsCh <- err
			}(lockClient)
		}
		close(start)
		for attempt := 0; attempt < 2; attempt++ {
			if err := <-errorsCh; err != nil {
				t.Fatalf("concurrent lease renewal round %d: %v", round, err)
			}
			if ok := <-results; !ok {
				t.Fatalf("same-owner lease renewal was reported lost in round %d", round)
			}
		}
	}
	first, err := client.publishCollabUpdate(bucket, documentID, "peer:1", []byte(`{"type":"scene","elements":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := client.publishCollabUpdate(bucket, documentID, "peer:1", []byte(`{"type":"scene","elements":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate != first {
		t.Fatalf("deduplicated sequence = %d, want %d", duplicate, first)
	}
	secondBoard, err := secondClient.publishCollabUpdate(bucket, secondDocumentID, "peer:2", []byte(`{"type":"scene","elements":[{"id":"other"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	third, err := thirdClient.publishCollabUpdate(bucket, documentID, "peer:3", []byte(`{"type":"scene","elements":[{"id":"third"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if first >= secondBoard || secondBoard >= third {
		t.Fatalf("stream sequences are not monotonic: %d, %d, %d", first, secondBoard, third)
	}
	// App subscriptions keep messages unacknowledged until the renderer confirms
	// receipt. This makes a lost Wails event redeliverable instead of silently
	// advancing the consumer.
	store := testStore(t)
	store.natsClients["test"] = client
	app := newApp()
	app.store = store
	app.outboxPath = filepath.Join(t.TempDir(), "collab-outbox.json")
	const blobID = "file:integration:1"
	const blobJSON = `{"id":"integration","dataURL":"data:image/png;base64,AA==","version":1}`
	if err := app.CollabPutFiles("nats:test", bucket, documentID, `{"`+blobID+`":`+blobJSON+`}`); err != nil {
		t.Fatal(err)
	}
	if len(app.collabOutbox) != 1 {
		t.Fatalf("durable blob was not queued: %#v", app.collabOutbox)
	}
	app.flushCollabOutbox()
	if len(app.collabOutbox) != 0 {
		t.Fatalf("replicated blob remained queued: %#v", app.collabOutbox)
	}
	if blob, err := client.readCollabFile(bucket, documentID, blobID); err != nil || string(blob) != blobJSON {
		t.Fatalf("replicated blob = %q, err=%v", blob, err)
	}
	const subscriberID = "renderer-test"
	if _, err := app.CollabJoin("nats:test", bucket, documentID, subscriberID, "0"); err != nil {
		t.Fatal(err)
	}
	pendingKey := "nats:test\x00" + bucket + "\x00" + documentID + "\x00" + subscriberID + "\x00" + strconv.FormatUint(first, 10)
	deadline = time.Now().Add(2 * time.Second)
	for {
		app.subMu.Lock()
		pending := app.collabPending[pendingKey] != nil
		app.subMu.Unlock()
		if pending || time.Now().After(deadline) {
			if !pending {
				t.Fatal("durable update was not held pending for renderer acknowledgement")
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := app.CollabAck("nats:test", bucket, documentID, subscriberID, strconv.FormatUint(first, 10)); err != nil {
		t.Fatal(err)
	}
	app.subMu.Lock()
	_, stillPending := app.collabPending[pendingKey]
	app.subMu.Unlock()
	if stillPending {
		t.Fatal("renderer acknowledgement did not clear the pending update")
	}
	if err := app.CollabLeave("nats:test", bucket, documentID, subscriberID); err != nil {
		t.Fatal(err)
	}
	stream := collabStreamName(bucket)
	firstBoardSub, err := client.js.SubscribeSync(collabUpdateSubject(bucket, documentID), nats.BindStream(stream), nats.DeliverAll())
	if err != nil {
		t.Fatal(err)
	}
	defer firstBoardSub.Unsubscribe()
	for i, wantSequence := range []uint64{first, third} {
		message, err := firstBoardSub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("first-board replay message %d: %v", i, err)
		}
		metadata, err := message.Metadata()
		if err != nil || metadata.Sequence.Stream != wantSequence {
			t.Fatalf("first-board replay sequence = %v, err=%v, want %d", metadata, err, wantSequence)
		}
	}
	if _, err := firstBoardSub.NextMsg(50 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("first-board consumer received a cross-board message: %v", err)
	}
	secondBoardSub, err := client.js.SubscribeSync(collabUpdateSubject(bucket, secondDocumentID), nats.BindStream(stream), nats.DeliverAll())
	if err != nil {
		t.Fatal(err)
	}
	defer secondBoardSub.Unsubscribe()
	message, err := secondBoardSub.NextMsg(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := message.Metadata()
	if err != nil || metadata.Sequence.Stream != secondBoard {
		t.Fatalf("second-board replay sequence = %v, err=%v, want %d", metadata, err, secondBoard)
	}
	const snapshot = `{"type":"excalidraw","elements":[]}`
	if err := client.writeCollabSnapshot(bucket, documentID, third, []byte(snapshot)); err != nil {
		t.Fatal(err)
	}
	data, sequence, err := client.latestCollabSnapshot(bucket, documentID)
	if err != nil || sequence != third || string(data) != snapshot {
		t.Fatalf("snapshot = %q at %d, err=%v", data, sequence, err)
	}
	if unchanged, unchangedSequence, err := client.collabSnapshotAfter(bucket, documentID, third); err != nil || unchanged != nil || unchangedSequence != third {
		t.Fatalf("unchanged snapshot was retransferred: data=%q sequence=%d err=%v", unchanged, unchangedSequence, err)
	}
	const baselineSubscriber = "renderer-with-baseline"
	baselineResult, err := app.CollabJoin("nats:test", bucket, documentID, baselineSubscriber, strconv.FormatUint(third, 10))
	if err != nil {
		t.Fatal(err)
	}
	if baselineResult.Snapshot != nil || baselineResult.SnapshotSequence != strconv.FormatUint(third, 10) {
		t.Fatalf("join retransferred loaded baseline: snapshot=%v sequence=%q", baselineResult.Snapshot != nil, baselineResult.SnapshotSequence)
	}
	if err := app.CollabLeave("nats:test", bucket, documentID, baselineSubscriber); err != nil {
		t.Fatal(err)
	}
	info, err := client.js.StreamInfo(collabStreamName(bucket))
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxAge != 0 || info.Config.MaxBytes != -1 {
		t.Fatalf("collaboration stream can discard unsnapshotted edits: age=%s bytes=%d", info.Config.MaxAge, info.Config.MaxBytes)
	}
	// Compaction is subject-filtered and deliberately trails the newest snapshot
	// by one interval, so a join racing snapshot discovery can still replay.
	fourth, err := secondClient.publishCollabUpdate(bucket, documentID, "peer:4", []byte(`{"type":"scene","elements":[{"id":"fourth"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.writeCollabSnapshot(bucket, documentID, fourth, []byte(snapshot)); err != nil {
		t.Fatal(err)
	}
	if err := thirdClient.writeCollabSnapshot(bucket, documentID, third, []byte(`{"type":"excalidraw","elements":[{"id":"stale"}]}`)); err != nil {
		t.Fatal(err)
	}
	if data, sequence, err := client.latestCollabSnapshot(bucket, documentID); err != nil || sequence != fourth || string(data) != snapshot {
		t.Fatalf("stale compactor regressed snapshot: data=%q sequence=%d err=%v", data, sequence, err)
	}
	previous, err := client.previousCollabSnapshotSequence(bucket, documentID)
	if err != nil || previous != third {
		t.Fatalf("previous snapshot sequence = %d, err=%v, want %d", previous, err, third)
	}
	if err := client.purgeCollabUpdates(bucket, documentID, previous); err != nil {
		t.Fatal(err)
	}
	compactedSub, err := client.js.SubscribeSync(collabUpdateSubject(bucket, documentID), nats.BindStream(stream), nats.DeliverAll())
	if err != nil {
		t.Fatal(err)
	}
	defer compactedSub.Unsubscribe()
	message, err = compactedSub.NextMsg(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = message.Metadata()
	if err != nil || metadata.Sequence.Stream != fourth {
		t.Fatalf("post-compaction sequence = %v, err=%v, want %d", metadata, err, fourth)
	}
	secondAfterPurge, err := client.js.SubscribeSync(collabUpdateSubject(bucket, secondDocumentID), nats.BindStream(stream), nats.DeliverAll())
	if err != nil {
		t.Fatal(err)
	}
	defer secondAfterPurge.Unsubscribe()
	message, err = secondAfterPurge.NextMsg(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = message.Metadata()
	if err != nil || metadata.Sequence.Stream != secondBoard {
		t.Fatalf("cross-document compaction removed sequence: metadata=%v err=%v want=%d", metadata, err, secondBoard)
	}

	// Bucket deletion is destructive and must be enforced by the backend, not
	// merely by the menu state. Visible boards block deletion; hidden CRDT data
	// does not, and the per-bucket collaboration stream is removed with it.
	nonemptyBucket := bucket + "-nonempty"
	if err := client.writeObject(nonemptyBucket, "Keep.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.deleteBucket(nonemptyBucket); err == nil {
		t.Fatal("deleted a bucket that still contained a board")
	}
	if _, err := client.js.ObjectStore(nonemptyBucket); err != nil {
		t.Fatalf("nonempty bucket was removed after rejected delete: %v", err)
	}

	emptyBucket := bucket + "-empty"
	if err := client.writeObject(emptyBucket, ".hidden-metadata", []byte("metadata")); err != nil {
		t.Fatal(err)
	}
	emptyStream, err := client.ensureCollabStream(emptyBucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.deleteBucket(emptyBucket); err != nil {
		t.Fatal(err)
	}
	if _, err := client.js.ObjectStore(emptyBucket); err == nil {
		t.Fatal("empty bucket still exists after deletion")
	}
	if _, err := client.js.StreamInfo(emptyStream); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("collaboration stream still exists after bucket deletion: %v", err)
	}
}

func TestNormalizeCollaborationAppearance(t *testing.T) {
	for _, avatar := range []string{"🦊", "🧑‍🚀", "🗡️", "🇳🇴", ""} {
		if color, got, err := normalizeCollaborationAppearance(" #AABBCC ", avatar); err != nil || color != "#aabbcc" || got != avatar {
			t.Fatalf("appearance with %q = %q, %q, %v", avatar, color, got, err)
		}
	}
	if color, got, err := normalizeCollaborationAppearance("", ""); err != nil || color != "" || got != "" {
		t.Fatalf("empty appearance = %q, %q, %v", color, got, err)
	}
	for _, bad := range [][2]string{
		{"red", ""},
		{"#abc", ""},
		{"", "A"},
		{"", "7"},
		{"", "🦊🦊 🦊"},
		{"", "<svg>"},
		{"", "é"},
		{"", strings.Repeat("🦊", maxCollaborationAvatarRunes+1)},
	} {
		if _, _, err := normalizeCollaborationAppearance(bad[0], bad[1]); err == nil {
			t.Fatalf("appearance %q / %q should be rejected", bad[0], bad[1])
		}
	}
}

func TestNormalizeCollaborationNickname(t *testing.T) {
	if got, err := normalizeCollaborationNickname("  Merlin  "); err != nil || got != "Merlin" {
		t.Fatalf("normalized nickname = %q, %v", got, err)
	}
	if got, err := normalizeCollaborationNickname("   "); err != nil || got != "" {
		t.Fatalf("blank nickname = %q, %v", got, err)
	}
	if _, err := normalizeCollaborationNickname("line\nbreak"); err == nil {
		t.Fatal("nickname containing a control character should be rejected")
	}
	if _, err := normalizeCollaborationNickname(strings.Repeat("x", maxCollaborationNicknameLength+1)); err == nil {
		t.Fatal("overlong nickname should be rejected")
	}
	if got, err := normalizeCollaborationNickname(strings.Repeat("🗡", maxCollaborationNicknameLength)); err != nil || got == "" {
		t.Fatalf("rune-length nickname = %q, %v", got, err)
	}
}

func TestSetCollaborationNicknamePersists(t *testing.T) {
	store := testStore(t)
	app := newApp()
	app.store = store
	if err := app.SetCollaborationNickname("  Merlin  "); err != nil {
		t.Fatal(err)
	}
	if got := store.snapshot().CollabNickname; got != "Merlin" {
		t.Fatalf("snapshot nickname = %q", got)
	}
	data, err := os.ReadFile(store.prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	var prefs Preferences
	if err = json.Unmarshal(data, &prefs); err != nil {
		t.Fatal(err)
	}
	if prefs.CollabNickname != "Merlin" {
		t.Fatalf("persisted nickname = %q", prefs.CollabNickname)
	}
}

func TestNormalizeNatsServerConfigCredentialsFile(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "operator.creds")
	if err := os.WriteFile(credentials, []byte("test credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeNatsServerConfig(NatsServerConfig{
		ID:              " server ",
		Name:            " Secure NATS ",
		URL:             " nats://example.test:4222 ",
		Username:        " ignored ",
		Password:        "ignored",
		CredentialsFile: " " + credentials + " ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ID != "server" || config.Name != "Secure NATS" || config.URL != "nats://example.test:4222" {
		t.Fatalf("server fields were not normalized: %#v", config)
	}
	if config.CredentialsFile != credentials {
		t.Fatalf("credentials file = %q, want %q", config.CredentialsFile, credentials)
	}
	if config.Username != "" || config.Password != "" {
		t.Fatalf("credentials file should replace username/password: %#v", config)
	}
}

func TestNormalizeNatsServerConfigRejectsInvalidCredentialsPath(t *testing.T) {
	base := NatsServerConfig{ID: "server", Name: "Secure NATS", URL: "nats://example.test:4222"}
	base.CredentialsFile = filepath.Join(t.TempDir(), "missing.creds")
	if _, err := normalizeNatsServerConfig(base); err == nil || !strings.Contains(err.Error(), "could not access NATS credentials file") {
		t.Fatalf("missing credentials error = %v", err)
	}
	base.CredentialsFile = t.TempDir()
	if _, err := normalizeNatsServerConfig(base); err == nil || !strings.Contains(err.Error(), "is not a file") {
		t.Fatalf("directory credentials error = %v", err)
	}
}

func TestNatsServerSnapshotRedactsPassword(t *testing.T) {
	store := testStore(t)
	store.prefs.NatsServers = []NatsServerConfig{{
		ID:       "private",
		Name:     "Private NATS",
		URL:      "nats://example.test:4222",
		Username: "merlin",
		Password: "sword-in-the-stone",
		Buckets:  []string{"boards"},
	}}

	snapshot := store.snapshot()
	if len(snapshot.NatsServers) != 1 {
		t.Fatalf("NATS settings count = %d, want 1", len(snapshot.NatsServers))
	}
	settings := snapshot.NatsServers[0]
	if settings.ID != "private" || settings.Username != "merlin" || !settings.HasPassword {
		t.Fatalf("NATS settings = %#v", settings)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sword-in-the-stone") {
		t.Fatal("snapshot exposed the saved NATS password")
	}
}

func TestChatCompletionsURL(t *testing.T) {
	cases := map[string]string{
		"https://ai.example":                     "https://ai.example/v1/chat/completions",
		"https://ai.example/api/v1/":             "https://ai.example/api/v1/chat/completions",
		"https://ai.example/v1/chat/completions": "https://ai.example/v1/chat/completions",
	}
	for input, want := range cases {
		got, err := chatCompletionsURL(input)
		if err != nil || got != want {
			t.Errorf("chatCompletionsURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := chatCompletionsURL("file:///tmp/model"); err == nil {
		t.Fatal("non-HTTP URL should be rejected")
	}
}

func TestAIModelsURLUsesGatewayRoute(t *testing.T) {
	cases := map[string]string{
		"https://ai.example":                     "https://ai.example/gate/v1/models",
		"https://ai.example/api/v1/":             "https://ai.example/gate/v1/models",
		"https://ai.example/v1/chat/completions": "https://ai.example/gate/v1/models",
	}
	for input, want := range cases {
		got, err := aiModelsURL(input)
		if err != nil || got != want {
			t.Errorf("aiModelsURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestAIChatRejectsBoardSwitch(t *testing.T) {
	store := testStore(t)
	if err := store.createBoard("First"); err != nil {
		t.Fatal(err)
	}
	if err := store.createBoard("Second"); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	app.store = store

	if _, _, err := app.canvasDataForAI("First.excalidraw"); !errors.Is(err, errAIChatBoardChanged) {
		t.Fatalf("canvasDataForAI() error = %v, want board-changed error", err)
	}
	if _, err := app.AIChat([]AIChatMessage{{Role: "user", Content: "What is here?"}}, "test-model", "request", "First.excalidraw", false); !errors.Is(err, errAIChatBoardChanged) {
		t.Fatalf("AIChat() error = %v, want board-changed error", err)
	}
}

func TestRequestAIModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/gate/v1/models" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"zeta-id","model":"zeta","reasoning":true},{"id":"alpha-id","model":"alpha","reasoning":true,"options":{"chat_template_kwargs":{"enable_thinking":false}}},{"id":"duplicate-id","model":"alpha"},{"id":"id-only"},{"model":""}]}`))
	}))
	defer server.Close()

	models, err := requestAIModels(server.Client(), server.URL+"/gate/v1/models", "secret")
	if err != nil {
		t.Fatal(err)
	}
	want := []AIModel{{ID: "alpha-id", Model: "alpha", Thinking: false}, {ID: "zeta-id", Model: "zeta", Thinking: true}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v", models)
	}
	if !modelAvailable(models, "alpha") || modelAvailable(models, "alpha-id") || modelAvailable(models, "missing") {
		t.Fatalf("unexpected availability result for %#v", models)
	}
}

func TestOpenAIRequestIncludesCanvasToolAndOptionalKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		var payload struct {
			Model              string          `json:"model"`
			ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
			Tools              []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		if payload.Model != "test-model" || len(payload.Tools) != 2 || payload.Tools[0].Function.Name != canvasToolName || payload.Tools[1].Function.Name != canvasOperationsTool {
			t.Errorf("unexpected request: %#v", payload)
		}
		if payload.ChatTemplateKwargs != nil {
			t.Errorf("thinking options were sent for a non-thinking model: %#v", payload.ChatTemplateKwargs)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer server.Close()

	message, err := requestAI(server.Client(), server.URL, AIConfig{Model: "test-model", APIKey: "secret"}, []openAIMessage{{Role: "user", Content: "hi"}})
	if err != nil || message.Content != "hello" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIToolFreeFinalizationOmitsTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		if _, present := payload["tools"]; present {
			t.Errorf("tool-free final request unexpectedly contains tools: %s", payload["tools"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"finished"}}]}`))
	}))
	defer server.Close()

	message, err := requestAIWithTools(server.Client(), server.URL, AIConfig{Model: "test-model"}, []openAIMessage{{Role: "user", Content: "summarize"}}, nil)
	if err != nil || message.Content != "finished" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIForcedToolChoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Tools      []any `json:"tools"`
			ToolChoice struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(payload.Tools) != 2 || payload.ToolChoice.Type != "function" || payload.ToolChoice.Function.Name != canvasOperationsTool {
			t.Errorf("unexpected forced tool request: %#v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"apply_canvas_operations","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	message, err := requestAIWithToolChoice(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "draw a box"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, forceToolChoice(canvasOperationsTool),
	)
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != canvasOperationsTool {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
}

func TestOpenAIStreamingToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Stream             bool            `json:"stream"`
			ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !payload.Stream {
			t.Error("stream was not enabled")
		}
		if !payload.ChatTemplateKwargs["enable_thinking"] {
			t.Error("thinking was not enabled")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"apply_canvas_","arguments":"{\"expected\""}}]}}]}` + "\n\n"))
		_, _ = writer.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"operations","arguments":":true}"}}]}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	message, err := requestAIWithToolChoice(
		server.Client(), server.URL, AIConfig{Model: "test-model", EnableThinking: true},
		[]openAIMessage{{Role: "user", Content: "draw a box"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, forceToolChoice(canvasOperationsTool),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", message.ToolCalls)
	}
	call := message.ToolCalls[0]
	if call.ID != "call-1" || call.Type != "function" || call.Function.Name != canvasOperationsTool || call.Function.Arguments != `{"expected":true}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestOpenAIStreamingContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var streamed strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}},
		[]any{canvasTool, canvasOperationsToolDefinition}, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamAnswer {
				streamed.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "Hello world" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if streamed.String() != "Hello world" {
		t.Fatalf("streamed content = %q", streamed.String())
	}
}

func TestOpenAIStreamingThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Check \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"thinking\":\"canvas. \",\"content\":\"Done\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var thinking, answer strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}}, nil, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamThinking {
				thinking.WriteString(delta)
			} else {
				answer.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "Done" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if thinking.String() != "Check canvas. " || answer.String() != "Done" {
		t.Fatalf("thinking = %q, answer = %q", thinking.String(), answer.String())
	}
}

func TestOpenAIStreamingContentThoughtChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"th\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ought\\nCheck \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"the canvas\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var thinking, answer strings.Builder
	message, err := requestAIWithToolChoiceStream(
		server.Client(), server.URL, AIConfig{Model: "test-model"},
		[]openAIMessage{{Role: "user", Content: "hi"}}, nil, nil,
		func(kind aiStreamKind, delta string) {
			if kind == aiStreamThinking {
				thinking.WriteString(delta)
			} else {
				answer.WriteString(delta)
			}
		},
	)
	if err != nil || message.Content != "thought\nCheck the canvas" {
		t.Fatalf("message = %#v, error = %v", message, err)
	}
	if thinking.String() != "thought\nCheck the canvas" || answer.Len() != 0 {
		t.Fatalf("thinking = %q, answer = %q", thinking.String(), answer.String())
	}
}

func TestSlidingResponseTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newSlidingTimeoutBody(reader, 25*time.Millisecond)
	defer body.Close()

	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, errAIResponseIdleTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestCanvasEditIntent(t *testing.T) {
	for _, prompt := range []string{
		"Create a kanban board with three cards",
		"Please move the rectangle to the right",
		"delete the selected-looking element",
		"make a flowchart for the deployment",
	} {
		if !canvasEditIntent(prompt) {
			t.Errorf("expected edit intent for %q", prompt)
		}
	}
	for _, prompt := range []string{
		"What is on the canvas?",
		"Summarize this drawing",
		"How do I create a rectangle myself?",
	} {
		if canvasEditIntent(prompt) {
			t.Errorf("unexpected edit intent for %q", prompt)
		}
	}
}

func TestApplyCanvasOperations(t *testing.T) {
	document := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"keep","type":"rectangle","x":1,"y":2,"width":10,"height":20,"version":1,"versionNonce":1,"isDeleted":false}],"appState":{"viewBackgroundColor":"#fff"},"files":{}}`)
	arguments := []byte(`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"keep","changes":{"x":25,"strokeColor":"#ff0000"}},{"op":"add","element":{"id":"new-text","type":"text","x":50,"y":60,"text":"Hello","originalText":"Hello"}},{"op":"delete","id":"keep"}]}`)

	updated, result, err := applyCanvasOperations(document, arguments, "Board.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
	var scene struct {
		Elements []map[string]any `json:"elements"`
		AppState map[string]any   `json:"appState"`
	}
	if err := json.Unmarshal(updated, &scene); err != nil {
		t.Fatal(err)
	}
	if len(scene.Elements) != 2 || scene.Elements[0]["x"] != float64(25) || scene.Elements[0]["isDeleted"] != true {
		t.Fatalf("updated existing element = %#v", scene.Elements)
	}
	if scene.Elements[0]["version"] != float64(3) {
		t.Fatalf("version = %#v, want 3", scene.Elements[0]["version"])
	}
	if scene.Elements[1]["id"] != "new-text" || scene.Elements[1]["type"] != "text" || scene.Elements[1]["width"] != float64(100) {
		t.Fatalf("new element = %#v", scene.Elements[1])
	}
	if scene.AppState["viewBackgroundColor"] != "#fff" {
		t.Fatalf("app state was not preserved: %#v", scene.AppState)
	}
}

func TestApplyCanvasOperationsRejectsUnsafeChanges(t *testing.T) {
	document := []byte(`{"elements":[{"id":"a","type":"rectangle","x":0,"y":0}]}`)
	cases := []string{
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"a","changes":{"id":"b"}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"rectangle","x":"bad","y":0}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"delete","id":"missing"}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[]}`,
		`{"expectedFilename":"Other.excalidraw","operations":[{"op":"delete","id":"a"}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"update","id":"a","changes":{"madeUpField":true}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"selection","x":0,"y":0}}]}`,
		`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","element":{"type":"rectangle","x":0,"y":0,"isDeleted":true}}]}`,
	}
	for _, arguments := range cases {
		if _, _, err := applyCanvasOperations(document, []byte(arguments), "Board.excalidraw"); err == nil {
			t.Errorf("expected %s to be rejected", arguments)
		}
	}
}

func TestCanvasToolCoversExcalidrawElementAPI(t *testing.T) {
	properties := canvasElementProperties(true)
	for _, field := range []string{
		"id", "type", "x", "y", "width", "height", "angle", "strokeColor", "backgroundColor",
		"fillStyle", "strokeWidth", "strokeStyle", "roughness", "opacity", "roundness", "groupIds",
		"frameId", "boundElements", "link", "locked", "customData", "points", "startBinding", "endBinding",
		"startArrowhead", "endArrowhead", "elbowed", "fixedSegments", "startIsSpecial", "endIsSpecial",
		"pressures", "simulatePressure", "text", "originalText", "fontSize", "fontFamily", "textAlign",
		"verticalAlign", "containerId", "autoResize", "lineHeight", "fileId", "status", "scale", "crop", "name",
	} {
		if _, ok := properties[field]; !ok {
			t.Errorf("canvas tool schema is missing %q", field)
		}
	}
	wantTypes := map[string]bool{
		"rectangle": true, "diamond": true, "ellipse": true, "line": true, "arrow": true,
		"text": true, "image": true, "freedraw": true, "frame": true, "magicframe": true,
		"iframe": true, "embeddable": true,
	}
	for _, elementType := range canvasElementTypes {
		delete(wantTypes, elementType)
	}
	if len(wantTypes) != 0 {
		t.Fatalf("canvas tool schema is missing element types: %#v", wantTypes)
	}
	if _, err := json.Marshal(canvasOperationsToolDefinition); err != nil {
		t.Fatalf("canvas tool definition is not JSON serializable: %v", err)
	}
}

func TestApplyCanvasOperationsSupportsZOrderAndTypeDefaults(t *testing.T) {
	document := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"back","type":"rectangle","x":0,"y":0},{"id":"front","type":"ellipse","x":10,"y":10}],"appState":{},"files":{}}`)
	arguments := []byte(`{"expectedFilename":"Board.excalidraw","operations":[{"op":"add","position":"after","referenceId":"back","element":{"id":"label","type":"text","x":5,"y":5,"text":"Hello"}},{"op":"add","position":"back","element":{"id":"connector","type":"arrow","x":0,"y":0}},{"op":"reorder","id":"front","position":"before","referenceId":"label"}]}`)

	updated, _, err := applyCanvasOperations(document, arguments, "Board.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	var scene struct {
		Elements []map[string]any `json:"elements"`
	}
	if err := json.Unmarshal(updated, &scene); err != nil {
		t.Fatal(err)
	}
	gotOrder := make([]string, len(scene.Elements))
	for index, element := range scene.Elements {
		gotOrder[index], _ = element["id"].(string)
	}
	wantOrder := []string{"connector", "back", "front", "label"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("z-order = %v, want %v", gotOrder, wantOrder)
	}
	connector := scene.Elements[0]
	if connector["endArrowhead"] != "arrow" || len(connector["points"].([]any)) != 2 {
		t.Fatalf("arrow defaults = %#v", connector)
	}
	label := scene.Elements[3]
	if label["originalText"] != "Hello" || label["fontSize"] != float64(20) {
		t.Fatalf("text defaults = %#v", label)
	}
}

func TestRequestAIReportsPlainTextServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("no clients with model deepseek-v4-flash"))
	}))
	defer server.Close()

	_, err := requestAI(server.Client(), server.URL, AIConfig{Model: "deepseek-v4-flash"}, []openAIMessage{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "no clients with model deepseek-v4-flash") || strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplicationZoomDefaultsAndValidation(t *testing.T) {
	store := testStore(t)
	if store.prefs.ApplicationZoom != defaultApplicationZoom {
		t.Fatalf("application zoom = %v", store.prefs.ApplicationZoom)
	}
	for _, zoom := range []float64{minApplicationZoom, 1, maxApplicationZoom} {
		if !validApplicationZoom(zoom) {
			t.Errorf("zoom %v should be valid", zoom)
		}
	}
	for _, zoom := range []float64{0, minApplicationZoom - 0.01, maxApplicationZoom + 0.01} {
		if validApplicationZoom(zoom) {
			t.Errorf("zoom %v should be invalid", zoom)
		}
	}
}

func TestBoardsFolderUsableTempDir(t *testing.T) {
	if !boardsFolderUsable(t.TempDir()) {
		t.Fatal("expected a temp dir to be a usable boards folder")
	}
}

func TestBoardsFolderFallsBackWhenDocumentsBlocked(t *testing.T) {
	home := t.TempDir()
	// Block the preferred ~/Documents/Whiteboards location by making Documents a file.
	if err := os.WriteFile(filepath.Join(home, "Documents"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := boardsFolder(home), filepath.Join(home, "Whiteboards"); got != want {
		t.Fatalf("expected fallback %q, got %q", want, got)
	}
}

func TestImportCredentialsFileCopiesPrivately(t *testing.T) {
	src := filepath.Join(t.TempDir(), "user.creds")
	if err := os.WriteFile(src, []byte("-----BEGIN NATS USER JWT-----\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "credentials")
	dst, err := importCredentialsFile(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dst) != dir {
		t.Fatalf("copy %q is not inside %q", dst, dir)
	}
	if err = os.Remove(src); err != nil { // the copy must not depend on the original
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(data), "NATS USER JWT") {
		t.Fatalf("copy unreadable or wrong: %v", err)
	}
	// Windows has no Unix permission bits; files are protected by ACLs there.
	if info, _ := os.Stat(dst); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %v", info.Mode().Perm())
	}
}

func TestImportCredentialsFileRejectsOversized(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big.creds")
	if err := os.WriteFile(src, make([]byte, maxCredentialsFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importCredentialsFile(src, t.TempDir()); err == nil {
		t.Fatal("expected an error for an oversized file")
	}
}

func TestDiagramIntent(t *testing.T) {
	for _, prompt := range []string{
		"Draw a class diagram with 15 classes",
		"make a flowchart for the deployment",
		"Mind map about our release checklist",
		"Show the architecture of the sync layer",
	} {
		if !diagramIntent(prompt) {
			t.Errorf("expected diagram intent for %q", prompt)
		}
	}
	for _, prompt := range []string{
		"Summarize this board",
		"What is on the canvas?",
		"recolor everything blue",
	} {
		if diagramIntent(prompt) {
			t.Errorf("unexpected diagram intent for %q", prompt)
		}
	}
}

func TestDiagramBudgetError(t *testing.T) {
	// The fifteen-class wall from a real session: 15 boxes plus labels and arrows.
	wall := `{"operations":[` + strings.TrimSuffix(strings.Repeat(`{"op":"add"},`, 45), ",") + `]}`
	if err := diagramBudgetError(wall); err == nil {
		t.Fatal("expected the oversized diagram to be rejected")
	} else if !strings.Contains(err.Error(), "budget is 24") {
		t.Fatalf("unexpected error: %v", err)
	}
	small := `{"operations":[{"op":"add"},{"op":"add"},{"op":"update"}]}`
	if err := diagramBudgetError(small); err != nil {
		t.Fatalf("expected the small diagram to pass, got %v", err)
	}
	// Malformed arguments are left to the apply path to report.
	if err := diagramBudgetError("{"); err != nil {
		t.Fatalf("expected malformed arguments to pass through, got %v", err)
	}
}

func TestIsReplicaError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("nats: replicas > 1 not supported in non-clustered mode"), want: true},
		{err: errors.New("nats: no suitable peers for placement"), want: true},
		{err: errors.New("nats: insufficient resources"), want: true},
		{err: errors.New("nats: stream name already in use"), want: false},
		{err: errors.New("context deadline exceeded"), want: false},
	}
	for _, tc := range cases {
		if got := isReplicaError(tc.err); got != tc.want {
			t.Errorf("isReplicaError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestReplicaCandidates(t *testing.T) {
	cases := []struct {
		name      string
		clustered bool
		want      []int
	}{
		{name: "non-clustered server only hosts one replica", clustered: false, want: []int{1}},
		{name: "clustered server tries three, then two, then one", clustered: true, want: []int{3, 2, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replicaCandidatesFor(tc.clustered)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("replicaCandidatesFor(%v) = %v, want %v", tc.clustered, got, tc.want)
			}
		})
	}
}
