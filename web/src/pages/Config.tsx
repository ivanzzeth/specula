import { useEffect, useState } from 'react';
import { getConfig } from '../api/client';
import type { ConfigResponse } from '../api/types';
import Spinner from '../ui/Spinner';

export function Config() {
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    getConfig()
      .then(setConfig)
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-slate-400">
        <Spinner /> Loading config…
      </div>
    );
  }

  return (
    <div className="space-y-5 max-w-3xl">
      <div>
        <h1 className="text-base font-semibold text-slate-100">Config</h1>
        <p className="text-xs text-slate-500 mt-0.5">Read-only — secrets are redacted by the server</p>
      </div>

      {err && <div className="text-red-400 text-sm">{err}</div>}

      {config && (
        <>
          {/* Top-level fields */}
          <div className="rounded-lg border border-slate-800 bg-slate-900 divide-y divide-slate-800">
            {[
              { label: 'Data Plane', value: config.data_plane_addr },
              { label: 'Control Plane', value: config.control_plane_addr },
              { label: 'Blob Driver', value: config.blob_driver },
              { label: 'Meta Driver', value: config.meta_driver },
            ].map(({ label, value }) => (
              <div key={label} className="flex items-center gap-4 px-4 py-2.5">
                <span className="text-[11px] uppercase tracking-wider text-slate-500 w-32 shrink-0">
                  {label}
                </span>
                <span className="text-slate-300 font-mono text-xs">{value || '—'}</span>
              </div>
            ))}
          </div>

          {/* Protocols */}
          {config.protocols && config.protocols.length > 0 && (
            <div className="space-y-3">
              <div className="text-[11px] uppercase tracking-wider text-slate-500">Protocols</div>
              {config.protocols.map((p) => (
                <div key={p.protocol} className="rounded-lg border border-slate-800 bg-slate-900">
                  <div className="px-4 py-2.5 border-b border-slate-800 flex items-center gap-3">
                    <span className="text-brand font-semibold">{p.protocol}</span>
                    <span className="text-xs text-slate-500">
                      TTL {p.mutable_ttl_seconds}s
                    </span>
                    {p.verify_tiers && p.verify_tiers.length > 0 && (
                      <span className="text-xs text-slate-500">
                        tiers: {p.verify_tiers.join(', ')}
                      </span>
                    )}
                  </div>
                  {p.upstreams && p.upstreams.length > 0 && (
                    <table className="w-full text-[13px]">
                      <thead>
                        <tr className="border-b border-slate-800/60">
                          {['Name', 'Base URL', 'Priority', 'Official'].map((h) => (
                            <th
                              key={h}
                              className="px-4 py-2 text-left text-[11px] font-medium uppercase tracking-wider text-slate-600"
                            >
                              {h}
                            </th>
                          ))}
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-slate-800/50">
                        {p.upstreams.map((u) => (
                          <tr key={u.name}>
                            <td className="px-4 py-2 text-slate-300">{u.name}</td>
                            <td className="px-4 py-2 text-slate-400 font-mono text-xs break-all">
                              {u.base_url}
                            </td>
                            <td className="px-4 py-2 text-slate-500 tabular-nums">{u.priority}</td>
                            <td className="px-4 py-2 text-slate-500">
                              {u.official ? (
                                <span className="text-emerald-400">yes</span>
                              ) : (
                                '—'
                              )}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}
