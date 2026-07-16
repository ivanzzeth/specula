import * as React from 'react';

import { cn } from '@/lib/utils';

/**
 * Table — the workhorse of this UI (cache browser, repos, tags, mirrors).
 *
 * Re-skinned for density and scanning:
 *   · 28px rows, 12.5px data type — an operator sees ~25 rows without scrolling
 *   · hairline row separators, no zebra striping (stripes fight status colour)
 *   · sticky header so the column meaning survives a long scroll
 *   · hover raises the row surface only — a scan aid, not decoration
 *
 * The wrapper scrolls horizontally on its own so a wide table can never make
 * the page body scroll sideways.
 */
const Table = React.forwardRef<HTMLTableElement, React.HTMLAttributes<HTMLTableElement>>(
  ({ className, ...props }, ref) => (
    <div className="relative w-full overflow-x-auto">
      <table
        ref={ref}
        className={cn('w-full caption-bottom border-collapse text-data', className)}
        {...props}
      />
    </div>
  )
);
Table.displayName = 'Table';

const TableHeader = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => (
  <thead
    ref={ref}
    className={cn('sticky top-0 z-10 bg-slate-900 [&_tr]:border-b [&_tr]:border-slate-800', className)}
    {...props}
  />
));
TableHeader.displayName = 'TableHeader';

const TableBody = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => <tbody ref={ref} className={cn(className)} {...props} />);
TableBody.displayName = 'TableBody';

const TableFooter = React.forwardRef<
  HTMLTableSectionElement,
  React.HTMLAttributes<HTMLTableSectionElement>
>(({ className, ...props }, ref) => (
  <tfoot
    ref={ref}
    className={cn('border-t border-slate-800 text-slate-400', className)}
    {...props}
  />
));
TableFooter.displayName = 'TableFooter';

const TableRow = React.forwardRef<HTMLTableRowElement, React.HTMLAttributes<HTMLTableRowElement>>(
  ({ className, ...props }, ref) => (
    <tr
      ref={ref}
      className={cn(
        'border-b border-slate-800/70 transition-colors duration-fast',
        'hover:bg-slate-800/40 data-[state=selected]:bg-slate-800',
        className
      )}
      {...props}
    />
  )
);
TableRow.displayName = 'TableRow';

/** TableHead — a column header. Pair with `.tnum` + text-right for numbers. */
const TableHead = React.forwardRef<
  HTMLTableCellElement,
  React.ThHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <th
    ref={ref}
    className={cn(
      'h-7 px-2.5 text-left align-middle',
      'text-label font-semibold uppercase tracking-wider text-slate-400',
      '[&:has([role=checkbox])]:pr-0',
      className
    )}
    {...props}
  />
));
TableHead.displayName = 'TableHead';

const TableCell = React.forwardRef<
  HTMLTableCellElement,
  React.TdHTMLAttributes<HTMLTableCellElement>
>(({ className, ...props }, ref) => (
  <td
    ref={ref}
    className={cn('h-7 px-2.5 align-middle text-slate-100', className)}
    {...props}
  />
));
TableCell.displayName = 'TableCell';

const TableCaption = React.forwardRef<
  HTMLTableCaptionElement,
  React.HTMLAttributes<HTMLTableCaptionElement>
>(({ className, ...props }, ref) => (
  <caption ref={ref} className={cn('mt-2 text-data text-slate-400', className)} {...props} />
));
TableCaption.displayName = 'TableCaption';

/**
 * SortableTableHead — a column header that toggles sort.
 *
 * The active column is amber (text-colour-as-state, matching the nav) with a
 * direction caret; inactive columns stay neutral and only reveal their
 * affordance on hover. No pills, no chrome.
 */
export function SortableTableHead({
  children,
  active,
  desc,
  onSort,
  className,
  align = 'left',
}: {
  children: React.ReactNode;
  active: boolean;
  desc: boolean;
  onSort: () => void;
  className?: string;
  align?: 'left' | 'right';
}) {
  return (
    <TableHead className={cn(align === 'right' && 'text-right', className)}>
      <button
        type="button"
        onClick={onSort}
        aria-sort={active ? (desc ? 'descending' : 'ascending') : 'none'}
        className={cn(
          'inline-flex items-center gap-1 uppercase tracking-wider transition-colors duration-fast',
          'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
          active ? 'text-brand' : 'text-slate-400 hover:text-slate-200'
        )}
      >
        {children}
        <span aria-hidden className={cn('text-micro', !active && 'opacity-0')}>
          {desc ? '▼' : '▲'}
        </span>
      </button>
    </TableHead>
  );
}

/** EmptyRow renders a centered "nothing here" state spanning the whole table. */
export function EmptyRow({ colSpan, children }: { colSpan: number; children: React.ReactNode }) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={colSpan} className="h-20 text-center text-slate-400">
        {children}
      </TableCell>
    </TableRow>
  );
}

export { Table, TableHeader, TableBody, TableFooter, TableHead, TableRow, TableCell, TableCaption };
