import { BarChart3 } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { fetchInsights, fetchAnalyticsStatus, fetchMetrics } from "@/lib/api/analytics";
import { StatCard } from "@/components/charts/StatCard";
import { TimeSeriesChart } from "@/components/charts/TimeSeriesChart";
import { DonutChart } from "@/components/charts/DonutChart";
import { AlertCircle } from "lucide-react";

interface InsightsPanelProps {
  doctype: string;
}

export function InsightsPanel({ doctype }: InsightsPanelProps) {
  const status = useQuery({
    queryKey: ["analytics", "status"],
    queryFn: fetchAnalyticsStatus,
    staleTime: 30000,
  });

  const metrics = useQuery({
    queryKey: ["analytics", "metrics"],
    queryFn: fetchMetrics,
    staleTime: 60000,
  });

  const insights = useQuery({
    queryKey: ["analytics", "insights", doctype],
    queryFn: () => fetchInsights(doctype),
    staleTime: 30000,
    enabled: !!doctype,
  });

  // Build metric name → label map for human-readable display.
  const labelMap: Record<string, string> = {};
  if (metrics.data) {
    for (const m of metrics.data) {
      labelMap[m.name] = m.label;
    }
  }

  if (!status.data?.enabled) {
    return (
      <div className="flex flex-col items-center justify-center py-16 text-center">
        <AlertCircle className="h-12 w-12 text-muted-foreground mb-4" />
        <h2 className="text-lg font-semibold mb-2">Analytics Not Enabled</h2>
        <p className="text-muted-foreground max-w-md">
          Set <code className="bg-muted px-1 rounded">KORA_ANALYTICS=true</code> to enable analytics for this site.
        </p>
      </div>
    );
  }

  if (insights.isLoading) {
    return (
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4 animate-pulse">
        {[...Array(4)].map((_, i) => (
          <div key={i} className="h-32 bg-muted rounded-lg" />
        ))}
      </div>
    );
  }

  if (insights.error || !insights.data) {
    return (
      <div className="flex items-center justify-center py-16">
        <p className="text-muted-foreground">No analytics data available yet. Data will appear as documents are created.</p>
      </div>
    );
  }

  const data = insights.data;

  // Extract count values (single numbers).
  const stats: { title: string; value: number; change?: number }[] = [];
  // Extract distribution data (objects).
  const distributions: { title: string; data: { name: string; value: number }[] }[] = [];
  // Extract time series.
  const timeSeries: { title: string; data: { bucket: string; value: number }[] }[] = [];

  for (const [key, val] of Object.entries(data)) {
    const title = labelMap[key] || key;
    if (typeof val === "number") {
      stats.push({ title, value: val });
    } else if (typeof val === "object" && val !== null) {
      const entries = Object.entries(val as Record<string, any>);
      if (entries.length > 0 && typeof entries[0][1] === "number") {
        distributions.push({
          title,
          data: entries.map(([k, v]) => ({ name: k, value: v as number })),
        });
      }
    }
  }

  return (
    <div className="space-y-6">
      {stats.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
          {stats.slice(0, 4).map((s) => (
            <StatCard key={s.title} title={s.title} value={s.value} />
          ))}
        </div>
      )}

      {distributions.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {distributions.map((d) => (
            <DonutChart key={d.title} title={d.title} data={d.data} height={280} />
          ))}
        </div>
      )}

      {timeSeries.length > 0 && (
        <div className="grid grid-cols-1 gap-4">
          {timeSeries.map((ts) => (
            <TimeSeriesChart key={ts.title} title={ts.title} data={ts.data} />
          ))}
        </div>
      )}

      {stats.length === 0 && distributions.length === 0 && (
        <div className="flex flex-col items-center justify-center py-16 text-muted-foreground text-sm border-2 border-dashed rounded-lg gap-2">
          <BarChart3 className="h-8 w-8 opacity-30" />
          <p className="font-medium">No analytics data yet</p>
          <p>Create your first {doctype} record to see insights appear here automatically.</p>
          <p className="text-xs opacity-50">Counts, distributions, sums, and trends are pre-computed on every write.</p>
        </div>
      )}
    </div>
  );
}

