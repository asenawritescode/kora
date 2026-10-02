import React, { type ComponentType } from 'react'
import { DocumentForm } from '../forms/DocumentForm'
import { StatCard } from '../charts/StatCard'
import { DonutChart } from '../charts/DonutChart'
import { TimeSeriesChart } from '../charts/TimeSeriesChart'

export interface ViewComponentProps {
  config: RegisteredComponentConfig
  data?: any
  isLoading?: boolean
  disabled?: boolean
  readonly?: boolean
  children?: React.ReactNode
  onAction: (actionId: string, context: Record<string, any>) => Promise<any> | any
}

export interface RegisteredComponentConfig {
  id: string
  type: string
  region: string
  label?: string
  source_doctype?: string
  capabilities?: string[]
  bindings?: Record<string, string>
  actions?: Array<{ id: string; trigger: string; type: string; config?: Record<string, any> }>
  desktop_columns?: string[]
  mobile_columns?: string[]
  filters?: Array<{ field: string; op?: string; value?: unknown }>
  components?: RegisteredComponentConfig[]
  position: number
  span?: number
}

export type ViewCapability =
  | 'tables'
  | 'forms'
  | 'detail'
  | 'lists'
  | 'cards'
  | 'filters'
  | 'workflow'
  | 'split'
  | 'kanban'
  | 'calendar'
  | 'dashboard'
  | 'charts'
  | 'analytics'
  | 'scanner'
  | 'commerce'
  | 'document'
  | 'wizard'
  | 'public'
  | 'print'

type LazyEntry = {
  loader: () => Promise<{ default: ComponentType<ViewComponentProps> }>
  preload?: () => Promise<unknown>
}

type ComponentEntry = {
  component: ComponentType<ViewComponentProps>
  requiredCapabilities?: ViewCapability[]
  preload?: () => Promise<unknown>
}

const createLazyEntry = (
  loader: () => Promise<{ default: ComponentType<ViewComponentProps> }>,
  requiredCapabilities: ViewCapability[] = [],
): ComponentEntry => ({
  component: React.lazy(loader),
  preload: loader,
  requiredCapabilities,
})

const createStaticEntry = (
  component: ComponentType<ViewComponentProps>,
  requiredCapabilities: ViewCapability[] = [],
): ComponentEntry => ({
  component,
  requiredCapabilities,
})

const KanbanBoard = React.lazy(() => import('./KanbanBoard'))
const ApprovalQueue = React.lazy(() => import('./ApprovalQueue'))
const CalendarView = React.lazy(() => import('./CalendarView'))
const DashboardGrid = React.lazy(() => import('./DashboardGrid'))
const MetricCard = React.lazy(() => import('./MetricCard'))
const ChartView = React.lazy(() => import('./ChartView'))
const ScannerInput = React.lazy(() => import('./ScannerInput'))
const ProductGrid = React.lazy(() => import('./ProductGrid'))
const CartPanel = React.lazy(() => import('./CartPanel'))
const PaymentPanel = React.lazy(() => import('./PaymentPanel'))
const ScannerCount = React.lazy(() => import('./ScannerCount'))
const DocumentPreview = React.lazy(() => import('./DocumentPreview'))
const ConfirmationStep = React.lazy(() => import('./ConfirmationStep'))
const ReceiptPreview = React.lazy(() => import('./ReceiptPreview'))
const Drawer = React.lazy(() => import('./Drawer'))
const CategoryTabs = React.lazy(() => import('./CategoryTabs'))
const Tabs = React.lazy(() => import('./Tabs'))
const LineItemBuilder = React.lazy(() => import('./LineItemBuilder'))
const Wizard = React.lazy(() => import('./Wizard'))
const Checklist = React.lazy(() => import('./Checklist'))
const RecentRecords = React.lazy(() => import('./RecentRecords'))
const PublicForm = React.lazy(() => import('./PublicForm'))
const WorkspaceDashboard = React.lazy(() => import('./WorkspaceDashboard'))
const PrintLayout = React.lazy(() => import('./PrintLayout'))

function RecordTable(props: ViewComponentProps) {
  const { data, isLoading } = props
  if (isLoading) return <div className="p-4 text-muted-foreground" role="status" aria-live="polite">Loading records…</div>
  if (!data?.data?.length) return <div className="rounded-lg border border-dashed p-6 text-center text-muted-foreground text-sm" role="status">No records found</div>
  const columns = props.config.desktop_columns || Object.keys(data.data[0] || {}).filter(k => !k.startsWith('_'))
  return (
    <div className="overflow-x-auto rounded-lg border">
      <table className="min-w-[1000px] w-full text-sm" aria-label={props.config.label || `${props.config.source_doctype || 'Records'} table`}>
        <caption className="sr-only">{props.config.label || `${props.config.source_doctype || 'Records'} table`}</caption>
        <thead className="bg-muted/50">
          <tr>{columns.map((col: string) => <th key={col} scope="col" className="whitespace-nowrap px-4 py-2 text-left font-medium">{col.replace(/_/g, ' ')}</th>)}</tr>
        </thead>
        <tbody>
          {data.data.map((row: any, i: number) => (
            <tr key={row.name || i} className="border-t hover:bg-muted/30 cursor-pointer"
              tabIndex={0}
              onClick={() => props.onAction('select', { name: row.name, row })}
              onKeyDown={(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); props.onAction('select', { name: row.name, row }) } }}>
              {columns.map((col: string) => <td key={col} className="whitespace-nowrap px-4 py-2">{row[col] != null ? String(row[col]) : '—'}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function RecordForm(props: ViewComponentProps) {
  return (
    <DocumentForm
      doctype={props.config.source_doctype || ''}
      label={props.config.label}
      disabled={props.disabled}
      readonly={props.readonly}
      onCreated={(created) => props.onAction('created', { doctype: props.config.source_doctype, name: created.name, document: created })}
    />
  )
}

function RecordDetail(props: ViewComponentProps) {
  const { data, config } = props
  const record = Array.isArray(data?.data) ? data.data[0] : data?.data || data
  const fields = record && typeof record === 'object'
    ? Object.entries(record).filter(([key]) => !key.startsWith('_'))
    : []

  const formatValue = (value: unknown) => {
    if (value == null || value === '') return '—'
    if (Array.isArray(value)) return `${value.length} item${value.length === 1 ? '' : 's'}`
    if (typeof value === 'object') return Object.entries(value as Record<string, unknown>).slice(0, 3).map(([key, entry]) => `${key.replace(/_/g, ' ')}: ${entry ?? '—'}`).join(' · ')
    return String(value)
  }

  return <div className="rounded-lg border p-4"><h3 className="font-semibold text-sm">{config.label || 'Detail'}</h3>
    {fields.length ? (
      <dl className="mt-3 divide-y text-sm">
        {fields.map(([key, value]) => (
          <div key={key} className="grid grid-cols-[minmax(120px,0.7fr)_1fr] gap-4 py-2">
            <dt className="capitalize text-muted-foreground">{key.replace(/_/g, ' ')}</dt>
            <dd className="break-words font-medium">{formatValue(value)}</dd>
          </div>
        ))}
      </dl>
    ) : <p className="mt-2 text-sm text-muted-foreground">Select a record</p>}
  </div>
}

function RecordList(props: ViewComponentProps) {
  const { data, isLoading } = props
  if (isLoading) return <div className="p-4 text-muted-foreground text-sm">Loading...</div>
  if (!data?.data?.length) return <div className="p-4 text-sm text-muted-foreground">No records</div>
  return <div className="space-y-1">{data.data.map((row: any, i: number) => (
    <div key={row.name || i} className="cursor-pointer rounded-md px-3 py-2 hover:bg-muted/50 text-sm"
      onClick={() => props.onAction('select', { name: row.name, row })}>{row.name}</div>
  ))}</div>
}

function RecordCards(props: ViewComponentProps) {
  const { data, isLoading } = props
  if (isLoading) return <div className="p-4 text-muted-foreground text-sm">Loading...</div>
  if (!data?.data?.length) return <div className="p-4 text-sm text-muted-foreground">No records</div>
  return <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
    {data.data.map((row: any, i: number) => (
      <div key={row.name || i} className="cursor-pointer rounded-lg border p-3 hover:bg-muted/30" onClick={() => props.onAction('select', { name: row.name, row })}>
        <p className="font-medium text-sm">{row.name}</p></div>))}
  </div>
}

function FilterBar(props: ViewComponentProps) {
  const rows = Array.isArray(props.data?.data) ? props.data.data : []
  const candidates = ['status', 'network', 'product_type', 'doc_status', 'category', 'is_active']
  const field = candidates.find((candidate) => rows.some((row: Record<string, any>) => row?.[candidate] != null))
  const values = field
    ? [...new Set(rows.map((row: Record<string, any>) => row?.[field]).filter((value: unknown) => value != null && value !== ''))].slice(0, 6)
    : []

  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-card p-3">
      <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Filter</span>
      <button
        type="button"
        className="rounded-full bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground"
        onClick={() => props.onAction('filter', { field: field || '', value: '' })}
      >
        All
      </button>
      {values.length > 0 ? (
        values.map((value) => (
          <button
            key={String(value)}
            type="button"
            className="rounded-full bg-muted px-3 py-1.5 text-xs font-medium hover:bg-muted/80"
            onClick={() => props.onAction('filter', { field: field || '', value })}
          >
            {String(value)}
          </button>
        ))
      ) : (
        <span className="text-sm text-muted-foreground">No filters available</span>
      )}
    </div>
  )
}

function SearchBox(props: ViewComponentProps) {
  return (
    <input
      type="text"
    placeholder="Search..."
    aria-label={props.config.label || 'Search records'}
      className="w-full rounded-md border px-3 py-2 text-sm"
      autoFocus
      onChange={(e) => props.onAction('search', { value: e.target.value })}
    />
  )
}

function WorkflowActions(props: ViewComponentProps) {
  const record = Array.isArray(props.data?.data) ? props.data.data[0] : props.data?.data || props.data
  const context = record?.name ? { name: record.name, row: record } : {}
  return <div className="flex gap-2">
    <button className="rounded-md bg-primary px-4 py-1.5 text-sm text-primary-foreground" onClick={() => props.onAction('submit', context)}>Send</button>
    <button className="rounded-md border px-4 py-1.5 text-sm" onClick={() => props.onAction('cancel', context)}>Cancel</button>
  </div>
}

function InsightsPanel(props: ViewComponentProps) {
  const payload = props.data && typeof props.data === 'object' ? props.data : {}
  const entries = Object.entries(payload as Record<string, any>)
  const stats: { title: string; value: number }[] = []
  const distributions: { title: string; data: { name: string; value: number }[] }[] = []

  for (const [key, value] of entries) {
    if (typeof value === 'number') {
      stats.push({ title: key.replace(/_/g, ' '), value })
      continue
    }
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      const series = Object.entries(value as Record<string, any>)
      if (series.length > 0 && typeof series[0][1] === 'number') {
        distributions.push({
          title: key.replace(/_/g, ' '),
          data: series.map(([name, amount]) => ({ name, value: amount as number })),
        })
      }
    }
  }

  return (
    <div className="space-y-4">
      {stats.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
          {stats.slice(0, 4).map((stat) => (
            <StatCard key={stat.title} title={stat.title} value={stat.value} />
          ))}
        </div>
      )}
      {distributions.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2">
          {distributions.map((item) => (
            <DonutChart key={item.title} title={item.title} data={item.data} height={260} />
          ))}
        </div>
      )}
      {stats.length === 0 && distributions.length === 0 && (
        <div className="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
          No insights data yet.
        </div>
      )}
      {Array.isArray((payload as Record<string, any>).trend) && (
        <TimeSeriesChart title="Trend" data={(payload as Record<string, any>).trend} />
      )}
    </div>
  )
}

function SplitView(props: ViewComponentProps) {
  return <div className="grid h-full gap-4 lg:grid-cols-[280px_1fr] xl:grid-cols-[300px_1fr_260px]">{props.children}</div>
}

function Placeholder(props: ViewComponentProps) {
  return <div className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">{props.config.type} (coming soon)</div>
}


export const COMPONENT_REGISTRY = {
  record_table: createStaticEntry(RecordTable, ['tables']),
  record_form: createStaticEntry(RecordForm, ['forms']),
  record_detail: createStaticEntry(RecordDetail, ['detail']),
  record_list: createStaticEntry(RecordList, ['lists']),
  record_cards: createStaticEntry(RecordCards, ['cards']),
  filter_bar: createStaticEntry(FilterBar, ['filters']),
  search_box: createStaticEntry(SearchBox, ['filters']),
  workflow_actions: createStaticEntry(WorkflowActions, ['workflow']),
  insights_panel: createStaticEntry(InsightsPanel, ['dashboard', 'charts', 'analytics']),
  split_view: createStaticEntry(SplitView, ['split']),
  kanban_board: createLazyEntry(() => import('./KanbanBoard'), ['kanban']),
  approval_queue: createLazyEntry(() => import('./ApprovalQueue'), ['workflow']),
  calendar_view: createLazyEntry(() => import('./CalendarView'), ['calendar']),
  dashboard_grid: createLazyEntry(() => import('./DashboardGrid'), ['dashboard']),
  metric_card: createLazyEntry(() => import('./MetricCard'), ['dashboard']),
  chart: createLazyEntry(() => import('./ChartView'), ['charts']),
  scanner_input: createLazyEntry(() => import('./ScannerInput'), ['scanner']),
  product_grid: createLazyEntry(() => import('./ProductGrid'), ['commerce']),
  cart_panel: createLazyEntry(() => import('./CartPanel'), ['commerce']),
  payment_panel: createLazyEntry(() => import('./PaymentPanel'), ['commerce']),
  scanner_count: createLazyEntry(() => import('./ScannerCount'), ['scanner']),
  document_preview: createLazyEntry(() => import('./DocumentPreview'), ['document']),
  confirmation_step: createLazyEntry(() => import('./ConfirmationStep'), ['wizard']),
  receipt_preview: createLazyEntry(() => import('./ReceiptPreview'), ['document']),
  drawer: createLazyEntry(() => import('./Drawer'), ['split']),
  category_tabs: createLazyEntry(() => import('./CategoryTabs'), ['commerce']),
  tabs: createLazyEntry(() => import('./Tabs'), ['split']),
  line_item_builder: createLazyEntry(() => import('./LineItemBuilder'), ['commerce']),
  wizard: createLazyEntry(() => import('./Wizard'), ['wizard']),
  checklist: createLazyEntry(() => import('./Checklist'), ['workflow']),
  recent_records: createLazyEntry(() => import('./RecentRecords'), ['lists']),
  public_form: createLazyEntry(() => import('./PublicForm'), ['public', 'forms']),
  workspace_dashboard: createLazyEntry(() => import('./WorkspaceDashboard'), ['dashboard']),
  print_layout: createLazyEntry(() => import('./PrintLayout'), ['print']),
} satisfies Record<string, ComponentEntry>

export type ComponentTypeName = keyof typeof COMPONENT_REGISTRY

export function resolveComponentEntry(type: string, capabilities: string[] = []): ComponentEntry | undefined {
  const entry = COMPONENT_REGISTRY[type as ComponentTypeName]
  if (!entry) return undefined
  if (!entry.requiredCapabilities?.length) return entry
  if (capabilities.length === 0) return entry
  return entry.requiredCapabilities.every((capability) => capabilities.includes(capability)) ? entry : undefined
}

export function UnsupportedComponent({ config }: ViewComponentProps) {
  return (
    <div className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground" role="alert">
      Unsupported component type: {config.type}
    </div>
  )
}
