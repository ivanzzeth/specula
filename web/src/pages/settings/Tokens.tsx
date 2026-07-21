/**
 * Tokens — user self-service API key management (REGISTRY-DESIGN §5.3).
 *
 * Owned by: members-tokens sub-agent (web/src/pages/settings/**)
 *
 * Design critical path: `raw_key` is returned EXACTLY ONCE at creation.
 * The reveal step is treated as the most important moment on this page:
 * an amber-bordered panel, explicit "you will not see this again" text,
 * a one-click copy, and a deliberate "I have saved my token" dismiss.
 *
 * Usage snippets show docker/pip/npm/go examples right on the page (not
 * hidden behind a help link) so the token's purpose is immediately clear.
 *
 * API consumed:
 *   createKey({ label })   → KeyDTO   (raw_key present only here)
 *   listKeys()             → KeysResponse
 *   revokeKey(id)          → 204
 */
import { type FormEvent, useCallback, useEffect, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { AlertCircle, Check, Copy, Plus } from 'lucide-react';

import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { ApiError, createKey, listKeys, revokeKey } from '@/api/client';
import { translateServerError } from '@/i18n/server-errors';
import type { KeyDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { useToast } from '@/hooks/use-toast';
import { formatRelative } from '@/lib/utils';
import { useRegistryHost } from '../../hooks/useRegistryHost';

// ── Date helpers ──────────────────────────────────────────────────────────────

function isoRelative(iso: string | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return formatRelative(Math.floor(d.getTime() / 1000));
}

function isoDate(iso: string | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  });
}

/** errMessage routes API errors through the shared server-error allow-list. */
function errMessage(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail) || e.message;
  return e instanceof Error ? e.message : String(e);
}

// ── CodeBlock: copyable monospace block ───────────────────────────────────────

function CodeBlock({ children }: { children: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const copy = () => {
    void navigator.clipboard.writeText(children).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  };

  return (
    <div className="group relative">
      <pre className="overflow-x-auto rounded border border-slate-800 bg-slate-950 p-2.5 text-data leading-relaxed text-slate-300">
        {children}
      </pre>
      <button
        type="button"
        onClick={copy}
        className="absolute right-1.5 top-1.5 rounded p-1 text-slate-500 opacity-0 transition-all duration-fast hover:bg-slate-800 hover:text-slate-200 group-hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        title={t('tokens.usage.copySnippet')}
        aria-label={t('tokens.usage.copySnippet')}
      >
        {copied ? (
          <Check className="size-3 text-health-up" />
        ) : (
          <Copy className="size-3" />
        )}
      </button>
    </div>
  );
}

// ── Usage snippets card ───────────────────────────────────────────────────────

/**
 * The snippet bodies stay English in every locale on purpose: they are literal
 * shell/config text a user copies and pastes, and their comments are part of
 * what lands in the user's `.npmrc` / `pip.conf` / shell profile. Translating a
 * comment inside a pasted config is noise at best; translating anything else in
 * them would produce a command that does not run.
 */
function UsageSnippets({ host, email, orgSlug }: { host: string; email: string; orgSlug: string }) {
  const { t } = useTranslation();
  const T = '<your-token>'; // placeholder — the actual token is in the reveal dialog

  const docker = `# Authenticate
docker login ${host} -u ${email} -p ${T}

# Pull a hosted image
docker pull ${host}/${orgSlug}/{repo}:{tag}

# Push an image
docker tag {image} ${host}/${orgSlug}/{repo}:{tag}
docker push ${host}/${orgSlug}/{repo}:{tag}`;

  const pip = `# ~/.pip/pip.conf
[global]
extra-index-url = https://${email}:${T}@${host}/pypi/

# or per-command
pip install --extra-index-url \\
  https://${email}:${T}@${host}/pypi/ \\
  {package}`;

  const npm = `# .npmrc (project root or ~/.npmrc)
registry=https://${host}/npm/
//${host}/npm/:_authToken=${T}

# or set via npm config
npm config set //${host}/npm/:_authToken ${T}`;

  const gomod = `# export in shell profile (or .env)
export GOPROXY=https://${email}:${T}@${host}/go,direct
export GONOSUMCHECK=${host}

# or per-command
GOPROXY=https://${email}:${T}@${host}/go,direct \\
  go get {module}@{version}`;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('tokens.usage.title')}</CardTitle>
        <span className="text-data text-slate-400">
          {/* The literal "<your-token>" rides in as the <token/> slot's CHILDREN.
              It must NOT be spelled as &lt;…&gt; entities in the locale JSON:
              Trans parses the translated string as HTML but does not decode
              entities, so React then renders them verbatim and the user reads
              "&lt;your-token&gt;" on the page. Verified in the browser. Passing
              it as children lets React escape it exactly once. */}
          <Trans
            i18nKey="tokens.usage.hint"
            components={{ token: <code className="text-brand/80">{'<your-token>'}</code> }}
          />
        </span>
      </CardHeader>
      <CardContent className="pb-3 pt-0">
        <Tabs defaultValue="docker">
          <TabsList className="mb-3">
            <TabsTrigger value="docker">{t('tokens.usage.docker')}</TabsTrigger>
            <TabsTrigger value="pip">{t('tokens.usage.pip')}</TabsTrigger>
            <TabsTrigger value="npm">{t('tokens.usage.npm')}</TabsTrigger>
            <TabsTrigger value="go">{t('tokens.usage.go')}</TabsTrigger>
          </TabsList>
          <TabsContent value="docker">
            <CodeBlock>{docker}</CodeBlock>
          </TabsContent>
          <TabsContent value="pip">
            <CodeBlock>{pip}</CodeBlock>
          </TabsContent>
          <TabsContent value="npm">
            <CodeBlock>{npm}</CodeBlock>
          </TabsContent>
          <TabsContent value="go">
            <CodeBlock>{gomod}</CodeBlock>
          </TabsContent>
        </Tabs>
      </CardContent>
    </Card>
  );
}

// ── Main component ────────────────────────────────────────────────────────────

export function Tokens() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const { activeOrg } = useOrg();

  const [keys, setKeys] = useState<KeyDTO[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');

  // Create dialog state machine: form → reveal
  type Step = 'form' | 'reveal';
  const [createOpen, setCreateOpen] = useState(false);
  const [step, setStep] = useState<Step>('form');
  const [keyLabel, setKeyLabel] = useState('');
  const [scopePull, setScopePull] = useState(true);
  const [scopePush, setScopePush] = useState(true);
  const [createBusy, setCreateBusy] = useState(false);
  const [createErr, setCreateErr] = useState('');
  const [rawKey, setRawKey] = useState('');
  const [keyCopied, setKeyCopied] = useState(false);

  // Revoke confirm dialog
  const [revokeTarget, setRevokeTarget] = useState<KeyDTO | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);

  const { toast } = useToast();

  const host = useRegistryHost();
  const email = user?.email ?? '';
  const orgSlug = activeOrg?.slug ?? '{org}';

  // ── Data loading ───────────────────────────────────────────────────────────

  const load = useCallback(() => {
    setLoading(true);
    setErr('');
    listKeys()
      .then((r) => setKeys(r.keys ?? []))
      .catch((e: unknown) => setErr(errMessage(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // ── Create flow ────────────────────────────────────────────────────────────

  const handleCreate = async (e: FormEvent) => {
    e.preventDefault();
    if (!scopePull && !scopePush) {
      setCreateErr(t('tokens.createDialog.scopesHint'));
      return;
    }
    setCreateBusy(true);
    setCreateErr('');
    try {
      const scopes: string[] = [];
      if (scopePull) scopes.push('pull');
      if (scopePush) scopes.push('push');
      const key = await createKey({
        label: keyLabel.trim() || undefined,
        scopes,
      });
      // Prepend to list even before reveal — so it's there when the dialog closes.
      setKeys((prev) => [key, ...prev]);

      if (key.raw_key) {
        // Advance to the reveal step — this is the one-time window.
        setRawKey(key.raw_key);
        setStep('reveal');
      } else {
        // Server didn't return raw_key — unusual, but don't crash.
        toast({ variant: 'success', title: t('tokens.created') });
        closeCreate();
      }
    } catch (e: unknown) {
      setCreateErr(errMessage(e));
    } finally {
      setCreateBusy(false);
    }
  };

  const copyKey = () => {
    void navigator.clipboard.writeText(rawKey).then(() => {
      setKeyCopied(true);
      setTimeout(() => setKeyCopied(false), 3000);
    });
  };

  const closeCreate = () => {
    setCreateOpen(false);
    // Reset after exit animation completes
    setTimeout(() => {
      setStep('form');
      setKeyLabel('');
      setScopePull(true);
      setScopePush(true);
      setRawKey('');
      setKeyCopied(false);
      setCreateErr('');
    }, 150);
  };

  // ── Revoke ─────────────────────────────────────────────────────────────────

  const handleRevoke = async () => {
    if (!revokeTarget) return;
    setRevokeBusy(true);
    try {
      await revokeKey(revokeTarget.id);
      setKeys((prev) => prev.filter((k) => k.id !== revokeTarget.id));
      toast({
        variant: 'success',
        title: t('tokens.revoked'),
        description: revokeTarget.label || revokeTarget.prefix,
      });
      setRevokeTarget(null);
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('tokens.revokeFailed'),
        description: errMessage(e),
        duration: Infinity,
      });
    } finally {
      setRevokeBusy(false);
    }
  };

  // ── Render ─────────────────────────────────────────────────────────────────

  const activeKeys = keys.filter((k) => !k.revoked);

  return (
    <div className="space-y-3">
      {/* Page heading + primary action */}
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-display font-semibold text-slate-100">{t('tokens.title')}</h1>
          <p className="mt-0.5 text-data text-slate-400">{t('tokens.subtitle')}</p>
        </div>
        <Button variant="default" size="sm" onClick={() => setCreateOpen(true)}>
          <Plus />
          {t('tokens.new')}
        </Button>
      </div>

      {/* Usage snippets — always visible, not hidden behind a help link */}
      <UsageSnippets host={host} email={email} orgSlug={orgSlug} />

      {/* ── Tokens table ─────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>{t('tokens.active')}</CardTitle>
          {!loading && (
            <span className="tnum text-data text-slate-400">{activeKeys.length}</span>
          )}
        </CardHeader>
        <CardContent className="p-0">
          {loading ? (
            <div className="p-3">
              <SkeletonRows rows={4} />
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
                  <TableHead className="w-32">{t('tokens.colPrefix')}</TableHead>
                  <TableHead>{t('tokens.colLabel')}</TableHead>
                  <TableHead className="w-28">{t('tokens.colScopes')}</TableHead>
                  <TableHead className="w-28">{t('tokens.colCreated')}</TableHead>
                  <TableHead className="w-28">{t('tokens.colLastUsed')}</TableHead>
                  <TableHead className="w-28">{t('tokens.colExpires')}</TableHead>
                  <TableHead className="w-20" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {activeKeys.length === 0 ? (
                  <EmptyRow colSpan={7}>{t('tokens.empty')}</EmptyRow>
                ) : (
                  activeKeys.map((k) => (
                    <TableRow key={k.id}>
                      <TableCell>
                        <code className="tnum text-brand/80">{k.prefix}…</code>
                      </TableCell>
                      <TableCell className="text-slate-300">
                        {k.label ?? <span className="text-slate-500">—</span>}
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        {(k.scopes ?? []).join(', ') || '—'}
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        <span title={isoDate(k.created_at)}>{isoRelative(k.created_at)}</span>
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        {isoRelative(k.last_used_at)}
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        {k.expires_at ? (
                          isoDate(k.expires_at)
                        ) : (
                          <span className="text-slate-500">{t('common.never')}</span>
                        )}
                      </TableCell>
                      <TableCell>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-slate-500 hover:text-destructive"
                          onClick={() => setRevokeTarget(k)}
                        >
                          {t('tokens.revoke')}
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* ── Create token dialog ───────────────────────────────────────────── */}
      <Dialog open={createOpen} onOpenChange={(open) => !open && closeCreate()}>
        <DialogContent>
          {step === 'form' ? (
            /* Step 1: label input */
            <>
              <DialogHeader>
                <DialogTitle>{t('tokens.createDialog.title')}</DialogTitle>
                <DialogDescription>{t('tokens.createDialog.description')}</DialogDescription>
              </DialogHeader>

              <form id="create-key-form" onSubmit={(e) => void handleCreate(e)}>
                <div className="space-y-3 px-3 py-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="key-label">
                      {t('tokens.createDialog.label')}{' '}
                      <span className="font-normal text-slate-500">
                        {t('tokens.createDialog.labelOptional')}
                      </span>
                    </Label>
                    <Input
                      id="key-label"
                      placeholder={t('tokens.createDialog.labelPlaceholder')}
                      value={keyLabel}
                      onChange={(e) => setKeyLabel(e.target.value)}
                      autoFocus
                    />
                  </div>

                  <div className="space-y-1.5">
                    <Label>{t('tokens.createDialog.scopes')}</Label>
                    <div className="flex flex-wrap gap-4 text-data text-slate-300">
                      <label className="inline-flex items-center gap-2">
                        <input
                          type="checkbox"
                          checked={scopePull}
                          onChange={(e) => setScopePull(e.target.checked)}
                        />
                        {t('tokens.createDialog.scopePull')}
                      </label>
                      <label className="inline-flex items-center gap-2">
                        <input
                          type="checkbox"
                          checked={scopePush}
                          onChange={(e) => setScopePush(e.target.checked)}
                        />
                        {t('tokens.createDialog.scopePush')}
                      </label>
                    </div>
                    <p className="text-label text-slate-500">{t('tokens.createDialog.scopesHint')}</p>
                  </div>

                  {createErr && (
                    <p className="flex items-center gap-1.5 text-data text-destructive">
                      <AlertCircle className="size-3.5 shrink-0" />
                      {createErr}
                    </p>
                  )}
                </div>
              </form>

              <DialogFooter>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={closeCreate}
                  disabled={createBusy}
                >
                  {t('common.cancel')}
                </Button>
                <Button
                  variant="default"
                  size="sm"
                  form="create-key-form"
                  type="submit"
                  disabled={createBusy}
                >
                  {createBusy ? t('tokens.createDialog.busy') : t('tokens.createDialog.submit')}
                </Button>
              </DialogFooter>
            </>
          ) : (
            /* Step 2: one-time key reveal — the critical UX moment */
            <>
              <DialogHeader>
                <DialogTitle>{t('tokens.reveal.title')}</DialogTitle>
              </DialogHeader>

              <div className="space-y-3 px-3 py-3">
                {/* Amber-bordered warning panel: unmissable, deliberate */}
                <div className="rounded border border-brand/40 bg-brand/5 px-3 py-2.5">
                  <p className="label-caps text-label font-semibold text-brand">
                    {t('tokens.reveal.warnTitle')}
                  </p>
                  <p className="mt-1 text-data text-slate-400">{t('tokens.reveal.warnBody')}</p>
                </div>

                {/* Token value in a selectable, copyable code block */}
                <div className="space-y-1.5">
                  <Label>{t('tokens.reveal.token')}</Label>
                  <div className="relative">
                    <pre className="select-all overflow-x-auto rounded border border-slate-800 bg-slate-950 p-2.5 text-data text-slate-100 break-all whitespace-pre-wrap">
                      {rawKey}
                    </pre>
                    <button
                      type="button"
                      onClick={copyKey}
                      className="absolute right-1.5 top-1.5 rounded p-1 text-slate-500 transition-all duration-fast hover:bg-slate-800 hover:text-slate-200 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                      aria-label={t('tokens.reveal.copyToken')}
                      title={t('tokens.reveal.copyToken')}
                    >
                      {keyCopied ? (
                        <Check className="size-3.5 text-health-up" />
                      ) : (
                        <Copy className="size-3.5" />
                      )}
                    </button>
                  </div>
                  {keyCopied && (
                    <p className="text-data text-health-up">
                      {t('tokens.reveal.copiedToClipboard')}
                    </p>
                  )}
                </div>
              </div>

              {/* Deliberate dismiss: the button wording is a confirmation statement */}
              <DialogFooter>
                <Button variant="default" size="sm" onClick={closeCreate}>
                  {t('tokens.savedDismiss')}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* ── Revoke confirm dialog ─────────────────────────────────────────── */}
      <Dialog open={!!revokeTarget} onOpenChange={(open) => !open && setRevokeTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('tokens.revokeDialog.title')}</DialogTitle>
            <DialogDescription>
              {/* Prefix and optional label are one emphasised span: zh puts the
                  object before the verb, so it cannot be positional in JSX.

                  The target rides in as the <target/> slot's CHILDREN, never as
                  a `values` entry. Trans interpolates `values` BEFORE parsing
                  the result as HTML, and i18n/index.ts sets escapeValue:false
                  (React escapes for us) — so a user-supplied token label like
                  "<laptop>" would be parsed as an unknown tag. Verified: it did
                  not merely drop the label, its phantom closing tag swallowed
                  the rest of the sentence, leaving a destructive confirmation
                  that named no target. As children the text is escaped, so
                  "<laptop>" and even "<script>" render literally. */}
              <Trans
                i18nKey="tokens.revokeDialog.description"
                components={{
                  target: (
                    <span className="font-medium text-slate-100">
                      {`${revokeTarget?.prefix ?? ''}…${
                        revokeTarget?.label ? ` (${revokeTarget.label})` : ''
                      }`}
                    </span>
                  ),
                }}
              />
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setRevokeTarget(null)}
              disabled={revokeBusy}
            >
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              size="sm"
              onClick={() => void handleRevoke()}
              disabled={revokeBusy}
            >
              {revokeBusy ? t('tokens.revokeDialog.busy') : t('tokens.revokeDialog.submit')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
