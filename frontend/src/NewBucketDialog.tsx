import { useEffect, useRef, useState } from 'react'
import { activateSource, addBucket, type SourceInfo } from './ipc'

/**
 * Dialog for creating a bucket on a NATS server (boards are stored in buckets).
 *
 * Shared by the sidebar tree and Morgan le Fayl, which adds folders to a remote
 * server from inside the file manager. `onCreated` lets the caller decide what
 * happens next: the sidebar opens the new bucket, while the file manager points
 * one of its panes at it.
 */
export default function NewBucketDialog({
  source,
  note,
  onClose,
  onCreated,
}: {
  source: SourceInfo
  /** A standing note shown above the input, e.g. why a "folder" becomes a bucket. */
  note?: string
  onClose: () => void
  onCreated?: (bucket: string) => void | Promise<void>
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const trimmed = name.trim()
  // Mirrors BoardStore::bucket_name_problem (JetStream Object Store bucket names).
  const problem = !trimmed
    ? 'Enter a bucket name.'
    : !/^[A-Za-z0-9_-]+$/.test(trimmed)
      ? 'Bucket names can only contain letters, digits, “-” and “_”.'
      : trimmed.length > 64
        ? 'That bucket name is too long.'
        : source.groups.some((g) => g.id === trimmed)
          ? 'That bucket already exists.'
          : null

  const confirm = async () => {
    if (problem || busy) return
    setBusy(true)
    setError(null)
    try {
      await addBucket(source.id, trimmed)
      if (onCreated) await onCreated(trimmed)
      else
        // The sidebar's behaviour: open the new bucket so the next "New Board"
        // goes there.
        await activateSource(source.id, trimmed)
      onClose()
    } catch (err) {
      setError(String(err))
      setBusy(false)
    }
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose()
      }}
    >
      <div className="modal" role="dialog" aria-modal="true">
        <h2>New Bucket on {source.name}</h2>
        {note && <p className="modal-note">{note}</p>}
        <input
          ref={inputRef}
          value={name}
          placeholder="e.g. team-boards"
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') confirm()
            if (event.key === 'Escape' && !busy) onClose()
          }}
        />
        <p className={error || (trimmed && problem) ? 'problem' : 'hint'}>
          {error ?? (trimmed && problem) ?? 'Boards in a bucket are shared with everyone using this server.'}
        </p>
        <div className="modal-actions">
          <button className="secondary" disabled={busy} onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={problem !== null || busy} onClick={confirm}>
            {busy ? 'Creating…' : 'Create Bucket'}
          </button>
        </div>
      </div>
    </div>
  )
}
