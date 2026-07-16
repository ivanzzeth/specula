/**
 * Dashboard — the cache overview, and the app's landing route ("/").
 *
 * This is the Cache zone's "Overview" (REGISTRY-DESIGN §5.3): what the proxy is
 * holding right now, how much room is left, and how it grew. It is the first
 * surface an operator sees, so it carries the design language rather than
 * apologising for it.
 *
 * ── INTEGRATION NOTE (R3) ────────────────────────────────────────────────────
 * The stats overview previously existed TWICE: here (pre-R3, off-token) and as
 * Config.tsx's "Overview" tab (on-token, built by the ops agent). One overview,
 * one home: this route owns it, and Config.tsx is now configuration-only. The
 * charts moved to components/charts/CacheCharts.tsx because they are no longer
 * an ops-page detail — they are this page's primary content.
 *
 * HONESTY CONTRACT (REGISTRY-DESIGN §5.0):
 *   · No hit/miss ratio — the API does not expose one. We do not invent it.
 *   · No tier distribution here — that needs per-protocol cache queries; the
 *     Cache Browser answers "was this verified?" per entry instead.
 *   · `backend_disk_*` is the backend store's disk, not necessarily the host's.
 *   · Unknown values render "—", never a fake 0.
 */

import { lazy, Suspense, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';

import { getStats, getStatsSeries } from '@/api/client';
import type { StatsResponse, SeriesResponse } from '@/api/types';
import { Card, CardContent, CardHeader, CardTitle, Readout } from '@/components/ui/card';
import { SkeletonRows } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { formatBytes } from '@/lib/utils';

// recharts is heavy — it only enters the bundle once this route renders.
const CacheCharts = lazy(() => import('@/components/charts/CacheCharts'));

export function Dashboard() {
  const { t } = useTranslation();
  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [series, setSeries] = useState<SeriesResponse | null>(null);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    // The series is supplementary: if it fails, the page is still useful, so it
    // must not take the whole overview down with it.
    Promise.all([getStats(), getStatsSeries().catch(() => null)])
      .then(([s, sr]) => {
        setStats(s);
        setSeries(sr);
      })
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent>
            <SkeletonRows rows={6} />
          </CardContent>
        </Card>
      </div>
    );
  }

  if (err) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      </div>
    );
  }

  if (!stats) return null;

  const diskTotal = stats.backend_disk_free + stats.backend_disk_used;
  const diskPct =
    diskTotal > 0
      ? Math.min(100, Math.round((stats.backend_disk_used / diskTotal) * 100))
      : 0;

  // Gauge colour is DATA, so it comes off the status-lamp ramp, never off the
  // amber accent — amber means "interactive" everywhere else in this UI, and a
  // gauge that turns amber at 80% would read as a control, not a warning.
  const gaugeClass =
    diskPct >= 90 ? 'bg-health-blocked' : diskPct >= 80 ? 'bg-tier-tofu' : 'bg-tier-signed';

  const perProtocol = stats.per_protocol ?? [];
  const hasPerProtocol = perProtocol.length > 0;
  const hasSeries = (series?.points?.length ?? 0) > 0;
  const showCharts = hasPerProtocol || hasSeries;

  return (
    <div className="space-y-3">
      <PageHeading />

      {/* ── Readout tiles ─────────────────────────────────────────────────────
          Hairline grid: the 1px gap IS the border (gap-px over a slate-800
          field), so four tiles read as one instrument cluster rather than four
          floating cards. */}
      <div className="grid grid-cols-2 gap-px border border-slate-800 bg-slate-800 sm:grid-cols-4">
        <Readout
          label={t('dashboard.totalObjects')}
          value={stats.total_objects.toLocaleString()}
          accent
          className="bg-slate-900"
        />
        <Readout
          label={t('dashboard.totalCached')}
          value={formatBytes(stats.total_bytes)}
          className="bg-slate-900"
        />
        <Readout
          label={t('dashboard.diskUsed')}
          value={formatBytes(stats.backend_disk_used)}
          className="bg-slate-900"
        />
        <Readout
          label={t('dashboard.diskFree')}
          value={formatBytes(stats.backend_disk_free)}
          className="bg-slate-900"
        />
      </div>

      {/* ── Disk gauge ────────────────────────────────────────────────────── */}
      {diskTotal > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>{t('dashboard.diskUsage')}</CardTitle>
            <span className="tnum text-data text-slate-400">
              {t('dashboard.diskOf', { pct: diskPct, total: formatBytes(diskTotal) })}
            </span>
          </CardHeader>
          <CardContent>
            <div
              className="h-1.5 w-full overflow-hidden rounded-[1px] bg-slate-800"
              role="meter"
              aria-valuenow={diskPct}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-label={t('dashboard.diskAria', { pct: diskPct })}
            >
              <div className={`h-full transition-all ${gaugeClass}`} style={{ width: `${diskPct}%` }} />
            </div>
            <div className="mt-1.5 flex justify-between text-data text-slate-500">
              <span className="tnum">
                {t('dashboard.usedSuffix', { value: formatBytes(stats.backend_disk_used) })}
              </span>
              <span className="tnum">
                {t('dashboard.totalSuffix', { value: formatBytes(diskTotal) })}
              </span>
            </div>
          </CardContent>
        </Card>
      )}

      {/* ── Per-protocol table ────────────────────────────────────────────── */}
      {hasPerProtocol && (
        <Card>
          <CardHeader>
            <CardTitle>{t('dashboard.perProtocol')}</CardTitle>
            <p className="text-data text-slate-400">{t('dashboard.perProtocolHint')}</p>
          </CardHeader>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('dashboard.colProtocol')}</TableHead>
                <TableHead className="w-28 text-right">{t('dashboard.colObjects')}</TableHead>
                <TableHead className="w-28 text-right">{t('dashboard.colBytes')}</TableHead>
                <TableHead className="w-32 text-right">{t('dashboard.colOldest')}</TableHead>
                <TableHead className="w-32 text-right">{t('dashboard.colNewest')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {perProtocol.map((p) => (
                <TableRow key={p.protocol}>
                  <TableCell className="font-medium">
                    {/* The protocol name is the way into the Cache Browser —
                        "6,412 npm objects" is a number until you can open it. */}
                    <Link
                      to={`/cache/${p.protocol}`}
                      className="text-brand transition-colors duration-fast hover:text-slate-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                    >
                      {p.protocol}
                    </Link>
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-300">
                    {p.objects.toLocaleString()}
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-300">
                    {formatBytes(p.bytes)}
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-500">
                    {p.oldest_unix > 0 ? new Date(p.oldest_unix * 1000).toLocaleDateString() : '—'}
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-500">
                    {p.newest_unix > 0 ? new Date(p.newest_unix * 1000).toLocaleDateString() : '—'}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}

      {/* ── Charts (lazy) ─────────────────────────────────────────────────── */}
      {showCharts && (
        <Card>
          <CardHeader>
            <CardTitle>{t('dashboard.charts')}</CardTitle>
            <p className="text-data text-slate-400">{t('dashboard.chartsHint')}</p>
          </CardHeader>
          <CardContent>
            <Suspense
              fallback={
                <div className="space-y-2 py-2">
                  <SkeletonRows rows={5} />
                </div>
              }
            >
              <CacheCharts protocolStats={perProtocol} seriesPoints={series?.points ?? []} />
            </Suspense>
          </CardContent>
        </Card>
      )}

      {!showCharts && (
        <Card>
          <CardContent className="text-data text-slate-400">{t('dashboard.empty')}</CardContent>
        </Card>
      )}
    </div>
  );
}

function PageHeading() {
  const { t } = useTranslation();
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">{t('dashboard.title')}</h1>
      <p className="mt-0.5 text-data text-slate-400">{t('dashboard.subtitle')}</p>
    </div>
  );
}
