import { lazy, Suspense, useEffect, useState } from 'react';
import { getStats, getStatsSeries } from '../api/client';
import type { StatsResponse, SeriesResponse } from '../api/types';
import Spinner from '../ui/Spinner';

// Recharts loaded lazily so it only pulls in on first navigation to this page.
const Charts = lazy(() => import('./DashboardCharts'));

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-slate-800 bg-slate-900 p-4">
      <div className="text-[11px] uppercase tracking-wider text-slate-500 mb-1">{label}</div>
      <div className="text-2xl font-semibold text-slate-100 tabular-nums">{value}</div>
    </div>
  );
}

export function Dashboard() {
  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [series, setSeries] = useState<SeriesResponse | null>(null);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([getStats(), getStatsSeries()])
      .then(([s, sr]) => {
        setStats(s);
        setSeries(sr);
      })
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-slate-400">
        <Spinner /> Loading stats…
      </div>
    );
  }

  if (err) {
    return <div className="text-red-400 text-sm">{err}</div>;
  }

  if (!stats) return null;

  const diskTotal = stats.backend_disk_free + stats.backend_disk_used;
  const diskPct = diskTotal > 0 ? Math.round((stats.backend_disk_used / diskTotal) * 100) : 0;

  return (
    <div className="space-y-6 max-w-5xl">
      <div>
        <h1 className="text-base font-semibold text-slate-100">Dashboard</h1>
        <p className="text-xs text-slate-500 mt-0.5">Cache statistics overview</p>
      </div>

      {/* Top stat cards */}
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <StatCard label="Total Objects" value={stats.total_objects.toLocaleString()} />
        <StatCard label="Total Bytes" value={fmtBytes(stats.total_bytes)} />
        <StatCard label="Disk Used" value={fmtBytes(stats.backend_disk_used)} />
        <StatCard label="Disk Free" value={fmtBytes(stats.backend_disk_free)} />
      </div>

      {/* Disk usage bar */}
      {diskTotal > 0 && (
        <div className="rounded-lg border border-slate-800 bg-slate-900 p-4">
          <div className="flex justify-between text-[11px] text-slate-500 mb-2 uppercase tracking-wider">
            <span>Disk Usage</span>
            <span>{diskPct}%</span>
          </div>
          <div className="h-2 rounded-full bg-slate-800 overflow-hidden">
            <div
              className="h-full bg-brand transition-all"
              style={{ width: `${diskPct}%` }}
            />
          </div>
          <div className="flex justify-between text-[11px] text-slate-600 mt-1">
            <span>{fmtBytes(stats.backend_disk_used)} used</span>
            <span>{fmtBytes(diskTotal)} total</span>
          </div>
        </div>
      )}

      {/* Per-protocol table */}
      {stats.per_protocol && stats.per_protocol.length > 0 && (
        <div className="rounded-lg border border-slate-800 bg-slate-900 overflow-hidden">
          <div className="px-4 py-3 border-b border-slate-800">
            <span className="text-[11px] uppercase tracking-wider text-slate-500">
              Per-Protocol
            </span>
          </div>
          <table className="w-full text-[13px]">
            <thead>
              <tr className="border-b border-slate-800">
                {['Protocol', 'Objects', 'Bytes', 'Oldest', 'Newest'].map((h) => (
                  <th
                    key={h}
                    className="px-4 py-2 text-left text-[11px] font-medium uppercase tracking-wider text-slate-500"
                  >
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800/70">
              {stats.per_protocol.map((p) => (
                <tr key={p.protocol}>
                  <td className="px-4 py-2.5 text-brand font-medium">{p.protocol}</td>
                  <td className="px-4 py-2.5 text-slate-300 tabular-nums">
                    {p.objects.toLocaleString()}
                  </td>
                  <td className="px-4 py-2.5 text-slate-300 tabular-nums">{fmtBytes(p.bytes)}</td>
                  <td className="px-4 py-2.5 text-slate-500 tabular-nums">
                    {p.oldest_unix > 0 ? new Date(p.oldest_unix * 1000).toLocaleDateString() : '—'}
                  </td>
                  <td className="px-4 py-2.5 text-slate-500 tabular-nums">
                    {p.newest_unix > 0 ? new Date(p.newest_unix * 1000).toLocaleDateString() : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Time-series chart (lazy) */}
      {series && series.points && series.points.length > 0 && (
        <div className="rounded-lg border border-slate-800 bg-slate-900 p-4">
          <div className="text-[11px] uppercase tracking-wider text-slate-500 mb-3">
            Bytes Over Time {series.protocol ? `— ${series.protocol}` : '(all protocols)'}
          </div>
          <Suspense
            fallback={
              <div className="flex items-center gap-2 text-slate-400 h-40">
                <Spinner /> Loading chart…
              </div>
            }
          >
            <Charts points={series.points} />
          </Suspense>
        </div>
      )}
    </div>
  );
}
