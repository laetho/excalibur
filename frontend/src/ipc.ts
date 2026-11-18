/**
 * The frontend's half of the Wails IPC bridge.
 *
 * Go → JS: Wails events (`store-changed`, `canvas:*`, `menu:*`, …).
 * JS → Go: generated Wails v3 service bindings.
 */
import { Events } from '@wailsio/runtime'
import * as App from '../bindings/excalibur/app'

const app = () => App
const listen = <T>(name: string, callback: (payload: T) => void): Promise<Unlisten> =>
  Promise.resolve(Events.On(name, (event) => callback(event.data as T)))

// MARK: - Types (mirror of the Rust snapshot)

export type Viewport = { scrollX: number; scrollY: number; zoom: number }
export type CanvasAppearance = 'system' | 'light' | 'dark'
export type BoardSortOrder = 'name' | 'dateEdited'
export type Notice = { id: string; title: string; message: string }

export type BoardInfo = {
  id: string
  name: string
  /** Stable collaboration identity; unlike the filename, it survives rename. */
  documentId?: string
  /** Last modified, unix seconds. */
  modified: number
  isLink: boolean
  target: string | null
  /** Text found inside the drawing, for search. */
  texts: string[]
  /** PNG data URL for the sidebar, or null if there is none yet. */
  thumbnail: string | null
}

/** One group of boards within a source: a local folder or a NATS bucket. */
export type GroupInfo = { id: string; name: string; kind: 'folder' | 'bucket' }

/** One entry in the navigation tree: the local boards folder or a NATS server. */
export type SourceInfo = { id: string; name: string; kind: 'local' | 'nats'; groups: GroupInfo[] }

/**
 * A folder or bucket pinned open: its boards stay in the sidebar while another
 * group is active. The active group's entry carries an empty board list, since
 * its boards are already shown.
 */
export type PinnedGroupInfo = { source: string; group: string; boards: BoardInfo[] }

/**
 * How one folder or bucket looks in the sidebar. The default (find box open, no
 * filter, list expanded) is not stored, so a group only appears once touched.
 */
export type GroupState = { source: string; group: string; query?: string; find: boolean; folded: boolean }

/** A change to a group's sidebar state; omitted fields keep their value. */
export type GroupStatePatch = { source: string; group: string; query?: string; find?: boolean; folded?: boolean }

/** A NATS server the user added (persisted in preferences). */
export type NatsServerConfig = {
  id: string
  name: string
  url: string
  username: string
  password: string
  credentialsFile: string
  buckets: string[]
}
export type NatsServerSettings = Omit<NatsServerConfig, 'password'> & { hasPassword: boolean }
export type AISettings = { enabled: boolean; baseUrl: string; model: string; hasApiKey: boolean }
export type AISettingsUpdate = { enabled: boolean; baseUrl: string; model: string; apiKey: string; clearApiKey: boolean }
export type AIChatMessage = { role: 'user' | 'assistant'; content: string; thinking?: string }
export type AIModel = { id: string; model: string; thinking: boolean }

export type StoreSnapshot = {
  folder: string
  boards: BoardInfo[]
  selection: string | null
  sortOrder: BoardSortOrder
  showThumbnails: boolean
  canvasAppearance: CanvasAppearance
  applicationZoom: number
  collaborationNickname: string
  /** "#rrggbb" or empty for an automatic color. */
  collaborationColor: string
  /** Small image data URL, or empty. */
  collaborationAvatar: string
  /** Others see only the avatar, not the name, beside this client's cursor. */
  collaborationAvatarOnly: boolean
  alerts: Notice[]
  isAlertPresented: boolean
  /** The navigation tree: local folder + NATS servers with their buckets. */
  sources: SourceInfo[]
  /** Editable NATS configuration with passwords redacted. */
  natsServers: NatsServerSettings[]
  /** The active source id ("local" or "nats:<id>"). */
  activeSource: string
  /** The active group id (a folder path or a bucket name). */
  activeGroup: string
  /** Folders and buckets pinned open, with their boards. */
  pinned: PinnedGroupInfo[]
  /** Sidebar state saved for the folders and buckets the user has touched. */
  tree: GroupState[]
  ai: AISettings
}

export type OpenRequest = {
  name: string
  json: string
  view: Viewport | null
  snapshotSequence: string
}

// MARK: - Commands (JS → Rust)

export const getState = () => app().GetState() as Promise<StoreSnapshot>
export const canvasReady = () => app().CanvasReady()
export const canvasChange = (name: string, json: string) => app().CanvasChange(name, json)
export const canvasView = (name: string, view: Viewport) =>
  app().CanvasView(name, view.scrollX, view.scrollY, view.zoom)
export const canvasLibrary = (json: string) => app().CanvasLibrary(json)
export const openLink = (url: string) => app().OpenLink(url)
export const showAlert = (message: string) => app().ShowAlert(message)
export const log = (level: 'error' | 'info', message: string) => app().Log(level, message)
export const thumbnailResult = (requestId: string, dataUrl: string | null) => app().ThumbnailResult(requestId, dataUrl)
export const pendingResult = (requestId: string, name: string | null, json: string | null, errorMessage: string | null = null) =>
  app().PendingResult(requestId, name, json, errorMessage)
export const selectBoard = (id: string) => app().SelectBoard(id)
export const createBoard = (name: string) => app().CreateBoard(name)
export const renameBoard = (id: string, name: string) => app().RenameBoard(id, name)
export const duplicateBoard = (id: string) => app().DuplicateBoard(id)
export const trashBoard = (id: string) => app().TrashBoard(id)
export const importPaths = (paths: string[]) => app().ImportPaths(paths)
export const importBoardData = (name: string, json: string) => app().ImportBoardData(name, json)
export const chooseImport = () => app().ChooseImport()
export const addLocalFolder = () => app().AddLocalFolder()
/** Adds a boards folder by path, for desktops whose picker cannot select a directory. */
export const addLocalFolderPath = (path: string) => app().AddLocalFolderPath(path)
export const removeLocalFolder = (folder: string) => app().RemoveLocalFolder(folder)
export const reveal = (id: string | null) => app().Reveal(id)
/** Opens the boards folder in the file manager (Settings → Show in Finder). */
export const revealFolder = (folder: string) => app().RevealFolder(folder)
export const setSort = (order: BoardSortOrder) => app().SetSort(order)
export const setShowThumbnails = (show: boolean) => app().SetShowThumbnails(show)
export const setCanvasAppearance = (appearance: CanvasAppearance) => app().SetCanvasAppearance(appearance)
export const setApplicationZoom = (zoom: number) => app().SetApplicationZoom(zoom)
export type AppVersionInfo = { version: string; development: boolean }
export const appVersion = () => app().AppVersion() as Promise<AppVersionInfo>
export const setCollaborationNickname = (nickname: string) => app().SetCollaborationNickname(nickname)
export const setCollaborationAppearance = (color: string, avatar: string, avatarOnly: boolean) =>
  app().SetCollaborationAppearance(color, avatar, avatarOnly)
export const discoverAIModels = (baseUrl: string, apiKey: string, clearApiKey: boolean) =>
  app().DiscoverAIModels(baseUrl, apiKey, clearApiKey) as Promise<AIModel[]>
export const setAISettings = (settings: AISettingsUpdate) => app().SetAISettings(settings)
export const aiChat = (messages: AIChatMessage[], model: string, requestId: string, boardId: string, enableThinking: boolean) =>
  app().AIChat(messages, model, requestId, boardId, enableThinking) as Promise<string>
export const dismissAlert = () => app().DismissAlert()
export const setWindowTitle = (title: string) => app().SetWindowTitle(title)
/** Native open dialog for the canvas' "Insert image" / library "Open". */
export const pickFile = (extensions: string[]) => app().PickFile(extensions) as Promise<{ name: string; dataUrl: string } | null>
/** Native save dialog for the canvas' "Save as image". */
export const saveFile = (fileName: string, dataUrl: string) => app().SaveFile(fileName, dataUrl)

// MARK: - NATS commands (JS → Rust)

/** Adds a NATS server (connects, lists its buckets, persists it in preferences). */
export const addNatsServer = (config: NatsServerConfig) => app().AddNatsServer(config)
/** Updates a server after verifying the replacement connection. */
export const updateNatsServer = (config: NatsServerConfig, keepPassword: boolean) => app().UpdateNatsServer(config, keepPassword)
/** Opens a native picker and returns a NATS .creds path without reading its secret contents into the webview. */
export const chooseNatsCredentialsFile = () => app().ChooseNatsCredentialsFile() as Promise<string>
/** Removes a NATS server by id. */
export const removeNatsServer = (id: string) => app().RemoveNatsServer(id)
/** Re-lists the buckets on every configured NATS server and rescans the active source. */
export const refreshBuckets = () => app().RefreshBuckets()
/** Adds a bucket to a NATS source (creates it on the server if needed). */
export const addBucket = (sourceId: string, bucket: string) => app().AddBucket(sourceId, bucket)
/** Permanently deletes a bucket; the backend rejects it if any boards remain. */
export const deleteBucket = (sourceId: string, bucket: string) => app().DeleteBucket(sourceId, bucket)
/** Switches the sidebar's active source/group (a local folder or a NATS bucket). */
export const activateSource = (sourceId: string, groupId: string) => app().ActivateSource(sourceId, groupId)
/** Pins a folder or bucket open so its boards stay visible beside the active group. */
export const togglePinnedGroup = (sourceId: string, groupId: string) => app().TogglePinnedGroup(sourceId, groupId)
/** Saves a folder or bucket's sidebar state (find box, filter, folded list). */
export const setGroupState = (patch: GroupStatePatch) => app().SetGroupState(patch)
/** Copies a board into another source/group (drag & drop between sources). */
export const copyBoardToSource = (boardId: string, targetSource: string, targetGroup: string) =>
  app().CopyBoardToSource(boardId, targetSource, targetGroup)
/** Publishes ephemeral pointer presence for the open document. */
export const collabSend = (sourceId: string, bucket: string, documentId: string, data: string) => app().CollabSend(sourceId, bucket, documentId, data)
/** Subscribes to durable updates and returns the newest compacted snapshot. */
export const collabJoin = (sourceId: string, bucket: string, documentId: string, subscriberId: string, baselineSequence: string) =>
  app().CollabJoin(sourceId, bucket, documentId, subscriberId, baselineSequence)
/** Stops the per-document consumers so reopening can replay from its snapshot. */
export const collabLeave = (sourceId: string, bucket: string, documentId: string, subscriberId: string) => app().CollabLeave(sourceId, bucket, documentId, subscriberId)
/** Confirms that the renderer received and validated one durable update. */
export const collabAck = (sourceId: string, bucket: string, documentId: string, subscriberId: string, sequence: string) => app().CollabAck(sourceId, bucket, documentId, subscriberId, sequence)
/** Appends a drawing update to JetStream and returns its stream sequence. */
export const collabPublish = (sourceId: string, bucket: string, documentId: string, messageId: string, data: string) => app().CollabPublish(sourceId, bucket, documentId, messageId, data)
/** Stores large Excalidraw file blobs outside the JetStream update log. */
export const collabPutFiles = (sourceId: string, bucket: string, documentId: string, filesJson: string) => app().CollabPutFiles(sourceId, bucket, documentId, filesJson)
/** Loads file blobs referenced by a collaboration scene. */
export const collabGetFiles = (sourceId: string, bucket: string, documentId: string, fileIds: string[]) => app().CollabGetFiles(sourceId, bucket, documentId, fileIds)
/** Writes an immutable compacted scene at a caught-up stream sequence. */
export const collabSnapshot = (sourceId: string, bucket: string, boardId: string, documentId: string, sequence: string, json: string, owner: string) => app().CollabSnapshot(sourceId, bucket, boardId, documentId, sequence, json, owner)
/** Tries to become the snapshot compactor; the lease is held in a JetStream KV bucket. */
export const collabAcquireLock = (sourceId: string, bucket: string, documentId: string, owner: string, ttlSeconds: number) => app().CollabAcquireLock(sourceId, bucket, documentId, owner, ttlSeconds)
/** Extends the leader lease; resolves false when this client no longer owns it. */
export const collabRenewLock = (sourceId: string, bucket: string, documentId: string, owner: string, ttlSeconds: number) => app().CollabRenewLock(sourceId, bucket, documentId, owner, ttlSeconds)
/** Drops the leader lease (only if this client still owns it). */
export const collabReleaseLock = (sourceId: string, bucket: string, documentId: string, owner: string) => app().CollabReleaseLock(sourceId, bucket, documentId, owner)

// MARK: - Events (Rust → JS)

type Unlisten = () => void

export function onStoreChanged(cb: (snapshot: StoreSnapshot) => void): Promise<Unlisten> {
  return listen<StoreSnapshot>('store-changed', cb)
}

export function onBoardModified(cb: (id: string, modified: number | null) => void): Promise<Unlisten> {
  return listen<{ id: string; modified: number | null }>('board-modified', (payload) => cb(payload.id, payload.modified))
}

export function onCanvasOpen(cb: (request: OpenRequest) => void): Promise<Unlisten> {
  return listen<OpenRequest>('canvas:open', cb)
}

export function onCanvasRename(cb: (name: string) => void): Promise<Unlisten> {
  return listen<{ name: string }>('canvas:rename', (payload) => cb(payload.name))
}

export function onCanvasSetLibrary(cb: (json: string | null) => void): Promise<Unlisten> {
  return listen<{ json: string | null }>('canvas:set-library', (payload) => cb(payload.json))
}

export function onRenderThumbnail(cb: (requestId: string, json: string, maxSize: number) => void): Promise<Unlisten> {
  return listen<{ requestId: string; json: string; maxSize: number }>('canvas:render-thumbnail', (payload) =>
    cb(payload.requestId, payload.json, payload.maxSize),
  )
}

export function onTakePending(cb: (requestId: string) => void): Promise<Unlisten> {
  return listen<{ requestId: string }>('canvas:take-pending', (payload) => cb(payload.requestId))
}

/** A validated granular AI edit to apply through Excalidraw's normal undo/save path. */
export function onAICanvasOperations(cb: (boardId: string, json: string) => void): Promise<Unlisten> {
  return listen<{ boardId: string; json: string }>('canvas:ai-operations', (payload) => cb(payload.boardId, payload.json))
}

/**
 * A fragment of the active streamed Merlin response: answer and thinking
 * fragments are appended, while status replaces the loader line's label.
 */
export function onAIStream(cb: (requestId: string, kind: 'answer' | 'thinking' | 'status', delta: string) => void): Promise<Unlisten> {
  return listen<{ requestId: string; kind: 'answer' | 'thinking' | 'status'; delta: string }>('ai:stream', (payload) =>
    cb(payload.requestId, payload.kind, payload.delta),
  )
}

/** A durable scene update or ephemeral pointer update arrived. */
export function onCollab(cb: (documentId: string, subscriberId: string, data: string, sequence: string, delivery: string) => void): Promise<Unlisten> {
  return listen<{ documentId: string; subscriberId: string; data: string; sequence: string; delivery: string }>('canvas:collab', (payload) =>
    cb(payload.documentId, payload.subscriberId, payload.data, payload.sequence, payload.delivery),
  )
}

/** Number of locally durable updates still waiting for a JetStream publish ACK. */
export function onCollabOutbox(cb: (documentId: string, pending: number) => void): Promise<Unlisten> {
  return listen<{ documentId: string; pending: number }>('canvas:collab-outbox', (payload) =>
    cb(payload.documentId, payload.pending),
  )
}

/** Any menu item clicked in the native menu bar. */
export function onMenu(cb: (id: string) => void): Promise<Unlisten> {
  return listen<{ id?: unknown }>('menu', (payload) => {
    if (typeof payload?.id === 'string') cb(payload.id)
  })
}
