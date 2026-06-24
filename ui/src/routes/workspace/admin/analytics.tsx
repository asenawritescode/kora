import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchAnalyticsStatus, fetchMetrics, createMetric } from "@/lib/api/analytics";
import { fetchNavigation } from "@/lib/api/system";
import { fetchDoctypeSchema } from "@/lib/api/system";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { Activity, BarChart3, CircleCheck, CircleX, Plus, Trash2 } from "lucide-react";

export default function AdminAnalyticsPage() {
  const queryClient = useQueryClient();
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({
    name: "", doctype: "", type: "count", field: "", group_by_field: "",
    link_field: "", label: "",
  });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const status = useQuery({ queryKey: ["analytics", "status"], queryFn: fetchAnalyticsStatus, staleTime: 15000 });
  const metrics = useQuery({ queryKey: ["analytics", "metrics"], queryFn: fetchMetrics, staleTime: 30000 });
  const nav = useQuery({ queryKey: ["navigation"], queryFn: fetchNavigation, staleTime: 60000 });

  const selectedDoctype = form.doctype;
  const schema = useQuery({
    queryKey: ["doctype", selectedDoctype],
    queryFn: () => fetchDoctypeSchema(selectedDoctype),
    enabled: !!selectedDoctype,
    staleTime: 60000,
  });

  const doctypes = nav.data?.modules?.flatMap((m: any) => m.doctypes || []) || [];
  const dataFields = schema.data?.doctype?.fields?.filter((f: any) =>
    !["Section Break", "Column Break", "Heading", "Table", "Password", "Attach", "Attach Image", "JSON", "Text Editor"].includes(f.fieldtype)
  ) || [];
  const linkFields = dataFields.filter((f: any) => f.fieldtype === "Link" || f.fieldtype === "Dynamic Link");

  const handleCreate = async () => {
    if (!form.name || !form.doctype) { setError("Name and DocType are required."); return; }
    setSaving(true);
    setError("");
    try {
      await createMetric(form);
      queryClient.invalidateQueries({ queryKey: ["analytics", "metrics"] });
      setShowForm(false);
      setForm({ name: "", doctype: "", type: "count", field: "", group_by_field: "", link_field: "", label: "" });
    } catch (e: any) {
      setError(e.message || "Failed to create metric");
    }
    setSaving(false);
  };

  // Separate auto-generated from custom (custom metrics have auto_generated: false).
  const autoMetrics = metrics.data?.filter((m: any) => m.auto_generated !== false) || [];
  const customMetrics = metrics.data?.filter((m: any) => m.auto_generated === false) || [];

  return (
    <div className="p-8">
      <h1 className="text-2xl font-bold tracking-tight mb-6">Analytics</h1>

      {/* Status */}
      <Card className="mb-6">
        <CardHeader><CardTitle className="text-sm font-medium flex items-center gap-2"><Activity className="h-4 w-4" />Pipeline Status</CardTitle></CardHeader>
        <CardContent>
          {status.isLoading ? <Skeleton className="h-8 w-48" /> : status.data ? (
            <div className="flex items-center gap-4">
              {status.data.enabled ? <CircleCheck className="h-4 w-4 text-emerald-500" /> : <CircleX className="h-4 w-4 text-muted-foreground" />}
              <span className="text-sm">{status.data.enabled ? "Enabled" : "Disabled"}</span>
              {status.data.enabled && <Badge variant="outline">Events Dropped: {status.data.events_dropped}</Badge>}
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Metric Builder */}
      <Card className="mb-6">
        <CardHeader className="flex flex-row items-center justify-between">
          <CardTitle className="text-sm font-medium flex items-center gap-2"><BarChart3 className="h-4 w-4" />Custom Metrics</CardTitle>
          <Button size="sm" variant="outline" onClick={() => setShowForm(!showForm)}>
            <Plus className="h-3 w-3 mr-1" /> New Metric
          </Button>
        </CardHeader>
        <CardContent>
          {showForm && (
            <div className="border rounded-lg p-4 mb-4 space-y-4">
              <div className="grid grid-cols-2 gap-4">
                <div>
                  <Label>Metric Name</Label>
                  <Input placeholder="e.g. high_value_sales" value={form.name} onChange={e => setForm({...form, name: e.target.value})} />
                </div>
                <div>
                  <Label>Label</Label>
                  <Input placeholder="e.g. High Value Sales" value={form.label} onChange={e => setForm({...form, label: e.target.value})} />
                </div>
              </div>
              <div className="grid grid-cols-3 gap-4">
                <div>
                  <Label>DocType</Label>
                  <Select value={form.doctype} onValueChange={(v) => setForm({...form, doctype: v || "", field: "", group_by_field: "", link_field: ""})}>
                    <SelectTrigger><SelectValue placeholder="Select..." /></SelectTrigger>
                    <SelectContent>
                      {doctypes.map((d: any) => <SelectItem key={d.name} value={d.name}>{d.name}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div>
                  <Label>Type</Label>
                  <Select value={form.type} onValueChange={(v) => setForm({...form, type: v || ""})}>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="count">Count</SelectItem>
                      <SelectItem value="count_by_field">Count By Field</SelectItem>
                      <SelectItem value="count_by_linked_field">Count By Linked Field</SelectItem>
                      <SelectItem value="sum">Sum</SelectItem>
                      <SelectItem value="avg">Average</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div>
                  <Label>{form.type === "count_by_linked_field" ? "Link Field" : form.type !== "count" ? "Field" : "Group By (optional)"}</Label>
                  {form.type === "count_by_linked_field" ? (
                    <Select value={form.link_field || ""} onValueChange={(v) => setForm({...form, link_field: v || ""})}>
                      <SelectTrigger><SelectValue placeholder="Select link field..." /></SelectTrigger>
                      <SelectContent>
                        {linkFields.map((f: any) => <SelectItem key={f.fieldname} value={f.fieldname}>{f.label}</SelectItem>)}
                      </SelectContent>
                    </Select>
                  ) : (
                    <Select value={form.field || ""} onValueChange={(v) => setForm({...form, field: v || ""})}>
                      <SelectTrigger><SelectValue placeholder="Select field..." /></SelectTrigger>
                      <SelectContent>
                        {dataFields.map((f: any) => <SelectItem key={f.fieldname} value={f.fieldname}>{f.label} ({f.fieldtype})</SelectItem>)}
                      </SelectContent>
                    </Select>
                  )}
                </div>
              </div>
              {form.type === "count_by_linked_field" && form.link_field && (
                <div className="grid grid-cols-2 gap-4">
                  <div>
                    <Label>Group By Field (on linked doctype)</Label>
                    <Input placeholder="e.g. city" value={form.group_by_field} onChange={e => setForm({...form, group_by_field: e.target.value})} />
                  </div>
                </div>
              )}
              {error && <p className="text-sm text-destructive">{error}</p>}
              <div className="flex gap-2">
                <Button size="sm" onClick={handleCreate} disabled={saving}>{saving ? "Saving..." : "Save Metric"}</Button>
                <Button size="sm" variant="ghost" onClick={() => setShowForm(false)}>Cancel</Button>
              </div>
            </div>
          )}

          {customMetrics.length === 0 && !showForm && (
            <p className="text-sm text-muted-foreground">No custom metrics yet. Click "New Metric" to create one.</p>
          )}
          {customMetrics.map((m: any) => (
            <div key={m.name} className="flex items-center justify-between text-sm py-1 border-b border-muted last:border-0">
              <div>
                <span className="font-medium">{m.label || m.name}</span>
                <span className="text-muted-foreground ml-2">({m.doctype})</span>
              </div>
              <div className="flex items-center gap-2">
                <Badge variant="secondary" className="text-xs">{m.type}</Badge>
              </div>
            </div>
          ))}
        </CardContent>
      </Card>

      {/* Auto-generated Metrics */}
      <Card>
        <CardHeader><CardTitle className="text-sm font-medium flex items-center gap-2"><BarChart3 className="h-4 w-4" />Auto-Generated Metrics</CardTitle></CardHeader>
        <CardContent>
          {metrics.isLoading ? [...Array(5)].map((_, i) => <Skeleton key={i} className="h-4 w-full mb-2" />) :
           autoMetrics.length > 0 ? autoMetrics.map((m: any) => (
            <div key={m.name} className="flex items-center justify-between text-sm py-1 border-b border-muted last:border-0">
              <span className="font-medium">{m.label}</span>
              <div className="flex items-center gap-2 text-muted-foreground">
                <Badge variant="secondary" className="text-xs">{m.type}</Badge>
                <span className="text-xs">{m.doctype}</span>
              </div>
            </div>
          )) : <p className="text-sm text-muted-foreground">No metrics yet.</p>}
        </CardContent>
      </Card>
    </div>
  );
}
