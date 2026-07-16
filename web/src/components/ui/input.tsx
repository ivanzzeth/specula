import * as React from 'react';

import { cn } from '@/lib/utils';

/**
 * Input — a hairline field on the panel surface.
 *
 * Re-skinned: near-square, no inner shadow, and the focus state moves the
 * BORDER to amber rather than only drawing an outer ring — in a dense form the
 * ring alone is easy to lose between neighbouring fields.
 */
const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  ({ className, type, ...props }, ref) => (
    <input
      type={type}
      ref={ref}
      className={cn(
        'flex h-7 w-full rounded border border-slate-800 bg-slate-950 px-2 py-1',
        'text-data text-slate-100 placeholder:text-slate-500',
        'transition-colors duration-fast',
        'hover:border-slate-700',
        'focus-visible:border-brand focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
        'disabled:cursor-not-allowed disabled:opacity-40',
        'file:border-0 file:bg-transparent file:text-data file:font-medium',
        className
      )}
      {...props}
    />
  )
);
Input.displayName = 'Input';

export { Input };
