/** How a collaborator looks to others: cursor color and avatar. Shared by the settings and the canvas. */

export type PeerColor = { background: string; stroke: string }

/** Colors given to peers that did not pick one, chosen by peer id. */
export const PEER_COLORS: PeerColor[] = [
  { background: '#ffc9c9', stroke: '#e03131' },
  { background: '#a5d8ff', stroke: '#1971c2' },
  { background: '#b2f2bb', stroke: '#2f9e44' },
  { background: '#ffec99', stroke: '#f08c00' },
  { background: '#d0bfff', stroke: '#6741d9' },
  { background: '#ffdeeb', stroke: '#e64980' },
]

export const COLOR_PATTERN = /^#[0-9a-f]{6}$/i

/** Swatches offered in the settings; any other color can be picked with the color input. */
export const COLOR_PRESETS = [...PEER_COLORS.map((c) => c.stroke), '#0c8599', '#343a40']

/** A chosen color as Excalidraw's cursor color: the color itself plus a light tint of it. */
export function peerColor(peerId: string, chosen?: string): PeerColor {
  if (chosen && COLOR_PATTERN.test(chosen)) {
    const channels = [1, 3, 5].map((i) => parseInt(chosen.slice(i, i + 2), 16))
    const tint = channels.map((c) => Math.round(c + (255 - c) * 0.75).toString(16).padStart(2, '0')).join('')
    return { background: `#${tint}`, stroke: chosen.toLowerCase() }
  }
  return PEER_COLORS[peerId.charCodeAt(0) % PEER_COLORS.length]
}

/** Avatars to choose from. Any non-ASCII emoji sent by a peer is shown, but only these are offered. */
export const AVATAR_EMOJIS = [
  '🦊', '🐼', '🐨', '🦁', '🐯', '🐸', '🐵', '🐙',
  '🦄', '🐢', '🦉', '🐧', '🦋', '🐝', '🐳', '🦈',
  '🐺', '🦖', '🐉', '🦔', '🐶', '🐱', '🐰', '🦝',
  '🗡️', '👑', '🧙', '🤖', '👻', '🚀', '🔥', '⭐',
  '🌈', '🌵', '🍕', '🎸', '🎨', '💡', '🧠', '🍀',
]

/** Peers are untrusted: accept only a short run of non-ASCII characters (no markup, no text). */
export function isAvatarEmoji(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const characters = Array.from(value)
  return characters.length > 0 && characters.length <= 24 && characters.every((character) => (character.codePointAt(0) ?? 0) > 0x7f)
}

/** An emoji as an image, for places that only take an image URL (Excalidraw's user list). */
export function avatarImageUrl(emoji: string): string {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><text x="32" y="47" font-size="44" text-anchor="middle">${emoji}</text></svg>`
  return `data:image/svg+xml,${encodeURIComponent(svg)}`
}
