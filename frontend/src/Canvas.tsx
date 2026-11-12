import {
  CaptureUpdateAction,
  Excalidraw,
  exportToCanvas,
  getCommonBounds,
  getVisibleSceneBounds,
  hashElementsVersion,
  MainMenu,
  reconcileElements,
  restore,
  restoreElements,
  restoreLibraryItems,
  serializeAsJSON,
  serializeLibraryAsJSON,
  WelcomeScreen,
  zoomToFitBounds,
} from '@excalidraw/excalidraw'
import '@excalidraw/excalidraw/index.css'
import type { ImportedDataState } from '@excalidraw/excalidraw/data/types'
import type { ExcalidrawElement, OrderedExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { RemoteExcalidrawElement } from '@excalidraw/excalidraw/data/reconcile'
import type { AppState, BinaryFiles, Collaborator, ExcalidrawImperativeAPI, LibraryItems, SocketId } from '@excalidraw/excalidraw/types'
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import {
  canvasChange,
  canvasLibrary,
  canvasReady,
  canvasView,
  collabAck,
  collabAcquireLock,
  collabGetFiles,
  collabJoin,
  collabLeave,
  collabPublish,
  collabPutFiles,
  collabReleaseLock,
  collabRenewLock,
  collabSend,
  collabSnapshot,
  log,
	  onAICanvasOperations,
  onCanvasOpen,
  onCanvasRename,
  onCanvasSetLibrary,
  onCollab,
  onCollabOutbox,
  onRenderThumbnail,
  onTakePending,
  openLink,
  pendingResult,
  thumbnailResult,
  type CanvasAppearance,
  type Viewport,
} from './backend'
import { FILE_CHOSEN_EVENT } from './shims/browser-fs-access'
import { avatarImageUrl, isAvatarEmoji, peerColor } from './collabIdentity'
import { RemoteCursors, type RemoteCursorData } from './RemoteCursors'
import { useToolbarRoom } from './toolbarRoom'

const SAVE_DEBOUNCE_MS = 300
/**
 * Bound durable drawing updates. Remote reconciliation replaces the complete
 * Excalidraw scene, so pointer-frame rates steal input time on large boards.
 * The latest trailing state is always retained behind an in-flight publish.
 */
const COLLAB_UPDATE_INTERVAL_MS = 150
/** Active drags/resizes publish durable checkpoints less often; live deltas mask the gap. */
const COLLAB_ACTIVE_UPDATE_INTERVAL_MS = 1_000
/** Ephemeral scene deltas sent over Core NATS while an element interaction is active. */
const LIVE_COLLAB_UPDATE_INTERVAL_MS = 100
/** Revert live-only element state if its sender disappears before a durable checkpoint. */
const LIVE_COLLAB_TTL_MS = 3_000
const LIVE_COLLAB_EXPIRY_SWEEP_MS = 500
/** Force a durable checkpoint if an Excalidraw interaction flag gets stuck. */
const LIVE_COLLAB_INTERACTION_TIMEOUT_MS = 1_500
/** Compaction is less frequent than live repair; JetStream retains updates between snapshots. */
const COLLAB_SNAPSHOT_INTERVAL_MS = 60_000
const COLLAB_INITIAL_SNAPSHOT_DELAY_MS = 2_000
/** Cursor presence is best-effort; 10fps is enough while keeping constrained links usable. */
const POINTER_UPDATE_INTERVAL_MS = 100
/** Laser trails need a denser stream than a cursor to look smooth. */
const LASER_UPDATE_INTERVAL_MS = 33
const PRESENCE_TTL_MS = 30_000
/** Leader lease TTL and how often it is renewed/contested while collaborating. */
const LOCK_TTL_SECONDS = 30
const LOCK_RENEW_MS = 10_000
/** Scrolling and zooming are reported less often than edits; only the last position matters. */
const VIEW_DEBOUNCE_MS = 500
const LIBRARY_FILE = /\.excalidrawlib$/i
/** Give up waiting for an import to finish after this long (Excalidraw shows its own error if it fails). */
const IMPORT_TIMEOUT_MS = 60_000
/** Where the library used to be stored (in the web view); migrated to the boards folder on first launch. */
const LEGACY_LIBRARY_KEY = 'whiteboards.library'

/** A NATS board with collaboration active: which source/bucket it lives in and its board id. */
export type CollabInfo = { sourceId: string; bucket: string; boardId: string; documentId: string }
type CollabSession = CollabInfo & { generation: number; subscriberId: string }

/** One anonymous peer per browser session, so several collaborators get distinct cursors. */
const peerId = Math.random().toString(36).slice(2)

type CollabMessage =
  | {
      type: 'scene'
      from: string
      messageId?: string
      elements: readonly ExcalidrawElement[]
      appState?: Partial<AppState>
      appStateVersion?: AppStateVersion
      fileVersions?: Record<string, number>
      /** Immutable ObjectStore blob key for each advertised file version. */
      fileRefs?: Record<string, string>
      /** Compatibility with early v2 messages. */
      fileIds?: string[]
    }
  | { type: 'scene-ref'; from: string; ref: string; messageId?: string }
  /** Best-effort Core NATS preview; never enters the durable log or snapshots. */
  | { type: 'scene-live'; from: string; elements: readonly ExcalidrawElement[] }
  | { type: 'pointer'; peerId: string; nickname?: string; x: number; y: number; tool: 'pointer' | 'laser'; button?: 'up' | 'down'; color?: string; avatar?: string; avatarOnly?: boolean }
  /** The scene rectangle a peer currently sees; lets others follow them. */
  | { type: 'view'; peerId: string; nickname?: string; color?: string; avatar?: string; avatarOnly?: boolean; bounds: Bounds }
  /** `peerId` started or stopped following `target`; re-sent as a heartbeat while following. */
  | { type: 'follow'; peerId: string; nickname?: string; target: string; action: 'FOLLOW' | 'UNFOLLOW' }

type OpenBoard = { name: string; data: ImportedDataState; key: number; library: LibraryItems; snapshotSequence: string }

/**
 * How to place the view once a board has loaded: at its remembered viewport, or zoomed to fit
 * (`hasContent` tells whether to wait for the board's elements to arrive before fitting).
 */
type InitialView = { saved: Viewport | null; hasContent: boolean }

function collaborationDisplayName(remotePeerId: string, nickname?: string): string {
  const cleaned = Array.from(nickname?.trim() ?? '').filter((character) => {
    const code = character.codePointAt(0) ?? 0
    return code > 31 && (code < 127 || code > 159)
  })
  const limited = cleaned.slice(0, 40).join('')
  return limited || `Peer ${remotePeerId.slice(0, 4)}`
}

/** What Excalidraw needs to draw a remote peer: name, cursor color, and (if they have one) avatar. */
function remoteIdentity(
  remotePeerId: string,
  nickname: string | undefined,
  color: unknown,
  avatar: unknown,
): Pick<Collaborator, 'username' | 'color' | 'avatarUrl'> {
  return {
    username: collaborationDisplayName(remotePeerId, nickname),
    color: peerColor(remotePeerId, typeof color === 'string' ? color : undefined),
    avatarUrl: isAvatarEmoji(avatar) ? avatarImageUrl(avatar) : undefined,
  }
}

/** The latest unsaved scene. Serialized only when it's actually sent, not on every change (which can be every mouse move). */
type PendingScene = { name: string; elements: readonly ExcalidrawElement[]; appState: AppState; files: BinaryFiles }
type PendingCollabScene = Pick<PendingScene, 'elements' | 'appState' | 'files'> & { session: CollabSession }
type BufferedCollabEvent = { data: string; sequence: string; delivery: string }
export type PersistenceStatus = 'idle' | 'unwritten' | 'writing' | 'queued' | 'retrying' | 'synced'
/** A sync status worth showing (anything but idle). */
export type SyncStatus = Exclude<PersistenceStatus, 'idle'>
type AppStateVersion = { counter: number; peerId: string }
type CollabSnapshotData = ImportedDataState & { collaboration?: { appStateVersion?: AppStateVersion } }

export const PERSISTENCE_LABELS: Record<SyncStatus, string> = {
  unwritten: 'Unwritten changes',
  writing: 'Saving locally…',
  queued: 'Saved locally · syncing…',
  retrying: 'Saving changes · retrying…',
  synced: 'Synced',
}

const MAX_COLLAB_MESSAGE_BYTES = 700_000

function sequenceMax(a: bigint, value: string): bigint {
  try {
    const b = BigInt(value)
    return b > a ? b : a
  } catch {
    return a
  }
}

function fileVersionSignature(files: BinaryFiles): string {
  return Object.entries(files)
    .map(([id, file]) => `${id}:${file.version ?? 0}`)
    .sort()
    .join('|')
}

function elementVersionKey(element: ExcalidrawElement): string {
  return `${element.version}:${element.versionNonce}`
}

/** Excalidraw conflict precedence: version first, then versionNonce. */
function compareElementVersions(a: ExcalidrawElement, b: ExcalidrawElement): number {
  if (a.version !== b.version) return a.version - b.version
  return a.versionNonce - b.versionNonce
}

function compareElementVersionKeys(a: string, b: string): number {
  const [aVersion, aNonce] = a.split(':').map(Number)
  const [bVersion, bNonce] = b.split(':').map(Number)
  if (aVersion !== bVersion) return aVersion - bVersion
  return aNonce - bNonce
}

function isLiveAppliedElement(live: ReadonlyMap<string, { key: string }>, element: ExcalidrawElement): boolean {
  return live.get(element.id)?.key === elementVersionKey(element)
}

/**
 * Snapshot material must never contain live-only state. Replace a live-applied
 * element with its last durable version, or omit it when no durable version exists.
 */
function materializeDurableElements(
  elements: readonly ExcalidrawElement[],
  live: ReadonlyMap<string, { key: string }>,
  durable: ReadonlyMap<string, ExcalidrawElement>,
): readonly ExcalidrawElement[] {
  const materialized: ExcalidrawElement[] = []
  for (const element of elements) {
    if (!isLiveAppliedElement(live, element)) {
      materialized.push(element)
      continue
    }
    const durableElement = durable.get(element.id)
    if (durableElement) materialized.push(durableElement)
  }
  return materialized
}

/** Whether Excalidraw is actively manipulating elements rather than idling. */
function isLiveInteraction(appState: AppState): boolean {
  return Boolean(
    appState.selectedElementsAreBeingDragged ||
      appState.resizingElement ||
      appState.isResizing ||
      appState.isRotating ||
      appState.newElement ||
      appState.multiElement ||
      appState.editingLinearElement ||
      appState.editingTextElement,
  )
}

/** Fast content fingerprint for immutable ObjectStore blob routing. */
function contentFingerprint(value: string): string {
  let a = 0x811c9dc5
  let b = 0x9e3779b9
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i)
    a = Math.imul(a ^ code, 0x01000193)
    b = Math.imul(b ^ code, 0x85ebca6b)
  }
  return `${(a >>> 0).toString(16).padStart(8, '0')}${(b >>> 0).toString(16).padStart(8, '0')}`
}

/** File blobs are immutable by id, Excalidraw version, and content. */
function collabFileRef(id: string, version: number, dataURL: string): string {
  return `file:${id}:${version}:${contentFingerprint(dataURL)}`
}

function serialize(scene: PendingScene) {
  return { name: scene.name, json: serializeAsJSON(scene.elements, scene.appState, scene.files, 'local') }
}

/** Excalidraw exports intentionally remove deleted elements. Collaboration
 * checkpoints must retain those tombstones until every stale peer has observed
 * them, otherwise compaction can allow deleted shapes to be resurrected. */
function serializeCollabSnapshot(scene: PendingScene, appStateVersion: AppStateVersion) {
  const data = JSON.parse(serializeAsJSON(scene.elements, scene.appState, scene.files, 'local')) as Record<string, unknown>
  data.elements = scene.elements
  data.collaboration = { appStateVersion }
  return JSON.stringify(data)
}

function compareAppStateVersion(a: AppStateVersion, b: AppStateVersion): number {
  return a.counter === b.counter ? a.peerId.localeCompare(b.peerId) : a.counter - b.counter
}

function parseBoard(json: string): ImportedDataState {
  if (!json.trim()) return {}
  return JSON.parse(json) as ImportedDataState
}

function loadLegacyLibrary(): LibraryItems {
  try {
    return JSON.parse(localStorage.getItem(LEGACY_LIBRARY_KEY) ?? '[]') as LibraryItems
  } catch {
    return []
  }
}

/** Library items from an `.excalidrawlib` document (version 2 uses `libraryItems`, version 1 `library`). */
function parseLibrary(json: string): LibraryItems {
  const data = JSON.parse(json) as { libraryItems?: unknown; library?: unknown }
  return restoreLibraryItems((data.libraryItems ?? data.library ?? []) as LibraryItems, 'unpublished')
}

const darkScheme = window.matchMedia('(prefers-color-scheme: dark)')
const subscribeToScheme = (onChange: () => void) => {
  darkScheme.addEventListener('change', onChange)
  return () => darkScheme.removeEventListener('change', onChange)
}

/** Light/dark from the web view's colour scheme (set natively, see CanvasAppearance), subscribed once. */
function useSystemTheme(): 'light' | 'dark' {
  return useSyncExternalStore(subscribeToScheme, () => darkScheme.matches) ? 'dark' : 'light'
}

function currentView(api: ExcalidrawImperativeAPI): Viewport {
  const { scrollX, scrollY, zoom } = api.getAppState()
  return { scrollX, scrollY, zoom: zoom.value }
}

function sameView(a: Viewport | null, b: Viewport) {
  return !!a && Math.abs(a.scrollX - b.scrollX) < 0.5 && Math.abs(a.scrollY - b.scrollY) < 0.5 && Math.abs(a.zoom - b.zoom) < 0.001
}

function isViewport(value: unknown): value is Viewport {
  const v = value as Viewport | null | undefined
  return !!v && [v.scrollX, v.scrollY, v.zoom].every((n) => typeof n === 'number' && Number.isFinite(n)) && v.zoom > 0
}

/** A PNG data URL of the whole board for the sidebar, or null if the board is empty. */
async function renderThumbnail(json: string, maxSize: number): Promise<string | null> {
  const data = restore(parseBoard(json), null, null)
  const elements = data.elements.filter((e) => !e.isDeleted)
  if (elements.length === 0) return null
  const canvas = await exportToCanvas({
    elements,
    appState: { ...data.appState, exportBackground: true, exportWithDarkMode: false },
    files: data.files,
    maxWidthOrHeight: maxSize,
    exportPadding: 16,
  })
  return canvas.toDataURL('image/png')
}

/** Zoom so the whole drawing fits. Excalidraw's own fit rounds down to 10% steps, which is too small for big boards. */
function zoomToFit(api: ExcalidrawImperativeAPI, elements: readonly ExcalidrawElement[]): Viewport {
  const visible = elements.filter((e) => !e.isDeleted)
  if (visible.length === 0) return currentView(api)
  const { width, height } = api.getAppState()
  const [minX, minY, maxX, maxY] = getCommonBounds(visible)
  const padding = 48
  const zoom = Math.min(2, Math.max(0.05, Math.min((width - padding * 2) / (maxX - minX), (height - padding * 2) / (maxY - minY))))
  const view = { scrollX: width / (2 * zoom) - (minX + maxX) / 2, scrollY: height / (2 * zoom) - (minY + maxY) / 2, zoom }
  applyView(api, view)
  return view
}

/** Show the scene rectangle another peer is looking at (Excalidraw's "follow user" behaviour). */
type Bounds = readonly [number, number, number, number]

function applyFollowedBounds(api: ExcalidrawImperativeAPI, bounds: Bounds) {
  const { appState } = zoomToFitBounds({ appState: api.getAppState(), bounds, fitToViewport: true, viewportZoomFactor: 1 })
  api.updateScene({
    appState: { scrollX: appState.scrollX, scrollY: appState.scrollY, zoom: appState.zoom },
    captureUpdate: CaptureUpdateAction.NEVER,
  })
}

function isBounds(value: unknown): value is Bounds {
  return Array.isArray(value) && value.length === 4 && value.every((n) => typeof n === 'number' && Number.isFinite(n))
}

function applyView(api: ExcalidrawImperativeAPI, { scrollX, scrollY, zoom }: Viewport) {
  api.updateScene({ appState: { scrollX, scrollY, zoom: { value: zoom as AppState['zoom']['value'] } } })
}

/**
 * The Excalidraw canvas. Always mounted (like the Swift app's CanvasView): the native backend drives it
 * through `canvas:*` events (Wails) or `window.whiteboards` (Swift) and it reports edits, viewport
 * changes and the library back through the same backend.
 *
 * `showEmptyPlaceholder` is only for the Wails shell: the Swift app has no sidebar, so it shows nothing
 * until a board is opened.
 */
export default function Canvas({
  canvasAppearance,
  showEmptyPlaceholder = true,
  collab = null,
  boardId = null,
  collaborationNickname = '',
  collaborationColor = '',
  collaborationAvatar = '',
  collaborationAvatarOnly = false,
  onSyncStatusHidden,
}: {
  canvasAppearance: CanvasAppearance
  showEmptyPlaceholder?: boolean
  /** When set, the open board is on a NATS source and edits/pointers are shared over NATS. */
  collab?: CollabInfo | null
  /** Wails board identity used to reject late AI edits after a board switch. */
  boardId?: string | null
  /** Name advertised beside this client's cursor during NATS collaboration. */
  collaborationNickname?: string
  /** Cursor color other people see for this client ("#rrggbb"); empty picks one automatically. */
  collaborationColor?: string
  /** Emoji shown beside this client's cursor for other people; may be empty. */
  collaborationAvatar?: string
  /** Show only the avatar (not the name) beside this client's cursor for other people. */
  collaborationAvatarOnly?: boolean
  /**
   * The sync status the canvas has no room to show (it only shows it on large screens, beside the menu
   * button), so the host can show it elsewhere; null when there is none or the canvas shows it.
   */
  onSyncStatusHidden?: (status: SyncStatus | null) => void
}) {
  const [board, setBoard] = useState<OpenBoard | null>(null)
  const [collabRestart, setCollabRestart] = useState(0)
  /** Keep a stale snapshot covered until its retained updates are reconciled. */
  const [collabSynchronizing, setCollabSynchronizing] = useState(false)
  /** Visible durability state for the open board. */
  const [persistenceStatus, setPersistenceStatus] = useState<PersistenceStatus>('idle')
  /** Whether the canvas has room for the sync badge beside its menu button. */
  const [badgeFits, setBadgeFits] = useState(true)
  const boardRef = useRef<OpenBoard | null>(null)
  boardRef.current = board
  const boardKey = board?.key ?? null
  const systemTheme = useSystemTheme()
  const theme: 'light' | 'dark' = canvasAppearance === 'system' ? systemTheme : canvasAppearance === 'dark' ? 'dark' : 'light'
  const collabSourceId = collab?.sourceId ?? null
  const collabBucket = collab?.bucket ?? null
  const collabBoardId = collab?.boardId ?? null
  const collabDocumentId = collab?.documentId ?? null

  const api = useRef<ExcalidrawImperativeAPI | null>(null)
  /** Immutable routing identity for the mounted collaboration session. */
  const collabSession = useRef<CollabSession | null>(null)
  const collabGeneration = useRef(0)
  const collabJoinReady = useRef(false)
  const bufferedCollabEvents = useRef<BufferedCollabEvent[]>([])
  const boardIdRef = useRef(boardId)
  /** Name of the open board, for serializing the reconciled scene on flush. */
  const boardNameRef = useRef('')
  /** Latest reconciled collaboration scene; the lease holder materializes it to JSON. */
  const collabScene = useRef<PendingScene | null>(null)
  const collabDirty = useRef(false)
  /** Monotonic local materialization revision, used to avoid clearing dirtiness after a racing snapshot. */
  const collabRevision = useRef(0)
  /** Snapshots wait until every local durable publish has an acknowledged stream sequence. */
  const collabPublishesPending = useRef(0)
  /** Durable events currently queued or waiting on referenced blobs/files. */
  const collabAppliesPending = useRef(0)
  const collabApplyChain = useRef<Promise<void>>(Promise.resolve())
  /** Backend outbox count: zero is required before the UI may claim Synced. */
  const collabOutboxPending = useRef(0)
  /** Locally durable messages that have not yet been replayed and applied. */
  const awaitingOwnMessages = useRef<Set<string>>(new Set())
  /** Latest local scene waiting for a bounded-rate collaboration publication. */
  const pendingCollab = useRef<PendingCollabScene | null>(null)
  const collabSyncTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const snapshotTimer = useRef<ReturnType<typeof setInterval> | undefined>(undefined)
  const initialSnapshotTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const snapshotInFlight = useRef<number | null>(null)
  const joinRetryTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  /** Highest durable update sequence incorporated into the local scene. */
  const collabSequence = useRef(0n)
  /** Join-time stream watermark required before initial catch-up is complete. */
  const collabReplayThrough = useRef(0n)
  /** Contiguous per-consumer delivery watermark; unlike stream sequence it has no cross-board gaps. */
  const collabDelivery = useRef(0n)
  /** Stream sequence of the latest applied remote background value. */
  const remoteAppStateSequence = useRef(0n)
  const appStateClock = useRef(0)
  const appStateVersion = useRef<AppStateVersion>({ counter: 0, peerId })
  const hasModernAppStateVersion = useRef(false)
  const publishedAppStateVersion = useRef<AppStateVersion>({ counter: -1, peerId: '' })
  const collabMessageCounter = useRef(0)
  /** File versions already stored in ObjectStore for this document. */
  const uploadedFileRefs = useRef<Map<string, string>>(new Map())
  /** Avoid hashing large image data URLs again on every 80ms drawing update. */
  const fileRefCache = useRef<WeakMap<object, { version: number; dataURL: string; ref: string }>>(new WeakMap())
  /** Last element versions acknowledged by JetStream, for lightweight deltas. */
  const publishedElementVersions = useRef<Map<string, string>>(new Map())
  /** Last acknowledged durable element object per id, used for live eligibility and TTL reversion. */
  const durableElementMap = useRef<Map<string, ExcalidrawElement>>(new Map())
  /** Remote live-only element versions currently visible on this canvas. */
  const liveAppliedElements = useRef<Map<string, { key: string; at: number }>>(new Map())
  /** Element versions last sent over the ephemeral live channel. */
  const livePublishedElementVersions = useRef<Map<string, string>>(new Map())
  const liveCollabTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const liveInteractionTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const liveExpiryTimer = useRef<ReturnType<typeof setInterval> | undefined>(undefined)
  const livePublishInFlight = useRef(false)
  const liveInteraction = useRef(false)
  /** At most one normal scene publication runs per session; newer input coalesces behind it. */
  const sessionPublishes = useRef<Map<string, number>>(new Map())
  /** Every local-durability operation for a session, including forced navigation flushes. */
  const sessionDurability = useRef<Map<string, Promise<void>>>(new Map())
  /** Set after scheduleCollabSync is declared, so a completed publish can drain coalesced input. */
  const resumeCollabSync = useRef<() => void>(() => undefined)
  /** Failed publications remain bound to their document and retry independently. */
  const retryTimers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())
  /** Visible collaboration health. Repeated failures reuse one persistent toast. */
  const collabRepairing = useRef(false)
  const collabRetryingPublishes = useRef<Set<string>>(new Set())
  /** Whether this client currently holds the leader lease (the single bucket writer). */
  const isLeader = useRef(false)
  /** Bumped on every lease acquisition/loss; guards a stale flush across a takeover. */
  const leaderEpoch = useRef(0)
  /** Timer for the leader renewal / follower contest loop. */
  const lockTimer = useRef<ReturnType<typeof setInterval> | undefined>(undefined)
  const pending = useRef<PendingScene | null>(null)
  /** Serial local disk writes; failed scenes remain in `pending` for retry/navigation. */
  const localSaveInFlight = useRef<Promise<void>>(Promise.resolve())
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  /** Latest cursor position waiting for the bandwidth-limited presence update. */
  const pendingPointer = useRef<Extract<CollabMessage, { type: 'pointer' }> | null>(null)
  const pointerTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const presenceTimer = useRef<ReturnType<typeof setInterval> | undefined>(undefined)
  const peerLastSeen = useRef<Map<string, number>>(new Map())
  /** Latest visible scene rectangle per peer, applied when one of them is followed. */
  const peerViews = useRef<Map<string, Bounds>>(new Map())
  /** Peers currently following this user (expire without a heartbeat). */
  const followers = useRef<Map<string, { name: string; lastSeen: number }>>(new Map())
  const [followerNames, setFollowerNames] = useState<string[]>([])
  const pendingBounds = useRef<Bounds | null>(null)
  const lastSentBounds = useRef('')
  const nicknameRef = useRef('')
  nicknameRef.current = collaborationNickname.trim()
  const colorRef = useRef('')
  colorRef.current = collaborationColor
  const avatarRef = useRef('')
  avatarRef.current = collaborationAvatar
  const avatarOnlyRef = useRef(false)
  avatarOnlyRef.current = collaborationAvatarOnly
  /** Peers with an avatar are drawn by the avatar overlay instead of Excalidraw's own cursor. */
  const peerLooks = useRef<Map<string, { emoji: string; avatarOnly: boolean }>>(new Map())
  const rememberPeerLook = (remotePeerId: string, avatar: unknown, avatarOnly: unknown) => {
    if (isAvatarEmoji(avatar)) peerLooks.current.set(remotePeerId, { emoji: avatar, avatarOnly: avatarOnly === true })
    else peerLooks.current.delete(remotePeerId)
  }
  /** Setter of the remote cursor overlay, and a cheap key to skip no-op updates. */
  const cursorOverlay = useRef<((items: RemoteCursorData[]) => void) | null>(null)
  const cursorOverlayKey = useRef('')
  const registerCursorOverlay = useCallback((update: ((items: RemoteCursorData[]) => void) | null) => {
    cursorOverlay.current = update
    cursorOverlayKey.current = ''
  }, [])
  const lastVersion = useRef<number | null>(null)
  const lastFileSignature = useRef('')
  const lastBackground = useRef<AppState['viewBackgroundColor'] | undefined>(undefined)
  /** Set when a board is opened, until its initial view (remembered or fitted) has been applied. */
  const initialView = useRef<InitialView | null>(null)
  /** Viewport changes are only reported once the initial view is in place. */
  const trackingView = useRef(false)
  const lastView = useRef<Viewport | null>(null)
  const pendingView = useRef<({ name: string } & Viewport) | null>(null)
  const viewTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const library = useRef<LibraryItems>([])
  /** Last library JSON received from or sent to Rust; avoids writing back what was just loaded. */
  const lastLibraryJson = useRef<string | null>(null)
  const libraryLoaded = useRef(false)
  /** A library file is being imported (dropped or opened): number of items before, for the "Added N" message. */
  const importing = useRef<{ before: number; timer: ReturnType<typeof setTimeout> } | null>(null)

  useEffect(() => {
    boardIdRef.current = boardId
  }, [boardId])

  const showCollabRepairing = useCallback((message = 'Reconnecting collaboration…') => {
    collabRepairing.current = true
    api.current?.setToast({ message, duration: Infinity, closable: false })
  }, [])

  const finishCollabRepair = useCallback(() => {
    if (!collabRepairing.current) return
    collabRepairing.current = false
    if (collabRetryingPublishes.current.size > 0) {
      api.current?.setToast({ message: 'Changes pending — retrying…', duration: Infinity, closable: false })
      return
    }
    api.current?.setToast({ message: 'Collaboration restored', duration: 3000, closable: true })
  }, [])

  const requestCollabReplay = useCallback((message: string) => {
    showCollabRepairing(message)
    setCollabSynchronizing(true)
    collabJoinReady.current = false
    // Back off persistent corruption/missing-blob failures instead of spinning
    // a tight join/replay loop. Repeated failures coalesce onto this timer.
    if (joinRetryTimer.current) return
    joinRetryTimer.current = setTimeout(() => {
      joinRetryTimer.current = undefined
      setCollabRestart((value) => value + 1)
    }, 1_000)
  }, [showCollabRepairing])

  // Do not leave a reconnecting toast behind after the user switches to a
  // local board. A durable publication from the previous board may still be
  // retrying, in which case that status remains globally relevant.
  useEffect(() => {
    if (collabDocumentId) return
    setCollabSynchronizing(false)
    if (!collabRepairing.current) return
    collabRepairing.current = false
    if (collabRetryingPublishes.current.size > 0) {
      api.current?.setToast({ message: 'Changes pending — retrying…', duration: Infinity, closable: false })
    } else {
      api.current?.setToast(null)
    }
  }, [collabDocumentId])

  // Show progress while a library file is imported; Excalidraw itself gives no feedback until it's done.
  useEffect(() => {
    const start = (names: string[]) => {
      if (!names.some((name) => LIBRARY_FILE.test(name))) return
      if (importing.current) clearTimeout(importing.current.timer)
      importing.current = {
        before: library.current.length,
        timer: setTimeout(() => {
          importing.current = null
          api.current?.setToast(null)
        }, IMPORT_TIMEOUT_MS),
      }
      api.current?.setToast({ message: 'Importing library…', duration: Infinity })
    }
    const onChosen = (event: Event) => start((event as CustomEvent<string[]>).detail)
    const onDrop = (event: DragEvent) => start(Array.from(event.dataTransfer?.files ?? []).map((file) => file.name))
    window.addEventListener(FILE_CHOSEN_EVENT, onChosen)
    document.addEventListener('drop', onDrop, { capture: true })
    return () => {
      window.removeEventListener(FILE_CHOSEN_EVENT, onChosen)
      document.removeEventListener('drop', onDrop, { capture: true })
    }
  }, [])

  const flushView = useCallback(() => {
    clearTimeout(viewTimer.current)
    const view = pendingView.current
    pendingView.current = null
    if (view) {
      const { name, ...viewport } = view
      canvasView(name, viewport)
    }
  }, [])

  /** Store large files first, then append bounded changed-element batches to JetStream. */
  const publishCollabScene = useCallback(
    (session: CollabSession, elements: readonly ExcalidrawElement[], appState: AppState, files: BinaryFiles, errorContext: string, full = false) => {
      const sessionPublishKey = `${session.documentId}:${session.generation}`
      sessionPublishes.current.set(sessionPublishKey, (sessionPublishes.current.get(sessionPublishKey) ?? 0) + 1)
      const changedFiles: BinaryFiles = {}
      const fileRefs: Record<string, string> = {}
      for (const [id, file] of Object.entries(files)) {
        const version = file.version ?? 0
        const cached = fileRefCache.current.get(file)
        const ref = cached?.version === version && cached.dataURL === file.dataURL
          ? cached.ref
          : collabFileRef(id, version, file.dataURL)
        if (cached?.ref !== ref) fileRefCache.current.set(file, { version, dataURL: file.dataURL, ref })
        fileRefs[id] = ref
        if (uploadedFileRefs.current.get(id) !== ref) changedFiles[ref] = file
      }
      const changedElements = elements.filter((element) => {
        // A receiver must never persist another peer's live-only preview.
        if (isLiveAppliedElement(liveAppliedElements.current, element)) return false
        return full || publishedElementVersions.current.get(element.id) !== elementVersionKey(element)
      })
      const batches: ExcalidrawElement[][] = []
      let batch: ExcalidrawElement[] = []
      const encoder = new TextEncoder()
      const emptyMessageBytes = encoder.encode(JSON.stringify({ type: 'scene', from: peerId, elements: [] })).byteLength
      let batchBytes = emptyMessageBytes
      for (const element of changedElements) {
        // Encode each element once. Re-stringifying the growing candidate array
        // made full-scene sync O(n²) and froze large drawings on the UI thread.
        const elementBytes = encoder.encode(JSON.stringify(element)).byteLength + (batch.length > 0 ? 1 : 0)
        if (batch.length > 0 && batchBytes + elementBytes > MAX_COLLAB_MESSAGE_BYTES) {
          batches.push(batch)
          batch = [element]
          batchBytes = emptyMessageBytes + elementBytes
        } else {
          batch.push(element)
          batchBytes += elementBytes
        }
      }
      if (batch.length > 0) batches.push(batch)
      if (batches.length === 0) batches.push([])
      // Reuse the same IDs across retries so JetStream deduplicates a batch
      // whose publish acknowledgement was lost after the server stored it.
      const messageIds = batches.map(() => `${peerId}:${++collabMessageCounter.current}`)
      const advertisedAppStateVersion = { ...appStateVersion.current }
      const advertiseAppState = full || compareAppStateVersion(advertisedAppStateVersion, publishedAppStateVersion.current) > 0
      if (advertiseAppState) hasModernAppStateVersion.current = true

      // Each logical publication retries independently. A session-wide key lets
      // a later successful update cancel an earlier failed update and strands
      // its Promise (which also blocks snapshots forever).
      const retryKey = `${session.documentId}:${session.generation}:${messageIds[0]}`
      if (collabSession.current?.generation === session.generation) collabPublishesPending.current++
      if (collabSession.current?.generation === session.generation) setPersistenceStatus('writing')
      const run = async () => {
        if (Object.keys(changedFiles).length > 0) {
          await collabPutFiles(session.sourceId, session.bucket, session.documentId, JSON.stringify(changedFiles))
          if (collabSession.current?.generation === session.generation) {
            for (const [id, ref] of Object.entries(fileRefs)) uploadedFileRefs.current.set(id, ref)
          }
        }
        for (let i = 0; i < batches.length; i++) {
          const message: CollabMessage = {
            type: 'scene',
            from: peerId,
            messageId: messageIds[i],
            elements: batches[i],
            appState: i === 0 && advertiseAppState ? { viewBackgroundColor: appState.viewBackgroundColor } : undefined,
            appStateVersion: i === 0 && advertiseAppState ? advertisedAppStateVersion : undefined,
            fileVersions: i === 0
              ? Object.fromEntries(Object.entries(files).map(([id, file]) => [id, file.version ?? 0]))
              : undefined,
            fileRefs: i === 0 ? fileRefs : undefined,
          }
          let encoded = JSON.stringify(message)
          if (new TextEncoder().encode(encoded).byteLength > MAX_COLLAB_MESSAGE_BYTES) {
            // A single large freehand/metadata element cannot be split without
            // changing Excalidraw semantics. Store the complete wire message as
            // an immutable blob and publish a small durable reference instead.
            const ref = `scene:${messageIds[i]}`
            await collabPutFiles(session.sourceId, session.bucket, session.documentId, JSON.stringify({ [ref]: message }))
            encoded = JSON.stringify({ type: 'scene-ref', from: peerId, ref, messageId: messageIds[i] } satisfies CollabMessage)
          }
          // A publish acknowledgement does not mean this client has consumed
          // every earlier stream entry. Snapshot progress advances only in the
          // ordered subscription callback below, after each scene is applied.
          awaitingOwnMessages.current.add(messageIds[i])
          await collabPublish(session.sourceId, session.bucket, session.documentId, messageIds[i], encoded)
        }
        if (collabSession.current?.generation === session.generation) {
          for (const element of changedElements) {
            publishedElementVersions.current.set(element.id, elementVersionKey(element))
            // Forced final deltas can run beside an older checkpoint. Never let
            // the older publication move the durable baseline backwards.
            const durable = durableElementMap.current.get(element.id)
            if (!durable || compareElementVersions(element, durable) >= 0) {
              durableElementMap.current.set(element.id, element)
            }
          }
          if (advertiseAppState && compareAppStateVersion(advertisedAppStateVersion, publishedAppStateVersion.current) > 0) {
            publishedAppStateVersion.current = advertisedAppStateVersion
          }
        }
        const existing = retryTimers.current.get(retryKey)
        if (existing) clearTimeout(existing)
        retryTimers.current.delete(retryKey)
        const wasRetrying = collabRetryingPublishes.current.delete(retryKey)
        if (collabSession.current?.generation === session.generation) setPersistenceStatus('queued')
        if (wasRetrying && collabRetryingPublishes.current.size === 0 && !collabRepairing.current) {
          api.current?.setToast({ message: 'Collaboration changes saved', duration: 3000, closable: true })
        }
      }
      const attempt = (): Promise<void> => run().catch((err: unknown) => {
        log('error', `${errorContext}: ${String(err)}; retrying`)
        collabRetryingPublishes.current.add(retryKey)
        if (collabSession.current?.generation === session.generation) setPersistenceStatus('retrying')
        if (!collabRepairing.current) {
          api.current?.setToast({ message: 'Changes pending — retrying…', duration: Infinity, closable: false })
        }
        return new Promise<void>((resolve) => {
          const existing = retryTimers.current.get(retryKey)
          if (existing) clearTimeout(existing)
          retryTimers.current.set(retryKey, setTimeout(() => {
            retryTimers.current.delete(retryKey)
            attempt().then(resolve)
          }, 1_000))
        })
      })
      const publication = attempt()
        .finally(() => {
          const remaining = Math.max(0, (sessionPublishes.current.get(sessionPublishKey) ?? 1) - 1)
          if (remaining === 0) {
            sessionPublishes.current.delete(sessionPublishKey)
            // Changes that arrived while this publish was in flight were kept
            // as one latest scene. Drain them now instead of spawning parallel
            // retries for every 80ms input sample.
            resumeCollabSync.current()
          } else {
            sessionPublishes.current.set(sessionPublishKey, remaining)
          }
          if (collabSession.current?.generation === session.generation) {
            collabPublishesPending.current = Math.max(0, collabPublishesPending.current - 1)
          }
        })
      // Forced navigation can start a final full scene beside an older publish.
      // Track both so the backend does not change boards until every update has
      // reached the durable local outbox.
      const previous = sessionDurability.current.get(sessionPublishKey) ?? Promise.resolve()
      const durability = Promise.all([previous, publication]).then(() => undefined)
      sessionDurability.current.set(sessionPublishKey, durability)
      // Use both branches instead of dropping the Promise returned by `finally`:
      // a terminal failure must reach the navigation barrier without also
      // becoming an unrelated unhandled rejection in the renderer.
      const clearDurability = () => {
        if (sessionDurability.current.get(sessionPublishKey) === durability) {
          sessionDurability.current.delete(sessionPublishKey)
        }
      }
      durability.then(clearDurability, clearDurability)
      return publication
    },
    [],
  )

  /** Publish the latest local scene at a bounded rate. Excalidraw elements carry
   * their own version/versionNonce conflict metadata and are reconciled by peers. */
  const flushPendingCollab = useCallback((force = false, full = false) => {
    clearTimeout(collabSyncTimer.current)
    collabSyncTimer.current = undefined
    const scene = pendingCollab.current
    // The pending scene is already an immutable onChange capture. Do not depend
    // on the imperative API still being mounted: during teardown its ref may be
    // cleared before the final durability barrier runs.
    if (!scene) return null
    const sessionPublishKey = `${scene.session.documentId}:${scene.session.generation}`
    if (!force && (sessionPublishes.current.get(sessionPublishKey) ?? 0) > 0) {
      // Keep replacing this with the newest onChange scene while one update is
      // in flight. Its completion will call resumeCollabSync and publish once.
      return null
    }
    pendingCollab.current = null
    const durableElements = materializeDurableElements(scene.elements, liveAppliedElements.current, durableElementMap.current)
    collabScene.current = {
      name: boardNameRef.current,
      elements: durableElements,
      appState: scene.appState,
      files: scene.files,
    }
    collabDirty.current = true
    collabRevision.current++
    // A forced navigation flush is a self-contained repair scene. It may run
    // beside an older retry, so include every element and let Excalidraw's
    // element versions deterministically keep the newest state on any arrival
    // order. A full scene uses the durable materialization so a live-only
    // preview can never replace its committed version.
    return publishCollabScene(
      scene.session,
      full ? durableElements : scene.elements,
      scene.appState,
      scene.files,
      'Could not publish a collaboration update',
      full,
    )
  }, [publishCollabScene])

  const sendFollow = useCallback((target: string, action: 'FOLLOW' | 'UNFOLLOW') => {
    const session = collabSession.current
    if (!session) return
    const message: CollabMessage = { type: 'follow', peerId, nickname: nicknameRef.current || undefined, target, action }
    collabSend(session.sourceId, session.bucket, session.documentId, JSON.stringify(message))
  }, [])

  /** Send only the newest cursor position in each bandwidth window. */
  const flushPointer = useCallback(() => {
    clearTimeout(pointerTimer.current)
    pointerTimer.current = undefined
    const message = pendingPointer.current
    pendingPointer.current = null
    const bounds = pendingBounds.current
    pendingBounds.current = null
    const target = collabSession.current
    if (!target) return
    if (bounds) {
      const view: CollabMessage = { type: 'view', peerId, nickname: nicknameRef.current || undefined, color: colorRef.current || undefined, avatar: avatarRef.current || undefined, avatarOnly: avatarRef.current ? avatarOnlyRef.current || undefined : undefined, bounds }
      collabSend(target.sourceId, target.bucket, target.documentId, JSON.stringify(view))
    }
    if (message) collabSend(target.sourceId, target.bucket, target.documentId, JSON.stringify(message))
  }, [])

  // Let everyone see a changed name, color or avatar right away instead of at the next heartbeat.
  useEffect(() => {
    const instance = api.current
    if (!collabSession.current || !collabJoinReady.current || !instance) return
    pendingBounds.current = getVisibleSceneBounds(instance.getAppState())
    flushPointer()
  }, [collaborationNickname, collaborationColor, collaborationAvatar, collaborationAvatarOnly, flushPointer])

  /**
   * Send changed elements over Core NATS without touching the durable outbox.
   * Restricted to elements that already have a durable baseline so a crashed
   * sender can always be rolled back to known state by receivers.
   */
  const flushLiveCollab = useCallback(() => {
    const target = collabSession.current
    if (!target || !collabJoinReady.current || livePublishInFlight.current) return
    const elements = pendingCollab.current?.elements ?? api.current?.getSceneElementsIncludingDeleted()
    if (!elements) return
    const changed = elements.filter((element) => {
      if (!durableElementMap.current.has(element.id)) return false
      if (isLiveAppliedElement(liveAppliedElements.current, element)) return false
      const key = elementVersionKey(element)
      // Already durable state needs no preview; only changes newer than the
      // last durable version are live-only.
      if (publishedElementVersions.current.get(element.id) === key) return false
      return livePublishedElementVersions.current.get(element.id) !== key
    })
    if (changed.length === 0) return
    const message: CollabMessage = { type: 'scene-live', from: peerId, elements: changed }
    const encoded = JSON.stringify(message)
    if (new TextEncoder().encode(encoded).byteLength > MAX_COLLAB_MESSAGE_BYTES) {
      // Do not rebuild the same oversized batch every interval. A later element
      // edit changes its version and becomes eligible again.
      for (const element of changed) livePublishedElementVersions.current.set(element.id, elementVersionKey(element))
      return
    }
    const sent = collabSend(target.sourceId, target.bucket, target.documentId, encoded)
    if (!sent) return
    livePublishInFlight.current = true
    void sent
      .then(() => {
        if (collabSession.current?.generation !== target.generation) return
        for (const element of changed) livePublishedElementVersions.current.set(element.id, elementVersionKey(element))
      })
      .catch((err: unknown) => log('error', `Could not send a live collaboration update: ${String(err)}`))
      .finally(() => {
        livePublishInFlight.current = false
      })
  }, [])

  const scheduleLiveCollab = useCallback(() => {
    if (liveCollabTimer.current) return
    liveCollabTimer.current = setTimeout(() => {
      liveCollabTimer.current = undefined
      flushLiveCollab()
    }, LIVE_COLLAB_UPDATE_INTERVAL_MS)
  }, [flushLiveCollab])

  const savePendingLocal = useCallback((): Promise<void> => {
    const operation = localSaveInFlight.current.then(async () => {
      while (!collabSession.current) {
        const scene = pending.current
        if (!scene) return
        const { name, json } = serialize(scene)
        await Promise.resolve(canvasChange(name, json))
        // Never clear a newer edit that arrived while this disk write ran.
        if (pending.current === scene) pending.current = null
      }
    })
    // Keep the serialization chain usable after an error, while returning the
    // real failure to the caller that initiated this save.
    localSaveInFlight.current = operation.catch(() => undefined)
    return operation
  }, [])

  const flush = useCallback(() => {
    clearTimeout(timer.current)
    flushPendingCollab()
    // Collaboration is persisted by durable updates plus immutable snapshots.
    // Local boards retain the ordinary debounced JSON write path.
    if (!collabSession.current) {
      void savePendingLocal().catch((err: unknown) => log('error', `Could not save canvas changes: ${String(err)}`))
    }
    flushView()
    return true
  }, [flushPendingCollab, flushView, savePendingLocal])

  /**
   * Batch rapid Excalidraw changes into one latest-scene publication per
   * interval. Local rendering remains immediate.
   */
  const scheduleCollabSync = useCallback(() => {
    if (collabSyncTimer.current) return
    collabSyncTimer.current = setTimeout(() => {
      collabSyncTimer.current = undefined
      if (!flushPendingCollab()) return
      clearTimeout(timer.current)
      timer.current = setTimeout(flush, SAVE_DEBOUNCE_MS)
    }, liveInteraction.current ? COLLAB_ACTIVE_UPDATE_INTERVAL_MS : COLLAB_UPDATE_INTERVAL_MS)
  }, [flush, flushPendingCollab])
  resumeCollabSync.current = scheduleCollabSync

  /**
   * Excalidraw normally clears interaction flags on pointer-up. If a flag gets
   * stuck, force a durable checkpoint so the crash-loss window stays bounded.
   */
  const scheduleLiveInteractionTimeout = useCallback(() => {
    clearTimeout(liveInteractionTimer.current)
    liveInteractionTimer.current = setTimeout(() => {
      liveInteractionTimer.current = undefined
      if (!liveInteraction.current) return
      liveInteraction.current = false
      clearTimeout(liveCollabTimer.current)
      liveCollabTimer.current = undefined
      flushPendingCollab(true)
    }, LIVE_COLLAB_INTERACTION_TIMEOUT_MS)
  }, [flushPendingCollab])

  /**
   * Reconcile a remote scene using Excalidraw's own collaboration semantics.
   * Remote updates never enter the local undo stack.
   */
  const applyRemoteScene = useCallback(async (message: Extract<CollabMessage, { type: 'scene' }>, sequence = '0'): Promise<void> => {
    const instance = api.current
    if (!instance) throw new Error('canvas is not ready')
    if (!Array.isArray(message.elements)) throw new Error('invalid collaboration scene')
    // Publish a local drag waiting in the coalescing window before merging, so
    // peers can independently reconcile both versioned states.
    flushPendingCollab()
    const local = instance.getSceneElementsIncludingDeleted()
    // restoreElements returns ordered elements; reconciliation's remote brand is
    // compile-time-only in Excalidraw, so mark the restored wire scene as remote.
    const restored = restoreElements(message.elements, local) as unknown as readonly RemoteExcalidrawElement[]
    const reconciled = reconcileElements(
      local as readonly OrderedExcalidrawElement[],
      restored,
      instance.getAppState(),
    )
    // Track the newest durable version independently of any live preview that
    // concurrently won reconciliation on the canvas. A durable checkpoint that
    // matches the live key must still replace the older durable baseline.
    const reconciledById = new Map(reconciled.map((element) => [element.id, element]))
    for (const element of restored) {
      const existing = durableElementMap.current.get(element.id)
      if (!existing || compareElementVersions(element, existing) >= 0) {
        durableElementMap.current.set(element.id, element)
      }
      // Only mark a remote durable version as published when it is the exact
      // object reflected in the scene. A distinct local edit that happens to
      // share a version:nonce key must remain publishable, so never bump the
      // map from observed remote state alone.
      if ((reconciledById.get(element.id) as unknown) !== (element as unknown)) continue
      const key = elementVersionKey(element)
      const publishedKey = publishedElementVersions.current.get(element.id)
      if (!publishedKey || compareElementVersionKeys(key, publishedKey) >= 0) {
        publishedElementVersions.current.set(element.id, key)
      }
    }
    const files = instance.getFiles()
    const currentAppState = instance.getAppState()
    const messageSequence = sequenceMax(0n, sequence)
    const advertisedBackground = message.appState?.viewBackgroundColor
    const advertisedVersion = message.appStateVersion
    let remoteBackground: AppState['viewBackgroundColor'] | undefined
    if (
      advertisedBackground !== undefined &&
      advertisedVersion &&
      Number.isSafeInteger(advertisedVersion.counter) &&
      advertisedVersion.counter >= 0 &&
      typeof advertisedVersion.peerId === 'string'
    ) {
      hasModernAppStateVersion.current = true
      appStateClock.current = Math.max(appStateClock.current, advertisedVersion.counter)
      if (compareAppStateVersion(advertisedVersion, appStateVersion.current) >= 0) {
        appStateVersion.current = advertisedVersion
        remoteBackground = advertisedBackground
      }
    } else if (
      advertisedBackground !== undefined &&
      !hasModernAppStateVersion.current &&
      messageSequence >= remoteAppStateSequence.current
    ) {
      // Compatibility for updates written before explicit app-state clocks.
      remoteAppStateSequence.current = messageSequence
      remoteBackground = advertisedBackground
    }
    const reconciledAppState = remoteBackground !== undefined
      ? ({ ...currentAppState, viewBackgroundColor: remoteBackground } as AppState)
      : currentAppState
    // Set before updateScene: its onChange is synchronous in parts of Excalidraw.
    lastVersion.current = hashElementsVersion(reconciled)
    lastFileSignature.current = fileVersionSignature(files)
    lastBackground.current = reconciledAppState.viewBackgroundColor
    collabScene.current = {
      name: boardNameRef.current,
      elements: materializeDurableElements(reconciled, liveAppliedElements.current, durableElementMap.current),
      appState: reconciledAppState,
      files,
    }
    collabDirty.current = true
    collabRevision.current++
    // Our own updates come back through the log, usually while the local scene is
    // already ahead of them (e.g. mid-drag). A NEVER update re-baselines
    // Excalidraw's history snapshot to the current scene, which would swallow the
    // undo entry for the move in progress. Only touch the scene if it changed.
    const sceneChanged =
      remoteBackground !== undefined ||
      reconciled.length !== local.length ||
      reconciled.some((element, i) =>
        element.id !== local[i].id || element.version !== local[i].version || element.versionNonce !== local[i].versionNonce,
      )
    if (sceneChanged) {
      instance.updateScene({
        elements: reconciled,
        appState: remoteBackground !== undefined ? ({ viewBackgroundColor: remoteBackground } as AppState) : undefined,
        captureUpdate: CaptureUpdateAction.NEVER,
      })
    }
    const target = collabSession.current
    const remoteFileVersions = message.fileVersions ?? Object.fromEntries((message.fileIds ?? []).map((id) => [id, 0]))
    const missingFiles = Object.entries(remoteFileVersions)
      .filter(([id, version]) => !files[id] || (files[id].version ?? 0) < version)
      .map(([id]) => ({ id, ref: message.fileRefs?.[id] ?? id }))
    if (target && missingFiles.length > 0) {
      collabPublishesPending.current++
      try {
        const json = await collabGetFiles(target.sourceId, target.bucket, target.documentId, missingFiles.map(({ ref }) => ref))
          if (collabSession.current?.generation !== target.generation) return
          const blobs = JSON.parse(json) as BinaryFiles
          const received = Object.fromEntries(
            missingFiles.flatMap(({ id, ref }) => blobs[ref] ? [[id, blobs[ref]]] : []),
          ) as BinaryFiles
          if (Object.keys(received).length !== missingFiles.length) {
            throw new Error('one or more collaboration file blobs are unavailable')
          }
          api.current?.addFiles(Object.values(received))
          for (const [id, file] of Object.entries(received)) {
            uploadedFileRefs.current.set(id, collabFileRef(id, file.version ?? 0, file.dataURL))
          }
          if (collabScene.current) {
            collabScene.current = { ...collabScene.current, files: { ...collabScene.current.files, ...received } }
            collabDirty.current = true
            collabRevision.current++
          }
      } catch (err) {
        if (collabSession.current?.generation === target.generation) {
          log('error', `Could not load collaboration files: ${String(err)}; replaying the document`)
          requestCollabReplay('Reconnecting collaboration — retrieving shared files…')
        }
        throw err
      } finally {
        if (collabSession.current?.generation === target.generation) {
          collabPublishesPending.current = Math.max(0, collabPublishesPending.current - 1)
        }
      }
    }
    clearTimeout(timer.current)
    timer.current = setTimeout(flush, SAVE_DEBOUNCE_MS)
  }, [flush, flushPendingCollab, requestCollabReplay])

  /**
   * Apply an ephemeral live delta. This deliberately never touches durable
   * bookkeeping: no snapshot baseline, dirty flag, file fetch, or local save.
   */
  const applyLiveScene = useCallback((message: Extract<CollabMessage, { type: 'scene-live' }>): void => {
    const instance = api.current
    if (!instance) return
    // Only elements with a durable baseline may be previewed. This guarantees a
    // receiver always has a version to revert to if the sender dies.
    const incoming = message.elements.filter((element) => durableElementMap.current.has(element.id))
    if (incoming.length === 0) return
    const local = instance.getSceneElementsIncludingDeleted()
    const restored = restoreElements(incoming, local) as unknown as readonly RemoteExcalidrawElement[]
    const reconciled = reconcileElements(
      local as readonly OrderedExcalidrawElement[],
      restored,
      instance.getAppState(),
    )
    const files = instance.getFiles()
    const currentAppState = instance.getAppState()
    lastVersion.current = hashElementsVersion(reconciled)
    lastFileSignature.current = fileVersionSignature(files)
    lastBackground.current = currentAppState.viewBackgroundColor
    instance.updateScene({ elements: reconciled, captureUpdate: CaptureUpdateAction.NEVER })
    const reconciledById = new Map(reconciled.map((element) => [element.id, element]))
    const restoredById = new Map(restored.map((element) => [element.id, element]))
    const now = Date.now()
    for (const element of incoming) {
      const current = reconciledById.get(element.id)
      const incomingKey = elementVersionKey(element)
      const restoredElement = restoredById.get(element.id)
      // restoreElements can normalize some fields and bump versionNonce. Track
      // the version actually reconciled onto the canvas, not only the wire one.
      const appliedKey = restoredElement ? elementVersionKey(restoredElement) : incomingKey
      const entry = liveAppliedElements.current.get(element.id)
      if (current && elementVersionKey(current) === appliedKey) {
        // Refresh each id independently so long drags never expire mid-motion.
        liveAppliedElements.current.set(element.id, { key: appliedKey, at: now })
      } else if (entry && (entry.key === incomingKey || entry.key === appliedKey)) {
        liveAppliedElements.current.delete(element.id)
      }
    }
  }, [])

  const sendFullScene = useCallback(() => {
    const session = collabSession.current
    const instance = api.current
    if (!session || !instance) return
    const appState = instance.getAppState()
    const elements = instance.getSceneElementsIncludingDeleted()
    const files = instance.getFiles()
    const durableElements = materializeDurableElements(elements, liveAppliedElements.current, durableElementMap.current)
    collabScene.current = {
      name: boardNameRef.current,
      elements: durableElements,
      appState,
      files,
    }
    collabDirty.current = true
    collabRevision.current++
    publishCollabScene(session, durableElements, appState, files, 'Could not publish a full collaboration scene', true)
  }, [publishCollabScene])

  /** Compact the reconciled scene into an immutable sequence-addressed snapshot. */
  const writeSnapshot = useCallback(() => {
    const target = collabSession.current
    const scene = collabScene.current
    const sequence = collabSequence.current
    if (
      !target ||
      !scene ||
      !isLeader.current ||
      !collabDirty.current ||
      !collabJoinReady.current ||
      collabRepairing.current ||
      sequence === 0n ||
      collabPublishesPending.current > 0 ||
      collabAppliesPending.current > 0 ||
      snapshotInFlight.current === target.generation
    ) return
    const revision = collabRevision.current
    snapshotInFlight.current = target.generation
    const json = serializeCollabSnapshot(scene, appStateVersion.current)
    collabSnapshot(target.sourceId, target.bucket, target.boardId, target.documentId, sequence.toString(), json, target.subscriberId)
      .then(() => {
        if (collabRevision.current === revision) collabDirty.current = false
      })
      .catch((err: unknown) => log('error', `Could not write a collaboration snapshot: ${String(err)}`))
      .finally(() => {
        if (snapshotInFlight.current === target.generation) snapshotInFlight.current = null
      })
  }, [])

  /** Roll back live-only state whose sender vanished before a durable checkpoint. */
  const expireLiveAppliedElements = useCallback(() => {
    const live = liveAppliedElements.current
    if (live.size === 0) return
    const instance = api.current
    if (!instance) {
      live.clear()
      return
    }
    const now = Date.now()
    const elements = instance.getSceneElementsIncludingDeleted()
    const currentById = new Map(elements.map((element) => [element.id, element]))
    const replacements = new Map<string, ExcalidrawElement>()
    for (const [id, entry] of live) {
      const current = currentById.get(id)
      if (!current || elementVersionKey(current) !== entry.key) {
        // A local edit or a durable update already superseded the preview.
        live.delete(id)
        continue
      }
      const durable = durableElementMap.current.get(id)
      if (!durable || elementVersionKey(durable) === entry.key) {
        live.delete(id)
        continue
      }
      if (now - entry.at < LIVE_COLLAB_TTL_MS) continue
      replacements.set(id, durable)
      live.delete(id)
    }
    if (replacements.size === 0) return
    const next = elements.map((element) => replacements.get(element.id) ?? element)
    lastVersion.current = hashElementsVersion(next)
    instance.updateScene({ elements: next, captureUpdate: CaptureUpdateAction.NEVER })
  }, [])

  const teardownCollab = useCallback(() => {
    // A normal bounded-rate flush coalesces behind an in-flight publish. During
    // navigation there will be no next timer to drain that trailing scene, so
    // force it into its own immutable-session publication before clearing the
    // refs. The backend outbox makes the initiated publish durable and its retry
    // remains routed to the board being left.
    flushPendingCollab(true, true)
    const session = collabSession.current
    collabSession.current = null
    collabJoinReady.current = false
    bufferedCollabEvents.current = []
    clearTimeout(collabSyncTimer.current)
    collabSyncTimer.current = undefined
    pendingCollab.current = null
    clearInterval(snapshotTimer.current)
    snapshotTimer.current = undefined
    clearTimeout(initialSnapshotTimer.current)
    initialSnapshotTimer.current = undefined
    snapshotInFlight.current = null
    clearTimeout(joinRetryTimer.current)
    joinRetryTimer.current = undefined
    clearTimeout(pointerTimer.current)
    pointerTimer.current = undefined
    pendingPointer.current = null
    pendingBounds.current = null
    lastSentBounds.current = ''
    clearInterval(presenceTimer.current)
    presenceTimer.current = undefined
    clearInterval(liveExpiryTimer.current)
    liveExpiryTimer.current = undefined
    clearTimeout(liveCollabTimer.current)
    liveCollabTimer.current = undefined
    clearTimeout(liveInteractionTimer.current)
    liveInteractionTimer.current = undefined
    liveInteraction.current = false
    livePublishInFlight.current = false
    liveAppliedElements.current.clear()
    livePublishedElementVersions.current.clear()
    durableElementMap.current.clear()
    peerLastSeen.current.clear()
    peerViews.current.clear()
    peerLooks.current.clear()
    followers.current.clear()
    setFollowerNames([])
    collabScene.current = null
    collabDirty.current = false
    collabRevision.current = 0
    collabPublishesPending.current = 0
    collabAppliesPending.current = 0
    collabApplyChain.current = Promise.resolve()
    collabOutboxPending.current = 0
    awaitingOwnMessages.current.clear()
    uploadedFileRefs.current.clear()
    publishedElementVersions.current.clear()
    collabSequence.current = 0n
    collabReplayThrough.current = 0n
    collabDelivery.current = 0n
    remoteAppStateSequence.current = 0n
    appStateClock.current = 0
    appStateVersion.current = { counter: 0, peerId }
    hasModernAppStateVersion.current = false
    publishedAppStateVersion.current = { counter: -1, peerId: '' }
    // Stop the election loop and drop the leader lease (only if we still own it).
    clearInterval(lockTimer.current)
    lockTimer.current = undefined
    if (isLeader.current) {
      isLeader.current = false
      leaderEpoch.current++
      if (session) {
        collabReleaseLock(session.sourceId, session.bucket, session.documentId, session.subscriberId).catch((err: unknown) =>
          log('error', `Could not release the collaboration lock: ${String(err)}`),
        )
      }
    }
    if (session) {
      collabLeave(session.sourceId, session.bucket, session.documentId, session.subscriberId).catch((err: unknown) =>
        log('error', `Could not leave collaboration: ${String(err)}`),
      )
    }
  }, [flushPendingCollab])

  /** Stop being the leader: stop renewing and stop writing the bucket. */
  const loseLeadership = useCallback(() => {
    if (!isLeader.current) return
    isLeader.current = false
    leaderEpoch.current++
    // Drop any queued flush; it must not write the bucket as a follower.
    clearTimeout(timer.current)
  }, [])

  /** Become the compaction leader; live publishing never depends on the lease. */
  const becomeLeader = useCallback(() => {
    if (isLeader.current) return
    isLeader.current = true
    leaderEpoch.current++
    if (collabDirty.current) {
      setTimeout(writeSnapshot, 0)
    }
  }, [writeSnapshot])

  /**
   * One tick of the leader election loop: the leader renews its lease (stepping
   * down if it lost it), followers try to take over an expired lease.
   */
  const lockLoop = useCallback(() => {
    const target = collabSession.current
    if (!target) return
    if (isLeader.current) {
      collabRenewLock(target.sourceId, target.bucket, target.documentId, target.subscriberId, LOCK_TTL_SECONDS)
        .then((ok) => {
          if (!ok) loseLeadership()
        })
        .catch(() => loseLeadership())
    } else {
      collabAcquireLock(target.sourceId, target.bucket, target.documentId, target.subscriberId, LOCK_TTL_SECONDS)
        .then((won) => {
          if (won) becomeLeader()
        })
        .catch((err: unknown) => log('error', `Could not acquire the collaboration lock: ${String(err)}`))
    }
  }, [becomeLeader, loseLeadership])

  /** Resolve a wire scene that was too large for one NATS message. */
  const applySceneReference = useCallback((session: CollabSession, ref: string, sequence: string): Promise<void> => {
    collabPublishesPending.current++
    return collabGetFiles(session.sourceId, session.bucket, session.documentId, [ref])
      .then((json) => {
        if (collabSession.current?.generation !== session.generation) return
        const blobs = JSON.parse(json) as Record<string, unknown>
        const message = blobs[ref]
        if (!message || typeof message !== 'object' || !('type' in message) || message.type !== 'scene') {
          throw new Error('invalid referenced collaboration scene')
        }
        return applyRemoteScene(message as Extract<CollabMessage, { type: 'scene' }>, sequence)
      })
      .catch((err: unknown) => {
        if (collabSession.current?.generation !== session.generation) return
        log('error', `Could not load a referenced collaboration scene: ${String(err)}; replaying the document`)
        requestCollabReplay('Reconnecting collaboration — retrieving a large update…')
        throw err
      })
      .finally(() => {
        if (collabSession.current?.generation === session.generation) {
          collabPublishesPending.current = Math.max(0, collabPublishesPending.current - 1)
        }
      })
  }, [applyRemoteScene, requestCollabReplay])

  const acknowledgeCollabEvent = useCallback((session: CollabSession, event: BufferedCollabEvent) => {
    if (event.sequence === '0') return Promise.resolve()
    return collabAck(session.sourceId, session.bucket, session.documentId, session.subscriberId, event.sequence)
  }, [])

  const markCollabSyncedIfComplete = useCallback((session: CollabSession) => {
    const key = `${session.documentId}:${session.generation}`
    if (
      collabSession.current?.generation === session.generation &&
      collabJoinReady.current &&
      collabSequence.current >= collabReplayThrough.current &&
      collabAppliesPending.current === 0
    ) {
      setCollabSynchronizing(false)
      finishCollabRepair()
    }
    if (
      collabSession.current?.generation === session.generation &&
      collabJoinReady.current &&
      !collabRepairing.current &&
      collabOutboxPending.current === 0 &&
      awaitingOwnMessages.current.size === 0 &&
      collabSequence.current >= collabReplayThrough.current &&
      collabAppliesPending.current === 0 &&
      !pendingCollab.current &&
      (sessionPublishes.current.get(key) ?? 0) === 0 &&
      collabRetryingPublishes.current.size === 0
    ) setPersistenceStatus('synced')
  }, [finishCollabRepair])

  /** Apply one event after the join snapshot has established the replay baseline. */
  const handleCollabEvent = useCallback(
    async (session: CollabSession, event: BufferedCollabEvent): Promise<void> => {
      if (collabSession.current?.generation !== session.generation || !collabJoinReady.current) return
      let delivery = 0n
      if (event.sequence !== '0') {
        try {
          delivery = BigInt(event.delivery)
        } catch {
          delivery = 0n
        }
        if (delivery <= collabDelivery.current) {
          await acknowledgeCollabEvent(session, event)
          return
        }
        if (delivery !== collabDelivery.current + 1n) {
          // Durable messages are ACKed only below, after this renderer validates
          // them. A delivery gap still forces a fresh snapshot-based replay;
          // never advance a compaction watermark across it.
          log('error', 'Collaboration delivery gap detected; replaying the document')
          requestCollabReplay('Reconnecting collaboration — repairing a missed update…')
          return
        }
      }
      let parsed: unknown
      try {
        parsed = JSON.parse(event.data)
      } catch {
        if (event.sequence !== '0') {
          log('error', 'Invalid durable collaboration event; replaying the document')
          requestCollabReplay('Reconnecting collaboration — repairing an invalid update…')
        }
        return
      }
      if (!parsed || typeof parsed !== 'object' || !(('type' in parsed))) {
        if (event.sequence !== '0') {
          log('error', 'Unknown durable collaboration event; replaying the document')
          requestCollabReplay('Reconnecting collaboration — repairing an unknown update…')
        }
        return
      }
      const message = parsed as CollabMessage
      // Never advance the compaction watermark across an event we did not
      // understand and apply. Presence is Core NATS (`sequence === "0"`), while
      // every durable event must be a valid scene or scene reference.
      const validDurable =
        (message.type === 'scene' && Array.isArray(message.elements) && typeof message.from === 'string') ||
        (message.type === 'scene-ref' && typeof message.from === 'string' && typeof message.ref === 'string')
      if (event.sequence !== '0' && !validDurable) {
        log('error', 'Unsupported durable collaboration event; replaying the document')
        requestCollabReplay('Reconnecting collaboration — repairing an unsupported update…')
        return
      }
      if (message.type === 'scene') {
        if (!Array.isArray(message.elements) || typeof message.from !== 'string') return
        try {
          // Do not skip our own retained events. They are already visible during
          // the live session, but after reopening the board its baseline snapshot
          // may predate them. Reconciliation is version-based and idempotent.
          await applyRemoteScene(message, event.sequence)
        } catch (err) {
          log('error', `Could not reconcile a remote scene: ${String(err)}; replaying the document`)
          requestCollabReplay('Reconnecting collaboration — reconciling shared changes…')
          return
        }
      } else if (message.type === 'scene-ref') {
        if (typeof message.from !== 'string' || typeof message.ref !== 'string') return
        // ACK only after the referenced scene is fetched and reconciled. This
        // keeps a missing ObjectStore blob replayable instead of falsely marking
        // the update as synchronized.
        try {
          await applySceneReference(session, message.ref, event.sequence)
        } catch {
          return
        }
      } else if (message.type === 'scene-live') {
        if (event.sequence !== '0' || typeof message.from !== 'string' || !Array.isArray(message.elements)) return
        if (message.from === peerId) return
        try {
          applyLiveScene(message)
        } catch (err) {
          log('error', `Could not apply a live collaboration preview: ${String(err)}`)
        }
        return
      } else if (message.type === 'follow') {
        if (
          typeof message.peerId !== 'string' ||
          message.peerId === peerId ||
          message.target !== peerId ||
          (message.action !== 'FOLLOW' && message.action !== 'UNFOLLOW')
        ) return
        if (message.action === 'FOLLOW') {
          const known = followers.current.has(message.peerId)
          followers.current.set(message.peerId, {
            name: collaborationDisplayName(message.peerId, message.nickname),
            lastSeen: Date.now(),
          })
          if (!known) setFollowerNames([...followers.current.values()].map((f) => f.name))
        } else if (followers.current.delete(message.peerId)) {
          setFollowerNames([...followers.current.values()].map((f) => f.name))
        }
        return
      } else if (message.type === 'view') {
        if (typeof message.peerId !== 'string' || message.peerId === peerId || !isBounds(message.bounds)) return
        peerLastSeen.current.set(message.peerId, Date.now())
        peerViews.current.set(message.peerId, message.bounds)
        rememberPeerLook(message.peerId, message.avatar, message.avatarOnly)
        const instance = api.current
        if (!instance) return
        const collaborators = new Map(instance.getAppState().collaborators)
        const existing = collaborators.get(message.peerId as SocketId)
        collaborators.set(message.peerId as SocketId, {
          ...existing,
          pointer: existing?.pointer && { ...existing.pointer, renderCursor: !peerLooks.current.has(message.peerId) },
          ...remoteIdentity(message.peerId, message.nickname, message.color, message.avatar),
        })
        instance.updateScene({ collaborators, captureUpdate: CaptureUpdateAction.NEVER })
        if (instance.getAppState().userToFollow?.socketId === message.peerId) applyFollowedBounds(instance, message.bounds)
        return
      } else if (
        message.type !== 'pointer' ||
        typeof message.peerId !== 'string' ||
        typeof message.x !== 'number' ||
        typeof message.y !== 'number' ||
        (message.tool !== 'pointer' && message.tool !== 'laser')
      ) {
        return
      } else {
        if (message.peerId === peerId) return
        peerLastSeen.current.set(message.peerId, Date.now())
        rememberPeerLook(message.peerId, message.avatar, message.avatarOnly)
        const collaborators = new Map(api.current?.getAppState().collaborators ?? [])
        collaborators.set(message.peerId as SocketId, {
          ...collaborators.get(message.peerId as SocketId),
          pointer: { x: message.x, y: message.y, tool: message.tool, renderCursor: !peerLooks.current.has(message.peerId) },
          button: message.button === 'down' ? 'down' : 'up',
          ...remoteIdentity(message.peerId, message.nickname, message.color, message.avatar),
        })
        api.current?.updateScene({ collaborators })
        return
      }
      if (message.from === peerId && message.messageId) {
        awaitingOwnMessages.current.delete(message.messageId)
      }
      // This is the contiguous-applied commit point. Referenced scenes and all
      // file dependencies have completed; only now may compaction cover it.
      if (collabSession.current?.generation !== session.generation || !collabJoinReady.current) return
      collabDelivery.current = delivery
      collabSequence.current = sequenceMax(collabSequence.current, event.sequence)
      try {
        await acknowledgeCollabEvent(session, event)
      } catch (err) {
        log('error', `Could not acknowledge collaboration update: ${String(err)}`)
      }
    },
    [acknowledgeCollabEvent, applyLiveScene, applyRemoteScene, applySceneReference, requestCollabReplay],
  )

  const enqueueCollabEvent = useCallback((session: CollabSession, event: BufferedCollabEvent): Promise<void> => {
    if (event.sequence === '0') return handleCollabEvent(session, event)
    collabAppliesPending.current++
    const operation = collabApplyChain.current.then(() => handleCollabEvent(session, event))
    const tracked = operation.finally(() => {
      if (collabSession.current?.generation === session.generation) {
        collabAppliesPending.current = Math.max(0, collabAppliesPending.current - 1)
        markCollabSyncedIfComplete(session)
      }
    })
    collabApplyChain.current = tracked.catch(() => undefined)
    return tracked
  }, [handleCollabEvent, markCollabSyncedIfComplete])

  useEffect(() => {
    let key = 0
    const unlistens: Promise<() => void>[] = [
      onCanvasOpen(({ name, json, view, snapshotSequence }) => {
        // Commit the final coalesced state for the board being left.
        if (collabScene.current || pending.current) flush()
        pending.current = null
        try {
          const data = parseBoard(json)
          setBoard({ name, data, key: ++key, library: library.current, snapshotSequence: snapshotSequence ?? '0' })
          setPersistenceStatus('idle')
          boardNameRef.current = name
          lastVersion.current = null
          lastBackground.current = data.appState?.viewBackgroundColor
          trackingView.current = false
          lastView.current = null
          initialView.current = {
            saved: isViewport(view) ? view : null,
            hasContent: (data.elements ?? []).some((e) => !e.isDeleted),
          }
          teardownCollab()
          // The route-driven join effect owns collaboration session creation.
          // Creating one here from a ref can briefly attach the previous remote
          // route to a newly opened local board and send local input remotely.
        } catch (err) {
          log('error', `Could not read ${name}: ${String(err)}`)
        }
      }),
      onCanvasRename((name) => {
        setBoard((current) => (current ? { ...current, name } : current))
        boardNameRef.current = name
        if (collabScene.current) collabScene.current = { ...collabScene.current, name }
        if (pending.current) pending.current = { ...pending.current, name }
        if (pendingView.current) pendingView.current = { ...pendingView.current, name }
      }),
      onCanvasSetLibrary((json) => {
        let items: LibraryItems = []
        try {
          if (json) {
            items = parseLibrary(json)
          } else {
            // No library file yet: move the library that used to live in the web view's storage.
            items = loadLegacyLibrary()
            if (items.length > 0) canvasLibrary(serializeLibraryAsJSON(items))
          }
          localStorage.removeItem(LEGACY_LIBRARY_KEY)
        } catch (err) {
          log('error', `Could not read the library: ${String(err)}`)
          return
        }
        library.current = items
        lastLibraryJson.current = serializeLibraryAsJSON(items)
        libraryLoaded.current = true
        api.current?.updateLibrary({ libraryItems: items, merge: false })
      }),
      onRenderThumbnail((requestId, json, maxSize) => {
        renderThumbnail(json, maxSize)
          .then((dataUrl) => thumbnailResult(requestId, dataUrl))
          .catch((err) => {
            log('error', `Could not render a thumbnail: ${String(err)}`)
            thumbnailResult(requestId, null)
          })
      }),
      onTakePending((requestId) => {
        clearTimeout(timer.current)
        const session = collabSession.current
        const publication = flushPendingCollab(true, true)
        flushView()
        // Durable collaboration updates/snapshots own remote-board persistence;
        // returning a full JSON scene here would restore the old multi-writer
        // race. Do not acknowledge navigation until all current publications
        // have reached the backend's durable local outbox.
        if (session) {
          const key = `${session.documentId}:${session.generation}`
          const durability = sessionDurability.current.get(key) ?? publication ?? Promise.resolve()
          setPersistenceStatus('writing')
          durability
            .then(() => {
              if (collabSession.current?.generation === session.generation) setPersistenceStatus('queued')
              return pendingResult(requestId, null, null, null)
            })
            .catch((err: unknown) => {
              if (collabSession.current?.generation === session.generation) setPersistenceStatus('retrying')
              return pendingResult(requestId, null, null, String(err))
            })
          return
        }
        // Wait for any debounced disk write already in flight. If it failed,
        // `pending` still contains the exact scene and is returned to the
        // backend navigation/quit barrier for a synchronous retry. Do not clear
        // it here; onCanvasOpen clears it only after navigation succeeds.
        localSaveInFlight.current.then(() => {
          const scene = pending.current
          if (scene) {
            const { name, json } = serialize(scene)
            return pendingResult(requestId, name, json, null)
          }
          return pendingResult(requestId, null, null, null)
        }).catch((err: unknown) => pendingResult(requestId, null, null, String(err)))
      }),
	      onAICanvasOperations((targetBoardId, json) => {
	        if (targetBoardId !== boardIdRef.current) return
	        const instance = api.current
	        if (!instance) return
	        try {
	          const restored = restore(parseBoard(json), instance.getAppState(), instance.getSceneElements(), {
	            refreshDimensions: true,
	            repairBindings: true,
	          })
	          instance.updateScene({ elements: restored.elements, captureUpdate: CaptureUpdateAction.IMMEDIATELY })
	          instance.setToast({ message: 'Merlin updated the canvas — use Undo to revert', duration: 5000, closable: true })
	        } catch (err) {
	          log('error', `Could not apply Merlin canvas operations: ${String(err)}`)
	          instance.setToast({ message: 'Could not apply Merlin’s canvas update', duration: 5000, closable: true })
	        }
	      }),
    ]
    Promise.resolve(canvasReady()).catch((err: unknown) => log('error', `Could not open the selected board: ${String(err)}`))
    window.addEventListener('pagehide', flush)
    return () => {
      window.removeEventListener('pagehide', flush)
      flush()
      flushPointer()
      teardownCollab()
      unlistens.forEach((p) => p.then((unlisten) => unlisten()))
    }
  }, [flush, flushPendingCollab, flushPointer, flushView, teardownCollab])

  // Join the durable per-document log, apply its snapshot, publish a repair scene,
  // and elect one client to compact updates into immutable snapshots.
  useEffect(() => {
    const currentBoard = boardRef.current
    if (!currentBoard || !collabSourceId || !collabBucket || !collabBoardId || !collabDocumentId) return
    setCollabSynchronizing(true)
    let session = collabSession.current
    if (
      !session ||
      session.sourceId !== collabSourceId ||
      session.bucket !== collabBucket ||
      session.boardId !== collabBoardId ||
      session.documentId !== collabDocumentId
    ) {
      // The effect owns its session. This is required for rename and protects
      // against a parent rerender/route transition after onCanvasOpen fired.
      teardownCollab()
      const generation = ++collabGeneration.current
      session = {
        sourceId: collabSourceId,
        bucket: collabBucket,
        boardId: collabBoardId,
        documentId: collabDocumentId,
        generation,
        subscriberId: `${peerId}:${generation}`,
      }
      collabSession.current = session
      const instance = api.current
      const initialScene = instance
        ? {
            name: boardNameRef.current,
            elements: instance.getSceneElementsIncludingDeleted(),
            appState: instance.getAppState(),
            files: instance.getFiles(),
          }
        : {
            name: currentBoard.name,
            elements: currentBoard.data.elements ?? [],
            appState: (currentBoard.data.appState ?? {}) as AppState,
            files: currentBoard.data.files ?? {},
          }
      collabScene.current = initialScene
      durableElementMap.current = new Map(initialScene.elements.map((element) => [element.id, element]))
    }
    let cancelled = false
    collabJoinReady.current = false
    bufferedCollabEvents.current = []
    collabJoin(session.sourceId, session.bucket, session.documentId, session.subscriberId, currentBoard.snapshotSequence)
      .then(async (result) => {
        if (cancelled || collabSession.current?.generation !== session.generation) {
          // Cleanup may have run before the asynchronous backend join created
          // its subscriptions. Leave again after completion to avoid a leak.
          collabLeave(session.sourceId, session.bucket, session.documentId, session.subscriberId).catch(() => undefined)
          return
        }
        collabOutboxPending.current = result.pendingLocalUpdates ?? 0
        collabReplayThrough.current = sequenceMax(0n, result.replayThroughSequence ?? result.snapshotSequence)
        if (result.snapshot) {
          try {
            const data = parseBoard(result.snapshot) as CollabSnapshotData
            const snapshotFiles = data.files ?? {}
            if (Object.keys(snapshotFiles).length > 0) {
              api.current?.addFiles(Object.values(snapshotFiles))
              for (const [id, file] of Object.entries(snapshotFiles)) {
                uploadedFileRefs.current.set(id, collabFileRef(id, file.version ?? 0, file.dataURL))
              }
            }
            const message: CollabMessage = {
              type: 'scene',
              from: 'snapshot',
              elements: data.elements ?? [],
              appState: data.appState ?? undefined,
              appStateVersion: data.collaboration?.appStateVersion,
              fileVersions: Object.fromEntries(Object.entries(snapshotFiles).map(([id, file]) => [id, file.version ?? 0])),
            }
            await applyRemoteScene(message, result.snapshotSequence)
          } catch (err) {
            log('error', `Could not apply the collaboration snapshot: ${String(err)}`)
            requestCollabReplay('Reconnecting collaboration — repairing the saved scene…')
            return
          }
        }
        collabSequence.current = sequenceMax(collabSequence.current, result.snapshotSequence)
        // Events can arrive as soon as the backend subscribes, before the join
        // result reaches us. Apply the snapshot first, then replay those events
        // in exact stream order. Presence (sequence 0) is harmless in this list.
        const buffered = bufferedCollabEvents.current
        bufferedCollabEvents.current = []
        buffered.sort((a, b) => {
          const aa = sequenceMax(0n, a.delivery)
          const bb = sequenceMax(0n, b.delivery)
          return aa < bb ? -1 : aa > bb ? 1 : 0
        })
        collabJoinReady.current = true
        await Promise.all(buffered.map((event) => enqueueCollabEvent(session, event)))
        // A buffered event may have exposed a delivery gap or invalid payload
        // and already requested a fresh replay. Do not start a second set of
        // timers or report recovery for the abandoned session.
        if (!collabJoinReady.current) return
        markCollabSyncedIfComplete(session)
        // Only seed a genuinely new collaboration log. Existing documents are
        // recovered from snapshots plus retained updates; republishing the whole
        // scene on every join wastes bandwidth and reconciliation time.
        if (collabReplayThrough.current === 0n && collabSequence.current === 0n) sendFullScene()
        snapshotTimer.current = setInterval(writeSnapshot, COLLAB_SNAPSHOT_INTERVAL_MS)
        if (api.current) {
          pendingBounds.current = getVisibleSceneBounds(api.current.getAppState())
          flushPointer()
        }
        presenceTimer.current = setInterval(() => {
          const instance = api.current
          if (!instance) return
          // Heartbeat: keeps this peer listed (and followable) while idle.
          const own = getVisibleSceneBounds(instance.getAppState())
          lastSentBounds.current = own.join(',')
          pendingBounds.current = own
          flushPointer()
          const cutoff = Date.now() - PRESENCE_TTL_MS
          const followed = instance.getAppState().userToFollow
          if (followed) sendFollow(followed.socketId, 'FOLLOW')
          let followersChanged = false
          for (const [followerId, follower] of followers.current) {
            if (follower.lastSeen >= cutoff) continue
            followers.current.delete(followerId)
            followersChanged = true
          }
          if (followersChanged) setFollowerNames([...followers.current.values()].map((f) => f.name))
          const collaborators = new Map(instance.getAppState().collaborators)
          let changed = false
          for (const [remotePeerId, lastSeen] of peerLastSeen.current) {
            if (lastSeen >= cutoff) continue
            peerLastSeen.current.delete(remotePeerId)
            peerLooks.current.delete(remotePeerId)
            changed = collaborators.delete(remotePeerId as SocketId) || changed
          }
          if (changed) instance.updateScene({ collaborators })
        }, PRESENCE_TTL_MS / 3)
        clearInterval(liveExpiryTimer.current)
        liveExpiryTimer.current = setInterval(expireLiveAppliedElements, LIVE_COLLAB_EXPIRY_SWEEP_MS)
        lockLoop()
        lockTimer.current = setInterval(lockLoop, LOCK_RENEW_MS)
        initialSnapshotTimer.current = setTimeout(writeSnapshot, COLLAB_INITIAL_SNAPSHOT_DELAY_MS)
      })
      .catch((err: unknown) => {
        if (cancelled || collabSession.current?.generation !== session.generation) return
        setCollabSynchronizing(true)
        log('error', `Could not join collaboration: ${String(err)}; retrying`)
        showCollabRepairing('Collaboration unavailable — retrying…')
        joinRetryTimer.current = setTimeout(() => setCollabRestart((value) => value + 1), 2_000)
      })
    return () => {
      cancelled = true
      if (collabSession.current?.generation === session.generation) {
        flushPendingCollab()
        teardownCollab()
      }
    }
  }, [
    applyRemoteScene,
    boardKey,
    collabSourceId,
    collabBucket,
    collabBoardId,
    collabDocumentId,
    collabRestart,
    expireLiveAppliedElements,
    flushPendingCollab,
    finishCollabRepair,
    enqueueCollabEvent,
    lockLoop,
    markCollabSyncedIfComplete,
    sendFollow,
    sendFullScene,
    showCollabRepairing,
    teardownCollab,
    writeSnapshot,
    requestCollabReplay,
  ])

  // Reconcile durable Excalidraw scenes and show ephemeral cursors.
  useEffect(() => {
    let unlisten: (() => void) | undefined
    let cancelled = false
    onCollab((documentId, subscriberId, data, sequence, delivery) => {
      const session = collabSession.current
      if (!session || documentId !== session.documentId || subscriberId !== session.subscriberId) return
      const event = { data, sequence, delivery }
      if (!collabJoinReady.current) {
        bufferedCollabEvents.current.push(event)
        return
      }
      void enqueueCollabEvent(session, event)
    }).then((u) => {
      if (cancelled) u()
      else unlisten = u
    })
    return () => {
      cancelled = true
      unlisten?.()
    }
  }, [enqueueCollabEvent])

  useEffect(() => {
    let unlisten: (() => void) | undefined
    let cancelled = false
    onCollabOutbox((documentId, count) => {
      const session = collabSession.current
      if (!session || session.documentId !== documentId) return
      collabOutboxPending.current = Math.max(0, count)
      if (count > 0) {
        setPersistenceStatus('queued')
      } else {
        markCollabSyncedIfComplete(session)
      }
    }).then((u) => {
      if (cancelled) u()
      else unlisten = u
    })
    return () => {
      cancelled = true
      unlisten?.()
    }
  }, [markCollabSyncedIfComplete])

  /** Place the cursor of each peer with an avatar, in canvas pixels. */
  const updateRemoteCursors = (appState: AppState) => {
    const update = cursorOverlay.current
    if (!update) return
    const items: RemoteCursorData[] = []
    for (const [id, collaborator] of appState.collaborators) {
      const look = peerLooks.current.get(id)
      if (!look || !collaborator.pointer) continue
      const x = Math.round((collaborator.pointer.x + appState.scrollX) * appState.zoom.value)
      const y = Math.round((collaborator.pointer.y + appState.scrollY) * appState.zoom.value)
      if (x < 0 || y < 0 || x > appState.width || y > appState.height) continue
      items.push({
        id,
        x,
        y,
        emoji: look.emoji,
        name: look.avatarOnly ? '' : (collaborator.username ?? ''),
        stroke: collaborator.color?.stroke ?? '#868e96',
        background: collaborator.color?.background ?? '#e9ecef',
        down: collaborator.button === 'down',
      })
    }
    const key = items.map((item) => `${item.id}|${item.x}|${item.y}|${item.emoji}|${item.name}|${item.stroke}|${item.down}`).join(';')
    if (key === cursorOverlayKey.current) return
    cursorOverlayKey.current = key
    update(items)
  }

  const onChange = (elements: readonly ExcalidrawElement[], appState: AppState, files: BinaryFiles) => {
    if (!board) return
    updateRemoteCursors(appState)
    // Place the view once the board has loaded: where the user left it, or zoomed to fit.
    const initial = initialView.current
    if (initial && api.current && (!initial.hasContent || elements.length > 0)) {
      initialView.current = null
      const instance = api.current
      // After Excalidraw has finished applying the loaded scene (a timeout, not an animation frame:
      // those don't run while the window is hidden).
      setTimeout(() => {
        if (initial.saved) applyView(instance, initial.saved)
        // getAppState() only reflects updateScene after the next render, so remember the view that was set.
        lastView.current = initial.saved ?? zoomToFit(instance, elements)
        trackingView.current = true
      })
    }
    // Remember scrolling and zooming (reported to Rust, debounced; never written into the board file).
    if (collabSession.current) {
      const bounds = getVisibleSceneBounds(appState)
      const key = bounds.join(',')
      if (key !== lastSentBounds.current) {
        lastSentBounds.current = key
        pendingBounds.current = bounds
        if (!pointerTimer.current) pointerTimer.current = setTimeout(flushPointer, POINTER_UPDATE_INTERVAL_MS)
      }
    }
    // A view borrowed from the followed user is not the user's own place on the board.
    if (trackingView.current && !appState.userToFollow) {
      const view: Viewport = { scrollX: appState.scrollX, scrollY: appState.scrollY, zoom: appState.zoom.value }
      if (!sameView(lastView.current, view)) {
        lastView.current = view
        pendingView.current = { name: board.name, ...view }
        clearTimeout(viewTimer.current)
        viewTimer.current = setTimeout(flushView, VIEW_DEBOUNCE_MS)
      }
    }
    // onChange also fires for scrolling and selection. Persist only drawing,
    // file, or board-background changes.
    const version = hashElementsVersion(elements)
    const fileSignature = fileVersionSignature(files)
    const background = appState.viewBackgroundColor
    if (lastVersion.current === null) {
      lastVersion.current = version
      lastFileSignature.current = fileSignature
      lastBackground.current = background
      return
    }
    if (
      version === lastVersion.current &&
      fileSignature === lastFileSignature.current &&
      background === lastBackground.current
    ) return
    const backgroundChanged = background !== lastBackground.current
    lastVersion.current = version
    lastFileSignature.current = fileSignature
    lastBackground.current = background
    const session = collabSession.current
    if (session) {
      if (backgroundChanged) {
        const counter = Math.max(appStateClock.current, appStateVersion.current.counter) + 1
        appStateClock.current = counter
        appStateVersion.current = { counter, peerId }
        hasModernAppStateVersion.current = true
      }
      // Keep only the newest drag/resize snapshot and publish it at a bounded
      // rate. Peers reconcile Excalidraw's element versions and tombstones.
      setPersistenceStatus('unwritten')
      pendingCollab.current = { session, elements, appState, files }
      if (isLiveInteraction(appState)) {
        liveInteraction.current = true
        scheduleLiveInteractionTimeout()
        scheduleLiveCollab()
        scheduleCollabSync()
      } else if (liveInteraction.current) {
        // Pointer-up/gesture end: send one last preview, then commit the final
        // delta immediately instead of waiting for the reduced durable interval.
        liveInteraction.current = false
        clearTimeout(liveInteractionTimer.current)
        liveInteractionTimer.current = undefined
        clearTimeout(liveCollabTimer.current)
        liveCollabTimer.current = undefined
        flushLiveCollab()
        flushPendingCollab(true)
      } else {
        scheduleCollabSync()
      }
    } else {
      pending.current = { name: board.name, elements, appState, files }
      clearTimeout(timer.current)
      timer.current = setTimeout(flush, SAVE_DEBOUNCE_MS)
    }
  }

  const showCollabSync = Boolean(
    collabDocumentId &&
    (collabSynchronizing || collabSession.current?.documentId !== collabDocumentId || !collabJoinReady.current),
  )
  const syncStatus: SyncStatus | null =
    board && collabDocumentId && persistenceStatus !== 'idle' && !showCollabSync ? persistenceStatus : null

  // Hand the status to the host whenever the canvas can't show it.
  const hiddenSyncStatus = syncStatus && !badgeFits ? syncStatus : null
  const onSyncStatusHiddenRef = useRef(onSyncStatusHidden)
  onSyncStatusHiddenRef.current = onSyncStatusHidden
  useEffect(() => {
    onSyncStatusHiddenRef.current?.(hiddenSyncStatus)
  }, [hiddenSyncStatus])
  useEffect(() => () => onSyncStatusHiddenRef.current?.(null), [])

  if (!board) {
    if (!showEmptyPlaceholder) return null
    return (
      <div className="canvas">
        <div className="no-board">No Board Selected</div>
      </div>
    )
  }

  return (
    <div className="canvas" data-canvas-theme={theme}>
      <Excalidraw
        key={board.key}
        name={board.name}
        theme={theme}
        initialData={{ ...board.data, libraryItems: board.library }}
        excalidrawAPI={(instance) => {
          api.current = instance
        }}
        onChange={onChange}
        isCollaborating={Boolean(collabDocumentId)}
        onUserFollow={({ userToFollow, action }) => {
          sendFollow(userToFollow.socketId, action)
          if (action !== 'FOLLOW') return
          // Jump to the followed user's last known view right away instead of
          // waiting for their next scroll.
          const bounds = peerViews.current.get(userToFollow.socketId)
          if (bounds && api.current) applyFollowedBounds(api.current, bounds)
        }}
        onPointerUpdate={(payload) => {
          const target = collabSession.current
          if (!target) return
          const nickname = collaborationNickname.trim() || undefined
          const message: Extract<CollabMessage, { type: 'pointer' }> = {
            type: 'pointer',
            peerId,
            nickname,
            color: collaborationColor || undefined,
            avatar: collaborationAvatar || undefined,
            avatarOnly: collaborationAvatar && collaborationAvatarOnly ? true : undefined,
            x: payload.pointer.x,
            y: payload.pointer.y,
            tool: payload.pointer.tool,
            button: payload.button,
          }
          // Laser strokes are defined by press/release; never let a coalesced
          // message swallow either edge.
          const previous = pendingPointer.current
          if (previous && (previous.button !== message.button || previous.tool !== message.tool)) {
            flushPointer()
          }
          pendingPointer.current = message
          if (message.tool === 'laser') {
            if (!pointerTimer.current || previous?.button !== message.button) {
              clearTimeout(pointerTimer.current)
              pointerTimer.current = setTimeout(flushPointer, LASER_UPDATE_INTERVAL_MS)
            }
          } else if (!pointerTimer.current) {
            pointerTimer.current = setTimeout(flushPointer, POINTER_UPDATE_INTERVAL_MS)
          }
        }}
        onLibraryChange={(items) => {
          // Never overwrite the library file before it has been loaded (e.g. with an empty library on startup).
          if (!libraryLoaded.current) return
          if (importing.current) {
            clearTimeout(importing.current.timer)
            const added = items.length - importing.current.before
            importing.current = null
            api.current?.setToast({
              message: added > 0 ? `Added ${added} shape${added === 1 ? '' : 's'} to the library` : 'These shapes are already in the library',
              duration: 4000,
              closable: true,
            })
          }
          library.current = items
          const json = serializeLibraryAsJSON(items)
          if (json === lastLibraryJson.current) return
          lastLibraryJson.current = json
          canvasLibrary(json)
        }}
        onLinkOpen={(element, event) => {
          if (!element.link) return
          event.preventDefault()
          openLink(element.link)
        }}
        UIOptions={{ canvasActions: { loadScene: false, saveToActiveFile: false, toggleTheme: null } }}
      >
        <MainMenu>
          <MainMenu.DefaultItems.CommandPalette />
          <MainMenu.DefaultItems.SearchMenu />
          <MainMenu.DefaultItems.SaveAsImage />
          <MainMenu.DefaultItems.Help />
          <MainMenu.DefaultItems.ClearCanvas />
          <MainMenu.Separator />
          <MainMenu.DefaultItems.ChangeCanvasBackground />
        </MainMenu>
        <WelcomeScreen>
          <WelcomeScreen.Hints.MenuHint />
          <WelcomeScreen.Hints.ToolbarHint />
          <WelcomeScreen.Hints.HelpHint />
        </WelcomeScreen>
      </Excalidraw>
      <RemoteCursors register={registerCursorOverlay} />
      {followerNames.length > 0 && (
        <div className="canvas-followed-status" role="status" aria-live="polite">
          <span className="canvas-persistence-dot" aria-hidden="true" />
          <span>
            {followerNames.length <= 2 ? `Followed by ${followerNames.join(' and ')}` : `Followed by ${followerNames.length} people`}
          </span>
        </div>
      )}
      {showCollabSync && (
        <div className="canvas-syncing" role="status" aria-live="polite" aria-busy="true">
          <div className="canvas-syncing-card">
            <span className="canvas-syncing-spinner" aria-hidden="true" />
            <span>Syncing latest changes…</span>
          </div>
        </div>
      )}
      {syncStatus && <PersistenceBadge status={syncStatus} onFitChange={setBadgeFits} />}
    </div>
  )
}

/**
 * The collaboration sync status beside Excalidraw's menu button, shown only while it has room beside the
 * shape toolbar (see useToolbarRoom). Otherwise it stays laid out, to keep measuring, but hidden, and
 * reports that through `onFitChange` so the host can show the status elsewhere.
 */
function PersistenceBadge({ status, onFitChange }: { status: SyncStatus; onFitChange: (fits: boolean) => void }) {
  const badgeRef = useRef<HTMLDivElement>(null)
  const { fits } = useToolbarRoom(badgeRef)

  useEffect(() => onFitChange(fits), [fits, onFitChange])
  // Unmounting (no status, or another board) leaves room for the next badge.
  useEffect(() => () => onFitChange(true), [onFitChange])

  return (
    <div
      ref={badgeRef}
      className={`canvas-persistence-status ${status}${fits ? '' : ' offstage'}`}
      role="status"
      aria-live="polite"
      aria-hidden={!fits}
    >
      <span className="canvas-persistence-dot" aria-hidden="true" />
      <span>{PERSISTENCE_LABELS[status]}</span>
    </div>
  )
}
