/**
 * EntryDetail — the drawer that opens when an operator clicks a cache table row.
 *
 * Shows:
 *   · Protocol-specific name / version header
 *   · Tier badge with the protocol-specific tier context sentence
 *   · Full digest (copyable) and size
 *   · First-cached and verified-at timestamps
 *   · Protocol-specific "fetch via proxy" snippet (when derivable from the DTO)
 *   · Admin actions: pin/unpin toggle · evict (inline confirm, no nested dialog)
 *
 * HONESTY: fields that are absent or always-zero on the current backend are not
 * rendered (no hit/pull count, no per-entry last-pulled).
 */

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Check, Copy, Pin, Trash2 } from 'lucide-react';

import { deleteCacheEntry, pinCacheEntry } from '@/api/client';
import type { CacheEntryDTO } from '@/api/types';
import { TierBadge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { useToast } from '@/hooks/use-toast';
import { cn, formatBytes, formatUnix } from '@/lib/utils';

import { errorText } from './types';
import type { ProtocolMeta, ProtocolSlug } from './types';
import { useRegistryHost } from '../../hooks/useRegistryHost';

interface EntryDetailProps {
  entry: CacheEntryDTO | null;
  protocol: ProtocolSlug;
  meta: ProtocolMeta;
  onClose: () => void;
  /** Called when an entry is successfully evicted so the table can remove it. */
  onEvict: (id: string) => void;
  /** Called when pin state changes so the table row updates immediately. */
  onPinChange: (id: string, pinned: boolean) => void;
}

export function EntryDetail({
  entry,
  protocol,
  meta,
  onClose,
  onEvict,
  onPinChange,
}: EntryDetailProps) {
  const { t } = useTranslation();
  const host = useRegistryHost();
  const { toast } = useToast();
  const [copied, setCopied] = useState(false);
  const [confirmEvict, setConfirmEvict] = useState(false);
  const [busy, setBusy] = useState<'evict' | 'pin' | null>(null);

  const copyDigest = async () => {
    if (!entry) return;
    try {
      await navigator.clipboard.writeText(entry.digest);
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    } catch {
      /* clipboard API not available */
    }
  };

  const handlePin = async () => {
    if (!entry || busy) return;
    setBusy('pin');
    const next = !entry.pinned;
    try {
      await pinCacheEntry(protocol, entry.id, next);
      onPinChange(entry.id, next);
      toast({
        variant: 'success',
        title: next ? t('entryDetail.toast.pinned') : t('entryDetail.toast.unpinned'),
        description: t('entryDetail.toast.pinDesc', {
          name: entry.name,
          version: entry.version,
          state: next
            ? t('entryDetail.protectedFromGC')
            : t('entryDetail.gcMayEvict'),
        }),
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('entryDetail.toast.pinFailed'),
        description: errorText(e),
        duration: Infinity,
      });
    } finally {
      setBusy(null);
    }
  };

  const handleEvict = async () => {
    if (!entry || busy) return;
    setBusy('evict');
    try {
      await deleteCacheEntry(protocol, entry.id);
      onEvict(entry.id);
      onClose();
      toast({
        variant: 'success',
        title: t('entryDetail.toast.evicted'),
        description: t('entryDetail.toast.evictedDesc', {
          name: entry.name,
          version: entry.version,
        }),
      });
    } catch (e: unknown) {
      setBusy(null);
      setConfirmEvict(false);
      toast({
        variant: 'destructive',
        title: t('entryDetail.toast.evictFailed'),
        description: errorText(e),
        duration: Infinity,
      });
    }
  };

  const usageHint = entry ? buildUsageHint(protocol, entry, host) : null;

  return (
    <Dialog
      open={!!entry}
      onOpenChange={(open) => {
        if (!open) {
          setConfirmEvict(false);
          onClose();
        }
      }}
    >
      <DialogContent className="max-w-lg">
        {entry && (
          <>
            <DialogHeader>
              <DialogTitle className="text-body font-semibold text-slate-100 pr-6">
                <span className="block truncate">{entry.name}</span>
                <span className="mt-0.5 block truncate text-data font-normal text-slate-400">
                  {t('entryDetail.versionLine', {
                    label: meta.versionCol.toLowerCase(),
                    version: entry.version,
                  })}
                </span>
              </DialogTitle>
              <DialogDescription className="label-caps text-micro text-slate-500">
                {protocol} · {entry.upstream || t('entryDetail.unknownUpstream')}
              </DialogDescription>
            </DialogHeader>

            {/* ── tier + size summary ──────────────────────────────────────── */}
            <div className="flex items-center gap-3 border-b border-slate-800 px-3 pb-3 pt-2">
              <TierBadge tier={entry.tier} />
              <span className="tnum text-data text-slate-300">{formatBytes(entry.size)}</span>
              {entry.pinned && (
                <span className="ml-auto flex items-center gap-1 text-micro text-brand">
                  <Pin className="size-2.5" />
                  {t('entryDetail.protectedFromGC')}
                </span>
              )}
            </div>

            {/* ── tier context (per-protocol explanation) ──────────────────── */}
            <div className="border-b border-slate-800 px-3 py-2">
              <p className="text-data text-slate-400">{meta.tierContext}</p>
            </div>

            {/* ── digest ──────────────────────────────────────────────────── */}
            <div className="border-b border-slate-800 px-3 py-2">
              <div className="section-label mb-1">{t('entryDetail.digest')}</div>
              <div className="flex items-center gap-2">
                <code className="min-w-0 flex-1 truncate rounded-[2px] bg-slate-950 px-2 py-1 text-data text-slate-300 font-mono">
                  {entry.digest}
                </code>
                <button
                  type="button"
                  onClick={() => void copyDigest()}
                  className={cn(
                    'shrink-0 rounded p-1 transition-colors duration-fast',
                    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
                    copied
                      ? 'text-tier-signed'
                      : 'text-slate-400 hover:text-brand'
                  )}
                  title={copied ? t('common.copied') : t('entryDetail.copyDigest')}
                  aria-label={
                    copied ? t('entryDetail.copiedAria') : t('entryDetail.copyDigestAria')
                  }
                >
                  {copied ? (
                    <Check className="size-3.5" />
                  ) : (
                    <Copy className="size-3.5" />
                  )}
                </button>
              </div>
            </div>

            {/* ── timestamps ──────────────────────────────────────────────── */}
            <div className="grid grid-cols-2 gap-4 border-b border-slate-800 px-3 py-2">
              <div>
                <div className="section-label mb-0.5">{t('entryDetail.firstCached')}</div>
                <span className="tnum text-data text-slate-200">
                  {formatUnix(entry.first_cached_unix)}
                </span>
              </div>
              <div>
                <div className="section-label mb-0.5">{t('entryDetail.verifiedAt')}</div>
                <span className="tnum text-data text-slate-200">
                  {formatUnix(entry.verified_unix)}
                </span>
              </div>
            </div>

            {/* ── protocol-specific fetch hint ─────────────────────────────── */}
            {usageHint && (
              <div className="border-b border-slate-800 px-3 py-2">
                <div className="section-label mb-1">{t('entryDetail.fetchViaProxy')}</div>
                <code className="block overflow-x-auto whitespace-pre rounded-[2px] bg-slate-950 px-2 py-1.5 text-data text-slate-300">
                  {usageHint}
                </code>
              </div>
            )}

            {/* ── actions ─────────────────────────────────────────────────── */}
            <DialogFooter className="flex-wrap gap-2">
              {/* Pin/unpin */}
              <Button
                variant="ghost"
                size="sm"
                disabled={busy !== null}
                onClick={() => void handlePin()}
                className="gap-1.5"
                aria-pressed={entry.pinned}
              >
                <Pin className="size-3.5" />
                {entry.pinned ? t('entryDetail.unpin') : t('entryDetail.pin')}
              </Button>

              {/* Evict — inline confirm to avoid a nested dialog */}
              {confirmEvict ? (
                <div className="flex items-center gap-1.5">
                  <span className="text-data text-destructive">
                    {t('entryDetail.evictConfirmPrompt')}
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy === 'evict'}
                    onClick={() => setConfirmEvict(false)}
                  >
                    {t('common.cancel')}
                  </Button>
                  <Button
                    variant="destructive"
                    size="sm"
                    disabled={busy === 'evict'}
                    onClick={() => void handleEvict()}
                  >
                    {busy === 'evict'
                      ? t('entryDetail.evicting')
                      : t('entryDetail.evictConfirm')}
                  </Button>
                </div>
              ) : (
                <Button
                  variant="destructive"
                  size="sm"
                  disabled={busy !== null}
                  onClick={() => setConfirmEvict(true)}
                  className="gap-1.5"
                >
                  <Trash2 className="size-3.5" />
                  {t('entryDetail.evict')}
                </Button>
              )}

              <div className="flex-1" />

              <DialogClose asChild>
                <Button variant="secondary" size="sm">
                  {t('common.close')}
                </Button>
              </DialogClose>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

/**
 * Build a protocol-specific "how to fetch this" snippet.
 *
 * Returns null when no useful command can be derived — for example apt entries
 * name a suite/component, not a package name; tarball names are already URLs.
 */
function buildUsageHint(
  protocol: ProtocolSlug,
  entry: CacheEntryDTO,
  host: string,
): string | null {
  // host is a parameter, not a useRegistryHost() call: this is a plain helper,
  // and calling a hook here is a rules-of-hooks violation (it runs outside a
  // component/hook, so React cannot track it). Caught by eslint, not by tsc.
  switch (protocol) {
    case 'oci':
      return `docker pull ${host}/${entry.name}:${entry.version}`;
    case 'pypi':
      // version might include extras like "2.31.0 (wheel)" — strip parens
      return `pip install "${entry.name}==${entry.version.replace(/\s.*$/, '')}"`;
    case 'npm':
      return `npm install "${entry.name}@${entry.version}"`;
    case 'go': {
      // version is an @v file like "@v/v0.6.0.mod" — extract the semver
      const ver = entry.version.replace(/^@v\//, '').replace(/\.(info|mod|zip)$/, '');
      return `GOPROXY=http://${host}/go go get ${entry.name}@${ver}`;
    }
    case 'apt':
      // suite/component is not a directly installable name
      return null;
    case 'helm':
      return `helm pull oci://${host}/helm/${entry.name} --version ${entry.version}`;
    case 'git':
      return `git clone http://${host}/git/${entry.name}`;
    case 'tarball':
      // name IS the original URL — show it but don't build a proxy URL
      return null;
    case 'cargo':
      return `CARGO_HOME=… cargo fetch  # sparse+http://${host}/cargo/index/`;
    case 'conda':
      return `conda install -c http://${host}/conda/conda-forge ${entry.name}`;
    case 'hf':
      return `HF_ENDPOINT=http://${host}/hf huggingface-cli download ${entry.name}`;
  }
}
