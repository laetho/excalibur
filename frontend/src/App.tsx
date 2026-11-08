import Canvas from './Canvas'
import Shell from './Shell'
import { isWails, log } from './backend'
import { showAlert } from './ipc'

/**
 * The app entry: the Wails app gets the sidebar + canvas shell; the SwiftUI macOS app hosts the canvas
 * full-screen and drives it through the native bridge (window.whiteboards, see backend.ts).
 */
export default function App() {
  if (isWails) return <Shell />
  return <Canvas canvasAppearance="system" showEmptyPlaceholder={false} />
}

// The webview has no synchronous native confirm bridge (the SwiftUI app shows native panels),
// so `window.alert` / `window.confirm` would silently do nothing / answer "no". `confirm` has to answer
// synchronously, which a dialog can't. Excalidraw only asks one thing this way: "Add N shapes to the library?",
// right after the user dropped or opened a library file (the explicit action), so that one is accepted. Anything
// else is declined and logged.
if (isWails) {
  window.alert = (message?: unknown) => {
    showAlert(String(message ?? '')).catch((err) => log('error', `Could not show an alert: ${String(err)}`))
  }
  window.confirm = (message?: string) => {
    const text = String(message ?? '')
    if (/\blibrary\b/i.test(text)) return true
    log('error', `Declined an unsupported confirm(): ${text.slice(0, 200)}`)
    return false
  }
}

// The canvas' own error listeners live here (not in a component) so they cover every render.
window.addEventListener('error', (e) => log('error', String(e.message)))
window.addEventListener('unhandledrejection', (e) => log('error', String(e.reason)))
// The app serves a strict Content-Security-Policy; record violations so they show up in logs and tests.
// Excalidraw registers every font with a backup URL on esm.sh next to our bundled copy; the CSP blocks the
// backup (as intended) while the bundled font loads, so those reports are expected noise.
const EXPECTED_BLOCKED = /^https:\/\/esm\.sh\/@excalidraw\/excalidraw@[^/]+\/dist\/prod\/fonts\//
document.addEventListener('securitypolicyviolation', (e) => {
  if (e.effectiveDirective === 'font-src' && EXPECTED_BLOCKED.test(e.blockedURI)) return
  const violation = `${e.effectiveDirective} blocked ${e.blockedURI || 'inline'}`
  ;(window.__cspViolations ??= []).push(violation)
  log('error', `CSP: ${violation}`)
})
