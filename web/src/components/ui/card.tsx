import * as React from 'react';

import { cn } from '@/lib/utils';

/**
 * Card ("panel") — the base surface.
 *
 * Re-skinned hard away from the shadcn default: NO shadow and NO large radius.
 * Depth comes from the surface step (950 app bg → 900 panel) plus a 1px
 * hairline border. That is the instrument-panel read; a drop shadow would make
 * it a generic SaaS card.
 */
const Card = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => (
    <div
      ref={ref}
      className={cn('rounded border border-slate-800 bg-slate-900 text-card-foreground', className)}
      {...props}
    />
  )
);
Card.displayName = 'Card';

/**
 * CardHeader — a dense header rail separated by a hairline, not by padding.
 * Deliberately tight (h-9): the rhythm is header-tight / body-roomy, not
 * uniform padding everywhere.
 */
const CardHeader = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => (
    <div
      ref={ref}
      className={cn(
        'flex h-9 shrink-0 items-center justify-between gap-3 border-b border-slate-800 px-3',
        className
      )}
      {...props}
    />
  )
);
CardHeader.displayName = 'CardHeader';

/** CardTitle — the small wide-tracked label that names a panel. */
const CardTitle = React.forwardRef<HTMLHeadingElement, React.HTMLAttributes<HTMLHeadingElement>>(
  ({ className, ...props }, ref) => (
    <h3
      ref={ref}
      className={cn(
        'label-caps text-label font-semibold text-slate-400',
        className
      )}
      {...props}
    />
  )
);
CardTitle.displayName = 'CardTitle';

const CardDescription = React.forwardRef<
  HTMLParagraphElement,
  React.HTMLAttributes<HTMLParagraphElement>
>(({ className, ...props }, ref) => (
  <p ref={ref} className={cn('text-data text-slate-400', className)} {...props} />
));
CardDescription.displayName = 'CardDescription';

const CardContent = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => <div ref={ref} className={cn('p-3', className)} {...props} />
);
CardContent.displayName = 'CardContent';

const CardFooter = React.forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  ({ className, ...props }, ref) => (
    <div
      ref={ref}
      className={cn('flex items-center gap-2 border-t border-slate-800 px-3 py-2', className)}
      {...props}
    />
  )
);
CardFooter.displayName = 'CardFooter';

/**
 * Readout — the big-number stat tile.
 *
 * This is where the scale contrast lives: an 11px wide-tracked label above a
 * 30px tabular readout. That jump IS the hierarchy, and it is why the type
 * scale has no intermediate sizes to drift into.
 */
export function Readout({
  label,
  value,
  hint,
  accent = false,
  className,
}: {
  label: string;
  /** Pre-formatted value. Pass "—" for genuinely unknown data — never a fake 0. */
  value: React.ReactNode;
  hint?: React.ReactNode;
  /** Amber the value. Use for the single most important figure in a group. */
  accent?: boolean;
  className?: string;
}) {
  return (
    <div className={cn('flex flex-col gap-1 p-3', className)}>
      <span className="section-label">{label}</span>
      <span
        className={cn('tnum text-readout font-semibold', accent ? 'text-brand' : 'text-slate-100')}
      >
        {value}
      </span>
      {hint ? <span className="text-data text-slate-400">{hint}</span> : null}
    </div>
  );
}

export { Card, CardHeader, CardFooter, CardTitle, CardDescription, CardContent };
