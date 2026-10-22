import { cn } from "@/lib/utils";

/**
 * Compact inline area sparkline. Pure SVG (no axes/tooltip) — the right tool
 * for tiny trend chips, where the full UPlotAreaChart's axes would dominate.
 * `null` values are dropped. Stretches to fill its container width.
 */
export function Sparkline({
  values,
  color = "var(--brand)",
  height = 40,
  className,
}: {
  values: (number | null)[];
  color?: string;
  height?: number;
  className?: string;
}) {
  const pts = values.filter((v): v is number => v != null && !isNaN(v));

  if (pts.length < 2) {
    return <div className={className} style={{ height }} aria-hidden="true" />;
  }

  const width = 100; // viewBox units; scaled to container via preserveAspectRatio
  const min = Math.min(...pts);
  const max = Math.max(...pts);
  const range = max - min || 1;
  const stepX = width / (pts.length - 1);

  const coords = pts.map(
    (v, i) =>
      [i * stepX, height - ((v - min) / range) * height] as [number, number],
  );
  const line = coords
    .map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`)
    .join(" ");
  const area = `${line} L${width},${height} L0,${height} Z`;

  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={cn("w-full", className)}
      style={{ height }}
      aria-hidden="true"
    >
      <path d={area} fill={color} fillOpacity={0.12} />
      <path
        d={line}
        fill="none"
        stroke={color}
        strokeWidth={1.5}
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
