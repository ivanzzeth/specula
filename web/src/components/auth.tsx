// Auth context: on startup fetch /api/v1/me to determine login state.
//   - Logged in → render app; RequireAdmin gates admin pages by system_role.
//   - Not logged in → RequireAuth renders LoginScreen (email/password login or register).
// First registered user automatically becomes admin (backend enforced).
//
// Migrated from ../ui/* (deprecated) to @/components/ui/* (members-tokens agent, §DEPRECATED.md).

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type FormEvent,
  type ReactNode,
} from 'react';
import { AlertCircle } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { ApiError, getMe, login, logout as apiLogout, register } from '../api/client';
import type { UserDTO } from '../api/types';
import { translateServerError } from '@/i18n/server-errors';
import { LanguageSwitcher } from '@/components/language-switcher';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';

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

// ── LoginScreen ───────────────────────────────────────────────────────────────

/** LoginScreen is shown when the user is not authenticated. */
function LoginScreen() {
  const { refresh } = useAuth();
  const { t } = useTranslation();
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
      // Server errors arrive in English. translateServerError localises the
      // explicit auth allow-list (bad password, email taken…) — the errors a
      // user can actually act on — and passes anything else through verbatim
      // rather than guessing. See i18n/server-errors.ts.
      setErr(
        ex instanceof ApiError
          ? translateServerError(ex.detail) || translateServerError(ex.message)
          : t('auth.failed')
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-screen place-items-center bg-slate-950 px-4">
      {/* The login screen has no identity rail, so the switcher lives in the
          corner. Someone who cannot read the form must still be able to change
          the language BEFORE they are asked to authenticate. */}
      <div className="absolute right-3 top-3">
        <LanguageSwitcher />
      </div>
      <div className="w-full max-w-xs">
        {/* Brand mark — the amber square that appears once in the chrome */}
        <div className="mb-6 flex flex-col items-center gap-2">
          <div
            className="grid size-10 place-items-center rounded bg-brand font-bold text-brand-fg"
            style={{ fontSize: '18px' }}
          >
            S
          </div>
          <div className="text-center">
            <h1 className="text-section font-semibold tracking-tight text-slate-100">Specula</h1>
            <p className="mt-0.5 text-data text-slate-400">
              {mode === 'login' ? t('auth.signInSubtitle') : t('auth.registerSubtitle')}
            </p>
          </div>
        </div>

        {/* Panel */}
        <div className="rounded border border-slate-800 bg-slate-900">
          <form className="space-y-3 p-4" onSubmit={(e) => void submit(e)}>
            <div className="space-y-1.5">
              <Label htmlFor="auth-email">{t('auth.email')}</Label>
              <Input
                id="auth-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder={t('auth.emailPlaceholder')}
                autoComplete="email"
                required
                autoFocus
              />
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="auth-password">{t('auth.password')}</Label>
              <Input
                id="auth-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                autoComplete={mode === 'register' ? 'new-password' : 'current-password'}
                minLength={mode === 'register' ? 8 : undefined}
                required
              />
              {mode === 'register' && (
                <p className="text-micro text-slate-500">{t('auth.passwordHint')}</p>
              )}
            </div>

            {err && (
              <p className="flex items-center gap-1.5 text-data text-destructive">
                <AlertCircle className="size-3.5 shrink-0" />
                {err}
              </p>
            )}

            <Button
              type="submit"
              variant="default"
              size="default"
              className="w-full"
              disabled={busy}
            >
              {busy
                ? t('common.pleaseWait')
                : mode === 'login'
                  ? t('auth.signIn')
                  : t('auth.createAccount')}
            </Button>
          </form>

          {/* Mode toggle — hairline-separated footer */}
          <div className="border-t border-slate-800 px-4 py-3 text-center">
            <button
              type="button"
              className="text-data text-slate-500 transition-colors duration-fast hover:text-slate-200 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
              onClick={() => {
                setMode(mode === 'login' ? 'register' : 'login');
                setErr('');
              }}
            >
              {mode === 'login' ? t('auth.toRegister') : t('auth.toLogin')}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// ── Guards ────────────────────────────────────────────────────────────────────

export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  const { t } = useTranslation();
  if (loading) {
    return (
      <div className="grid min-h-screen place-items-center bg-slate-950">
        {/* Inline spinner — no deprecated import needed */}
        <span
          role="status"
          aria-label={t('common.loading')}
          className="inline-block size-5 animate-spin rounded-full border-2 border-slate-800 border-t-brand"
        />
      </div>
    );
  }
  if (!user) return <LoginScreen />;
  return <>{children}</>;
}

export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin, loading } = useAuth();
  const { t } = useTranslation();
  if (loading) return null;
  if (!isAdmin) {
    return (
      <div className="rounded border border-slate-800 bg-slate-900 p-8 text-center text-data text-slate-400">
        {t('auth.adminRequired')}
      </div>
    );
  }
  return <>{children}</>;
}
