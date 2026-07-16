/**
 * Users — system-wide user administration (Ops zone, admin only).
 *
 * Distinct from Members: a *member* is an identity's role inside one org; a
 * *user* is the account itself, across the whole install. Only a system admin
 * sees this route (RequireAdmin in App.tsx).
 *
 * ── INTEGRATION NOTE (R3) ────────────────────────────────────────────────────
 * This page was the last holdout on the pre-R3 primitives (`src/ui/*`): hand
 * rolled modals with a large radius, raw `text-red-400`, and a spinner that
 * collapsed the layout on load. It is now on the shared system — Radix Dialog
 * (focus trap + Esc + restore), the sanctioned Table, and status colour drawn
 * from the lamp ramp rather than raw Tailwind reds. With this, nothing imports
 * `src/ui/*` and that directory is deleted.
 */

import { useEffect, useState, type FormEvent, type ReactNode } from 'react';

import { ApiError, createUser, deleteUser, listUsers, patchUser } from '@/api/client';
import type { CreateUserRequest, UserDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
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

type Modal =
  | { kind: 'create' }
  | { kind: 'edit'; user: UserDTO }
  | { kind: 'delete'; user: UserDTO }
  | null;

/** The system roles the API accepts. '' is a plain user. */
const SYSTEM_ROLES = [
  { value: 'user', label: 'user' },
  { value: 'admin', label: 'admin' },
] as const;

/** The API models "no role" as ''; Radix Select cannot hold '' as a value. */
const toRoleValue = (role: string): string => role || 'user';
const fromRoleValue = (value: string): string => (value === 'user' ? '' : value);

function errMessage(ex: unknown): string {
  if (ex instanceof ApiError) return ex.detail || ex.message;
  return ex instanceof Error ? ex.message : String(ex);
}

export function Users() {
  const [users, setUsers] = useState<UserDTO[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');
  const [modal, setModal] = useState<Modal>(null);

  const load = () => {
    setLoading(true);
    listUsers()
      .then((r) => {
        setUsers(r.users ?? []);
        setErr('');
      })
      .catch((e: unknown) => setErr(errMessage(e)))
      .finally(() => setLoading(false));
  };

  useEffect(load, []);

  const closeAnd = (reload: boolean) => {
    setModal(null);
    if (reload) load();
  };

  return (
    <div className="space-y-3">
      <div className="flex items-end justify-between gap-3">
        <div>
          <h1 className="text-display font-semibold text-slate-100">Users</h1>
          <p className="mt-0.5 text-data text-slate-400">
            Every account on this install. Org-level roles live under Members.
          </p>
        </div>
        <Button variant="default" size="sm" onClick={() => setModal({ kind: 'create' })}>
          New user
        </Button>
      </div>

      {err && (
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      )}

      <Card>
        {loading ? (
          <CardContent>
            <SkeletonRows rows={5} />
          </CardContent>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-16 text-right">ID</TableHead>
                <TableHead>Email</TableHead>
                <TableHead className="w-40">Name</TableHead>
                <TableHead className="w-24">Role</TableHead>
                <TableHead className="w-28 text-right">Created</TableHead>
                <TableHead className="w-28 text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.length === 0 ? (
                <EmptyRow colSpan={6}>No users found.</EmptyRow>
              ) : (
                users.map((u) => (
                  <TableRow key={u.id}>
                    <TableCell className="tnum text-right text-slate-500">{u.id}</TableCell>
                    <TableCell className="text-slate-100">{u.email}</TableCell>
                    <TableCell className="text-slate-400">{u.name || '—'}</TableCell>
                    <TableCell>
                      {/* Admin is a privilege level, so it is stated plainly in
                          amber-free lamp terms — a solid amber badge here would
                          read as a control, not a fact. */}
                      {u.system_role === 'admin' ? (
                        <span className="inline-flex items-center gap-1.5 text-micro font-semibold uppercase tracking-wider text-tier-consensus">
                          <span className="lamp bg-tier-consensus" aria-hidden />
                          admin
                        </span>
                      ) : (
                        <span className="text-data text-slate-500">user</span>
                      )}
                    </TableCell>
                    <TableCell className="tnum text-right text-slate-500">
                      {new Date(u.created_at).toLocaleDateString()}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-2">
                        <button
                          className="text-data text-slate-400 transition-colors duration-fast hover:text-slate-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                          onClick={() => setModal({ kind: 'edit', user: u })}
                        >
                          Edit
                        </button>
                        <button
                          className="text-data text-slate-500 transition-colors duration-fast hover:text-destructive focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                          onClick={() => setModal({ kind: 'delete', user: u })}
                        >
                          Delete
                        </button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        )}
      </Card>

      {modal?.kind === 'create' && <CreateModal onClose={closeAnd} />}
      {modal?.kind === 'edit' && <EditModal user={modal.user} onClose={closeAnd} />}
      {modal?.kind === 'delete' && <DeleteModal user={modal.user} onClose={closeAnd} />}
    </div>
  );
}

// ── Modals ────────────────────────────────────────────────────────────────────

/** Shared shell: Radix Dialog wired so dismissal always routes through onClose. */
function ModalShell({
  title,
  description,
  onClose,
  children,
}: {
  title: string;
  description?: ReactNode;
  onClose: (reload: boolean) => void;
  children: ReactNode;
}) {
  return (
    <Dialog open onOpenChange={(open) => !open && onClose(false)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description ? <DialogDescription>{description}</DialogDescription> : null}
        </DialogHeader>
        {children}
      </DialogContent>
    </Dialog>
  );
}

function FormError({ message }: { message: string }) {
  if (!message) return null;
  return <p className="text-data text-destructive">{message}</p>;
}

function CreateModal({ onClose }: { onClose: (reload: boolean) => void }) {
  const [form, setForm] = useState<CreateUserRequest>({
    email: '',
    name: '',
    password: '',
    system_role: '',
  });
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setErr('');
    setBusy(true);
    try {
      await createUser(form);
      onClose(true);
    } catch (ex) {
      setErr(errMessage(ex));
    } finally {
      setBusy(false);
    }
  }

  return (
    <ModalShell title="Create user" onClose={onClose}>
      <form onSubmit={(e) => void submit(e)}>
        <div className="space-y-3 p-3">
          <div className="space-y-1.5">
            <Label htmlFor="cu-email">Email</Label>
            <Input
              id="cu-email"
              type="email"
              required
              autoFocus
              value={form.email}
              onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="cu-name">Name</Label>
            <Input
              id="cu-name"
              value={form.name}
              onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="cu-password">Password</Label>
            <Input
              id="cu-password"
              type="password"
              required
              minLength={8}
              value={form.password}
              onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
            />
            <p className="text-micro text-slate-500">At least 8 characters</p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="cu-role">System role</Label>
            <Select
              value={toRoleValue(form.system_role ?? '')}
              onValueChange={(v) => setForm((f) => ({ ...f, system_role: fromRoleValue(v) }))}
            >
              <SelectTrigger id="cu-role">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SYSTEM_ROLES.map((r) => (
                  <SelectItem key={r.value} value={r.value}>
                    {r.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <FormError message={err} />
        </div>
        <DialogFooter>
          <Button variant="secondary" size="sm" type="button" onClick={() => onClose(false)}>
            Cancel
          </Button>
          <Button variant="default" size="sm" type="submit" disabled={busy}>
            {busy ? 'Creating…' : 'Create'}
          </Button>
        </DialogFooter>
      </form>
    </ModalShell>
  );
}

function EditModal({ user, onClose }: { user: UserDTO; onClose: (reload: boolean) => void }) {
  const [name, setName] = useState(user.name);
  const [role, setRole] = useState(user.system_role);
  const [password, setPassword] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setErr('');
    setBusy(true);
    try {
      // Send only what changed — a PATCH that echoes every field would clobber
      // concurrent edits with values this form never intended to set.
      const body: { name?: string; system_role?: string; password?: string } = {};
      if (name !== user.name) body.name = name;
      if (role !== user.system_role) body.system_role = role;
      if (password) body.password = password;
      await patchUser(user.id, body);
      onClose(true);
    } catch (ex) {
      setErr(errMessage(ex));
    } finally {
      setBusy(false);
    }
  }

  return (
    <ModalShell title="Edit user" description={user.email} onClose={onClose}>
      <form onSubmit={(e) => void submit(e)}>
        <div className="space-y-3 p-3">
          <div className="space-y-1.5">
            <Label htmlFor="eu-name">Name</Label>
            <Input id="eu-name" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eu-role">System role</Label>
            <Select value={toRoleValue(role)} onValueChange={(v) => setRole(fromRoleValue(v))}>
              <SelectTrigger id="eu-role">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SYSTEM_ROLES.map((r) => (
                  <SelectItem key={r.value} value={r.value}>
                    {r.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="eu-password">New password</Label>
            <Input
              id="eu-password"
              type="password"
              minLength={8}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
            />
            <p className="text-micro text-slate-500">Leave blank to keep the current password</p>
          </div>
          <FormError message={err} />
        </div>
        <DialogFooter>
          <Button variant="secondary" size="sm" type="button" onClick={() => onClose(false)}>
            Cancel
          </Button>
          <Button variant="default" size="sm" type="submit" disabled={busy}>
            {busy ? 'Saving…' : 'Save'}
          </Button>
        </DialogFooter>
      </form>
    </ModalShell>
  );
}

function DeleteModal({ user, onClose }: { user: UserDTO; onClose: (reload: boolean) => void }) {
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function confirm() {
    setErr('');
    setBusy(true);
    try {
      await deleteUser(user.id);
      onClose(true);
    } catch (ex) {
      setErr(errMessage(ex));
      setBusy(false);
    }
  }

  return (
    <ModalShell
      title="Delete user"
      description={
        <>
          <span className="text-slate-100">{user.email}</span> will be removed. This cannot be
          undone.
        </>
      }
      onClose={onClose}
    >
      {err && (
        <div className="p-3">
          <FormError message={err} />
        </div>
      )}
      <DialogFooter>
        <Button variant="secondary" size="sm" onClick={() => onClose(false)}>
          Cancel
        </Button>
        <Button variant="destructive" size="sm" onClick={() => void confirm()} disabled={busy}>
          {busy ? 'Deleting…' : 'Delete user'}
        </Button>
      </DialogFooter>
    </ModalShell>
  );
}
