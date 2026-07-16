import { useEffect, useState } from 'react';
import { getUpstreams } from '../api/client';
import type { UpstreamHealth } from '../api/types';
import { Badge } from '../ui/Badge';
import Spinner from '../ui/Spinner';

export function Upstreams() {
  const [upstreams, setUpstreams] = useState<UpstreamHealth[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    getUpstreams()
      .then((r) => setUpstreams(r.upstreams ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-slate-400">
        <Spinner /> Loading upstreams…
      </div>
    );
  }

  return (
    <div className="space-y-4 max-w-4xl">
      <div>
        <h1 className="text-base font-semibold text-slate-100">Upstreams</h1>
        <p className="text-xs text-slate-500 mt-0.5">Upstream registry health</p>
      </div>

      {err && <div className="text-red-400 text-sm">{err}</div>}

      <div className="rounded-lg border border-slate-800 bg-slate-900 overflow-hidden">
        <table className="w-full text-[13px]">
          <thead>
            <tr className="border-b border-slate-800">
              {['Protocol', 'URL', 'Status', 'Last Error'].map((h) => (
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
            {upstreams.length === 0 ? (
              <tr>
                <td colSpan={4} className="px-4 py-10 text-center text-slate-500">
                  No upstreams configured
                </td>
              </tr>
            ) : (
              upstreams.map((u, i) => (
                <tr key={i}>
                  <td className="px-4 py-2.5 text-brand font-medium">{u.protocol}</td>
                  <td className="px-4 py-2.5 text-slate-300 font-mono text-xs break-all">
                    {u.url}
                  </td>
                  <td className="px-4 py-2.5">
                    {u.blocked ? (
                      <Badge tone="red">Blocked</Badge>
                    ) : (
                      <Badge tone="green">OK</Badge>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-slate-500 text-xs">
                    {u.last_err || '—'}
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
