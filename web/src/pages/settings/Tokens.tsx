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
import { AlertCircle, Check, Copy, Plus } from 'lucide-react';

import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { createKey, listKeys, revokeKey } from '@/api/client';
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

// ── CodeBlock: copyable monospace block ───────────────────────────────────────

function CodeBlock({ children }: { children: string }) {
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
        title="Copy to clipboard"
        aria-label="Copy snippet to clipboard"
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

function UsageSnippets({ host, email, orgSlug }: { host: string; email: string; orgSlug: string }) {
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
        <CardTitle>Using a token</CardTitle>
        <span className="text-data text-slate-400">
          Replace <code className="text-brand/80">&lt;your-token&gt;</code> with your token value
        </span>
      </CardHeader>
      <CardContent className="pb-3 pt-0">
        <Tabs defaultValue="docker">
          <TabsList className="mb-3">
            <TabsTrigger value="docker">Docker / OCI</TabsTrigger>
            <TabsTrigger value="pip">pip / PyPI</TabsTrigger>
            <TabsTrigger value="npm">npm</TabsTrigger>
            <TabsTrigger value="go">Go modules</TabsTrigger>
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
  const [createBusy, setCreateBusy] = useState(false);
  const [createErr, setCreateErr] = useState('');
  const [rawKey, setRawKey] = useState('');
  const [keyCopied, setKeyCopied] = useState(false);

  // Revoke confirm dialog
  const [revokeTarget, setRevokeTarget] = useState<KeyDTO | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);

  const { toast } = useToast();

  const host = window.location.host;
  const email = user?.email ?? '';
  const orgSlug = activeOrg?.slug ?? '{org}';

  // ── Data loading ───────────────────────────────────────────────────────────

  const load = useCallback(() => {
    setLoading(true);
    setErr('');
    listKeys()
      .then((r) => setKeys(r.keys ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // ── Create flow ────────────────────────────────────────────────────────────

  const handleCreate = async (e: FormEvent) => {
    e.preventDefault();
    setCreateBusy(true);
    setCreateErr('');
    try {
      const key = await createKey({ label: keyLabel.trim() || undefined });
      // Prepend to list even before reveal — so it's there when the dialog closes.
      setKeys((prev) => [key, ...prev]);

      if (key.raw_key) {
        // Advance to the reveal step — this is the one-time window.
        setRawKey(key.raw_key);
        setStep('reveal');
      } else {
        // Server didn't return raw_key — unusual, but don't crash.
        toast({ variant: 'success', title: 'Token created' });
        closeCreate();
      }
    } catch (e: unknown) {
      setCreateErr(e instanceof Error ? e.message : String(e));
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
        title: 'Token revoked',
        description: revokeTarget.label || revokeTarget.prefix,
      });
      setRevokeTarget(null);
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Revoke failed',
        description: e instanceof Error ? e.message : String(e),
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
          <h1 className="text-display font-semibold text-slate-100">Access tokens</h1>
          <p className="mt-0.5 text-data text-slate-400">
            API keys for the registry and package manager proxies.
          </p>
        </div>
        <Button variant="default" size="sm" onClick={() => setCreateOpen(true)}>
          <Plus />
          New token
        </Button>
      </div>

      {/* Usage snippets — always visible, not hidden behind a help link */}
      <UsageSnippets host={host} email={email} orgSlug={orgSlug} />

      {/* ── Tokens table ─────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>Active tokens</CardTitle>
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
                  <TableHead className="w-32">Prefix</TableHead>
                  <TableHead>Label</TableHead>
                  <TableHead className="w-28">Created</TableHead>
                  <TableHead className="w-28">Last used</TableHead>
                  <TableHead className="w-28">Expires</TableHead>
                  <TableHead className="w-20" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {activeKeys.length === 0 ? (
                  <EmptyRow colSpan={6}>
                    No active tokens — create one above.
                  </EmptyRow>
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
                        <span title={isoDate(k.created_at)}>{isoRelative(k.created_at)}</span>
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        {isoRelative(k.last_used_at)}
                      </TableCell>
                      <TableCell className="tnum text-slate-400">
                        {k.expires_at ? (
                          isoDate(k.expires_at)
                        ) : (
                          <span className="text-slate-500">never</span>
                        )}
                      </TableCell>
                      <TableCell>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-slate-500 hover:text-destructive"
                          onClick={() => setRevokeTarget(k)}
                        >
                          Revoke
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
                <DialogTitle>New access token</DialogTitle>
                <DialogDescription>
                  Tokens authenticate against the registry and all protocol proxies.
                </DialogDescription>
              </DialogHeader>

              <form id="create-key-form" onSubmit={(e) => void handleCreate(e)}>
                <div className="space-y-3 px-3 py-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="key-label">
                      Label{' '}
                      <span className="font-normal text-slate-500">(optional)</span>
                    </Label>
                    <Input
                      id="key-label"
                      placeholder="e.g. CI deploy key, laptop"
                      value={keyLabel}
                      onChange={(e) => setKeyLabel(e.target.value)}
                      autoFocus
                    />
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
                  Cancel
                </Button>
                <Button
                  variant="default"
                  size="sm"
                  form="create-key-form"
                  type="submit"
                  disabled={createBusy}
                >
                  {createBusy ? 'Creating…' : 'Create token'}
                </Button>
              </DialogFooter>
            </>
          ) : (
            /* Step 2: one-time key reveal — the critical UX moment */
            <>
              <DialogHeader>
                <DialogTitle>Token created</DialogTitle>
              </DialogHeader>

              <div className="space-y-3 px-3 py-3">
                {/* Amber-bordered warning panel: unmissable, deliberate */}
                <div className="rounded border border-brand/40 bg-brand/5 px-3 py-2.5">
                  <p className="text-label font-semibold uppercase tracking-wider text-brand">
                    Save this token — it will not be shown again
                  </p>
                  <p className="mt-1 text-data text-slate-400">
                    Once you close this dialog, the plaintext is permanently gone. Copy it
                    to your password manager or CI secrets store before continuing.
                  </p>
                </div>

                {/* Token value in a selectable, copyable code block */}
                <div className="space-y-1.5">
                  <Label>Token</Label>
                  <div className="relative">
                    <pre className="select-all overflow-x-auto rounded border border-slate-800 bg-slate-950 p-2.5 text-data text-slate-100 break-all whitespace-pre-wrap">
                      {rawKey}
                    </pre>
                    <button
                      type="button"
                      onClick={copyKey}
                      className="absolute right-1.5 top-1.5 rounded p-1 text-slate-500 transition-all duration-fast hover:bg-slate-800 hover:text-slate-200 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                      aria-label="Copy token"
                      title="Copy token"
                    >
                      {keyCopied ? (
                        <Check className="size-3.5 text-health-up" />
                      ) : (
                        <Copy className="size-3.5" />
                      )}
                    </button>
                  </div>
                  {keyCopied && (
                    <p className="text-data text-health-up">Copied to clipboard.</p>
                  )}
                </div>
              </div>

              {/* Deliberate dismiss: the button wording is a confirmation statement */}
              <DialogFooter>
                <Button variant="default" size="sm" onClick={closeCreate}>
                  I have saved my token
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
            <DialogTitle>Revoke token</DialogTitle>
            <DialogDescription>
              Revoke{' '}
              <span className="font-medium text-slate-100">
                {revokeTarget?.prefix}…
              </span>
              {revokeTarget?.label && (
                <> ({revokeTarget.label})</>
              )}
              ? Any client using this token will lose access immediately and cannot
              be recovered — create a new token to replace it.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setRevokeTarget(null)}
              disabled={revokeBusy}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              size="sm"
              onClick={() => void handleRevoke()}
              disabled={revokeBusy}
            >
              {revokeBusy ? 'Revoking…' : 'Revoke token'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
