package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/nats-io/nats.go"
)

// FileLocation identifies one pane location or one entry: a local path, or an
// object in a NATS bucket. A pane is either a local directory (Kind "local",
// Path set) or a bucket's object namespace (Kind "nats", Server and Bucket set).
// An entry inside a bucket carries its object name in Name; a local entry keeps
// its full path in Path.
type FileLocation struct {
	Kind   string `json:"kind"`   // "local" | "nats"
	Path   string `json:"path"`   // local: absolute path; nats: ""
	Server string `json:"server"` // nats: configured server id; local: ""
	Bucket string `json:"bucket"` // nats: bucket; local: ""
	Name   string `json:"name"`   // nats: object name; local: ""
}

// FileRoot is one pickable location in a pane's picker.
type FileRoot struct {
	Location FileLocation `json:"location"`
	Label    string       `json:"label"` // "~/Whiteboards" or the bucket name
	Group    string       `json:"group"` // "Local" or the server's display name (optgroup)
}

// FileEntry is one row or card in Morgan le Fayl.
type FileEntry struct {
	Name     string       `json:"name"`
	Location FileLocation `json:"location"`
	IsDir    bool         `json:"isDir"`
	Size     int64        `json:"size"`
	Modified int64        `json:"modified"` // unix seconds
	IsLink   bool         `json:"isLink"`
	Target   string       `json:"target"` // symlink target, display only
	Kind     string       `json:"kind"`   // "folder" | "excalidraw" | "image" | "file"
	Hidden   bool         `json:"hidden"`
}

// FileListing is one pane's directory or bucket listing.
type FileListing struct {
	Location FileLocation  `json:"location"`
	Parent   *FileLocation `json:"parent"` // nil at a local root and always for a bucket
	Entries  []FileEntry   `json:"entries"`
	Roots    []FileRoot    `json:"roots"`
}

// FileConflict is a source file whose name already exists in the target.
type FileConflict struct {
	Source FileLocation `json:"source"`
	Name   string       `json:"name"`
	Target string       `json:"target"` // display-only description of what is in the way
}

// FileAction is the user's decision for one colliding source path.
type FileAction struct {
	Source FileLocation `json:"source"`
	Action string       `json:"action"` // "skip" | "overwrite" | "rename"
}

// FileTransferRequest moves or copies a selection into a target.
type FileTransferRequest struct {
	Sources []FileLocation `json:"sources"`
	Target  FileLocation   `json:"target"` // a directory or a bucket
	Copy    bool           `json:"copy"`
	Actions []FileAction   `json:"actions"` // optional; missing entries on a collision default to "skip"
}

// FileTransferResult is the outcome for one source path.
type FileTransferResult struct {
	Source FileLocation  `json:"source"`
	Target *FileLocation `json:"target"` // nil when skipped
	Name   string        `json:"name"`
	Action string        `json:"action"` // "moved" | "copied" | "skipped" | "overwritten" | "renamed"
	Error  string        `json:"error"`
}

// errOutsideBoards is the single, clear error every confined operation returns
// when a path is not inside one of the configured boards folders.
var errOutsideBoards = errors.New("that path is outside the boards folders")

// maxThumbCacheEntries bounds the in-memory thumbnail cache. When it is
// exceeded the whole map is dropped rather than tracking LRU order, because
// thumbnails are cheap to rebuild and the simple eviction keeps the code small.
const maxThumbCacheEntries = 256

// maxThumbImageBytes caps how large an image may be before it is turned into a
// base64 data URL, so a huge file cannot exhaust memory as a preview string.
const maxThumbImageBytes = 12 * 1024 * 1024

// thumbCacheEntry is one cached preview, keyed by location and invalidated by
// the file's mtime and size so an edited file gets a fresh thumbnail.
type thumbCacheEntry struct {
	mtime   int64
	size    int64
	dataURL string
}

// rootForPath maps an absolute path to the canonical resolved path that lives
// inside one of the configured boards folders. It resolves symlinks on the raw
// path first (before any lexical cleaning) so a "<root>/link/../x" escape is
// caught: the kernel resolves link/.. against the symlink target, which
// filepath.Clean would collapse away and let through. It returns the resolved
// path so callers operate on the same path the filesystem will, and it accepts
// paths under a symlinked boards folder's resolved target so a symlinked folder
// still works.
func (a *App) rootForPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errOutsideBoards
	}
	resolved, err := resolveExisting(path)
	if err != nil {
		return "", err
	}
	if a.rootForResolved(resolved) == "" {
		return "", errOutsideBoards
	}
	return resolved, nil
}

// resolveExisting returns the canonical path for path. It resolves symlinks on
// the raw path (before any lexical cleaning) so a "link/.." escape is caught:
// filepath.Clean collapses link/.. to nothing, but the kernel resolves it
// against the symlink target. When the path does not exist yet (a folder about
// to be created, a rename target) it resolves the deepest existing ancestor and
// re-appends the remaining components. A path that goes through a broken symlink
// is rejected, because the kernel would still follow the link and we cannot know
// where it leads.
func resolveExisting(path string) (string, error) {
	// Fast path: the whole path exists, so EvalSymlinks resolves it in the
	// correct order (a symlink component is resolved before a following "..").
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	// The path does not exist yet. Walk it component by component, resolving
	// symlinks in order, until a component does not exist; then re-append the
	// remaining components. We cannot use filepath.Dir to walk up, because it
	// cleans the path and would collapse a "link/.." before it is resolved.
	sep := string(os.PathSeparator)
	components := strings.Split(path, sep)
	resolved := ""
	if len(components) > 0 && components[0] == "" {
		resolved = sep
	}
	var rest []string
	for i := 0; i < len(components); i++ {
		comp := components[i]
		if comp == "" || comp == "." {
			continue
		}
		if comp == ".." {
			// Go up one level in the resolved path. The resolved path is fully
			// resolved (no symlinks left), so this is safe; at the root ".."
			// stays put.
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, comp)
		if r, err := filepath.EvalSymlinks(candidate); err == nil {
			resolved = r
			continue
		}
		// The component does not exist. If it is a symlink, the kernel would
		// still follow it, so the path is not safe to use even though it cannot
		// be resolved here.
		if info, lerr := os.Lstat(candidate); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errOutsideBoards
		}
		rest = append(rest, comp)
		rest = append(rest, components[i+1:]...)
		break
	}
	for _, comp := range rest {
		if comp == "" || comp == "." {
			continue
		}
		resolved = filepath.Join(resolved, comp)
	}
	return filepath.Clean(resolved), nil
}

// rootForResolved returns the resolved boards folder that contains the given
// already-resolved path, or "" when it is outside every root. It is the inverse
// of rootForPath: after resolving a path we still need to know which root it
// lives under for the listing. The resolved form is returned so it matches the
// resolved Path the listing reports.
func (a *App) rootForResolved(resolved string) string {
	best := ""
	for _, root := range a.store.localFolders {
		if a.pathWithinRoot(resolved, root) {
			resolvedRoot := root
			if r, err := filepath.EvalSymlinks(root); err == nil {
				resolvedRoot = r
			}
			if len(resolvedRoot) > len(best) {
				best = resolvedRoot
			}
		}
	}
	return best
}

// isRoot reports whether the resolved path is exactly one of the configured
// boards folders (or its resolved target). Mutating a root itself would remove
// or rename the folder the app is configured to watch, so it is refused.
func (a *App) isRoot(resolved string) bool {
	for _, root := range a.store.localFolders {
		if resolved == root {
			return true
		}
		if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil && resolved == resolvedRoot {
			return true
		}
	}
	return false
}

// pathWithinRoot reports whether path lives under root, either directly or
// under the root's resolved symlink target (so a symlinked boards folder still
// works even when the OS reports the resolved path).
func (a *App) pathWithinRoot(path, root string) bool {
	if withinRoot(path, root) {
		return true
	}
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil && resolvedRoot != root {
		return withinRoot(path, resolvedRoot)
	}
	return false
}

func withinRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

// validateLocation checks that a location is well formed and, for a NATS
// location, that the server and bucket are configured. The webview is never
// trusted, so a Kind that is neither "local" nor "nats" is rejected outright.
func (a *App) validateLocation(loc FileLocation) error {
	switch loc.Kind {
	case "local":
		if loc.Path == "" {
			return errors.New("a local location needs a path")
		}
		return nil
	case "nats":
		if loc.Server == "" || loc.Bucket == "" {
			return errors.New("a NATS location needs a server and a bucket")
		}
		for _, server := range a.store.prefs.NatsServers {
			if server.ID == loc.Server {
				if !slices.Contains(server.Buckets, loc.Bucket) {
					return fmt.Errorf("bucket %q is not configured on server %q", loc.Bucket, loc.Server)
				}
				return nil
			}
		}
		return fmt.Errorf("unknown NATS server %q", loc.Server)
	default:
		return fmt.Errorf("invalid location kind %q", loc.Kind)
	}
}

// validateSource validates a transfer source and, for a local source, resolves
// it and refuses a boards folder itself (which cannot be moved or copied).
func (a *App) validateSource(loc FileLocation) (string, error) {
	if err := a.validateLocation(loc); err != nil {
		return "", err
	}
	if loc.Kind != "local" {
		return "", nil
	}
	resolved, err := a.rootForPath(loc.Path)
	if err != nil {
		return "", err
	}
	if a.isRoot(resolved) {
		return "", errors.New("can't move or copy a boards folder")
	}
	return resolved, nil
}

// validateTarget validates a transfer target and, for a local target, resolves
// it. A target may be a boards folder itself (you can move files into it).
func (a *App) validateTarget(loc FileLocation) (string, error) {
	if err := a.validateLocation(loc); err != nil {
		return "", err
	}
	if loc.Kind != "local" {
		return "", nil
	}
	return a.rootForPath(loc.Path)
}

// openBucket opens an existing object-store bucket without creating it, so a
// read-only operation (a collision check) does not create a bucket as a side
// effect. The write path uses writeObject, which creates the bucket on demand.
func (a *App) openBucket(client *natsClient, bucket string) (nats.ObjectStore, error) {
	store, err := client.js.ObjectStore(bucket)
	if err != nil {
		return nil, fmt.Errorf("could not open bucket %q: %w", bucket, err)
	}
	return store, nil
}

// fileRoots returns every pickable location for a pane's picker: the configured
// local folders (group "Local") and, for each configured NATS server, its
// buckets (group = the server's display name). Nothing is cached, so a dead
// server only affects listing its bucket, never the picker.
func (a *App) fileRoots() []FileRoot {
	roots := make([]FileRoot, 0, len(a.store.localFolders)+len(a.store.prefs.NatsServers))
	for _, folder := range a.store.localFolders {
		resolved := folder
		if r, err := filepath.EvalSymlinks(folder); err == nil {
			resolved = r
		}
		roots = append(roots, FileRoot{
			Location: FileLocation{Kind: "local", Path: resolved},
			Label:    displayPath(folder),
			Group:    "Local",
		})
	}
	for _, server := range a.store.prefs.NatsServers {
		for _, bucket := range server.Buckets {
			roots = append(roots, FileRoot{
				Location: FileLocation{Kind: "nats", Server: server.ID, Bucket: bucket},
				Label:    bucket,
				Group:    server.Name,
			})
		}
	}
	return roots
}

// fileKind classifies an entry for the UI. Folders are always "folder"; the
// rest are classified by extension so the grid can pick a thumbnail or badge.
func fileKind(entry FileEntry) string {
	if entry.IsDir {
		return "folder"
	}
	switch strings.ToLower(filepath.Ext(entry.Name)) {
	case ".excalidraw":
		return "excalidraw"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".avif":
		return "image"
	default:
		return "file"
	}
}

// fileEntryFor builds one FileEntry from a directory entry and its Lstat info.
// A symlink is followed only to report whether it points at a directory (so the
// UI can navigate into it and sort it with the folders) and to surface its
// target for display; the entry itself is never dereferenced for confinement.
func fileEntryFor(loc FileLocation, name string, info os.FileInfo) FileEntry {
	entry := FileEntry{
		Name:     name,
		Location: loc,
		Size:     info.Size(),
		Modified: info.ModTime().Unix(),
		IsLink:   info.Mode()&os.ModeSymlink != 0,
		Hidden:   strings.HasPrefix(name, "."),
	}
	if entry.IsLink {
		if target, err := filepath.EvalSymlinks(loc.Path); err == nil {
			entry.Target = target
		}
		if targetInfo, err := os.Stat(loc.Path); err == nil {
			entry.IsDir = targetInfo.IsDir()
			if !entry.IsDir {
				entry.Size = targetInfo.Size()
				entry.Modified = targetInfo.ModTime().Unix()
			}
		}
	} else {
		entry.IsDir = info.IsDir()
	}
	entry.Kind = fileKind(entry)
	return entry
}

// natsEntryFor builds one FileEntry from a bucket object. Bucket objects are
// never directories, so IsDir is always false and the kind comes from the name.
func natsEntryFor(loc FileLocation, info *nats.ObjectInfo) FileEntry {
	entry := FileEntry{
		Name:     info.Name,
		Location: loc,
		Size:     int64(info.Size),
		Modified: info.ModTime.Unix(),
		Hidden:   strings.HasPrefix(info.Name, "."),
	}
	entry.Kind = fileKind(entry)
	return entry
}

// sortFileEntries puts folders first, then a case-insensitive name sort, which
// matches the sidebar's localeCompare(..., {sensitivity:'base'}) spirit.
func sortFileEntries(entries []FileEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

// ListFiles lists one directory inside the configured boards folders, or one
// NATS bucket's objects.
func (a *App) ListFiles(location FileLocation) (*FileListing, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return nil, errors.New("store is not ready")
	}
	if err := a.validateLocation(location); err != nil {
		return nil, err
	}
	listing := &FileListing{Roots: a.fileRoots()}
	if location.Kind == "local" {
		resolved, err := a.rootForPath(location.Path)
		if err != nil {
			return nil, err
		}
		listing.Location = FileLocation{Kind: "local", Path: resolved}
		root := a.rootForResolved(resolved)
		if resolved != root {
			listing.Parent = &FileLocation{Kind: "local", Path: filepath.Dir(resolved)}
		}
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			entryPath := filepath.Join(resolved, entry.Name())
			info, err := os.Lstat(entryPath)
			if err != nil {
				// The entry vanished between ReadDir and Lstat; skip it and keep
				// going so a racing change does not fail the whole listing.
				continue
			}
			listing.Entries = append(listing.Entries, fileEntryFor(FileLocation{Kind: "local", Path: entryPath}, entry.Name(), info))
		}
		sortFileEntries(listing.Entries)
		return listing, nil
	}
	// NATS bucket listing.
	client, err := a.store.clientFor("nats:" + location.Server)
	if err != nil {
		return nil, err
	}
	listing.Location = FileLocation{Kind: "nats", Server: location.Server, Bucket: location.Bucket}
	objects, err := client.listObjects(location.Bucket)
	if err != nil {
		return nil, err
	}
	for _, object := range objects {
		if object == nil || object.Deleted {
			continue
		}
		listing.Entries = append(listing.Entries, natsEntryFor(FileLocation{Kind: "nats", Server: location.Server, Bucket: location.Bucket, Name: object.Name}, object))
	}
	sortFileEntries(listing.Entries)
	return listing, nil
}

// FileThumbnail returns a data URL preview for a file, or "" when it has none.
// .excalidraw reuses the renderer's exportToCanvas round-trip; images are
// base64 data URLs. Results are cached in memory by location+mtime+size.
func (a *App) FileThumbnail(location FileLocation) (string, error) {
	a.mu.Lock()
	if a.store == nil {
		a.mu.Unlock()
		return "", errors.New("store is not ready")
	}
	if err := a.validateLocation(location); err != nil {
		a.mu.Unlock()
		return "", err
	}
	var (
		key   string
		mtime int64
		size  int64
		build func() string
	)
	if location.Kind == "local" {
		resolved, err := a.rootForPath(location.Path)
		if err != nil {
			a.mu.Unlock()
			return "", err
		}
		info, err := os.Lstat(resolved)
		if err != nil {
			a.mu.Unlock()
			return "", err
		}
		if info.IsDir() {
			a.mu.Unlock()
			return "", nil
		}
		key = "local:" + resolved
		mtime = info.ModTime().UnixNano()
		size = info.Size()
		build = func() string { return a.fileThumbnailBuild(resolved, info) }
	} else {
		client, err := a.store.clientFor("nats:" + location.Server)
		if err != nil {
			a.mu.Unlock()
			return "", err
		}
		store, err := a.openBucket(client, location.Bucket)
		if err != nil {
			a.mu.Unlock()
			return "", err
		}
		info, err := store.GetInfo(location.Name)
		if err != nil {
			a.mu.Unlock()
			return "", err
		}
		key = "nats:" + location.Server + ":" + location.Bucket + ":" + location.Name
		mtime = info.ModTime.UnixNano()
		size = int64(info.Size)
		build = func() string { return a.natsThumbnailBuild(client, location, info) }
	}
	a.mu.Unlock()

	a.thumbMu.Lock()
	if cached, ok := a.thumbCache[key]; ok && cached.mtime == mtime && cached.size == size {
		dataURL := cached.dataURL
		a.thumbMu.Unlock()
		return dataURL, nil
	}
	a.thumbMu.Unlock()

	// Build the preview outside the store lock: the renderer round-trip can
	// take up to 10 seconds and must not stall other store operations.
	dataURL := build()

	a.thumbMu.Lock()
	if len(a.thumbCache) >= maxThumbCacheEntries {
		a.thumbCache = map[string]thumbCacheEntry{}
	}
	a.thumbCache[key] = thumbCacheEntry{mtime: mtime, size: size, dataURL: dataURL}
	a.thumbMu.Unlock()
	return dataURL, nil
}

// fileThumbnailBuild produces the preview for one local file. Missing
// .excalidraw scenes that render to nothing return "" and are cached too, so
// the renderer round-trip is not retried in a loop for the same unchanged file.
func (a *App) fileThumbnailBuild(path string, info os.FileInfo) string {
	switch fileKind(FileEntry{Name: filepath.Base(path), IsDir: info.IsDir()}) {
	case "excalidraw":
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return a.renderThumbnail(string(data))
	case "image":
		// Follow symlinks for the size cap so a link to a huge image is still
		// bounded; the Lstat size would only be the link's own length.
		size := info.Size()
		if targetInfo, err := os.Stat(path); err == nil {
			size = targetInfo.Size()
		}
		if size > maxThumbImageBytes {
			return ""
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return "data:" + mimeForPath(path) + ";base64," + base64.StdEncoding.EncodeToString(data)
	default:
		return ""
	}
}

// natsThumbnailBuild produces the preview for one bucket object. The renderer
// round-trip for .excalidraw is the same one the local path uses.
func (a *App) natsThumbnailBuild(client *natsClient, location FileLocation, info *nats.ObjectInfo) string {
	switch fileKind(FileEntry{Name: location.Name}) {
	case "excalidraw":
		data, err := client.readObject(location.Bucket, location.Name)
		if err != nil {
			return ""
		}
		return a.renderThumbnail(string(data))
	case "image":
		if info.Size > maxThumbImageBytes {
			return ""
		}
		data, err := client.readObject(location.Bucket, location.Name)
		if err != nil {
			return ""
		}
		return "data:" + mimeForPath(location.Name) + ";base64," + base64.StdEncoding.EncodeToString(data)
	default:
		return ""
	}
}

// sourceName returns the base name of a source for collision reporting.
func sourceName(src FileLocation) string {
	if src.Kind == "local" {
		return filepath.Base(src.Path)
	}
	return src.Name
}

// describeTarget renders a human-readable description of where a source would
// land, for the conflict dialog.
func describeTarget(target FileLocation, targetResolved, name string) string {
	if target.Kind == "local" {
		return filepath.Join(targetResolved, name)
	}
	return target.Bucket + "/" + name
}

// targetExists reports whether a name already exists in the target. For a
// bucket target a missing bucket is treated as "no objects", so a transfer into
// a not-yet-created bucket is free rather than an error.
func (a *App) targetExists(target FileLocation, targetResolved, name string) (bool, error) {
	if target.Kind == "local" {
		_, err := os.Lstat(filepath.Join(targetResolved, name))
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	client, err := a.store.clientFor("nats:" + target.Server)
	if err != nil {
		return false, err
	}
	store, err := a.openBucket(client, target.Bucket)
	if err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			return false, nil
		}
		return false, err
	}
	_, err = store.GetInfo(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, nats.ErrObjectNotFound) {
		return false, nil
	}
	return false, err
}

// nextFreeBucketName returns the next free "name 2.ext", "name 3.ext", ... for
// an object name that already exists, keeping the original extension.
func nextFreeBucketName(store nats.ObjectStore, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s %d%s", stem, n, ext)
		_, err := store.GetInfo(candidate)
		if errors.Is(err, nats.ErrObjectNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// resolveCollision determines the action and final name for a source name in a
// target. An empty action means there is no collision. A missing action on a
// collision defaults to "skip", and an unknown action is treated as "skip".
func (a *App) resolveCollision(target FileLocation, targetResolved, name, action string) (string, string, error) {
	exists, err := a.targetExists(target, targetResolved, name)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return "", name, nil
	}
	if action == "" {
		action = "skip"
	}
	switch action {
	case "skip":
		return "skip", name, nil
	case "overwrite":
		return "overwrite", name, nil
	case "rename":
		if target.Kind == "local" {
			candidate, err := nextFreeName(filepath.Join(targetResolved, name))
			if err != nil {
				return "", "", err
			}
			return "rename", filepath.Base(candidate), nil
		}
		client, err := a.store.clientFor("nats:" + target.Server)
		if err != nil {
			return "", "", err
		}
		store, err := a.openBucket(client, target.Bucket)
		if err != nil {
			return "", "", err
		}
		candidate, err := nextFreeBucketName(store, name)
		if err != nil {
			return "", "", err
		}
		return "rename", candidate, nil
	default:
		return "skip", name, nil
	}
}

// CheckFileTransfer reports which of the sources would collide in the target.
// It only checks for existing targets and never writes anything.
func (a *App) CheckFileTransfer(sources []FileLocation, target FileLocation) ([]FileConflict, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return nil, errors.New("store is not ready")
	}
	if err := a.validateLocation(target); err != nil {
		return nil, err
	}
	for _, src := range sources {
		if _, err := a.validateSource(src); err != nil {
			return nil, err
		}
	}
	targetResolved, err := a.validateTarget(target)
	if err != nil {
		return nil, err
	}
	conflicts := make([]FileConflict, 0)
	for _, src := range sources {
		name := sourceName(src)
		exists, err := a.targetExists(target, targetResolved, name)
		if err != nil {
			return nil, err
		}
		if exists {
			conflicts = append(conflicts, FileConflict{Source: src, Name: name, Target: describeTarget(target, targetResolved, name)})
		}
	}
	return conflicts, nil
}

// TransferFiles moves or copies sources into the target, applying Actions on
// collisions. Every source and the target are validated before anything is
// touched, and one failing path never aborts the batch.
func (a *App) TransferFiles(request FileTransferRequest) ([]FileTransferResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return nil, errors.New("store is not ready")
	}
	targetResolved, err := a.validateTarget(request.Target)
	if err != nil {
		return nil, err
	}
	// Resolve every source up front; nothing is touched before all validation
	// passes, and a source that is itself a boards folder is refused.
	sourceResolved := make([]string, len(request.Sources))
	for i, src := range request.Sources {
		resolved, err := a.validateSource(src)
		if err != nil {
			return nil, err
		}
		sourceResolved[i] = resolved
	}
	actions := make(map[FileLocation]string, len(request.Actions))
	for _, action := range request.Actions {
		actions[action.Source] = action.Action
	}
	results := make([]FileTransferResult, 0, len(request.Sources))
	for i, src := range request.Sources {
		results = append(results, a.transferOne(src, sourceResolved[i], request.Target, targetResolved, request.Copy, actions))
	}
	return results, nil
}

// transferOne dispatches a single transfer to the right implementation for the
// source/target kind pair. It never aborts the batch: failures are recorded on
// the result and the next source is still processed.
func (a *App) transferOne(src FileLocation, srcResolved string, target FileLocation, targetResolved string, copy bool, actions map[FileLocation]string) FileTransferResult {
	result := FileTransferResult{Source: src}
	switch {
	case src.Kind == "local" && target.Kind == "local":
		return a.transferLocalToLocal(src, srcResolved, target, targetResolved, copy, actions, result)
	case src.Kind == "local" && target.Kind == "nats":
		return a.transferLocalToNATS(src, srcResolved, target, copy, actions, result)
	case src.Kind == "nats" && target.Kind == "local":
		return a.transferNATSToLocal(src, target, targetResolved, copy, actions, result)
	case src.Kind == "nats" && target.Kind == "nats":
		return a.transferNATSToNATS(src, target, copy, actions, result)
	default:
		result.Error = "unsupported transfer"
		return result
	}
}

// actionLabel maps a collision action and the copy flag to the result action.
func actionLabel(action string, copy bool) string {
	switch {
	case action == "overwrite":
		return "overwritten"
	case action == "rename":
		return "renamed"
	case copy:
		return "copied"
	default:
		return "moved"
	}
}

// transferLocalToLocal moves or copies a local source into a local directory,
// honouring the collision action. It is the original local-only transfer path.
func (a *App) transferLocalToLocal(src FileLocation, srcResolved string, target FileLocation, targetResolved string, copy bool, actions map[FileLocation]string, result FileTransferResult) FileTransferResult {
	if err := rejectSelfMove(srcResolved, targetResolved); err != nil {
		result.Error = err.Error()
		return result
	}
	name := filepath.Base(src.Path)
	action, finalName, err := a.resolveCollision(target, targetResolved, name, actions[src])
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if action == "skip" {
		result.Action = "skipped"
		result.Name = name
		return result
	}
	targetPath := filepath.Join(targetResolved, finalName)
	if action == "overwrite" {
		// Trash the existing target first so nothing is lost if the transfer
		// fails afterwards; the old file is recoverable from the OS Trash.
		if err := moveToTrash(targetPath); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	// Pass the original path to transferPath, not the resolved one, so a
	// symlink is recreated as a link rather than dereferenced into its target.
	// The source was already validated to resolve inside a boards folder, so
	// the kernel resolving it during the operation cannot escape the root.
	if err := transferPath(src.Path, targetPath, copy); err != nil {
		result.Error = err.Error()
		return result
	}
	result.Target = &FileLocation{Kind: "local", Path: targetPath}
	result.Name = filepath.Base(targetPath)
	result.Action = actionLabel(action, copy)
	return result
}

// transferLocalToNATS copies a local file into a bucket. Directories are
// refused because a bucket has no folder structure. A moved source goes to the
// OS Trash, so it stays recoverable.
func (a *App) transferLocalToNATS(src FileLocation, srcResolved string, target FileLocation, copy bool, actions map[FileLocation]string, result FileTransferResult) FileTransferResult {
	// os.Stat (not Lstat) follows a symlink, so a link to a directory is
	// refused here rather than failing later as "is a directory".
	info, err := os.Stat(srcResolved)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if info.IsDir() {
		result.Error = "folders can't be copied into a bucket"
		return result
	}
	name := filepath.Base(src.Path)
	action, finalName, err := a.resolveCollision(target, "", name, actions[src])
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if action == "skip" {
		result.Action = "skipped"
		result.Name = name
		return result
	}
	client, err := a.store.clientFor("nats:" + target.Server)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	data, err := os.ReadFile(srcResolved)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	// Overwriting a bucket object simply puts the new object over it; there is
	// no Trash in a bucket, so nothing is kept.
	if err := client.writeObject(target.Bucket, finalName, data); err != nil {
		result.Error = err.Error()
		return result
	}
	// If the source is a .excalidraw board, mint a fresh document id so the
	// copy does not inherit the source's collaboration history, exactly like
	// Store.copyBoardToSource does.
	if strings.HasSuffix(finalName, fileExtension) {
		documentID, err := newDocumentID()
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if err := client.setDocumentID(target.Bucket, finalName, documentID); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	if !copy {
		if err := moveToTrash(srcResolved); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	result.Target = &FileLocation{Kind: "nats", Server: target.Server, Bucket: target.Bucket, Name: finalName}
	result.Name = finalName
	result.Action = actionLabel(action, copy)
	return result
}

// transferNATSToLocal copies a bucket object into a local directory. A move
// deletes the source object permanently (there is no Trash in a bucket) and
// best-effort deletes the source's document sidecar.
func (a *App) transferNATSToLocal(src FileLocation, target FileLocation, targetResolved string, copy bool, actions map[FileLocation]string, result FileTransferResult) FileTransferResult {
	client, err := a.store.clientFor("nats:" + src.Server)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	data, err := client.readObject(src.Bucket, src.Name)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if data == nil {
		result.Error = "object not found"
		return result
	}
	name := src.Name
	action, finalName, err := a.resolveCollision(target, targetResolved, name, actions[src])
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if action == "skip" {
		result.Action = "skipped"
		result.Name = name
		return result
	}
	targetPath := filepath.Join(targetResolved, finalName)
	if action == "overwrite" {
		// Trash the existing target first so nothing is lost if the transfer
		// fails afterwards; the old file is recoverable from the OS Trash.
		if err := moveToTrash(targetPath); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	if err := atomicWrite(targetPath, data); err != nil {
		result.Error = err.Error()
		return result
	}
	if !copy {
		// A move deletes the source object permanently; there is no Trash in a
		// bucket. Best-effort delete the source's document sidecar so a stale
		// collaboration id does not survive the object.
		if err := client.deleteObject(src.Bucket, src.Name); err != nil {
			result.Error = err.Error()
			return result
		}
		_ = client.deleteObject(src.Bucket, documentMetaName(src.Name))
	}
	result.Target = &FileLocation{Kind: "local", Path: targetPath}
	result.Name = filepath.Base(targetPath)
	result.Action = actionLabel(action, copy)
	return result
}

// transferNATSToNATS copies a bucket object into another bucket. On move the
// source's document id is carried across so the collaboration log follows the
// board; on copy a fresh id is minted so the copy starts its own history. Same
// or different servers both work (read from one client, write through the
// other).
func (a *App) transferNATSToNATS(src FileLocation, target FileLocation, copy bool, actions map[FileLocation]string, result FileTransferResult) FileTransferResult {
	srcClient, err := a.store.clientFor("nats:" + src.Server)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	targetClient, err := a.store.clientFor("nats:" + target.Server)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	data, err := srcClient.readObject(src.Bucket, src.Name)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if data == nil {
		result.Error = "object not found"
		return result
	}
	name := src.Name
	action, finalName, err := a.resolveCollision(target, "", name, actions[src])
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if action == "skip" {
		result.Action = "skipped"
		result.Name = name
		return result
	}
	// Transferring an object onto itself (same bucket, same name) ends with the
	// source delete removing the copy that was just written, so refuse it rather
	// than destroy the board. A copy inside the same bucket is still allowed when
	// the collision dialog renamed it.
	if src.Server == target.Server && src.Bucket == target.Bucket && finalName == src.Name {
		result.Error = "that object is already in this bucket"
		return result
	}
	// Overwriting a bucket object simply puts the new object over it; there is
	// no Trash in a bucket, so nothing is kept.
	if err := targetClient.writeObject(target.Bucket, finalName, data); err != nil {
		result.Error = err.Error()
		return result
	}
	if strings.HasSuffix(finalName, fileExtension) {
		if copy {
			documentID, err := newDocumentID()
			if err != nil {
				result.Error = err.Error()
				return result
			}
			if err := targetClient.setDocumentID(target.Bucket, finalName, documentID); err != nil {
				result.Error = err.Error()
				return result
			}
		} else {
			documentID, err := srcClient.documentID(src.Bucket, src.Name)
			if err != nil {
				result.Error = err.Error()
				return result
			}
			if err := targetClient.setDocumentID(target.Bucket, finalName, documentID); err != nil {
				result.Error = err.Error()
				return result
			}
		}
	}
	if !copy {
		if err := srcClient.deleteObject(src.Bucket, src.Name); err != nil {
			result.Error = err.Error()
			return result
		}
		_ = srcClient.deleteObject(src.Bucket, documentMetaName(src.Name))
	}
	result.Target = &FileLocation{Kind: "nats", Server: target.Server, Bucket: target.Bucket, Name: finalName}
	result.Name = finalName
	result.Action = actionLabel(action, copy)
	return result
}

// rejectSelfMove refuses to move or copy a directory into itself or one of its
// own descendants, which would otherwise recurse without ever finishing.
func rejectSelfMove(src, targetDir string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	cleanSrc := filepath.Clean(src)
	cleanTarget := filepath.Clean(targetDir)
	if cleanSrc == cleanTarget || strings.HasPrefix(cleanTarget, cleanSrc+string(os.PathSeparator)) {
		return fmt.Errorf("cannot move or copy a folder into itself")
	}
	return nil
}

// transferPath moves or copies one file or directory. A move uses os.Rename and
// falls back to copy+delete on EXDEV (cross-device), so moving between two
// boards folders on different filesystems still works.
func transferPath(src, dst string, copy bool) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return transferSymlink(src, dst, copy)
	}
	if info.IsDir() {
		if !copy {
			if err := os.Rename(src, dst); err == nil {
				return nil
			} else if !errors.Is(err, syscall.EXDEV) {
				return err
			}
		}
		return transferDir(src, dst, copy, info)
	}
	if !copy {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFileContents(src, dst, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Remove(src)
	}
	return copyFileContents(src, dst, info.Mode().Perm())
}

// transferSymlink recreates a symlink at dst. A move renames the link itself
// when possible (same filesystem) and otherwise recreates it and removes the
// source; a copy always recreates it. Copying a symlink to a directory would
// otherwise try to read the target as a file and fail.
func transferSymlink(src, dst string, copy bool) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if !copy {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EXDEV) {
			return err
		}
	}
	if err := os.Symlink(target, dst); err != nil {
		return err
	}
	if !copy {
		return os.Remove(src)
	}
	return nil
}

// transferDir recursively copies or moves a directory tree, then removes the
// source when moving. The source directory mode is preserved on the copy.
func transferDir(src, dst string, copy bool, info os.FileInfo) error {
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := transferPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), copy); err != nil {
			return err
		}
	}
	if !copy {
		return os.Remove(src)
	}
	return nil
}

// copyFileContents copies one regular file with a small buffered loop,
// preserving the source's permission bits.
func copyFileContents(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				out.Close()
				return writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			out.Close()
			return readErr
		}
	}
	return out.Close()
}

// nextFreeName returns the next free "name 2.ext", "name 3.ext", ... for a
// target that already exists, keeping the original extension. It returns an
// error when Lstat fails for a reason other than the candidate not existing
// (permission denied, a symlink loop), so the caller does not loop forever.
func nextFreeName(target string) (string, error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 2; ; n++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s %d%s", stem, n, ext))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

// validateFileName checks a rename target. Separators, "." and ".." are
// rejected, and a leading period is only allowed when the file already is
// hidden so renaming cannot hide or unhide an entry.
func validateFileName(name, currentPath string) error {
	if name == "" {
		return errors.New("enter a name")
	}
	if name == "." || name == ".." {
		return errors.New("that name is not allowed")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("names can't contain a path separator")
	}
	if strings.HasPrefix(name, ".") && !strings.HasPrefix(filepath.Base(currentPath), ".") {
		return errors.New("names can't start with a period")
	}
	return nil
}

// validateFolderName checks a new folder name. It is stricter than a rename
// because a brand-new hidden folder would be invisible by default.
func validateFolderName(name string) error {
	if name == "" {
		return errors.New("enter a name")
	}
	if name == "." || name == ".." {
		return errors.New("that name is not allowed")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("names can't contain a path separator")
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("names can't start with a period")
	}
	return nil
}

// RenameFilePath renames one file or folder in place. It rejects separators,
// ".", "..", names starting with a period (unless the file already is hidden),
// and existing names. For a bucket object it mirrors Store.renameBoard's remote
// branch: read, write the new name, carry the document id across, delete the
// old name.
func (a *App) RenameFilePath(location FileLocation, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	if err := a.validateLocation(location); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if location.Kind == "local" {
		resolved, err := a.rootForPath(location.Path)
		if err != nil {
			return err
		}
		if a.isRoot(resolved) {
			return errors.New("that is a boards folder and can't be renamed")
		}
		if err := validateFileName(name, resolved); err != nil {
			return err
		}
		target := filepath.Join(filepath.Dir(resolved), name)
		if _, err := os.Lstat(target); err == nil {
			return errors.New("a file or folder with that name already exists")
		}
		return os.Rename(resolved, target)
	}
	// NATS bucket object rename.
	client, err := a.store.clientFor("nats:" + location.Server)
	if err != nil {
		return err
	}
	if err := validateFileName(name, location.Name); err != nil {
		return err
	}
	if name == location.Name {
		return nil
	}
	// A bucket object may not be renamed to an existing object name.
	store, err := a.openBucket(client, location.Bucket)
	if err != nil {
		return err
	}
	if _, err := store.GetInfo(name); err == nil {
		return errors.New("an object with that name already exists")
	} else if !errors.Is(err, nats.ErrObjectNotFound) {
		return err
	}
	data, err := client.readObject(location.Bucket, location.Name)
	if err != nil {
		return err
	}
	if data == nil {
		return errors.New("object not found")
	}
	if err := client.writeObject(location.Bucket, name, data); err != nil {
		return err
	}
	if strings.HasSuffix(location.Name, fileExtension) {
		documentID, err := client.documentID(location.Bucket, location.Name)
		if err != nil {
			return err
		}
		if err := client.setDocumentID(location.Bucket, name, documentID); err != nil {
			return err
		}
	}
	if err := client.deleteObject(location.Bucket, location.Name); err != nil {
		return err
	}
	_ = client.deleteObject(location.Bucket, documentMetaName(location.Name))
	return nil
}

// NewFolder creates one folder inside dir. Buckets have no folder structure, so
// a NATS location is refused.
func (a *App) NewFolder(location FileLocation, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	if err := a.validateLocation(location); err != nil {
		return err
	}
	if location.Kind != "local" {
		return errors.New("buckets have no folders")
	}
	resolved, err := a.rootForPath(location.Path)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if err := validateFolderName(name); err != nil {
		return err
	}
	target := filepath.Join(resolved, name)
	if _, err := os.Lstat(target); err == nil {
		return errors.New("a file or folder with that name already exists")
	}
	return os.Mkdir(target, 0o755)
}

// TrashFilePath moves one file or folder to the OS Trash using the existing
// moveToTrash helper, never os.Remove. For a bucket object it is a permanent
// delete, because there is no Trash in a bucket.
func (a *App) TrashFilePath(location FileLocation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	if err := a.validateLocation(location); err != nil {
		return err
	}
	if location.Kind == "local" {
		resolved, err := a.rootForPath(location.Path)
		if err != nil {
			return err
		}
		if a.isRoot(resolved) {
			return errors.New("that is a boards folder and can't be trashed")
		}
		return moveToTrash(resolved)
	}
	client, err := a.store.clientFor("nats:" + location.Server)
	if err != nil {
		return err
	}
	if err := client.deleteObject(location.Bucket, location.Name); err != nil {
		return err
	}
	_ = client.deleteObject(location.Bucket, documentMetaName(location.Name))
	return nil
}

// RefreshBoards rescans the active source so the sidebar reflects files Morgan
// le Fayl touched. It takes the store lock the same way the existing rescan and
// emit handlers do. Note that store.scan() only rescans the active group, so
// changes made in a non-active root appear in the sidebar only when that group
// is activated.
func (a *App) RefreshBoards() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return errors.New("store is not ready")
	}
	if err := a.store.scan(); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}
