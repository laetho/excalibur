import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import Canvas, { PERSISTENCE_LABELS, type SyncStatus } from './Canvas'
import { AVATAR_EMOJIS, COLOR_PRESETS, peerColor } from './collabIdentity'
import { RemoteCursor } from './RemoteCursors'
import { useToolbarRoom } from './toolbarRoom'
import {
  AppearanceIcon,
  ArrowUpIcon,
  ChevronDownIcon,
  CloseIcon,
  CloudIcon,
  FolderIcon,
  LogoMark,
  InfoIcon,
  MinusIcon,
  PinIcon,
  PlusIcon,
  RefreshIcon,
  SearchIcon,
  SidebarIcon,
  SparklesIcon,
  UsersIcon,
} from './icons'
import {
  activateSource,
  addBucket,
  addNatsServer,
  aiChat,
  addLocalFolder,
  addLocalFolderPath,
  chooseImport,
  chooseNatsCredentialsFile,
  copyBoardToSource,
  createBoard,
  deleteBucket,
  discoverAIModels,
  dismissAlert,
  duplicateBoard,
  getState,
  importBoardData,
  log,
  onBoardModified,
  onAIStream,
  onMenu,
  onStoreChanged,
  removeNatsServer,
  updateNatsServer,
  refreshBuckets,
  renameBoard,
  reveal,
  removeLocalFolder,
  revealFolder,
  selectBoard,
  setCanvasAppearance,
  setApplicationZoom,
  appVersion,
  type AppVersionInfo,
  setCollaborationNickname,
  setCollaborationAppearance,
  setAISettings,
  setShowThumbnails,
  setSort,
  setWindowTitle,
  showAlert,
  togglePinnedGroup,
  trashBoard,
  type BoardInfo,
  type BoardSortOrder,
  type CanvasAppearance,
  type GroupInfo,
  type AIChatMessage,
  type AIModel,
  type NatsServerConfig,
  type NatsServerSettings,
  type PinnedGroupInfo,
  type SourceInfo,
  type StoreSnapshot,
} from './ipc'

const FILE_EXTENSION = 'excalidraw'
/** How long a board's "Edited … ago" stays current before the sidebar refreshes it. */
const NOW_TICK_MS = 30_000

// MARK: - Helpers

/** How the sidebar orders the boards (View → Sort Boards By). Name: A–Z; date: most recent first, ties by name. */
function sortBoards(boards: BoardInfo[], order: BoardSortOrder): BoardInfo[] {
  const byName = (a: BoardInfo, b: BoardInfo) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  const sorted = [...boards]
  if (order === 'name') sorted.sort(byName)
  else sorted.sort((a, b) => (a.modified !== b.modified ? b.modified - a.modified : byName(a, b)))
  return sorted
}

type Match = { kind: 'name' } | { kind: 'text'; excerpt: string }

/** Whether a board matches the sidebar search: by name, or by text on the board. */
function searchMatch(board: BoardInfo, query: string): Match | null {
  const q = query.toLowerCase()
  if (board.name.toLowerCase().includes(q)) return { kind: 'name' }
  const text = board.texts.find((t) => t.toLowerCase().includes(q))
  if (!text) return null
  return { kind: 'text', excerpt: snippet(text, query) }
}

/** One line of text around the first match, e.g. "…the deploy pipeline runs…". */
function snippet(text: string, query: string, radius = 30): string {
  const flat = text.split(/\r?\n/).join(' ')
  const index = flat.toLowerCase().indexOf(query.toLowerCase())
  if (index < 0) return flat.slice(0, radius * 2)
  const start = Math.max(0, index - radius)
  const end = Math.min(flat.length, index + query.length + radius)
  return (start > 0 ? '…' : '') + flat.slice(start, end) + (end < flat.length ? '…' : '')
}

/** "Edited 3 minutes ago", relative to `now` (unix seconds) so the sidebar can refresh it. */
function editedText(modified: number, now: number): string {
  const seconds = Math.max(0, Math.floor(now - modified))
  if (seconds < 60) return 'Edited just now'
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `Edited ${minutes} minute${minutes === 1 ? '' : 's'} ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `Edited ${hours} hour${hours === 1 ? '' : 's'} ago`
  const days = Math.floor(hours / 24)
  if (days < 7) return `Edited ${days} day${days === 1 ? '' : 's'} ago`
  const weeks = Math.floor(days / 7)
  if (weeks < 5) return `Edited ${weeks} week${weeks === 1 ? '' : 's'} ago`
  const months = Math.floor(days / 30)
  if (months < 12) return `Edited ${months} month${months === 1 ? '' : 's'} ago`
  const years = Math.floor(days / 365)
  return `Edited ${years} year${years === 1 ? '' : 's'} ago`
}

/** Mirrors the store's name validation (BoardStore.validationMessage). */
function validateName(name: string, boards: BoardInfo[], excludingId?: string): string | null {
  const trimmed = name.trim()
  if (!trimmed) return 'Enter a name.'
  if (trimmed.includes('/') || trimmed.includes(':')) return 'Names can’t contain “/” or “:”.'
  if (trimmed.startsWith('.')) return 'Names can’t start with a period.'
  if (Array.from(trimmed).length > 120) return 'That name is too long.'
  const taken = boards.some((b) => b.id !== excludingId && b.name.toLowerCase() === trimmed.toLowerCase())
  if (taken) return 'A board with this name already exists.'
  return null
}

/** The name shown for the local boards folder, in the sidebar and in Settings. */
const SIDEBAR_COLLAPSED_KEY = 'excalibur.sidebarCollapsed'

function folderDisplayName(path: string): string {
  return lastPathComponent(path) || path
}

function lastPathComponent(path: string): string {
  const parts = path.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? path
}

const APPEARANCE_TITLES: Record<CanvasAppearance, string> = { system: 'System', light: 'Light', dark: 'Dark' }

const PLATFORM = typeof navigator === 'undefined' ? '' : navigator.platform || navigator.userAgent
const IS_MAC = /Mac/i.test(PLATFORM)
/** The platform's name for its file manager, for "Show in …" actions. */
const FILE_MANAGER = IS_MAC ? 'Finder' : /Win/i.test(PLATFORM) ? 'File Explorer' : 'File Manager'

/** The raw server id behind a NATS source id ("nats:<id>"). */
function rawServerId(sourceId: string): string {
  return sourceId.startsWith('nats:') ? sourceId.slice(5) : sourceId
}

/** A fresh id for a NATS server the user is adding (the backend keys servers by id). */
function newServerId(): string {
  return `nats-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`
}

// MARK: - Shell

/** The app shell: sidebar (boards, search, thumbnails) + the always-mounted canvas. */
export default function Shell() {
  const [snapshot, setSnapshot] = useState<StoreSnapshot | null>(null)
  /** The find filter per folder or bucket, so each group returns to its own search. */
  const [queries, setQueries] = useState<Record<string, string>>({})
  /** Refreshed every 30 s so "Edited … ago" stays current. */
  const [now, setNow] = useState(() => Date.now() / 1000)
  const [creating, setCreating] = useState(false)
  const [renaming, setRenaming] = useState<BoardInfo | null>(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  /** The open board's sync status when the canvas has no room to show it; the sidebar shows it instead. */
  const [sidebarSyncStatus, setSidebarSyncStatus] = useState<SyncStatus | null>(null)
  /** Collapsed to a rail with just the logo; remembered between sessions. */
  const [sidebarCollapsed, setSidebarCollapsed] = useState(() => localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === 'true')
  const setCollapsed = (collapsed: boolean) => {
    setSidebarCollapsed(collapsed)
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(collapsed))
  }
  const [addServerOpen, setAddServerOpen] = useState(false)
  /** True while the sources refresh button is re-listing NATS buckets. */
  const [refreshingSources, setRefreshingSources] = useState(false)
  /** Source currently being activated; backend scans stay asynchronous and visible. */
  const [switchingSource, setSwitchingSource] = useState<string | null>(null)
  const [dropTargeted, setDropTargeted] = useState(false)
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; board: BoardInfo } | null>(null)
  /** Right-click menu on a NATS server heading in the sources list. */
  const [sourceMenu, setSourceMenu] = useState<{ x: number; y: number; source: SourceInfo } | null>(null)
  /** Right-click menu on one local boards folder. */
  const [folderMenu, setFolderMenu] = useState<{ x: number; y: number; folder: string } | null>(null)
  /** Right-click menu on one NATS Object Store bucket. */
  const [bucketMenu, setBucketMenu] = useState<{ x: number; y: number; source: SourceInfo; group: GroupInfo } | null>(null)
  const [deletingBucket, setDeletingBucket] = useState<{ source: SourceInfo; group: GroupInfo } | null>(null)
  const [bucketDeleteBusy, setBucketDeleteBusy] = useState(false)
  const [bucketDeleteError, setBucketDeleteError] = useState<string | null>(null)
  /** The NATS server a new bucket is being created on. */
  const [newBucketFor, setNewBucketFor] = useState<SourceInfo | null>(null)
  const [removingServer, setRemovingServer] = useState<SourceInfo | null>(null)
  const [aiOpen, setAIOpen] = useState(false)
  const closeAI = useCallback(() => setAIOpen(false), [])

  // Menu handlers and drop handlers read the latest snapshot without re-subscribing.
  const snapshotRef = useRef<StoreSnapshot | null>(null)
  useEffect(() => {
    snapshotRef.current = snapshot
  }, [snapshot])

  /** The switch in flight, read by click handlers without re-rendering them. */
  const switchingRef = useRef<string | null>(null)
  const handleActivateSource = useCallback((sourceId: string, groupId: string): Promise<void> | undefined => {
    const current = snapshotRef.current
    // One switch at a time: further clicks are ignored until the boards have loaded.
    if (!current || switchingRef.current || (current.activeSource === sourceId && current.activeGroup === groupId)) return undefined
    const key = `${sourceId}\u0000${groupId}`
    switchingRef.current = key
    setSwitchingSource(key)
    const finish = () => {
      if (switchingRef.current !== key) return
      switchingRef.current = null
      setSwitchingSource(null)
    }
    // On success the switch ends when the snapshot shows the new group (the effect below), not when the
    // call returns: the two can arrive in either order, and ending early would flash the old group open.
    return activateSource(sourceId, groupId)
      .then(() => {
        setTimeout(finish, 2000) // safety net if that snapshot never comes
      })
      .catch((err: unknown) => {
        log('error', `Could not switch source: ${String(err)}`)
        finish()
      })
  }, [])

  useEffect(() => {
    if (snapshot && switchingRef.current === `${snapshot.activeSource}\u0000${snapshot.activeGroup}`) {
      switchingRef.current = null
      setSwitchingSource(null)
    }
  }, [snapshot])

  const handleAddFolder = useCallback(() => {
    addLocalFolder().catch((err: unknown) => log('error', `Could not add the folder: ${String(err)}`))
  }, [])

  const handleRemoveFolder = useCallback((folder: string) => {
    removeLocalFolder(folder).catch((err: unknown) => log('error', `Could not remove the folder: ${String(err)}`))
  }, [])

  const handleMenu = useCallback((id: string) => {
    const current = snapshotRef.current
    if (!current) return
    switch (id) {
      case 'undo':
      case 'redo': {
        // Native GTK menu roles swallow these accelerators before the webview
        // receives them. Re-emit the action where Excalidraw's history handler
        // owns the scene and can preserve collaboration semantics.
        const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
        document.dispatchEvent(new KeyboardEvent('keydown', {
          key: 'z',
          code: 'KeyZ',
          ctrlKey: !isMac,
          metaKey: isMac,
          shiftKey: id === 'redo',
          bubbles: true,
          cancelable: true,
        }))
        break
      }
      case 'new-board':
        setCreating(true)
        break
      case 'import':
        chooseImport()
        break
      case 'add-folder':
        handleAddFolder()
        break
      case 'reveal':
        reveal(current.selection)
        break
      case 'rename': {
        const board = current.boards.find((b) => b.id === current.selection)
        if (board) setRenaming(board)
        break
      }
      case 'duplicate':
        if (current.selection) duplicateBoard(current.selection)
        break
      case 'trash':
        if (current.selection) trashBoard(current.selection)
        break
      case 'sort-name':
        setSort('name')
        break
      case 'sort-date':
        setSort('dateEdited')
        break
      case 'show-thumbnails':
        setShowThumbnails(!current.showThumbnails)
        break
      case 'settings':
        setSettingsOpen(true)
        break
      case 'add-server':
        setAddServerOpen(true)
        break
    }
  }, [handleAddFolder])

  useEffect(() => {
    getState()
      .then((snapshot) => {
        setSnapshot(snapshot)
        // CanvasReady reopens the persisted selection after the canvas event
        // listeners are installed. Do not navigate here: startup has no prior
        // canvas whose pending changes need the navigation durability barrier.
      })
      .catch((err) => log('error', `Could not load the boards: ${String(err)}`))
    const unlistens: Promise<() => void>[] = [
      onStoreChanged(setSnapshot),
      onMenu(handleMenu),
      // A save changes only the board's time: update that without a full snapshot, so "Edited … ago" stays right.
      onBoardModified((id, modified) =>
        setSnapshot((current) =>
          current && modified !== null
            ? { ...current, boards: current.boards.map((b) => (b.id === id ? { ...b, modified } : b)) }
            : current,
        ),
      ),
    ]
    return () => {
      unlistens.forEach((p) => p.then((unlisten) => unlisten()))
    }
  }, [handleMenu])

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now() / 1000), NOW_TICK_MS)
    return () => clearInterval(timer)
  }, [])

  // Close the menus on a click outside them. Listens to `pointerdown` (capture
  // phase) and ignores clicks inside: React runs this effect synchronously during the click that opened the
  // menu, so a `click` listener on window would receive that same click and close it again right away.
  // Not `mousedown`: Excalidraw prevents the default of `pointerdown` on the canvas, which suppresses it.
  useEffect(() => {
    if (!contextMenu && !sourceMenu && !folderMenu && !bucketMenu) return
    const closeAll = () => {
      setContextMenu(null)
      setSourceMenu(null)
      setFolderMenu(null)
      setBucketMenu(null)
    }
    const close = (event: Event) => {
      if (event.target instanceof Element && event.target.closest('.context-menu')) return
      closeAll()
    }
    const closeOnBlur = closeAll
    window.addEventListener('pointerdown', close, true)
    window.addEventListener('blur', closeOnBlur)
    return () => {
      window.removeEventListener('pointerdown', close, true)
      window.removeEventListener('blur', closeOnBlur)
    }
  }, [contextMenu, sourceMenu, folderMenu, bucketMenu])

  // The window title: the open board and when it was last edited (as the SwiftUI app's title and subtitle).
  const openBoard = snapshot?.boards.find((b) => b.id === snapshot.selection)
  // Keep the collaboration route referentially stable. Canvas owns a durable
  // subscription whose effect must not restart for unrelated Shell renders
  // (notably the 30-second relative-time tick).
  const collab = useMemo(
    () => snapshot?.activeSource.startsWith('nats:') && snapshot.selection && openBoard?.documentId
      ? { sourceId: snapshot.activeSource, bucket: snapshot.activeGroup, boardId: snapshot.selection, documentId: openBoard.documentId }
      : null,
    [snapshot?.activeSource, snapshot?.activeGroup, snapshot?.selection, openBoard?.documentId],
  )
  const title = openBoard ? `${openBoard.name} — ${editedText(openBoard.modified, now)}` : 'Excalibur'
  useEffect(() => {
    // A failure to set the title must never take the UI down.
    try {
      setWindowTitle(title)
        .catch((err) => log('error', `Could not set the window title: ${String(err)}`))
    } catch (err) {
      log('error', `Could not set the window title: ${String(err)}`)
    }
  }, [title])

  // Every open list keeps its own filter: the active group's and each pinned one's.
  const setQuery = useCallback((sourceId: string, groupId: string, value: string) => {
    setQueries((previous) => ({ ...previous, [groupKey(sourceId, groupId)]: value }))
  }, [])
  const query = (queries[snapshot ? groupKey(snapshot.activeSource, snapshot.activeGroup) : ''] ?? '').trim()
  const visibleBoards = useMemo(() => {
    const sorted = snapshot ? sortBoards(snapshot.boards, snapshot.sortOrder) : []
    if (!query) return sorted.map((board) => ({ board, match: null as Match | null }))
    return sorted.flatMap((board) => {
      const match = searchMatch(board, query)
      return match ? [{ board, match }] : []
    })
  }, [snapshot, query])

  /** Folders and buckets pinned open, keyed the way the tree keys a group. */
  const pinnedByKey = useMemo(
    () => new Map((snapshot?.pinned ?? []).map((pin) => [groupKey(pin.source, pin.group), pin])),
    [snapshot?.pinned],
  )

  const handleDrop = useCallback(async (event: React.DragEvent) => {
    event.preventDefault()
    setDropTargeted(false)
    const files = Array.from(event.dataTransfer?.files ?? []).filter((f) => f.name.toLowerCase().endsWith(`.${FILE_EXTENSION}`))
    for (const file of files) {
      try {
        await importBoardData(file.name, await file.text())
      } catch (err) {
        log('error', `Could not import “${file.name}”: ${String(err)}`)
      }
    }
  }, [])

  /** Drag & drop of a board onto another source group: copy it there (the original stays). */
  const handleDropBoard = useCallback(async (boardId: string, targetSource: string, targetGroup: string) => {
    try {
      await copyBoardToSource(boardId, targetSource, targetGroup)
    } catch (err) {
      log('error', `Could not copy “${boardId}” to that source: ${String(err)}`)
    }
  }, [])

  /** Pins a folder or bucket open so its boards stay beside the active group. */
  const handleTogglePin = useCallback((sourceId: string, groupId: string) => {
    togglePinnedGroup(sourceId, groupId).catch((err: unknown) => log('error', `Could not change the pin: ${String(err)}`))
  }, [])

  /** Re-lists the buckets on every configured NATS server. */
  const handleRefreshSources = useCallback(async () => {
    setRefreshingSources(true)
    try {
      await refreshBuckets()
    } catch (err) {
      log('error', `Could not refresh the sources: ${String(err)}`)
      showAlert(String(err)).catch((alertErr) => log('error', `Could not show an alert: ${String(alertErr)}`))
    } finally {
      setRefreshingSources(false)
    }
  }, [])

  if (!snapshot) {
    return (
      <div className="shell">
        <div className="loading">Loading…</div>
      </div>
    )
  }

  const alert = snapshot.isAlertPresented ? snapshot.alerts[0] : null
  const activeNatsSource = snapshot.activeSource.startsWith('nats:') ? snapshot.sources.find((s) => s.id === snapshot.activeSource) : undefined

  /**
   * The board list under a group: the active group's boards carry the filter and the empty states; a pinned
   * group shows its own boards, unwatched, and opening one switches to that group first.
   */
  const renderBoards = (sourceId: string, groupId: string): ReactNode => {
    if (sourceId === snapshot.activeSource && groupId === snapshot.activeGroup) {
      if (snapshot.boards.length === 0) {
        return (
          <div className="tree-empty">
            <p>
              {activeNatsSource
                ? `No boards yet. They’ll be stored on ${activeNatsSource.name}.`
                : `No boards yet. They’ll be saved as .excalidraw files in ${lastPathComponent(snapshot.folder)}.`}
            </p>
            <button className="tree-empty-action" onClick={() => setCreating(true)}>
              <PlusIcon /> New Board
            </button>
          </div>
        )
      }
      if (visibleBoards.length === 0) {
        return (
          <div className="tree-empty">
            <p>No results for “{query}”</p>
          </div>
        )
      }
      return visibleBoards.map(({ board, match }) => (
        <BoardRow
          key={board.id}
          board={board}
          match={match}
          showThumbnail={snapshot.showThumbnails}
          selected={snapshot.selection === board.id}
          syncStatus={snapshot.selection === board.id ? sidebarSyncStatus : null}
          now={now}
          onSelect={() => selectBoard(board.id)}
          onContextMenu={(event) => {
            event.preventDefault()
            setContextMenu({ x: event.clientX, y: event.clientY, board })
          }}
        />
      ))
    }
    const boards = pinnedByKey.get(groupKey(sourceId, groupId))?.boards ?? []
    const term = (queries[groupKey(sourceId, groupId)] ?? '').trim()
    // Pinned boards come without text, so their filter matches names only.
    const sorted = sortBoards(boards, snapshot.sortOrder)
    const shown: { board: BoardInfo; match: Match | null }[] = term
      ? sorted.flatMap((board) => {
          const match = searchMatch(board, term)
          return match ? [{ board, match }] : []
        })
      : sorted.map((board) => ({ board, match: null }))
    if (shown.length === 0) {
      return (
        <div className="tree-empty">
          <p>{term ? `No results for “${term}”` : 'No boards yet.'}</p>
        </div>
      )
    }
    return shown.map(({ board, match }) => (
      <BoardRow
        key={board.id}
        board={board}
        match={match}
        showThumbnail={snapshot.showThumbnails}
        selected={snapshot.selection === board.id}
        syncStatus={null}
        now={now}
        draggable={false}
        onSelect={() => {
          // The board belongs to another group, so switch there before selecting it.
          void Promise.resolve(handleActivateSource(sourceId, groupId))
            .then(() => selectBoard(board.id))
            .catch((err: unknown) => log('error', `Could not open “${board.name}”: ${String(err)}`))
        }}
      />
    ))
  }

  return (
    <div className="shell">
      <aside
        className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}${dropTargeted ? ' drop-target' : ''}`}
        onDragOver={(event) => {
          event.preventDefault()
          setDropTargeted(true)
        }}
        onDragLeave={(event) => {
          if (!event.currentTarget.contains(event.relatedTarget as Node)) setDropTargeted(false)
        }}
        onDrop={handleDrop}
      >
        <div className="sidebar-top">
          {sidebarCollapsed ? (
            // Collapsed, the logo is the way back: hovering shows the expand icon, clicking opens the sidebar.
            <button className="app-logo expandable" onClick={() => setCollapsed(false)} title="Show Sidebar" aria-label="Show Sidebar">
              <LogoMark className="app-logo-mark" />
              <SidebarIcon className="app-logo-expand" />
            </button>
          ) : (
            <span className="app-logo" role="img" aria-label="Excalibur">
              <LogoMark className="app-logo-mark" />
            </span>
          )}
          {!sidebarCollapsed && (
            <span className="row-actions">
              <button className="icon-button row-action" onClick={() => setCollapsed(true)} title="Hide Sidebar" aria-label="Hide Sidebar">
                <SidebarIcon />
              </button>
            </span>
          )}
        </div>
        {sidebarCollapsed && (
          <div className="rail-actions">
            <button className="icon-button row-action" onClick={() => setCreating(true)} title="New Board (⌘N)" aria-label="New Board">
              <PlusIcon />
            </button>
            {sidebarSyncStatus && <SyncDot status={sidebarSyncStatus} />}
          </div>
        )}
        {!sidebarCollapsed && (
          <>
            <SourceTree
              sources={snapshot.sources}
              activeSource={snapshot.activeSource}
              activeGroup={snapshot.activeGroup}
              switchingSource={switchingSource}
              refreshing={refreshingSources}
              queries={queries}
              onQueryChange={setQuery}
              sortOrder={snapshot.sortOrder}
              onToggleSort={() => setSort(snapshot.sortOrder === 'name' ? 'dateEdited' : 'name')}
              onRefresh={handleRefreshSources}
              onActivateSource={handleActivateSource}
              onDropBoard={handleDropBoard}
              onNewBoard={() => setCreating(true)}
              onAddFolder={handleAddFolder}
              onFolderMenu={(event, folder) => {
                event.preventDefault()
                setFolderMenu({ x: event.clientX, y: event.clientY, folder })
              }}
              onNewBucket={setNewBucketFor}
              onSourceMenu={(event, source) => {
                event.preventDefault()
                setSourceMenu({ x: event.clientX, y: event.clientY, source })
              }}
              onBucketMenu={(event, source, group) => {
                event.preventDefault()
                setBucketMenu({ x: event.clientX, y: event.clientY, source, group })
              }}
              pinned={snapshot.pinned}
              onTogglePin={handleTogglePin}
              renderBoards={renderBoards}
            />
          </>
        )}
      </aside>

      <main className="main">
        <div className="canvas-stage" data-canvas-appearance={snapshot.canvasAppearance}>
          <Canvas
            canvasAppearance={snapshot.canvasAppearance}
            collaborationNickname={snapshot.collaborationNickname}
            collaborationColor={snapshot.collaborationColor}
            collaborationAvatar={snapshot.collaborationAvatar}
            collaborationAvatarOnly={snapshot.collaborationAvatarOnly}
            onSyncStatusHidden={setSidebarSyncStatus}
            boardId={snapshot.selection}
            collab={collab}
          />
          {snapshot.selection == null && (
            <div className="no-selection">
              <p className="empty-title">No Board Selected</p>
              <p className="empty-description">Select a board in the sidebar, or create a new one.</p>
              <button className="primary" onClick={() => setCreating(true)}>
                New Board
              </button>
            </div>
          )}
          {/* Only with a board open, and, like the Library trigger, gone while its panel is open. */}
          {snapshot.ai.enabled && openBoard && !aiOpen && <MerlinTrigger onOpen={() => setAIOpen(true)} />}
        </div>
        {snapshot.ai.enabled && (
          <aside className={`merlin-sidebar${aiOpen && openBoard ? ' open' : ''}`} aria-label="Merlin assistant">
            {aiOpen && openBoard && (
              <MerlinPanel
                key={openBoard.id}
                baseUrl={snapshot.ai.baseUrl}
                model={snapshot.ai.model}
                boardId={openBoard.id}
                boardName={openBoard.name}
                onClose={closeAI}
              />
            )}
          </aside>
        )}
      </main>

      {creating && (
        <NameDialog
          title="New Board"
          confirmTitle="Create"
          initialName=""
          boards={snapshot.boards}
          onConfirm={(name) => {
            setCreating(false)
            createBoard(name)
          }}
          onCancel={() => setCreating(false)}
        />
      )}
      {renaming && (
        <NameDialog
          title="Rename Board"
          confirmTitle="Rename"
          initialName={renaming.name}
          boards={snapshot.boards}
          excludingId={renaming.id}
          onConfirm={(name) => {
            setRenaming(null)
            renameBoard(renaming.id, name)
          }}
          onCancel={() => setRenaming(null)}
        />
      )}
      {addServerOpen && <AddServerDialog onClose={() => setAddServerOpen(false)} />}
      {newBucketFor && <NewBucketDialog source={newBucketFor} onClose={() => setNewBucketFor(null)} />}
      {removingServer && <RemoveServerDialog source={removingServer} onClose={() => setRemovingServer(null)} />}
      {folderMenu && (
        <div className="context-menu" style={{ left: folderMenu.x, top: folderMenu.y }} role="menu">
          <button
            role="menuitem"
            onClick={() => {
              revealFolder(folderMenu.folder).catch((err: unknown) => log('error', `Could not reveal the folder: ${String(err)}`))
              setFolderMenu(null)
            }}
          >
            Show in {FILE_MANAGER}
          </button>
          <div className="context-menu-divider" />
          <button
            role="menuitem"
            className="destructive"
            disabled={(snapshot.sources.find((source) => source.kind === 'local')?.groups.length ?? 0) <= 1}
            title="Removes the folder from the sidebar. Its boards stay on disk."
            onClick={() => {
              handleRemoveFolder(folderMenu.folder)
              setFolderMenu(null)
            }}
          >
            Remove from Sidebar
          </button>
        </div>
      )}
      {sourceMenu && (
        <div className="context-menu" style={{ left: sourceMenu.x, top: sourceMenu.y }} role="menu">
          <button
            role="menuitem"
            onClick={() => {
              setNewBucketFor(sourceMenu.source)
              setSourceMenu(null)
            }}
          >
            New Bucket
          </button>
          <div className="context-menu-divider" />
          <button
            role="menuitem"
            className="destructive"
            onClick={() => {
              setRemovingServer(sourceMenu.source)
              setSourceMenu(null)
            }}
          >
            Remove Server
          </button>
        </div>
      )}
      {bucketMenu && (
        <div className="context-menu" style={{ left: bucketMenu.x, top: bucketMenu.y }} role="menu">
          <button
            role="menuitem"
            className="destructive"
            disabled={
              bucketMenu.source.id === snapshot.activeSource &&
              bucketMenu.group.id === snapshot.activeGroup &&
              snapshot.boards.length > 0
            }
            title="A bucket can only be deleted when it contains no boards."
            onClick={() => {
              setDeletingBucket({ source: bucketMenu.source, group: bucketMenu.group })
              setBucketDeleteError(null)
              setBucketMenu(null)
            }}
          >
            Delete Bucket
          </button>
        </div>
      )}
      {deletingBucket && (
        <div className="modal-backdrop">
          <div className="modal alert" role="alertdialog" aria-modal="true" aria-labelledby="delete-bucket-title">
            <h2 id="delete-bucket-title">Delete “{deletingBucket.group.name}”?</h2>
            <p className="alert-message">
              This permanently deletes the empty bucket and its collaboration history from {deletingBucket.source.name}.
            </p>
            {bucketDeleteError && <p className="problem">{bucketDeleteError}</p>}
            <div className="modal-actions">
              <button className="secondary" disabled={bucketDeleteBusy} onClick={() => setDeletingBucket(null)}>
                Cancel
              </button>
              <button
                className="destructive"
                disabled={bucketDeleteBusy}
                onClick={async () => {
                  setBucketDeleteBusy(true)
                  setBucketDeleteError(null)
                  try {
                    await deleteBucket(deletingBucket.source.id, deletingBucket.group.id)
                    setDeletingBucket(null)
                  } catch (err) {
                    setBucketDeleteError(String(err))
                  } finally {
                    setBucketDeleteBusy(false)
                  }
                }}
              >
                {bucketDeleteBusy ? 'Deleting…' : 'Delete Bucket'}
              </button>
            </div>
          </div>
        </div>
      )}
      {settingsOpen && <SettingsModal snapshot={snapshot} onClose={() => setSettingsOpen(false)} />}
      {alert && (
        <div className="modal-backdrop">
          <div className="modal alert" role="alertdialog" aria-modal="true">
            <h2>{alert.title}</h2>
            <p className="alert-message">{alert.message}</p>
            <div className="modal-actions">
              <button className="primary" onClick={() => dismissAlert()}>
                OK
              </button>
            </div>
          </div>
        </div>
      )}
      {contextMenu && (
        <ContextMenu
          x={contextMenu.x}
          y={contextMenu.y}
          onRename={() => {
            setRenaming(contextMenu.board)
            setContextMenu(null)
          }}
          onDuplicate={() => {
            duplicateBoard(contextMenu.board.id)
            setContextMenu(null)
          }}
          onReveal={() => {
            reveal(contextMenu.board.id)
            setContextMenu(null)
          }}
          onTrash={() => {
            trashBoard(contextMenu.board.id)
            setContextMenu(null)
          }}
        />
      )}
    </div>
  )
}

// MARK: - Sidebar row

function BoardRow({
  board,
  match,
  showThumbnail,
  selected,
  syncStatus,
  now,
  draggable = true,
  onSelect,
  onContextMenu,
}: {
  board: BoardInfo
  match: Match | null
  showThumbnail: boolean
  selected: boolean
  /** Shown on the board when the canvas has no room for its sync badge. */
  syncStatus: SyncStatus | null
  now: number
  /** Only boards of the active group can be dragged: copying reads them from there. */
  draggable?: boolean
  onSelect: () => void
  /** Board actions act on the active group, so a pinned group's rows have no menu. */
  onContextMenu?: (event: React.MouseEvent) => void
}) {
  return (
    <div
      className={`board-row${selected ? ' selected' : ''}`}
      onClick={onSelect}
      onContextMenu={onContextMenu}
      draggable={draggable}
      onDragStart={(event) => {
        event.dataTransfer.setData('text/plain', board.id)
        event.dataTransfer.effectAllowed = 'copy'
      }}
    >
      {showThumbnail && (
        <div className="thumbnail-frame">
          <div className="thumbnail">{board.thumbnail ? <img src={board.thumbnail} alt="" draggable={false} /> : null}</div>
          {syncStatus && <SyncDot status={syncStatus} />}
        </div>
      )}
      <div className="board-info">
        <div className="board-name-line">
          {!showThumbnail && syncStatus && <SyncDot status={syncStatus} />}
          <span className="board-name">{board.name}</span>
          {board.isLink && (
            <span className="link-icon" title={board.target ? `Link to ${board.target}` : 'Link'}>
              ↗
            </span>
          )}
        </div>
        {match?.kind === 'text' ? (
          <span className="board-excerpt">{match.excerpt}</span>
        ) : (
          <span className="board-modified">{editedText(board.modified, now)}</span>
        )}
      </div>
    </div>
  )
}

/** The open board's sync status as a colored dot; the label is its tooltip and accessible name. */
function SyncDot({ status }: { status: SyncStatus }) {
  const label = PERSISTENCE_LABELS[status]
  return <span className={`sync-dot ${status}`} role="img" aria-label={label} title={label} />
}

// MARK: - Source tree

const groupKey = (sourceId: string, groupId: string) => `${sourceId}\u0000${groupId}`

/**
 * The navigation tree. Sources (this computer, each NATS server) are headings; under each, its folders or
 * buckets are accordions. The active group opens with its boards nested beneath it; opening another group
 * switches to it. A group pinned open also lists its boards, so several stay in view at once, each pinned
 * one from the backend (the active group is the only one the backend fully loads).
 *
 * A switch is shown at once: the clicked group takes the highlight and opens with a spinner, and its boards
 * appear as soon as the backend reports them (the previous group's boards are never shown under it).
 */
function SourceTree({
  sources,
  activeSource,
  activeGroup,
  switchingSource,
  refreshing,
  queries,
  onQueryChange,
  sortOrder,
  onToggleSort,
  onRefresh,
  onActivateSource,
  onDropBoard,
  onNewBoard,
  onAddFolder,
  onFolderMenu,
  onNewBucket,
  onSourceMenu,
  onBucketMenu,
  pinned,
  onTogglePin,
  renderBoards,
}: {
  sources: SourceInfo[]
  activeSource: string
  activeGroup: string
  /** Group key being switched to, while its boards load. */
  switchingSource: string | null
  refreshing: boolean
  /** The find filter per group, keyed like the tree keys a group. */
  queries: Record<string, string>
  onQueryChange: (sourceId: string, groupId: string, value: string) => void
  sortOrder: BoardSortOrder
  onToggleSort: () => void
  onRefresh: () => void
  onActivateSource: (sourceId: string, groupId: string) => void
  onDropBoard: (boardId: string, targetSource: string, targetGroup: string) => void
  onNewBoard: () => void
  onAddFolder: () => void
  onFolderMenu: (event: React.MouseEvent, folder: string) => void
  onNewBucket: (source: SourceInfo) => void
  onSourceMenu: (event: React.MouseEvent, source: SourceInfo) => void
  onBucketMenu: (event: React.MouseEvent, source: SourceInfo, group: GroupInfo) => void
  /** Folders and buckets pinned open, with their boards. */
  pinned: PinnedGroupInfo[]
  onTogglePin: (sourceId: string, groupId: string) => void
  /** The boards of a group: filtered for the active one, plain for a pinned one. */
  renderBoards: (sourceId: string, groupId: string) => React.ReactNode
}) {
  const [dragOver, setDragOver] = useState<string | null>(null)
  /** The active group, if the user folded it away. Any other group is closed because it isn't loaded. */
  const [foldedGroup, setFoldedGroup] = useState<string | null>(null)
  /** Pinned groups the user folded away; they stay pinned but list no boards. */
  const [foldedPins, setFoldedPins] = useState<string[]>([])
  const pinnedKeys = useMemo(() => new Set(pinned.map((pin) => groupKey(pin.source, pin.group))), [pinned])
  const loadedKey = groupKey(activeSource, activeGroup)
  /** The group the user is on: the one being switched to, else the loaded one. */
  const currentKey = switchingSource ?? loadedKey
  const treeRef = useRef<HTMLElement>(null)

  // Once another group's boards arrive, bring the group into view (it may sit below the fold).
  useEffect(() => {
    treeRef.current?.querySelector('.tree-group.active')?.scrollIntoView({ block: 'nearest' })
  }, [loadedKey])

  const renderGroup = (source: SourceInfo, group: GroupInfo, label: string, panelId: string) => {
    const key = groupKey(source.id, group.id)
    const current = key === currentKey
    const loading = current && key !== loadedKey
    const isPinned = pinnedKeys.has(key)
    const pinnedOpen = !current && isPinned
    const filter = queries[key] ?? ''
    // The active group opens unless folded; a pinned one opens unless it was folded on its own.
    const open = current ? !loading && foldedGroup !== key : pinnedOpen && !foldedPins.includes(key)
    return (
      <div
        key={key}
        className={`tree-group${current ? ' active' : ''}${pinnedOpen ? ' pinned' : ''}${dragOver === key ? ' drag-over' : ''}`}
      >
        {/* The header is the group's handle: drop a board on it to copy it here; right-click for options. */}
        <div
          className="tree-group-row"
          title={group.id}
          onContextMenu={(event) => (source.kind === 'nats' ? onBucketMenu(event, source, group) : onFolderMenu(event, group.id))}
          onDragEnter={(event) => {
            event.preventDefault()
            setDragOver(key)
          }}
          onDragLeave={(event) => {
            if (!event.currentTarget.contains(event.relatedTarget as Node)) setDragOver((value) => (value === key ? null : value))
          }}
          onDragOver={(event) => {
            event.preventDefault()
            event.dataTransfer.dropEffect = 'copy'
          }}
          onDrop={(event) => {
            event.preventDefault()
            event.stopPropagation()
            setDragOver(null)
            const boardId = event.dataTransfer.getData('text/plain')
            if (boardId) onDropBoard(boardId, source.id, group.id)
          }}
        >
          <button
            className="tree-group-toggle"
            aria-expanded={open}
            aria-controls={open ? panelId : undefined}
            aria-current={current ? 'true' : undefined}
            aria-busy={loading}
            onClick={() => {
              if (current) {
                if (!loading) setFoldedGroup((value) => (value === key ? null : key))
              } else if (isPinned) {
                setFoldedPins((value) => (value.includes(key) ? value.filter((folded) => folded !== key) : [...value, key]))
              } else {
                setFoldedGroup(null)
                onActivateSource(source.id, group.id)
              }
            }}
          >
            <ChevronDownIcon className="tree-chevron" aria-hidden="true" />
            <span className="tree-group-name">{label}</span>
          </button>
          {loading ? (
            <span className="tree-spinner" role="status" aria-label="Loading boards" />
          ) : (
            <span className="row-actions">
              {current && (
                <button className="icon-button row-action" onClick={onNewBoard} title="New Board (⌘N)" aria-label={`New Board in ${label}`}>
                  <PlusIcon />
                </button>
              )}
              <button
                className={`icon-button row-action pin${isPinned ? ' on' : ''}`}
                onClick={() => onTogglePin(source.id, group.id)}
                title={isPinned ? 'Unpin this group' : 'Pin this group open'}
                aria-label={isPinned ? `Unpin ${label}` : `Pin ${label} open`}
                aria-pressed={isPinned}
              >
                <PinIcon />
              </button>
            </span>
          )}
        </div>
        {open && (
          <div id={panelId} className="tree-boards" role="group" aria-label={`Boards in ${label}`}>
            <div className="tree-tools">
              <div className="tree-find">
                <SearchIcon className="tree-find-icon" aria-hidden="true" />
                <input
                  type="search"
                  value={filter}
                  placeholder="Find board"
                  aria-label={`Find a board in ${label}`}
                  onChange={(event) => onQueryChange(source.id, group.id, event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Escape') onQueryChange(source.id, group.id, '')
                  }}
                />
                {filter !== '' && (
                  <button
                    type="button"
                    className="tree-find-clear"
                    title="Clear"
                    aria-label="Clear the filter"
                    onClick={(event) => {
                      onQueryChange(source.id, group.id, '')
                      // Keep typing in the field, not on the button that just emptied it.
                      event.currentTarget.parentElement?.querySelector('input')?.focus()
                    }}
                  >
                    <CloseIcon />
                  </button>
                )}
              </div>
              <button
                className="tree-sort"
                title={sortOrder === 'name' ? 'Sorted by name. Switch to newest first.' : 'Newest first. Switch to name.'}
                aria-label={sortOrder === 'name' ? 'Sort by date edited' : 'Sort by name'}
                onClick={onToggleSort}
              >
                {sortOrder === 'name' ? 'A–Z' : 'Newest'}
              </button>
            </div>
            {renderBoards(source.id, group.id)}
          </div>
        )}
      </div>
    )
  }

  return (
    <nav ref={treeRef} className="source-tree" aria-label="Boards">
      {sources.map((source, sourceIndex) => (
        <section key={source.id} className="tree-source" aria-label={source.name}>
          {source.kind === 'local' ? (
            <div className="tree-source-row">
              <FolderIcon className="source-icon" />
              <span className="source-title">Local</span>
              <span className="row-actions">
                <button className="icon-button row-action" onClick={onAddFolder} title="Add Folder" aria-label="Add Folder">
                  <PlusIcon />
                </button>
              </span>
            </div>
          ) : (
            <div className="tree-source-row" title={`${source.name} (right-click for options)`} onContextMenu={(event) => onSourceMenu(event, source)}>
              <CloudIcon className="source-icon" />
              <span className="source-title">{source.name}</span>
              <span className="row-actions">
                <RefreshButton refreshing={refreshing} onRefresh={onRefresh} />
                <button className="icon-button row-action" onClick={() => onNewBucket(source)} title="New Bucket" aria-label="New Bucket">
                  <PlusIcon />
                </button>
              </span>
            </div>
          )}
          {source.kind === 'nats' && source.groups.length === 0 ? (
            <button className="source-empty" onClick={() => onNewBucket(source)}>
              No buckets yet. Create one
            </button>
          ) : (
            source.groups.map((group, groupIndex) =>
              renderGroup(
                source,
                group,
                source.kind === 'local' ? folderDisplayName(group.name) : group.name,
                `tree-group-${sourceIndex}-${groupIndex}`,
              ),
            )
          )}
        </section>
      ))}
    </nav>
  )
}

/**
 * Refresh buckets. Every click turns the arrows at least one full circle, however fast the refresh is
 * (from a local board it's nearly instant), and they always stop at a full turn instead of snapping back.
 */
function RefreshButton({ refreshing, onRefresh }: { refreshing: boolean; onRefresh: () => void }) {
  const [spinning, setSpinning] = useState(false)
  const reducedMotion = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

  // Also spin when another server's button started the refresh (it refreshes them all).
  useEffect(() => {
    if (refreshing && !reducedMotion()) setSpinning(true)
  }, [refreshing])

  return (
    <button
      className={`icon-button row-action${spinning ? ' refreshing' : ''}`}
      onClick={() => {
        if (!reducedMotion()) setSpinning(true)
        onRefresh()
      }}
      // Each turn ends here; stop once the refresh is done.
      onAnimationIteration={() => {
        if (!refreshing) setSpinning(false)
      }}
      disabled={refreshing}
      title="Refresh buckets"
      aria-label="Refresh buckets"
      aria-busy={refreshing}
    >
      <RefreshIcon />
    </button>
  )
}

// MARK: - Merlin assistant

/**
 * Opens Merlin. Normally a button below Excalidraw's Library button; when that would run into the shape
 * toolbar, or in Excalidraw's small-screen layout, a tab on the canvas's right edge instead, styled after
 * (and stacked under) Excalidraw's own small-screen tool tab. The full button stays laid out, hidden, so it
 * can keep measuring whether it fits again.
 */
function MerlinTrigger({ onOpen }: { onOpen: () => void }) {
  const buttonRef = useRef<HTMLButtonElement>(null)
  const { fits, tabTop } = useToolbarRoom(buttonRef)
  const props = {
    // Open on press, not on click: an open Library closes on any pointerdown outside it, and the button
    // then slides back right (it moves aside for the Library), so the release would miss it.
    onPointerDown: (event: React.PointerEvent) => {
      if (event.button === 0) onOpen()
    },
    // Keyboard activation (Enter/Space) only arrives as a click.
    onClick: onOpen,
    title: 'Open Merlin',
    'aria-label': 'Open Merlin',
    'aria-expanded': false,
    'aria-controls': 'merlin-panel',
  }
  return (
    <>
      <button ref={buttonRef} className={`merlin-canvas-toggle${fits ? '' : ' offstage'}`} aria-hidden={!fits} tabIndex={fits ? 0 : -1} {...props}>
        <SparklesIcon />
        <span>Merlin</span>
      </button>
      {!fits && (
        <button className="merlin-canvas-toggle tab" style={{ top: tabTop }} {...props}>
          <SparklesIcon />
        </button>
      )}
    </>
  )
}

function MerlinPanel({
  baseUrl,
  model,
  boardId,
  boardName,
  onClose,
}: {
  baseUrl: string
  model: string
  boardId: string
  boardName: string
  onClose: () => void
}) {
  const [messages, setMessages] = useState<AIChatMessage[]>([])
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [queue, setQueue] = useState<{ id: number; content: string; model: string; thinking: boolean }[]>([])
  const [errors, setErrors] = useState<{ id: number; message: string }[]>([])
  const [models, setModels] = useState<AIModel[]>(model ? [{ id: model, model, thinking: false }] : [])
  const [selectedModel, setSelectedModel] = useState(model)
  const [modelsLoading, setModelsLoading] = useState(true)
  const [modelsError, setModelsError] = useState<string | null>(null)
  const [streamedAnswer, setStreamedAnswer] = useState('')
  const [streamedThinking, setStreamedThinking] = useState('')
  const [streamedStatus, setStreamedStatus] = useState('')
  const endRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const requestId = useRef(0)
  const historyRef = useRef<AIChatMessage[]>([])
  const activeRequestRef = useRef<string | null>(null)
  const streamedThinkingRef = useRef('')

  useEffect(() => {
    let unlisten: (() => void) | undefined
    void onAIStream((streamRequestId, kind, delta) => {
      if (streamRequestId !== activeRequestRef.current) return
      if (kind === 'status') {
        // Statuses are replacement labels for the loader, not fragments.
        setStreamedStatus(delta)
      } else if (kind === 'thinking') {
        streamedThinkingRef.current += delta
        setStreamedThinking(streamedThinkingRef.current)
      } else {
        setStreamedAnswer((current) => current + delta)
      }
    }).then((stop) => {
      unlisten = stop
    })
    return () => unlisten?.()
  }, [])

  useEffect(() => {
    let active = true
    setModelsLoading(true)
    discoverAIModels(baseUrl, '', false)
      .then((available) => {
        if (!active) return
        setModels(available)
        setSelectedModel((current) => {
          const match = available.find((item) => item.model === current || item.id === current)
          return match?.model ?? available[0]?.model ?? ''
        })
        setModelsError(null)
      })
      .catch((err) => {
        if (!active) return
        setModels([])
        setModelsError(String(err))
      })
      .finally(() => {
        if (active) setModelsLoading(false)
      })
    return () => {
      active = false
    }
  }, [baseUrl])

  useEffect(() => {
    inputRef.current?.focus()
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', closeOnEscape)
    return () => window.removeEventListener('keydown', closeOnEscape)
  }, [onClose])

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, queue, busy, errors, modelsError, streamedAnswer, streamedThinking, streamedStatus])

  const selected = models.find((available) => available.model === selectedModel)
  const ready = !modelsLoading && Boolean(selected)

  /** Queue a message for the selected model; returns whether it was accepted. */
  const submit = (text: string) => {
    const content = text.trim()
    if (!content || modelsLoading || !selected) return false
    setQueue((current) => [...current, { id: ++requestId.current, content, model: selected.model, thinking: selected.thinking }])
    requestAnimationFrame(() => inputRef.current?.focus())
    return true
  }

  const send = () => {
    if (submit(draft)) setDraft('')
  }

  useEffect(() => {
    if (busy || queue.length === 0) return
    const request = queue[0]
    const next: AIChatMessage[] = [...historyRef.current, { role: 'user', content: request.content }]
    historyRef.current = next
    setQueue((current) => current.filter((item) => item.id !== request.id))
    setMessages(next)
    setBusy(true)
    const streamRequestId = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${request.id}`
    activeRequestRef.current = streamRequestId
    setStreamedAnswer('')
    streamedThinkingRef.current = ''
    setStreamedThinking('')
    setStreamedStatus('')
    void aiChat(next, request.model, streamRequestId, boardId, request.thinking)
      .then((answer) => {
        const thinking = streamedThinkingRef.current
        historyRef.current = [
          ...historyRef.current,
          // When an upstream emits a leading `thought` channel as ordinary
          // content, it is shown live in the collapsible panel but is also the
          // final response body. Avoid retaining the same text twice.
          { role: 'assistant', content: answer, thinking: thinking && thinking !== answer ? thinking : undefined },
        ]
        setMessages(historyRef.current)
      })
      .catch((err) => setErrors((current) => [...current, { id: request.id, message: String(err) }]))
      .finally(() => {
        activeRequestRef.current = null
        setStreamedAnswer('')
        streamedThinkingRef.current = ''
        setStreamedThinking('')
        setStreamedStatus('')
        setBusy(false)
        requestAnimationFrame(() => inputRef.current?.focus())
      })
  }, [boardId, busy, queue])

  const canSend = draft.trim() !== '' && ready
  const conversationEmpty = messages.length === 0 && queue.length === 0 && !busy

  // Grow the composer with its text, up to the CSS max-height; then it scrolls.
  useLayoutEffect(() => {
    const input = inputRef.current
    if (!input) return
    input.style.height = 'auto'
    input.style.height = `${input.scrollHeight}px`
  }, [draft])

  return (
    <section id="merlin-panel" className="merlin-panel" aria-label="Merlin assistant">
      <header className="merlin-header">
        <span className="merlin-badge" aria-hidden="true">
          <SparklesIcon />
        </span>
        <div className="merlin-title">
          <h2>Merlin</h2>
          <p title={boardName}>{boardName}</p>
        </div>
        <button className="icon-button merlin-close" onClick={onClose} title="Close Merlin" aria-label="Close Merlin">
          <CloseIcon />
        </button>
      </header>

      <div className="merlin-messages" aria-live="polite">
        {conversationEmpty && (
          <div className="merlin-welcome">
            <span className="merlin-badge large" aria-hidden="true">
              <SparklesIcon />
            </span>
            <h3>How can I help with this board?</h3>
            <p>Merlin reads the canvas at the start of each turn, so answers and edits stay grounded in what’s on it.</p>
            <div className="merlin-suggestions">
              {MERLIN_SUGGESTIONS.map((suggestion) => (
                <button
                  key={suggestion}
                  type="button"
                  disabled={!ready}
                  title={ready ? undefined : 'Waiting for the model list'}
                  onClick={() => submit(suggestion)}
                >
                  {suggestion}
                </button>
              ))}
            </div>
          </div>
        )}
        {messages.map((message, index) =>
          message.role === 'user' ? (
            <div key={index} className="merlin-message user">
              <p>{message.content}</p>
            </div>
          ) : (
            <div key={index} className="merlin-message assistant">
              <MerlinMessageHead>
                <CopyButton text={message.content} className="merlin-copy" label="Merlin response" />
              </MerlinMessageHead>
              {message.thinking && <AIThinking text={message.thinking} />}
              <MerlinRichText text={message.content} />
            </div>
          ),
        )}
        {busy && (
          <div className="merlin-message assistant streaming">
            <MerlinMessageHead />
            {streamedThinking && <AIThinking text={streamedThinking} />}
            {streamedAnswer ? (
              <MerlinRichText text={streamedAnswer} />
            ) : (
              <MerlinLoader label={streamedStatus} />
            )}
          </div>
        )}
        {queue.map((request, index) => (
          <div key={request.id} className="merlin-message user queued">
            <span className="merlin-queued-label">Queued {index + 1}</span>
            <p>{request.content}</p>
            <button
              className="merlin-queue-remove"
              type="button"
              aria-label="Remove queued message"
              title="Remove from queue"
              onClick={() => setQueue((current) => current.filter((item) => item.id !== request.id))}
            >
              <CloseIcon />
            </button>
          </div>
        ))}
        {modelsError && <CopyableAIError message={`Could not refresh models: ${modelsError}`} />}
        {errors.map((item) => (
          <CopyableAIError
            key={item.id}
            message={item.message}
            onDismiss={() => setErrors((current) => current.filter((error) => error.id !== item.id))}
          />
        ))}
        <div ref={endRef} />
      </div>

      <form
        className="merlin-composer"
        onSubmit={(event) => {
          event.preventDefault()
          send()
        }}
      >
        <textarea
          ref={inputRef}
          value={draft}
          rows={1}
          aria-label="Message Merlin"
          placeholder={busy ? 'Ask a follow-up — it will be queued' : 'Ask Merlin anything…'}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              send()
            }
          }}
        />
        <div className="merlin-composer-bar">
          {/* The native select covers the whole pill (invisibly), so a click anywhere on it opens the menu. */}
          <label className="merlin-model" title={modelsError ?? selected?.model ?? 'Choose the model for this chat'}>
            <SparklesIcon aria-hidden="true" />
            <span className="merlin-model-name">
              {selected?.id ?? (modelsLoading ? 'Loading models…' : 'No models')}
            </span>
            <ChevronDownIcon aria-hidden="true" />
            <select
              aria-label="AI model"
              value={selectedModel}
              disabled={busy || modelsLoading || models.length === 0}
              onChange={(event) => setSelectedModel(event.target.value)}
            >
              {models.map((available) => (
                <option key={available.model} value={available.model} title={available.model}>
                  {available.id}
                </option>
              ))}
            </select>
          </label>
          <span className="merlin-hint">⇧⏎ new line</span>
          <button type="submit" className="merlin-send" disabled={!canSend} title="Send" aria-label="Send">
            <ArrowUpIcon />
          </button>
        </div>
      </form>
    </section>
  )
}

/** Starters for an empty conversation; picking one sends it right away. */
const MERLIN_SUGGESTIONS = [
  'Summarize this board',
  'Tidy up the layout',
  'Suggest what’s missing',
]

/** The "Merlin" byline above each assistant message, with optional actions on the right. */
function MerlinMessageHead({ children }: { children?: React.ReactNode }) {
  return (
    <div className="merlin-message-head">
      <span className="merlin-avatar" aria-hidden="true">
        <SparklesIcon />
      </span>
      <span className="merlin-byline">Merlin</span>
      {children}
    </div>
  )
}

/**
 * Merlin's reasoning. Collapsed by default and always one click away, so a
 * working turn shows a loader instead of a growing wall of prose.
 */
/**
 * Merlin's reasoning. Collapsed by default, and the whole card is the toggle so a
 * long block can be closed from wherever the reader ended up instead of forcing
 * a scroll back to the summary.
 */
function AIThinking({ text }: { text: string }) {
  const ref = useRef<HTMLDetailsElement>(null)
  return (
    <details
      ref={ref}
      className="merlin-thinking"
      title="Click to expand or collapse"
      onClick={(event) => {
        const card = ref.current
        if (!card) return
        // The summary already toggles natively, and selecting the reasoning text
        // must not close the card out from under the selection.
        if (event.target instanceof Element && event.target.closest('summary')) return
        if (!window.getSelection()?.isCollapsed) return
        card.open = !card.open
      }}
    >
      <summary>Thinking</summary>
      <div>{text}</div>
    </details>
  )
}

/** The single progress line shown while Merlin works; the label is the current tool step. */
function MerlinLoader({ label }: { label: string }) {
  return (
    <span className="merlin-loader" role="status">
      <i className="merlin-spinner" aria-hidden="true" />
      <span className="merlin-loader-label">{label || 'Working…'}</span>
    </span>
  )
}

type MerlinBlock = {
  kind: 'heading' | 'bullet' | 'ordered' | 'paragraph'
  text: string
}

/**
 * Parse the small markdown subset Merlin is asked to use: `#` headings, `-` or
 * `*` bullets, `1.` steps, and blank-line separated paragraphs. Anything else
 * stays literal, so a partial stream never renders as broken markup.
 */
function parseMerlinBlocks(text: string): MerlinBlock[] {
  const blocks: MerlinBlock[] = []
  for (const line of text.split('\n')) {
    if (!line.trim()) continue
    const heading = /^#{1,6}\s+(.+)$/.exec(line)
    if (heading) {
      blocks.push({ kind: 'heading', text: heading[1] })
      continue
    }
    const bullet = /^[-*•]\s+(.+)$/.exec(line)
    if (bullet) {
      blocks.push({ kind: 'bullet', text: bullet[1] })
      continue
    }
    const ordered = /^\d+[.)]\s+(.+)$/.exec(line)
    if (ordered) {
      blocks.push({ kind: 'ordered', text: ordered[1] })
      continue
    }
    blocks.push({ kind: 'paragraph', text: line.trim() })
  }
  return blocks
}

/** Inline `**bold**`, `*italic*`, and `` `code` `` for the parsed blocks. */
function renderMerlinInline(text: string): ReactNode[] {
  const nodes: ReactNode[] = []
  const pattern = /\*\*([^*]+)\*\*|`([^`]+)`|(?<![*\w])\*(?!\s)([^*]+)\*/g
  let last = 0
  for (const match of text.matchAll(pattern)) {
    const start = match.index ?? 0
    if (start > last) nodes.push(text.slice(last, start))
    if (match[1] !== undefined) {
      nodes.push(<strong key={start}>{match[1]}</strong>)
    } else if (match[2] !== undefined) {
      nodes.push(<code key={start}>{match[2]}</code>)
    } else {
      nodes.push(<em key={start}>{match[3]}</em>)
    }
    last = start + match[0].length
  }
  if (last < text.length) nodes.push(text.slice(last))
  return nodes
}

/** Renders an assistant turn: short headings, real bullets, and emoji the model sends. */
function MerlinRichText({ text }: { text: string }) {
  const blocks = useMemo(() => parseMerlinBlocks(text), [text])
  const rendered: ReactNode[] = []
  let list: MerlinBlock[] = []
  let listKind: 'bullet' | 'ordered' = 'bullet'

  /** Close the open list, if any, before the next block. */
  const flushList = (key: number) => {
    if (list.length === 0) return
    const items = list.map((item, index) => <li key={index}>{renderMerlinInline(item.text)}</li>)
    rendered.push(
      listKind === 'ordered' ? <ol key={`list-${key}`}>{items}</ol> : <ul key={`list-${key}`}>{items}</ul>,
    )
    list = []
  }

  blocks.forEach((block, index) => {
    if (block.kind === 'bullet' || block.kind === 'ordered') {
      if (list.length > 0 && listKind !== block.kind) flushList(index)
      listKind = block.kind
      list.push(block)
      return
    }
    flushList(index)
    rendered.push(
      block.kind === 'heading' ? (
        <h4 key={index}>{renderMerlinInline(block.text)}</h4>
      ) : (
        <p key={index}>{renderMerlinInline(block.text)}</p>
      ),
    )
  })
  flushList(blocks.length)

  return <div className="merlin-rich">{rendered}</div>
}

function CopyButton({ text, className, label }: { text: string; className?: string; label: string }) {
  const [copied, setCopied] = useState(false)
  const resetTimer = useRef<ReturnType<typeof window.setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
    },
    [],
  )

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
    } catch {
      const field = document.createElement('textarea')
      field.value = text
      field.style.position = 'fixed'
      field.style.opacity = '0'
      document.body.appendChild(field)
      field.select()
      document.execCommand('copy')
      field.remove()
    }
    setCopied(true)
    if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
    resetTimer.current = window.setTimeout(() => setCopied(false), 1500)
  }

  return (
    <button
      type="button"
      className={className}
      onClick={copy}
      title={copied ? `${label} copied` : `Copy ${label.toLowerCase()}`}
      aria-label={copied ? `${label} copied` : `Copy ${label.toLowerCase()}`}
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

function CopyableAIError({ message, onDismiss }: { message: string; onDismiss?: () => void }) {
  return (
    <div className="merlin-error" role="alert">
      <span>{message}</span>
      <div className="merlin-error-actions">
        <CopyButton text={message} label="error" />
        {onDismiss && (
          <button type="button" onClick={onDismiss}>
            Dismiss
          </button>
        )}
      </div>
    </div>
  )
}

function NatsCredentialsPicker({ value, onChange }: { value: string; onChange: (path: string) => void }) {
  const choose = async () => {
    try {
      const path = await chooseNatsCredentialsFile()
      if (path) onChange(path)
    } catch (err) {
      log('error', `Could not choose a NATS credentials file: ${String(err)}`)
    }
  }

  return (
    <div className="nats-credentials-picker">
      <input value={value} readOnly placeholder="Credentials file (.creds, optional)" aria-label="NATS credentials file" title={value} />
      <button type="button" className="secondary" onClick={choose}>
        Choose
      </button>
      {value && (
        <button type="button" className="secondary" onClick={() => onChange('')}>
          Clear
        </button>
      )}
    </div>
  )
}

/** Dialog for adding a NATS server. Buckets are listed after connecting. */
function AddServerDialog({ onClose }: { onClose: () => void }) {
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [credentialsFile, setCredentialsFile] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const trimmedName = name.trim()
  const trimmedUrl = url.trim()
  const problem = !trimmedName ? 'Enter a name.' : !trimmedUrl ? 'Enter a server URL.' : null
  // Only point out missing fields once the user has started filling in the form.
  const touched = Boolean(trimmedName || trimmedUrl)

  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    const config: NatsServerConfig = {
      id: newServerId(),
      name: trimmedName,
      url: trimmedUrl,
      username: username.trim(),
      password,
      credentialsFile,
      buckets: [],
    }
    setError(null)
    try {
      await addNatsServer(config)
      onClose()
    } catch (err) {
      log('error', `Could not add the NATS server: ${String(err)}`)
      setError(String(err))
      setBusy(false)
    }
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose()
      }}
    >
      <div className="modal" role="dialog" aria-modal="true">
        <h2>Add NATS Server</h2>
        <input
          ref={inputRef}
          value={name}
          placeholder="Name, e.g. Office"
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape') onClose()
          }}
        />
        <input
          value={url}
          placeholder="URL, e.g. nats://localhost:4222"
          onChange={(event) => setUrl(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape') onClose()
          }}
        />
        <input
          value={username}
          autoComplete="username"
          placeholder="Username (optional)"
          disabled={Boolean(credentialsFile)}
          onChange={(event) => setUsername(event.target.value)}
        />
        <input
          value={password}
          type="password"
          autoComplete="current-password"
          placeholder="Password (optional)"
          disabled={Boolean(credentialsFile)}
          onChange={(event) => setPassword(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape') onClose()
          }}
        />
        <NatsCredentialsPicker
          value={credentialsFile}
          onChange={(path) => {
            setCredentialsFile(path)
            if (path) {
              setUsername('')
              setPassword('')
            }
          }}
        />
        <p className={error || (touched && problem) ? 'problem' : 'hint'}>
          {error ?? (touched ? problem : null) ?? 'Sign in with a username and password, or choose a NATS credentials file.'}
        </p>
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null || busy} onClick={confirm}>
            {busy ? 'Connecting…' : 'Add Server'}
          </button>
        </div>
      </div>
    </div>
  )
}

/** Dialog for editing a saved NATS server without exposing its password. */
function EditServerDialog({ server, onClose }: { server: NatsServerSettings; onClose: () => void }) {
  const [name, setName] = useState(server.name)
  const [url, setUrl] = useState(server.url)
  const [username, setUsername] = useState(server.username)
  const [password, setPassword] = useState('')
  const [credentialsFile, setCredentialsFile] = useState(server.credentialsFile)
  const [removePassword, setRemovePassword] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [])

  const trimmedName = name.trim()
  const trimmedUrl = url.trim()
  const problem = !trimmedName ? 'Enter a name.' : !trimmedUrl ? 'Enter a server URL.' : null
  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    setError(null)
    try {
      await updateNatsServer(
        {
          id: server.id,
          name: trimmedName,
          url: trimmedUrl,
          username: username.trim(),
          password,
          credentialsFile,
          buckets: server.buckets,
        },
        server.hasPassword && !removePassword && !password && !credentialsFile,
      )
      onClose()
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose()
      }}
    >
      <div className="modal" role="dialog" aria-modal="true">
        <h2>Edit NATS Server</h2>
        <input ref={inputRef} value={name} placeholder="Name, e.g. Office" onChange={(event) => setName(event.target.value)} />
        <input value={url} placeholder="URL, e.g. nats://localhost:4222" onChange={(event) => setUrl(event.target.value)} />
        <input
          value={username}
          autoComplete="username"
          placeholder="Username (optional)"
          disabled={Boolean(credentialsFile)}
          onChange={(event) => setUsername(event.target.value)}
        />
        <input
          value={password}
          type="password"
          autoComplete="new-password"
          placeholder={
            removePassword
              ? 'The saved password will be removed'
              : server.hasPassword
                ? 'Saved. Enter a new password to replace it.'
                : 'Password (optional)'
          }
          disabled={Boolean(credentialsFile) || removePassword}
          onChange={(event) => {
            setPassword(event.target.value)
            if (event.target.value) setRemovePassword(false)
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter') void confirm()
            if (event.key === 'Escape' && !busy) onClose()
          }}
        />
        {server.hasPassword && !credentialsFile && (
          <button
            type="button"
            className="link-button dialog-link"
            onClick={() => {
              setRemovePassword((remove) => !remove)
              setPassword('')
            }}
          >
            {removePassword ? 'Keep Saved Password' : 'Remove Saved Password'}
          </button>
        )}
        <NatsCredentialsPicker
          value={credentialsFile}
          onChange={(path) => {
            setCredentialsFile(path)
            if (path) {
              setUsername('')
              setPassword('')
              setRemovePassword(false)
            }
          }}
        />
        <p className={error || problem ? 'problem' : 'hint'}>
          {error ?? problem ?? 'The connection is tested before your changes are saved.'}
        </p>
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null || busy} onClick={() => void confirm()}>
            {busy ? 'Connecting…' : 'Save Changes'}
          </button>
        </div>
      </div>
    </div>
  )
}

/** Dialog for creating a bucket on a NATS server (boards are stored in buckets). */
function NewBucketDialog({ source, onClose }: { source: SourceInfo; onClose: () => void }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const trimmed = name.trim()
  // Mirrors BoardStore::bucket_name_problem (JetStream Object Store bucket names).
  const problem = !trimmed
    ? 'Enter a bucket name.'
    : !/^[A-Za-z0-9_-]+$/.test(trimmed)
      ? 'Bucket names can only contain letters, digits, “-” and “_”.'
      : trimmed.length > 64
        ? 'That bucket name is too long.'
        : source.groups.some((g) => g.id === trimmed)
          ? 'That bucket already exists.'
          : null

  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    setError(null)
    try {
      await addBucket(source.id, trimmed)
      // Open the new bucket so the next "New Board" goes there.
      await activateSource(source.id, trimmed)
      onClose()
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose()
      }}
    >
      <div className="modal" role="dialog" aria-modal="true">
        <h2>New Bucket on {source.name}</h2>
        <input
          ref={inputRef}
          value={name}
          placeholder="e.g. team-boards"
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape' && !busy) onClose()
          }}
        />
        <p className={error || (trimmed && problem) ? 'problem' : 'hint'}>
          {error ?? (trimmed && problem) ?? 'Boards in a bucket are shared with everyone using this server.'}
        </p>
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null || busy} onClick={confirm}>
            {busy ? 'Creating…' : 'Create Bucket'}
          </button>
        </div>
      </div>
    </div>
  )
}

// MARK: - Dialogs and menus

/** Dialog for naming a new board or renaming one, with live validation (mirrors the store). */
function NameDialog({
  title,
  confirmTitle,
  initialName,
  boards,
  excludingId,
  onConfirm,
  onCancel,
}: {
  title: string
  confirmTitle: string
  initialName: string
  boards: BoardInfo[]
  excludingId?: string
  onConfirm: (name: string) => void
  onCancel: () => void
}) {
  const [name, setName] = useState(initialName)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [])

  const problem = validateName(name, boards, excludingId)
  const trimmed = name.trim()
  const showProblem = trimmed.length > 0 && problem !== null

  const confirm = () => {
    if (problem) return
    onConfirm(name)
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onCancel()
      }}
    >
      <div className="modal" role="dialog" aria-modal="true">
        <h2>{title}</h2>
        <input
          ref={inputRef}
          value={name}
          placeholder="e.g. Sprint planning"
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape') onCancel()
          }}
        />
        <p className={showProblem ? 'problem' : 'hint'}>
          {showProblem ? problem : trimmed ? `Saved as “${trimmed}.excalidraw”` : ''}
        </p>
        <div className="modal-actions">
          <button className="secondary" onClick={onCancel}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null} onClick={confirm}>
            {confirmTitle}
          </button>
        </div>
      </div>
    </div>
  )
}

// MARK: - Settings

type SettingsTab = 'appearance' | 'boards' | 'collaboration' | 'servers' | 'ai' | 'about'

const SETTINGS_TABS: { id: SettingsTab; title: string; description: string; Icon: (props: React.SVGProps<SVGSVGElement>) => React.ReactElement }[] = [
  { id: 'appearance', title: 'Appearance', description: 'Adjust how the canvas and the interface look.', Icon: AppearanceIcon },
  { id: 'boards', title: 'Boards', description: 'How boards are listed, and the folders on this computer where they’re saved.', Icon: FolderIcon },
  {
    id: 'collaboration',
    title: 'Collaboration',
    description: 'Control how you appear to others when you edit a shared board together.',
    Icon: UsersIcon,
  },
  {
    id: 'servers',
    title: 'NATS Servers',
    description: 'Connect to NATS servers to store boards in shared buckets and collaborate in real time.',
    Icon: CloudIcon,
  },
  {
    id: 'ai',
    title: 'Merlin',
    description: 'Chat with Merlin about the open board, using any OpenAI-compatible API.',
    Icon: SparklesIcon,
  },
  { id: 'about', title: 'About', description: 'Version and credits.', Icon: InfoIcon },
]

/** One labelled setting: label and help text on the left, the control on the right (or below when `stacked`). */
function SettingsField({
  label,
  help,
  htmlFor,
  stacked = false,
  children,
}: {
  label: string
  help?: React.ReactNode
  htmlFor?: string
  stacked?: boolean
  children: React.ReactNode
}) {
  return (
    <div className={`settings-field${stacked ? ' stacked' : ''}`}>
      <div className="settings-field-text">
        <label className="settings-field-label" htmlFor={htmlFor}>
          {label}
        </label>
        {help && <p className="settings-field-help">{help}</p>}
      </div>
      <div className="settings-field-control">{children}</div>
    </div>
  )
}

/** Settings, split into tabs with a vertical navigation list on the left. */
function SettingsModal({ snapshot, onClose }: { snapshot: StoreSnapshot; onClose: () => void }) {
  const [tab, setTab] = useState<SettingsTab>('appearance')
  const [addingServer, setAddingServer] = useState(false)
  const [editingServer, setEditingServer] = useState<NatsServerSettings | null>(null)
  const [removingServer, setRemovingServer] = useState<SourceInfo | null>(null)
  const [newBucketFor, setNewBucketFor] = useState<SourceInfo | null>(null)
  const tabRefs = useRef(new Map<SettingsTab, HTMLButtonElement>())
  const nestedDialogOpen = addingServer || editingServer !== null || removingServer !== null || newBucketFor !== null

  // Escape closes Settings, unless one of its own dialogs is open on top.
  useEffect(() => {
    if (nestedDialogOpen) return
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', closeOnEscape)
    return () => window.removeEventListener('keydown', closeOnEscape)
  }, [nestedDialogOpen, onClose])

  // Arrow keys move between tabs, as in a native vertical tab list.
  const onTabKeyDown = (event: React.KeyboardEvent) => {
    const index = SETTINGS_TABS.findIndex((item) => item.id === tab)
    let next = index
    if (event.key === 'ArrowDown') next = (index + 1) % SETTINGS_TABS.length
    else if (event.key === 'ArrowUp') next = (index - 1 + SETTINGS_TABS.length) % SETTINGS_TABS.length
    else if (event.key === 'Home') next = 0
    else if (event.key === 'End') next = SETTINGS_TABS.length - 1
    else return
    event.preventDefault()
    const id = SETTINGS_TABS[next].id
    setTab(id)
    tabRefs.current.get(id)?.focus()
  }

  const current = SETTINGS_TABS.find((item) => item.id === tab) ?? SETTINGS_TABS[0]

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div className="modal settings" role="dialog" aria-modal="true" aria-labelledby="settings-title">
        <nav className="settings-nav" aria-label="Settings sections">
          <h2 id="settings-title" className="settings-nav-title">
            Settings
          </h2>
          <div role="tablist" aria-orientation="vertical" onKeyDown={onTabKeyDown}>
            {SETTINGS_TABS.map(({ id, title, Icon }) => (
              <button
                key={id}
                ref={(element) => {
                  if (element) tabRefs.current.set(id, element)
                  else tabRefs.current.delete(id)
                }}
                id={`settings-tab-${id}`}
                role="tab"
                aria-selected={tab === id}
                aria-controls={`settings-panel-${id}`}
                tabIndex={tab === id ? 0 : -1}
                className={`settings-nav-item${tab === id ? ' active' : ''}`}
                onClick={() => setTab(id)}
              >
                <Icon />
                {title}
              </button>
            ))}
          </div>
        </nav>
        <section className="settings-pane">
          <header className="settings-pane-header">
            <div>
              <h3>{current.title}</h3>
              <p>{current.description}</p>
            </div>
            <button className="icon-button settings-close" onClick={onClose} title="Close (Esc)" aria-label="Close settings">
              <CloseIcon />
            </button>
          </header>
          {/* Every page stays mounted (only hidden), so unsaved edits survive switching tabs. */}
          {SETTINGS_TABS.map(({ id }) => (
            <div
              key={id}
              id={`settings-panel-${id}`}
              className="settings-pane-body"
              role="tabpanel"
              aria-labelledby={`settings-tab-${id}`}
              hidden={tab !== id}
            >
              {id === 'appearance' && <AppearanceSettings snapshot={snapshot} />}
              {id === 'boards' && <BoardsSettings snapshot={snapshot} />}
              {id === 'collaboration' && (
                <>
                  <CollaborationSettings snapshot={snapshot} />
                  <CollaborationAppearanceSettings snapshot={snapshot} />
                </>
              )}
              {id === 'servers' && (
                <ServersSettings
                  snapshot={snapshot}
                  onAdd={() => setAddingServer(true)}
                  onEdit={setEditingServer}
                  onRemove={setRemovingServer}
                  onNewBucket={setNewBucketFor}
                />
              )}
              {id === 'ai' && <AISettingsPane snapshot={snapshot} />}
              {id === 'about' && <AboutSettings />}
            </div>
          ))}
        </section>
      </div>
      {addingServer && <AddServerDialog onClose={() => setAddingServer(false)} />}
      {editingServer && <EditServerDialog server={editingServer} onClose={() => setEditingServer(null)} />}
      {removingServer && <RemoveServerDialog source={removingServer} onClose={() => setRemovingServer(null)} />}
      {newBucketFor && <NewBucketDialog source={newBucketFor} onClose={() => setNewBucketFor(null)} />}
    </div>
  )
}

// Mirrors minApplicationZoom / maxApplicationZoom / applicationZoomStep in app.go.
const ZOOM_MIN = 50
const ZOOM_MAX = 200
const ZOOM_STEP = 10
const ZOOM_SHORTCUTS = IS_MAC ? '⌘+ and ⌘−' : 'Ctrl++ and Ctrl+−'

function AppearanceSettings({ snapshot }: { snapshot: StoreSnapshot }) {
  const zoomPercent = Math.round(snapshot.applicationZoom * 100)
  const stepZoom = (delta: number) => {
    const next = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, zoomPercent + delta))
    setApplicationZoom(next / 100).catch((err) => log('error', `Could not change the interface zoom: ${String(err)}`))
  }
  return (
    <>
      <div className="settings-group">
        <SettingsField label="Canvas theme" help="The rest of the app follows your system appearance.">
          <div className="segmented" role="radiogroup" aria-label="Canvas theme">
            {(['system', 'light', 'dark'] as const).map((appearance) => (
              <button
                key={appearance}
                role="radio"
                aria-checked={snapshot.canvasAppearance === appearance}
                className={snapshot.canvasAppearance === appearance ? 'active' : ''}
                onClick={() => setCanvasAppearance(appearance)}
              >
                {APPEARANCE_TITLES[appearance]}
              </button>
            ))}
          </div>
        </SettingsField>
        <SettingsField label="Interface zoom" help={`Scales the whole interface. Shortcuts: ${ZOOM_SHORTCUTS}.`}>
          {/* Steps rather than a slider: rescaling the webview under a dragged
              slider moves the slider itself, which makes it jump. */}
          <div className="zoom-setting">
            <div className="stepper" role="group" aria-label="Interface zoom">
              <button aria-label="Zoom out" disabled={zoomPercent <= ZOOM_MIN} onClick={() => stepZoom(-ZOOM_STEP)}>
                <MinusIcon />
              </button>
              <output aria-live="polite">{zoomPercent}%</output>
              <button aria-label="Zoom in" disabled={zoomPercent >= ZOOM_MAX} onClick={() => stepZoom(ZOOM_STEP)}>
                <PlusIcon />
              </button>
            </div>
            <button className="secondary" disabled={zoomPercent === 100} onClick={() => void setApplicationZoom(1)}>
              Reset
            </button>
          </div>
        </SettingsField>
      </div>
      <div className="settings-group">
        <SettingsField label="Board thumbnails" help="Show a preview of each board in the sidebar." htmlFor="settings-thumbnails">
          <input
            id="settings-thumbnails"
            className="switch"
            type="checkbox"
            role="switch"
            checked={snapshot.showThumbnails}
            onChange={(event) => setShowThumbnails(event.target.checked)}
          />
        </SettingsField>
      </div>
    </>
  )
}

function BoardsSettings({ snapshot }: { snapshot: StoreSnapshot }) {
  // The folders the sidebar shows under Local. Group ids are full paths; names are shortened with ~.
  const folders = snapshot.sources.find((source) => source.kind === 'local')?.groups ?? []
  const report = (action: string) => (err: unknown) => log('error', `Could not ${action}: ${String(err)}`)
  // Power users, and desktops whose native picker cannot select a directory, can
  // type the location instead of using the dialog.
  const [folderPath, setFolderPath] = useState<string | null>(null)
  const addTypedFolder = () => {
    const value = folderPath?.trim() ?? ''
    if (!value) return
    addLocalFolderPath(value)
      .then(() => setFolderPath(null))
      .catch(report('add the folder'))
  }
  return (
    <>
      <div className="settings-group">
        <SettingsField label="Sort boards" help="The order of the boards in the sidebar.">
          <div className="segmented" role="radiogroup" aria-label="Sort boards">
            {(
              [
                ['name', 'Name'],
                ['dateEdited', 'Date Edited'],
              ] as const
            ).map(([order, title]) => (
              <button
                key={order}
                role="radio"
                aria-checked={snapshot.sortOrder === order}
                className={snapshot.sortOrder === order ? 'active' : ''}
                onClick={() => setSort(order)}
              >
                {title}
              </button>
            ))}
          </div>
        </SettingsField>
      </div>
      <div className="settings-group">
        <SettingsField
          label="Boards folders"
          help="Boards are saved as plain .excalidraw files. To keep a history, make a folder a Git repository."
          stacked
        >
          {folders.map((folder) => (
            <div key={folder.id} className="folder-setting">
              <FolderIcon className="folder-icon" />
              <div className="folder-text">
                <span className="folder-name">{folderDisplayName(folder.name)}</span>
                <span className="folder-path" title={folder.id}>
                  {folder.name}
                </span>
              </div>
              <div className="settings-actions">
                <button className="secondary" onClick={() => revealFolder(folder.id).catch(report('reveal the folder'))}>
                  Show in {FILE_MANAGER}
                </button>
                <button
                  className="secondary"
                  disabled={folders.length <= 1}
                  title={folders.length <= 1 ? 'There must be at least one folder.' : 'Removes the folder from the sidebar. Its boards stay on disk.'}
                  onClick={() => removeLocalFolder(folder.id).catch(report('remove the folder'))}
                >
                  Remove
                </button>
              </div>
            </div>
          ))}
          <div className="settings-pane-actions">
            <button className="secondary" onClick={() => addLocalFolder().catch(report('add the folder'))}>
              Add Folder
            </button>
            <button
              className="secondary"
              title="Type the folder location instead of using the file picker."
              onClick={() => setFolderPath(folderPath === null ? '' : null)}
            >
              {folderPath === null ? 'Add by Path…' : 'Cancel'}
            </button>
          </div>
          {folderPath !== null && (
            <form
              className="settings-pane-actions"
              onSubmit={(event) => {
                event.preventDefault()
                addTypedFolder()
              }}
            >
              <input
                value={folderPath}
                autoFocus
                placeholder="/home/you/boards"
                aria-label="Boards folder path"
                onChange={(event) => setFolderPath(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Escape') setFolderPath(null)
                }}
              />
              <button type="submit" className="secondary">
                Add
              </button>
            </form>
          )}
        </SettingsField>
      </div>
      {IS_MAC && (
        <p className="settings-footnote">
          Avoid folders inside Documents, Desktop or Downloads. macOS restricts access to them, so Finder can’t reveal individual boards.
        </p>
      )}
    </>
  )
}

function CollaborationAppearanceSettings({ snapshot }: { snapshot: StoreSnapshot }) {
  const { collaborationColor: color, collaborationAvatar: avatar, collaborationAvatarOnly: avatarOnly } = snapshot
  const [error, setError] = useState<string | null>(null)

  const apply = async (nextColor: string, nextAvatar: string, nextAvatarOnly = avatarOnly) => {
    setError(null)
    try {
      await setCollaborationAppearance(nextColor, nextAvatar, nextAvatarOnly)
    } catch (err) {
      setError(String(err))
    }
  }

  return (
    <div className="settings-group">
      <SettingsField
        label="Preview"
        help={error ? <span className="problem">{error}</span> : 'How other people see your cursor on a shared board.'}
        stacked
      >
        <CollaboratorPreview name={snapshot.collaborationNickname} color={color} avatar={avatar} avatarOnly={avatarOnly} />
      </SettingsField>
      <SettingsField
        label="Color"
        help="The color of your cursor and name. Automatic gives you a different color each session."
        stacked
      >
        <div className="collab-color-picker" role="radiogroup" aria-label="Cursor color">
          <button
            type="button"
            role="radio"
            aria-checked={color === ''}
            className={`collab-color-auto${color === '' ? ' selected' : ''}`}
            onClick={() => void apply('', avatar)}
          >
            Automatic
          </button>
          {COLOR_PRESETS.map((preset) => (
            <button
              key={preset}
              type="button"
              role="radio"
              aria-checked={color === preset}
              aria-label={preset}
              className={`collab-color-swatch${color === preset ? ' selected' : ''}`}
              style={{ background: preset }}
              onClick={() => void apply(preset, avatar)}
            />
          ))}
          <label className={`collab-color-swatch collab-color-custom${color !== '' && !COLOR_PRESETS.includes(color) ? ' selected' : ''}`} title="Custom color">
            <input
              type="color"
              value={color || '#6965db'}
              aria-label="Custom color"
              onChange={(event) => void apply(event.target.value, avatar)}
            />
          </label>
        </div>
      </SettingsField>
      <SettingsField label="Avatar" help="An emoji that floats next to your cursor for the other people on a board." stacked>
        <div className="collab-avatar-picker" role="radiogroup" aria-label="Avatar">
          <button
            type="button"
            role="radio"
            aria-checked={avatar === ''}
            className={`collab-avatar-option collab-avatar-none${avatar === '' ? ' selected' : ''}`}
            title="No avatar"
            onClick={() => void apply(color, '')}
          >
            None
          </button>
          {AVATAR_EMOJIS.map((emoji) => (
            <button
              key={emoji}
              type="button"
              role="radio"
              aria-checked={avatar === emoji}
              aria-label={emoji}
              className={`collab-avatar-option${avatar === emoji ? ' selected' : ''}`}
              onClick={() => void apply(color, emoji)}
            >
              {emoji}
            </button>
          ))}
        </div>
      </SettingsField>
      <SettingsField
        label="Show"
        help={avatar ? 'What others see beside your cursor.' : 'Choose an avatar first. Without one, others see just your name.'}
      >
        <div className="segmented" role="radiogroup" aria-label="What others see beside your cursor">
          {([[false, 'Avatar and name'], [true, 'Avatar only']] as const).map(([only, title]) => (
            <button
              key={title}
              role="radio"
              aria-checked={avatarOnly === only}
              className={avatarOnly === only ? 'active' : ''}
              disabled={avatar === ''}
              onClick={() => void apply(color, avatar, only)}
            >
              {title}
            </button>
          ))}
        </div>
      </SettingsField>
    </div>
  )
}

/** A mock of a remote cursor, drawn with the same component the canvas uses for peers with an avatar. */
function CollaboratorPreview({ name, color, avatar, avatarOnly }: { name: string; color: string; avatar: string; avatarOnly: boolean }) {
  const shown = peerColor('preview', color || undefined)
  const label = name || 'Anonymous'
  return (
    <div className="collab-preview" aria-label="Preview of how others see you">
      <div className="collab-preview-stage">
        <RemoteCursor
          item={{
            id: 'preview',
            x: 0,
            y: 0,
            emoji: avatar,
            // Without an avatar the name is all there is to show.
            name: avatar && avatarOnly ? '' : label,
            stroke: shown.stroke,
            background: shown.background,
            down: false,
          }}
        />
      </div>
    </div>
  )
}

function CollaborationSettings({ snapshot }: { snapshot: StoreSnapshot }) {
  const [nickname, setNickname] = useState(snapshot.collaborationNickname)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const unchanged = nickname.trim() === snapshot.collaborationNickname

  const save = async () => {
    if (saving || unchanged) return
    setSaving(true)
    setError(null)
    try {
      await setCollaborationNickname(nickname)
      setNickname(nickname.trim())
      setSaved(true)
    } catch (err) {
      setError(String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="settings-group">
      <SettingsField
        label="Display name"
        help={
          error ? (
            <span className="problem">{error}</span>
          ) : (
            'Shown next to your cursor on shared boards. Leave it empty to appear as an anonymous peer.'
          )
        }
        htmlFor="settings-nickname"
        stacked
      >
        <div className="inline-form">
          <input
            id="settings-nickname"
            className="settings-text-input"
            value={nickname}
            maxLength={40}
            placeholder="Anonymous"
            onChange={(event) => {
              setNickname(event.target.value)
              setSaved(false)
              setError(null)
            }}
            onKeyDown={(event) => {
              if (event.key === 'Enter') void save()
            }}
          />
          <button className="primary" disabled={saving || unchanged} onClick={() => void save()}>
            {saving ? 'Saving…' : saved && unchanged ? 'Saved' : 'Save'}
          </button>
        </div>
      </SettingsField>
    </div>
  )
}

function serverAuthSummary(server: NatsServerSettings | undefined): string {
  if (!server) return ''
  if (server.credentialsFile) return 'Credentials file'
  if (server.username) return `User “${server.username}”`
  return 'No authentication'
}

function ServersSettings({
  snapshot,
  onAdd,
  onEdit,
  onRemove,
  onNewBucket,
}: {
  snapshot: StoreSnapshot
  onAdd: () => void
  onEdit: (server: NatsServerSettings) => void
  onRemove: (source: SourceInfo) => void
  onNewBucket: (source: SourceInfo) => void
}) {
  const natsSources = snapshot.sources.filter((source) => source.kind === 'nats')
  return (
    <>
      {natsSources.length === 0 ? (
        <div className="settings-group settings-empty">
          <p className="settings-empty-title">No servers</p>
          <p className="settings-field-help">Add a NATS server to store boards in shared buckets and edit them together in real time.</p>
        </div>
      ) : (
        natsSources.map((source) => {
          const server = snapshot.natsServers.find((item) => item.id === rawServerId(source.id))
          return (
            <div key={source.id} className="settings-group nats-server">
              <div className="nats-server-head">
                <CloudIcon className="nats-server-icon" />
                <div className="nats-server-text">
                  <span className="nats-server-name">{source.name}</span>
                  <span className="settings-field-help">
                    {server?.url}
                    {server && ' · '}
                    {serverAuthSummary(server)}
                  </span>
                </div>
                <div className="settings-actions">
                  <button className="secondary" disabled={!server} onClick={() => server && onEdit(server)}>
                    Edit
                  </button>
                  <button className="secondary" onClick={() => onRemove(source)}>
                    Remove
                  </button>
                </div>
              </div>
              <div className="nats-buckets">
                <span className="settings-field-label">Buckets</span>
                <div className="nats-bucket-list">
                  {source.groups.length === 0 ? (
                    <span className="settings-field-help">No buckets yet.</span>
                  ) : (
                    source.groups.map((group) => (
                      <span key={group.id} className="nats-bucket">
                        {group.name}
                      </span>
                    ))
                  )}
                  <button className="secondary small" onClick={() => onNewBucket(source)}>
                    <PlusIcon />
                    New Bucket
                  </button>
                </div>
              </div>
            </div>
          )
        })
      )}
      <div className="settings-pane-actions">
        <button className="primary" onClick={onAdd}>
          Add Server
        </button>
      </div>
      <p className="settings-footnote">Removing a server only disconnects this app. Boards stay on the server.</p>
    </>
  )
}

function AISettingsPane({ snapshot }: { snapshot: StoreSnapshot }) {
  const [enabled, setEnabled] = useState(snapshot.ai.enabled)
  const [baseUrl, setBaseUrl] = useState(snapshot.ai.baseUrl)
  const [model, setModel] = useState(snapshot.ai.model)
  const [apiKey, setApiKey] = useState('')
  const [removingKey, setRemovingKey] = useState(false)
  const [models, setModels] = useState<AIModel[]>([])
  const [modelsLoading, setModelsLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const loadedInitialModels = useRef(false)

  const loadModels = useCallback(async () => {
    setModelsLoading(true)
    setError(null)
    try {
      const discovered = await discoverAIModels(baseUrl.trim(), apiKey, false)
      setModels(discovered)
      setModel((current) => {
        const match = discovered.find((item) => item.model === current || item.id === current)
        return match?.model ?? discovered[0]?.model ?? ''
      })
    } catch (err) {
      setModels([])
      setError(String(err))
    } finally {
      setModelsLoading(false)
    }
  }, [apiKey, baseUrl])

  useEffect(() => {
    if (loadedInitialModels.current || !enabled || !baseUrl.trim()) return
    loadedInitialModels.current = true
    void loadModels()
  }, [baseUrl, enabled, loadModels])

  const changed = () => {
    setSaved(false)
    setError(null)
  }
  const modelChosen = models.some((item) => item.model === model)

  // Removes only the saved key, right away, keeping any other unsaved edits.
  const removeKey = async () => {
    setRemovingKey(true)
    setError(null)
    try {
      await setAISettings({
        enabled: snapshot.ai.enabled,
        baseUrl: snapshot.ai.baseUrl,
        model: snapshot.ai.model,
        apiKey: '',
        clearApiKey: true,
      })
      setApiKey('')
    } catch (err) {
      setError(String(err))
    } finally {
      setRemovingKey(false)
    }
  }

  const save = async () => {
    setSaving(true)
    setError(null)
    try {
      await setAISettings({ enabled, baseUrl: baseUrl.trim(), model: model.trim(), apiKey, clearApiKey: false })
      setApiKey('')
      setSaved(true)
    } catch (err) {
      setError(String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <>
      <div className="settings-group">
        <SettingsField label="Enable Merlin" help="Adds an expandable assistant panel beside the canvas." htmlFor="settings-ai-enabled">
          <input
            id="settings-ai-enabled"
            className="switch"
            type="checkbox"
            role="switch"
            checked={enabled}
            onChange={(event) => {
              setEnabled(event.target.checked)
              changed()
            }}
          />
        </SettingsField>
      </div>
      <div className="settings-group">
        <SettingsField label="API URL" help="The base URL of an OpenAI-compatible API." htmlFor="settings-ai-url" stacked>
          <input
            id="settings-ai-url"
            className="settings-text-input"
            value={baseUrl}
            placeholder="http://localhost:8080/v1"
            onChange={(event) => {
              setBaseUrl(event.target.value)
              setModels([])
              changed()
            }}
          />
        </SettingsField>
        <SettingsField label="API key" help="Optional. Stored outside the app’s web view." htmlFor="settings-ai-key" stacked>
          <div className="inline-form">
            <input
              id="settings-ai-key"
              className="settings-text-input"
              type="password"
              value={apiKey}
              placeholder={snapshot.ai.hasApiKey ? 'Saved. Enter a new key to replace it.' : 'None'}
              onChange={(event) => {
                setApiKey(event.target.value)
                setModels([])
                changed()
              }}
            />
            {snapshot.ai.hasApiKey && (
              <button className="secondary" disabled={removingKey} onClick={() => void removeKey()}>
                {removingKey ? 'Removing…' : 'Remove Key'}
              </button>
            )}
          </div>
        </SettingsField>
        <SettingsField label="Model" help="Load the models available at the API URL, then choose one." htmlFor="settings-ai-model" stacked>
          <div className="inline-form">
            <select
              id="settings-ai-model"
              className="settings-text-input"
              value={modelChosen ? model : ''}
              disabled={modelsLoading || models.length === 0}
              onChange={(event) => {
                setModel(event.target.value)
                changed()
              }}
            >
              <option value="">{modelsLoading ? 'Loading models…' : models.length === 0 ? 'No models loaded' : 'Choose a model'}</option>
              {models.map((item) => (
                <option key={item.model} value={item.model} title={item.model}>
                  {item.id}
                </option>
              ))}
            </select>
            <button className="secondary" disabled={modelsLoading || !baseUrl.trim()} onClick={() => void loadModels()}>
              {modelsLoading ? 'Loading…' : 'Load Models'}
            </button>
          </div>
        </SettingsField>
      </div>
      <p className={error ? 'settings-footnote problem' : 'settings-footnote'}>
        {error ?? 'The board is only sent to the API when Merlin asks to read it.'}
      </p>
      <div className="settings-pane-actions">
        <button className="primary" disabled={saving || saved || (enabled && !modelChosen)} onClick={() => void save()}>
          {saving ? 'Saving…' : saved ? 'Saved' : 'Save'}
        </button>
      </div>
    </>
  )
}

function AboutSettings() {
  const [version, setVersion] = useState<AppVersionInfo | null>(null)
  useEffect(() => {
    appVersion()
      .then(setVersion)
      .catch((err) => log('error', `Could not read the app version: ${String(err)}`))
  }, [])
  return (
    <div className="settings-group settings-about">
      <p className="settings-about-name">Excalibur</p>
      {version && (
        <p className="settings-about-version">
          {version.version
            ? `Version ${version.version}${version.development ? ' (development build)' : ''}`
            : 'Development build'}
        </p>
      )}
      <p className="settings-field-help">The board of legend. A desktop home for your Excalidraw boards.</p>
      <p className="settings-field-help">
        Copyright © 2026{' '}
        <a href="https://github.com/laetho" target="_blank" rel="noreferrer">
          laetho
        </a>{' '}
        and{' '}
        <a href="https://github.com/niklashaakonsen" target="_blank" rel="noreferrer">
          niklashaakonsen
        </a>
        .
      </p>
    </div>
  )
}

/** Confirms disconnecting a NATS server. Its boards stay on the server. */
function RemoveServerDialog({ source, onClose }: { source: SourceInfo; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      await removeNatsServer(rawServerId(source.id))
      onClose()
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }
  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose()
      }}
      onKeyDown={(event) => {
        if (event.key === 'Escape' && !busy) onClose()
      }}
    >
      <div className="modal alert" role="alertdialog" aria-modal="true" aria-labelledby="remove-server-title">
        <h2 id="remove-server-title">Remove “{source.name}”?</h2>
        <p className="alert-message">Its boards stay on the server. Add the server again to see them.</p>
        {error && <p className="problem">{error}</p>}
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onClose} autoFocus>
            Cancel
          </button>
          <button className="destructive" disabled={busy} onClick={() => void remove()}>
            {busy ? 'Removing…' : 'Remove Server'}
          </button>
        </div>
      </div>
    </div>
  )
}

/** Right-click menu on a board in the sidebar. */
function ContextMenu({
  x,
  y,
  onRename,
  onDuplicate,
  onReveal,
  onTrash,
}: {
  x: number
  y: number
  onRename: () => void
  onDuplicate: () => void
  onReveal: () => void
  onTrash: () => void
}) {
  return (
    <div className="context-menu" style={{ left: x, top: y }} role="menu">
      <button role="menuitem" onClick={onRename}>
        Rename
      </button>
      <button role="menuitem" onClick={onDuplicate}>
        Duplicate
      </button>
      <button role="menuitem" onClick={onReveal}>
        Show in {FILE_MANAGER}
      </button>
      <div className="context-menu-divider" />
      <button role="menuitem" className="destructive" onClick={onTrash}>
        Move to Trash
      </button>
    </div>
  )
}
