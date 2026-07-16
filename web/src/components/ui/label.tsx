import * as React from 'react';
import * as LabelPrimitive from '@radix-ui/react-label';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

const labelVariants = cva(
  // `label-caps` carries the caps/tracking and is language-aware (index.css):
  // in Chinese the device turns itself off rather than no-op'ing on han glyphs.
  'label-caps text-label font-semibold text-slate-400 peer-disabled:opacity-40'
);

/** Label — the small wide-tracked field label, wired to Radix for a11y. */
const Label = React.forwardRef<
  React.ElementRef<typeof LabelPrimitive.Root>,
  React.ComponentPropsWithoutRef<typeof LabelPrimitive.Root> & VariantProps<typeof labelVariants>
>(({ className, ...props }, ref) => (
  <LabelPrimitive.Root ref={ref} className={cn(labelVariants(), className)} {...props} />
));
Label.displayName = LabelPrimitive.Root.displayName;

export { Label };
