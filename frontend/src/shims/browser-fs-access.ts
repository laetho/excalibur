/**
 * Stand-in for the `browser-fs-access` package that Excalidraw uses to open and save files
 * (wired up with an alias in vite.config.ts).
 *
 * Inside the Wails app, opening and saving goes through native dialogs (`PickFile` / `SaveFile`
 * commands). Outside the app (browser dev/tests) it falls back to a hidden `<input type="file">` click,
 * done synchronously inside the user's gesture so WebKit shows its open panel.
 */

import { pickFile, saveFile } from '../ipc'

type FileOpenOptions = {
  description?: string
  mimeTypes?: string[]
  extensions?: string[]
  multiple?: boolean
  legacySetup?: (resolve: (value: File | File[]) => void, reject: () => void, input: HTMLInputElement) => unknown
}

type FileSaveOptions = {
  fileName?: string
  extensions?: string[]
  description?: string
  mimeTypes?: string[]
}

/** Fired with the chosen file's name, so the canvas can show progress (e.g. "Importing library…"). */
export const FILE_CHOSEN_EVENT = 'whiteboards:file-chosen'

export const supported = false

const inWails = typeof window !== 'undefined' && Boolean(window._wails?.environment)

function announce(names: string[]) {
  window.dispatchEvent(new CustomEvent(FILE_CHOSEN_EVENT, { detail: names }))
}

function dataUrlToFile(dataUrl: string, name: string): File {
  const [header, b64] = dataUrl.split(',')
  const mime = header.slice(5).split(';')[0] || 'application/octet-stream'
  const binary = atob(b64 ?? '')
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return new File([bytes], name, { type: mime })
}

function openViaInput(options: FileOpenOptions): Promise<File | File[]> {
  return new Promise((resolve, reject) => {
    const input = document.createElement('input')
    input.type = 'file'
    input.multiple = options.multiple ?? false
    input.accept = [...(options.mimeTypes ?? []), ...(options.extensions ?? [])].join()

    const done = (value: File | File[]) => {
      const files = Array.isArray(value) ? value : [value]
      announce(files.map((f) => f.name))
      resolve(value)
    }
    input.addEventListener('change', () => {
      const files = Array.from(input.files ?? [])
      if (files.length > 0) done(input.multiple ? files : files[0])
    })
    input.addEventListener('cancel', () => reject(new DOMException('The user aborted a request.', 'AbortError')))
    // Must stay synchronous: WebKit only shows the open panel for a click inside a user gesture.
    input.click()
  })
}

export function fileOpen(options: FileOpenOptions | FileOpenOptions[] = [{}]): Promise<File | File[]> {
  const list = Array.isArray(options) ? options : [options]
  const first = list[0] ?? {}

  if (!inWails) return openViaInput(first)

  // Native dialog via Go; the extensions are what Excalidraw asked for (images, library files, …).
  const extensions = [...new Set(list.flatMap((o) => [...(o.extensions ?? []), ...(o.mimeTypes ?? []).map(mimeToExtension)]))]
  return pickFile(extensions).then((result) => {
    if (!result) throw new DOMException('The user aborted a request.', 'AbortError')
    announce([result.name])
    const file = dataUrlToFile(result.dataUrl, result.name)
    return first.multiple ? [file] : file
  })
}

function mimeToExtension(mime: string): string {
  const known: Record<string, string> = {
    'image/png': 'png',
    'image/jpeg': 'jpg',
    'image/gif': 'gif',
    'image/webp': 'webp',
    'image/svg+xml': 'svg',
    'image/bmp': 'bmp',
    'application/json': 'json',
  }
  return known[mime] ?? mime.split('/')[1] ?? ''
}

function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error ?? new Error('Could not read the image'))
    reader.readAsDataURL(blob)
  })
}

export async function fileSave(
  blobOrPromise: Blob | Promise<Blob>,
  options: FileSaveOptions | FileSaveOptions[] = {},
): Promise<null> {
  const first = Array.isArray(options) ? (options[0] ?? {}) : options
  const blob = await blobOrPromise

  if (!inWails) {
    // Browser fallback: a download link.
    const link = document.createElement('a')
    link.download = first.fileName ?? 'Untitled'
    link.href = URL.createObjectURL(blob)
    link.click()
    setTimeout(() => URL.revokeObjectURL(link.href), 30_000)
    return null
  }

  const dataUrl = await blobToDataUrl(blob)
  await saveFile(first.fileName ?? 'Untitled', dataUrl)
  return null
}
