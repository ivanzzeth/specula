import type { InputHTMLAttributes, ReactNode } from 'react';

interface FieldProps {
  label?: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
}

export function Field({ label, hint, children }: FieldProps) {
  return (
    <label className="block space-y-1.5">
      {label != null && (
        <span className="block text-[11px] font-semibold uppercase tracking-wide text-slate-400">
          {label}
        </span>
      )}
      {children}
      {hint != null && <span className="block text-xs text-slate-500">{hint}</span>}
    </label>
  );
}

export const inputClass =
  'w-full h-8 rounded-md border border-slate-700 bg-slate-900 px-2.5 text-[13px] text-slate-100 ' +
  'placeholder:text-slate-500 focus:border-brand/70 focus:outline-none focus:ring-2 focus:ring-brand/30';

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  const { className = '', ...rest } = props;
  return <input className={`${inputClass} ${className}`} {...rest} />;
}

export default Field;
