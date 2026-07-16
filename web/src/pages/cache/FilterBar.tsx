/**
 * FilterBar — the search/filter strip above each protocol's entry table.
 *
 * Controls:
 *   · Name text input (debounced 300 ms) — case-insensitive contains
 *   · Tier select (any | signed | consensus | tofu | checksum)
 *   · Upstream text input (debounced 300 ms) — contains
 *   · Sort select + order toggle (↑/↓)
 *   · Clear button — visible only when any filter is active
 *
 * The bar is a single hairline-bordered instrument strip; each control reads
 * as one field in a dense panel row, not as floating cards.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { X } from 'lucide-react';

import type { CacheQuery, CacheSort, Tier } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';

import { SORT_OPTIONS } from './types';

/**
 * Tier filter values. These are API literals, and the zh-CN copy deliberately
 * keeps them English (see `tier.*` in common.json) — they are what a Chinese
 * operator reads in the badge, the logs and the API response alike.
 */
const TIER_OPTIONS: Tier[] = ['signed', 'consensus', 'tofu', 'checksum'];

interface FilterBarProps {
  value: CacheQuery;
  onChange: (q: CacheQuery) => void;
}

export function FilterBar({ value, onChange }: FilterBarProps) {
  const { t } = useTranslation();
  // Local text state for the two debounced inputs.
  const [nameInput, setNameInput] = useState(value.name ?? '');
  const [upstreamInput, setUpstreamInput] = useState(value.upstream ?? '');

  // Always-current reference to the parent query — used inside timers so we
  // never spread a stale snapshot when the debounced call fires.
  const valueRef = useRef(value);
  valueRef.current = value;

  // Reset local inputs when the panel re-mounts (protocol switch resets query).
  useEffect(() => {
    setNameInput(value.name ?? '');
    setUpstreamInput(value.upstream ?? '');
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []); // Only on mount — protocol switch remounts the whole panel.

  // Debounce helpers — cancel previous timer, start a fresh one.
  const nameTimer = useRef<ReturnType<typeof setTimeout>>();
  const upTimer = useRef<ReturnType<typeof setTimeout>>();

  const handleName = useCallback(
    (s: string) => {
      setNameInput(s);
      clearTimeout(nameTimer.current);
      nameTimer.current = setTimeout(() => {
        onChange({ ...valueRef.current, name: s || undefined, offset: 0 });
      }, 300);
    },
    [onChange]
  );

  const handleUpstream = useCallback(
    (s: string) => {
      setUpstreamInput(s);
      clearTimeout(upTimer.current);
      upTimer.current = setTimeout(() => {
        onChange({ ...valueRef.current, upstream: s || undefined, offset: 0 });
      }, 300);
    },
    [onChange]
  );

  const handleTier = useCallback(
    (v: string) => {
      onChange({ ...valueRef.current, tier: v ? (v as Tier) : undefined, offset: 0 });
    },
    [onChange]
  );

  const handleSort = useCallback(
    (v: string) => {
      onChange({ ...valueRef.current, sort: v as CacheSort, offset: 0 });
    },
    [onChange]
  );

  const toggleOrder = useCallback(() => {
    onChange({
      ...valueRef.current,
      order: valueRef.current.order === 'asc' ? 'desc' : 'asc',
      offset: 0,
    });
  }, [onChange]);

  const clearFilters = useCallback(() => {
    setNameInput('');
    setUpstreamInput('');
    onChange({
      sort: valueRef.current.sort,
      order: valueRef.current.order,
      limit: valueRef.current.limit,
      offset: 0,
    });
  }, [onChange]);

  const hasFilter = !!(value.name || value.tier || value.upstream);
  const isDesc = (value.order ?? 'desc') === 'desc';

  return (
    <div className="flex flex-wrap items-center gap-2 border border-slate-800 bg-slate-900 px-3 py-2 rounded-[3px]">
      {/* Name search */}
      <Input
        className="h-7 w-52"
        placeholder={t('cache.filter.namePlaceholder')}
        value={nameInput}
        onChange={(e) => handleName(e.target.value)}
        aria-label={t('cache.filter.nameAria')}
      />

      {/* Tier filter */}
      <Select value={value.tier ?? ''} onValueChange={handleTier}>
        <SelectTrigger className="h-7 w-36" aria-label={t('cache.filter.tierAria')}>
          <SelectValue placeholder={t('cache.filter.anyTier')} />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="">{t('cache.filter.anyTier')}</SelectItem>
          {TIER_OPTIONS.map((v) => (
            <SelectItem key={v} value={v}>
              {t(`tier.${v}`)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      {/* Upstream filter */}
      <Input
        className="h-7 w-36"
        placeholder={t('cache.filter.upstreamPlaceholder')}
        value={upstreamInput}
        onChange={(e) => handleUpstream(e.target.value)}
        aria-label={t('cache.filter.upstreamAria')}
      />

      <div className="flex-1" />

      {/* Sort controls */}
      <div className="flex items-center gap-1.5">
        <span className="section-label mr-0.5">{t('cache.filter.sort')}</span>
        <Select value={value.sort ?? 'created_at'} onValueChange={handleSort}>
          <SelectTrigger className="h-7 w-32" aria-label={t('cache.filter.sortAria')}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SORT_OPTIONS.map((v) => (
              <SelectItem key={v} value={v}>
                {t(`cache.sort.${v}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Button
          size="icon"
          variant="ghost"
          className="h-7 w-7 text-slate-400 hover:text-slate-100"
          title={
            isDesc ? t('cache.filter.orderDescTitle') : t('cache.filter.orderAscTitle')
          }
          onClick={toggleOrder}
          aria-label={t('cache.filter.orderAria')}
        >
          <span aria-hidden className="text-[11px] font-semibold">
            {isDesc ? '↓' : '↑'}
          </span>
        </Button>
      </div>

      {hasFilter && (
        <Button
          size="sm"
          variant="ghost"
          className="h-7 gap-1 text-slate-400 hover:text-slate-200"
          onClick={clearFilters}
          aria-label={t('cache.filter.clearAria')}
        >
          <X className="size-3" />
          {t('cache.filter.clear')}
        </Button>
      )}
    </div>
  );
}
