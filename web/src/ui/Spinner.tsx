export function Spinner({ className = '' }: { className?: string }) {
  return (
    <span
      role="status"
      aria-label="Loading"
      className={`inline-block h-4 w-4 animate-spin rounded-full border-2 border-slate-700 border-t-brand ${className}`}
    />
  );
}

export default Spinner;
