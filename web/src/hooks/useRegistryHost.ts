import { useEffect, useState } from 'react';

import { getInstance } from '../api/client';

/** Module-level cache: the registry host is deployment-static, so one fetch serves every page. */
let cached: string | null = null;
let inflight: Promise<string> | null = null;

/**
 * Returns the host to print into `docker login` / `docker pull` commands.
 *
 * Do NOT use window.location.host for this: that is the control plane (which
 * serves this UI), while the registry answers on the data plane — a different
 * port locally, and typically a different hostname behind an Ingress. The
 * server resolves the correct value (config `server.registry_public_host`, else
 * derived) and exposes it at GET /api/v1/instance.
 *
 * Falls back to '<registry-host>' — a visibly-a-placeholder string, so a failed
 * fetch yields a command the user must fix rather than one that silently points
 * at the wrong port.
 */
export function useRegistryHost(): string {
  const [host, setHost] = useState<string>(cached ?? '<registry-host>');

  useEffect(() => {
    if (cached) return;
    inflight ??= getInstance()
      .then((r) => (cached = r.registry_host))
      .catch(() => '<registry-host>');
    let alive = true;
    void inflight.then((h) => alive && setHost(h));
    return () => {
      alive = false;
    };
  }, []);

  return host;
}
