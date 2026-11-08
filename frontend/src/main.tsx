import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './index.css'

// Excalidraw's fonts are bundled with the app (see vite.config.ts) and served from the app's own origin.
window.EXCALIDRAW_ASSET_PATH = '/excalidraw-assets/'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
