import type { ButtonHTMLAttributes } from 'react';

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost';
type Size = 'sm' | 'md';

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
}

const base =
  'inline-flex items-center justify-center gap-1.5 rounded-md font-semibold tracking-wide ' +
  'transition-colors disabled:opacity-40 disabled:cursor-not-allowed focus:outline-none ' +
  'focus-visible:ring-2 focus-visible:ring-brand/50 focus-visible:ring-offset-0';

const variants: Record<Variant, string> = {
  primary: 'bg-brand text-brand-fg hover:bg-[#ffc158]',
  secondary:
    'bg-transparent text-slate-100 border border-slate-700 hover:border-slate-500 hover:bg-slate-800/50',
  danger: 'bg-red-500/90 text-white hover:bg-red-500',
  ghost: 'bg-transparent text-slate-400 hover:bg-slate-800/70 hover:text-slate-100',
};

const sizes: Record<Size, string> = {
  sm: 'h-7 px-2.5 text-xs',
  md: 'h-8 px-3 text-[13px]',
};

export function Button({ variant = 'primary', size = 'md', className = '', ...rest }: Props) {
  return (
    <button className={`${base} ${variants[variant]} ${sizes[size]} ${className}`} {...rest} />
  );
}

export default Button;
