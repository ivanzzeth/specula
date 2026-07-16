import { useEffect, useState } from 'react';

import { getConfig } from '@/api/client';
import type { ConfigResponse } from '@/api/types';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { SkeletonRows } from '@/components/ui/skeleton';
import { SettingsPanel } from '@/pages/settings/SettingsPanel';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

/**
 * Config — read-only server configuration (REGISTRY-DESIGN §5.3).
 *
 * Drivers, data-plane/control-plane addresses, and the enabled protocol list.
 * Secrets are redacted server-side.
 *
 * ── INTEGRATION NOTE (R3) ────────────────────────────────────────────────────
 * This page used to carry an "Overview" tab duplicating the cache stats that
 * Dashboard.tsx ("/", Cache → Overview) also rendered. Two pages owning one
 * question is an IA bug, not a feature: the overview now lives only on the
 * Dashboard, and this page answers only "how is this server configured?".
 * With one tab left, the Tabs shell went too — a single tab is just a heading.
 *
 * ── SETTINGS vs CONFIG (settings-layer port) ─────────────────────────────────
 * The page now leads with SettingsPanel — the runtime settings an operator can
 * actually CHANGE (persisted encrypted, shared by every replica) — and keeps the
 * read-only config echo beneath it. They answer two different questions and the
 * order reflects which one an operator arrives here to ask:
 *
 *   Settings — "what is in effect, and what can I change right now?"  (writable)
 *   Config   — "how was this server started?"                        (read-only)
 *
 * Crucially, a setting's SOURCE is shown: a runtime override beats the config
 * file, so without it an operator can edit specula.yaml, restart, see nothing
 * change, and have no way to find out why.
 *
 * HONESTY CONTRACT (REGISTRY-DESIGN §5.0):
 *   · Values are the server's live, redacted view — not the YAML on disk.
 *   · A secret's plaintext is never sent to the browser, so it is never shown.
 *
 * Owned by: the Ops UI agent.
 */

// ── Main page ─────────────────────────────────────────────────────────────────

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

  // The two panels load independently: a failure to read the startup config
  // echo must not hide the settings an operator came here to change.
  return (
    <div className="space-y-3">
      <PageHeading />

      <SettingsPanel />

      <div className="pt-1">
        <p className="section-label">Startup configuration (read-only)</p>
      </div>

      {loading ? (
        <Card>
          <CardContent>
            <SkeletonRows rows={8} />
          </CardContent>
        </Card>
      ) : err ? (
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      ) : (
        <ConfigTab config={config} />
      )}
    </div>
  );
}

function PageHeading() {
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">Config</h1>
      <p className="mt-0.5 text-data text-slate-400">
        What this server is running. Settings are changeable at runtime; the startup
        configuration below is read-only. Secrets are redacted by the server.
      </p>
    </div>
  );
}

// ── Config tab ────────────────────────────────────────────────────────────────

function ConfigTab({ config }: { config: ConfigResponse | null }) {
  if (!config) {
    return (
      <Card>
        <CardContent className="text-data text-slate-400">
          Configuration not available.
        </CardContent>
      </Card>
    );
  }

  const topFields = [
    { label: 'Data Plane', value: config.data_plane_addr },
    { label: 'Control Plane', value: config.control_plane_addr },
    { label: 'Blob Driver', value: config.blob_driver },
    { label: 'Meta Driver', value: config.meta_driver },
  ];

  return (
    <div className="space-y-3">
      {/* ── Top-level addresses and drivers ─────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>Server</CardTitle>
        </CardHeader>
        <div className="divide-y divide-slate-800">
          {topFields.map(({ label, value }) => (
            <div
              key={label}
              className="flex items-center gap-4 px-3 py-2 text-data"
            >
              <span className="section-label w-32 shrink-0">{label}</span>
              <span className="truncate text-slate-300" title={value || undefined}>
                {value || '—'}
              </span>
            </div>
          ))}
        </div>
      </Card>

      {/* ── Protocol list ─────────────────────────────────────────────────── */}
      {config.protocols && config.protocols.length > 0 && (
        <div className="space-y-2">
          <p className="section-label">Protocols ({config.protocols.length})</p>
          {config.protocols.map((p) => (
            <Card key={p.protocol}>
              <CardHeader>
                <CardTitle>{p.protocol}</CardTitle>
                <div className="flex items-center gap-3 text-data text-slate-400">
                  <span>
                    mutable TTL{' '}
                    <span className="tnum text-slate-200">
                      {p.mutable_ttl_seconds}s
                    </span>
                  </span>
                  {p.verify_tiers && p.verify_tiers.length > 0 && (
                    <>
                      <span className="text-slate-700">·</span>
                      <span>
                        tiers{' '}
                        <span className="text-slate-200">
                          {p.verify_tiers.join(', ')}
                        </span>
                      </span>
                    </>
                  )}
                </div>
              </CardHeader>

              {p.upstreams && p.upstreams.length > 0 ? (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-8 text-right">#</TableHead>
                      <TableHead className="w-36">Name</TableHead>
                      <TableHead>Base URL</TableHead>
                      <TableHead className="w-20 text-right">Origin</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {p.upstreams.map((u) => (
                      <TableRow key={u.name}>
                        <TableCell className="tnum text-right text-slate-500">
                          {u.priority}
                        </TableCell>
                        <TableCell className="font-medium text-slate-100">
                          {u.name}
                        </TableCell>
                        <TableCell
                          className="max-w-[1px] truncate text-slate-400"
                          title={u.base_url}
                        >
                          {u.base_url}
                        </TableCell>
                        <TableCell className="text-right">
                          {u.official ? (
                            <span className="text-tier-signed text-micro font-semibold uppercase tracking-wider">
                              origin
                            </span>
                          ) : (
                            <span className="text-slate-600">—</span>
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <CardContent className="text-data text-slate-400">
                  No upstreams configured for {p.protocol}.
                </CardContent>
              )}
            </Card>
          ))}
        </div>
      )}

      {(!config.protocols || config.protocols.length === 0) && (
        <Card>
          <CardContent className="text-data text-slate-400">
            No protocols configured.
          </CardContent>
        </Card>
      )}
    </div>
  );
}
