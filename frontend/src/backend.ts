/**
 * The canvas' backend, shared by both native apps.
 *
 * - Wails: talks to the Go backend through generated service bindings (./ipc).
 * - SwiftUI macOS app: talks to the native app through the bridge (./bridge) — Swift calls
 *   `window.whiteboards.*` and receives messages on the `bridge` webkit message handler.
 *
 * Both expose the same API, so Canvas.tsx doesn't care which app it runs in.
 */
import * as ipc from './ipc'
import { postToNative, type OpenRequest as BridgeOpenRequest } from './bridge'

export type { CanvasAppearance, Viewport } from './ipc'

/** True inside the Wails app; false in the legacy SwiftUI host and in a plain browser. */
export const isWails = typeof window !== 'undefined' && Boolean(window._wails?.environment)

// MARK: - Commands (JS → native)

export const canvasReady = () => {
  if (isWails) return ipc.canvasReady()
  postToNative({ type: 'ready' })
}

export const canvasChange = (name: string, json: string) => {
  if (isWails) return ipc.canvasChange(name, json)
  postToNative({ type: 'change', name, json })
}

export const canvasView = (name: string, view: ipc.Viewport) => {
  if (isWails) return ipc.canvasView(name, view)
  postToNative({ type: 'view', name, ...view })
}

export const canvasLibrary = (json: string) => {
  if (isWails) return ipc.canvasLibrary(json)
  postToNative({ type: 'library', json })
}

export const openLink = (url: string) => {
  if (isWails) return ipc.openLink(url)
  postToNative({ type: 'openLink', url })
}

export const log = (level: 'error' | 'info', message: string) => {
  if (isWails) return ipc.log(level, message)
  postToNative({ type: 'log', level, message })
}

// MARK: - Collaboration (NATS, Wails only)

/** Publishes ephemeral pointer presence. No-op in the SwiftUI app. */
export const collabSend = (sourceId: string, bucket: string, documentId: string, data: string) => {
  if (isWails) return ipc.collabSend(sourceId, bucket, documentId, data)
}

/** Subscribes to durable updates and returns the newest snapshot. */
export const collabJoin = (sourceId: string, bucket: string, documentId: string, subscriberId: string, baselineSequence: string) => {
  if (isWails) return ipc.collabJoin(sourceId, bucket, documentId, subscriberId, baselineSequence)
  return Promise.resolve({ snapshot: null, snapshotSequence: baselineSequence })
}

export const collabLeave = (sourceId: string, bucket: string, documentId: string, subscriberId: string) => {
  if (isWails) return ipc.collabLeave(sourceId, bucket, documentId, subscriberId)
  return Promise.resolve()
}

/** Acknowledges a durable update only after it reached the renderer. */
export const collabAck = (sourceId: string, bucket: string, documentId: string, subscriberId: string, sequence: string) => {
  if (isWails) return ipc.collabAck(sourceId, bucket, documentId, subscriberId, sequence)
  return Promise.resolve()
}

export const collabPublish = (sourceId: string, bucket: string, documentId: string, messageId: string, data: string) => {
  if (isWails) return ipc.collabPublish(sourceId, bucket, documentId, messageId, data)
  return Promise.resolve('0')
}

export const collabPutFiles = (sourceId: string, bucket: string, documentId: string, filesJson: string) => {
  if (isWails) return ipc.collabPutFiles(sourceId, bucket, documentId, filesJson)
  return Promise.resolve()
}

export const collabGetFiles = (sourceId: string, bucket: string, documentId: string, fileIds: string[]) => {
  if (isWails) return ipc.collabGetFiles(sourceId, bucket, documentId, fileIds)
  return Promise.resolve('{}')
}

export const collabSnapshot = (sourceId: string, bucket: string, boardId: string, documentId: string, sequence: string, json: string, owner: string) => {
  if (isWails) return ipc.collabSnapshot(sourceId, bucket, boardId, documentId, sequence, json, owner)
  return Promise.resolve()
}

/** Tries to become the snapshot compactor (lease in a JetStream KV bucket). */
export const collabAcquireLock = (sourceId: string, bucket: string, documentId: string, owner: string, ttlSeconds: number) => {
  if (isWails) return ipc.collabAcquireLock(sourceId, bucket, documentId, owner, ttlSeconds)
  return Promise.resolve(true)
}

/** Extends the leader lease; false when this client no longer owns it. No-op in the SwiftUI app. */
export const collabRenewLock = (sourceId: string, bucket: string, documentId: string, owner: string, ttlSeconds: number) => {
  if (isWails) return ipc.collabRenewLock(sourceId, bucket, documentId, owner, ttlSeconds)
  return Promise.resolve(true)
}

/** Drops the leader lease (only if this client still owns it). No-op in the SwiftUI app. */
export const collabReleaseLock = (sourceId: string, bucket: string, documentId: string, owner: string) => {
  if (isWails) return ipc.collabReleaseLock(sourceId, bucket, documentId, owner)
  return Promise.resolve()
}

/** A collaboration message arrived for the open board. No-op in the SwiftUI app. */
export function onCollab(cb: (documentId: string, subscriberId: string, data: string, sequence: string, delivery: string) => void): Promise<Unlisten> {
  if (isWails) return ipc.onCollab(cb)
  return Promise.resolve(() => {})
}

// MARK: - Events (native → JS)

type Unlisten = () => void

export function onCanvasOpen(cb: (request: ipc.OpenRequest) => void): Promise<Unlisten> {
  if (isWails) return ipc.onCanvasOpen(cb)
  setWhiteboards('open', (request: BridgeOpenRequest) =>
    cb({ name: request.name, json: request.json, view: request.view ?? null, snapshotSequence: '0' }),
  )
  return Promise.resolve(() => {})
}

export function onCanvasRename(cb: (name: string) => void): Promise<Unlisten> {
  if (isWails) return ipc.onCanvasRename(cb)
  setWhiteboards('rename', (name: string) => cb(name))
  return Promise.resolve(() => {})
}

export function onCanvasSetLibrary(cb: (json: string | null) => void): Promise<Unlisten> {
  if (isWails) return ipc.onCanvasSetLibrary(cb)
  setWhiteboards('setLibrary', (json: string | null) => cb(json))
  return Promise.resolve(() => {})
}

/** Granular canvas operations produced by the Wails AI harness. */
export function onAICanvasOperations(cb: (boardId: string, json: string) => void): Promise<Unlisten> {
  if (isWails) return ipc.onAICanvasOperations(cb)
  return Promise.resolve(() => {})
}

// MARK: - Request plumbing (Swift mode)
//
// Swift calls `window.whiteboards.renderThumbnail(json, maxSize)` and awaits the returned Promise,
// and calls `window.whiteboards.takePending()` synchronously and reads the return value. Both are
// bridged through requestIds to the same callbacks Canvas.tsx uses in Wails mode.

const thumbnailResolvers = new Map<string, (dataUrl: string | null) => void>()
let thumbnailRequestId = 0

const pendingSlots = new Map<string, { value: { name: string; json: string } | null }>()
let pendingRequestId = 0

/** Assigns one method of `window.whiteboards`, keeping any already-installed ones. */
function setWhiteboards<K extends keyof NonNullable<Window['whiteboards']>>(key: K, value: NonNullable<Window['whiteboards']>[K]) {
  const whiteboards = (window.whiteboards ?? {}) as NonNullable<Window['whiteboards']>
  whiteboards[key] = value
  window.whiteboards = whiteboards
}

export function onRenderThumbnail(cb: (requestId: string, json: string, maxSize: number) => void): Promise<Unlisten> {
  if (isWails) return ipc.onRenderThumbnail(cb)
  setWhiteboards('renderThumbnail', (json: string, maxSize: number) => {
    const requestId = `swift-thumbnail-${++thumbnailRequestId}`
    return new Promise<string | null>((resolve) => {
      thumbnailResolvers.set(requestId, resolve)
      cb(requestId, json, maxSize)
    })
  })
  return Promise.resolve(() => {})
}

export function thumbnailResult(requestId: string, dataUrl: string | null) {
  if (isWails) return ipc.thumbnailResult(requestId, dataUrl)
  const resolve = thumbnailResolvers.get(requestId)
  thumbnailResolvers.delete(requestId)
  resolve?.(dataUrl)
}

export function onTakePending(cb: (requestId: string) => void): Promise<Unlisten> {
  if (isWails) return ipc.onTakePending(cb)
  setWhiteboards('takePending', () => {
    const requestId = `swift-pending-${++pendingRequestId}`
    const slot: { value: { name: string; json: string } | null } = { value: null }
    pendingSlots.set(requestId, slot)
    cb(requestId)
    pendingSlots.delete(requestId)
    return slot.value
  })
  return Promise.resolve(() => {})
}

export function pendingResult(requestId: string, name: string | null, json: string | null) {
  if (isWails) return ipc.pendingResult(requestId, name, json)
  const slot = pendingSlots.get(requestId)
  if (slot) slot.value = name !== null && json !== null ? { name, json } : null
}
