/**
 * Members — org membership management (REGISTRY-DESIGN §5.3).
 *
 * Owned by: members-tokens sub-agent (web/src/pages/settings/**)
 *
 * Design: instrument-panel density, two cards — members table + org info
 * readout. Admin column (role select / remove) only renders for org admins.
 * The last-owner guard is a disabled action with an inline explanation, never
 * a server error the user has to decode.
 *
 * API consumed:
 *   listMembers(orgId)           → MembersResponse
 *   addMember(orgId, {…})        → MemberDTO
 *   patchMember(orgId, email, {…}) → MemberDTO
 *   removeMember(orgId, email)   → 204
 */
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import { AlertCircle, Trash2, UserPlus } from 'lucide-react';

import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { addMember, listMembers, patchMember, removeMember } from '@/api/client';
import type { MemberDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle, Readout } from '@/components/ui/card';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { SkeletonRows } from '@/components/ui/skeleton';
import {
  EmptyRow,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { useToast } from '@/hooks/use-toast';
import { cn, formatRelative } from '@/lib/utils';

// ── Role helpers ──────────────────────────────────────────────────────────────

const ROLES = ['viewer', 'editor', 'admin', 'owner'] as const;
type Role = (typeof ROLES)[number];

/** Convert an RFC3339 string to a human relative age ("3d ago"). */
function isoRelative(iso: string | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return formatRelative(Math.floor(d.getTime() / 1000));
}

/** Short human date "Jan 3, 2025" */
function isoDate(iso: string | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

/** Inline role text — amber for owner, descending neutral otherwise. */
function RoleLabel({ role }: { role: string }) {
  const cls =
    {
      owner: 'text-brand',
      admin: 'text-slate-200',
      editor: 'text-slate-300',
      viewer: 'text-slate-400',
    }[role] ?? 'text-slate-400';
  return <span className={cn('text-data font-medium', cls)}>{role}</span>;
}

// ── Main component ────────────────────────────────────────────────────────────

export function Members() {
  const { user } = useAuth();
  const { activeOrg, canAdminOrg, loading: orgLoading } = useOrg();

  const [members, setMembers] = useState<MemberDTO[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');

  // Add member dialog
  const [addOpen, setAddOpen] = useState(false);
  const [addEmail, setAddEmail] = useState('');
  const [addRole, setAddRole] = useState<Role>('viewer');
  const [addBusy, setAddBusy] = useState(false);
  const [addErr, setAddErr] = useState('');

  // Remove confirm dialog
  const [removeTarget, setRemoveTarget] = useState<MemberDTO | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);

  // Inline role-change busy key (keyed by email)
  const [roleBusy, setRoleBusy] = useState<string | null>(null);

  const { toast } = useToast();

  // ── Data loading ───────────────────────────────────────────────────────────

  const load = useCallback(() => {
    if (!activeOrg) {
      setLoading(false);
      return;
    }
    setLoading(true);
    setErr('');
    listMembers(activeOrg.id)
      .then((r) => setMembers(r.members ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [activeOrg]);

  useEffect(() => {
    load();
  }, [load]);

  // ── Derived state ──────────────────────────────────────────────────────────

  const ownerCount = members.filter((m) => m.role === 'owner').length;
  const isLastOwner = (m: MemberDTO) => m.role === 'owner' && ownerCount === 1;
  const isSelf = (m: MemberDTO) => m.email === user?.email;

  // An admin may change roles on rows that are NOT the last owner and NOT self.
  const canEdit = (m: MemberDTO) => canAdminOrg && !isLastOwner(m) && !isSelf(m);

  // ── Mutations ──────────────────────────────────────────────────────────────

  const handleRoleChange = async (m: MemberDTO, newRole: string) => {
    if (!activeOrg) return;
    setRoleBusy(m.email);
    try {
      const updated = await patchMember(activeOrg.id, m.email, { role: newRole });
      setMembers((prev) => prev.map((x) => (x.email === updated.email ? updated : x)));
      toast({
        variant: 'success',
        title: 'Role updated',
        description: `${m.email} → ${newRole}`,
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Role change failed',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setRoleBusy(null);
    }
  };

  const handleAdd = async (e: FormEvent) => {
    e.preventDefault();
    if (!activeOrg) return;
    setAddBusy(true);
    setAddErr('');
    try {
      const member = await addMember(activeOrg.id, { email: addEmail, role: addRole });
      setMembers((prev) => [...prev, member]);
      toast({ variant: 'success', title: 'Member added', description: addEmail });
      closeAddDialog();
    } catch (e: unknown) {
      setAddErr(e instanceof Error ? e.message : String(e));
    } finally {
      setAddBusy(false);
    }
  };

  const handleRemove = async () => {
    if (!removeTarget || !activeOrg) return;
    setRemoveBusy(true);
    try {
      await removeMember(activeOrg.id, removeTarget.email);
      setMembers((prev) => prev.filter((m) => m.email !== removeTarget.email));
      toast({
        variant: 'success',
        title: 'Member removed',
        description: removeTarget.email,
      });
      setRemoveTarget(null);
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Remove failed',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setRemoveBusy(false);
    }
  };

  const closeAddDialog = () => {
    setAddOpen(false);
    setAddEmail('');
    setAddRole('viewer');
    setAddErr('');
  };

  // ── Guard: no active org ──────────────────────────────────────────────────

  if (!orgLoading && !activeOrg) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent className="p-3 text-data text-slate-400">
            No active organization. Select or create one from the switcher above.
          </CardContent>
        </Card>
      </div>
    );
  }

  // ── Render ─────────────────────────────────────────────────────────────────

  return (
    <div className="space-y-3">
      {/* Page heading + primary action */}
      <div className="flex items-start justify-between gap-4">
        <PageHeading orgSlug={activeOrg?.slug} />
        {canAdminOrg && (
          <Button variant="default" size="sm" onClick={() => setAddOpen(true)}>
            <UserPlus />
            Add member
          </Button>
        )}
      </div>

      {/* ── Members table ────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>Members</CardTitle>
          {!loading && (
            <span className="tnum text-data text-slate-400">
              {members.length} member{members.length !== 1 ? 's' : ''}
            </span>
          )}
        </CardHeader>
        <CardContent className="p-0">
          {loading ? (
            <div className="p-3">
              <SkeletonRows rows={5} />
            </div>
          ) : err ? (
            <p className="flex items-center gap-1.5 p-3 text-data text-destructive">
              <AlertCircle className="size-3.5 shrink-0" />
              {err}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Email</TableHead>
                  <TableHead className="w-36">Role</TableHead>
                  <TableHead className="w-28">Joined</TableHead>
                  {canAdminOrg && <TableHead className="w-24" />}
                </TableRow>
              </TableHeader>
              <TableBody>
                {members.length === 0 ? (
                  <EmptyRow colSpan={canAdminOrg ? 4 : 3}>No members yet.</EmptyRow>
                ) : (
                  members.map((m) => {
                    const lastOwner = isLastOwner(m);
                    const self = isSelf(m);
                    const editable = canEdit(m);
                    return (
                      <TableRow key={m.email}>
                        {/* Email */}
                        <TableCell>
                          <span className="text-slate-100">{m.email}</span>
                          {self && (
                            <span className="ml-2 text-micro text-slate-500">(you)</span>
                          )}
                        </TableCell>

                        {/* Role: select for admins on editable rows, text otherwise */}
                        <TableCell>
                          {editable ? (
                            <Select
                              value={m.role}
                              onValueChange={(r) => {
                                void handleRoleChange(m, r);
                              }}
                              disabled={roleBusy === m.email}
                            >
                              <SelectTrigger
                                className="h-6 w-auto min-w-[6.5rem] border-slate-700 bg-transparent"
                                aria-label={`Role for ${m.email}`}
                              >
                                <SelectValue />
                              </SelectTrigger>
                              <SelectContent>
                                {ROLES.map((r) => (
                                  <SelectItem key={r} value={r}>
                                    {r}
                                  </SelectItem>
                                ))}
                              </SelectContent>
                            </Select>
                          ) : (
                            <RoleLabel role={m.role} />
                          )}
                        </TableCell>

                        {/* Joined */}
                        <TableCell className="text-slate-400">
                          <span
                            className="tnum"
                            title={isoDate(m.created_at)}
                          >
                            {isoRelative(m.created_at)}
                          </span>
                        </TableCell>

                        {/* Actions — admin column only */}
                        {canAdminOrg && (
                          <TableCell>
                            {lastOwner ? (
                              /* Last-owner guard: disabled + inline explanation */
                              <span
                                className="text-micro text-slate-500"
                                title="Last owner — cannot be removed or demoted. Transfer ownership to another member first."
                              >
                                last owner
                              </span>
                            ) : self ? (
                              /* No self-remove */
                              <span className="text-micro text-slate-500" title="Cannot remove yourself.">
                                —
                              </span>
                            ) : (
                              <Button
                                variant="ghost"
                                size="icon"
                                className="size-6 text-slate-500 hover:text-destructive focus-visible:text-destructive"
                                onClick={() => setRemoveTarget(m)}
                                aria-label={`Remove ${m.email}`}
                                title={`Remove ${m.email}`}
                              >
                                <Trash2 className="size-3" />
                              </Button>
                            )}
                          </TableCell>
                        )}
                      </TableRow>
                    );
                  })
                )}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* ── Org info readout ─────────────────────────────────────────────── */}
      {activeOrg && (
        <Card>
          <CardHeader>
            <CardTitle>Organization</CardTitle>
          </CardHeader>
          {/* Four equal-width readout tiles */}
          <CardContent className="grid grid-cols-2 divide-x divide-slate-800 p-0 sm:grid-cols-4">
            <Readout label="Name" value={activeOrg.name || '—'} />
            <Readout
              label="Slug"
              value={activeOrg.slug}
              hint="Registry namespace for push/pull"
            />
            <Readout label="Status" value={activeOrg.status || '—'} />
            <Readout
              label="Default visibility"
              value="private"
              hint="New repos default to private — change per-repo in Repositories"
            />
          </CardContent>
        </Card>
      )}

      {/* ── Add member dialog ─────────────────────────────────────────────── */}
      <Dialog
        open={addOpen}
        onOpenChange={(open) => {
          if (!open) closeAddDialog();
          else setAddOpen(true);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add member</DialogTitle>
            <DialogDescription>
              The account must already exist. They will gain access immediately.
            </DialogDescription>
          </DialogHeader>

          <form id="add-member-form" onSubmit={(e) => void handleAdd(e)}>
            <div className="space-y-3 px-3 py-3">
              <div className="space-y-1.5">
                <Label htmlFor="add-email">Email</Label>
                <Input
                  id="add-email"
                  type="email"
                  placeholder="colleague@example.com"
                  value={addEmail}
                  onChange={(e) => setAddEmail(e.target.value)}
                  required
                  autoFocus
                  autoComplete="email"
                />
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="add-role">Role</Label>
                <Select value={addRole} onValueChange={(v) => setAddRole(v as Role)}>
                  <SelectTrigger id="add-role">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {ROLES.map((r) => (
                      <SelectItem key={r} value={r}>
                        {r}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-micro text-slate-500">
                  viewer &lt; editor &lt; admin &lt; owner
                </p>
              </div>

              {addErr && (
                <p className="flex items-center gap-1.5 text-data text-destructive">
                  <AlertCircle className="size-3.5 shrink-0" />
                  {addErr}
                </p>
              )}
            </div>
          </form>

          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={closeAddDialog}
              disabled={addBusy}
            >
              Cancel
            </Button>
            <Button
              variant="default"
              size="sm"
              form="add-member-form"
              type="submit"
              disabled={addBusy}
            >
              {addBusy ? 'Adding…' : 'Add member'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* ── Remove confirm dialog ─────────────────────────────────────────── */}
      <Dialog open={!!removeTarget} onOpenChange={(open) => !open && setRemoveTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove member</DialogTitle>
            <DialogDescription>
              Remove{' '}
              <span className="font-medium text-slate-100">{removeTarget?.email}</span> from{' '}
              <span className="font-medium text-slate-100">{activeOrg?.slug}</span>? They will
              lose all access to this organization immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setRemoveTarget(null)}
              disabled={removeBusy}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              size="sm"
              onClick={() => void handleRemove()}
              disabled={removeBusy}
            >
              {removeBusy ? 'Removing…' : 'Remove member'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function PageHeading({ orgSlug }: { orgSlug?: string }) {
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">
        Members
        {orgSlug && <span className="ml-2 font-normal text-slate-500">· {orgSlug}</span>}
      </h1>
      <p className="mt-0.5 text-data text-slate-400">
        Manage who has access to this organization and their role.
      </p>
    </div>
  );
}
