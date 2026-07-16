import * as React from 'react';
import { Slot } from '@radix-ui/react-slot';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

/**
 * Button — shadcn/ui behaviour base (Radix Slot + CVA), fully re-skinned to the
 * instrument-panel direction:
 *
 *   · near-square (radius 3px), never a pill
 *   · amber is the ONLY filled variant — one primary action per view
 *   · `secondary`/`outline` are hairline boxes, not grey fills
 *   · states are designed, not defaults: hover shifts the surface, active
 *     nudges 1px down (a physical key-press read), focus is an amber ring
 *   · dense heights (h-7/h-8) — this is a control surface, not a landing page
 */
const buttonVariants = cva(
  cn(
    'inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded',
    'font-medium tracking-wide transition-colors duration-fast ease-instrument',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 focus-visible:ring-offset-slate-950',
    'disabled:pointer-events-none disabled:opacity-40',
    'active:translate-y-px',
    '[&_svg]:pointer-events-none [&_svg]:size-3.5 [&_svg]:shrink-0'
  ),
  {
    variants: {
      variant: {
        // The single amber action. Use at most one per view.
        default: 'bg-brand text-brand-fg hover:bg-[#ffc158]',
        // Hairline box — the default for most controls in a dense panel.
        secondary:
          'border border-slate-700 bg-transparent text-slate-100 hover:border-slate-600 hover:bg-slate-800',
        outline:
          'border border-slate-800 bg-transparent text-slate-300 hover:border-slate-700 hover:text-slate-100',
        // Destructive is outlined by default: eviction/delete should look
        // deliberate, not inviting.
        destructive:
          'border border-destructive/50 bg-transparent text-destructive hover:bg-destructive hover:text-destructive-foreground',
        ghost: 'bg-transparent text-slate-400 hover:bg-slate-800 hover:text-slate-100',
        link: 'text-brand underline-offset-4 hover:underline',
      },
      size: {
        sm: 'h-7 px-2.5 text-data',
        default: 'h-8 px-3 text-data',
        lg: 'h-9 px-4 text-body',
        icon: 'h-7 w-7',
      },
    },
    defaultVariants: {
      variant: 'secondary',
      size: 'default',
    },
  }
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  /** Render as the child element (e.g. a react-router <Link>) instead of <button>. */
  asChild?: boolean;
}

const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, asChild = false, ...props }, ref) => {
    const Comp = asChild ? Slot : 'button';
    return (
      <Comp className={cn(buttonVariants({ variant, size, className }))} ref={ref} {...props} />
    );
  }
);
Button.displayName = 'Button';

export { Button, buttonVariants };
