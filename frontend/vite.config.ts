import react from '@vitejs/plugin-react'
import { cpSync, createReadStream, existsSync, statSync } from 'node:fs'
import { join, normalize, resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vite'

const EXCALIDRAW_ASSETS = 'node_modules/@excalidraw/excalidraw/dist/prod'
const ASSET_BASE = '/excalidraw-assets/'
/** Xiaolai only covers Chinese/Japanese characters (~12 MB); without it, such text falls back to a system font. */
const EXCLUDED_FONTS = ['Xiaolai']

const isExcludedFont = (path: string) => EXCLUDED_FONTS.some((font) => path.split(/[\\/]/).includes(font))

/**
 * The app's UI is English only, so replace Excalidraw's other translations (about 1.2 MB) with empty modules.
 * Excalidraw falls back to English for missing texts, so nothing breaks even if one were loaded.
 */
function englishOnly(): Plugin {
  return {
    name: 'excalidraw-english-only',
    enforce: 'pre',
    load(id) {
      const locale = id.match(/@excalidraw[\\/]excalidraw[\\/]dist[\\/](?:prod|dev)[\\/]locales[\\/]([^/\\?]+)\.js$/)?.[1]
      if (locale && !locale.startsWith('en-')) return 'export default {}'
    },
  }
}

/**
 * Bundle Excalidraw's fonts with the app instead of loading them from esm.sh:
 * served from node_modules in dev, copied into dist on build (and from there into the .app).
 * The canvas points Excalidraw at them via window.EXCALIDRAW_ASSET_PATH.
 */
function excalidrawAssets(): Plugin {
  return {
    name: 'excalidraw-assets',
    configureServer(server) {
      server.middlewares.use(ASSET_BASE, (req, res, next) => {
        const file = join(EXCALIDRAW_ASSETS, normalize(decodeURIComponent(req.url?.split('?')[0] ?? '')))
        if (!file.startsWith(join(EXCALIDRAW_ASSETS, 'fonts')) || isExcludedFont(file) || !existsSync(file) || !statSync(file).isFile()) return next()
        res.setHeader('Content-Type', 'font/woff2')
        createReadStream(file).pipe(res)
      })
    },
    closeBundle() {
      cpSync(join(EXCALIDRAW_ASSETS, 'fonts'), join('dist', ASSET_BASE, 'fonts'), {
        recursive: true,
        filter: (source) => !isExcludedFont(source),
      })
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [englishOnly(), react(), excalidrawAssets()],
  resolve: {
    // Excalidraw's file picker helper doesn't open the native panel in WKWebView; see the shim for why.
    alias: { 'browser-fs-access': resolve(import.meta.dirname, 'src/shims/browser-fs-access.ts') },
  },
})
