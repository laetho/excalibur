import { useEffect, useState } from 'react'

/** A remote collaborator drawn in canvas pixels; `name` is empty when only the avatar is shown. */
export type RemoteCursorData = {
  id: string
  x: number
  y: number
  emoji: string
  name: string
  stroke: string
  background: string
  down: boolean
}

/**
 * Cursors of collaborators that have an avatar. Excalidraw draws cursors and name tags on its canvas and
 * can't put an avatar in them, so for these peers Excalidraw's cursor is turned off and this overlay draws
 * the whole thing: the arrow and a label holding the avatar and, unless hidden, the name.
 * The overlay owns its state and is driven through `register`, so a remote cursor moving never re-renders the whole canvas.
 */
export function RemoteCursors({ register }: { register: (update: ((items: RemoteCursorData[]) => void) | null) => void }) {
  const [items, setItems] = useState<RemoteCursorData[]>([])
  useEffect(() => {
    register(setItems)
    return () => register(null)
  }, [register])
  if (items.length === 0) return null
  return (
    <div className="remote-cursors" aria-hidden="true">
      {items.map((item) => (
        <RemoteCursor key={item.id} item={item} />
      ))}
    </div>
  )
}

/** One cursor, positioned by `item.x`/`item.y`; also used for the settings preview. */
export function RemoteCursor({ item }: { item: RemoteCursorData }) {
  return (
    <div className="remote-cursor" style={{ transform: `translate(${item.x}px, ${item.y}px)` }}>
      {item.down && <span className="remote-cursor-press" style={{ borderColor: item.stroke }} />}
      <svg className="remote-cursor-arrow" width="16" height="20" viewBox="-2 -2 16 20">
        <path d="M0 0 L0 14 L4 9 L11 8 Z" fill={item.stroke} stroke="#ffffff" strokeWidth="2" strokeLinejoin="round" paintOrder="stroke" />
      </svg>
      {/* One label for every case; with no name it shrinks to a circle around the avatar. */}
      <span
        className={`remote-cursor-label${item.name ? '' : ' icon-only'}`}
        style={{ background: item.background, borderColor: item.stroke }}
      >
        {item.emoji && <span className="remote-cursor-emoji">{item.emoji}</span>}
        {item.name && <span className="remote-cursor-name">{item.name}</span>}
      </span>
    </div>
  )
}
