import { useEffect, useRef, useState } from "react";
import * as echarts from "echarts/core";
import { BarChart, HeatmapChart, LineChart } from "echarts/charts";
import { DataZoomComponent, GridComponent, LegendComponent, MarkAreaComponent, TooltipComponent, VisualMapComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import type { EChartsOption } from "echarts";
import { useTranslation } from "react-i18next";

echarts.use([BarChart, LineChart, HeatmapChart, GridComponent, LegendComponent, TooltipComponent, MarkAreaComponent, VisualMapComponent, DataZoomComponent, CanvasRenderer]);

// EChart mounts one ECharts instance and keeps it showing option. A chart
// that cannot start (no canvas) says so in its place instead of taking the
// page down with it.
export default function EChart({ option, height, label }: { option: EChartsOption; height: number; label: string }) {
  const { t } = useTranslation("analytics");
  const hostRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    if (!hostRef.current) return;
    let chart: ReturnType<typeof echarts.init>;
    try {
      chart = echarts.init(hostRef.current, undefined, { renderer: "canvas" });
    } catch {
      setFailed(true);
      return;
    }
    chartRef.current = chart;
    const resize = () => chart.resize();
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);
    window.addEventListener("resize", resize);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", resize);
      chart.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    chartRef.current?.setOption(option, true);
  }, [option]);

  useEffect(() => {
    chartRef.current?.resize();
  }, [height]);

  if (failed) return <div style={{ height, display: "flex", alignItems: "center", justifyContent: "center" }}>{t("chart.unavailable")}</div>;
  return <div ref={hostRef} role="img" aria-label={label} style={{ width: "100%", height }} />;
}
