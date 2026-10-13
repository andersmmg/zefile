import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import {
  CircleNotchIcon,
  KeyIcon,
  ShareNetworkIcon,
  SignInIcon,
  SignOutIcon,
  TrashIcon,
  UserPlusIcon,
  UsersThreeIcon,
  type Icon,
} from '@phosphor-icons/react'

import { Empty } from '../App'
import { api, ApiError, type AuditEntry } from '@/api'
import { formatRelativeTime } from '@/lib/files'
import { Button } from '@/components/ui/button'

// Each action maps to a short human label and an icon. Unknown actions fall back
// to their raw name, so a newer server never renders a blank row.
const ACTIONS: Record<string, { label: string; icon: Icon }> = {
  'auth.login': { label: 'signed in', icon: SignInIcon },
  'auth.logout': { label: 'signed out', icon: SignOutIcon },
  'auth.setup': { label: 'set up the instance', icon: KeyIcon },
  'auth.password_changed': { label: 'changed their password', icon: KeyIcon },
  'user.joined': { label: 'joined', icon: UserPlusIcon },
  'user.updated': { label: 'updated an account', icon: UsersThreeIcon },
  'user.deleted': { label: 'removed an account', icon: UsersThreeIcon },
  'invitation.created': { label: 'invited someone', icon: UserPlusIcon },
  'invitation.revoked': { label: 'revoked an invitation', icon: UserPlusIcon },
  'share.created': { label: 'created a share', icon: ShareNetworkIcon },
  'share.revoked': { label: 'revoked a share', icon: ShareNetworkIcon },
  'access.granted': { label: 'granted access to', icon: KeyIcon },
  'access.revoked': { label: 'revoked an access rule', icon: KeyIcon },
  'group.created': { label: 'created a group', icon: UsersThreeIcon },
  'group.deleted': { label: 'deleted a group', icon: UsersThreeIcon },
  'group.member_added': { label: 'added a group member', icon: UsersThreeIcon },
  'group.member_removed': { label: 'removed a group member', icon: UsersThreeIcon },
  'file.trashed': { label: 'deleted', icon: TrashIcon },
  'trash.restored': { label: 'restored', icon: TrashIcon },
  'trash.purged': { label: 'purged an item from trash', icon: TrashIcon },
  'trash.emptied': { label: 'emptied the trash', icon: TrashIcon },
}

/**
 * ActivityScreen is the administrator's record of what happened on the instance:
 * who signed in, shared, granted access, deleted. It reads the audit log,
 * newest first, a page at a time.
 */
export function ActivityScreen() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [before, setBefore] = useState<number | null>(null) // next cursor; 0/null = end
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)

  const loadFirst = useCallback(async () => {
    setLoading(true)
    try {
      const page = await api.listAudit()
      setEntries(page.entries)
      setBefore(page.next_before || null)
    } catch {
      toast.error('Could not load the activity log.')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadFirst()
  }, [loadFirst])

  async function loadMore() {
    if (!before) return
    setLoadingMore(true)
    try {
      const page = await api.listAudit(before)
      setEntries((cur) => [...cur, ...page.entries])
      setBefore(page.next_before || null)
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Could not load more.')
    } finally {
      setLoadingMore(false)
    }
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <header className="flex h-14 shrink-0 items-center gap-3 border-b px-4">
        <h1 className="font-serif text-xl font-semibold tracking-tight">Activity</h1>
      </header>

      <div className="min-h-0 flex-1 overflow-auto">
        {loading ? (
          <div className="grid h-full place-items-center">
            <CircleNotchIcon className="size-6 animate-spin text-muted-foreground" aria-label="Loading" />
          </div>
        ) : entries.length === 0 ? (
          <Empty title="Nothing yet" detail="Actions on the instance will show up here." />
        ) : (
          <div className="mx-auto max-w-3xl p-4">
            <div className="overflow-hidden rounded-lg border">
              {entries.map((entry) => (
                <ActivityRow key={entry.id} entry={entry} />
              ))}
            </div>
            {before && (
              <div className="mt-3 grid place-items-center">
                <Button variant="outline" size="sm" disabled={loadingMore} onClick={loadMore}>
                  {loadingMore ? 'Loading…' : 'Load more'}
                </Button>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function ActivityRow({ entry }: { entry: AuditEntry }) {
  const known = ACTIONS[entry.action]
  const ActionIcon = known?.icon ?? KeyIcon
  const label = known?.label ?? entry.action

  return (
    <div className="flex items-center gap-3 border-b border-border/60 px-4 py-2.5 last:border-b-0 hover:bg-accent/40">
      <ActionIcon className="size-5 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm">
          <span className="font-medium">{entry.actor || 'Someone'}</span> {label}
          {entry.target && <span className="text-muted-foreground"> · {entry.target}</span>}
        </p>
        <p className="truncate text-xs text-muted-foreground">
          {formatRelativeTime(entry.at)}
          {entry.ip && ` · ${entry.ip}`}
        </p>
      </div>
    </div>
  )
}
