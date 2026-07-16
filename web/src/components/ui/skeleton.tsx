import { cn } from '@/lib/utils';

/**
 * Skeleton — a loading placeholder.
 *
 * Deliberately a flat pulsing block, not a shimmer sweep: a shimmering gradient
 * is exactly the kind of decorative motion this direction rejects. It also
 * sizes to the real content it replaces so the layout does not shift on load.
 */
function Skeleton({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('animate-pulse rounded-[2px] bg-slate-800', className)} {...props} />;
}

/** SkeletonRows renders n table-row-height skeleton bars — the list loading state. */
function SkeletonRows({ rows = 6, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn('flex flex-col gap-px', className)}>
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-7 w-full" />
      ))}
    </div>
  );
}

export { Skeleton, SkeletonRows };
