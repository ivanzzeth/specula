/**
 * Per-protocol configuration for the cache browser.
 *
 * The API returns uniform CacheEntryDTO fields across all protocols; what
 * differs per protocol is the semantic label for those fields and the
 * human-readable context for trust tiers, empty states, and usage hints.
 *
 * i18n: the human-readable half of that per-protocol copy (column labels,
 * empty state, tier context) lives in `locales/*\/cache.json` under
 * `cache.protocol.<slug>.*` and is read through `useProtocolMeta`. What stays
 * HERE is only what must not be translated: the protocol list itself and the
 * display labels for it, which are API/ecosystem identifiers (`oci`, `pypi`,
 * `npm`, …) in every language.
 */

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';

import { ApiError } from '@/api/client';
import type { CacheSort } from '@/api/types';
import { translateServerError } from '@/i18n/server-errors';

export interface ProtocolMeta {
  /** Tab / heading label. */
  label: string;
  /** Column header for the `name` field. */
  nameCol: string;
  /** Column header for the `version` field. */
  versionCol: string;
  /** Empty-state message when nothing is cached for this protocol. */
  emptyMsg: string;
  /**
   * One-line explanation of what the tier field means for this protocol.
   * Shown below the tier badge in the detail view.
   */
  tierContext: string;
}

export const PROTOCOLS = [
  'oci',
  'pypi',
  'npm',
  'go',
  'apt',
  'helm',
  'git',
  'tarball',
] as const;

export type ProtocolSlug = (typeof PROTOCOLS)[number];

export function isValidProtocol(s: string): s is ProtocolSlug {
  return (PROTOCOLS as readonly string[]).includes(s);
}

/**
 * Display labels for the protocol tabs. NOT translated: these are the names of
 * the ecosystems themselves — a Chinese developer says "npm", not a rendering
 * of it — and they double as the route segment (/cache/pypi).
 */
export const PROTOCOL_LABELS: Record<ProtocolSlug, string> = {
  oci: 'OCI',
  pypi: 'PyPI',
  npm: 'npm',
  go: 'Go',
  apt: 'apt',
  helm: 'Helm',
  git: 'git',
  tarball: 'tarball',
};

/** Sort columns; the display label comes from `cache.sort.<value>`. */
export const SORT_OPTIONS: CacheSort[] = ['created_at', 'size', 'name', 'verified_at'];

/** Resolve the translated, protocol-specific labels for a protocol. */
export function useProtocolMeta(protocol: ProtocolSlug): ProtocolMeta {
  const { t } = useTranslation();
  return useMemo(
    () => ({
      label: PROTOCOL_LABELS[protocol],
      nameCol: t(`cache.protocol.${protocol}.nameCol`),
      versionCol: t(`cache.protocol.${protocol}.versionCol`),
      emptyMsg: t(`cache.protocol.${protocol}.emptyMsg`),
      tierContext: t(`cache.protocol.${protocol}.tierContext`),
    }),
    [protocol, t]
  );
}

/**
 * Render an unknown thrown value as display text.
 *
 * API failures carry an English `detail` from the Go server; hand those to the
 * server-error allow-list, which localises the ones we know and passes the rest
 * through as English by design. Anything else is a client-side fault and its
 * message is already whatever the browser/runtime produced.
 */
export function errorText(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail);
  if (e instanceof Error) return e.message;
  return String(e);
}
