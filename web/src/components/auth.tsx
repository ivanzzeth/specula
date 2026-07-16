// Auth context: on startup fetch /api/v1/me to determine login state.
//   - Logged in → render app; RequireAdmin gates admin pages by system_role.
//   - Not logged in → RequireAuth renders LoginScreen (email/password login or register).
// First registered user automatically becomes admin (backend enforced).

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type FormEvent,
  type ReactNode,
} from 'react';
import { getMe, login, register, logout as apiLogout, ApiError } from '../api/client';
import type { UserDTO } from '../api/types';
import Button from '../ui/Button';
import Spinner from '../ui/Spinner';
import { Input, Field } from '../ui/Field';

interface AuthCtx {
  user: UserDTO | null;
  isAdmin: boolean;
  loading: boolean;
  refresh: () => void;
  logout: () => Promise<void>;
}

const Ctx = createContext<AuthCtx>({
  user: null,
  isAdmin: false,
  loading: true,
  refresh: () => {},
  logout: async () => {},
});

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserDTO | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(() => {
    setLoading(true);
    getMe()
      .then((r) => setUser(r.user))
      .catch(() => setUser(null))
      .finally(() => setLoading(false));
  }, []);

  useEffect(refresh, [refresh]);

  const doLogout = useCallback(async () => {
    await apiLogout();
    setUser(null);
    refresh();
  }, [refresh]);

  const isAdmin = user?.system_role === 'admin';

  return (
    <Ctx.Provider value={{ user, isAdmin, loading, refresh, logout: doLogout }}>
      {children}
    </Ctx.Provider>
  );
}

export function useAuth(): AuthCtx {
  return useContext(Ctx);
}

// LoginScreen: shown when unauthenticated.
function LoginScreen() {
  const { refresh } = useAuth();
  const [mode, setMode] = useState<'login' | 'register'>('login');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setErr('');
    setBusy(true);
    try {
      if (mode === 'register') {
        await register(email, password);
      } else {
        await login(email, password);
      }
      refresh();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.detail || ex.message : 'Login failed, please retry');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-screen place-items-center bg-slate-950">
      <div className="w-full max-w-sm rounded-xl border border-slate-800 bg-slate-900 p-8 text-center">
        {/* Logo mark */}
        <div className="mx-auto mb-4 grid h-12 w-12 place-items-center rounded-lg bg-brand text-brand-fg font-bold text-xl">
          S
        </div>
        <h1 className="text-lg font-semibold text-slate-100">Specula</h1>
        <p className="mt-1 mb-6 text-sm text-slate-400">
          {mode === 'login'
            ? 'Sign in to continue'
            : '首个注册用户自动成为管理员'}
        </p>

        <form className="space-y-3 text-left" onSubmit={submit}>
          <Field label="Email">
            <Input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="you@example.com"
              autoComplete="email"
              required
            />
          </Field>
          <Field label="Password" hint={mode === 'register' ? 'At least 8 characters' : undefined}>
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
              autoComplete={mode === 'register' ? 'new-password' : 'current-password'}
              minLength={mode === 'register' ? 8 : undefined}
              required
            />
          </Field>
          {err && <p className="text-xs text-red-400">{err}</p>}
          <Button type="submit" className="w-full" disabled={busy}>
            {busy ? 'Please wait…' : mode === 'login' ? 'Sign in' : 'Register'}
          </Button>
        </form>

        <button
          className="mt-4 text-xs text-slate-500 hover:text-slate-300 transition-colors"
          onClick={() => {
            setMode(mode === 'login' ? 'register' : 'login');
            setErr('');
          }}
        >
          {mode === 'login' ? 'No account? Register' : 'Already have an account? Sign in'}
        </button>
      </div>
    </div>
  );
}

export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  if (loading) {
    return (
      <div className="grid min-h-screen place-items-center bg-slate-950">
        <Spinner />
      </div>
    );
  }
  if (!user) return <LoginScreen />;
  return <>{children}</>;
}

export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin, loading } = useAuth();
  if (loading) return null;
  if (!isAdmin) {
    return (
      <div className="rounded-lg border border-slate-800 bg-slate-900 p-8 text-center text-sm text-slate-400">
        Admin access required. Ask an existing admin to elevate your account.
      </div>
    );
  }
  return <>{children}</>;
}
