/**
 * ProtocolPanel — the table + pagination state for a single protocol's cache.
 *
 * This is the main scanning surface: an operator opens /cache/pypi and sees
 * every cached PyPI package with its tier badge. The tier badge is the visual
 * anchor — it is the column that answers "was this actually verified?".
 *
 * Columns (protocol labels vary, values are uniform CacheEntryDTO fields):
 *   · Name     — protocol-specific: image, package, module, repo, URL, …
 *   · Version  — protocol-specific: tag, semver, @v file, ref/object, …
 *   · Tier     — the achieved verification tier (signed/consensus/tofu/checksum)
 *   · Size     — binary bytes
 *   · Upstream — the mirror that served this artifact
 *   · First cached — relative age + absolute on hover
 *
 * HONESTY: no hit/pull count column (the serve path has no per-entry counter).
 * `mutable` is always false on PG; `arch` for OCI is always empty — neither
 * is rendered here.
 */

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Pin } from 'lucide-react';

import { listCacheEntries } from '@/api/client';
import type { CacheEntryDTO, CacheQuery } from '@/api/types';
import { TierBadge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from '@/components/ui/card';
import { SkeletonRows } from '@/components/ui/skeleton';
import {
  EmptyRow,
  SortableTableHead,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { cn, formatBytes, formatRelative, formatUnix } from '@/lib/utils';

import { EntryDetail } from './EntryDetail';
import { FilterBar } from './FilterBar';
import { errorText, useProtocolMeta } from './types';
import type { ProtocolSlug } from './types';

const PAGE_SIZE = 50;

const DEFAULT_QUERY: CacheQuery = {
  sort: 'created_at',
  order: 'desc',
  limit: PAGE_SIZE,
  offset: 0,
};

interface ProtocolPanelProps {
  protocol: ProtocolSlug;
}

export function ProtocolPanel({ protocol }: ProtocolPanelProps) {
  const { t } = useTranslation();
  const meta = useProtocolMeta(protocol);

  const [query, setQuery] = useState<CacheQuery>(DEFAULT_QUERY);
  const [entries, setEntries] = useState<CacheEntryDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');
  const [selected, setSelected] = useState<CacheEntryDTO | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    setErr('');
    listCacheEntries(protocol, query)
      .then((r) => {
        setEntries(r.entries ?? []);
        setTotal(r.total ?? 0);
      })
      .catch((e: unknown) => setErr(errorText(e)))
      .finally(() => setLoading(false));
  }, [protocol, query]);

  useEffect(load, [load]);

  // Pagination
  const limit = query.limit ?? PAGE_SIZE;
  const offset = query.offset ?? 0;
  const canPrev = offset > 0;
  const canNext = offset + limit < total;

  const goPage = (newOffset: number) =>
    setQuery((q) => ({ ...q, offset: Math.max(0, newOffset) }));

  // Mutation callbacks passed to the detail drawer so the table reflects the
  // result immediately without a round-trip refetch.

  const onEvict = (id: string) => {
    setEntries((es) => es.filter((e) => e.id !== id));
    setTotal((t) => Math.max(0, t - 1));
    setSelected(null);
  };

  const onPinChange = (id: string, pinned: boolean) => {
    setEntries((es) => es.map((e) => (e.id === id ? { ...e, pinned } : e)));
    setSelected((s) => (s?.id === id ? { ...s, pinned } : s));
  };

  // Sort state helpers

  const activeSort = query.sort ?? 'created_at';
  const isDesc = (query.order ?? 'desc') === 'desc';

  const toggleSort = (col: CacheQuery['sort']) => {
    setQuery((q) => ({
      ...q,
      sort: col,
      order: q.sort === col ? (q.order === 'desc' ? 'asc' : 'desc') : 'desc',
      offset: 0,
    }));
  };

  // Pagination summary string
  const pageRange =
    total === 0
      ? t('cache.noEntries')
      : t('cache.range', {
          from: offset + 1,
          to: Math.min(offset + limit, total),
          total,
        });

  return (
    <div className="space-y-3 data-[state=active]:animate-panel-in">
      <FilterBar
        value={query}
        onChange={(q) => setQuery({ ...q, offset: 0 })}
      />

      <Card>
        <CardHeader>
          <CardTitle>{t('cache.panelTitle', { protocol: meta.label })}</CardTitle>
          <div className="tnum text-data text-slate-400">
            {loading ? (
              <span className="text-slate-500">{t('common.loading')}…</span>
            ) : err ? null : (
              <span>{pageRange}</span>
            )}
          </div>
        </CardHeader>

        {/* Loading */}
        {loading && (
          <CardContent>
            <SkeletonRows rows={10} />
          </CardContent>
        )}

        {/* Error */}
        {!loading && err && (
          <CardContent>
            <p className="text-data text-destructive">{err}</p>
            <Button variant="ghost" size="sm" className="mt-2" onClick={load}>
              {t('common.retry')}
            </Button>
          </CardContent>
        )}

        {/* Table */}
        {!loading && !err && (
          <Table>
            <TableHeader>
              <TableRow>
                <SortableTableHead
                  active={activeSort === 'name'}
                  desc={isDesc}
                  onSort={() => toggleSort('name')}
                  className="min-w-[160px]"
                >
                  {meta.nameCol}
                </SortableTableHead>

                <TableHead className="w-44">{meta.versionCol}</TableHead>

                {/* Tier is the widest intentionally — it is the reason this
                    table exists. Give it room and use the dedicated badge. */}
                <TableHead className="w-28">{t('cache.col.tier')}</TableHead>

                <SortableTableHead
                  active={activeSort === 'size'}
                  desc={isDesc}
                  onSort={() => toggleSort('size')}
                  align="right"
                  className="w-24"
                >
                  {t('cache.col.size')}
                </SortableTableHead>

                <TableHead className="w-36">{t('cache.col.upstream')}</TableHead>

                <SortableTableHead
                  active={activeSort === 'created_at'}
                  desc={isDesc}
                  onSort={() => toggleSort('created_at')}
                  align="right"
                  className="w-28"
                >
                  {t('cache.col.firstCached')}
                </SortableTableHead>
              </TableRow>
            </TableHeader>

            <TableBody>
              {entries.length === 0 ? (
                <EmptyRow colSpan={6}>{meta.emptyMsg}</EmptyRow>
              ) : (
                entries.map((entry) => (
                  <EntryRow
                    key={entry.id}
                    entry={entry}
                    isSelected={selected?.id === entry.id}
                    onClick={() => setSelected(entry)}
                  />
                ))
              )}
            </TableBody>
          </Table>
        )}

        {/* Pagination footer — only shown when there is more than one page */}
        {!loading && !err && total > limit && (
          <CardFooter className="justify-between">
            <Button
              variant="ghost"
              size="sm"
              disabled={!canPrev}
              onClick={() => goPage(offset - limit)}
              aria-label={t('cache.prevAria')}
            >
              ← {t('common.prev')}
            </Button>
            <span className="tnum text-data text-slate-400">{pageRange}</span>
            <Button
              variant="ghost"
              size="sm"
              disabled={!canNext}
              onClick={() => goPage(offset + limit)}
              aria-label={t('cache.nextAria')}
            >
              {t('common.next')} →
            </Button>
          </CardFooter>
        )}
      </Card>

      {/* Entry detail drawer — open when a row is clicked */}
      <EntryDetail
        entry={selected}
        protocol={protocol}
        meta={meta}
        onClose={() => setSelected(null)}
        onEvict={onEvict}
        onPinChange={onPinChange}
      />
    </div>
  );
}

/**
 * EntryRow — a single cache entry row.
 *
 * The tier badge is the visual anchor. The pin icon (amber) and the row-level
 * hover are the only state signals on the row itself — no row-level action
 * buttons (those live in the detail drawer).
 */
function EntryRow({
  entry,
  isSelected,
  onClick,
}: {
  entry: CacheEntryDTO;
  isSelected: boolean;
  onClick: () => void;
}) {
  const { t } = useTranslation();
  return (
    <TableRow
      className={cn(
        'cursor-pointer select-none',
        isSelected && 'bg-slate-800/60'
      )}
      onClick={onClick}
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onClick();
        }
      }}
      aria-selected={isSelected}
    >
      {/* Name — may be long (OCI image path, npm scope/pkg, Go module path) */}
      <TableCell className="max-w-[240px]">
        <div className="flex min-w-0 items-center gap-1.5">
          <span
            className="min-w-0 truncate font-medium text-slate-100"
            title={entry.name}
          >
            {entry.name}
          </span>
          {entry.pinned && (
            <span title={t('cache.row.pinnedTitle')} aria-label={t('cache.row.pinnedAria')}>
              <Pin className="size-2.5 shrink-0 text-brand" />
            </span>
          )}
        </div>
      </TableCell>

      {/* Version — tag, semver, @v file, ref, digest… */}
      <TableCell
        className="max-w-[176px] truncate text-slate-300"
        title={entry.version}
      >
        {entry.version}
      </TableCell>

      {/* Tier — the ANCHOR COLUMN. Wide, always visible. */}
      <TableCell>
        <TierBadge tier={entry.tier} />
      </TableCell>

      {/* Size */}
      <TableCell className="tnum text-right text-slate-300">
        {formatBytes(entry.size)}
      </TableCell>

      {/* Upstream */}
      <TableCell
        className="max-w-[144px] truncate text-slate-400"
        title={entry.upstream || undefined}
      >
        {entry.upstream || '—'}
      </TableCell>

      {/* First cached — relative ("3d ago"), full date on hover */}
      <TableCell
        className="tnum text-right text-slate-400"
        title={formatUnix(entry.first_cached_unix)}
      >
        {formatRelative(entry.first_cached_unix)}
      </TableCell>
    </TableRow>
  );
}
