import {
  CaptureUpdateAction,
  Excalidraw,
  exportToCanvas,
  getCommonBounds,
  hashElementsVersion,
  MainMenu,
  restore,
  restoreLibraryItems,
  serializeAsJSON,
  serializeLibraryAsJSON,
  WelcomeScreen,
} from '@excalidraw/excalidraw'
import '@excalidraw/excalidraw/index.css'
import type { ImportedDataState } from '@excalidraw/excalidraw/data/types'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState, BinaryFileData, BinaryFiles, ExcalidrawImperativeAPI, LibraryItems, SocketId } from '@excalidraw/excalidraw/types'
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { generateKeyBetween, generateNKeysBetween } from 'fractional-indexing'
import * as Y from 'yjs'
import {
  canvasChange,
  canvasLibrary,
  canvasReady,
  canvasView,
  collabJoin,
  collabSend,
  log,
	  onAICanvasOperations,
  onCanvasOpen,
  onCanvasRename,
  onCanvasSetLibrary,
  onCollab,
  onRenderThumbnail,
  onTakePending,
  openLink,
  pendingResult,
  thumbnailResult,
  type CanvasAppearance,
  type Viewport,
} from './backend'
import { FILE_CHOSEN_EVENT } from './shims/browser-fs-access'

const SAVE_DEBOUNCE_MS = 300
/** While collaborating, scenes are shared more eagerly so remote edits land quickly. */
const COLLAB_DEBOUNCE_MS = 80
/** Scrolling and zooming are reported less often than edits; only the last position matters. */
const VIEW_DEBOUNCE_MS = 500
const LIBRARY_FILE = /\.excalidrawlib$/i
/** Give up waiting for an import to finish after this long (Excalidraw shows its own error if it fails). */
const IMPORT_TIMEOUT_MS = 60_000
/** Where the library used to be stored (in the web view); migrated to the boards folder on first launch. */
const LEGACY_LIBRARY_KEY = 'whiteboards.library'

/** A NATS board with collaboration active: which source/bucket it lives in and its board id. */
export type CollabInfo = { sourceId: string; bucket: string; boardId: string }

/** One anonymous peer per browser session, so several collaborators get distinct cursors. */
const peerId = Math.random().toString(36).slice(2)
const PEER_COLORS = [
  { background: '#ffc9c9', stroke: '#e03131' },
  { background: '#a5d8ff', stroke: '#1971c2' },
  { background: '#b2f2bb', stroke: '#2f9e44' },
  { background: '#ffec99', stroke: '#f08c00' },
  { background: '#d0bfff', stroke: '#6741d9' },
  { background: '#ffdeeb', stroke: '#e64980' },
]

type CollabMessage =
  | { type: 'yjs'; from: string; update: string }
  | { type: 'sync'; from: string; sv: string }
  | { type: 'sync-reply'; from: string; update: string }
  | { type: 'pointer'; peerId: string; nickname?: string; x: number; y: number; tool: 'pointer' | 'laser' }

/**
 * AppState keys shared with collaborators through the Yjs document. Selection,
 * cursor and editing state stay local; only the visual defaults (background and
 * current tool style) are shared.
 */
const APP_STATE_KEYS = [
  'viewBackgroundColor',
  'currentItemStrokeColor',
  'currentItemBackgroundColor',
  'currentItemFillStyle',
  'currentItemStrokeWidth',
  'currentItemStrokeStyle',
  'currentItemRoughness',
  'currentItemOpacity',
  'currentItemFontFamily',
  'currentItemFontSize',
  'currentItemTextAlign',
  'currentItemStartArrowhead',
  'currentItemEndArrowhead',
  'currentItemRoundness',
] as const

/** One element inside the Yjs `elements` array: the whole element plus its z-order key. */
type YElementItem = { map: Y.Map<unknown>; el: ExcalidrawElement; pos: string }

/** Yjs binary updates ride over NATS as base64 strings. */
function bytesToBase64(bytes: Uint8Array): string {
  let binary = ''
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i])
  return btoa(binary)
}

function base64ToBytes(base64: string): Uint8Array {
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes
}

type OpenBoard = { name: string; data: ImportedDataState; key: number; library: LibraryItems }

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

/** The latest unsaved scene. Serialized only when it's actually sent, not on every change (which can be every mouse move). */
type PendingScene = { name: string; elements: readonly ExcalidrawElement[]; appState: AppState; files: BinaryFiles }

function serialize(scene: PendingScene) {
  return { name: scene.name, json: serializeAsJSON(scene.elements, scene.appState, scene.files, 'local') }
}

function parseBoard(json: string): ImportedDataState {
  if (!json.trim()) return {}
  return JSON.parse(json) as ImportedDataState
}

/**
 * Union of two element lists by id, for merging a remote scene into the local
 * scene without clobbering disjoint concurrent edits. Local order is preserved;
 * the remote element wins for overlapping ids; remote-only elements are appended.
 */

function comparePos(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0
}

/** The elements stored in the Yjs document, in array order (not yet z-sorted). */
function ydocElements(doc: Y.Doc): YElementItem[] {
  return (doc.getArray('elements').toArray() as Y.Map<unknown>[]).map((map) => ({
    map,
    el: map.get('el') as ExcalidrawElement,
    pos: map.get('pos') as string,
  }))
}

/** The elements in z-order (frontmost last), as Excalidraw expects them. */
function ydocElementsSorted(doc: Y.Doc): ExcalidrawElement[] {
  return ydocElements(doc)
    .sort((a, b) => comparePos(a.pos, b.pos))
    .map((item) => item.el)
}

/** The whitelisted AppState keys currently stored in the Yjs document. */
function ydocAppState(doc: Y.Doc): Partial<AppState> {
  const map = doc.getMap('appState')
  const result: Partial<AppState> = {}
  for (const key of APP_STATE_KEYS) {
    if (map.has(key)) (result as Record<string, unknown>)[key] = map.get(key)
  }
  return result
}

function ydocFiles(doc: Y.Doc): BinaryFiles {
  const map = doc.getMap('files')
  const result: BinaryFiles = {}
  for (const [id, file] of map) result[id] = file as BinaryFiles[string]
  return result
}

/**
 * Whether an element changed in a way worth writing back to the Yjs document.
 * Excalidraw bumps `versionNonce` on every mutation, so equal bookkeeping means
 * unchanged. When it differs, compare the content (excluding the version fields)
 * so a re-normalization-only bump on an applied remote scene is not echoed back
 * out as a new edit (which would loop).
 */
function elementChanged(a: ExcalidrawElement, b: ExcalidrawElement): boolean {
  if (a.version === b.version && a.versionNonce === b.versionNonce) return false
  const strip = ({ version, versionNonce, updated, ...rest }: ExcalidrawElement) => rest
  return JSON.stringify(strip(a)) !== JSON.stringify(strip(b))
}

/** Build a Yjs document from a loaded scene (elements, whitelisted appState, files). */
function buildYdoc(data: ImportedDataState): Y.Doc {
  const doc = new Y.Doc()
  doc.transact(() => {
    const elements = doc.getArray('elements')
    const items = data.elements ?? []
    const keys = generateNKeysBetween(null, null, Math.max(items.length, 1))
    for (let i = 0; i < items.length; i++) {
      const map = new Y.Map<unknown>()
      map.set('el', items[i])
      map.set('pos', keys[i])
      elements.push([map])
    }
    const appState = doc.getMap('appState')
    for (const key of APP_STATE_KEYS) {
      const value = (data.appState as Partial<AppState> | undefined)?.[key]
      if (value !== undefined) appState.set(key, value)
    }
    const files = doc.getMap('files')
    for (const [id, file] of Object.entries(data.files ?? {})) {
      files.set(id, file)
    }
  }, 'local')
  return doc
}

/**
 * Write the current Excalidraw scene into the Yjs document (origin 'local').
 * Elements are matched by id: changed ones are updated, new ones inserted with a
 * fractional index between their neighbours, and ones no longer present are
 * removed. Returns whether anything was written.
 */
function syncElementsToYdoc(doc: Y.Doc, elements: readonly ExcalidrawElement[]): boolean {
  const yArray = doc.getArray('elements')
  const current = new Map<string, YElementItem>()
  for (const item of ydocElements(doc)) current.set(item.el.id, item)

  const seen = new Set<string>()
  let prevPos: string | null = null
  const inserts: { el: ExcalidrawElement; pos: string }[] = []
  const updates: { map: Y.Map<unknown>; el: ExcalidrawElement }[] = []
  const reorders: { map: Y.Map<unknown>; pos: string }[] = []
  const removals: Y.Map<unknown>[] = []

  for (let i = 0; i < elements.length; i++) {
    const el = elements[i]
    const existing = current.get(el.id)
    if (existing) {
      seen.add(el.id)
      if (prevPos !== null && existing.pos <= prevPos) {
        // The element moved before its previous neighbour: give it a fresh index.
        let next = nextPosInNewArray(elements, current, i)
        if (next !== null && next <= prevPos) next = null
        const pos = generateKeyBetween(prevPos, next)
        reorders.push({ map: existing.map, pos })
        prevPos = pos
      } else {
        prevPos = existing.pos
      }
      if (elementChanged(existing.el, el)) updates.push({ map: existing.map, el })
    } else {
      seen.add(el.id)
      let next = nextPosInNewArray(elements, current, i)
      if (prevPos !== null && next !== null && next <= prevPos) next = null
      const pos = generateKeyBetween(prevPos, next)
      inserts.push({ el, pos })
      prevPos = pos
    }
  }
  for (const [id, item] of current) {
    if (!seen.has(id)) removals.push(item.map)
  }

  if (inserts.length + updates.length + reorders.length + removals.length === 0) return false

  doc.transact(() => {
    for (const { el, pos } of inserts) {
      const map = new Y.Map<unknown>()
      map.set('el', el)
      map.set('pos', pos)
      yArray.push([map])
    }
    for (const { map, el } of updates) map.set('el', el)
    for (const { map, pos } of reorders) map.set('pos', pos)
    for (const map of removals) {
      const index = yArray.toArray().indexOf(map)
      if (index >= 0) yArray.delete(index)
    }
  }, 'local')
  return true
}

/** The z-order key of the next element in `elements` (from index i) that is already in the document. */
function nextPosInNewArray(
  elements: readonly ExcalidrawElement[],
  current: Map<string, YElementItem>,
  i: number,
): string | null {
  for (let j = i + 1; j < elements.length; j++) {
    const existing = current.get(elements[j].id)
    if (existing) return existing.pos
  }
  return null
}

/** Copy the whitelisted AppState keys into the Yjs document (origin 'local'). */
function syncAppStateToYdoc(doc: Y.Doc, appState: AppState): void {
  const map = doc.getMap('appState')
  doc.transact(() => {
    for (const key of APP_STATE_KEYS) {
      const value = appState[key]
      if (value === undefined) continue
      if (map.get(key) !== value) map.set(key, value)
    }
  }, 'local')
}

/** Copy files that are new or changed into the Yjs document (origin 'local'). */
function syncFilesToYdoc(doc: Y.Doc, files: BinaryFiles): void {
  const map = doc.getMap('files')
  doc.transact(() => {
    for (const [id, file] of Object.entries(files)) {
      if (map.get(id) !== file) map.set(id, file)
    }
  }, 'local')
}

/** Serialize the Yjs document back into a plain scene for persistence. */
function ydocToScene(doc: Y.Doc): PendingScene {
  return {
    name: '',
    elements: ydocElementsSorted(doc),
    appState: ydocAppState(doc) as AppState,
    files: ydocFiles(doc),
  }
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
}: {
  canvasAppearance: CanvasAppearance
  showEmptyPlaceholder?: boolean
  /** When set, the open board is on a NATS source and edits/pointers are shared over NATS. */
  collab?: CollabInfo | null
  /** Wails board identity used to reject late AI edits after a board switch. */
  boardId?: string | null
  /** Name advertised beside this client's cursor during NATS collaboration. */
  collaborationNickname?: string
}) {
  const [board, setBoard] = useState<OpenBoard | null>(null)
  const systemTheme = useSystemTheme()
  const theme: 'light' | 'dark' = canvasAppearance === 'system' ? systemTheme : canvasAppearance === 'dark' ? 'dark' : 'light'

  const api = useRef<ExcalidrawImperativeAPI | null>(null)
  /** The collaboration target for the current board, kept in a ref so memoized callbacks can read it. */
  const collabRef = useRef(collab)
  const boardIdRef = useRef(boardId)
  /** Name of the open board, for serializing the Yjs document on flush. */
  const boardNameRef = useRef('')
  /** The Yjs document backing collaboration for the current NATS board, or null when not collaborating. */
  const ydoc = useRef<Y.Doc | null>(null)
  /** File ids already handed to Excalidraw (via addFiles), so remote file updates are not re-added. */
  const appliedFileIds = useRef<Set<string>>(new Set())
  /** The Yjs document changed since the last flush, so it must be persisted. */
  const ydocDirty = useRef(false)
  const pending = useRef<PendingScene | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const lastVersion = useRef<number | null>(null)
  const lastFileCount = useRef(0)
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

  // Keep the collaboration target in sync with the prop.
  useEffect(() => {
    collabRef.current = collab
  }, [collab])
  useEffect(() => {
    boardIdRef.current = boardId
  }, [boardId])

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

  const flush = useCallback(() => {
    clearTimeout(timer.current)
    // While collaborating the Yjs document is the source of truth: persist it as a
    // plain scene snapshot (live sharing happens through Yjs updates, not here).
    const doc = ydoc.current
    if (doc && ydocDirty.current) {
      ydocDirty.current = false
      const scene = ydocToScene(doc)
      scene.name = boardNameRef.current
      const { name, json } = serialize(scene)
      canvasChange(name, json)
    } else {
      const scene = pending.current
      pending.current = null
      if (scene) {
        const { name, json } = serialize(scene)
        canvasChange(name, json)
      }
    }
    flushView()
    return true
  }, [flushView])

  /**
   * Push a remote Yjs change into Excalidraw (never captured into undo history),
   * hand new files to the API, and persist the change. Called from the Yjs
   * observers for anything that did not originate locally.
   */
  const observeYdoc = useCallback(() => {
    const doc = ydoc.current
    const instance = api.current
    if (!doc || !instance) return
    const elements = ydocElementsSorted(doc)
    const appState = ydocAppState(doc)
    const files = ydocFiles(doc)
    if (hashElementsVersion(elements) !== hashElementsVersion(instance.getSceneElementsIncludingDeleted())) {
      instance.updateScene({ elements, captureUpdate: CaptureUpdateAction.NEVER })
    }
    const current = instance.getAppState()
    const merged: Partial<AppState> = {}
    let appChanged = false
    for (const key of APP_STATE_KEYS) {
      const value = appState[key]
      if (value !== undefined && value !== current[key]) {
        ;(merged as Record<string, unknown>)[key] = value
        appChanged = true
      }
    }
    if (appChanged) instance.updateScene({ appState: merged as AppState })
    const newFiles: BinaryFileData[] = []
    for (const [id, file] of Object.entries(files)) {
      if (!appliedFileIds.current.has(id)) {
        appliedFileIds.current.add(id)
        newFiles.push(file)
      }
    }
    if (newFiles.length > 0) instance.addFiles(newFiles)
    // Remote changes must be persisted too; the Yjs document is the source of truth.
    ydocDirty.current = true
    clearTimeout(timer.current)
    timer.current = setTimeout(flush, COLLAB_DEBOUNCE_MS)
  }, [flush])

  /** Yjs observers: ignore local writes (already applied) and the initial build. */
  const onYdocChange = useCallback((event: Y.YEvent<any>) => {
    if (event.transaction.origin === 'local' || event.transaction.origin === 'init') return
    observeYdoc()
  }, [observeYdoc])

  /** Broadcast our own edits (origin 'local') to collaborators as Yjs binary updates. */
  const onYdocUpdate = useCallback((_update: Uint8Array, origin: unknown) => {
    if (origin !== 'local') return
    const target = collabRef.current
    if (!target) return
    const message: CollabMessage = { type: 'yjs', from: peerId, update: bytesToBase64(_update) }
    collabSend(target.boardId, JSON.stringify(message))
  }, [])

  const wireYdoc = useCallback(
    (doc: Y.Doc) => {
      doc.getArray('elements').observe(onYdocChange)
      doc.getMap('appState').observe(onYdocChange)
      doc.getMap('files').observe(onYdocChange)
      doc.on('update', onYdocUpdate)
    },
    [onYdocChange, onYdocUpdate],
  )

  const teardownYdoc = useCallback(() => {
    const doc = ydoc.current
    ydoc.current = null
    ydocDirty.current = false
    if (doc) doc.destroy()
  }, [])

  useEffect(() => {
    let key = 0
    const unlistens: Promise<() => void>[] = [
      onCanvasOpen(({ name, json, view }) => {
        clearTimeout(timer.current)
        pending.current = null
        // Remember where the user was on the board being left.
        flushView()
        try {
          const data = parseBoard(json)
          setBoard({ name, data, key: ++key, library: library.current })
          boardNameRef.current = name
          lastVersion.current = null
          trackingView.current = false
          lastView.current = null
          initialView.current = {
            saved: isViewport(view) ? view : null,
            hasContent: (data.elements ?? []).some((e) => !e.isDeleted),
          }
          // A NATS board gets a fresh Yjs document; local boards stay on the plain JSON path.
          teardownYdoc()
          if (collabRef.current) {
            ydoc.current = buildYdoc(data)
            appliedFileIds.current = new Set(Object.keys(data.files ?? {}))
            wireYdoc(ydoc.current)
          }
        } catch (err) {
          log('error', `Could not read ${name}: ${String(err)}`)
        }
      }),
      onCanvasRename((name) => {
        setBoard((current) => (current ? { ...current, name } : current))
        boardNameRef.current = name
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
        flushView()
        const doc = ydoc.current
        const scene = doc ? { ...ydocToScene(doc), name: boardNameRef.current } : pending.current
        pending.current = null
        if (scene) {
          const { name, json } = serialize(scene)
          pendingResult(requestId, name, json)
        } else {
          pendingResult(requestId, null, null)
        }
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
	          instance.setToast({ message: 'AI updated the canvas — use Undo to revert', duration: 5000, closable: true })
	        } catch (err) {
	          log('error', `Could not apply AI canvas operations: ${String(err)}`)
	          instance.setToast({ message: 'Could not apply the AI canvas update', duration: 5000, closable: true })
	        }
	      }),
    ]
    canvasReady()
    window.addEventListener('pagehide', flush)
    return () => {
      window.removeEventListener('pagehide', flush)
      teardownYdoc()
      unlistens.forEach((p) => p.then((unlisten) => unlisten()))
    }
  }, [flush, flushView, teardownYdoc, wireYdoc])

  // Join collaboration when a NATS board is open (the backend relays messages as `canvas:collab`),
  // then ask peers for anything our (bucket-loaded) Yjs document is missing.
  useEffect(() => {
    if (!board || !collab) return
    const { sourceId, bucket, boardId } = collab
    collabJoin(sourceId, bucket, boardId)
      .then(() => {
        const doc = ydoc.current
        if (!doc) return
        const message: CollabMessage = { type: 'sync', from: peerId, sv: bytesToBase64(Y.encodeStateVector(doc)) }
        collabSend(boardId, JSON.stringify(message))
      })
      .catch((err: unknown) => log('error', `Could not join collaboration: ${String(err)}`))
  }, [board?.name, collab])

  // Apply remote Yjs updates, answer sync requests, and show remote cursors.
  useEffect(() => {
    let unlisten: (() => void) | undefined
    onCollab((boardId, data) => {
      const target = collabRef.current
      if (!target || boardId !== target.boardId) return
      let message: CollabMessage
      try {
        message = JSON.parse(data)
      } catch {
        return
      }
      // NATS echoes our own published messages back; ignore them.
      if ('from' in message && message.from === peerId) return
      const doc = ydoc.current
      if (message.type === 'yjs') {
        if (!doc) return
        try {
          Y.applyUpdate(doc, base64ToBytes(message.update), 'remote')
        } catch (err) {
          log('error', `Could not apply a remote Yjs update: ${String(err)}`)
        }
      } else if (message.type === 'sync') {
        if (!doc) return
        try {
          // The state vector arrives base64-encoded; encodeStateAsUpdate takes it raw.
          const update = Y.encodeStateAsUpdate(doc, base64ToBytes(message.sv))
          if (update.byteLength > 0) {
            const reply: CollabMessage = { type: 'sync-reply', from: peerId, update: bytesToBase64(update) }
            collabSend(target.boardId, JSON.stringify(reply))
          }
        } catch (err) {
          log('error', `Could not answer a collaboration sync request: ${String(err)}`)
        }
      } else if (message.type === 'sync-reply') {
        if (!doc) return
        try {
          Y.applyUpdate(doc, base64ToBytes(message.update), 'remote')
        } catch (err) {
          log('error', `Could not apply a collaboration sync reply: ${String(err)}`)
        }
      } else if (message.type === 'pointer') {
        const collaborators = new Map(api.current?.getAppState().collaborators ?? [])
        collaborators.set(message.peerId as SocketId, {
          pointer: { x: message.x, y: message.y, tool: message.tool },
          username: collaborationDisplayName(message.peerId, message.nickname),
          color: PEER_COLORS[message.peerId.charCodeAt(0) % PEER_COLORS.length],
        })
        api.current?.updateScene({ collaborators })
      }
    }).then((u) => {
      unlisten = u
    })
    return () => {
      unlisten?.()
    }
  }, [])

  const onChange = (elements: readonly ExcalidrawElement[], appState: AppState, files: BinaryFiles) => {
    if (!board) return
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
    if (trackingView.current) {
      const view: Viewport = { scrollX: appState.scrollX, scrollY: appState.scrollY, zoom: appState.zoom.value }
      if (!sameView(lastView.current, view)) {
        lastView.current = view
        pendingView.current = { name: board.name, ...view }
        clearTimeout(viewTimer.current)
        viewTimer.current = setTimeout(flushView, VIEW_DEBOUNCE_MS)
      }
    }
    // onChange also fires for scrolling, selection etc. Only save when the drawing itself changed.
    const version = hashElementsVersion(elements)
    const fileCount = Object.keys(files).length
    // Share the whitelisted AppState (background, current style) even when no element changed.
    const doc = ydoc.current
    if (doc) syncAppStateToYdoc(doc, appState)
    if (lastVersion.current === null) {
      lastVersion.current = version
      lastFileCount.current = fileCount
      return
    }
    if (version === lastVersion.current && fileCount === lastFileCount.current) return
    lastVersion.current = version
    lastFileCount.current = fileCount
    if (doc) {
      // Write the diff into the Yjs document (origin 'local'). The observer skips
      // these, and the diff-based sync means applying a remote scene produces no
      // writes here, so remote state is never echoed back out.
      const wrote = syncElementsToYdoc(doc, elements)
      syncFilesToYdoc(doc, files)
      if (wrote) {
        ydocDirty.current = true
        clearTimeout(timer.current)
        timer.current = setTimeout(flush, COLLAB_DEBOUNCE_MS)
      }
    } else {
      pending.current = { name: board.name, elements, appState, files }
      clearTimeout(timer.current)
      timer.current = setTimeout(flush, SAVE_DEBOUNCE_MS)
    }
  }

  if (!board) {
    if (!showEmptyPlaceholder) return null
    return (
      <div className="canvas">
        <div className="no-board">No Board Selected</div>
      </div>
    )
  }

  return (
    <div className="canvas">
      <Excalidraw
        key={board.key}
        name={board.name}
        theme={theme}
        initialData={{ ...board.data, libraryItems: board.library }}
        excalidrawAPI={(instance) => {
          api.current = instance
        }}
        onChange={onChange}
        onPointerUpdate={(payload) => {
          const target = collabRef.current
          if (!target) return
          const nickname = collaborationNickname.trim() || undefined
          const message: CollabMessage = { type: 'pointer', peerId, nickname, x: payload.pointer.x, y: payload.pointer.y, tool: 'pointer' }
          collabSend(target.boardId, JSON.stringify(message))
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
    </div>
  )
}
