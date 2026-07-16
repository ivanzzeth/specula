import type { ReactNode } from 'react';

type Tone = 'neutral' | 'green' | 'amber' | 'red' | 'blue';

const dot: Record<Tone, string> = {
  neutral: 'bg-slate-500',
  green: 'bg-emerald-400',
  amber: 'bg-amber-400',
  red: 'bg-red-400',
  blue: 'bg-sky-400',
};

export function Badge({ tone = 'neutral', children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5 rounded-md border border-slate-700 px-1.5 py-0.5 text-xs font-medium text-slate-300">
      <span className={`h-[7px] w-[7px] shrink-0 ${dot[tone]}`} />
      {children}
    </span>
  );
}

export function resultTone(result: string): Tone {
  switch (result) {
    case 'pass':
      return 'green';
    case 'fail':
      return 'red';
    case 'warn':
      return 'amber';
    default:
      return 'neutral';
  }
}

export default Badge;
