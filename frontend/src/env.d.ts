interface Window {
  /** Wails v3 injects its bridge into native webviews. */
  _wails?: { environment?: { OS?: string } }
  /** Where Excalidraw loads its fonts from. */
  EXCALIDRAW_ASSET_PATH?: string | string[]
  /** CSP violations recorded for tests and logs (see App.tsx). */
  __cspViolations?: string[]
}
