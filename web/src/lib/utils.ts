import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

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
 */
export function formatUnix(unix: number): string {
  if (!unix) return '—';
  return new Date(unix * 1000).toLocaleString(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
}

/** formatRelative renders a Unix-seconds timestamp as a compact age ("3m ago"). */
export function formatRelative(unix: number): string {
  if (!unix) return '—';
  const seconds = Math.floor(Date.now() / 1000) - unix;
  if (seconds < 0) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

/** formatPercent renders a 0..1 share as a percentage ("0.75" → "75%"). */
export function formatPercent(share: number, digits = 0): string {
  if (!Number.isFinite(share)) return '—';
  return `${(share * 100).toFixed(digits)}%`;
}
