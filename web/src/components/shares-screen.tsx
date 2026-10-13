import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { CircleNotchIcon, LockSimpleIcon } from '@phosphor-icons/react'

import { Empty } from '../App'
import { api, ApiError, type Entry, type Share, type User } from '@/api'
import { entryKind, formatRelativeTime } from '@/lib/files'
import { Button } from '@/components/ui/button'

/** SharesScreen lists public links or incoming shares. */
export function SharesScreen({
  me,
  mode,
  onOpen,
}: {
  me: User
  mode: 'links' | 'incoming'
  onOpen: (path: string) => void
}) {
  const [shares, setShares] = useState<Share[]>([])
  const [incoming, setIncoming] = useState<Entry[]>([])
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [mine, theirs] = await Promise.all([
        api.listShares(),
        me.is_admin ? Promise.resolve({ entries: [] as Entry[] }) : api.sharedWithMe(),
      ])
      setShares(mine.shares)
      setIncoming(theirs.entries)
    } catch {
      toast.error('Could not load sharing.')
    } finally {
      setLoading(false)
    }
  }, [me.is_admin])

  useEffect(() => {
    void load()
  }, [load])

  async function revoke(share: Share) {
    try {
      await api.revokeShare(share.id)
      toast.success(`Link to “${share.name}” revoked`)
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Could not revoke this link.')
    }
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <header className="flex h-14 shrink-0 items-center gap-3 border-b px-4">
        <h1 className="font-serif text-xl font-semibold tracking-tight">
          {mode === 'incoming' ? 'Shared with me' : 'Shares'}
        </h1>
      </header>

      <div className="min-h-0 flex-1 overflow-auto">
        {loading ? (
          <div className="grid h-full place-items-center">
            <CircleNotchIcon className="size-6 animate-spin text-muted-foreground" aria-label="Loading" />
          </div>
        ) : mode === 'incoming' ? (
          incoming.length === 0 ? (
            <Empty title="Nothing shared with you" detail="Folders others give you access to appear here." />
          ) : (
            <div>
              {incoming.map((entry) => (
                <IncomingRow key={entry.path} entry={entry} onOpen={() => onOpen(entry.path)} />
              ))}
            </div>
          )
        ) : shares.length === 0 ? (
          <Empty title="No shared links" detail="Create a public link from a file's right-click menu." />
        ) : (
          <div>
            {shares.map((share) => (
              <ShareRow key={share.id} share={share} onRevoke={() => revoke(share)} />
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

function IncomingRow({ entry, onOpen }: { entry: Entry; onOpen: () => void }) {
  const { icon: Icon, color } = entryKind(entry)

  return (
    <button
      type="button"
      onClick={onOpen}
      className="flex h-14 w-full items-center gap-3 border-b border-border/60 px-4 text-left hover:bg-accent/40"
    >
      <Icon className={`size-5 shrink-0 ${color}`} />
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm">{entry.name}</p>
        <p className="truncate text-xs text-muted-foreground">{entry.path}</p>
      </div>
    </button>
  )
}

function ShareRow({ share, onRevoke }: { share: Share; onRevoke: () => void }) {
  const { icon: Icon, color } = entryKind({ name: share.name, is_dir: false } as Entry)
  const downloads = `${share.download_count} download${share.download_count === 1 ? '' : 's'}`
  const expiry = share.expires_at ? `expires ${formatRelativeTime(share.expires_at)}` : 'no expiry'

  return (
    <div className="flex h-14 items-center gap-3 border-b border-border/60 px-4 hover:bg-accent/40">
      <Icon className={`size-5 shrink-0 ${color}`} />
      <div className="min-w-0 flex-1">
        <p className="flex items-center gap-1.5 truncate text-sm">
          {share.has_password && (
            <LockSimpleIcon className="size-3.5 shrink-0 text-muted-foreground" aria-label="Password-protected" />
          )}
          <span className="truncate">{share.name}</span>
        </p>
        <p className="truncate text-xs text-muted-foreground">
          {expiry} · {downloads}
        </p>
      </div>
      <Button
        variant="ghost"
        size="sm"
        className="shrink-0 text-muted-foreground hover:text-destructive"
        onClick={onRevoke}
      >
        Revoke
      </Button>
    </div>
  )
}
