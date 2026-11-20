package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testFileManagerApp builds an App wired to a fresh test store, the same way
// the other App tests do. The store is cleaned up by testStore's t.Cleanup.
func testFileManagerApp(t *testing.T) *App {
	t.Helper()
	app := newApp()
	app.store = testStore(t)
	return app
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// localLoc builds a local FileLocation for a path.
func localLoc(path string) FileLocation {
	return FileLocation{Kind: "local", Path: path}
}

// TestFileManagerListFiles checks the listing shape: folders first then a
// case-insensitive name sort, kinds, the parent at the root and in a
// subdirectory, and that hidden entries are marked so the UI can filter them.
func TestFileManagerListFiles(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(root, "zeta.txt"), "z")
	mustWriteFile(t, filepath.Join(root, "alpha.excalidraw"), `{"type":"excalidraw","elements":[]}`)
	mustWriteFile(t, filepath.Join(root, ".secret"), "hidden")
	mustWriteFile(t, filepath.Join(root, "image.png"), "png")
	mustMkdir(t, filepath.Join(root, "folder"))

	listing, err := app.ListFiles(localLoc(root))
	if err != nil {
		t.Fatal(err)
	}
	if listing.Location.Kind != "local" || listing.Location.Path != resolvedRoot {
		t.Fatalf("location = %#v, want local %q", listing.Location, resolvedRoot)
	}
	if listing.Parent != nil {
		t.Fatalf("parent at the root = %#v, want nil", listing.Parent)
	}
	if len(listing.Roots) != 1 || listing.Roots[0].Location.Path != resolvedRoot {
		t.Fatalf("roots = %#v", listing.Roots)
	}

	// Folders first, then case-insensitive alphabetical. Hidden entries are
	// still returned (the UI filters them) but marked hidden; a leading dot
	// sorts before letters, so .secret comes before alpha.
	wantNames := []string{"folder", ".secret", "alpha.excalidraw", "image.png", "zeta.txt"}
	if len(listing.Entries) != len(wantNames) {
		t.Fatalf("entries = %#v", listing.Entries)
	}
	for i, want := range wantNames {
		if listing.Entries[i].Name != want {
			t.Fatalf("entry %d = %q, want %q", i, listing.Entries[i].Name, want)
		}
	}

	kinds := map[string]string{}
	hidden := map[string]bool{}
	for _, entry := range listing.Entries {
		kinds[entry.Name] = entry.Kind
		hidden[entry.Name] = entry.Hidden
		if entry.Location.Kind != "local" || entry.Location.Path != filepath.Join(resolvedRoot, entry.Name) {
			t.Fatalf("entry %q location = %#v", entry.Name, entry.Location)
		}
	}
	if kinds["folder"] != "folder" || kinds["alpha.excalidraw"] != "excalidraw" || kinds["image.png"] != "image" || kinds["zeta.txt"] != "file" {
		t.Fatalf("kinds = %#v", kinds)
	}
	if !hidden[".secret"] || hidden["zeta.txt"] {
		t.Fatalf("hidden flags = %#v", hidden)
	}

	// A subdirectory reports its parent as the containing folder.
	sub := filepath.Join(root, "folder")
	subListing, err := app.ListFiles(localLoc(sub))
	if err != nil {
		t.Fatal(err)
	}
	if subListing.Parent == nil || subListing.Parent.Path != resolvedRoot {
		t.Fatalf("subdirectory parent = %#v, want %q", subListing.Parent, resolvedRoot)
	}
}

// TestFileManagerRejectsOutsideRoots verifies the root confinement: a ".."
// escape, an absolute path outside every root, and a symlink pointing out are
// all refused.
func TestFileManagerRejectsOutsideRoots(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	outside := t.TempDir()

	if _, err := app.ListFiles(localLoc(filepath.Join(root, ".."))); err == nil {
		t.Fatal("a '..' escape was accepted")
	}
	if _, err := app.ListFiles(localLoc(outside)); err == nil {
		t.Fatal("an absolute path outside the roots was accepted")
	}
	link := filepath.Join(root, "out-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if _, err := app.ListFiles(localLoc(link)); err == nil {
		t.Fatal("a symlink pointing outside the roots was accepted")
	}
}

// TestFileManagerRejectsSymlinkDotDotEscape verifies that a "<root>/link/../x"
// path is refused on every operation. filepath.Clean collapses link/.. to
// nothing, but the kernel resolves link/.. against the symlink target, so a
// naive clean-then-resolve would let the path through and the filesystem call
// would escape the root.
func TestFileManagerRejectsSymlinkDotDotEscape(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	outside := t.TempDir()
	link := filepath.Join(root, "out-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	// Build the path without filepath.Join, which would clean the ".." away.
	sep := string(os.PathSeparator)
	escape := link + sep + ".." + sep + "escape"

	if _, err := app.ListFiles(localLoc(escape)); err == nil {
		t.Fatal("a symlink + '..' escape was accepted on list")
	}
	// link/.. resolves to the parent of the symlink target, so the escape would
	// land in the parent of the outside directory, not inside it.
	escapeParent := filepath.Dir(outside)
	if err := app.NewFolder(localLoc(escape), "newdir"); err == nil {
		t.Fatal("a symlink + '..' escape was accepted on new-folder")
	}
	if _, err := os.Stat(filepath.Join(escapeParent, "escape", "newdir")); !os.IsNotExist(err) {
		t.Fatal("new-folder escape created a directory outside the root")
	}
	if _, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(escape)},
		Target:  localLoc(root),
	}); err == nil {
		t.Fatal("a symlink + '..' escape was accepted on transfer")
	}
	// The escape path must not have been created outside the root either.
	if _, err := os.Stat(filepath.Join(escapeParent, "escape")); !os.IsNotExist(err) {
		t.Fatal("transfer escape touched a path outside the root")
	}
}

// TestFileManagerCheckTransferConflicts verifies that CheckFileTransfer reports
// only existing targets and never writes anything.
func TestFileManagerCheckTransferConflicts(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	src := filepath.Join(root, "src")
	target := filepath.Join(root, "target")
	mustMkdir(t, src)
	mustMkdir(t, target)
	mustWriteFile(t, filepath.Join(src, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(src, "b.txt"), "b")
	mustWriteFile(t, filepath.Join(target, "a.txt"), "old-a")

	conflicts, err := app.CheckFileTransfer([]FileLocation{localLoc(filepath.Join(src, "a.txt")), localLoc(filepath.Join(src, "b.txt"))}, localLoc(target))
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Name != "a.txt" || conflicts[0].Target != filepath.Join(resolvedTarget, "a.txt") {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	// CheckFileTransfer must not modify anything.
	if data, err := os.ReadFile(filepath.Join(target, "a.txt")); err != nil || string(data) != "old-a" {
		t.Fatalf("check modified the target: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal("check removed a source")
	}
}

// TestFileManagerTransferSkip verifies the skip policy leaves the target and
// the source untouched.
func TestFileManagerTransferSkip(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	src := filepath.Join(root, "src")
	target := filepath.Join(root, "target")
	mustMkdir(t, src)
	mustMkdir(t, target)
	mustWriteFile(t, filepath.Join(src, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(target, "a.txt"), "old-a")

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(src, "a.txt"))},
		Target:  localLoc(target),
		Actions: []FileAction{{Source: localLoc(filepath.Join(src, "a.txt")), Action: "skip"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "skipped" || results[0].Target != nil {
		t.Fatalf("result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(target, "a.txt")); err != nil || string(data) != "old-a" {
		t.Fatalf("target changed after skip: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal("source removed after skip")
	}
}

// TestFileManagerTransferOverwrite verifies the overwrite policy replaces the
// existing target (trashing the old file first) and removes the source.
func TestFileManagerTransferOverwrite(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	src := filepath.Join(root, "src")
	target := filepath.Join(root, "target")
	mustMkdir(t, src)
	mustMkdir(t, target)
	mustWriteFile(t, filepath.Join(src, "a.txt"), "new-a")
	mustWriteFile(t, filepath.Join(target, "a.txt"), "old-a")

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(src, "a.txt"))},
		Target:  localLoc(target),
		Actions: []FileAction{{Source: localLoc(filepath.Join(src, "a.txt")), Action: "overwrite"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "overwritten" {
		t.Fatalf("result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(target, "a.txt")); err != nil || string(data) != "new-a" {
		t.Fatalf("target not overwritten: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("source still present after overwrite")
	}
}

// TestFileManagerTransferRename verifies the rename policy picks the next free
// "name 2.ext" and keeps the extension.
func TestFileManagerTransferRename(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	src := filepath.Join(root, "src")
	target := filepath.Join(root, "target")
	mustMkdir(t, src)
	mustMkdir(t, target)
	mustWriteFile(t, filepath.Join(src, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(target, "a.txt"), "old-a")
	mustWriteFile(t, filepath.Join(target, "a 2.txt"), "old-2")

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(src, "a.txt"))},
		Target:  localLoc(target),
		Actions: []FileAction{{Source: localLoc(filepath.Join(src, "a.txt")), Action: "rename"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "renamed" {
		t.Fatalf("result = %#v", results[0])
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedTarget, "a 3.txt"); results[0].Target == nil || results[0].Target.Path != want {
		t.Fatalf("renamed target = %#v, want %q", results[0].Target, want)
	}
	if data, err := os.ReadFile(filepath.Join(target, "a 3.txt")); err != nil || string(data) != "a" {
		t.Fatalf("renamed content = %q, %v", data, err)
	}
	// The original colliding files are untouched.
	if data, err := os.ReadFile(filepath.Join(target, "a.txt")); err != nil || string(data) != "old-a" {
		t.Fatalf("original target changed: %q, %v", data, err)
	}
}

// TestFileManagerMoveBetweenRoots verifies a move across two configured roots
// keeps the content and removes the source.
func TestFileManagerMoveBetweenRoots(t *testing.T) {
	app := testFileManagerApp(t)
	root1 := app.store.localFolders[0]
	root2 := filepath.Join(t.TempDir(), "boards2")
	if err := app.store.addLocalFolder(root2); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(root1, "move.txt"), "content")
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root1, "move.txt"))},
		Target:  localLoc(root2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "moved" {
		t.Fatalf("result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root2, "move.txt")); err != nil || string(data) != "content" {
		t.Fatalf("moved content = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root1, "move.txt")); !os.IsNotExist(err) {
		t.Fatal("source still present after move")
	}
}

// TestFileManagerCopyKeepsBoth verifies a copy leaves the source in place.
func TestFileManagerCopyKeepsBoth(t *testing.T) {
	app := testFileManagerApp(t)
	root1 := app.store.localFolders[0]
	root2 := filepath.Join(t.TempDir(), "boards2")
	if err := app.store.addLocalFolder(root2); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(root1, "copy.txt"), "content")
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root1, "copy.txt"))},
		Target:  localLoc(root2),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root2, "copy.txt")); err != nil || string(data) != "content" {
		t.Fatalf("copied content = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root1, "copy.txt")); err != nil {
		t.Fatal("source missing after copy")
	}
}

// TestFileManagerDirectoryRecursion copies and moves a folder tree, verifying
// nested files are preserved and the source is removed only on move.
func TestFileManagerDirectoryRecursion(t *testing.T) {
	app := testFileManagerApp(t)
	root1 := app.store.localFolders[0]
	root2 := filepath.Join(t.TempDir(), "boards2")
	if err := app.store.addLocalFolder(root2); err != nil {
		t.Fatal(err)
	}

	copyTree := filepath.Join(root1, "copy-tree")
	mustMkdir(t, filepath.Join(copyTree, "sub"))
	mustWriteFile(t, filepath.Join(copyTree, "root.txt"), "r")
	mustWriteFile(t, filepath.Join(copyTree, "sub", "child.txt"), "c")

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(copyTree)},
		Target:  localLoc(root2),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("copy result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root2, "copy-tree", "sub", "child.txt")); err != nil || string(data) != "c" {
		t.Fatalf("copied child = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(copyTree, "root.txt")); err != nil {
		t.Fatal("source tree missing after copy")
	}

	moveTree := filepath.Join(root1, "move-tree")
	mustMkdir(t, filepath.Join(moveTree, "sub"))
	mustWriteFile(t, filepath.Join(moveTree, "root.txt"), "r")
	mustWriteFile(t, filepath.Join(moveTree, "sub", "child.txt"), "c")

	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(moveTree)},
		Target:  localLoc(root2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "moved" {
		t.Fatalf("move result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root2, "move-tree", "sub", "child.txt")); err != nil || string(data) != "c" {
		t.Fatalf("moved child = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(moveTree, "root.txt")); !os.IsNotExist(err) {
		t.Fatal("source tree still present after move")
	}
}

// TestFileManagerRefusesSelfMove verifies a directory cannot be moved or copied
// into itself or one of its descendants.
func TestFileManagerRefusesSelfMove(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	dir := filepath.Join(root, "dir")
	mustMkdir(t, filepath.Join(dir, "sub"))

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(dir)},
		Target:  localLoc(filepath.Join(dir, "sub")),
	})
	if err != nil {
		t.Fatal(err) // the batch-level error is nil; the per-path error is on the result
	}
	if results[0].Error == "" || !strings.Contains(results[0].Error, "into itself") {
		t.Fatalf("self-move error = %q", results[0].Error)
	}
}

// TestFileManagerTransferBatchContinuesOnError verifies that one failing path
// (here a directory moved into its own descendant) does not abort the batch;
// the next path is still processed.
func TestFileManagerTransferBatchContinuesOnError(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	dir := filepath.Join(root, "dir")
	mustMkdir(t, filepath.Join(dir, "sub"))
	mustWriteFile(t, filepath.Join(root, "ok.txt"), "ok")

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(dir), localLoc(filepath.Join(root, "ok.txt"))},
		Target:  localLoc(filepath.Join(dir, "sub")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Error == "" {
		t.Fatal("self-move should report an error")
	}
	if results[1].Action != "moved" {
		t.Fatalf("second result = %#v", results[1])
	}
}

// TestFileManagerRenameValidation checks the rename rules: separators, an
// existing name and a leading dot on a non-hidden file are rejected, while a
// leading dot on an already-hidden file is allowed.
func TestFileManagerRenameValidation(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	mustWriteFile(t, filepath.Join(root, "old.txt"), "old")
	mustWriteFile(t, filepath.Join(root, "existing.txt"), "e")

	if err := app.RenameFilePath(localLoc(filepath.Join(root, "old.txt")), "a/b"); err == nil {
		t.Fatal("a path separator was accepted")
	}
	if err := app.RenameFilePath(localLoc(filepath.Join(root, "old.txt")), "existing.txt"); err == nil {
		t.Fatal("an existing name was accepted")
	}
	if err := app.RenameFilePath(localLoc(filepath.Join(root, "old.txt")), ".hidden"); err == nil {
		t.Fatal("a leading dot on a non-hidden file was accepted")
	}
	if err := app.RenameFilePath(localLoc(filepath.Join(root, "old.txt")), "new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal("renamed file is missing")
	}

	mustWriteFile(t, filepath.Join(root, ".dot"), "d")
	if err := app.RenameFilePath(localLoc(filepath.Join(root, ".dot")), ".renamed"); err != nil {
		t.Fatalf("a leading dot on a hidden file was rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".renamed")); err != nil {
		t.Fatal("hidden rename is missing")
	}
}

// TestFileManagerNewFolder verifies folder creation and duplicate rejection.
func TestFileManagerNewFolder(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]

	if err := app.NewFolder(localLoc(root), "newdir"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !info.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	if err := app.NewFolder(localLoc(root), "newdir"); err == nil {
		t.Fatal("a duplicate folder was accepted")
	}
	if err := app.NewFolder(localLoc(root), "a/b"); err == nil {
		t.Fatal("a path separator was accepted")
	}
	if err := app.NewFolder(localLoc(root), ".hidden"); err == nil {
		t.Fatal("a leading-dot folder was accepted")
	}
}

// TestFileManagerTrash verifies TrashFilePath removes the file from its folder.
// HOME is redirected so the OS Trash lands in a temp directory.
func TestFileManagerTrash(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	t.Setenv("HOME", t.TempDir())

	mustWriteFile(t, filepath.Join(root, "trash.txt"), "t")
	if err := app.TrashFilePath(localLoc(filepath.Join(root, "trash.txt"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "trash.txt")); !os.IsNotExist(err) {
		t.Fatal("file still present after trash")
	}
}

// TestFileManagerThumbnailImage verifies an image is returned as a data URL and
// a non-image returns "".
func TestFileManagerThumbnailImage(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	// A minimal PNG signature is enough to exercise the data-URL path.
	mustWriteFile(t, filepath.Join(root, "img.png"), "\x89PNG\r\n\x1a\n")
	dataURL, err := app.FileThumbnail(localLoc(filepath.Join(root, "img.png")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("data URL = %q", dataURL)
	}

	mustWriteFile(t, filepath.Join(root, "note.txt"), "plain")
	dataURL, err = app.FileThumbnail(localLoc(filepath.Join(root, "note.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if dataURL != "" {
		t.Fatalf("non-image thumbnail = %q, want empty", dataURL)
	}
}

// TestFileManagerThumbnailCacheInvalidates verifies the in-memory preview cache
// is keyed by mtime+size, so editing a file produces a fresh thumbnail.
func TestFileManagerThumbnailCacheInvalidates(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	path := filepath.Join(root, "img.png")
	mustWriteFile(t, path, "old")
	first, err := app.FileThumbnail(localLoc(path))
	if err != nil {
		t.Fatal(err)
	}
	// Different size guarantees the cache key changes even if the mtime
	// granularity does not.
	mustWriteFile(t, path, "new-content-longer")
	second, err := app.FileThumbnail(localLoc(path))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("thumbnail did not refresh after the file changed")
	}
}

// TestFileManagerRefreshBoards verifies RefreshBoards rescans the active source
// so a board created on disk becomes visible to the sidebar.
func TestFileManagerRefreshBoards(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	if err := app.store.createBoard("Before"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "After.excalidraw"), `{"type":"excalidraw","elements":[]}`)
	if app.store.board("After.excalidraw") != nil {
		t.Fatal("board visible before refresh")
	}
	if err := app.RefreshBoards(); err != nil {
		t.Fatal(err)
	}
	if app.store.board("After.excalidraw") == nil {
		t.Fatal("board not visible after refresh")
	}
}

// TestFileManagerRefusesRootMutation verifies the mutating methods refuse to act
// on a configured boards folder itself, which would remove or rename the folder
// the app is configured to watch.
func TestFileManagerRefusesRootMutation(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]

	if err := app.TrashFilePath(localLoc(root)); err == nil {
		t.Fatal("trashing a boards folder was accepted")
	}
	if err := app.RenameFilePath(localLoc(root), "renamed"); err == nil {
		t.Fatal("renaming a boards folder was accepted")
	}
	if _, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(root)},
		Target:  localLoc(filepath.Join(root, "sub")),
	}); err == nil {
		t.Fatal("transferring a whole boards folder was accepted")
	}
	if _, err := app.CheckFileTransfer([]FileLocation{localLoc(root)}, localLoc(filepath.Join(root, "sub"))); err == nil {
		t.Fatal("checking a transfer of a whole boards folder was accepted")
	}
}

// TestFileManagerCopySymlink verifies copying a symlink recreates the link
// rather than trying to read the target as a file, which would fail for a link
// that points at a directory.
func TestFileManagerCopySymlink(t *testing.T) {
	app := testFileManagerApp(t)
	root := app.store.localFolders[0]
	target := filepath.Join(root, "target")
	mustMkdir(t, target)
	dest := filepath.Join(root, "dest")
	mustMkdir(t, dest)
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(link)},
		Target:  localLoc(dest),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("result = %#v", results[0])
	}
	// The copy must be a symlink pointing at the same target, not a directory.
	copied := filepath.Join(dest, "link")
	if info, err := os.Lstat(copied); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("copied link is not a symlink: %v, %v", info, err)
	}
	if got, err := os.Readlink(copied); err != nil || got != target {
		t.Fatalf("copied link target = %q, %v", got, err)
	}
	// The original link is still present after a copy.
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("original link missing after copy")
	}
}

// natsLoc builds a NATS FileLocation for the integration-test server.
func natsLoc(bucket, name string) FileLocation {
	return FileLocation{Kind: "nats", Server: "test", Bucket: bucket, Name: name}
}

// testNATSApp builds an App wired to a test store configured with a NATS server
// and two buckets. It is gated on EXCALIBUR_TEST_NATS_URL exactly like the other
// JetStream integration tests, and waits for the spawned server to accept a
// connection before returning.
func testNATSApp(t *testing.T) *App {
	t.Helper()
	serverURL := os.Getenv("EXCALIBUR_TEST_NATS_URL")
	if serverURL == "" {
		t.Skip("set EXCALIBUR_TEST_NATS_URL to a server URL or 'spawn' to run the JetStream integration test")
	}
	serverURL = resolveTestNATS(t, serverURL)
	app := newApp()
	app.store = testStore(t)
	app.store.prefs.NatsServers = []NatsServerConfig{{
		ID:      "test",
		Name:    "Test",
		URL:     serverURL,
		Buckets: []string{"boards", "boards2"},
	}}
	// The spawned server may still be starting; retry the first dial so a
	// connection-refused on startup does not fail the whole test.
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err = app.store.clientFor("nats:test"); err == nil {
			return app
		}
	}
	t.Fatalf("connect to integration server: %v", err)
	return nil
}

// mustWriteObject writes an object into a bucket, creating the bucket on demand.
func mustWriteObject(t *testing.T, app *App, bucket, name string, data []byte) {
	t.Helper()
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.writeObject(bucket, name, data); err != nil {
		t.Fatal(err)
	}
}

// TestFileManagerNATSListBucket checks a bucket listing: hidden objects are
// returned but marked hidden, kinds come from the extension, sizes are reported,
// and the parent is always nil.
func TestFileManagerNATSListBucket(t *testing.T) {
	app := testNATSApp(t)
	mustWriteObject(t, app, "boards", "alpha.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`))
	mustWriteObject(t, app, "boards", "image.png", []byte("\x89PNG\r\n\x1a\n"))
	mustWriteObject(t, app, "boards", "note.txt", []byte("plain"))
	mustWriteObject(t, app, "boards", ".secret", []byte("hidden"))

	listing, err := app.ListFiles(natsLoc("boards", ""))
	if err != nil {
		t.Fatal(err)
	}
	if listing.Location.Kind != "nats" || listing.Location.Server != "test" || listing.Location.Bucket != "boards" {
		t.Fatalf("location = %#v", listing.Location)
	}
	if listing.Parent != nil {
		t.Fatalf("bucket parent = %#v, want nil", listing.Parent)
	}
	// Roots include the local folder (group "Local") and the server's buckets.
	groups := map[string]bool{}
	for _, root := range listing.Roots {
		groups[root.Group] = true
	}
	if !groups["Local"] || !groups["Test"] {
		t.Fatalf("roots = %#v", listing.Roots)
	}
	wantNames := []string{".secret", "alpha.excalidraw", "image.png", "note.txt"}
	if len(listing.Entries) != len(wantNames) {
		t.Fatalf("entries = %#v", listing.Entries)
	}
	kinds := map[string]string{}
	hidden := map[string]bool{}
	for i, want := range wantNames {
		entry := listing.Entries[i]
		if entry.Name != want {
			t.Fatalf("entry %d = %q, want %q", i, entry.Name, want)
		}
		if entry.IsDir {
			t.Fatalf("entry %q is a directory", want)
		}
		if entry.Location.Kind != "nats" || entry.Location.Bucket != "boards" || entry.Location.Name != want {
			t.Fatalf("entry %q location = %#v", want, entry.Location)
		}
		kinds[entry.Name] = entry.Kind
		hidden[entry.Name] = entry.Hidden
	}
	if kinds["alpha.excalidraw"] != "excalidraw" || kinds["image.png"] != "image" || kinds["note.txt"] != "file" {
		t.Fatalf("kinds = %#v", kinds)
	}
	if !hidden[".secret"] || hidden["note.txt"] {
		t.Fatalf("hidden flags = %#v", hidden)
	}
}

// TestFileManagerNATSTransferLocalToBucket verifies local→bucket copy keeps the
// source and move trashes it, and that a .excalidraw source mints a fresh
// document id.
func TestFileManagerNATSTransferLocalToBucket(t *testing.T) {
	app := testNATSApp(t)
	root := app.store.localFolders[0]
	t.Setenv("HOME", t.TempDir())

	mustWriteFile(t, filepath.Join(root, "copy.txt"), "content")
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "copy.txt"))},
		Target:  natsLoc("boards", ""),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("copy result = %#v", results[0])
	}
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := client.readObject("boards", "copy.txt"); err != nil || string(data) != "content" {
		t.Fatalf("bucket copy = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "copy.txt")); err != nil {
		t.Fatal("source missing after copy")
	}

	mustWriteFile(t, filepath.Join(root, "move.txt"), "content")
	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "move.txt"))},
		Target:  natsLoc("boards", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "moved" {
		t.Fatalf("move result = %#v", results[0])
	}
	if data, err := client.readObject("boards", "move.txt"); err != nil || string(data) != "content" {
		t.Fatalf("bucket move = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "move.txt")); !os.IsNotExist(err) {
		t.Fatal("source still present after move")
	}

	// A .excalidraw board copied into a bucket gets a fresh document id.
	mustWriteFile(t, filepath.Join(root, "board.excalidraw"), `{"type":"excalidraw","elements":[]}`)
	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "board.excalidraw"))},
		Target:  natsLoc("boards", ""),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("board copy result = %#v", results[0])
	}
	if _, err := client.documentID("boards", "board.excalidraw"); err != nil {
		t.Fatalf("board document id: %v", err)
	}
}

// TestFileManagerNATSTransferBucketToLocal verifies bucket→local copy keeps the
// object and move deletes it permanently.
func TestFileManagerNATSTransferBucketToLocal(t *testing.T) {
	app := testNATSApp(t)
	root := app.store.localFolders[0]
	t.Setenv("HOME", t.TempDir())
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}

	mustWriteObject(t, app, "boards", "copy.txt", []byte("content"))
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{natsLoc("boards", "copy.txt")},
		Target:  localLoc(root),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("copy result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root, "copy.txt")); err != nil || string(data) != "content" {
		t.Fatalf("local copy = %q, %v", data, err)
	}
	if data, err := client.readObject("boards", "copy.txt"); err != nil || string(data) != "content" {
		t.Fatalf("source object missing after copy: %q, %v", data, err)
	}

	mustWriteObject(t, app, "boards", "move.txt", []byte("content"))
	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{natsLoc("boards", "move.txt")},
		Target:  localLoc(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "moved" {
		t.Fatalf("move result = %#v", results[0])
	}
	if data, err := os.ReadFile(filepath.Join(root, "move.txt")); err != nil || string(data) != "content" {
		t.Fatalf("local move = %q, %v", data, err)
	}
	if data, err := client.readObject("boards", "move.txt"); err != nil || data != nil {
		t.Fatalf("source object still present after move: %q, %v", data, err)
	}
}

// TestFileManagerNATSTransferBucketToBucket verifies bucket→bucket copy mints a
// fresh document id and move carries it across and deletes the source.
func TestFileManagerNATSTransferBucketToBucket(t *testing.T) {
	app := testNATSApp(t)
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}

	mustWriteObject(t, app, "boards", "copy.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`))
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{natsLoc("boards", "copy.excalidraw")},
		Target:  natsLoc("boards2", ""),
		Copy:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "copied" {
		t.Fatalf("copy result = %#v", results[0])
	}
	srcID, err := client.documentID("boards", "copy.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	copyID, err := client.documentID("boards2", "copy.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if srcID == copyID {
		t.Fatal("copy did not mint a fresh document id")
	}

	mustWriteObject(t, app, "boards", "move.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`))
	documentID, err := newDocumentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.setDocumentID("boards", "move.excalidraw", documentID); err != nil {
		t.Fatal(err)
	}
	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{natsLoc("boards", "move.excalidraw")},
		Target:  natsLoc("boards2", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "moved" {
		t.Fatalf("move result = %#v", results[0])
	}
	movedID, err := client.documentID("boards2", "move.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if movedID != documentID {
		t.Fatalf("move did not carry the document id: %q != %q", movedID, documentID)
	}
	if data, err := client.readObject("boards", "move.excalidraw"); err != nil || data != nil {
		t.Fatalf("source object still present after move: %q, %v", data, err)
	}
}

// TestFileManagerNATSRefusesSelfTransfer covers the case where an object is
// moved onto itself (same bucket, same name): the copy is written and then the
// source delete would remove it again, so the transfer has to be refused while
// the object is still readable and a same-bucket copy with a new name still
// works.
func TestFileManagerNATSRefusesSelfTransfer(t *testing.T) {
	app := testNATSApp(t)
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"excalidraw","elements":[]}`)
	mustWriteObject(t, app, "boards", "self.excalidraw", body)

	for _, copyMode := range []bool{false, true} {
		results, err := app.TransferFiles(FileTransferRequest{
			Sources: []FileLocation{natsLoc("boards", "self.excalidraw")},
			Target:  natsLoc("boards", ""),
			Copy:    copyMode,
			Actions: []FileAction{{Source: natsLoc("boards", "self.excalidraw"), Action: "overwrite"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if results[0].Error == "" || results[0].Action != "" {
			t.Fatalf("copy=%v result = %#v, want a refusal", copyMode, results[0])
		}
		data, err := client.readObject("boards", "self.excalidraw")
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(body) {
			t.Fatalf("copy=%v object changed or vanished: %q", copyMode, data)
		}
	}

	// A same-bucket copy that the collision dialog renamed is still allowed.
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{natsLoc("boards", "self.excalidraw")},
		Target:  natsLoc("boards", ""),
		Copy:    true,
		Actions: []FileAction{{Source: natsLoc("boards", "self.excalidraw"), Action: "rename"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "renamed" || results[0].Name != "self 2.excalidraw" {
		t.Fatalf("renamed copy result = %#v", results[0])
	}
}

// TestFileManagerNATSCollisions verifies skip / overwrite / rename inside a
// bucket target.
func TestFileManagerNATSCollisions(t *testing.T) {
	app := testNATSApp(t)
	root := app.store.localFolders[0]
	t.Setenv("HOME", t.TempDir())
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}

	mustWriteObject(t, app, "boards", "a.txt", []byte("old"))
	mustWriteFile(t, filepath.Join(root, "a.txt"), "new")
	results, err := app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "a.txt"))},
		Target:  natsLoc("boards", ""),
		Actions: []FileAction{{Source: localLoc(filepath.Join(root, "a.txt")), Action: "skip"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "skipped" {
		t.Fatalf("skip result = %#v", results[0])
	}
	if data, err := client.readObject("boards", "a.txt"); err != nil || string(data) != "old" {
		t.Fatalf("skip changed bucket: %q, %v", data, err)
	}

	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "a.txt"))},
		Target:  natsLoc("boards", ""),
		Actions: []FileAction{{Source: localLoc(filepath.Join(root, "a.txt")), Action: "overwrite"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "overwritten" {
		t.Fatalf("overwrite result = %#v", results[0])
	}
	if data, err := client.readObject("boards", "a.txt"); err != nil || string(data) != "new" {
		t.Fatalf("overwrite did not replace: %q, %v", data, err)
	}

	mustWriteObject(t, app, "boards", "b.txt", []byte("old-b"))
	mustWriteFile(t, filepath.Join(root, "b.txt"), "new-b")
	results, err = app.TransferFiles(FileTransferRequest{
		Sources: []FileLocation{localLoc(filepath.Join(root, "b.txt"))},
		Target:  natsLoc("boards", ""),
		Actions: []FileAction{{Source: localLoc(filepath.Join(root, "b.txt")), Action: "rename"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != "renamed" {
		t.Fatalf("rename result = %#v", results[0])
	}
	if data, err := client.readObject("boards", "b 2.txt"); err != nil || string(data) != "new-b" {
		t.Fatalf("renamed object = %q, %v", data, err)
	}
}

// TestFileManagerNATSRenameKeepsDocumentID verifies renaming a bucket object
// carries the document id across and deletes the old object.
func TestFileManagerNATSRenameKeepsDocumentID(t *testing.T) {
	app := testNATSApp(t)
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteObject(t, app, "boards", "old.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`))
	documentID, err := newDocumentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.setDocumentID("boards", "old.excalidraw", documentID); err != nil {
		t.Fatal(err)
	}

	if err := app.RenameFilePath(natsLoc("boards", "old.excalidraw"), "new.excalidraw"); err != nil {
		t.Fatal(err)
	}
	if data, err := client.readObject("boards", "old.excalidraw"); err != nil || data != nil {
		t.Fatalf("old object still present: %q, %v", data, err)
	}
	if data, err := client.readObject("boards", "new.excalidraw"); err != nil || string(data) != `{"type":"excalidraw","elements":[]}` {
		t.Fatalf("new object = %q, %v", data, err)
	}
	got, err := client.documentID("boards", "new.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if got != documentID {
		t.Fatalf("document id not preserved: %q != %q", got, documentID)
	}
}

// TestFileManagerNATSPermanentDelete verifies TrashFilePath on a bucket object
// is a permanent delete.
func TestFileManagerNATSPermanentDelete(t *testing.T) {
	app := testNATSApp(t)
	client, err := app.store.clientFor("nats:test")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteObject(t, app, "boards", "a.txt", []byte("content"))
	if err := app.TrashFilePath(natsLoc("boards", "a.txt")); err != nil {
		t.Fatal(err)
	}
	if data, err := client.readObject("boards", "a.txt"); err != nil || data != nil {
		t.Fatalf("object still present after delete: %q, %v", data, err)
	}
}

// TestFileManagerNATSThumbnail verifies an image in a bucket is returned as a
// data URL through the NATS read path.
func TestFileManagerNATSThumbnail(t *testing.T) {
	app := testNATSApp(t)
	mustWriteObject(t, app, "boards", "img.png", []byte("\x89PNG\r\n\x1a\n"))
	dataURL, err := app.FileThumbnail(natsLoc("boards", "img.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("data URL = %q", dataURL)
	}
}

// TestFileManagerNATSThumbnailBoard verifies a .excalidraw board in a bucket is
// read and handed to the renderer. In tests there is no renderer, so it returns
// "" (the renderer round-trip times out); the point is that the bucket read
// succeeds and does not error.
func TestFileManagerNATSThumbnailBoard(t *testing.T) {
	app := testNATSApp(t)
	mustWriteObject(t, app, "boards", "board.excalidraw", []byte(`{"type":"excalidraw","elements":[]}`))
	dataURL, err := app.FileThumbnail(natsLoc("boards", "board.excalidraw"))
	if err != nil {
		t.Fatal(err)
	}
	if dataURL != "" {
		t.Fatalf("board thumbnail = %q, want empty without a renderer", dataURL)
	}
}

// TestFileManagerNATSRejectsUnknownServerBucket verifies that an unknown server,
// an unconfigured bucket, and an invalid location kind are all rejected.
func TestFileManagerNATSRejectsUnknownServerBucket(t *testing.T) {
	app := testNATSApp(t)
	if _, err := app.ListFiles(FileLocation{Kind: "nats", Server: "unknown", Bucket: "boards"}); err == nil {
		t.Fatal("an unknown server was accepted")
	}
	if _, err := app.ListFiles(FileLocation{Kind: "nats", Server: "test", Bucket: "missing"}); err == nil {
		t.Fatal("an unconfigured bucket was accepted")
	}
	if _, err := app.ListFiles(FileLocation{Kind: "weird", Path: "/tmp"}); err == nil {
		t.Fatal("an invalid location kind was accepted")
	}
	if err := app.TrashFilePath(FileLocation{Kind: "nats", Server: "test", Bucket: "missing", Name: "a.txt"}); err == nil {
		t.Fatal("an unconfigured bucket was accepted on trash")
	}
}
