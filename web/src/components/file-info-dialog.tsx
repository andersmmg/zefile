import { useEffect, useState, type ReactNode } from 'react'
import { CircleNotchIcon } from '@phosphor-icons/react'

import { api, ApiError, formatSize, parentOf, type Entry, type FileInfo } from '@/api'
import { canThumb, categoryLabel, entryKind } from '@/lib/files'
import { LinkIcon } from '@phosphor-icons/react'
import { COLORS } from '@/lib/colors'
import { Thumbnail } from '@/components/thumbnail'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'

// renders a timestamp the way a details panel does
function when(iso: string): string {
  const at = new Date(iso)
  if (Number.isNaN(at.getTime()) || at.getTime() <= 0) return 'Unknown'
  return at.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="shrink-0 text-muted-foreground">{label}</dt>
      <dd className="truncate text-right">{children}</dd>
    </div>
  )
}

/**
 * file details panel
 */
export function FileInfoDialog({ entry, onClose }: { entry: Entry; onClose: () => void }) {
  const [info, setInfo] = useState<FileInfo | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let live = true
    setInfo(null)
    setError('')
    api
      .info(entry.path)
      .then((i) => {
        if (live) setInfo(i)
      })
      .catch((err) => {
        if (live) setError(err instanceof ApiError ? err.message : 'Could not read this file’s details.')
      })
    return () => {
      live = false
    }
  }, [entry.path])

  const kind = entryKind(entry)
  const Icon = kind.icon
  const tint = entry.color ? COLORS[entry.color] : undefined
  const location = parentOf(entry.path)

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="pr-8 break-words">{entry.name}</DialogTitle>
          {entry.ref && (
            <p
              className="flex items-center gap-1.5 text-sm text-muted-foreground"
              title={`This is a link to ${entry.ref}`}
            >
              <LinkIcon className="size-4 shrink-0" aria-hidden />
              <span className="truncate">Link to {entry.ref}</span>
            </p>
          )}
        </DialogHeader>

        {canThumb(entry) ? (
          <Thumbnail
            entry={entry}
            size={512}
            className="mx-auto grid h-40 w-40 place-items-center overflow-hidden rounded-lg border bg-muted/40"
          >
            <Icon className={`size-12 ${kind.color}`} />
          </Thumbnail>
        ) : (
          <div className="mx-auto grid h-40 w-40 place-items-center rounded-lg border bg-muted/40">
            <Icon className={`size-12 ${kind.color}`} />
          </div>
        )}

        {error && <p className="text-sm text-destructive">{error}</p>}

        {!info && !error && (
          <div className="grid place-items-center py-4">
            <CircleNotchIcon className="size-5 animate-spin text-muted-foreground" aria-label="Loading" />
          </div>
        )}

        {info && !error && (
          <dl className="space-y-1.5 text-sm">
            <Row label="Type">{entry.is_dir ? 'Folder' : categoryLabel(entry)}</Row>
            {!entry.is_dir && <Row label="Size">{formatSize(info.size)}</Row>}
            <Row label="Location">{location === '/' ? 'Home' : location.slice(1)}</Row>
            <Row label="Modified">{when(info.mod_time)}</Row>
            <Row label="Added">{info.added_at ? when(info.added_at) : 'Unknown'}</Row>
            <Row label="Owner">{info.owner ?? 'Unknown'}</Row>
            {tint && (
              <Row label="Colour">
                <span className="inline-flex items-center gap-1.5">
                  <span className="size-2.5 rounded-full ring-1 ring-border" style={{ background: tint.hex }} aria-hidden />
                  {tint.label}
                </span>
              </Row>
            )}
          </dl>
        )}
      </DialogContent>
    </Dialog>
  )
}
