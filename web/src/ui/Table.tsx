import type { ReactNode } from 'react';

export interface Column<T> {
  key: string;
  header: ReactNode;
  render: (row: T) => ReactNode;
  className?: string;
}

interface Props<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  empty?: ReactNode;
  onRowClick?: (row: T) => void;
}

export function Table<T>({ columns, rows, rowKey, empty, onRowClick }: Props<T>) {
  return (
    <div className="overflow-x-auto rounded-lg border border-slate-800">
      <table className="w-full text-[13px]">
        <thead>
          <tr className="border-b border-slate-800">
            {columns.map((c) => (
              <th
                key={c.key}
                className={`px-3 py-2 text-left text-[11px] font-medium uppercase tracking-wider text-slate-500 ${c.className ?? ''}`}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="px-3 py-10 text-center text-slate-500">
                {empty ?? 'No data'}
              </td>
            </tr>
          ) : (
            rows.map((row) => (
              <tr
                key={rowKey(row)}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                className={
                  onRowClick ? 'cursor-pointer transition-colors hover:bg-slate-800/40' : ''
                }
              >
                {columns.map((c) => (
                  <td key={c.key} className={`px-3 py-2.5 text-slate-300 ${c.className ?? ''}`}>
                    {c.render(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

export default Table;
