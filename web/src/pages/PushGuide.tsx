import { useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';

import { ApiError, createKey } from '@/api/client';
import type { KeyDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { useToast } from '@/hooks/use-toast';
import { translateServerError } from '@/i18n/server-errors';
import { useRegistryHost } from '../hooks/useRegistryHost';

/**
 * errText — the message to show for a failed request.
 *
 * ApiError carries the server's raw English `detail`; translateServerError
 * localises the small allow-list of user-actionable errors and passes anything
 * else through verbatim (see src/i18n/server-errors.ts for why).
 */
function errText(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail) || e.message;
  return e instanceof Error ? e.message : String(e);
}

/**
 * CopyButton — clipboard copy with a transient "✓ Copied" confirmation.
 * Silently degrades if the clipboard API is unavailable (insecure context).
 */
function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  const { t } = useTranslation();
  return (
    <Button
      size="sm"
      variant="outline"
      // Keep an accessible name stable while the label flips to the ✓ state.
      aria-label={done ? t('common.copied') : t('common.copy')}
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
      {done ? `✓ ${t('common.copied')}` : t('common.copy')}
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
  const { t } = useTranslation();

  const [generatedKey, setGeneratedKey] = useState<KeyDTO | null>(null);
  const [creatingKey, setCreatingKey] = useState(false);

  const host = useRegistryHost();
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
        title: t('push.toast.keyCreated'),
        description: t('push.toast.keyCreatedDesc'),
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('push.toast.keyFailed'),
        description: errText(e),
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
        <h1 className="text-display font-semibold text-slate-100">{t('push.title')}</h1>
        <p className="mt-0.5 text-data text-slate-400">
          {/* Two whole sentences rather than a spliced fragment: Chinese puts the
              org before the host, so the "under org …" clause cannot be appended.
              host/org ride in as components, not interpolated values — Trans parses
              the interpolated string as HTML and both fall back to placeholders
              ('<registry-host>', '<org>') that would be eaten as tags. */}
          <Trans
            i18nKey={activeOrg ? 'push.subtitle' : 'push.subtitleNoOrg'}
            components={{
              host: <span className="text-slate-200">{host}</span>,
              org: <span className="text-slate-200">{orgSlug}</span>,
            }}
          />
        </p>
      </div>

      {/* ── Step 1: Authenticate ────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            {/* Step number in amber: structural sequence marker, not decoration */}
            <span className="section-label text-brand">01</span>
            <CardTitle>{t('push.step1.title')}</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-data text-slate-400">
            <Trans
              i18nKey="push.step1.body"
              components={{
                lnk: (
                  <Link
                    to="/tokens"
                    className="rounded text-brand hover:underline focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  />
                ),
              }}
            />
          </p>

          {/* Inline API key generator */}
          {!generatedKey ? (
            <div className="flex items-center justify-between gap-3 rounded border border-slate-800 bg-slate-950 p-2.5">
              <span className="text-data text-slate-400">
                {!activeOrg ? t('push.step1.selectOrgFirst') : t('push.step1.noKeyYet')}
              </span>
              <Button
                size="sm"
                variant="default"
                disabled={creatingKey || !activeOrg}
                onClick={() => void generateKey()}
              >
                {creatingKey ? t('push.step1.creating') : t('push.step1.generate')}
              </Button>
            </div>
          ) : (
            /* One-time key reveal — amber border signals "act now" */
            <div className="space-y-2 rounded border border-brand/25 bg-slate-950 p-2.5">
              <div className="flex items-center justify-between">
                <span className="section-label text-brand">{t('push.step1.keyLabel')}</span>
                <span className="text-micro text-slate-500">
                  {t('push.step1.keyPrefix', { prefix: generatedKey.prefix })}
                </span>
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
                <Trans
                  i18nKey="push.step1.keyWarning"
                  components={{
                    lnk: <Link to="/tokens" className="text-brand hover:underline" />,
                  }}
                />
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
            <CardTitle>{t('push.step2.title')}</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-data text-slate-400">
            {/* The literals live here, not in the locale string — they are
                placeholders the operator substitutes, never translated copy. They
                must be components rather than interpolated values because Trans
                parses the interpolated string as HTML and '<your-image>' would be
                consumed as a tag. */}
            <Trans
              i18nKey="push.step2.body"
              components={{
                image: <code className="text-slate-200">&lt;your-image&gt;</code>,
                repo: <code className="text-slate-200">&lt;repo&gt;</code>,
                tag: <code className="text-slate-200">&lt;tag&gt;</code>,
              }}
            />
          </p>
          <CommandBlock cmd={tagCmd} />
          <p className="text-micro text-slate-500">
            <Trans
              i18nKey="push.step2.note"
              components={{
                b: <strong className="text-slate-400" />,
                lnk: <Link to="/repos" className="text-brand hover:underline" />,
              }}
            />
          </p>
        </CardContent>
      </Card>

      {/* ── Step 3: Push ────────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <span className="section-label text-brand">03</span>
            <CardTitle>{t('push.step3.title')}</CardTitle>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <CommandBlock cmd={pushCmd} />
          <p className="text-data text-slate-400">
            <Trans
              i18nKey="push.step3.body"
              components={{
                lnk: (
                  <Link
                    to="/repos"
                    className="rounded text-brand hover:underline focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                  />
                ),
              }}
            />
          </p>
        </CardContent>
      </Card>

      {/* ── Pull reference (not a numbered step — pulling is the consumer path) */}
      <Card>
        <CardHeader>
          <CardTitle>{t('push.pull.title')}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <CommandBlock cmd={pullCmd} />
          <p className="text-data text-slate-500">
            <Trans
              i18nKey="push.pull.body"
              components={{ c: <code className="text-slate-200" /> }}
            />
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
