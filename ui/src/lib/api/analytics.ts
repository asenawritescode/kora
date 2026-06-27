import { api } from "./client";

export interface AnalyticsStatus {
  enabled: boolean;
  events_processed: number;
  events_dropped: number;
}

export interface Metric {
  name: string;
  label: string;
  type: string;
  doctype: string;
  field?: string;
  auto_generated: boolean;
}

export interface QueryRequest {
  from?: string;
  to?: string;
  group_by?: string;
}

export interface QueryResult {
  metric: string;
  columns: string[];
  rows: Record<string, any>[];
  total: number;
}

export async function fetchAnalyticsStatus(): Promise<AnalyticsStatus> {
  return api.get("/api/v1/analytics/status");
}

export async function fetchMetrics(): Promise<Metric[]> {
  return api.get("/api/v1/analytics/metrics");
}

export async function queryMetric(name: string, req?: QueryRequest): Promise<QueryResult> {
  return api.post(`/api/v1/analytics/metrics/${encodeURIComponent(name)}/query`, req || {});
}

export async function fetchInsights(doctype: string): Promise<Record<string, any>> {
  return api.get(`/api/v1/analytics/insights/${encodeURIComponent(doctype)}`);
}

export async function createMetric(metric: {
  name: string; label?: string; type: string; doctype: string;
  field?: string; link_field?: string; group_by_field?: string;
}): Promise<Metric> {
  return api.post("/api/v1/analytics/metrics", metric);
}
