import { useEffect, useMemo, useState } from 'react'
import { ManifestRenderer } from '@/manifest/runtime/ManifestRenderer'
import { koraStudioDataAdapter } from '@/manifest/runtime/data-adapter'
import { executeLocalQuery, type QueryAnswer } from '@/manifest/query/query-executor'
import type { QuerySpec } from '@/manifest/query/query-spec'
import { addManifestComponent, createBlankPageManifest, PAGE_COMPONENT_LIBRARY, type PageComponent, type PageManifest } from '@/manifest/schema/page'
import { validatePageManifestContract } from '@/manifest/schema/page'
import { acceptGeneratedPage, createGeneratedRevision, GENERATED_COMPONENT_ALLOWLIST, type GeneratedPageRevision, type GeneratedTenantConfig } from '@/manifest/runtime/generated-page'
import { loadGeneratedRevision, saveGeneratedRevision } from '@/manifest/runtime/generated-page-store'
import { Badge, Button, Card, CardContent, CardHeader, CardTitle, ConfirmDialog, Textarea } from '@/components/views/StudioPrimitives'
import { Sparkles, PanelsTopLeft, Palette, PencilLine, Code2, WandSparkles, Monitor, Tablet, Smartphone, X } from 'lucide-react'

const starterManifest = (): PageManifest => ({
  ...createBlankPageManifest(),
  metadata: {
    ...createBlankPageManifest().metadata,
    name: 'Studio Home',
    package: 'studio.workspace',
    status: 'preview',
  },
  spec: {
    ...createBlankPageManifest().spec,
    route: '/studio-home',
    capabilities: ['dashboard', 'tables'],
    resources: [{ id: 'primary', query: 'document.list', params: { doctype: '', limit: 50 } }],
    layout: {
      type: 'single',
      columns: 12,
      children: [
        {
          id: 'summary_cards', component: 'dashboard_grid', version: 1, region: 'main', position: 0,
          props: { title: 'Record summary' }, required_capabilities: ['dashboard'], offline: 'read_only',
          children: [
            { id: 'total_records', component: 'metric_card', version: 1, region: 'main', position: 0, props: { title: 'Total records', bindings: { metric: 'count' } }, data: 'primary.data', required_capabilities: ['dashboard'] },
            { id: 'active_records', component: 'metric_card', version: 1, region: 'main', position: 1, props: { title: 'Active records', bindings: { metric: 'count', filter_field: 'status', filter_value: 'Active' } }, data: 'primary.data', required_capabilities: ['dashboard'] },
            { id: 'recent_records', component: 'metric_card', version: 1, region: 'main', position: 2, props: { title: 'Recent records', bindings: { metric: 'count' } }, data: 'primary.data', required_capabilities: ['dashboard'] },
            { id: 'all_records', component: 'metric_card', version: 1, region: 'main', position: 3, props: { title: 'Available records', bindings: { metric: 'count' } }, data: 'primary.data', required_capabilities: ['dashboard'] },
          ],
        },
        { id: 'query_summary', component: 'query_summary', version: 1, region: 'main', position: 1, props: { label: 'Question context', bindings: { summary: 'Connected records grouped for review.' } }, required_capabilities: [], offline: 'read_only' },
        { id: 'data_provenance', component: 'data_provenance', version: 1, region: 'main', position: 2, props: { bindings: { updated_at: 'just now' } }, required_capabilities: [], offline: 'read_only' },
        { id: 'data_quality', component: 'data_quality', version: 1, region: 'main', position: 3, props: {}, data: 'primary.data', required_capabilities: [], offline: 'read_only' },
        { id: 'primary_table', component: 'record_table', version: 1, region: 'main', position: 4, props: { title: 'Records', desktop_columns: ['name', 'status'] }, data: 'primary.data', required_capabilities: ['tables'], offline: 'read_only' },
      ],
    },
  },
})

const studioTenant: GeneratedTenantConfig = {
  tenantId: 'studio-workspace',
  theme: 'zinc',
  allowedComponents: GENERATED_COMPONENT_ALLOWLIST,
}

const starterQuestions = [
  'How many records are active?',
  'Show the most recently updated records.',
  'Summarize this resource for leadership.',
]

const defaultQuery: QuerySpec = { resource: '', filters: [], groupBy: [], metrics: [{ type: 'count', label: 'Total records' }], columns: ['name', 'status'], visualization: 'table' }

function findComponent(components: PageComponent[], id: string | null): PageComponent | null {
  if (!id) return null
  for (const component of components) {
    if (component.id === id) return component
    const nested = findComponent(component.children ?? [], id)
    if (nested) return nested
  }
  return null
}

function updateComponents(
  components: PageComponent[],
  id: string,
  updater: (component: PageComponent) => PageComponent,
): PageComponent[] {
  return components.map((component) => {
    if (component.id === id) return updater(component)
    if (!component.children?.length) return component
    return { ...component, children: updateComponents(component.children, id, updater) }
  })
}

function removeComponent(components: PageComponent[], id: string): PageComponent[] {
  return components
    .filter((component) => component.id !== id)
    .map((component) => component.children?.length
      ? { ...component, children: removeComponent(component.children, id) }
      : component)
}

function duplicateComponentTree(component: PageComponent): PageComponent {
  const copyId = `${component.id}_copy_${Date.now()}`
  return {
    ...component,
    id: copyId,
    children: component.children?.map((child) => duplicateComponentTree(child)),
  }
}

function duplicateComponent(components: PageComponent[], id: string): { components: PageComponent[]; copyId: string | null } {
  const index = components.findIndex((component) => component.id === id)
  if (index >= 0) {
    const copy = duplicateComponentTree(components[index])
    return { components: [...components.slice(0, index + 1), copy, ...components.slice(index + 1)], copyId: copy.id }
  }
  for (let index = 0; index < components.length; index += 1) {
    const component = components[index]
    if (!component.children?.length) continue
    const result = duplicateComponent(component.children, id)
    if (result.copyId) {
      const next = [...components]
      next[index] = { ...component, children: result.components }
      return { components: next, copyId: result.copyId }
    }
  }
  return { components, copyId: null }
}

export default function StudioPage() {
  const [resourceState, setResourceState] = useState<Record<string, Awaited<ReturnType<typeof koraStudioDataAdapter.load>>>>({})
  const [revision, setRevision] = useState<GeneratedPageRevision>(() => loadGeneratedRevision(studioTenant, starterManifest()))
  const [manifest, setManifest] = useState<PageManifest>(() => revision.manifest)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [aiPrompt, setAiPrompt] = useState('Build a focused workspace for the connected data with filters and a detail panel.')
  const [codeOpen, setCodeOpen] = useState(false)
  const [publishedRevision, setPublishedRevision] = useState<number | null>(null)
  const [pastManifests, setPastManifests] = useState<PageManifest[]>([])
  const [futureManifests, setFutureManifests] = useState<PageManifest[]>([])
  const [viewport, setViewport] = useState<'desktop' | 'tablet' | 'mobile'>('desktop')
  const [proposedManifest, setProposedManifest] = useState<PageManifest | null>(null)
  const [activeQuery, setActiveQuery] = useState<QuerySpec>(defaultQuery)
  const [sourceDoctype, setSourceDoctype] = useState('Kora resource')
  const [confirmAction, setConfirmAction] = useState<'remove' | 'reset' | null>(null)

  useEffect(() => {
    void koraStudioDataAdapter.load('primary').then((state) => {
      setResourceState({ primary: state })
      const doctype = state.data?.meta?.doctype
      if (state.kind === 'normal' && typeof doctype === 'string') {
        setSourceDoctype(doctype)
        setActiveQuery((query) => ({ ...query, resource: doctype }))
        const firstRecord = Array.isArray(state.data?.data) ? state.data.data[0] : null
        const liveColumns = firstRecord && typeof firstRecord === 'object'
          ? Object.keys(firstRecord).filter((key) => !key.startsWith('_')).slice(0, 8)
          : ['name']
        setManifest((current) => ({
          ...current,
          spec: {
            ...current.spec,
            resources: current.spec.resources.map((resource) => resource.id === 'primary' ? { ...resource, params: { ...resource.params, doctype } } : resource),
            layout: { ...current.spec.layout, children: updateComponents(current.spec.layout.children, 'primary_table', (component) => ({ ...component, props: { ...component.props, source_doctype: doctype, desktop_columns: liveColumns } })) },
          },
        }))
      }
    })
  }, [])

  const issues = useMemo(() => validatePageManifestContract(manifest), [manifest])
  const selected = useMemo(() => findComponent(manifest.spec.layout.children, selectedId), [manifest.spec.layout.children, selectedId])
  const queryAnswer = useMemo<QueryAnswer>(() => executeLocalQuery(activeQuery, resourceState.primary?.data), [activeQuery, resourceState.primary])

  const runQuestion = (question: string) => {
    setAiPrompt(question)
    if (/active.*department/i.test(question)) setActiveQuery({ ...defaultQuery, filters: [{ field: 'status', operator: 'equals', value: 'Active' }] })
    else if (/joined this month/i.test(question)) setActiveQuery({ ...defaultQuery, filters: [{ field: 'joined', operator: 'between', value: ['2026-08-01', '2026-08-31'] }] })
    else setActiveQuery(defaultQuery)
  }

  const syncSource = (next: PageManifest) => {
    const accepted = acceptGeneratedPage(next, studioTenant)
    if (!accepted.manifest) return
    setPastManifests((past) => [...past.slice(-19), manifest])
    setFutureManifests([])
    const nextRevision = createGeneratedRevision(accepted.manifest, studioTenant, revision.revision + 1)
    setManifest(accepted.manifest)
    setRevision(nextRevision)
    saveGeneratedRevision(nextRevision)
  }

  const restoreManifest = (next: PageManifest, remaining: PageManifest[], future: PageManifest[]) => {
    const accepted = acceptGeneratedPage(next, studioTenant)
    if (!accepted.manifest) return
    const nextRevision = createGeneratedRevision(accepted.manifest, studioTenant, revision.revision + 1)
    setManifest(accepted.manifest)
    setRevision(nextRevision)
    setPastManifests(remaining)
    setFutureManifests(future)
    saveGeneratedRevision(nextRevision)
  }

  const undo = () => {
    const previous = pastManifests.at(-1)
    if (!previous) return
    restoreManifest(previous, pastManifests.slice(0, -1), [manifest, ...futureManifests])
  }

  const redo = () => {
    const next = futureManifests[0]
    if (!next) return
    restoreManifest(next, [...pastManifests, manifest], futureManifests.slice(1))
  }

  const updateComponent = (componentId: string, updater: (component: PageComponent) => PageComponent) => {
    const next: PageManifest = {
      ...manifest,
      spec: {
        ...manifest.spec,
        layout: {
          ...manifest.spec.layout,
          children: updateComponents(manifest.spec.layout.children, componentId, updater),
        },
      },
    }
    syncSource(next)
  }

  const addComponent = (componentType: string) => {
    const addedManifest = addManifestComponent(manifest, componentType)
    const addedId = addedManifest.spec.layout.children.at(-1)?.id ?? null
    const next = addedId
      ? { ...addedManifest, spec: { ...addedManifest.spec, layout: { ...addedManifest.spec.layout, children: updateComponents(addedManifest.spec.layout.children, addedId, (component) => ({
          ...component,
          data: ['search_box', 'filter_bar'].includes(component.component) ? undefined : 'primary.data',
          props: { ...component.props, ...(sourceDoctype !== 'Kora resource' ? { source_doctype: sourceDoctype } : {}) },
        })) } } }
      : addedManifest
    setSelectedId(addedId)
    syncSource(next)
  }

  const publishDraft = () => {
    setPublishedRevision(revision.revision)
    const published = { ...revision, state: 'published' as const, updatedAt: new Date().toISOString() }
    setRevision(published)
    saveGeneratedRevision(published)
  }

  const removeSelectedComponent = () => {
    if (!selectedId) return
    setConfirmAction('remove')
  }

  const confirmRemove = () => {
    if (!selectedId) return
    const id = selectedId
    setSelectedId(null)
    setConfirmAction(null)
    syncSource({
      ...manifest,
      spec: { ...manifest.spec, layout: { ...manifest.spec.layout, children: removeComponent(manifest.spec.layout.children, id) } },
    })
  }

  const duplicateSelectedComponent = () => {
    if (!selectedId) return
    const result = duplicateComponent(manifest.spec.layout.children, selectedId)
    if (!result.copyId) return
    setSelectedId(result.copyId)
    syncSource({
      ...manifest,
      spec: { ...manifest.spec, layout: { ...manifest.spec.layout, children: result.components } },
    })
  }

  const applyAiEdit = () => {
    const prompt = aiPrompt.trim()
    if (!selected || !prompt) return
    const titleMatch = prompt.match(/(?:call it|rename it to|title(?: it)? to)\s+["']?([^"']+?)["']?$/i)
    const nextTitle = titleMatch?.[1]?.trim() || prompt.match(/make the title\s+["']?([^"']+?)["']?$/i)?.[1]?.trim()
    let next: PageManifest | null = null
    if (nextTitle) next = { ...manifest, spec: { ...manifest.spec, layout: { ...manifest.spec.layout, children: updateComponents(manifest.spec.layout.children, selected.id, (component) => ({ ...component, props: { ...component.props, title: nextTitle } })) } } }
    if (/compact|smaller|dense/i.test(prompt)) {
      next = { ...manifest, spec: { ...manifest.spec, layout: { ...manifest.spec.layout, children: updateComponents(manifest.spec.layout.children, selected.id, (component) => ({ ...component, props: { ...component.props, bindings: { ...(component.props.bindings as Record<string, string> || {}), density: 'compact' } } })) } } }
    }
    if (/large|bigger|prominent/i.test(prompt)) {
      next = { ...manifest, spec: { ...manifest.spec, layout: { ...manifest.spec.layout, children: updateComponents(manifest.spec.layout.children, selected.id, (component) => ({ ...component, props: { ...component.props, bindings: { ...(component.props.bindings as Record<string, string> || {}), density: 'comfortable' } } })) } } }
    }
    if (next) setProposedManifest(next)
  }

  const applyProposal = () => {
    if (!proposedManifest) return
    syncSource(proposedManifest)
    setProposedManifest(null)
  }

  return (
    <div className={`dark uk-theme-${studioTenant.theme} kora-brand-dark kora-franken-theme flex min-h-full flex-col bg-[#171313] text-foreground`}>
      <div className="kora-studio-toolbar border-b border-[#3a2c2a] bg-[#1d1717]/95 px-5 py-3 backdrop-blur">
        <div className="flex items-center gap-3">
              <div className="flex items-center gap-2">
            <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-white text-slate-900">
                  <PanelsTopLeft className="h-4 w-4" />
                </div>
                <div className="leading-tight">
              <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-slate-400">Kora Studio</div>
              <div className="text-sm font-semibold text-white">Kora Insights</div>
                </div>
              </div>
          <div className="kora-studio-actions ml-auto flex min-w-0 items-center gap-2">
            <Badge className="kora-studio-status" variant={issues.length ? 'destructive' : 'outline'}>{issues.length ? `${issues.length} issues` : `${revision.state} r${revision.revision}`}</Badge>
            {publishedRevision && <Badge className="kora-studio-status" variant="secondary">Published r{publishedRevision}</Badge>}
            <Button className="kora-studio-history" variant="ghost" size="sm" onClick={undo} disabled={!pastManifests.length} aria-label="Undo last change">Undo</Button>
            <Button className="kora-studio-history" variant="ghost" size="sm" onClick={redo} disabled={!futureManifests.length} aria-label="Redo last change">Redo</Button>
            <Button variant="secondary" size="sm" onClick={publishDraft} disabled={issues.length > 0 || revision.state === 'published'}>Publish</Button>
            <Button variant="secondary" size="sm" onClick={() => setConfirmAction('reset')}>Reset</Button>
          </div>
        </div>
      </div>

      <div className="border-b border-[#3a2c2a] bg-[#1d1717] px-5 py-3">
        <div className="mx-auto flex max-w-[1500px] items-center gap-3 rounded-xl border border-[#3a2c2a] bg-[#241b1b] px-3 py-2 shadow-[0_8px_30px_rgba(0,0,0,0.16)]">
          <Sparkles className="h-4 w-4 text-amber-600" />
          <input
            value={aiPrompt}
            onChange={(event) => setAiPrompt(event.target.value)}
            className="h-8 flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
            placeholder="Describe the screen you want..."
          />
          <Button size="sm" onClick={applyAiEdit}><WandSparkles className="mr-2 h-3.5 w-3.5" />Ask AI</Button>
        </div>
      </div>

      <div className="flex items-center justify-center gap-1 border-b border-[#3a2c2a] bg-[#1d1717] px-4 py-2">
        {([['desktop', Monitor, 'Desktop'], ['tablet', Tablet, 'Tablet'], ['mobile', Smartphone, 'Mobile']] as const).map(([key, Icon, label]) => (
          <Button key={key} variant={viewport === key ? 'secondary' : 'ghost'} size="sm" onClick={() => setViewport(key)} aria-pressed={viewport === key}>
            <Icon className="h-3.5 w-3.5" /><span className="kora-viewport-label">{label}</span>
          </Button>
        ))}
      </div>

      <div className="grid flex-1 gap-0 lg:grid-cols-[220px_minmax(0,1fr)_260px]">
        <aside className="kora-studio-insert border-r border-[#3a2c2a] bg-[#1d1717] p-4">
          <div className="space-y-3">
            <div className="px-1 text-[10px] font-semibold uppercase tracking-[0.18em] text-slate-500">Ask a question</div>
            <Card className="border-white/[0.1] bg-[#151a24] shadow-[0_10px_30px_rgba(0,0,0,0.12)]">
              <CardHeader className="px-3 pb-2 pt-3">
                <CardTitle className="flex items-center gap-2 text-sm text-foreground">
                  <Palette className="h-4 w-4" /> Starter questions
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-1.5 px-3 pb-3">
                {starterQuestions.map((question) => (
                  <button
                    key={question}
                    type="button"
                    onClick={() => runQuestion(question)}
                    className="flex w-full items-center justify-between rounded-lg border border-white/10 bg-[#0b1220] px-2.5 py-1.5 text-left text-sm hover:bg-[#111827]"
                  >
                    <span className="text-foreground">{question}</span>
                  </button>
                ))}
              </CardContent>
            </Card>

            <Card className="border-white/[0.1] bg-[#241b1b] shadow-[0_10px_30px_rgba(0,0,0,0.12)]">
              <CardHeader className="px-3 pb-2 pt-3"><CardTitle className="text-sm text-foreground">Query review</CardTitle></CardHeader>
              <CardContent className="space-y-2 px-3 pb-3 text-xs text-slate-300">
                <div className="flex justify-between"><span>Resource</span><strong className="text-slate-100">{activeQuery.resource}</strong></div>
                <div className="flex justify-between"><span>Filters</span><strong className="text-slate-100">{activeQuery.filters.length || 'None'}</strong></div>
                <div className="flex justify-between"><span>Grouping</span><strong className="text-slate-100">{activeQuery.groupBy.join(', ') || 'None'}</strong></div>
                <p className="border-t border-white/10 pt-2 text-slate-400">This answer uses approved fields from the connected resource.</p>
              </CardContent>
            </Card>

          </div>
        </aside>

        <main className="min-w-0 bg-[#171313] p-3 lg:p-5">
          <div className={`mx-auto transition-[max-width] ${viewport === 'mobile' ? 'max-w-[390px]' : viewport === 'tablet' ? 'max-w-[768px]' : 'max-w-6xl'}`}>
            <div className="overflow-hidden rounded-2xl border border-[#3a2c2a] bg-[#241b1b] p-2.5 shadow-[0_20px_60px_rgba(0,0,0,0.24)]">
              <div className="rounded-xl border border-[#3a2c2a] bg-[#1d1717] p-3">
                <div className="mb-3 flex flex-wrap items-center justify-between gap-2 border-b border-slate-200 pb-3 text-xs text-slate-500">
                  <span><strong className="text-slate-200">Answering:</strong> {aiPrompt}</span>
                  <span className="text-slate-400">{queryAnswer.total} matching records · {sourceDoctype} · {resourceState.primary?.kind === 'normal' ? 'Kora live data' : 'waiting for Kora data'}</span>
                </div>
                <div onClick={() => setSelectedId(null)}>
                  <div className="kora-generated-ui dark uk-theme-zinc text-slate-100">
                    <ManifestRenderer
                      manifest={manifest}
                      mode="runtime"
                      resourceState={resourceState}
                      selectedComponentId={selectedId}
                      onSelectComponent={setSelectedId}
                      onDuplicateComponent={duplicateSelectedComponent}
                      onRemoveComponent={() => removeSelectedComponent()}
                    />
                  </div>
                </div>
              </div>
            </div>

            {proposedManifest && (
              <Card className="mt-4 border-amber-400/30 bg-amber-500/10">
                <CardContent className="flex items-center gap-3 px-4 py-3">
                  <WandSparkles className="h-4 w-4 text-amber-300" />
                  <div className="flex-1 text-sm text-slate-200">AI proposed a change to <strong>{selected?.id}</strong>. Review it on the canvas before applying.</div>
                  <Button size="sm" onClick={applyProposal}>Apply</Button>
                  <Button size="sm" variant="ghost" onClick={() => setProposedManifest(null)}>Reject</Button>
                </CardContent>
              </Card>
            )}

            {issues.length > 0 && (
              <Card className="mt-4 border-amber-400/20 bg-amber-500/10">
                <CardHeader className="pb-3">
                  <CardTitle className="text-sm">Validation</CardTitle>
                </CardHeader>
                <CardContent className="space-y-2">
                  {issues.map((issue) => (
                    <div key={`${issue.path}:${issue.message}`} className="rounded-xl border border-amber-400/20 bg-[#0b1220] px-3 py-2 text-sm shadow-sm">
                      <div className="font-mono text-[11px] text-amber-300">{issue.path}</div>
                      <div className="text-slate-300">{issue.message}</div>
                    </div>
                  ))}
                </CardContent>
              </Card>
            )}
          </div>
        </main>

        <aside className={`kora-studio-inspector border-l border-[#3a2c2a] bg-[#1d1717] p-4 ${selected ? 'kora-studio-inspector-open' : ''}`}>
          <div className="space-y-3">
            <div className="flex items-center justify-between px-1">
              <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-slate-500">Properties</div>
              {selected && <Button className="kora-inspector-close" variant="ghost" size="icon" aria-label="Close inspector" onClick={() => setSelectedId(null)}><X className="h-4 w-4" aria-hidden="true" /></Button>}
            </div>
            <Card className="border-white/[0.1] bg-[#151a24] shadow-[0_10px_30px_rgba(0,0,0,0.12)]">
              <CardHeader className="px-3 pb-2 pt-3">
                  <CardTitle className="flex items-center gap-2 text-sm text-foreground">
                  <PencilLine className="h-4 w-4" /> Inspector
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-3 px-3 pb-3">
                {selected ? (
                  <>
                    <div className="rounded-lg border border-white/10 bg-[#0b1220] p-2.5">
                      <div className="font-medium text-slate-100">{selected.component}</div>
                      <div className="font-mono text-xs text-slate-400">{selected.id}</div>
                    </div>
                    <label className="block space-y-2 text-sm">
                      <span className="text-xs uppercase tracking-wide text-muted-foreground">Title</span>
                      <input
                        className="w-full rounded-xl border border-white/10 bg-[#0b1220] px-3 py-2 text-sm text-slate-100 outline-none focus:ring-2 focus:ring-amber-200"
                        value={String(selected.props.title || '')}
                        onChange={(event) => updateComponent(selected.id, (component) => ({
                          ...component,
                          props: {
                            ...component.props,
                            title: event.target.value,
                          },
                        }))}
                      />
                    </label>
                    <details className="rounded-lg border border-white/10 bg-[#0b1220] p-2.5">
                      <summary className="cursor-pointer text-xs font-medium text-slate-300 focus-visible:outline focus-visible:outline-2 focus-visible:outline-orange-400">Advanced properties</summary>
                      <dl className="mt-3 space-y-2 text-xs text-slate-400">
                        <div className="flex justify-between gap-3"><dt>Component type</dt><dd className="font-mono text-slate-200">{selected.component}</dd></div>
                        <div className="flex justify-between gap-3"><dt>Layer ID</dt><dd className="font-mono text-slate-200">{selected.id}</dd></div>
                        <div className="flex justify-between gap-3"><dt>Data binding</dt><dd className="font-mono text-right text-slate-200">{selected.data || 'None'}</dd></div>
                      </dl>
                    </details>
                    <div className="grid grid-cols-2 gap-2 border-t border-white/10 pt-3">
                    <Button
                      variant="secondary"
                      className="w-full"
                      onClick={duplicateSelectedComponent}
                    >
                      Duplicate
                    </Button>
                    <Button
                      variant="destructive"
                      className="w-full"
                      onClick={removeSelectedComponent}
                    >
                      Remove component
                    </Button>
                    </div>
                  </>
                ) : (
                  <p className="text-sm text-muted-foreground">Select a block on the canvas to edit its properties.</p>
                )}
              </CardContent>
            </Card>

            <Card className="border-white/[0.1] bg-[#151a24] shadow-[0_10px_30px_rgba(0,0,0,0.12)]">
              <CardHeader className="px-3 pb-2 pt-3">
                <CardTitle className="flex items-center gap-2 text-sm text-foreground">
                  <Code2 className="h-4 w-4" /> Code
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-2 px-3 pb-3">
                <Button variant="outline" className="w-full justify-start" onClick={() => setCodeOpen((open) => !open)}>
                  {codeOpen ? 'Hide source' : 'Show source'}
                </Button>
                {codeOpen && (
                  <Textarea
                    value={JSON.stringify(manifest, null, 2)}
                    readOnly
                    className="min-h-[300px] font-mono text-xs"
                  />
                )}
              </CardContent>
            </Card>
          </div>
        </aside>
      </div>
      <ConfirmDialog
        open={confirmAction !== null}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={confirmAction === 'reset' ? 'Reset this design?' : 'Remove this component?'}
        description={confirmAction === 'reset' ? 'This will replace the current draft with the starting layout. Your saved revision history remains available.' : 'This removes the selected block from the current draft. You can restore it with Undo.'}
        confirmLabel={confirmAction === 'reset' ? 'Reset design' : 'Remove component'}
        onConfirm={() => confirmAction === 'reset' ? (setConfirmAction(null), syncSource(starterManifest())) : confirmRemove()}
      />
    </div>
  )
}
