import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

import i18n from '@/i18n';

/**
 * cn merges Tailwind class names, letting later classes win over earlier ones
 * even when they are the same utility (`twMerge`), while still supporting
 * conditional/array/object class syntax (`clsx`).
 *
 * This is what makes a `className` prop on our primitives a real override
 * rather than a duplicate class the cascade resolves by source order.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/** formatBytes renders a byte count in binary units, e.g. 1536 → "1.5 KiB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes === 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / Math.pow(1024, i);
  return `${value.toFixed(i === 0 ? 0 : value >= 100 ? 0 : 1)} ${units[i]}`;
}

/**
 * formatUnix renders a Unix-seconds timestamp as a local date-time.
 *
 * Returns "—" for 0, which every timestamp in the API contract uses to mean
 * "never happened" — rendering it as 1970 would be a lie.
 *
 * The locale follows the UI language, not the browser's: a reader who switched
 * the interface to Chinese expects 2026/07/16, and leaving this on the browser
 * default would strand a Chinese UI showing 07/16/2026.
 */
export function formatUnix(unix: number): string {
  if (!unix) return '—';
  return new Date(unix * 1000).toLocaleString(i18n.resolvedLanguage ?? undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    // 24h in both locales: this is an ops tool, and an AM/PM flip in a log
    // column is a reading hazard, not a nicety.
    hour12: false,
  });
}

/**
 * formatRelative renders a Unix-seconds timestamp as a compact age.
 *
 * en "3m ago" · zh "3 分钟前". Deliberately NOT Intl.RelativeTimeFormat: its
 * narrow style still yields "3 min. ago", which is looser than the dense
 * instrument-panel column this sits in. The unit strings are locale keys so
 * both languages stay equally terse.
 *
 * Reads the i18n singleton rather than taking `t`, so call sites stay plain.
 * Callers re-render on `languageChanged` via their own useTranslation()
 * subscription, which re-invokes this with the new language.
 */
export function formatRelative(unix: number): string {
  if (!unix) return '—';
  const seconds = Math.floor(Date.now() / 1000) - unix;
  if (seconds < 0) return i18n.t('time.justNow');
  if (seconds < 60) return i18n.t('time.secondsAgo', { n: seconds });
  if (seconds < 3600) return i18n.t('time.minutesAgo', { n: Math.floor(seconds / 60) });
  if (seconds < 86400) return i18n.t('time.hoursAgo', { n: Math.floor(seconds / 3600) });
  return i18n.t('time.daysAgo', { n: Math.floor(seconds / 86400) });
}

/** formatPercent renders a 0..1 share as a percentage ("0.75" → "75%"). */
export function formatPercent(share: number, digits = 0): string {
  if (!Number.isFinite(share)) return '—';
  return `${(share * 100).toFixed(digits)}%`;
}
