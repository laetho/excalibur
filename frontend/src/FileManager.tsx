import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  checkFileTransfer,
  fileThumbnail,
  getState,
  listFiles,
  log,
  newFolder,
  refreshBoards,
  renameFilePath,
  revealFolder,
  transferFiles,
  trashFilePath,
  type FileAction,
  type FileConflict,
  type FileEntry,
  type FileLocation,
  type FileRoot,
  type FileTransferResult,
  type SourceInfo,
  type StoreSnapshot,
} from './ipc'
import NewBucketDialog from './NewBucketDialog'
import {
  ArrowUpIcon,
  ChevronDownIcon,
  CloseIcon,
  CloudIcon,
  DetailsIcon,
  EyeIcon,
  FileIcon,
  FilesIcon,
  FolderIcon,
  FolderPlusIcon,
  GridIcon,
  ListIcon,
  RefreshIcon,
  SwapIcon,
} from './icons'

// MARK: - Types

type PaneId = 'left' | 'right'
type ViewMode = 'list' | 'grid'
type SortKey = 'name' | 'size' | 'modified'
type SortDir = 'asc' | 'desc'
type SortState = { key: SortKey; dir: SortDir }
type SelectMode = 'single' | 'toggle' | 'range'

/** One pane's state. A pane is either a local directory or a NATS bucket. */
type PaneData = {
  location: FileLocation
  /** nil at a local root and always for a bucket. */
  parent: FileLocation | null
  entries: FileEntry[]
  loading: boolean
  error: string | null
  /** Selected entry keys (see entryKey), not locations, so a re-list keeps the selection stable. */
  selection: Set<string>
  anchor: string | null
  sort: SortState
}

const emptyPane = (location: FileLocation): PaneData => ({
  location,
  parent: null,
  entries: [],
  loading: true,
  error: null,
  selection: new Set<string>(),
  anchor: null,
  sort: { key: 'name', dir: 'asc' },
})

const PLATFORM = typeof navigator === 'undefined' ? '' : navigator.platform || navigator.userAgent
const IS_MAC = /Mac/i.test(PLATFORM)
/** The platform's name for its file manager, for "Show in …" actions. */
const FILE_MANAGER = IS_MAC ? 'Finder' : /Win/i.test(PLATFORM) ? 'File Explorer' : 'File Manager'

/** A file at or above this size gets a size badge in the grid. */
const LARGE_FILE = 1024 * 1024

// MARK: - Thumbnail cache
//
// Grid thumbnails are requested lazily and cached at module level so switching
// between panes and views never refetches a preview. A small concurrency limit
// keeps a big folder from firing a burst of renderer round-trips at once. The
// cache is keyed by location (local path or server/bucket/name) so a bucket
// object and a local file with the same name never collide.

const thumbnailCache = new Map<string, string>()
const thumbnailQueue: FileLocation[] = []
const thumbnailWaiters = new Map<string, (value: string) => void>()
let thumbnailActive = 0
const THUMBNAIL_CONCURRENCY = 3
// The cache is bounded so a long session over many folders cannot grow without
// limit; when it fills up the whole map is dropped rather than tracking LRU
// order, because thumbnails are cheap to rebuild.
const THUMBNAIL_CACHE_MAX = 300

/** A stable string identity for a location, used as a cache and selection key. */
function locationKey(loc: FileLocation): string {
  return loc.kind === 'local' ? `local:${loc.path}` : `nats:${loc.server}:${loc.bucket}:${loc.name}`
}

function cacheThumbnail(loc: FileLocation, dataUrl: string) {
  if (thumbnailCache.size >= THUMBNAIL_CACHE_MAX) thumbnailCache.clear()
  thumbnailCache.set(locationKey(loc), dataUrl)
}

function pumpThumbnails() {
  while (thumbnailActive < THUMBNAIL_CONCURRENCY && thumbnailQueue.length > 0) {
    const loc = thumbnailQueue.shift()!
    const key = locationKey(loc)
    thumbnailActive++
    fileThumbnail(loc)
      .then((dataUrl) => cacheThumbnail(loc, dataUrl))
      .catch(() => cacheThumbnail(loc, ''))
      .finally(() => {
        thumbnailActive--
        const resolve = thumbnailWaiters.get(key)
        if (resolve) {
          thumbnailWaiters.delete(key)
          resolve(thumbnailCache.get(key) ?? '')
        }
        pumpThumbnails()
      })
  }
}

// Drop cached previews for locations that changed, so an overwritten or renamed
// file shows its fresh thumbnail instead of a stale one.
function invalidateThumbnails(locations: FileLocation[]) {
  for (const loc of locations) thumbnailCache.delete(locationKey(loc))
}

function requestThumbnail(loc: FileLocation): Promise<string> {
  const key = locationKey(loc)
  const cached = thumbnailCache.get(key)
  if (cached !== undefined) return Promise.resolve(cached)
  const existing = thumbnailWaiters.get(key)
  if (existing) {
    return new Promise((resolve) => {
      thumbnailWaiters.set(key, (value) => {
        existing(value)
        resolve(value)
      })
    })
  }
  return new Promise((resolve) => {
    thumbnailWaiters.set(key, resolve)
    thumbnailQueue.push(loc)
    pumpThumbnails()
  })
}

// MARK: - Helpers

/** "B / KB / MB / GB", one decimal place until the number reaches three digits. */
function humanSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = -1
  do {
    value /= 1024
    unit++
  } while (value >= 1024 && unit < units.length - 1)
  const rounded = value >= 100 ? Math.round(value) : Math.round(value * 10) / 10
  return `${rounded} ${units[unit]}`
}

function formatDate(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

/** Folders first, then the chosen key; case-insensitive name sort matches the sidebar. */
function sortEntries(entries: FileEntry[], sort: SortState): FileEntry[] {
  const dir = sort.dir === 'asc' ? 1 : -1
  return [...entries].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    if (sort.key === 'name') return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) * dir
    if (sort.key === 'size') return (a.size - b.size) * dir
    return (a.modified - b.modified) * dir
  })
}

/** The entries a pane shows, in display order: sorted, and hidden filtered out unless shown. */
function visibleEntries(pane: PaneData, showHidden: boolean): FileEntry[] {
  const sorted = sortEntries(pane.entries, pane.sort)
  return showHidden ? sorted : sorted.filter((e) => !e.hidden)
}

/** The stable identity of an entry within its pane, used for selection and drag payloads. */
function entryKey(entry: FileEntry): string {
  return entry.location.kind === 'local' ? entry.location.path : `${entry.location.server}:${entry.location.bucket}:${entry.location.name}`
}

/** The selected entries' locations, in the order they appear in the pane. */
function selectedLocations(pane: PaneData): FileLocation[] {
  return pane.entries.filter((e) => pane.selection.has(entryKey(e))).map((e) => e.location)
}

/** The path shown in the path bar: the directory relative to its root, ~ for home. */
function relativePath(path: string, root: string): string {
  if (!path) return ''
  if (path === root) return '~'
  const rel = path.slice(root.length).replace(/^[\\/]+/, '')
  return `~/${rel}`
}

/** One line for the footer: item count, then selection count and size. */
function footerText(pane: PaneData): string {
  const count = pane.entries.length
  let text = `${count} ${count === 1 ? 'item' : 'items'}`
  if (pane.selection.size > 0) {
    const size = selectionSize(pane)
    text += ` · ${pane.selection.size} selected${size ? ` (${humanSize(size)})` : ''}`
  }
  return text
}

function selectionSize(pane: PaneData): number {
  let size = 0
  for (const entry of pane.entries) {
    if (pane.selection.has(entryKey(entry)) && !entry.isDir) size += entry.size
  }
  return size
}

/** "Moved 3 files, 2 skipped" — one line for the footer after a transfer. */
function summarize(results: FileTransferResult[]): string {
  const counts: Record<string, number> = {}
  for (const result of results) {
    const key = result.error ? 'error' : result.action
    counts[key] = (counts[key] ?? 0) + 1
  }
  const parts: string[] = []
  if (counts.moved) parts.push(`Moved ${counts.moved}`)
  if (counts.copied) parts.push(`Copied ${counts.copied}`)
  if (counts.overwritten) parts.push(`Overwrote ${counts.overwritten}`)
  if (counts.renamed) parts.push(`Renamed ${counts.renamed}`)
  if (counts.skipped) parts.push(`${counts.skipped} skipped`)
  if (counts.error) parts.push(`${counts.error} failed`)
  return parts.length ? parts.join(', ') : 'Nothing transferred'
}

/** Whether a path sits under a root, matching either platform separator. */
function isPathWithin(path: string, root: string): boolean {
  return path === root || path.startsWith(root + '/') || path.startsWith(root + '\\')
}

/** Whether two locations identify the same pane or entry. */
function sameLocation(a: FileLocation, b: FileLocation): boolean {
  if (a.kind !== b.kind) return false
  if (a.kind === 'local') return a.path === b.path
  return a.server === b.server && a.bucket === b.bucket && a.name === b.name
}

/** Whether any of the dragged locations is the target or, for a folder, an ancestor of it. */
function isSelfOrDescendant(sources: FileLocation[], target: FileLocation): boolean {
  for (const src of sources) {
    if (src.kind === 'local' && target.kind === 'local') {
      if (isPathWithin(target.path, src.path)) return true
    } else if (src.kind === 'nats' && target.kind === 'nats') {
      // A bucket has no folder structure, so an object can only be moved to a
      // different bucket; dropping it back onto its own bucket is a no-op.
      if (src.server === target.server && src.bucket === target.bucket) return true
    }
  }
  return false
}

/** The root a pane location lives under, for the picker label and the Up/path bar. */
function findContainingRoot(location: FileLocation, roots: FileRoot[]): FileRoot | null {
  if (location.kind === 'nats') {
    return roots.find((r) => r.location.kind === 'nats' && r.location.server === location.server && r.location.bucket === location.bucket) ?? null
  }
  return roots.find((r) => r.location.kind === 'local' && isPathWithin(location.path, r.location.path)) ?? null
}

/** The pill's visible label: the root plus the current subfolder, or "Server / bucket". */
function pickerLabelFor(location: FileLocation, root: FileRoot | null): string {
  if (location.kind === 'nats') {
    return root ? `${root.group} / ${root.label}` : location.bucket
  }
  if (!root) return location.path
  if (location.path === root.location.path) return root.label
  const rel = location.path.slice(root.location.path.length).replace(/^[\\/]+/, '')
  return `${root.label}/${rel}`
}

/** The folder a new subfolder would be created in, as the picker shows it. */
function newFolderTargetLabel(location: FileLocation, roots: FileRoot[]): string {
  return pickerLabelFor(location, findContainingRoot(location, roots))
}

/** A remembered pane location is only kept when it still sits under a configured root. */
function resolveRemembered(remembered: string | null, roots: FileRoot[], fallback: FileLocation): FileLocation {
  if (!remembered) return fallback
  try {
    const loc = JSON.parse(remembered) as FileLocation
    if (loc.kind !== 'local' && loc.kind !== 'nats') return fallback
    const root = roots.find((r) => {
      if (r.location.kind !== loc.kind) return false
      if (loc.kind === 'local') return isPathWithin(loc.path, r.location.path)
      return r.location.server === loc.server && r.location.bucket === loc.bucket
    })
    return root ? loc : fallback
  } catch {
    return fallback
  }
}

/** The directory part of a path, keeping the platform separator. */
function dirName(path: string): string {
  const idx = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'))
  return idx >= 0 ? path.slice(0, idx) : ''
}

/** Joins a directory and a name, matching the separator already in use. */
function joinPath(dir: string, name: string): string {
  if (dir.endsWith('/') || dir.endsWith('\\')) return dir + name
  return dir + (dir.includes('\\') ? '\\' : '/') + name
}

/** The location a rename would land at, so its thumbnail can be invalidated too. */
function renameTargetLocation(loc: FileLocation, name: string): FileLocation {
  if (loc.kind === 'local') return { ...loc, path: joinPath(dirName(loc.path), name) }
  return { ...loc, name }
}

/** The raw server id behind a NATS source id ("nats:<id>"), as the backend expects. */
function rawServerId(sourceId: string): string {
  return sourceId.startsWith('nats:') ? sourceId.slice(5) : sourceId
}

/** The best location to probe for the root list on open: the active boards folder. */
function defaultLeftLocation(snapshot: StoreSnapshot): FileLocation | null {
  if (snapshot.activeSource === 'local' && snapshot.activeGroup) {
    return { kind: 'local', path: snapshot.activeGroup, server: '', bucket: '', name: '' }
  }
  const local = snapshot.sources.find((s) => s.kind === 'local')
  const firstLocal = local?.groups?.[0]
  if (firstLocal) return { kind: 'local', path: firstLocal.id, server: '', bucket: '', name: '' }
  const nats = snapshot.sources.find((s) => s.kind === 'nats')
  const firstBucket = nats?.groups?.[0]
  if (firstBucket) return { kind: 'nats', path: '', server: rawServerId(nats!.id), bucket: firstBucket.id, name: '' }
  return null
}

/** Whether a location is a real pane (not the empty initial placeholder). */
function isValidPaneLocation(loc: FileLocation): boolean {
  if (loc.kind === 'local') return loc.path !== ''
  if (loc.kind === 'nats') return loc.server !== '' && loc.bucket !== ''
  return false
}

// MARK: - FileManager

/**
 * Morgan le Fayl: a two-pane file manager over the configured boards folders and
 * NATS buckets. The canvas stays mounted underneath (hidden and inert), so the
 * board is never torn down while the panel is open. Each pane is a FileLocation:
 * a local directory or a bucket's object namespace, and every action (list,
 * select, thumbnail, transfer, rename, trash) works across the local↔bucket
 * matrix.
 */
export default function FileManager({ onClose }: { onClose: () => void }) {
  const [view, setView] = useState<ViewMode>(() => (localStorage.getItem('excalibur.fm.view') === 'grid' ? 'grid' : 'list'))
  const [details, setDetails] = useState(() => localStorage.getItem('excalibur.fm.details') === 'true')
  const [showHidden, setShowHidden] = useState(() => localStorage.getItem('excalibur.fm.showHidden') === 'true')
  const [roots, setRoots] = useState<FileRoot[]>([])
  const [panes, setPanes] = useState<{ left: PaneData; right: PaneData }>(() => ({
    left: emptyPane({ kind: 'local', path: '', server: '', bucket: '', name: '' }),
    right: emptyPane({ kind: 'local', path: '', server: '', bucket: '', name: '' }),
  }))
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; paneId: PaneId; entry: FileEntry } | null>(null)
  const [renameDialog, setRenameDialog] = useState<{ paneId: PaneId; entry: FileEntry } | null>(null)
  const [newFolderDialog, setNewFolderDialog] = useState<{ paneId: PaneId } | null>(null)
  const [deleteDialog, setDeleteDialog] = useState<{ paneId: PaneId; entry: FileEntry } | null>(null)
  const [conflictDialog, setConflictDialog] = useState<{ conflicts: FileConflict[]; resolve: (actions: FileAction[] | null) => void } | null>(null)
  /** The server a new bucket is being created on, and the pane to open it in. */
  const [newBucketFor, setNewBucketFor] = useState<{ paneId: PaneId; source: SourceInfo } | null>(null)
  /** Every configured NATS server, so a bucket can be created on the pane's server. */
  const [natsSources, setNatsSources] = useState<SourceInfo[]>([])
  /** The pane the user last worked in; the header's transfer buttons act on it. */
  const [activePane, setActivePane] = useState<PaneId>('left')
  const [lastResult, setLastResult] = useState('')

  // Handlers read the latest panes without re-subscribing to every selection change.
  const panesRef = useRef(panes)
  useEffect(() => {
    panesRef.current = panes
  }, [panes])

  // A per-pane request sequence so a slow listing cannot overwrite a newer one
  // (for example when the user navigates twice quickly).
  const listSeq = useRef<Record<PaneId, number>>({ left: 0, right: 0 })

  // The pane and locations the current drag came from, so a drop back onto the
  // source pane is a no-op and a folder cannot be dropped onto itself.
  const dragSourceRef = useRef<{ paneId: PaneId; locations: FileLocation[]; location: FileLocation } | null>(null)

  useEffect(() => {
    localStorage.setItem('excalibur.fm.view', view)
  }, [view])
  useEffect(() => {
    localStorage.setItem('excalibur.fm.details', String(details))
  }, [details])
  useEffect(() => {
    localStorage.setItem('excalibur.fm.showHidden', String(showHidden))
  }, [showHidden])

  const patchPane = useCallback((id: PaneId, patch: Partial<PaneData>) => {
    setPanes((prev) => ({ ...prev, [id]: { ...prev[id], ...patch } }))
  }, [])

  const listPane = useCallback(
    async (id: PaneId, location: FileLocation, resetSelection: boolean) => {
      const seq = ++listSeq.current[id]
      patchPane(id, { loading: true, error: null })
      try {
        const listing = await listFiles(location)
        // Ignore a response that is no longer the latest request for this pane.
        if (seq !== listSeq.current[id]) return
        setPanes((prev) => {
          const current = prev[id]
          const entries = listing.entries ?? []
          const selection = resetSelection
            ? new Set<string>()
            : new Set(Array.from(current.selection).filter((key) => entries.some((e) => entryKey(e) === key)))
          return {
            ...prev,
            [id]: {
              ...current,
              location: listing.location,
              parent: listing.parent,
              entries,
              loading: false,
              error: null,
              selection,
              anchor: resetSelection ? null : current.anchor,
            },
          }
        })
        setRoots(listing.roots ?? [])
      } catch (err) {
        if (seq !== listSeq.current[id]) return
        patchPane(id, { loading: false, error: String(err) })
      }
    },
    [patchPane],
  )

  const navigate = useCallback(
    (id: PaneId, location: FileLocation) => {
      // Show the new location immediately; the listing fills in the entries.
      patchPane(id, { location, loading: true, error: null, selection: new Set<string>(), anchor: null })
      listPane(id, location, true)
    },
    [patchPane, listPane],
  )

  const refreshPanes = useCallback(async () => {
    const { left, right } = panesRef.current
    await Promise.all([listPane('left', left.location, false), listPane('right', right.location, false)])
  }, [listPane])

  const transfer = useCallback(
    async (sources: FileLocation[], target: FileLocation, copy: boolean) => {
      try {
        const conflicts = await checkFileTransfer(sources, target)
        let actions: FileAction[] = []
        if (conflicts.length > 0) {
          const chosen = await new Promise<FileAction[] | null>((resolve) => setConflictDialog({ conflicts, resolve }))
          if (!chosen) return
          actions = chosen
        }
        const results = await transferFiles({ sources, target, copy, actions })
        // Drop cached previews for every source and destination so an overwritten
        // or renamed file shows its fresh thumbnail, not a stale one.
        invalidateThumbnails(results.flatMap((r) => [r.source, r.target].filter((t): t is FileLocation => t !== null)))
        await refreshPanes()
        refreshBoards().catch((err) => log('error', `Could not refresh the sidebar: ${String(err)}`))
        setLastResult(summarize(results))
      } catch (err) {
        log('error', `Could not transfer files: ${String(err)}`)
        setLastResult(`Error: ${String(err)}`)
      }
    },
    [refreshPanes],
  )

  const handleSelect = useCallback(
    (paneId: PaneId, key: string, mode: SelectMode) => {
      setActivePane(paneId)
      setPanes((prev) => {
        const pane = prev[paneId]
        if (mode === 'single') {
          return { ...prev, [paneId]: { ...pane, selection: new Set([key]), anchor: key } }
        }
        if (mode === 'toggle') {
          const selection = new Set(pane.selection)
          if (selection.has(key)) selection.delete(key)
          else selection.add(key)
          return { ...prev, [paneId]: { ...pane, selection, anchor: key } }
        }
        // Range: extend from the anchor to the clicked entry, in the order the user sees.
        const anchor = pane.anchor ?? key
        const visible = visibleEntries(pane, showHidden)
        const anchorIndex = visible.findIndex((e) => entryKey(e) === anchor)
        const keyIndex = visible.findIndex((e) => entryKey(e) === key)
        if (anchorIndex < 0 || keyIndex < 0) {
          return { ...prev, [paneId]: { ...pane, selection: new Set([key]), anchor: key } }
        }
        const [start, end] = anchorIndex < keyIndex ? [anchorIndex, keyIndex] : [keyIndex, anchorIndex]
        const selection = new Set(visible.slice(start, end + 1).map((e) => entryKey(e)))
        return { ...prev, [paneId]: { ...pane, selection, anchor } }
      })
    },
    [showHidden],
  )

  const handleSort = useCallback((paneId: PaneId, key: SortKey) => {
    setPanes((prev) => {
      const pane = prev[paneId]
      const dir = pane.sort.key === key ? (pane.sort.dir === 'asc' ? 'desc' : 'asc') : 'asc'
      return { ...prev, [paneId]: { ...pane, sort: { key, dir } } }
    })
  }, [])

  const handleContextMenu = useCallback((paneId: PaneId, event: React.MouseEvent, entry: FileEntry) => {
    event.preventDefault()
    setActivePane(paneId)
    const pane = panesRef.current[paneId]
    const key = entryKey(entry)
    if (!pane.selection.has(key)) {
      setPanes((prev) => ({ ...prev, [paneId]: { ...prev[paneId], selection: new Set([key]), anchor: key } }))
    }
    setContextMenu({ x: event.clientX, y: event.clientY, paneId, entry })
  }, [])

  const handleDragStart = useCallback((paneId: PaneId, event: React.DragEvent, entry: FileEntry) => {
    setActivePane(paneId)
    const pane = panesRef.current[paneId]
    const locations = pane.selection.has(entryKey(entry)) ? selectedLocations(pane) : [entry.location]
    dragSourceRef.current = { paneId, locations, location: pane.location }
    event.dataTransfer.setData('text/plain', JSON.stringify(locations))
    event.dataTransfer.effectAllowed = 'copyMove'
  }, [])

  const canDrop = useCallback((target: FileLocation): boolean => {
    const source = dragSourceRef.current
    if (!source) return false
    return !isSelfOrDescendant(source.locations, target)
  }, [])

  const handleDrop = useCallback(
    (event: React.DragEvent, target: FileLocation) => {
      event.preventDefault()
      const payload = event.dataTransfer.getData('text/plain')
      let locations: FileLocation[] = []
      try {
        locations = JSON.parse(payload)
      } catch {
        return
      }
      const source = dragSourceRef.current
      // Dropping back onto the pane the drag started from is a no-op: the
      // selection is already there, and a self-rename would be confusing.
      if (source && sameLocation(target, source.location)) {
        setLastResult('Already in this location')
        return
      }
      // The backend still refuses a folder dropped onto itself or a descendant,
      // but we do not offer the drop client-side either.
      if (isSelfOrDescendant(locations, target)) {
        setLastResult("Can't move a folder into itself")
        return
      }
      if (locations.length) transfer(locations, target, event.altKey)
    },
    [transfer],
  )

  // The header's "Move over" / "Copy over" actions move the active pane's
  // selection into the other pane, the same transfer drag and drop performs.
  const transferAcross = useCallback(
    (copy: boolean) => {
      const other: PaneId = activePane === 'left' ? 'right' : 'left'
      const source = panesRef.current[activePane]
      const target = panesRef.current[other].location
      const locations = selectedLocations(source)
      if (!locations.length) return
      if (sameLocation(source.location, target) || isSelfOrDescendant(locations, target)) {
        setLastResult('Already in this location')
        return
      }
      transfer(locations, target, copy)
    },
    [activePane, transfer],
  )

  // The add button always works inside the pane it sits in: a local directory
  // gets a subfolder, and a bucket pane gets a new bucket on that same server,
  // because a bucket is the only folder a NATS source has. Either way the only
  // input is a name.
  const addFolderHere = useCallback(
    (paneId: PaneId) => {
      const pane = panesRef.current[paneId]
      if (pane.location.kind === 'local') {
        setNewFolderDialog({ paneId })
        return
      }
      const server = pane.location.server
      const known = natsSources.find((source) => rawServerId(source.id) === server)
      const buckets = roots.filter((r) => r.location.kind === 'nats' && r.location.server === server)
      setNewBucketFor({
        paneId,
        source: known ?? {
          id: `nats:${server}`,
          name: server,
          kind: 'nats',
          groups: buckets.map((r) => ({ id: r.label, name: r.label, kind: 'bucket' })),
        },
      })
    },
    [natsSources, roots],
  )

  const addBucketTo = useCallback(
    async (paneId: PaneId, source: SourceInfo, bucket: string) => {
      // Point the pane the button was in at the new bucket: that is the folder
      // the user just asked for.
      const location: FileLocation = { kind: 'nats', path: '', server: rawServerId(source.id), bucket, name: '' }
      await refreshPanes()
      navigate(paneId, location)
    },
    [navigate, refreshPanes],
  )

  const goUp = useCallback(
    (paneId: PaneId) => {
      const pane = panesRef.current[paneId]
      if (pane.parent) navigate(paneId, pane.parent)
    },
    [navigate],
  )

  const handleFolderPick = useCallback(
    (paneId: PaneId, value: string) => {
      try {
        navigate(paneId, JSON.parse(value) as FileLocation)
      } catch {
        // The select only emits serialized roots; ignore anything malformed.
      }
    },
    [navigate],
  )

  const swapPanes = useCallback(() => {
    const { left, right } = panesRef.current
    navigate('left', right.location)
    navigate('right', left.location)
  }, [navigate])

  const refreshAll = useCallback(() => {
    refreshPanes()
  }, [refreshPanes])

  /** Reveal the entry's root boards folder; the backend only exposes revealing a configured folder. */
  const revealEntry = useCallback(
    (paneId: PaneId) => {
      const pane = panesRef.current[paneId]
      const root = findContainingRoot(pane.location, roots)
      if (!root || root.location.kind !== 'local') return
      revealFolder(root.location.path).catch((err) => log('error', `Could not reveal the folder: ${String(err)}`))
    },
    [roots],
  )

  const trashEntry = useCallback(
    async (entry: FileEntry) => {
      try {
        await trashFilePath(entry.location)
        invalidateThumbnails([entry.location])
        await refreshPanes()
        refreshBoards().catch((err) => log('error', `Could not refresh the sidebar: ${String(err)}`))
        setLastResult(entry.location.kind === 'nats' ? `Deleted “${entry.name}”` : `Moved “${entry.name}” to Trash`)
      } catch (err) {
        log('error', `Could not remove “${entry.name}”: ${String(err)}`)
        setLastResult(`Error: ${String(err)}`)
      }
    },
    [refreshPanes],
  )

  const confirmRename = useCallback(
    async (name: string) => {
      if (!renameDialog) return
      const entry = renameDialog.entry
      await renameFilePath(entry.location, name)
      invalidateThumbnails([entry.location, renameTargetLocation(entry.location, name)])
      setRenameDialog(null)
      await refreshPanes()
      refreshBoards().catch((err) => log('error', `Could not refresh the sidebar: ${String(err)}`))
    },
    [renameDialog, refreshPanes],
  )

  const confirmNewFolder = useCallback(
    async (name: string) => {
      if (!newFolderDialog) return
      const dir = panesRef.current[newFolderDialog.paneId].location
      await newFolder(dir, name)
      setNewFolderDialog(null)
      setLastResult(`Created “${name}”`)
      await refreshPanes()
      refreshBoards().catch((err) => log('error', `Could not refresh the sidebar: ${String(err)}`))
    },
    [newFolderDialog, refreshPanes],
  )

  const confirmDelete = useCallback(async () => {
    if (!deleteDialog) return
    const entry = deleteDialog.entry
    setDeleteDialog(null)
    await trashEntry(entry)
  }, [deleteDialog, trashEntry])

  // Load the roots and the remembered pane locations on open. The roots come
  // from a listing (not the snapshot), so we probe the active boards folder
  // first to learn every pickable root, then validate the remembered locations.
  useEffect(() => {
    let active = true
    getState()
      .then(async (snapshot) => {
        const probe = defaultLeftLocation(snapshot)
        if (!probe) {
          if (active) {
            setPanes({
              left: emptyPane({ kind: 'local', path: '', server: '', bucket: '', name: '' }),
              right: emptyPane({ kind: 'local', path: '', server: '', bucket: '', name: '' }),
            })
          }
          return
        }
        const listing = await listFiles(probe)
        if (!active) return
        const roots = listing.roots ?? []
        setRoots(roots)
        if (active) setNatsSources(snapshot.sources.filter((source) => source.kind === 'nats'))
        const localRoots = roots.filter((r) => r.location.kind === 'local')
        const activeRoot = localRoots.find((r) => r.label === snapshot.folder) ?? localRoots[0]
        const defaultLeft = activeRoot?.location ?? roots[0]?.location ?? probe
        const defaultRight = roots.find((r) => !sameLocation(r.location, defaultLeft))?.location ?? defaultLeft
        const left = resolveRemembered(localStorage.getItem('excalibur.fm.left'), roots, defaultLeft)
        const right = resolveRemembered(localStorage.getItem('excalibur.fm.right'), roots, defaultRight)
        setPanes({ left: emptyPane(left), right: emptyPane(right) })
        listPane('left', left, true)
        listPane('right', right, true)
      })
      .catch((err) => log('error', `Could not open Morgan le Fayl: ${String(err)}`))
    return () => {
      active = false
    }
  }, [listPane])

  // Remember the pane locations so the panel reopens where it was left. They are
  // stored as JSON (a FileLocation), not a bare path, so a bucket pane survives.
  useEffect(() => {
    if (isValidPaneLocation(panes.left.location)) localStorage.setItem('excalibur.fm.left', JSON.stringify(panes.left.location))
  }, [panes.left.location])
  useEffect(() => {
    if (isValidPaneLocation(panes.right.location)) localStorage.setItem('excalibur.fm.right', JSON.stringify(panes.right.location))
  }, [panes.right.location])

  // Escape closes the panel, unless a dialog is open on top of it.
  useEffect(() => {
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      if (contextMenu) {
        setContextMenu(null)
        return
      }
      if (conflictDialog) {
        conflictDialog.resolve(null)
        setConflictDialog(null)
        return
      }
      if (renameDialog) {
        setRenameDialog(null)
        return
      }
      if (newFolderDialog) {
        setNewFolderDialog(null)
        return
      }
      if (deleteDialog) {
        setDeleteDialog(null)
        return
      }
      if (newBucketFor) {
        // Creating a bucket opens a modal of its own, which also reacts to Escape.
        setNewBucketFor(null)
        return
      }
      onClose()
    }
    window.addEventListener('keydown', closeOnEscape)
    return () => window.removeEventListener('keydown', closeOnEscape)
  }, [contextMenu, conflictDialog, renameDialog, newFolderDialog, deleteDialog, newBucketFor, onClose])

  // Close the context menu on a click outside it (same capture-close pattern as the shell).
  useEffect(() => {
    if (!contextMenu) return
    const close = (event: Event) => {
      if (event.target instanceof Element && event.target.closest('.context-menu')) return
      setContextMenu(null)
    }
    window.addEventListener('pointerdown', close, true)
    return () => window.removeEventListener('pointerdown', close, true)
  }, [contextMenu])

  const openEntry = (menu: { paneId: PaneId; entry: FileEntry }) => {
    if (menu.entry.isDir) navigate(menu.paneId, menu.entry.location)
    else revealEntry(menu.paneId)
  }

  const moveToOther = (menu: { paneId: PaneId }, copy: boolean) => {
    const pane = panesRef.current[menu.paneId]
    const other = menu.paneId === 'left' ? panesRef.current.right : panesRef.current.left
    transfer(selectedLocations(pane), other.location, copy)
  }

  // "Move over" / "Copy over" act on the selection in the pane the user last
  // worked in (one file or many), and land in the other pane.
  const activeData = panes[activePane]
  const otherData = panes[activePane === 'left' ? 'right' : 'left']
  const selectionCount = activeData.selection.size
  const canTransferAcross =
    selectionCount > 0 &&
    isValidPaneLocation(activeData.location) &&
    isValidPaneLocation(otherData.location) &&
    !sameLocation(activeData.location, otherData.location)

  return (
    <div className="fm-panel" role="dialog" aria-label="Morgan le Fayl">
      <header className="fm-header">
        <span className="fm-title">
          <FilesIcon aria-hidden="true" />
          <span>Morgan le Fayl</span>
        </span>
        {selectionCount > 0 && (
          <div className="fm-transfer-actions">
            <button
              className="secondary"
              disabled={!canTransferAcross}
              onClick={() => transferAcross(false)}
              title={`Move ${selectionCount === 1 ? 'the selected item' : `${selectionCount} items`} to the other pane`}
            >
              Move over
            </button>
            <button
              className="secondary"
              disabled={!canTransferAcross}
              onClick={() => transferAcross(true)}
              title={`Copy ${selectionCount === 1 ? 'the selected item' : `${selectionCount} items`} to the other pane`}
            >
              Copy over
            </button>
          </div>
        )}
        <div className="fm-toolbar">
          <div className="segmented" role="radiogroup" aria-label="View">
            <button role="radio" aria-checked={view === 'list'} className={view === 'list' ? 'active' : ''} onClick={() => setView('list')} title="List view" aria-label="List view">
              <ListIcon />
            </button>
            <button role="radio" aria-checked={view === 'grid'} className={view === 'grid' ? 'active' : ''} onClick={() => setView('grid')} title="Grid view" aria-label="Grid view">
              <GridIcon />
            </button>
          </div>
          <button className={`icon-button${details ? ' on' : ''}`} disabled={view === 'grid'} onClick={() => setDetails((d) => !d)} title="Toggle details" aria-label="Toggle details" aria-pressed={details}>
            <DetailsIcon />
          </button>
          <button className={`icon-button${showHidden ? ' on' : ''}`} onClick={() => setShowHidden((h) => !h)} title="Show hidden files" aria-label="Show hidden files" aria-pressed={showHidden}>
            <EyeIcon />
          </button>
          <button className="icon-button" onClick={swapPanes} title="Swap panes" aria-label="Swap panes">
            <SwapIcon />
          </button>
          <button className="icon-button" onClick={refreshAll} title="Refresh" aria-label="Refresh">
            <RefreshIcon />
          </button>
          <button className="icon-button" onClick={onClose} title="Close (Esc)" aria-label="Close Morgan le Fayl">
            <CloseIcon />
          </button>
        </div>
      </header>

      <div className="fm-panes">
        <Pane
          pane={panes.left}
          roots={roots}
          view={view}
          details={details}
          showHidden={showHidden}
          onNavigate={(location) => navigate('left', location)}
          onSelect={(key, mode) => handleSelect('left', key, mode)}
          onSort={(key) => handleSort('left', key)}
          onContextMenu={(e, entry) => handleContextMenu('left', e, entry)}
          onDrop={(e, target) => handleDrop(e, target)}
          onDragStart={(e, entry) => handleDragStart('left', e, entry)}
          onGoUp={() => goUp('left')}
          onFolderPick={(value) => handleFolderPick('left', value)}
          onAddFolder={() => addFolderHere('left')}
          canDrop={canDrop}
        />
        <Pane
          pane={panes.right}
          roots={roots}
          view={view}
          details={details}
          showHidden={showHidden}
          onNavigate={(location) => navigate('right', location)}
          onSelect={(key, mode) => handleSelect('right', key, mode)}
          onSort={(key) => handleSort('right', key)}
          onContextMenu={(e, entry) => handleContextMenu('right', e, entry)}
          onDrop={(e, target) => handleDrop(e, target)}
          onDragStart={(e, entry) => handleDragStart('right', e, entry)}
          onGoUp={() => goUp('right')}
          onFolderPick={(value) => handleFolderPick('right', value)}
          onAddFolder={() => addFolderHere('right')}
          canDrop={canDrop}
        />
      </div>

      <footer className="fm-footer">
        <span>{footerText(panes.left)}</span>
        <span>{footerText(panes.right)}</span>
        <span className="fm-result">{lastResult}</span>
      </footer>

      {contextMenu && (
        <div className="context-menu" style={{ left: contextMenu.x, top: contextMenu.y }} role="menu">
          {contextMenu.entry.location.kind === 'local' && (
            <button role="menuitem" onClick={() => { openEntry(contextMenu); setContextMenu(null) }}>
              {contextMenu.entry.isDir ? 'Open' : `Reveal in ${FILE_MANAGER}`}
            </button>
          )}
          <button role="menuitem" onClick={() => { moveToOther(contextMenu, false); setContextMenu(null) }}>
            Move to other pane
          </button>
          <button role="menuitem" onClick={() => { moveToOther(contextMenu, true); setContextMenu(null) }}>
            Copy to other pane
          </button>
          <div className="context-menu-divider" />
          <button role="menuitem" onClick={() => { setRenameDialog({ paneId: contextMenu.paneId, entry: contextMenu.entry }); setContextMenu(null) }}>
            Rename…
          </button>
          {contextMenu.entry.location.kind === 'local' ? (
            <button role="menuitem" onClick={() => { setNewFolderDialog({ paneId: contextMenu.paneId }); setContextMenu(null) }}>
              New Folder…
            </button>
          ) : (
            <button
              role="menuitem"
              onClick={() => {
                const paneId = contextMenu.paneId
                setContextMenu(null)
                addFolderHere(paneId)
              }}
            >
              New Bucket on this server…
            </button>
          )}
          {contextMenu.entry.location.kind === 'local' && (
            <button role="menuitem" onClick={() => { revealEntry(contextMenu.paneId); setContextMenu(null) }}>
              Show boards folder in {FILE_MANAGER}
            </button>
          )}
          <div className="context-menu-divider" />
          <button
            role="menuitem"
            className="destructive"
            onClick={() => {
              if (contextMenu.entry.location.kind === 'nats') setDeleteDialog({ paneId: contextMenu.paneId, entry: contextMenu.entry })
              else trashEntry(contextMenu.entry)
              setContextMenu(null)
            }}
          >
            {contextMenu.entry.location.kind === 'nats' ? 'Delete Permanently…' : 'Move to Trash'}
          </button>
        </div>
      )}

      {newBucketFor && (
        <NewBucketDialog
          source={newBucketFor.source}
          note={`A bucket has no folders, so this creates a new bucket on ${newBucketFor.source.name} instead of a subfolder.`}
          onClose={() => setNewBucketFor(null)}
          onCreated={(bucket) => addBucketTo(newBucketFor.paneId, newBucketFor.source, bucket)}
        />
      )}
      {renameDialog && (
        <NameDialog
          title="Rename"
          confirmTitle="Rename"
          initialName={renameDialog.entry.name}
          onConfirm={confirmRename}
          onCancel={() => setRenameDialog(null)}
        />
      )}
      {newFolderDialog && (
        <NameDialog
          title="New Folder"
          confirmTitle="Create"
          initialName=""
          note={`This creates a subfolder inside ${newFolderTargetLabel(panes[newFolderDialog.paneId].location, roots)}.`}
          onConfirm={confirmNewFolder}
          onCancel={() => setNewFolderDialog(null)}
        />
      )}
      {deleteDialog && (
        <ConfirmDialog
          title="Delete Permanently"
          message={`“${deleteDialog.entry.name}” will be deleted permanently from the bucket. There is no undo.`}
          confirmTitle="Delete"
          onConfirm={confirmDelete}
          onCancel={() => setDeleteDialog(null)}
        />
      )}
      {conflictDialog && (
        <ConflictDialog
          conflicts={conflictDialog.conflicts}
          onConfirm={(actions) => {
            conflictDialog.resolve(actions)
            setConflictDialog(null)
          }}
          onCancel={() => {
            conflictDialog.resolve(null)
            setConflictDialog(null)
          }}
        />
      )}
    </div>
  )
}

// MARK: - Pane

function Pane({
  pane,
  roots,
  view,
  details,
  showHidden,
  onNavigate,
  onSelect,
  onSort,
  onContextMenu,
  onDrop,
  onDragStart,
  onGoUp,
  onFolderPick,
  onAddFolder,
  canDrop,
}: {
  pane: PaneData
  roots: FileRoot[]
  view: ViewMode
  details: boolean
  showHidden: boolean
  onNavigate: (location: FileLocation) => void
  onSelect: (key: string, mode: SelectMode) => void
  onSort: (key: SortKey) => void
  onContextMenu: (event: React.MouseEvent, entry: FileEntry) => void
  onDrop: (event: React.DragEvent, target: FileLocation) => void
  onDragStart: (event: React.DragEvent, entry: FileEntry) => void
  onGoUp: () => void
  onFolderPick: (value: string) => void
  onAddFolder: () => void
  canDrop: (target: FileLocation) => boolean
}) {
  const [dragOver, setDragOver] = useState<string | null>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const visible = useMemo(() => visibleEntries(pane, showHidden), [pane, showHidden])
  const containingRoot = useMemo(() => findContainingRoot(pane.location, roots), [pane.location, roots])
  const pickerLabel = useMemo(() => pickerLabelFor(pane.location, containingRoot), [pane.location, containingRoot])
  const pickerValue = containingRoot ? JSON.stringify(containingRoot.location) : ''

  const localRoots = roots.filter((r) => r.location.kind === 'local')
  const natsGroups = useMemo(() => {
    const groups: { group: string; roots: FileRoot[] }[] = []
    for (const r of roots) {
      if (r.location.kind !== 'nats') continue
      const existing = groups.find((g) => g.group === r.group)
      if (existing) existing.roots.push(r)
      else groups.push({ group: r.group, roots: [r] })
    }
    return groups
  }, [roots])

  const scrollToEntry = (key: string) => {
    requestAnimationFrame(() => {
      listRef.current?.querySelector(`[data-path="${CSS.escape(key)}"]`)?.scrollIntoView({ block: 'nearest' })
    })
  }

  const handleKeyDown = (event: React.KeyboardEvent) => {
    if (visible.length === 0) return
    const currentIndex = pane.selection.size === 1 ? visible.findIndex((e) => pane.selection.has(entryKey(e))) : -1
    let nextIndex = -1
    if (event.key === 'ArrowDown') nextIndex = Math.min(visible.length - 1, currentIndex + 1)
    else if (event.key === 'ArrowUp') nextIndex = Math.max(0, currentIndex - 1)
    else if (event.key === 'Home') nextIndex = 0
    else if (event.key === 'End') nextIndex = visible.length - 1
    else if (event.key === 'Enter') {
      if (currentIndex >= 0 && visible[currentIndex].isDir) onNavigate(visible[currentIndex].location)
      return
    } else if (event.key === 'Backspace' || (event.metaKey && event.key === 'ArrowUp')) {
      event.preventDefault()
      onGoUp()
      return
    } else return
    if (nextIndex >= 0) {
      event.preventDefault()
      onSelect(entryKey(visible[nextIndex]), 'single')
      scrollToEntry(entryKey(visible[nextIndex]))
    }
  }

  const handleDragOver = (event: React.DragEvent, target: FileLocation, targetKey: string) => {
    // Do not offer the drop when the target is a dragged folder or its
    // descendant; the backend would refuse it anyway.
    if (!canDrop(target)) return
    event.preventDefault()
    event.dataTransfer.dropEffect = event.altKey ? 'copy' : 'move'
    setDragOver(targetKey)
  }

  const handleDrop = (event: React.DragEvent, target: FileLocation) => {
    event.preventDefault()
    setDragOver(null)
    onDrop(event, target)
  }

  const handleRowClick = (event: React.MouseEvent, entry: FileEntry) => {
    const key = entryKey(entry)
    if (event.shiftKey) onSelect(key, 'range')
    else if (event.metaKey || event.ctrlKey) onSelect(key, 'toggle')
    else onSelect(key, 'single')
  }

  const renderEntry = (entry: FileEntry) => {
    const key = entryKey(entry)
    const selected = pane.selection.has(key)
    const dropActive = dragOver === key
    const common = {
      key,
      'data-path': key,
      className: `${view === 'grid' ? 'fm-card' : 'fm-row'}${selected ? ' selected' : ''}${dropActive ? ' fm-drop-active' : ''}`,
      role: 'option',
      'aria-selected': selected,
      onClick: (e: React.MouseEvent) => handleRowClick(e, entry),
      onDoubleClick: () => {
        if (entry.isDir) onNavigate(entry.location)
      },
      onContextMenu: (e: React.MouseEvent) => onContextMenu(e, entry),
      draggable: true,
      onDragStart: (e: React.DragEvent) => onDragStart(e, entry),
      onDragOver: (e: React.DragEvent) => {
        if (entry.isDir) {
          e.stopPropagation()
          handleDragOver(e, entry.location, key)
        }
      },
      onDragLeave: () => setDragOver((v) => (v === key ? null : v)),
      onDrop: (e: React.DragEvent) => {
        if (entry.isDir) {
          e.stopPropagation()
          handleDrop(e, entry.location)
        }
      },
    }
    if (view === 'grid') {
      return (
        <div {...common}>
          <div className="fm-card-thumb">
            <Thumbnail entry={entry} />
          </div>
          <div className="fm-card-name" title={entry.name}>
            {entry.name}
          </div>
          {entry.isDir && <span className="fm-card-badge">Folder</span>}
          {!entry.isDir && entry.size > LARGE_FILE && <span className="fm-card-badge">{humanSize(entry.size)}</span>}
        </div>
      )
    }
    return (
      <div {...common}>
        <span className="fm-icon">{entry.isDir ? <FolderIcon /> : <FileIcon />}</span>
        <span className="fm-name" title={entry.name}>
          {entry.name}
        </span>
        {details && <span className="fm-size">{entry.isDir ? '—' : humanSize(entry.size)}</span>}
        {details && <span className="fm-modified">{formatDate(entry.modified)}</span>}
      </div>
    )
  }

  return (
    <section className="fm-pane" aria-label="Files">
      <div className="fm-pane-header">
        <label className="fm-picker" title={pickerLabel}>
          {pane.location.kind === 'nats' ? <CloudIcon aria-hidden="true" /> : <FolderIcon aria-hidden="true" />}
          <span className="fm-picker-label">{pickerLabel}</span>
          <ChevronDownIcon aria-hidden="true" />
          <select value={pickerValue} onChange={(e) => onFolderPick(e.target.value)} aria-label="Location">
            <optgroup label="Local">
              {localRoots.map((r) => (
                <option key={locationKey(r.location)} value={JSON.stringify(r.location)}>
                  {r.label}
                </option>
              ))}
            </optgroup>
            {natsGroups.map((g) => (
              <optgroup key={g.group} label={g.group}>
                {g.roots.map((r) => (
                  <option key={locationKey(r.location)} value={JSON.stringify(r.location)}>
                    {r.label}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </label>
        <button
          className="icon-button"
          onClick={onAddFolder}
          title={pane.location.kind === 'nats' ? 'New bucket on this server' : 'New folder here'}
          aria-label={pane.location.kind === 'nats' ? 'New bucket on this server' : 'New folder here'}
        >
          <FolderPlusIcon />
        </button>
        {pane.location.kind === 'local' && (
          <div className="fm-path">
            <span className="fm-path-text" title={pane.location.path}>
              {relativePath(pane.location.path, containingRoot?.location.path ?? '')}
            </span>
            <button className="icon-button" onClick={onGoUp} disabled={!pane.parent} title="Up" aria-label="Up">
              <ArrowUpIcon />
            </button>
          </div>
        )}
      </div>
      {pane.loading ? (
        <div className="fm-empty">
          <p className="hint">Loading…</p>
        </div>
      ) : pane.error ? (
        <div className="fm-empty">
          <p className="problem">{pane.error}</p>
        </div>
      ) : visible.length === 0 ? (
        <div
          className={`fm-empty${dragOver === '__pane__' ? ' fm-drop-active' : ''}`}
          onDragOver={(e) => handleDragOver(e, pane.location, '__pane__')}
          onDragLeave={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node)) setDragOver(null)
          }}
          onDrop={(e) => handleDrop(e, pane.location)}
        >
          <p className="hint">{pane.location.kind === 'nats' ? 'This bucket is empty.' : 'This folder is empty.'}</p>
        </div>
      ) : (
        <div
          ref={listRef}
          className={`${view === 'grid' ? 'fm-grid' : 'fm-list'}${dragOver === '__pane__' ? ' fm-drop-active' : ''}`}
          role="listbox"
          aria-label="Files"
          tabIndex={0}
          onKeyDown={handleKeyDown}
          onDragOver={(e) => handleDragOver(e, pane.location, '__pane__')}
          onDragLeave={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node)) setDragOver(null)
          }}
          onDrop={(e) => handleDrop(e, pane.location)}
        >
          {details && view === 'list' && (
            <div className="fm-row fm-row-header" role="presentation">
              <button className="fm-col-name" onClick={() => onSort('name')}>
                Name{pane.sort.key === 'name' ? (pane.sort.dir === 'asc' ? ' ↑' : ' ↓') : ''}
              </button>
              <button className="fm-col-size" onClick={() => onSort('size')}>
                Size{pane.sort.key === 'size' ? (pane.sort.dir === 'asc' ? ' ↑' : ' ↓') : ''}
              </button>
              <button className="fm-col-modified" onClick={() => onSort('modified')}>
                Modified{pane.sort.key === 'modified' ? (pane.sort.dir === 'asc' ? ' ↑' : ' ↓') : ''}
              </button>
            </div>
          )}
          {visible.map(renderEntry)}
        </div>
      )}
    </section>
  )
}

// MARK: - Thumbnail

function Thumbnail({ entry }: { entry: FileEntry }) {
  const [dataUrl, setDataUrl] = useState<string | null>(null)
  const key = locationKey(entry.location)
  useEffect(() => {
    let active = true
    if (entry.kind === 'folder') return
    requestThumbnail(entry.location).then((value) => {
      if (active) setDataUrl(value)
    })
    return () => {
      active = false
    }
  }, [key, entry.kind])
  if (entry.kind === 'folder') return <FolderIcon className="fm-thumb-icon" />
  if (dataUrl) return <img src={dataUrl} alt="" draggable={false} />
  return <FileIcon className="fm-thumb-icon" />
}

// MARK: - Dialogs

/** A small modal for a single text value, used for Rename and New Folder. */
function NameDialog({
  title,
  confirmTitle,
  initialName,
  note,
  onConfirm,
  onCancel,
}: {
  title: string
  confirmTitle: string
  initialName: string
  /** A standing note shown above the input, e.g. where the new item will land. */
  note?: string
  onConfirm: (name: string) => Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState(initialName)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [])
  const trimmed = name.trim()
  const problem = !trimmed ? 'Enter a name.' : null
  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    setError(null)
    try {
      await onConfirm(trimmed)
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }
  return (
    <div className="modal-backdrop" onMouseDown={(e) => { if (e.target === e.currentTarget && !busy) onCancel() }}>
      <div className="modal" role="dialog" aria-modal="true">
        <h2>{title}</h2>
        {note && <p className="modal-note">{note}</p>}
        <input
          ref={inputRef}
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') confirm()
            if (e.key === 'Escape') {
              e.stopPropagation()
              onCancel()
            }
          }}
        />
        <p className={error || problem ? 'problem' : 'hint'}>{error ?? problem ?? ''}</p>
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null || busy} onClick={confirm}>
            {busy ? 'Working…' : confirmTitle}
          </button>
        </div>
      </div>
    </div>
  )
}

/** A small modal for a destructive action that has no undo, used for bucket deletes. */
function ConfirmDialog({
  title,
  message,
  confirmTitle,
  onConfirm,
  onCancel,
}: {
  title: string
  message: string
  confirmTitle: string
  onConfirm: () => Promise<void>
  onCancel: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const confirm = async () => {
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      await onConfirm()
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }
  return (
    <div className="modal-backdrop" onMouseDown={(e) => { if (e.target === e.currentTarget && !busy) onCancel() }}>
      <div className="modal" role="dialog" aria-modal="true">
        <h2>{title}</h2>
        <p className="hint">{message}</p>
        {error && <p className="problem">{error}</p>}
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          <button className="primary" disabled={busy} onClick={confirm}>
            {busy ? 'Working…' : confirmTitle}
          </button>
        </div>
      </div>
    </div>
  )
}

/** Lists the colliding names with a per-file Skip / Overwrite / Rename choice. */
function ConflictDialog({
  conflicts,
  onConfirm,
  onCancel,
}: {
  conflicts: FileConflict[]
  onConfirm: (actions: FileAction[]) => void
  onCancel: () => void
}) {
  const [actions, setActions] = useState<Record<string, FileAction['action']>>(() => Object.fromEntries(conflicts.map((c) => [locationKey(c.source), 'skip'])))
  const [applyToAll, setApplyToAll] = useState(false)
  const setAction = (key: string, action: FileAction['action']) => {
    setActions((prev) => {
      const next = { ...prev, [key]: action }
      if (applyToAll) for (const c of conflicts) next[locationKey(c.source)] = action
      return next
    })
  }
  const confirm = () => {
    onConfirm(conflicts.map((c) => ({ source: c.source, action: actions[locationKey(c.source)] ?? 'skip' })))
  }
  return (
    <div className="modal-backdrop" onMouseDown={(e) => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal fm-conflict" role="dialog" aria-modal="true">
        <h2>Name conflicts</h2>
        <p className="hint">Some files already exist in the destination. Choose what to do for each.</p>
        <div className="fm-conflict-list">
          {conflicts.map((c) => {
            const key = locationKey(c.source)
            return (
              <div key={key} className="fm-conflict-row">
                <span className="fm-conflict-name" title={c.name}>
                  {c.name}
                </span>
                <div className="segmented" role="radiogroup" aria-label={`Action for ${c.name}`}>
                  {(['skip', 'overwrite', 'rename'] as const).map((action) => (
                    <button
                      key={action}
                      role="radio"
                      aria-checked={actions[key] === action}
                      className={actions[key] === action ? 'active' : ''}
                      onClick={() => setAction(key, action)}
                    >
                      {action === 'rename' ? 'Rename' : action === 'overwrite' ? 'Overwrite' : 'Skip'}
                    </button>
                  ))}
                </div>
              </div>
            )
          })}
        </div>
        <label className="fm-conflict-apply">
          <input type="checkbox" checked={applyToAll} onChange={(e) => setApplyToAll(e.target.checked)} />
          Apply to all
        </label>
        <div className="modal-actions">
          <button className="secondary" onClick={onCancel}>
            Cancel
          </button>
          <button className="primary" onClick={confirm}>
            Continue
          </button>
        </div>
      </div>
    </div>
  )
}
