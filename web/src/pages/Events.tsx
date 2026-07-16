import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ApiError, getEvents } from '@/api/client';
import { translateServerError } from '@/i18n/server-errors';
import type { VerificationEvent } from '@/api/types';
import { Badge, TierBadge } from '@/components/ui/badge';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { SkeletonRows } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  EmptyRow,
} from '@/components/ui/table';
import { useToast } from '@/hooks/use-toast';
import { cn, formatRelative, formatUnix } from '@/lib/utils';

/**
 * Events — verification/alert list (REGISTRY-DESIGN §5.3).
 *
 * Shows the last 200 verification events: tier failures, digest changes, force-push
 * alerts. Auto-refreshes every 30 seconds so an operator leaving this tab open
 * sees a live feed without a manual reload.
 *
 * Result → badge mapping:
 *   pass → tier-signed (green) — the happy path
 *   warn → tier-tofu  (lemon) — something changed but not a hard failure
 *   fail → health-blocked (red) — a verification failure; warrants action
 *
 * An unknown result renders neutral rather than guessing a severity.
 *
 * Owned by: the Ops UI agent.
 */

// ── Result badge ──────────────────────────────────────────────────────────────

type ResultVariant =
  | 'tier-signed'
  | 'tier-tofu'
  | 'health-blocked'
  | 'default';

function resultVariant(result: string): ResultVariant {
  if (result === 'pass') return 'tier-signed';
  if (result === 'warn') return 'tier-tofu';
  if (result === 'fail') return 'health-blocked';
  return 'default';
}

/** The hint key for a result, falling back to `unknown` for anything unrecognised. */
function resultHintKey(result: string): string {
  if (result === 'pass' || result === 'warn' || result === 'fail') {
    return `events.result.hint.${result}`;
  }
  return 'events.result.hint.unknown';
}

/**
 * The badge VALUE stays English: `result` is an API field literal that also
 * appears in logs and API responses, the same rule TierBadge/HealthBadge follow.
 * The tooltip is the legend, and it is translated.
 */
function ResultBadge({ result }: { result: string }) {
  const { t } = useTranslation();
  return (
    <Badge latin variant={resultVariant(result)} title={t(resultHintKey(result))}>
      {result || 'unknown'}
    </Badge>
  );
}

// ── Digest display — truncated with full on hover ─────────────────────────────

function Digest({ value }: { value: string }) {
  if (!value) return <span className="text-slate-600">—</span>;
  // "sha256:abcdef…ef12" — show the algo prefix + first 8 + last 4 chars of hex.
  const m = value.match(/^([^:]+):(.{8}).*(.{4})$/);
  const display = m ? `${m[1]}:${m[2]}…${m[3]}` : value.slice(0, 16) + '…';
  return (
    <span className="tnum font-mono text-slate-500" title={value}>
      {display}
    </span>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

const LIMIT = 200;
const REFRESH_MS = 30_000;

/** See Upstreams.tsx — an ApiError's `detail` is the server's own words. */
function errText(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail);
  return e instanceof Error ? e.message : String(e);
}

export function Events() {
  const [events, setEvents] = useState<VerificationEvent[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);
  const [lastRefresh, setLastRefresh] = useState(0);
  const { toast } = useToast();
  const { t } = useTranslation();

  const load = useCallback(
    (silent = false) => {
      if (!silent) setLoading(true);
      return getEvents(LIMIT)
        .then((r) => {
          setEvents(r.events ?? []);
          setLastRefresh(Date.now());
          setErr('');
        })
        .catch((e: unknown) => {
          const msg = errText(e);
          setErr(msg);
          if (silent) {
            // Background refresh failure — toast instead of overwriting the list.
            toast({
              variant: 'destructive',
              title: t('events.refreshFailed'),
              description: msg,
            });
          }
        })
        .finally(() => {
          if (!silent) setLoading(false);
        });
    },
    [t, toast]
  );

  // Initial load.
  useEffect(() => {
    void load(false);
  }, [load]);

  // Auto-refresh every 30 s (background, silent — keep the existing list visible).
  useEffect(() => {
    const id = setInterval(() => void load(true), REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  // ── count summary ───────────────────────────────────────────────────────────
  const failCount = events.filter((e) => e.result === 'fail').length;
  const warnCount = events.filter((e) => e.result === 'warn').length;

  return (
    <div className="space-y-3">
      <PageHeading
        total={events.length}
        failCount={failCount}
        warnCount={warnCount}
        lastRefresh={lastRefresh}
      />

      {loading ? (
        <Card>
          <CardContent>
            <SkeletonRows rows={10} />
          </CardContent>
        </Card>
      ) : err && events.length === 0 ? (
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle>{t('events.logTitle')}</CardTitle>
            <p className="text-data text-slate-400">
              {t('events.logSubtitle', {
                limit: LIMIT,
                seconds: REFRESH_MS / 1000,
              })}
            </p>
          </CardHeader>

          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-32">{t('events.col.time')}</TableHead>
                <TableHead className="w-20">{t('events.col.protocol')}</TableHead>
                <TableHead>{t('events.col.artifact')}</TableHead>
                <TableHead className="w-36">{t('events.col.digest')}</TableHead>
                <TableHead className="w-28">{t('events.col.tier')}</TableHead>
                <TableHead className="w-20">{t('events.col.result')}</TableHead>
                <TableHead>{t('events.col.detail')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {events.length === 0 ? (
                <EmptyRow colSpan={7}>{t('events.none')}</EmptyRow>
              ) : (
                events.map((ev) => (
                  <EventRow key={ev.id} ev={ev} />
                ))
              )}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  );
}

// ── Page heading ──────────────────────────────────────────────────────────────

function PageHeading({
  total,
  failCount,
  warnCount,
  lastRefresh,
}: {
  total: number;
  failCount: number;
  warnCount: number;
  lastRefresh: number;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex items-end justify-between gap-3">
      <div>
        <h1 className="text-display font-semibold text-slate-100">
          {t('events.title')}
        </h1>
        <p className="mt-0.5 text-data text-slate-400">{t('events.subtitle')}</p>
      </div>

      {/* Summary lamps: only surface fail/warn so a clean log stays quiet. */}
      {total > 0 && (
        <div className="flex shrink-0 items-center gap-3 text-data">
          {failCount > 0 && (
            <span
              className="tnum text-health-blocked"
              title={t('events.summary.failTitle', { count: failCount })}
            >
              {t('events.summary.fail', { n: failCount })}
            </span>
          )}
          {warnCount > 0 && (
            <span
              className="tnum text-tier-tofu"
              title={t('events.summary.warnTitle', { count: warnCount })}
            >
              {t('events.summary.warn', { n: warnCount })}
            </span>
          )}
          {lastRefresh > 0 && (
            <span className="text-slate-600" title={new Date(lastRefresh).toLocaleString()}>
              {t('events.summary.refreshed', {
                when: formatRelative(Math.floor(lastRefresh / 1000)),
              })}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// ── Event row ─────────────────────────────────────────────────────────────────

function EventRow({ ev }: { ev: VerificationEvent }) {
  const { t } = useTranslation();
  const isFail = ev.result === 'fail';
  return (
    <TableRow className={cn(isFail && 'bg-health-blocked/5')}>
      {/* Time: absolute value + relative on hover */}
      <TableCell
        className="tnum text-slate-500 whitespace-nowrap"
        title={t('events.relativeTitle', { when: formatRelative(ev.unix) })}
      >
        {formatUnix(ev.unix)}
      </TableCell>

      {/* Protocol */}
      <TableCell className="font-medium text-brand whitespace-nowrap">
        {ev.protocol}
      </TableCell>

      {/* Artifact name */}
      <TableCell
        className="max-w-[1px] truncate text-slate-200"
        title={ev.artifact}
      >
        {ev.artifact}
      </TableCell>

      {/* Digest — truncated, full on hover */}
      <TableCell>
        <Digest value={ev.digest} />
      </TableCell>

      {/* Trust tier */}
      <TableCell>
        <TierBadge tier={ev.tier} />
      </TableCell>

      {/* Pass / warn / fail */}
      <TableCell>
        <ResultBadge result={ev.result} />
      </TableCell>

      {/* Free-text detail */}
      <TableCell
        className="max-w-[1px] truncate text-slate-500"
        title={ev.detail || undefined}
      >
        {ev.detail || '—'}
      </TableCell>
    </TableRow>
  );
}
