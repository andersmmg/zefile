import { useCallback, useEffect, useState, type SubmitEvent } from 'react'
import { toast } from 'sonner'
import {
  CheckIcon,
  CircleNotchIcon,
  CopyIcon,
  DotsThreeIcon,
  EnvelopeSimpleIcon,
  UserPlusIcon,
} from '@phosphor-icons/react'

import { Empty } from '../App'
import { api, ApiError, formatSize, type Invitation, type User, type UserSummary } from '@/api'
import { GroupsSection } from '@/components/groups-section'
import { formatRelativeTime } from '@/lib/files'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'

function inviteLink(token: string): string {
  return `${location.origin}/invite?token=${encodeURIComponent(token)}`
}

/** MembersScreen manages accounts and invitations. */
export function MembersScreen({ me }: { me: User }) {
  const [users, setUsers] = useState<UserSummary[]>([])
  const [invitations, setInvitations] = useState<Invitation[]>([])
  const [loading, setLoading] = useState(true)
  const [inviting, setInviting] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<UserSummary | null>(null)
  const [quotaFor, setQuotaFor] = useState<UserSummary | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [people, invites] = await Promise.all([api.listUsers(), api.listInvitations()])
      setUsers(people.users)
      setInvitations(invites.invitations)
    } catch {
      toast.error('Could not load members.')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function update(user: UserSummary, patch: { is_admin?: boolean; disabled?: boolean; quota_bytes?: number }) {
    try {
      await api.updateUser(user.id, patch)
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Could not update this account.')
    }
  }

  async function remove(user: UserSummary) {
    try {
      await api.deleteUser(user.id)
      toast.success(`“${user.username}” removed`)
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Could not remove this account.')
    } finally {
      setConfirmDelete(null)
    }
  }

  async function revoke(invitation: Invitation) {
    try {
      await api.revokeInvitation(invitation.id)
      toast.success('Invitation revoked')
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Could not revoke this invitation.')
    }
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <header className="flex h-14 shrink-0 items-center gap-3 border-b px-4">
        <h1 className="font-serif text-xl font-semibold tracking-tight">Members</h1>
        <Button className="ml-auto gap-2" size="sm" onClick={() => setInviting(true)}>
          <UserPlusIcon />
          Invite
        </Button>
      </header>

      <div className="min-h-0 flex-1 overflow-auto">
        {loading ? (
          <div className="grid h-full place-items-center">
            <CircleNotchIcon className="size-6 animate-spin text-muted-foreground" aria-label="Loading" />
          </div>
        ) : (
          <div className="mx-auto max-w-3xl p-4">
            <section>
              <h2 className="mb-2 px-1 text-base font-semibold text-foreground">
                Accounts
              </h2>
              <div className="overflow-hidden rounded-lg border">
                {users.map((user) => (
                  <UserRow
                    key={user.id}
                    user={user}
                    isSelf={user.id === me.id}
                    onUpdate={(patch) => update(user, patch)}
                    onDelete={() => setConfirmDelete(user)}
                    onQuota={() => setQuotaFor(user)}
                  />
                ))}
              </div>
            </section>

            <section className="mt-6">
              <GroupsSection users={users} />
            </section>

            <section className="mt-6">
              <h2 className="mb-2 px-1 text-base font-semibold text-foreground">
                Pending invitations
              </h2>
              {invitations.length === 0 ? (
                <Empty
                  title="No pending invitations"
                  detail="Invite someone to create an account with a one-time link."
                />
              ) : (
                <div className="overflow-hidden rounded-lg border">
                  {invitations.map((invitation) => (
                    <InvitationRow
                      key={invitation.id}
                      invitation={invitation}
                      onRevoke={() => revoke(invitation)}
                    />
                  ))}
                </div>
              )}
            </section>
          </div>
        )}
      </div>

      {inviting && (
        <InviteDialog
          onClose={() => setInviting(false)}
          onCreated={() => {
            void load()
          }}
        />
      )}

      {quotaFor && (
        <QuotaDialog
          user={quotaFor}
          onClose={() => setQuotaFor(null)}
          onSave={(quotaBytes) => {
            setQuotaFor(null)
            void update(quotaFor, { quota_bytes: quotaBytes })
          }}
        />
      )}

      {confirmDelete && (
        <AlertDialog open onOpenChange={(open) => !open && setConfirmDelete(null)}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Remove “{confirmDelete.username}”?</AlertDialogTitle>
              <AlertDialogDescription>
                Their account, sessions and permission rules are deleted. Files they uploaded stay, but lose
                their owner. This cannot be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={() => remove(confirmDelete)}>Remove account</AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}

function UserRow({
  user,
  isSelf,
  onUpdate,
  onDelete,
  onQuota,
}: {
  user: UserSummary
  isSelf: boolean
  onUpdate: (patch: { is_admin?: boolean; disabled?: boolean; quota_bytes?: number }) => void
  onDelete: () => void
  onQuota: () => void
}) {
  return (
    <div className="flex h-14 items-center gap-3 border-b border-border/60 px-4 last:border-b-0 hover:bg-accent/40">
      <div className="grid size-8 shrink-0 place-items-center rounded-full bg-primary text-sm font-medium text-primary-foreground">
        {user.username.slice(0, 1).toUpperCase()}
      </div>
      <div className="min-w-0 flex-1">
        <p className="flex items-center gap-1.5 truncate text-sm">
          {user.username}
          {isSelf && <span className="text-xs text-muted-foreground">(you)</span>}
        </p>
        <p className="truncate text-xs text-muted-foreground">
          {user.is_admin ? 'Administrator' : 'Member'}
          {user.disabled && ' · disabled'} · {formatSize(user.used_bytes)} used
          {user.quota_bytes > 0 ? ` · limit ${formatSize(user.quota_bytes)}` : ' · no limit'}
        </p>
      </div>

      {!isSelf && (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8 shrink-0" aria-label="Account actions">
              <DotsThreeIcon weight="bold" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-44">
            <DropdownMenuItem onSelect={() => onUpdate({ is_admin: !user.is_admin })}>
              {user.is_admin ? 'Remove admin' : 'Make admin'}
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onUpdate({ disabled: !user.disabled })}>
              {user.disabled ? 'Enable' : 'Disable'}
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={onQuota}>Set storage limit…</DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onSelect={onDelete}>
              Remove account
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      )}
    </div>
  )
}

function InvitationRow({ invitation, onRevoke }: { invitation: Invitation; onRevoke: () => void }) {
  return (
    <div className="flex h-14 items-center gap-3 border-b border-border/60 px-4 last:border-b-0 hover:bg-accent/40">
      <EnvelopeSimpleIcon className="size-5 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm">{invitation.email || 'Anyone with the link'}</p>
        <p className="truncate text-xs text-muted-foreground">
          invited {formatRelativeTime(invitation.created_at)} · expires{' '}
          {formatRelativeTime(invitation.expires_at)}
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

function parseBytesInput(raw: string): number | null {
  const t = raw.trim().toUpperCase()
  if (t === '' || t === '0' || t === 'UNLIMITED') return 0
  const m = /^(\d+(?:\.\d+)?)\s*(B|KB|KIB|MB|MIB|GB|GIB)?$/.exec(t)
  if (!m) return null
  const mult: Record<string, number> = {
    B: 1,
    KB: 1 << 10,
    KIB: 1 << 10,
    MB: 1 << 20,
    MIB: 1 << 20,
    GB: 1 << 30,
    GIB: 1 << 30,
  }
  const n = Math.floor(parseFloat(m[1] ?? '') * (mult[m[2] ?? 'B'] ?? 1))
  return Number.isSafeInteger(n) && n >= 0 ? n : null
}

function QuotaDialog({
  user,
  onClose,
  onSave,
}: {
  user: UserSummary
  onClose: () => void
  onSave: (quotaBytes: number) => void
}) {
  const [value, setValue] = useState(user.quota_bytes > 0 ? formatSize(user.quota_bytes) : '')
  const [error, setError] = useState('')

  function submit(e: SubmitEvent) {
    e.preventDefault()
    const n = parseBytesInput(value)
    if (n === null) {
      setError('Enter a byte count like 512MiB, or leave it empty for no limit.')
      return
    }
    onSave(n)
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Storage limit for “{user.username}”</DialogTitle>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3">
          <p className="text-sm text-muted-foreground">
            Currently using {formatSize(user.used_bytes)}. Uploads past the limit are refused with a
            “storage full” error.
          </p>
          <div className="space-y-1.5">
            <Label htmlFor="quota">Limit</Label>
            <Input
              id="quota"
              value={value}
              onChange={(e) => {
                setValue(e.target.value)
                setError('')
              }}
              placeholder="Unlimited"
              autoFocus
            />
            {error ? (
              <p className="text-xs text-destructive">{error}</p>
            ) : (
              <p className="text-xs text-muted-foreground">Bytes with an optional KiB/MiB/GiB suffix.</p>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit">Save limit</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function InviteDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [email, setEmail] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [link, setLink] = useState('')
  const [copied, setCopied] = useState(false)

  async function create(event: SubmitEvent) {
    event.preventDefault()
    setBusy(true)
    setErr('')
    try {
      const invitation = await api.createInvitation(email.trim())
      setLink(invitation.token ? inviteLink(invitation.token) : '')
      onCreated()
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'Could not create the invitation.')
    } finally {
      setBusy(false)
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(link)
      setCopied(true)
      toast.success('Invite link copied')
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      toast.error('Could not copy the link.')
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Invite someone</DialogTitle>
        </DialogHeader>

        {link ? (
          <div className="space-y-3 py-2">
            <p className="text-sm text-muted-foreground">
              Send this link to whoever you’re inviting. It works once and expires in 7 days — copy it now, it
              is shown only here.
            </p>
            <div className="flex gap-2">
              <Input
                readOnly
                value={link}
                onFocus={(e) => e.currentTarget.select()}
                className="font-mono text-xs"
              />
              <Button type="button" variant="secondary" className="shrink-0" onClick={copy}>
                {copied ? <CheckIcon /> : <CopyIcon />}
                Copy
              </Button>
            </div>
            <DialogFooter>
              <Button onClick={onClose}>Done</Button>
            </DialogFooter>
          </div>
        ) : (
          <form onSubmit={create} className="space-y-4 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="invite-email">Email (optional)</Label>
              <Input
                id="invite-email"
                type="email"
                placeholder="name@example.com"
                value={email}
                onChange={(e) => setEmail(e.currentTarget.value)}
              />
              <p className="text-xs text-muted-foreground">
                A note for your own records — the person still picks their own username.
              </p>
            </div>

            {err && <p className="text-sm text-destructive">{err}</p>}

            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>
                Cancel
              </Button>
              <Button type="submit" disabled={busy}>
                {busy ? 'Creating…' : 'Create link'}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}
