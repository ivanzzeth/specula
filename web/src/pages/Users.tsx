import { useEffect, useState } from 'react';
import { listUsers, createUser, patchUser, deleteUser } from '../api/client';
import { ApiError } from '../api/client';
import type { UserDTO, CreateUserRequest } from '../api/types';
import Button from '../ui/Button';
import Spinner from '../ui/Spinner';
import { Input, Field } from '../ui/Field';

type Modal =
  | { kind: 'create' }
  | { kind: 'edit'; user: UserDTO }
  | { kind: 'delete'; user: UserDTO }
  | null;

export function Users() {
  const [users, setUsers] = useState<UserDTO[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');
  const [modal, setModal] = useState<Modal>(null);

  const load = () => {
    setLoading(true);
    listUsers()
      .then((r) => setUsers(r.users ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  };

  useEffect(load, []);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-slate-400">
        <Spinner /> Loading users…
      </div>
    );
  }

  return (
    <div className="space-y-4 max-w-4xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-base font-semibold text-slate-100">Users</h1>
          <p className="text-xs text-slate-500 mt-0.5">{users.length} user(s)</p>
        </div>
        <Button size="sm" onClick={() => setModal({ kind: 'create' })}>
          + New User
        </Button>
      </div>

      {err && <div className="text-red-400 text-sm">{err}</div>}

      <div className="rounded-lg border border-slate-800 bg-slate-900 overflow-hidden">
        <table className="w-full text-[13px]">
          <thead>
            <tr className="border-b border-slate-800">
              {['ID', 'Email', 'Name', 'Role', 'Created', 'Actions'].map((h) => (
                <th
                  key={h}
                  className="px-3 py-2 text-left text-[11px] font-medium uppercase tracking-wider text-slate-500"
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-800/70">
            {users.length === 0 ? (
              <tr>
                <td colSpan={6} className="px-3 py-10 text-center text-slate-500">
                  No users found
                </td>
              </tr>
            ) : (
              users.map((u) => (
                <tr key={u.id}>
                  <td className="px-3 py-2.5 text-slate-500 tabular-nums">{u.id}</td>
                  <td className="px-3 py-2.5 text-slate-200">{u.email}</td>
                  <td className="px-3 py-2.5 text-slate-400">{u.name || '—'}</td>
                  <td className="px-3 py-2.5">
                    {u.system_role === 'admin' ? (
                      <span className="text-brand text-xs font-semibold uppercase">admin</span>
                    ) : (
                      <span className="text-slate-500 text-xs">{u.system_role || 'user'}</span>
                    )}
                  </td>
                  <td className="px-3 py-2.5 text-slate-500 text-xs tabular-nums">
                    {new Date(u.created_at).toLocaleDateString()}
                  </td>
                  <td className="px-3 py-2.5">
                    <div className="flex items-center gap-2">
                      <button
                        className="text-xs text-slate-400 hover:text-slate-100 transition-colors"
                        onClick={() => setModal({ kind: 'edit', user: u })}
                      >
                        Edit
                      </button>
                      <button
                        className="text-xs text-red-400 hover:text-red-300 transition-colors"
                        onClick={() => setModal({ kind: 'delete', user: u })}
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {modal?.kind === 'create' && (
        <CreateModal
          onClose={() => setModal(null)}
          onDone={() => {
            setModal(null);
            load();
          }}
        />
      )}
      {modal?.kind === 'edit' && (
        <EditModal
          user={modal.user}
          onClose={() => setModal(null)}
          onDone={() => {
            setModal(null);
            load();
          }}
        />
      )}
      {modal?.kind === 'delete' && (
        <DeleteModal
          user={modal.user}
          onClose={() => setModal(null)}
          onDone={() => {
            setModal(null);
            load();
          }}
        />
      )}
    </div>
  );
}

function Overlay({ children }: { children: React.ReactNode }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-full max-w-sm rounded-lg border border-slate-700 bg-slate-900 p-6 shadow-xl">
        {children}
      </div>
    </div>
  );
}

function CreateModal({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [form, setForm] = useState<CreateUserRequest>({
    email: '',
    name: '',
    password: '',
    system_role: '',
  });
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr('');
    setBusy(true);
    try {
      await createUser(form);
      onDone();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.detail : String(ex));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Overlay>
      <h2 className="text-sm font-semibold text-slate-100 mb-4">Create User</h2>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Email">
          <Input
            type="email"
            required
            value={form.email}
            onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
          />
        </Field>
        <Field label="Name">
          <Input
            value={form.name}
            onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
          />
        </Field>
        <Field label="Password">
          <Input
            type="password"
            required
            minLength={8}
            value={form.password}
            onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
          />
        </Field>
        <Field label="Role">
          <select
            className="w-full h-8 rounded-md border border-slate-700 bg-slate-900 px-2.5 text-[13px] text-slate-100 focus:border-brand/70 focus:outline-none"
            value={form.system_role}
            onChange={(e) => setForm((f) => ({ ...f, system_role: e.target.value }))}
          >
            <option value="">user</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        {err && <p className="text-xs text-red-400">{err}</p>}
        <div className="flex gap-2 pt-1">
          <Button type="submit" disabled={busy} className="flex-1">
            {busy ? 'Creating…' : 'Create'}
          </Button>
          <Button variant="secondary" type="button" onClick={onClose} className="flex-1">
            Cancel
          </Button>
        </div>
      </form>
    </Overlay>
  );
}

function EditModal({
  user,
  onClose,
  onDone,
}: {
  user: UserDTO;
  onClose: () => void;
  onDone: () => void;
}) {
  const [name, setName] = useState(user.name);
  const [role, setRole] = useState(user.system_role);
  const [password, setPassword] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr('');
    setBusy(true);
    try {
      const body: { name?: string; system_role?: string; password?: string } = {};
      if (name !== user.name) body.name = name;
      if (role !== user.system_role) body.system_role = role;
      if (password) body.password = password;
      await patchUser(user.id, body);
      onDone();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.detail : String(ex));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Overlay>
      <h2 className="text-sm font-semibold text-slate-100 mb-1">Edit User</h2>
      <p className="text-xs text-slate-500 mb-4">{user.email}</p>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name">
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Role">
          <select
            className="w-full h-8 rounded-md border border-slate-700 bg-slate-900 px-2.5 text-[13px] text-slate-100 focus:border-brand/70 focus:outline-none"
            value={role}
            onChange={(e) => setRole(e.target.value)}
          >
            <option value="">user</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <Field label="New Password" hint="Leave blank to keep current">
          <Input
            type="password"
            minLength={8}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="••••••••"
          />
        </Field>
        {err && <p className="text-xs text-red-400">{err}</p>}
        <div className="flex gap-2 pt-1">
          <Button type="submit" disabled={busy} className="flex-1">
            {busy ? 'Saving…' : 'Save'}
          </Button>
          <Button variant="secondary" type="button" onClick={onClose} className="flex-1">
            Cancel
          </Button>
        </div>
      </form>
    </Overlay>
  );
}

function DeleteModal({
  user,
  onClose,
  onDone,
}: {
  user: UserDTO;
  onClose: () => void;
  onDone: () => void;
}) {
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function confirm() {
    setErr('');
    setBusy(true);
    try {
      await deleteUser(user.id);
      onDone();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.detail : String(ex));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Overlay>
      <h2 className="text-sm font-semibold text-slate-100 mb-2">Delete User</h2>
      <p className="text-sm text-slate-400 mb-4">
        Delete <span className="text-slate-200">{user.email}</span>? This cannot be undone.
      </p>
      {err && <p className="text-xs text-red-400 mb-3">{err}</p>}
      <div className="flex gap-2">
        <Button variant="danger" onClick={confirm} disabled={busy} className="flex-1">
          {busy ? 'Deleting…' : 'Delete'}
        </Button>
        <Button variant="secondary" onClick={onClose} className="flex-1">
          Cancel
        </Button>
      </div>
    </Overlay>
  );
}
