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
 * Kind separates maturity cool-down policy hits from TOFU digest drift even when
 * both share warn/fail severity.
 */

type ResultVariant =
  | 'tier-signed'
  | 'tier-tofu'
  | 'health-blocked'
  | 'default'
  | 'outline'
  | 'accent';

function resultVariant(result: string): ResultVariant {
  if (result === 'pass') return 'tier-signed';
  if (result === 'warn') return 'tier-tofu';
  if (result === 'fail') return 'health-blocked';
  return 'default';
}

function resultHintKey(result: string): string {
  if (result === 'pass' || result === 'warn' || result === 'fail') {
    return `events.result.hint.${result}`;
  }
  return 'events.result.hint.unknown';
}

function ResultBadge({ result }: { result: string }) {
  const { t } = useTranslation();
  return (
    <Badge latin variant={resultVariant(result)} title={t(resultHintKey(result))}>
      {result || 'unknown'}
    </Badge>
  );
}

/** Normalize API kind; fall back to detail heuristics for older payloads. */
export function eventKind(ev: Pick<VerificationEvent, 'kind' | 'detail'>): string {
  if (ev.kind === 'maturity' || ev.kind === 'tofu' || ev.kind === 'verify') {
    return ev.kind;
  }
  const d = ev.detail ?? '';
  if (d.includes('DIGEST CHANGED')) return 'tofu';
  if (d.includes('maturity: version too young')) return 'maturity';
  if (d.includes('maturity:')) {
    return d.includes('tofu:') ? 'tofu' : 'maturity';
  }
  if (d.includes('tofu:')) return 'tofu';
  return 'verify';
}

function kindVariant(kind: string): ResultVariant {
  if (kind === 'maturity') return 'accent';
  if (kind === 'tofu') return 'tier-tofu';
  return 'outline';
}

function KindBadge({ kind }: { kind: string }) {
  const { t } = useTranslation();
  const hintKey = `events.kind.hint.${kind}`;
  return (
    <Badge latin variant={kindVariant(kind)} title={t(hintKey)}>
      {kind}
    </Badge>
  );
}

function Digest({ value }: { value: string }) {
  if (!value) return <span className="text-slate-600">—</span>;
  const m = value.match(/^([^:]+):(.{8}).*(.{4})$/);
  const display = m ? `${m[1]}:${m[2]}…${m[3]}` : value.slice(0, 16) + '…';
  return (
    <span className="tnum font-mono text-slate-500" title={value}>
      {display}
    </span>
  );
}

const LIMIT = 200;
const REFRESH_MS = 30_000;

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

  useEffect(() => {
    void load(false);
  }, [load]);

  useEffect(() => {
    const id = setInterval(() => void load(true), REFRESH_MS);
    return () => clearInterval(id);
  }, [load]);

  const failCount = events.filter((e) => e.result === 'fail').length;
  const warnCount = events.filter((e) => e.result === 'warn').length;
  const maturityCount = events.filter((e) => eventKind(e) === 'maturity').length;
  const tofuCount = events.filter((e) => eventKind(e) === 'tofu').length;

  return (
    <div className="space-y-3">
      <PageHeading
        total={events.length}
        failCount={failCount}
        warnCount={warnCount}
        maturityCount={maturityCount}
        tofuCount={tofuCount}
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
                <TableHead className="w-24">{t('events.col.kind')}</TableHead>
                <TableHead className="w-20">{t('events.col.result')}</TableHead>
                <TableHead>{t('events.col.detail')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {events.length === 0 ? (
                <EmptyRow colSpan={8}>{t('events.none')}</EmptyRow>
              ) : (
                events.map((ev) => <EventRow key={ev.id} ev={ev} />)
              )}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  );
}

function PageHeading({
  total,
  failCount,
  warnCount,
  maturityCount,
  tofuCount,
  lastRefresh,
}: {
  total: number;
  failCount: number;
  warnCount: number;
  maturityCount: number;
  tofuCount: number;
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

      {total > 0 && (
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-3 text-data">
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
          {maturityCount > 0 && (
            <span
              className="tnum text-brand"
              title={t('events.summary.maturityTitle', { count: maturityCount })}
            >
              {t('events.summary.maturity', { n: maturityCount })}
            </span>
          )}
          {tofuCount > 0 && (
            <span
              className="tnum text-slate-300"
              title={t('events.summary.tofuTitle', { count: tofuCount })}
            >
              {t('events.summary.tofu', { n: tofuCount })}
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

function EventRow({ ev }: { ev: VerificationEvent }) {
  const { t } = useTranslation();
  const isFail = ev.result === 'fail';
  const kind = eventKind(ev);
  return (
    <TableRow className={cn(isFail && 'bg-health-blocked/5')}>
      <TableCell
        className="tnum text-slate-500 whitespace-nowrap"
        title={t('events.relativeTitle', { when: formatRelative(ev.unix) })}
      >
        {formatUnix(ev.unix)}
      </TableCell>

      <TableCell className="font-medium text-brand whitespace-nowrap">
        {ev.protocol}
      </TableCell>

      <TableCell className="max-w-[1px] truncate text-slate-200" title={ev.artifact}>
        {ev.artifact}
      </TableCell>

      <TableCell>
        <Digest value={ev.digest} />
      </TableCell>

      <TableCell>
        <TierBadge tier={ev.tier} />
      </TableCell>

      <TableCell>
        <KindBadge kind={kind} />
      </TableCell>

      <TableCell>
        <ResultBadge result={ev.result} />
      </TableCell>

      <TableCell className="max-w-[1px] truncate text-slate-500" title={ev.detail || undefined}>
        {ev.detail || '—'}
      </TableCell>
    </TableRow>
  );
}
