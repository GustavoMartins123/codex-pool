import { lazy, Suspense, useEffect, useRef, useState, type ReactNode } from "react";
import type { CartesianChartProps } from "./components/dither-kit/cartesian-root";
import type { SparklineProps } from "./components/dither-kit/sparkline";

export type { ChartConfig } from "./components/dither-kit/chart-context";
export type { DitherColor } from "./components/dither-kit/palette";

const kit = () => import("./components/dither-kit");
const LazyAreaChart = lazy(() => kit().then(m => ({ default: m.AreaChart })));
const LazyLineChart = lazy(() => kit().then(m => ({ default: m.LineChart })));
const LazyBarChart = lazy(() => kit().then(m => ({ default: m.BarChart })));
const LazySparkline = lazy(() => kit().then(m => ({ default: m.Sparkline })));
export const Area = lazy(() => kit().then(m => ({ default: m.Area })));
export const Line = lazy(() => kit().then(m => ({ default: m.Line })));
export const Bar = lazy(() => kit().then(m => ({ default: m.Bar })));
export const XAxis = lazy(() => kit().then(m => ({ default: m.XAxis })));
export const YAxis = lazy(() => kit().then(m => ({ default: m.YAxis })));
export const Grid = Object.assign(lazy(() => kit().then(m => ({ default: m.Grid }))), { chartLayer: "back" });
export const Legend = Object.assign(lazy(() => kit().then(m => ({ default: m.Legend }))), { chartLayer: "dom" });
export const Tooltip = Object.assign(lazy(() => kit().then(m => ({ default: m.Tooltip }))), { chartLayer: "dom" });

export function DeferredChart({ children, decorative = false }: { children: ReactNode; decorative?: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!ref.current) return;
    if (typeof IntersectionObserver !== "function") {
      setError("Chart visibility detection is unavailable.");
      return;
    }
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) {
        setVisible(true);
        observer.disconnect();
      }
    });
    observer.observe(ref.current);
    return () => observer.disconnect();
  }, []);
  return <div ref={ref} className="deferred-chart" aria-hidden={decorative || undefined}>
    {error ? <p role="alert">{error}</p> : visible ? <Suspense fallback={<span role={decorative ? undefined : "status"}>Loading chart…</span>}>{children}</Suspense> : <span className="chart-placeholder">{decorative ? "" : "Chart loads when visible"}</span>}
  </div>;
}

function ChartData<T extends object>({ data, config }: Pick<CartesianChartProps<T>, "data" | "config">) {
  const first = data[0] as Record<string, unknown> | undefined;
  const label = Object.keys(first ?? {}).find(key => !(key in config));
  const keys = Object.keys(config);
  return <details className="chart-data">
    <summary>View chart data</summary>
    <div className="chart-data-scroll" tabIndex={0} role="region" aria-label="Chart values">
      <table><caption>Chart values</caption><thead><tr><th scope="col">{label ?? "Row"}</th>{keys.map(key => <th scope="col" key={key}>{config[key].label ?? key}</th>)}</tr></thead>
        <tbody>{data.map((row, index) => {
          const values = row as Record<string, unknown>;
          return <tr key={index}><th scope="row">{label ? String(values[label] ?? "") : index + 1}</th>{keys.map(key => <td key={key}>{String(values[key] ?? "")}</td>)}</tr>;
        })}</tbody>
      </table>
    </div>
  </details>;
}

export function AreaChart<T extends object>(props: CartesianChartProps<T>) {
  return <div className="chart-with-data"><DeferredChart><LazyAreaChart {...props} /></DeferredChart><ChartData {...props} /></div>;
}
export function LineChart<T extends object>(props: CartesianChartProps<T>) {
  return <div className="chart-with-data"><DeferredChart><LazyLineChart {...props} /></DeferredChart><ChartData {...props} /></div>;
}
export function BarChart<T extends object>(props: CartesianChartProps<T>) {
  return <div className="chart-with-data"><DeferredChart><LazyBarChart {...props} /></DeferredChart><ChartData {...props} /></div>;
}
export function Sparkline(props: SparklineProps) {
  return <DeferredChart decorative><LazySparkline {...props} /></DeferredChart>;
}
