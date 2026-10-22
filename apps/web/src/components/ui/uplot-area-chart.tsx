import "uplot/dist/uPlot.min.css";
import uPlot from "uplot";
import UPlotReact from "uplot-react";
import { useRef, useState, useEffect, useMemo } from "react";

interface UPlotAreaChartProps {
  timestamps: number[];
  values: (number | null)[];
  color: string;
  yFormatter: (v: number) => string;
  xFormatter: (ts: number) => string; // receives ms
  yDomain?: [number, number];
  height?: number;
  // Charts sharing a syncKey link their cursors: hovering one moves the cursor
  // and shows the tooltip on all of them at the same time point.
  syncKey?: string;
}

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim();
}

function colorWithAlpha(color: string, alpha: number): string {
  // Handles hsl(H, S%, L%) → hsla(H, S%, L%, alpha)
  return color.replace(/^hsl\(/, "hsla(").replace(/\)$/, `, ${alpha})`);
}

function nearestNonNull(
  yData: (number | undefined)[],
  hoveredIdx: number,
): number {
  const val = yData[hoveredIdx];
  if (val != null && !isNaN(val)) return hoveredIdx;
  const len = yData.length;
  for (let d = 1; d < len; d++) {
    const li = hoveredIdx - d;
    const ri = hoveredIdx + d;
    if (li >= 0 && yData[li] != null && !isNaN(yData[li]!)) return li;
    if (ri < len && yData[ri] != null && !isNaN(yData[ri]!)) return ri;
  }
  return hoveredIdx;
}

function toAlignedData(
  timestamps: number[],
  values: (number | null)[],
): uPlot.AlignedData {
  return [
    timestamps.map((t) => t / 1000), // ms → Unix seconds
    values.map((v) => (v === null ? NaN : v)), // null → NaN for spanGaps
  ];
}

export function UPlotAreaChart({
  timestamps,
  values,
  color,
  yFormatter,
  xFormatter,
  yDomain,
  height = 200,
  syncKey,
}: UPlotAreaChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const uplotRef = useRef<uPlot | null>(null);
  const cursorPosRef = useRef<{ left: number; top: number } | null>(null);
  const [width, setWidth] = useState(300);
  const [tooltip, setTooltip] = useState<{
    left: number;
    top: number;
    value: string;
    time: string;
  } | null>(null);

  // Responsive sizing via ResizeObserver
  useEffect(() => {
    if (!containerRef.current) return;
    const ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width;
      if (w && w > 0) setWidth(Math.floor(w));
    });
    ro.observe(containerRef.current);
    return () => ro.disconnect();
  }, []);

  const options = useMemo<uPlot.Options>(() => {
    const borderColor = cssVar("--border");
    const mutedColor = cssVar("--muted-foreground");

    const gradientFn = (u: uPlot): CanvasGradient | string => {
      const { top, height } = u.bbox;
      if (!height || !isFinite(top) || !isFinite(height)) {
        return colorWithAlpha(color, 0.1);
      }
      const grad = u.ctx.createLinearGradient(0, top, 0, top + height);
      grad.addColorStop(0, colorWithAlpha(color, 0.3));
      grad.addColorStop(1, colorWithAlpha(color, 0.02));
      return grad;
    };

    return {
      width,
      height,
      cursor: {
        show: true,
        // Snap cursor to nearest non-null data point so the dot marker
        // doesn't drift to y=0 (top of chart) when hovering over NaN gaps
        dataIdx: (u, seriesIdx, hoveredIdx) => {
          if (seriesIdx !== 1 || hoveredIdx == null) return hoveredIdx;
          return nearestNonNull(
            u.data[1] as (number | undefined)[],
            hoveredIdx,
          );
        },
        // Link cursors across charts that share a key, matching on the x scale
        // (the time axis) so the same time point is highlighted everywhere.
        ...(syncKey
          ? { sync: { key: syncKey, setSeries: false, scales: ["x", null] } }
          : {}),
      },
      padding: [8, 0, 0, 0],
      series: [
        {},
        {
          stroke: color,
          fill: gradientFn as uPlot.Series["fill"],
          width: 1.5,
          spanGaps: true,
          points: { show: false },
          value: (_, v) => (v == null || isNaN(v) ? "—" : yFormatter(v)),
        },
      ],
      axes: [
        {
          values: (_, ticks) => ticks.map((t) => xFormatter(t * 1000)),
          size: 30,
          gap: 5,
          stroke: mutedColor,
          ticks: { stroke: borderColor, width: 1 },
          grid: { stroke: borderColor, width: 1, dash: [3, 3] },
          font: "11px sans-serif",
        },
        {
          values: (_, ticks) => ticks.map((v) => yFormatter(v)),
          size: 72,
          gap: 5,
          stroke: mutedColor,
          ticks: { stroke: borderColor, width: 1 },
          grid: { stroke: borderColor, width: 1, dash: [3, 3] },
          font: "11px sans-serif",
        },
      ],
      scales: {
        x: { time: true },
        y: yDomain ? { range: () => yDomain } : {},
      },
      hooks: {
        setCursor: [
          (u) => {
            const left = u.cursor.left;
            // Out-of-bounds cursor: the mouse left this chart, or a synced
            // sibling that was hovered. Clear so stale tooltips don't linger.
            if (left == null || left < 0) {
              cursorPosRef.current = null;
              setTooltip(null);
              return;
            }
            const idx = u.cursor.idx;
            if (idx == null) {
              // Transient null while data shifts under a held (in-bounds)
              // cursor — keep the tooltip rather than flickering it off.
              return;
            }
            const top = u.cursor.top ?? 0;
            cursorPosRef.current = { left, top };
            // Resolve to nearest non-null independently — u.cursor.idx is the
            // raw hovered index, not the dataIdx-resolved one
            const resolvedIdx = nearestNonNull(
              u.data[1] as (number | undefined)[],
              idx,
            );
            const ts = (u.data[0][resolvedIdx] as number) * 1000;
            const val = u.data[1][resolvedIdx] as number | undefined;
            if (val == null || isNaN(val)) return;
            setTooltip({
              left: left + u.bbox.left / devicePixelRatio,
              // Anchor vertically to this series' own data point (not the raw
              // cursor) so every synced chart's tooltip sits on its line.
              top: u.valToPos(val, "y") + u.bbox.top / devicePixelRatio,
              value: yFormatter(val),
              time: new Date(ts).toLocaleString(),
            });
          },
        ],
      },
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [width, height, color, yDomain, syncKey]);

  // Live update: call setData directly without re-mounting
  const alignedData = useMemo(
    () => toAlignedData(timestamps, values),
    [timestamps, values],
  );

  useEffect(() => {
    if (uplotRef.current) {
      uplotRef.current.setData(alignedData);
      // Re-fire cursor at last known position so tooltip stays visible as data shifts
      if (cursorPosRef.current) {
        uplotRef.current.setCursor(cursorPosRef.current);
      }
    }
  }, [alignedData]);

  return (
    <div
      ref={containerRef}
      className="relative w-full"
      onMouseLeave={() => {
        cursorPosRef.current = null;
        setTooltip(null);
      }}
    >
      <UPlotReact
        options={options}
        data={alignedData}
        onCreate={(u) => {
          uplotRef.current = u;
        }}
        onDelete={() => {
          uplotRef.current = null;
        }}
      />
      {tooltip && (
        <div
          className="bg-popover text-popover-foreground pointer-events-none absolute z-10 rounded-md border px-2 py-1.5 text-xs shadow-md"
          style={{ left: tooltip.left + 12, top: tooltip.top - 20 }}
        >
          <p className="font-medium">{tooltip.value}</p>
          <p className="text-muted-foreground">{tooltip.time}</p>
        </div>
      )}
    </div>
  );
}
