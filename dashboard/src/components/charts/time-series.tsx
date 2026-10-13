'use client';

import * as React from 'react';

import { cn } from '@/lib/utils';

export interface ChartSeries {
  name: string;
  /** Any CSS color, including a token like `hsl(var(--primary))`. */
  color: string;
  values: number[];
}

export interface LineChartProps {
  series: ChartSeries[];
  /** One label per x position; the same length as every series. */
  labels: string[];
  height?: number;
  formatValue?: (value: number) => string;
  className?: string;
  /** Render a filled area under a single-series chart. */
  area?: boolean;
  /**
   * Fill the area with a vertical gradient from the series color instead of a
   * flat wash. This is the hero-chart treatment: color that reads as depth
   * under the line rather than a tint sitting on the panel.
   */
  areaGradient?: boolean;
  /** Render straight segments instead of smooth curves. Defaults to false.
   *
   * Bucketed counts are discrete: nothing is known between two buckets, and a
   * smoothing spline would invent it — rising before a spike arrives and even
   * dipping below zero after a run of zeros. Straight segments never draw
   * traffic where the data says there is none. */
  curved?: boolean;
}

const VIEW_WIDTH = 760;
const PADDING_TOP = 20;
const PADDING_RIGHT = 20;
const PADDING_BOTTOM = 30;
const PADDING_LEFT = 52;

function getSplinePath(pts: Array<{ x: number; y: number }>): string {
  if (pts.length === 0) return '';
  const first = pts[0];
  if (!first) return '';
  if (pts.length === 1) return `M ${first.x.toFixed(1)},${first.y.toFixed(1)}`;
  if (pts.length === 2) {
    const second = pts[1];
    if (!second) return `M ${first.x.toFixed(1)},${first.y.toFixed(1)}`;
    return `M ${first.x.toFixed(1)},${first.y.toFixed(1)} L ${second.x.toFixed(1)},${second.y.toFixed(1)}`;
  }

  let d = `M ${first.x.toFixed(1)},${first.y.toFixed(1)}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[Math.max(i - 1, 0)] ?? first;
    const p1 = pts[i] ?? first;
    const p2 = pts[i + 1] ?? first;
    const p3 = pts[Math.min(i + 2, pts.length - 1)] ?? p2;

    // Catmull-Rom to cubic Bezier conversion
    const cp1x = p1.x + (p2.x - p0.x) / 6;
    const cp1y = p1.y + (p2.y - p0.y) / 6;
    const cp2x = p2.x - (p3.x - p1.x) / 6;
    const cp2y = p2.y - (p3.y - p1.y) / 6;

    d += ` C ${cp1x.toFixed(1)},${cp1y.toFixed(1)} ${cp2x.toFixed(1)},${cp2y.toFixed(1)} ${p2.x.toFixed(1)},${p2.y.toFixed(1)}`;
  }
  return d;
}

/**
 * A line chart built from SVG primitives.
 *
 * The project deliberately carries no charting library. Every chart here is a
 * handful of paths over a numeric array, and a charting dependency would cost
 * more bundle and more upgrade risk than the lines it replaces. It also means
 * the chart renders identically on the server and the client, which a
 * canvas-based library cannot do.
 */
export function LineChart({
  series,
  labels,
  height = 240,
  formatValue = (value) => String(value),
  className,
  area = false,
  areaGradient = false,
  curved = false,
}: LineChartProps) {
  const [hoveredIdx, setHoveredIdx] = React.useState<number | null>(null);
  const svgRef = React.useRef<SVGSVGElement>(null);

  const viewHeight = height;
  const innerWidth = VIEW_WIDTH - PADDING_LEFT - PADDING_RIGHT;
  const innerHeight = viewHeight - PADDING_TOP - PADDING_BOTTOM;

  const pointCount = labels.length;

  const max = React.useMemo(() => {
    let highest = 0;
    for (const item of series) {
      for (const value of item.values) {
        if (Number.isFinite(value) && value > highest) highest = value;
      }
    }
    return highest > 0 ? highest : 1;
  }, [series]);

  if (pointCount === 0 || series.length === 0) {
    return (
      <div
        className={cn('flex items-center justify-center rounded-xl border border-dashed border-border text-xs text-muted-foreground', className)}
        style={{ height }}
      >
        No data in this window
      </div>
    );
  }

  const xAt = (index: number): number => {
    if (pointCount === 1) return PADDING_LEFT + innerWidth / 2;
    return PADDING_LEFT + (index * innerWidth) / (pointCount - 1);
  };
  const yAt = (value: number): number => {
    const clamped = Math.max(0, Math.min(value, max));
    return PADDING_TOP + innerHeight * (1 - clamped / max);
  };

  const ticks = [0, 0.25, 0.5, 0.75, 1];
  const labelStride = Math.max(1, Math.ceil(pointCount / 5));

  const gradientId = (name: string) => `area-${name.replace(/[^a-zA-Z0-9]/g, '')}`;

  const handleMouseMove = (event: React.MouseEvent<SVGSVGElement>) => {
    if (!svgRef.current || pointCount < 2) return;
    const rect = svgRef.current.getBoundingClientRect();
    const clientX = event.clientX - rect.left;
    const svgX = (clientX / rect.width) * VIEW_WIDTH;
    const relX = Math.max(0, Math.min(innerWidth, svgX - PADDING_LEFT));
    const idx = Math.round((relX / innerWidth) * (pointCount - 1));
    setHoveredIdx(idx);
  };

  const handleMouseLeave = () => setHoveredIdx(null);

  return (
    <div className={cn('relative w-full select-none', className)}>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${VIEW_WIDTH} ${viewHeight}`}
        className="h-auto w-full overflow-visible"
        role="img"
        aria-label={`Line chart: ${series.map((item) => item.name).join(', ')}`}
        onMouseMove={handleMouseMove}
        onMouseLeave={handleMouseLeave}
      >
        <defs>
          {series.map((item) => (
            <linearGradient key={item.name} id={gradientId(item.name)} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={item.color} stopOpacity={0.32} />
              <stop offset="60%" stopColor={item.color} stopOpacity={0.08} />
              <stop offset="100%" stopColor={item.color} stopOpacity={0.0} />
            </linearGradient>
          ))}
        </defs>

        {/* Horizontal grid ticks */}
        {ticks.map((tick) => {
          const y = PADDING_TOP + innerHeight * (1 - tick);
          return (
            <g key={tick}>
              <line
                x1={PADDING_LEFT}
                x2={VIEW_WIDTH - PADDING_RIGHT}
                y1={y}
                y2={y}
                stroke="hsl(var(--border))"
                strokeDasharray={tick === 0 ? undefined : '3 4'}
              />
              <text
                x={PADDING_LEFT - 10}
                y={y + 3.5}
                textAnchor="end"
                className="fill-muted-foreground text-[10px] font-mono"
              >
                {formatValue(max * tick)}
              </text>
            </g>
          );
        })}

        {/* Series paths and area */}
        {series.map((item) => {
          const pts = item.values.map((val, idx) => ({ x: xAt(idx), y: yAt(val) }));
          const pathD = curved ? getSplinePath(pts) : `M ${pts.map((p) => `${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' L ')}`;
          const areaD = area
            ? `${pathD} L ${xAt(pointCount - 1)},${PADDING_TOP + innerHeight} L ${xAt(0)},${PADDING_TOP + innerHeight} Z`
            : undefined;

          return (
            <g key={item.name}>
              {areaD ? (
                <path
                  d={areaD}
                  fill={areaGradient ? `url(#${gradientId(item.name)})` : item.color}
                  opacity={areaGradient ? undefined : 0.12}
                />
              ) : null}

              {/* Main crisp line. One ink, no halo: a wide translucent stroke
                  underneath would read as decoration, not depth. */}
              <path
                d={pathD}
                fill="none"
                stroke={item.color}
                strokeWidth={2.25}
                strokeLinecap="round"
                strokeLinejoin="round"
              />

              {pointCount === 1 ? (
                <circle cx={xAt(0)} cy={yAt(item.values[0] ?? 0)} r={3.5} fill={item.color} />
              ) : null}
            </g>
          );
        })}

        {/* Hover vertical crosshair & indicator point */}
        {hoveredIdx != null ? (
          <g>
            <line
              x1={xAt(hoveredIdx)}
              x2={xAt(hoveredIdx)}
              y1={PADDING_TOP}
              y2={PADDING_TOP + innerHeight}
              stroke="hsl(var(--muted-foreground))"
              strokeOpacity={0.4}
              strokeDasharray="3 3"
              strokeWidth={1}
            />
            {series.map((item) => {
              const val = item.values[hoveredIdx] ?? 0;
              const y = yAt(val);
              return (
                <g key={`pt-${item.name}`}>
                  <circle cx={xAt(hoveredIdx)} cy={y} r={6} fill={item.color} opacity={0.25} />
                  <circle cx={xAt(hoveredIdx)} cy={y} r={3.5} fill={item.color} stroke="hsl(var(--card))" strokeWidth={1.5} />
                </g>
              );
            })}
          </g>
        ) : null}

        {/* Bottom X-axis labels */}
        {labels.map((label, index) =>
          index % labelStride === 0 || index === pointCount - 1 ? (
            <text
              key={`${label}-${index}`}
              x={xAt(index)}
              y={viewHeight - 6}
              textAnchor="middle"
              className={cn(
                'text-[10.5px] transition-colors',
                hoveredIdx === index ? 'fill-foreground font-medium' : 'fill-muted-foreground',
              )}
            >
              {label}
            </text>
          ) : null,
        )}
      </svg>

      {/* Floating tooltip on hover. Edge-aware: near the first/last point the
          box aligns to that edge instead of centering, so it never spills
          outside the card on a narrow phone. Hidden on touch (no hover). */}
      {hoveredIdx != null ? (
        <div
          className={cn(
            'pointer-events-none absolute -top-2 z-20 hidden -translate-y-full rounded-lg border border-border bg-card px-3 py-1.5 shadow-xl sm:block',
            hoveredIdx === 0 ? 'translate-x-0' : hoveredIdx === pointCount - 1 ? '-translate-x-full' : '-translate-x-1/2',
          )}
          style={{
            left: `${((xAt(hoveredIdx) - PADDING_LEFT) / innerWidth) * 100}%`,
            maxWidth: 'min(240px, calc(100% - 1rem))',
          }}
        >
          <p className="text-[10px] font-medium text-muted-foreground">{labels[hoveredIdx]}</p>
          <div className="mt-0.5 space-y-0.5">
            {series.map((s) => (
              <p key={s.name} className="flex items-center gap-1.5 text-xs font-semibold tabular-nums text-foreground">
                <span className="inline-block size-1.5 rounded-full" style={{ backgroundColor: s.color }} />
                <span>{s.name}:</span>
                <span>{formatValue(s.values[hoveredIdx] ?? 0)}</span>
              </p>
            ))}
          </div>
        </div>
      ) : null}

      {series.length > 1 ? (
        <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-1">
          {series.map((item) => (
            <span key={item.name} className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className="inline-block size-2 rounded-full" style={{ backgroundColor: item.color }} />
              {item.name}
            </span>
          ))}
        </div>
      ) : null}
    </div>
  );
}
