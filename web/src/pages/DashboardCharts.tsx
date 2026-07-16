// Recharts is in this module so it gets its own async chunk (lazy import in Dashboard.tsx).
import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from 'recharts';
import type { SeriesPoint } from '../api/types';

function fmtBytes(n: number): string {
  if (n < 1024) return `${n}B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)}KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)}MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)}GB`;
}

interface Props {
  points: SeriesPoint[];
}

export default function DashboardCharts({ points }: Props) {
  const data = points.map((p) => ({
    date: new Date(p.unix * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
    bytes: p.bytes,
  }));

  return (
    <ResponsiveContainer width="100%" height={200}>
      <AreaChart data={data} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
        <defs>
          <linearGradient id="chartGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="5%" stopColor="#ffb02e" stopOpacity={0.3} />
            <stop offset="95%" stopColor="#ffb02e" stopOpacity={0} />
          </linearGradient>
        </defs>
        <CartesianGrid strokeDasharray="3 3" stroke="#1c1a16" vertical={false} />
        <XAxis
          dataKey="date"
          tick={{ fill: '#8c8477', fontSize: 11 }}
          axisLine={false}
          tickLine={false}
        />
        <YAxis
          tickFormatter={fmtBytes}
          tick={{ fill: '#8c8477', fontSize: 11 }}
          axisLine={false}
          tickLine={false}
          width={56}
        />
        <Tooltip
          contentStyle={{
            background: '#131210',
            border: '1px solid #1c1a16',
            borderRadius: 2,
            fontSize: 12,
            color: '#ece7dd',
          }}
          formatter={(value: number) => [fmtBytes(value), 'Bytes']}
          labelStyle={{ color: '#8c8477' }}
          cursor={{ stroke: '#332e27' }}
        />
        <Area
          type="monotone"
          dataKey="bytes"
          stroke="#ffb02e"
          strokeWidth={1.5}
          fill="url(#chartGrad)"
          dot={false}
          activeDot={{ r: 3, fill: '#ffb02e', stroke: '#0a0908' }}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}
