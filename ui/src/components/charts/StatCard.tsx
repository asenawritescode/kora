import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { TrendingDown, TrendingUp, Minus } from "lucide-react";

interface StatCardProps {
  title: string;
  value: number | string;
  change?: number; // percentage change
  loading?: boolean;
  error?: string;
  formatter?: (val: number) => string;
}

export function StatCard({ title, value, change, loading, error, formatter }: StatCardProps) {
  if (error) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">{title}</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-destructive">{error}</p>
        </CardContent>
      </Card>
    );
  }

  if (loading) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <Skeleton className="h-4 w-24" />
        </CardHeader>
        <CardContent>
          <Skeleton className="h-8 w-16" />
        </CardContent>
      </Card>
    );
  }

  const displayValue = typeof value === "number" && formatter ? formatter(value) : String(value);
  const trendIcon = change === undefined ? null
    : change > 0 ? <TrendingUp className="h-4 w-4 text-emerald-500" />
    : change < 0 ? <TrendingDown className="h-4 w-4 text-red-500" />
    : <Minus className="h-4 w-4 text-muted-foreground" />;

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-bold">{displayValue}</div>
        {change !== undefined && (
          <p className={`text-xs mt-1 flex items-center gap-1 ${change > 0 ? 'text-emerald-500' : change < 0 ? 'text-red-500' : 'text-muted-foreground'}`}>
            {trendIcon}
            {change > 0 ? '+' : ''}{change}% vs previous period
          </p>
        )}
      </CardContent>
    </Card>
  );
}
