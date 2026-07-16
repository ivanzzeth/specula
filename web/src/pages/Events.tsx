import { useEffect, useState } from 'react';
import { getEvents } from '../api/client';
import type { VerificationEvent } from '../api/types';
import { Badge, resultTone } from '../ui/Badge';
import Spinner from '../ui/Spinner';

export function Events() {
  const [events, setEvents] = useState<VerificationEvent[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    getEvents(100)
      .then((r) => setEvents(r.events ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-slate-400">
        <Spinner /> Loading events…
      </div>
    );
  }

  return (
    <div className="space-y-4 max-w-5xl">
      <div>
        <h1 className="text-base font-semibold text-slate-100">Verification Events</h1>
        <p className="text-xs text-slate-500 mt-0.5">Recent artifact verification results</p>
      </div>

      {err && <div className="text-red-400 text-sm">{err}</div>}

      <div className="rounded-lg border border-slate-800 bg-slate-900 overflow-x-auto">
        <table className="w-full text-[13px]">
          <thead>
            <tr className="border-b border-slate-800">
              {['Time', 'Protocol', 'Artifact', 'Digest', 'Tier', 'Result', 'Detail'].map((h) => (
                <th
                  key={h}
                  className="px-3 py-2 text-left text-[11px] font-medium uppercase tracking-wider text-slate-500 whitespace-nowrap"
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-800/70">
            {events.length === 0 ? (
              <tr>
                <td colSpan={7} className="px-3 py-10 text-center text-slate-500">
                  No verification events
                </td>
              </tr>
            ) : (
              events.map((ev) => (
                <tr key={ev.id}>
                  <td className="px-3 py-2 text-slate-500 whitespace-nowrap tabular-nums text-xs">
                    {new Date(ev.unix * 1000).toLocaleString()}
                  </td>
                  <td className="px-3 py-2 text-brand font-medium whitespace-nowrap">
                    {ev.protocol}
                  </td>
                  <td className="px-3 py-2 text-slate-300 font-mono text-xs max-w-[160px] truncate">
                    {ev.artifact}
                  </td>
                  <td className="px-3 py-2 text-slate-500 font-mono text-xs max-w-[120px] truncate">
                    {ev.digest}
                  </td>
                  <td className="px-3 py-2 text-slate-400 text-xs whitespace-nowrap">{ev.tier}</td>
                  <td className="px-3 py-2 whitespace-nowrap">
                    <Badge tone={resultTone(ev.result)}>{ev.result}</Badge>
                  </td>
                  <td className="px-3 py-2 text-slate-500 text-xs max-w-[200px] truncate">
                    {ev.detail || '—'}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
