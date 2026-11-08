import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Canvas from './Canvas'
import {
  activateSource,
  addBucket,
  addNatsServer,
  aiChat,
  chooseFolder,
  chooseImport,
  copyBoardToSource,
  createBoard,
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
  renameBoard,
  reveal,
  revealFolder,
  selectBoard,
  setCanvasAppearance,
  setApplicationZoom,
  setAISettings,
  setShowThumbnails,
  setSort,
  setWindowTitle,
  trashBoard,
  type BoardInfo,
  type BoardSortOrder,
  type CanvasAppearance,
  type AIChatMessage,
  type AIModel,
  type NatsServerConfig,
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

function lastPathComponent(path: string): string {
  const parts = path.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? path
}

const APPEARANCE_TITLES: Record<CanvasAppearance, string> = { system: 'Match System', light: 'Light', dark: 'Dark' }

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
  const [search, setSearch] = useState('')
  /** Refreshed every 30 s so "Edited … ago" stays current. */
  const [now, setNow] = useState(() => Date.now() / 1000)
  const [creating, setCreating] = useState(false)
  const [renaming, setRenaming] = useState<BoardInfo | null>(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [viewOptionsOpen, setViewOptionsOpen] = useState(false)
  const [addServerOpen, setAddServerOpen] = useState(false)
  const [dropTargeted, setDropTargeted] = useState(false)
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; board: BoardInfo } | null>(null)
  /** Right-click menu on a NATS server heading in the sources list. */
  const [sourceMenu, setSourceMenu] = useState<{ x: number; y: number; source: SourceInfo } | null>(null)
  /** The NATS server a new bucket is being created on. */
  const [newBucketFor, setNewBucketFor] = useState<SourceInfo | null>(null)
  const [aiOpen, setAIOpen] = useState(false)

  // Menu handlers and drop handlers read the latest snapshot without re-subscribing.
  const snapshotRef = useRef<StoreSnapshot | null>(null)
  useEffect(() => {
    snapshotRef.current = snapshot
  }, [snapshot])

  const handleMenu = useCallback((id: string) => {
    const current = snapshotRef.current
    if (!current) return
    switch (id) {
      case 'new-board':
        setCreating(true)
        break
      case 'import':
        chooseImport()
        break
      case 'choose-folder':
        chooseFolder()
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
    }
  }, [])

  useEffect(() => {
    getState()
      .then((snapshot) => {
        setSnapshot(snapshot)
        // Open the selected board on startup (mirrors Swift's `.task(id: store.selection)`),
        // and again after a web-content reload, like Swift's `canvas.onReloaded`.
        if (snapshot.selection) selectBoard(snapshot.selection)
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

  // Close the menus and the view-options popover on a click outside them. Listens to `pointerdown` (capture
  // phase) and ignores clicks inside: React runs this effect synchronously during the click that opened the
  // popover, so a `click` listener on window would receive that same click and close it again right away.
  // Not `mousedown`: Excalidraw prevents the default of `pointerdown` on the canvas, which suppresses it.
  useEffect(() => {
    if (!contextMenu && !sourceMenu && !viewOptionsOpen) return
    const close = (event: Event) => {
      if (event.target instanceof Element && event.target.closest('.view-options, .context-menu')) return
      setContextMenu(null)
      setSourceMenu(null)
      setViewOptionsOpen(false)
    }
    const closeOnBlur = () => {
      setContextMenu(null)
      setSourceMenu(null)
      setViewOptionsOpen(false)
    }
    window.addEventListener('pointerdown', close, true)
    window.addEventListener('blur', closeOnBlur)
    return () => {
      window.removeEventListener('pointerdown', close, true)
      window.removeEventListener('blur', closeOnBlur)
    }
  }, [contextMenu, sourceMenu, viewOptionsOpen])

  // The window title: the open board and when it was last edited (as the SwiftUI app's title and subtitle).
  const openBoard = snapshot?.boards.find((b) => b.id === snapshot.selection)
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

  const query = search.trim()
  const visibleBoards = useMemo(() => {
    const sorted = snapshot ? sortBoards(snapshot.boards, snapshot.sortOrder) : []
    if (!query) return sorted.map((board) => ({ board, match: null as Match | null }))
    return sorted.flatMap((board) => {
      const match = searchMatch(board, query)
      return match ? [{ board, match }] : []
    })
  }, [snapshot, query])

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

  if (!snapshot) {
    return (
      <div className="shell">
        <div className="loading">Loading…</div>
      </div>
    )
  }

  const alert = snapshot.isAlertPresented ? snapshot.alerts[0] : null
  const activeNatsSource = snapshot.activeSource.startsWith('nats:') ? snapshot.sources.find((s) => s.id === snapshot.activeSource) : undefined

  return (
    <div className="shell">
      <aside
        className={`sidebar${dropTargeted ? ' drop-target' : ''}`}
        onDragOver={(event) => {
          event.preventDefault()
          setDropTargeted(true)
        }}
        onDragLeave={(event) => {
          if (!event.currentTarget.contains(event.relatedTarget as Node)) setDropTargeted(false)
        }}
        onDrop={handleDrop}
      >
        <div className="sidebar-toolbar">
          <button className="new-board" onClick={() => setCreating(true)} title="New Board (Ctrl+N)">
            New Board
          </button>
          <div className="view-options">
            <button className="icon-button" onClick={() => setViewOptionsOpen((open) => !open)} title="Sort boards and show thumbnails">
              ☰
            </button>
            {viewOptionsOpen && (
              <div className="popover">
                <div className="popover-group">
                  <button className={snapshot.sortOrder === 'name' ? 'active' : ''} onClick={() => setSort('name')}>
                    Sort by Name
                  </button>
                  <button className={snapshot.sortOrder === 'dateEdited' ? 'active' : ''} onClick={() => setSort('dateEdited')}>
                    Sort by Date Edited
                  </button>
                </div>
                <div className="popover-divider" />
                <label className="popover-toggle">
                  <input type="checkbox" checked={snapshot.showThumbnails} onChange={(event) => setShowThumbnails(event.target.checked)} />
                  Show Thumbnails
                </label>
              </div>
            )}
          </div>
          <button className="icon-button" onClick={() => setSettingsOpen(true)} title="Settings (⌘,)">
            ⚙
          </button>
        </div>
        <div className="search">
          <input type="search" placeholder="Search names and text" value={search} onChange={(event) => setSearch(event.target.value)} />
        </div>
        <div className="sources">
          <div className="sources-header">
            <span className="sources-title">Sources</span>
            <button className="icon-button" onClick={() => setAddServerOpen(true)} title="Add a NATS server">
              +
            </button>
          </div>
          <SourcesNav
            sources={snapshot.sources}
            activeSource={snapshot.activeSource}
            activeGroup={snapshot.activeGroup}
            onDropBoard={handleDropBoard}
            onNewBucket={setNewBucketFor}
            onSourceMenu={(event, source) => {
              event.preventDefault()
              setSourceMenu({ x: event.clientX, y: event.clientY, source })
            }}
          />
        </div>
        <div className="board-list">
          {snapshot.boards.length === 0 ? (
            <div className="empty-state">
              <p className="empty-title">No Boards</p>
              <p className="empty-description">
                {activeNatsSource
                  ? `Boards in “${snapshot.activeGroup}” are stored on ${activeNatsSource.name}.`
                  : `Boards are saved as .excalidraw files in ${lastPathComponent(snapshot.folder)}.`}
              </p>
              <button className="primary" onClick={() => setCreating(true)}>
                New Board
              </button>
            </div>
          ) : visibleBoards.length === 0 ? (
            <div className="empty-state">
              <p className="empty-title">No Results for “{query}”</p>
            </div>
          ) : (
            visibleBoards.map(({ board, match }) => (
              <BoardRow
                key={board.id}
                board={board}
                match={match}
                showThumbnail={snapshot.showThumbnails}
                selected={snapshot.selection === board.id}
                now={now}
                onSelect={() => selectBoard(board.id)}
                onContextMenu={(event) => {
                  event.preventDefault()
                  setContextMenu({ x: event.clientX, y: event.clientY, board })
                }}
              />
            ))
          )}
        </div>
      </aside>

      <main className="main">
        <Canvas
          canvasAppearance={snapshot.canvasAppearance}
          collab={
            snapshot.activeSource.startsWith('nats:') && snapshot.selection
              ? { sourceId: snapshot.activeSource, bucket: snapshot.activeGroup, boardId: snapshot.selection }
              : null
          }
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
        {snapshot.ai.enabled && (
          <>
            <button
              className={`ai-launcher${aiOpen ? ' open' : ''}`}
              onClick={() => setAIOpen((open) => !open)}
              title={aiOpen ? 'Close AI assistant' : 'Open AI assistant'}
              aria-expanded={aiOpen}
              aria-controls="ai-chat-panel"
            >
              AI
            </button>
            {aiOpen && (
              <AIChatOverlay baseUrl={snapshot.ai.baseUrl} model={snapshot.ai.model} onClose={() => setAIOpen(false)} />
            )}
          </>
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
      {sourceMenu && (
        <div className="context-menu" style={{ left: sourceMenu.x, top: sourceMenu.y }} role="menu">
          <button
            role="menuitem"
            onClick={() => {
              setNewBucketFor(sourceMenu.source)
              setSourceMenu(null)
            }}
          >
            New Bucket…
          </button>
          <div className="context-menu-divider" />
          <button
            role="menuitem"
            className="destructive"
            title="The boards stay on the server; add it again to see them."
            onClick={() => {
              removeNatsServer(rawServerId(sourceMenu.source.id)).catch((err) => log('error', `Could not remove the server: ${String(err)}`))
              setSourceMenu(null)
            }}
          >
            Remove Server
          </button>
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
  now,
  onSelect,
  onContextMenu,
}: {
  board: BoardInfo
  match: Match | null
  showThumbnail: boolean
  selected: boolean
  now: number
  onSelect: () => void
  onContextMenu: (event: React.MouseEvent) => void
}) {
  return (
    <div
      className={`board-row${selected ? ' selected' : ''}`}
      onClick={onSelect}
      onContextMenu={onContextMenu}
      draggable
      onDragStart={(event) => {
        event.dataTransfer.setData('text/plain', board.id)
        event.dataTransfer.effectAllowed = 'copy'
      }}
    >
      {showThumbnail && (
        <div className="thumbnail">{board.thumbnail ? <img src={board.thumbnail} alt="" draggable={false} /> : null}</div>
      )}
      <div className="board-info">
        <div className="board-name-line">
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

// MARK: - Sources navigation

/** The navigation tree: the local boards folder and every NATS server with its buckets. */
function SourcesNav({
  sources,
  activeSource,
  activeGroup,
  onDropBoard,
  onNewBucket,
  onSourceMenu,
}: {
  sources: SourceInfo[]
  activeSource: string
  activeGroup: string
  onDropBoard: (boardId: string, targetSource: string, targetGroup: string) => void
  onNewBucket: (source: SourceInfo) => void
  onSourceMenu: (event: React.MouseEvent, source: SourceInfo) => void
}) {
  const [dragOver, setDragOver] = useState<string | null>(null)
  return (
    <div className="sources-list">
      {sources.map((source) => (
        <div key={source.id} className="source">
          <div
            className="source-name"
            title={source.kind === 'nats' ? `${source.name} (right-click for options)` : undefined}
            onContextMenu={source.kind === 'nats' ? (event) => onSourceMenu(event, source) : undefined}
          >
            <span className="source-name-text">{source.name}</span>
            {source.kind === 'nats' && (
              <button className="icon-button source-add" onClick={() => onNewBucket(source)} title="New Bucket…">
                +
              </button>
            )}
          </div>
          {source.kind === 'nats' && source.groups.length === 0 && (
            <button className="source-empty" onClick={() => onNewBucket(source)}>
              No buckets yet. Create one…
            </button>
          )}
          {source.groups.map((group) => (
            <button
              key={group.id}
              className={`source-group${activeSource === source.id && activeGroup === group.id ? ' active' : ''}${
                dragOver === group.id ? ' drag-over' : ''
              }`}
              onClick={() => activateSource(source.id, group.id)}
              onDragEnter={(event) => {
                event.preventDefault()
                setDragOver(group.id)
              }}
              onDragLeave={() => setDragOver((g) => (g === group.id ? null : g))}
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
              title={group.id}
            >
              <span className="source-group-icon">{group.kind === 'bucket' ? '◈' : '▸'}</span>
              {group.name}
            </button>
          ))}
        </div>
      ))}
    </div>
  )
}

// MARK: - AI assistant

function AIChatOverlay({ baseUrl, model, onClose }: { baseUrl: string; model: string; onClose: () => void }) {
  const [messages, setMessages] = useState<AIChatMessage[]>([])
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [queue, setQueue] = useState<{ id: number; content: string; model: string }[]>([])
  const [errors, setErrors] = useState<{ id: number; message: string }[]>([])
  const [models, setModels] = useState<AIModel[]>(model ? [{ id: model, model }] : [])
  const [selectedModel, setSelectedModel] = useState(model)
  const [modelsLoading, setModelsLoading] = useState(true)
  const [modelsError, setModelsError] = useState<string | null>(null)
  const [streamedAnswer, setStreamedAnswer] = useState('')
  const endRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const requestId = useRef(0)
  const historyRef = useRef<AIChatMessage[]>([])
  const activeRequestRef = useRef<string | null>(null)

  useEffect(() => {
    let unlisten: (() => void) | undefined
    void onAIStream((streamRequestId, delta) => {
      if (streamRequestId === activeRequestRef.current) setStreamedAnswer((current) => current + delta)
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
  }, [messages, queue, busy, errors, modelsError, streamedAnswer])

  const send = () => {
    const content = draft.trim()
    if (!content || !selectedModel) return
    setQueue((current) => [...current, { id: ++requestId.current, content, model: selectedModel }])
    setDraft('')
    requestAnimationFrame(() => inputRef.current?.focus())
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
    void aiChat(next, request.model, streamRequestId)
      .then((answer) => {
        historyRef.current = [...historyRef.current, { role: 'assistant', content: answer }]
        setMessages(historyRef.current)
      })
      .catch((err) => setErrors((current) => [...current, { id: request.id, message: String(err) }]))
      .finally(() => {
        activeRequestRef.current = null
        setStreamedAnswer('')
        setBusy(false)
        requestAnimationFrame(() => inputRef.current?.focus())
      })
  }, [busy, queue])

  return (
    <section id="ai-chat-panel" className="ai-chat-panel" aria-label="Dramallama assistant">
      <header className="ai-chat-header">
        <div>
          <strong>Dramallama</strong>
          <select
            aria-label="AI model"
            value={selectedModel}
            disabled={busy || modelsLoading || models.length === 0}
            title={modelsError ?? 'Choose the model for this chat'}
            onChange={(event) => setSelectedModel(event.target.value)}
          >
            {models.map((available) => (
              <option key={available.model} value={available.model} title={available.model}>
                {available.id}
              </option>
            ))}
          </select>
        </div>
        <button className="icon-button" onClick={onClose} title="Close AI assistant" aria-label="Close AI assistant">
          ×
        </button>
      </header>
      <div className="ai-chat-messages" aria-live="polite">
        {messages.length === 0 && (
          <div className="ai-chat-empty">
            <strong>Ask about your canvas</strong>
            <p>Dramallama can inspect the open .excalidraw file through the canvas tool when it needs it.</p>
          </div>
        )}
        {messages.map((message, index) => (
          <div key={index} className={`ai-message ${message.role}`}>
            <div className="ai-message-meta">
              <span>{message.role === 'user' ? 'You' : 'AI'}</span>
              {message.role === 'assistant' && (
                <CopyButton text={message.content} className="ai-message-copy" label="AI response" />
              )}
            </div>
            <p>{message.content}</p>
          </div>
        ))}
        {busy && streamedAnswer && (
          <div className="ai-message assistant streaming">
            <div className="ai-message-meta">
              <span>AI</span>
            </div>
            <p>{streamedAnswer}</p>
          </div>
        )}
        {busy && !streamedAnswer && <div className="ai-chat-status">Dramallama is thinking…</div>}
        {queue.map((request, index) => (
          <div key={request.id} className="ai-message user queued">
            <span>Queued {index + 1}</span>
            <div>
              <p>{request.content}</p>
              <button
                className="ai-queue-remove"
                type="button"
                aria-label="Remove queued message"
                title="Remove from queue"
                onClick={() => setQueue((current) => current.filter((item) => item.id !== request.id))}
              >
                ×
              </button>
            </div>
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
        className="ai-chat-form"
        onSubmit={(event) => {
          event.preventDefault()
          send()
        }}
      >
        <textarea
          ref={inputRef}
          value={draft}
          rows={2}
          placeholder={busy ? 'Add another message to the queue…' : 'Ask Dramallama…'}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              send()
            }
          }}
        />
        <button className="primary" type="submit" disabled={!draft.trim() || !selectedModel}>
          {busy || queue.length > 0 ? 'Queue' : 'Send'}
        </button>
      </form>
    </section>
  )
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
    <div className="ai-chat-error" role="alert">
      <span>{message}</span>
      <div className="ai-error-actions">
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

/** Dialog for adding a NATS server (name + URL). Buckets are listed after connecting. */
function AddServerDialog({ onClose }: { onClose: () => void }) {
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const trimmedName = name.trim()
  const trimmedUrl = url.trim()
  const problem = !trimmedName ? 'Enter a name.' : !trimmedUrl ? 'Enter a server URL.' : null

  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    const config: NatsServerConfig = { id: newServerId(), name: trimmedName, url: trimmedUrl, username: username.trim(), password, buckets: [] }
    try {
      await addNatsServer(config)
      onClose()
    } catch (err) {
      log('error', `Could not add the NATS server: ${String(err)}`)
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
          placeholder="Name, e.g. Office server"
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
          onChange={(event) => setUsername(event.target.value)}
        />
        <input
          value={password}
          type="password"
          autoComplete="current-password"
          placeholder="Password (optional)"
          onChange={(event) => setPassword(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape') onClose()
          }}
        />
        <p className={problem ? 'problem' : 'hint'}>{problem ?? 'Boards live in buckets on this server.'}</p>
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

/** Settings: the boards folder, the canvas appearance, and NATS servers. */
function SettingsModal({ snapshot, onClose }: { snapshot: StoreSnapshot; onClose: () => void }) {
  const [serverName, setServerName] = useState('')
  const [serverUrl, setServerUrl] = useState('')
  const [serverUsername, setServerUsername] = useState('')
  const [serverPassword, setServerPassword] = useState('')
  const [bucketFor, setBucketFor] = useState<string | null>(null)
  const [bucketName, setBucketName] = useState('')
  const [aiEnabled, setAIEnabled] = useState(snapshot.ai.enabled)
  const [aiBaseUrl, setAIBaseUrl] = useState(snapshot.ai.baseUrl)
  const [aiModel, setAIModel] = useState(snapshot.ai.model)
  const [aiApiKey, setAIApiKey] = useState('')
  const [clearAIKey, setClearAIKey] = useState(false)
  const [aiModels, setAIModels] = useState<AIModel[]>([])
  const [aiModelsLoading, setAIModelsLoading] = useState(false)
  const [aiSaving, setAISaving] = useState(false)
  const [aiError, setAIError] = useState<string | null>(null)
  const loadedInitialAIModels = useRef(false)

  const natsSources = snapshot.sources.filter((source) => source.kind === 'nats')

  const loadAIModels = useCallback(async () => {
    setAIModelsLoading(true)
    setAIError(null)
    try {
      const models = await discoverAIModels(aiBaseUrl.trim(), aiApiKey, clearAIKey)
      setAIModels(models)
      setAIModel((current) => {
        const match = models.find((item) => item.model === current || item.id === current)
        return match?.model ?? models[0]?.model ?? ''
      })
    } catch (err) {
      setAIModels([])
      setAIError(String(err))
    } finally {
      setAIModelsLoading(false)
    }
  }, [aiApiKey, aiBaseUrl, clearAIKey])

  useEffect(() => {
    if (loadedInitialAIModels.current || !aiEnabled || !aiBaseUrl.trim()) return
    loadedInitialAIModels.current = true
    void loadAIModels()
  }, [aiBaseUrl, aiEnabled, loadAIModels])

  const addServer = async () => {
    const name = serverName.trim()
    const url = serverUrl.trim()
    if (!name || !url) return
    const config: NatsServerConfig = { id: newServerId(), name, url, username: serverUsername.trim(), password: serverPassword, buckets: [] }
    try {
      await addNatsServer(config)
      setServerName('')
      setServerUrl('')
      setServerUsername('')
      setServerPassword('')
    } catch (err) {
      log('error', `Could not add the NATS server: ${String(err)}`)
    }
  }

  const saveAI = async () => {
    setAISaving(true)
    setAIError(null)
    try {
      await setAISettings({
        enabled: aiEnabled,
        baseUrl: aiBaseUrl.trim(),
        model: aiModel.trim(),
        apiKey: aiApiKey,
        clearApiKey: clearAIKey,
      })
      setAIApiKey('')
      setClearAIKey(false)
    } catch (err) {
      setAIError(String(err))
    } finally {
      setAISaving(false)
    }
  }

  const addBucketTo = async (sourceId: string) => {
    const bucket = bucketName.trim()
    if (!bucket) return
    try {
      await addBucket(sourceId, bucket)
      setBucketFor(null)
      setBucketName('')
    } catch (err) {
      log('error', `Could not add the bucket: ${String(err)}`)
    }
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose()
      }}
    >
      <div className="modal settings" role="dialog" aria-modal="true">
        <h2>Settings</h2>
        <section>
          <h3>Boards</h3>
          <div className="settings-row">
            <span className="settings-label">Folder</span>
            <div className="settings-value">
              <span className="folder-path" title={snapshot.folder}>
                {snapshot.folder}
              </span>
              <div className="settings-actions">
                <button className="secondary" onClick={() => revealFolder()}>
                  Show in Finder
                </button>
                <button className="secondary" onClick={() => chooseFolder()}>
                  Change…
                </button>
              </div>
            </div>
          </div>
          <p className="settings-note">
            Boards are plain .excalidraw files. To version them, make the folder a git repository and commit when you like. Avoid folders
            inside Documents, Desktop or Downloads: macOS then won’t let Show in Finder select the file.
          </p>
        </section>
        <section>
          <h3>Application</h3>
          <div className="settings-row">
            <span className="settings-label">Interface zoom</span>
            <div className="settings-value zoom-setting">
              <input
                type="range"
                min="50"
                max="200"
                step="10"
                value={Math.round(snapshot.applicationZoom * 100)}
                aria-label="Application interface zoom"
                onChange={(event) => setApplicationZoom(Number(event.target.value) / 100)}
              />
              <output>{Math.round(snapshot.applicationZoom * 100)}%</output>
              <button className="secondary" disabled={snapshot.applicationZoom === 1} onClick={() => setApplicationZoom(1)}>
                Reset
              </button>
            </div>
          </div>
          <p className="settings-note">Scales the complete application interface. Excalidraw canvas zoom remains independent.</p>
        </section>
        <section>
          <h3>Canvas</h3>
          <div className="settings-row">
            <span className="settings-label">Appearance</span>
            <div className="segmented">
              {(['system', 'light', 'dark'] as const).map((appearance) => (
                <button
                  key={appearance}
                  className={snapshot.canvasAppearance === appearance ? 'active' : ''}
                  onClick={() => setCanvasAppearance(appearance)}
                >
                  {APPEARANCE_TITLES[appearance]}
                </button>
              ))}
            </div>
          </div>
          <p className="settings-note">Use a light canvas even when the app is in dark mode, or the other way around.</p>
        </section>
        <section>
          <h3>NATS Servers</h3>
          {natsSources.length === 0 ? (
            <p className="settings-note">
              No NATS servers yet. Add one to keep boards on a server (in buckets) and collaborate live. Boards stay on the server when you
              remove it here.
            </p>
          ) : (
            natsSources.map((source) => (
              <div key={source.id} className="nats-server">
                <div className="nats-server-head">
                  <span className="nats-server-name">{source.name}</span>
                  <button className="secondary" onClick={() => removeNatsServer(rawServerId(source.id))}>
                    Remove
                  </button>
                </div>
                <div className="nats-buckets">
                  {source.groups.map((group) => (
                    <span key={group.id} className="nats-bucket">
                      {group.name}
                    </span>
                  ))}
                </div>
                {bucketFor === source.id ? (
                  <div className="settings-row">
                    <input
                      value={bucketName}
                      placeholder="Bucket name"
                      onChange={(event) => setBucketName(event.target.value)}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter') addBucketTo(source.id)
                        if (event.key === 'Escape') {
                          setBucketFor(null)
                          setBucketName('')
                        }
                      }}
                    />
                    <button className="primary" onClick={() => addBucketTo(source.id)}>
                      Add
                    </button>
                    <button
                      className="secondary"
                      onClick={() => {
                        setBucketFor(null)
                        setBucketName('')
                      }}
                    >
                      Cancel
                    </button>
                  </div>
                ) : (
                  <button className="secondary" onClick={() => setBucketFor(source.id)}>
                    Add Bucket…
                  </button>
                )}
              </div>
            ))
          )}
          <div className="settings-row nats-add-server">
            <input value={serverName} placeholder="Server name" onChange={(event) => setServerName(event.target.value)} />
            <input
              value={serverUrl}
              placeholder="nats://host:port"
              onChange={(event) => setServerUrl(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') addServer()
              }}
            />
            <input value={serverUsername} placeholder="Username (optional)" onChange={(event) => setServerUsername(event.target.value)} />
            <input
              value={serverPassword}
              type="password"
              placeholder="Password (optional)"
              onChange={(event) => setServerPassword(event.target.value)}
            />
            <button className="primary" onClick={addServer}>
              Add Server
            </button>
          </div>
        </section>
        <section>
          <h3>AI Assistant</h3>
          <label className="settings-row ai-enabled-row">
            <span className="settings-label">Dramallama</span>
            <span className="settings-value ai-toggle-value">
              <input type="checkbox" checked={aiEnabled} onChange={(event) => setAIEnabled(event.target.checked)} />
              Enable the canvas chat overlay
            </span>
          </label>
          <div className="settings-row">
            <span className="settings-label">API URL</span>
            <input
              className="settings-text-input"
              value={aiBaseUrl}
              placeholder="http://localhost:8080 or …/v1"
              onChange={(event) => {
                setAIBaseUrl(event.target.value)
                setAIModels([])
              }}
            />
          </div>
          <div className="settings-row">
            <span className="settings-label">Model</span>
            <div className="settings-value ai-model-value">
              <select
                className="settings-text-input"
                value={aiModels.some((item) => item.model === aiModel) ? aiModel : ''}
                disabled={aiModelsLoading || aiModels.length === 0}
                onChange={(event) => setAIModel(event.target.value)}
              >
                <option value="">{aiModelsLoading ? 'Discovering models…' : 'Choose a discovered model'}</option>
                {aiModels.map((model) => (
                  <option key={model.model} value={model.model} title={model.model}>
                    {model.id}
                  </option>
                ))}
              </select>
              <button className="secondary" disabled={aiModelsLoading || !aiBaseUrl.trim()} onClick={loadAIModels}>
                {aiModelsLoading ? 'Discovering…' : 'Discover Models'}
              </button>
            </div>
          </div>
          <div className="settings-row">
            <span className="settings-label">API key</span>
            <div className="settings-value ai-key-value">
              <input
                className="settings-text-input"
                type="password"
                value={aiApiKey}
                placeholder={snapshot.ai.hasApiKey && !clearAIKey ? 'Saved — enter a new key to replace' : 'Optional'}
                onChange={(event) => {
                  setAIApiKey(event.target.value)
                  setAIModels([])
                  if (event.target.value) setClearAIKey(false)
                }}
              />
              {snapshot.ai.hasApiKey && (
                <label className="clear-key">
                  <input
                    type="checkbox"
                    checked={clearAIKey}
                    onChange={(event) => {
                      setClearAIKey(event.target.checked)
                      setAIModels([])
                    }}
                  />
                  Remove saved key
                </label>
              )}
            </div>
          </div>
          <p className="settings-note">The current .excalidraw file is sent only when Dramallama calls the get_canvas_file tool.</p>
          {aiError && <p className="settings-note problem">{aiError}</p>}
          <div className="settings-ai-actions">
            <button
              className="primary"
              disabled={aiSaving || (aiEnabled && !aiModels.some((item) => item.model === aiModel))}
              onClick={saveAI}
            >
              {aiSaving ? 'Saving…' : 'Save AI Settings'}
            </button>
          </div>
        </section>
        <div className="modal-actions">
          <button className="primary" onClick={onClose}>
            Done
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
        Rename…
      </button>
      <button role="menuitem" onClick={onDuplicate}>
        Duplicate
      </button>
      <button role="menuitem" onClick={onReveal}>
        Show in Finder
      </button>
      <div className="context-menu-divider" />
      <button role="menuitem" className="destructive" onClick={onTrash}>
        Move to Trash
      </button>
    </div>
  )
}
