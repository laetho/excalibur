/**
 * Messages between the Excalidraw canvas (this web view) and the native Swift app.
 *
 * Swift → JS: calls window.whiteboards.open(...) / .takePending() / …
 * JS → Swift: window.webkit.messageHandlers.bridge.postMessage(...)
 */
export type ToNative =
  | { type: 'ready' }
  | { type: 'change'; name: string; json: string }
  | { type: 'openLink'; url: string }
  | { type: 'library'; json: string }
  | ({ type: 'view'; name: string } & Viewport)
  | { type: 'log'; level: 'error' | 'info'; message: string }

/** Where the user was looking on a board (Excalidraw's scroll and zoom), remembered per board by Swift. */
export type Viewport = { scrollX: number; scrollY: number; zoom: number }

export type OpenRequest = {
  /** Board name, used to tag change messages so Swift writes to the right file. */
  name: string
  /** Contents of the .excalidraw file, or an empty string for a new/empty board. */
  json: string
  /** The board's last viewport, or null to zoom so the whole drawing fits. */
  view?: Viewport | null
}

type NativeBridge = { postMessage: (message: ToNative) => void }

declare global {
  interface Window {
    webkit?: { messageHandlers?: { bridge?: NativeBridge } }
    whiteboards?: {
      /** Shows a board. Any change not yet sent is discarded; Swift takes it with takePending() first. */
      open: (request: OpenRequest) => void
      /** The open board's file was renamed; keep editing (and undo history) under the new name. */
      rename: (name: string) => void
      /** The Excalidraw library from the boards folder (`.excalidrawlib` JSON), or null if there is none yet. */
      setLibrary: (json: string | null) => void
      /** Renders a board (`.excalidraw` JSON) as a PNG data URL for the sidebar, or null for an empty board. */
      renderThumbnail: (json: string, maxSize: number) => Promise<string | null>
      /** Returns (and clears) the not-yet-sent change, so Swift can save it before switching boards or quitting. */
      takePending: () => { name: string; json: string } | null
    }
    /** Collected messages when running outside the app (browser dev/tests). */
    __bridgeMessages?: ToNative[]
    /** Content-Security-Policy violations seen by the page (for logs and tests). */
    __cspViolations?: string[]
  }
}

export function postToNative(message: ToNative) {
  const bridge = window.webkit?.messageHandlers?.bridge
  if (bridge) {
    bridge.postMessage(message)
  } else {
    // Outside the native app (e.g. `pnpm dev` in a browser): keep messages for inspection.
    ;(window.__bridgeMessages ??= []).push(message)
    if (message.type === 'openLink') window.open(message.url, '_blank', 'noopener')
  }
}
