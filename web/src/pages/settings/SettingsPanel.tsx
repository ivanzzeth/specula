import { useCallback, useEffect, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { KeyRound, RotateCcw, TriangleAlert } from 'lucide-react';

import { ApiError, deleteSetting, getSettings, putSetting } from '@/api/client';
import type { SettingSource, SettingView } from '@/api/types';
import { translateServerError } from '@/i18n/server-errors';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { SkeletonRows } from '@/components/ui/skeleton';
import { useToast } from '@/hooks/use-toast';
import { cn } from '@/lib/utils';

/**
 * SettingsPanel — writable runtime settings (the settings layer ported from
 * ai-sandbox).
 *
 * This is the counterpart to the read-only Config view below it on the same
 * page, and the distinction is the whole point of the page's information
 * architecture:
 *
 *   Config   — what the operator wrote in specula.yaml. An echo. Read-only.
 *   Settings — what is ACTUALLY in effect right now, and the subset of it you
 *              can change without a redeploy.
 *
 * Four things every row must state honestly, because each is a real operational
 * trap this UI exists to close:
 *
 *   SOURCE  — an override in the encrypted store beats the config file. Without
 *             this, an operator edits specula.yaml, restarts, sees no change,
 *             and has no way to discover why.
 *   SECRET  — a redacted setting has NO plaintext to show. The server never
 *             sends one; this component never asks for one.
 *   HOT     — whether saving takes effect now or needs a restart. Saying
 *             "saved!" for a value that will not apply until a restart is a lie.
 *   DANGER  — a high-risk key gets an explicit typed confirmation, not a
 *             one-click save.
 *
 * Design: the established instrument-panel language — hairline rows, amber only
 * on the one primary action per row, status colour reserved for meaning.
 *
 * Owned by: the Ops UI agent.
 */
/** errMessage routes API errors through the shared server-error allow-list. */
function errMessage(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail) || e.message;
  return e instanceof Error ? e.message : String(e);
}

export function SettingsPanel() {
  const { t } = useTranslation();
  const [settings, setSettings] = useState<SettingView[]>([]);
  const [configEnabled, setConfigEnabled] = useState(true);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  const load = useCallback(() => {
    getSettings()
      .then((r) => {
        setSettings(r.settings ?? []);
        setConfigEnabled(r.config_enabled);
      })
      .catch((e: unknown) => setErr(errMessage(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(load, [load]);

  /** replace swaps one setting in place from a mutation's response. */
  const replace = (updated: SettingView) =>
    setSettings((prev) => prev.map((s) => (s.key === updated.key ? updated : s)));

  if (loading) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>{t('settings.title')}</CardTitle>
        </CardHeader>
        <CardContent>
          <SkeletonRows rows={4} />
        </CardContent>
      </Card>
    );
  }

  if (err) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>{t('settings.title')}</CardTitle>
        </CardHeader>
        <CardContent className="text-data text-destructive">{err}</CardContent>
      </Card>
    );
  }

  const restartPending = settings.some((s) => s.restart_required);

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('settings.title')}</CardTitle>
        <p className="text-data text-slate-400">{t('settings.subtitle')}</p>
      </CardHeader>

      {/* ── Store-disabled notice ──────────────────────────────────────────
          Explaining this up front is the difference between a read-only page
          and a page that looks broken when every save 503s. */}
      {!configEnabled && (
        <div className="flex items-start gap-2 border-b border-slate-800 bg-status-warn/5 px-3 py-2">
          <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-warn" />
          <p className="text-data text-slate-300">
            <span className="font-semibold text-status-warn">{t('settings.readOnlyLabel')}</span>{' '}
            {/* The three <code> spans are config keys and a shell command — they
                are identical in every locale, so only the prose around them moves. */}
            <Trans
              i18nKey="settings.readOnlyBody"
              components={[
                <code key="k" className="text-slate-100" />,
                <code key="env" className="text-slate-100" />,
                <code key="cmd" className="text-slate-100" />,
              ]}
            />
          </p>
        </div>
      )}

      {restartPending && (
        <div className="flex items-start gap-2 border-b border-slate-800 bg-status-info/5 px-3 py-2">
          <RotateCcw className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-info" />
          <p className="text-data text-slate-300">
            <span className="font-semibold text-status-info">
              {t('settings.restartPendingLabel')}
            </span>{' '}
            {t('settings.restartPendingBody')}
          </p>
        </div>
      )}

      <div className="divide-y divide-slate-800">
        {settings.map((s) => (
          <SettingRow
            key={s.key}
            setting={s}
            writable={configEnabled}
            onChanged={replace}
          />
        ))}
        {settings.length === 0 && (
          <CardContent className="text-data text-slate-400">{t('settings.empty')}</CardContent>
        )}
      </div>
    </Card>
  );
}

// ── Source badge ──────────────────────────────────────────────────────────────

/**
 * SOURCE_META maps the server's source enum onto operator-facing language.
 *
 * The wire values are the ported layer's (runtime/env/unset); the labels are
 * what an operator of THIS product actually needs to read. "runtime" means
 * nothing to someone looking for why their YAML is being ignored — "override"
 * does.
 */
const SOURCE_META: Record<SettingSource, { key: string; cls: string }> = {
  runtime: {
    key: 'runtime',
    cls: 'border-status-info/40 bg-status-info/10 text-status-info',
  },
  env: {
    key: 'env',
    cls: 'border-slate-700 bg-slate-800 text-slate-300',
  },
  unset: {
    key: 'unset',
    cls: 'border-slate-800 bg-transparent text-slate-500',
  },
};

function SourceBadge({ source }: { source: SettingSource }) {
  const { t } = useTranslation();
  const meta = SOURCE_META[source] ?? SOURCE_META.unset;
  return (
    <span
      title={t(`settings.source.${meta.key}Hint`)}
      className={cn(
        'label-caps inline-flex items-center rounded-[2px] border px-1.5 py-0.5',
        'text-micro font-semibold whitespace-nowrap',
        meta.cls
      )}
    >
      {t(`settings.source.${meta.key}`)}
    </span>
  );
}

// ── One setting row ───────────────────────────────────────────────────────────

function SettingRow({
  setting,
  writable,
  onChanged,
}: {
  setting: SettingView;
  writable: boolean;
  onChanged: (s: SettingView) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const { toast } = useToast();
  const { t } = useTranslation();

  const isSecret = setting.secret;
  // A dangerous key demands the operator type its name. Deliberately more
  // friction than a checkbox: replacing the registry token key breaks every
  // in-flight docker push, and that should not be one stray click away.
  const needsConfirm = !!setting.dangerous;
  const confirmed = !needsConfirm || confirm === setting.key;

  const startEdit = () => {
    // A secret has no plaintext to prefill — the server never sent one. Starting
    // from empty is the honest state: you are REPLACING it, not editing it.
    setDraft(isSecret ? '' : (setting.value ?? ''));
    setConfirm('');
    setEditing(true);
  };

  const cancel = () => {
    setEditing(false);
    setDraft('');
    setConfirm('');
  };

  const save = async () => {
    setBusy(true);
    try {
      const updated = await putSetting(setting.key, { value: draft });
      onChanged(updated);
      cancel();
      toast({
        variant: 'success',
        title: t('settings.saved'),
        description: updated.hot_reload
          ? t('settings.savedHot', { key: setting.key })
          : t('settings.savedCold', { key: setting.key }),
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('settings.saveFailed'),
        description: errMessage(e),
        duration: Infinity,
      });
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    setBusy(true);
    try {
      const updated = await deleteSetting(setting.key);
      onChanged(updated);
      cancel();
      toast({
        variant: 'success',
        title: t('settings.overrideCleared'),
        description: t('settings.overrideClearedDesc', { key: setting.key }),
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('settings.clearFailed'),
        description: errMessage(e),
        duration: Infinity,
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="px-3 py-2.5">
      {/* ── header: key + flags ────────────────────────────────────────── */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-data font-medium text-slate-100">{setting.key}</span>
        <SourceBadge source={setting.source} />

        {isSecret && (
          <Badge variant="outline" title={t('settings.secretHint')}>
            <KeyRound className="h-3 w-3" />
            {t('settings.secret')}
          </Badge>
        )}
        {setting.dangerous && (
          <Badge variant="tier-checksum" title={t('settings.dangerHint')}>
            <TriangleAlert className="h-3 w-3" />
            {t('settings.danger')}
          </Badge>
        )}
        {!setting.hot_reload && (
          <span
            className="label-caps text-micro text-slate-500"
            title={t('settings.restartToApplyHint')}
          >
            {t('settings.restartToApply')}
          </span>
        )}
        {setting.restart_required && (
          <Badge variant="tier-consensus" title={t('settings.restartPendingBadgeHint')}>
            {t('settings.restartPendingBadge')}
          </Badge>
        )}
      </div>

      {setting.desc && (
        <p className="mt-1 max-w-3xl text-data leading-relaxed text-slate-400">
          {setting.desc}
        </p>
      )}

      {/* ── value + actions ────────────────────────────────────────────── */}
      {!editing ? (
        <div className="mt-1.5 flex items-center gap-3">
          <span className="min-w-0 flex-1 truncate text-data text-slate-300">
            {isSecret ? (
              setting.set ? (
                <span className="text-slate-400">{setting.display}</span>
              ) : (
                <span className="text-slate-600">{t('settings.notSet')}</span>
              )
            ) : setting.value ? (
              <span className="tnum text-slate-200">{setting.value}</span>
            ) : (
              <span className="text-slate-600">—</span>
            )}
          </span>

          {writable && (
            <div className="flex shrink-0 items-center gap-1.5">
              {setting.source === 'runtime' && (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={reset}
                  disabled={busy}
                  title={t('settings.resetHint')}
                >
                  {t('common.reset')}
                </Button>
              )}
              <Button variant="outline" size="sm" onClick={startEdit} disabled={busy}>
                {isSecret ? t('settings.replace') : t('common.edit')}
              </Button>
            </div>
          )}
        </div>
      ) : (
        <div className="mt-2 space-y-2">
          <div className="flex items-center gap-2">
            <Input
              autoFocus
              type={isSecret ? 'password' : 'text'}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder={
                isSecret
                  ? t('settings.secretPlaceholder')
                  : t('settings.valuePlaceholder', { kind: setting.kind })
              }
              className="flex-1"
              onKeyDown={(e) => {
                if (e.key === 'Escape') cancel();
                if (e.key === 'Enter' && confirmed && !busy) void save();
              }}
            />
            <Button size="sm" onClick={save} disabled={busy || !confirmed}>
              {t('common.save')}
            </Button>
            <Button variant="ghost" size="sm" onClick={cancel} disabled={busy}>
              {t('common.cancel')}
            </Button>
          </div>

          {needsConfirm && (
            <div className="flex items-start gap-2 border border-status-danger/30 bg-status-danger/5 px-2.5 py-2">
              <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-danger" />
              <div className="min-w-0 flex-1 space-y-1.5">
                <p className="text-data text-slate-300">
                  <Trans
                    i18nKey="settings.confirmBody"
                    values={{ key: setting.key }}
                    components={[<code key="k" className="text-slate-100" />]}
                  />
                </p>
                <Input
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                  placeholder={setting.key}
                  aria-label={t('settings.confirmAria', { key: setting.key })}
                />
              </div>
            </div>
          )}

          {!setting.hot_reload && (
            <p className="text-data text-slate-500">{t('settings.restartNote')}</p>
          )}
        </div>
      )}
    </div>
  );
}
