/**
 * CacheBrowser — the 8-protocol cache scanning surface (REGISTRY-DESIGN §5.2).
 *
 * Core premise: operators should be able to SEE what the proxy has cached per
 * protocol — not just a total byte count. The tier badge on every row answers
 * "was this actually verified?" at a glance.
 *
 * Each protocol tab is a route segment (/cache/pypi, /cache/go, …) so the
 * view is linkable — an operator can paste /cache/npm into a ticket.
 *
 * Only the active protocol's ProtocolPanel mounts at any time: switching tabs
 * navigates to /cache/{protocol}, remounting a fresh panel with default
 * filters. This keeps the initial load to one API call, not eight.
 *
 * Owned by: the Cache UI agent.
 * Files: web/src/pages/CacheBrowser.tsx, web/src/pages/cache/**
 */

import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';

import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';

import { ProtocolPanel } from './cache/ProtocolPanel';
import { isValidProtocol, PROTOCOL_LABELS, PROTOCOLS } from './cache/types';
import type { ProtocolSlug } from './cache/types';

export function CacheBrowser() {
  const { protocol: param } = useParams<{ protocol: string }>();
  const navigate = useNavigate();
  const { t } = useTranslation();

  // Validate and default the protocol — fall back to 'oci' for unknown values
  // or when at the bare /cache route (no :protocol param in the path).
  const active: ProtocolSlug =
    param && isValidProtocol(param) ? param : 'oci';

  const onTabChange = (p: string) => {
    navigate(`/cache/${p}`, { replace: false });
  };

  return (
    <div className="space-y-3">
      <PageHeading />

      <Tabs value={active} onValueChange={onTabChange}>
        <TabsList>
          {PROTOCOLS.map((p) => (
            <TabsTrigger
              key={p}
              value={p}
              aria-label={t('cache.tabAria', { protocol: PROTOCOL_LABELS[p] })}
            >
              {PROTOCOL_LABELS[p]}
            </TabsTrigger>
          ))}
        </TabsList>

        {/*
          Only a single TabsContent is rendered — the active protocol's panel.
          Radix Tabs shows any TabsContent whose `value` matches the Root's
          `value`; since we only mount one (the current protocol), exactly that
          one is shown without the panel-in animation being suppressed.

          `key={active}` forces a fresh mount when switching protocols, which:
            · Resets filter state to defaults (correct — filters are per-protocol)
            · Cancels in-flight requests from the previous protocol
            · Triggers the panel-in animation (hierarchy reveal)
        */}
        <TabsContent value={active}>
          <ProtocolPanel key={active} protocol={active} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function PageHeading() {
  const { t } = useTranslation();
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">{t('cache.title')}</h1>
      <p className="mt-0.5 text-data text-slate-400">{t('cache.subtitle')}</p>
    </div>
  );
}
