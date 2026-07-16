import { useState } from 'react';
import { Link } from 'react-router-dom';

import { createKey } from '@/api/client';
import type { KeyDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { useToast } from '@/hooks/use-toast';

/**
 * CopyButton — clipboard copy with a transient "✓ Copied" confirmation.
 * Silently degrades if the clipboard API is unavailable (insecure context).
 */
function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  return (
    <Button
      size="sm"
      variant="outline"
      onClick={() => {
        void navigator.clipboard
          .writeText(text)
          .then(() => {
            setDone(true);
            setTimeout(() => setDone(false), 1800);
          })
          .catch(() => {
            // clipboard unavailable — degrade silently
          });
      }}
    >
      {done ? '✓ Copied' : 'Copy'}
    </Button>
  );
}

/**
 * CommandBlock — a preformatted code block with an inline copy button.
 *
 * Scrolls on its own axis so long commands don't break the page layout.
 * Uses bg-slate-950 (one step darker than the card surface) to read as an
 * inset panel, not a floating element.
 */
function CommandBlock({ cmd }: { cmd: string }) {
  return (
    <div className="flex items-start gap-2 overflow-x-auto rounded border border-slate-800 bg-slate-950 p-2.5">
      <pre className="flex-1 whitespace-pre-wrap break-all text-data text-slate-200">{cmd}</pre>
      <div className="shrink-0">
        <CopyButton text={cmd} />
      </div>
    </div>
  );
}

/**
 * Push guide — REGISTRY-DESIGN §5.1.
 *
 * Three numbered steps: authenticate → tag → push. All commands are pre-filled
 * with the active org's namespace and the current user's email so an operator
 * can copy-and-run without substituting values by hand.
 *
 * Step 1 includes an inline API key generator: a key is shown exactly once at
 * creation and never again. The "Generate key" flow is kept in-page so the
 * operator does not have to navigate away mid-setup.
 *
 * Owned by: Agent 1 · Registry.
 */
export function PushGuide() {
  const { user } = useAuth();
  const { activeOrg } = useOrg();
  const { toast } = useToast();

  const [generatedKey, setGeneratedKey] = useState<KeyDTO | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  const host = typeof window !== 'undefined' ? window.location.host : '<host>';
  const orgSlug = activeOrg?.slug ?? '<org>';
  const email = user?.email ?? '<email>';
  // Until a key is generated, keep the placeholder literal so the operator
  // can see exactly what to substitute rather than silently getting a broken command.
  const apiKey = generatedKey?.raw_key ?? '<api-key>';

  const loginCmd = `docker login ${host} -u ${email} -p ${apiKey}`;
  const tagCmd = `docker tag <your-image>:<tag> ${host}/${orgSlug}/<repo>:<tag>`;
  const pushCmd = `docker push ${host}/${orgSlug}/<repo>:<tag>`;
  const pullCmd = `docker pull ${host}/${orgSlug}/<repo>:<tag>`;

  const generateKey = async () => {
    setCreatingKey(true);
    try {
      const key = await createKey({ label: 'registry-push' });
      setGeneratedKey(key);
      toast({
        variant: 'success',
        title: 'API key created',
        description: "Copy it now — it won't be shown again.",
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Could not create key',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setCreatingKey(false);
    }
  };

  return (
    <div className="max-w-2xl space-y-4">
      {/* Page header */}
      <div>
        <h1 className="text-display font-semibold text-slate-100">Push guide</h1>
        <p className="mt-0.5 text-data text-slate-400">
          Three commands to push a private image to{' '}
          <span className="text-slate-200">{host}</span>
          {activeOrg && (
            <>
              {' '}
              under org <span className="text-slate-200">{orgSlug}</span>
            </>
          )}
          .
        </p>
      </div>

      {/* ── Step 1: Authenticate ────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            {/* Step number in amber: structural sequence marker, not decoration */}
            <span className="section-label text-brand">01</span>
            <CardTitle>Authenticate</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-data text-slate-400">
            Log in with your Specula email and an API key. Keys are org-scoped — generate
            one here, or manage existing keys on the{' '}
            <Link
              to="/tokens"
              className="rounded text-brand hover:underline focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
            >
              Tokens
            </Link>{' '}
            page.
          </p>

          {/* Inline API key generator */}
          {!generatedKey ? (
            <div className="flex items-center justify-between gap-3 rounded border border-slate-800 bg-slate-950 p-2.5">
              <span className="text-data text-slate-400">
                {!activeOrg
                  ? 'Select an active org first (identity rail above).'
                  : "Don't have an API key yet?"}
              </span>
              <Button
                size="sm"
                variant="default"
                disabled={creatingKey || !activeOrg}
                onClick={() => void generateKey()}
              >
                {creatingKey ? 'Creating…' : 'Generate key'}
              </Button>
            </div>
          ) : (
            /* One-time key reveal — amber border signals "act now" */
            <div className="space-y-2 rounded border border-brand/25 bg-slate-950 p-2.5">
              <div className="flex items-center justify-between">
                <span className="section-label text-brand">api key · shown once</span>
                <span className="text-micro text-slate-500">prefix: {generatedKey.prefix}…</span>
              </div>
              <div className="flex items-start gap-2">
                <code className="tnum flex-1 break-all text-data text-slate-100">
                  {generatedKey.raw_key}
                </code>
                <div className="shrink-0">
                  <CopyButton text={generatedKey.raw_key ?? ''} />
                </div>
              </div>
              <p className="text-micro text-slate-500">
                Save this key — it will not be shown again. Revoke it from the{' '}
                <Link to="/tokens" className="text-brand hover:underline">
                  Tokens
                </Link>{' '}
                page if compromised. You can generate additional keys there too.
              </p>
            </div>
          )}

          <CommandBlock cmd={loginCmd} />
        </CardContent>
      </Card>

      {/* ── Step 2: Tag ─────────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <span className="section-label text-brand">02</span>
            <CardTitle>Tag your image</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-data text-slate-400">
            Retag your local image with the Specula registry prefix. Replace{' '}
            <code className="text-slate-200">&lt;your-image&gt;</code>,{' '}
            <code className="text-slate-200">&lt;repo&gt;</code>, and{' '}
            <code className="text-slate-200">&lt;tag&gt;</code> with your values.
          </p>
          <CommandBlock cmd={tagCmd} />
          <p className="text-micro text-slate-500">
            A repository is created on first push and defaults to{' '}
            <strong className="text-slate-400">private</strong>. Change visibility on the{' '}
            <Link to="/repos" className="text-brand hover:underline">
              Repositories
            </Link>{' '}
            page.
          </p>
        </CardContent>
      </Card>

      {/* ── Step 3: Push ────────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <span className="section-label text-brand">03</span>
            <CardTitle>Push</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <CommandBlock cmd={pushCmd} />
          <p className="text-data text-slate-400">
            The image appears in{' '}
            <Link
              to="/repos"
              className="rounded text-brand hover:underline focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
            >
              Repositories
            </Link>{' '}
            after a successful push with its tag, digest, and manifest size.
          </p>
        </CardContent>
      </Card>

      {/* ── Pull reference (not a numbered step — pulling is the consumer path) */}
      <Card>
        <CardHeader>
          <CardTitle>Pull reference</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <CommandBlock cmd={pullCmd} />
          <p className="text-data text-slate-500">
            Public repos allow anonymous pull. Private repos require{' '}
            <code className="text-slate-200">docker login</code> first. Per-tag pull commands
            are available on each repository's detail page.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
