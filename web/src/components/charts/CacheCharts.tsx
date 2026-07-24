/**
 * CacheCharts — recharts components for the cache overview (Dashboard).
 *
 * Loaded lazily from Dashboard.tsx so recharts only enters the bundle when the
 * overview route is mounted. Do NOT import recharts outside this file.
 *
 * Chart design decisions (dataviz procedure applied):
 *
 * 1. FORM
 *    · Per-protocol bytes: horizontal bar chart (comparing magnitudes across a
 *      named categorical axis, ≤ 9 categories, value > identity).
 *    · Bytes over time: area chart (change-over-time, single series).
 *
 * 2. COLOR
 *    · Both charts use a SINGLE series, so no categorical palette is needed.
 *      A multi-hue categorical ramp would be wrong here — the bars represent
 *      the same metric (bytes cached) across different protocols, not different
 *      entities competing for attention. Single amber (#ffb02e) for all bars.
 *    · Area: amber stroke + gradient fill (the single sanctioned series treatment).
 *    · Grid, axes, labels: warm-neutral recessive tones from the slate ramp.
 *
 * 3. PALETTE VALIDATION
 *    Single-hue = no adjacent-pair CVD check required. Amber on the dark
 *    surface (#0a0908) was validated by the design system token authoring.
 *
 * 4. MARK SPECS
 *    · Bars: right-end radius 2px (anchored to baseline), maxBarSize 14px.
 *    · Area line: 1.5px stroke, no dot on every point (clutters at density).
 *    · Active dot: 3px radius, amber fill, app-bg ring.
 *    · Direct labels on bar right end (< 9 series → always label).
 *    · Grid: horizontal only on bar chart (aligns to the value axis);
 *      horizontal only on area (vertical gridlines cross the time axis noisily).
 *
 * 5. HOVER
 *    · Both charts have crosshair/tooltip via recharts built-in.
 *    · Tooltip surface matches the panel token (#131210 bg, #1c1a16 border).
 *
 * 6. ACCESSIBILITY
 *    · Single series → no legend box needed (title names it).
 *    · Table view of the same data is in the parent (the per-protocol table).
 *    · dark mode: page is dark-only by design; colors chosen against #0a0908.
 *
 * 7. i18n
 *    · Chart titles and tooltip series names are prose and ARE translated.
 *    · The category axis is protocol slugs — API identifiers, never translated.
 *    · Byte values keep going through `formatBytes` unchanged: the magnitude
 *      and the unit (KiB/MiB/GiB) are the same token in both languages, and
 *      localising the digits would make them harder to compare, not easier.
 *    · The time axis IS localised, but off the ACTIVE UI language rather than
 *      the browser's — an operator who switched the UI to Chinese should not
 *      keep getting English months because their browser is en-US.
 */

import { useTranslation } from 'react-i18next';
import {
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  LabelList,
  AreaChart,
  Area,
  LineChart,
  Line,
  Legend,
} from 'recharts';
import type { ProtocolStat, SeriesPoint, EventsSeriesPoint } from '@/api/types';
import { formatBytes } from '@/lib/utils';

// ── Design-system tokens (must stay in sync with tailwind.config.js) ─────────
const AMBER = '#ffb02e';
const RED = '#e35d5d';
const TOFU = '#c4a35a';
const SLATE_950 = '#0a0908'; // app bg (active dot ring)
const SLATE_800 = '#1c1a16'; // hairline border / grid
const SLATE_900 = '#131210'; // panel surface / tooltip bg
const SLATE_400 = '#8c8477'; // secondary text / axis ticks
const SLATE_100 = '#ece7dd'; // primary text

// Shared tooltip style — matches our panel surface exactly.
const TOOLTIP_STYLE: React.CSSProperties = {
  background: SLATE_900,
  border: `1px solid ${SLATE_800}`,
  borderRadius: 2,
  fontSize: 11,
  color: SLATE_100,
  padding: '5px 10px',
};

const TOOLTIP_LABEL_STYLE: React.CSSProperties = { color: SLATE_400 };
const TOOLTIP_CURSOR_BAR = { fill: 'rgba(255,176,46,0.07)' };
const TOOLTIP_CURSOR_LINE = { stroke: SLATE_800, strokeWidth: 1 };

// ── Per-protocol bytes — horizontal bar chart ─────────────────────────────────

interface ProtocolStat_ {
  protocol: string;
  bytes: number;
}

function ProtocolBytesChart({ stats }: { stats: ProtocolStat_[] }) {
  const { t } = useTranslation();
  const data = [...stats]
    .sort((a, b) => b.bytes - a.bytes)
    .map((s) => ({ protocol: s.protocol, bytes: s.bytes }));

  if (data.length === 0) return null;

  // Height: 14px bar + 14px gap per item, plus 24px vertical padding.
  const chartHeight = data.length * 28 + 24;

  return (
    <ResponsiveContainer width="100%" height={Math.max(80, chartHeight)}>
      <BarChart
        data={data}
        layout="vertical"
        margin={{ top: 0, right: 72, left: 0, bottom: 0 }}
      >
        <CartesianGrid horizontal={false} stroke={SLATE_800} strokeDasharray="0" />
        <XAxis
          type="number"
          tickFormatter={formatBytes}
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
          tickCount={4}
        />
        <YAxis
          type="category"
          dataKey="protocol"
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
          width={44}
        />
        <Tooltip
          contentStyle={TOOLTIP_STYLE}
          formatter={(value: number) => [formatBytes(value), t('charts.tooltipCached')]}
          labelStyle={TOOLTIP_LABEL_STYLE}
          cursor={TOOLTIP_CURSOR_BAR}
        />
        <Bar
          dataKey="bytes"
          fill={AMBER}
          radius={[0, 2, 2, 0]}
          maxBarSize={14}
          isAnimationActive={false}
        >
          <LabelList
            dataKey="bytes"
            position="right"
            formatter={(v: number) => formatBytes(v)}
            style={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          />
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  );
}

// ── Bytes over time — area chart ──────────────────────────────────────────────

function BytesTimeSeriesChart({ points }: { points: SeriesPoint[] }) {
  const { t, i18n } = useTranslation();
  // Follow the ACTIVE UI language, not the browser's — see header note 7.
  const data = points.map((p) => ({
    date: new Date(p.unix * 1000).toLocaleDateString(i18n.language, {
      month: 'short',
      day: 'numeric',
    }),
    bytes: p.bytes,
  }));

  return (
    <ResponsiveContainer width="100%" height={152}>
      <AreaChart data={data} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
        <defs>
          <linearGradient id="opsAreaGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="5%" stopColor={AMBER} stopOpacity={0.26} />
            <stop offset="95%" stopColor={AMBER} stopOpacity={0} />
          </linearGradient>
        </defs>
        <CartesianGrid
          strokeDasharray="3 3"
          stroke={SLATE_800}
          vertical={false}
        />
        <XAxis
          dataKey="date"
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
        />
        <YAxis
          tickFormatter={formatBytes}
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
          width={56}
        />
        <Tooltip
          contentStyle={TOOLTIP_STYLE}
          formatter={(value: number) => [formatBytes(value), t('charts.tooltipBytes')]}
          labelStyle={TOOLTIP_LABEL_STYLE}
          cursor={TOOLTIP_CURSOR_LINE}
        />
        <Area
          type="monotone"
          dataKey="bytes"
          stroke={AMBER}
          strokeWidth={1.5}
          fill="url(#opsAreaGrad)"
          dot={false}
          activeDot={{ r: 3, fill: AMBER, stroke: SLATE_950, strokeWidth: 2 }}
          isAnimationActive={false}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}

// ── Verification alert trend — stacked lines ─────────────────────────────────

function EventsTrendChart({ points }: { points: EventsSeriesPoint[] }) {
  const { t, i18n } = useTranslation();
  const data = points.map((p) => ({
    date: new Date(p.unix * 1000).toLocaleTimeString(i18n.language, {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
    }),
    fail: p.fail,
    warn: p.warn,
    maturity: p.maturity,
    tofu: p.tofu,
  }));
  const hasSignal = points.some((p) => p.fail + p.warn + p.maturity + p.tofu > 0);
  if (!hasSignal) return null;

  return (
    <ResponsiveContainer width="100%" height={152}>
      <LineChart data={data} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={SLATE_800} vertical={false} />
        <XAxis
          dataKey="date"
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
        />
        <YAxis
          allowDecimals={false}
          tick={{ fill: SLATE_400, fontSize: 11, fontFamily: 'inherit' }}
          axisLine={false}
          tickLine={false}
          width={28}
        />
        <Tooltip
          contentStyle={TOOLTIP_STYLE}
          labelStyle={TOOLTIP_LABEL_STYLE}
          cursor={TOOLTIP_CURSOR_LINE}
        />
        <Legend
          wrapperStyle={{ fontSize: 11, color: SLATE_400 }}
          formatter={(value: string) => {
            const labels: Record<string, string> = {
              fail: t('charts.tooltipFail'),
              warn: t('charts.tooltipWarn'),
              maturity: t('charts.tooltipMaturity'),
              tofu: t('charts.tooltipTofu'),
            };
            return labels[value] ?? value;
          }}
        />
        <Line
          type="monotone"
          dataKey="fail"
          name="fail"
          stroke={RED}
          strokeWidth={1.5}
          dot={false}
          isAnimationActive={false}
        />
        <Line
          type="monotone"
          dataKey="warn"
          name="warn"
          stroke={TOFU}
          strokeWidth={1.5}
          dot={false}
          isAnimationActive={false}
        />
        <Line
          type="monotone"
          dataKey="maturity"
          name="maturity"
          stroke={AMBER}
          strokeWidth={1}
          strokeDasharray="4 3"
          dot={false}
          isAnimationActive={false}
        />
        <Line
          type="monotone"
          dataKey="tofu"
          name="tofu"
          stroke={SLATE_400}
          strokeWidth={1}
          strokeDasharray="2 3"
          dot={false}
          isAnimationActive={false}
        />
      </LineChart>
    </ResponsiveContainer>
  );
}

// ── Default export: combined charts container (for React.lazy) ─────────────────

export interface CacheChartsProps {
  protocolStats: ProtocolStat[];
  seriesPoints: SeriesPoint[];
  eventsPoints?: EventsSeriesPoint[];
}

export default function CacheCharts({
  protocolStats,
  seriesPoints,
  eventsPoints = [],
}: CacheChartsProps) {
  const { t } = useTranslation();
  const hasBytes = protocolStats.some((s) => s.bytes > 0);
  const hasSeries = seriesPoints.length > 0;
  const hasEvents = eventsPoints.some((p) => p.fail + p.warn + p.maturity + p.tofu > 0);

  return (
    <div className="space-y-5">
      {hasBytes && (
        <div>
          <p className="section-label mb-2">{t('charts.bytesByProtocol')}</p>
          <ProtocolBytesChart stats={protocolStats} />
        </div>
      )}
      {hasSeries && (
        <div>
          <p className="section-label mb-2">{t('charts.cacheGrowth')}</p>
          <BytesTimeSeriesChart points={seriesPoints} />
        </div>
      )}
      {hasEvents && (
        <div>
          <p className="section-label mb-2">{t('charts.alertTrend')}</p>
          <EventsTrendChart points={eventsPoints} />
        </div>
      )}
    </div>
  );
}
