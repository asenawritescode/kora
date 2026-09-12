import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { createPageManifest, fetchPageManifestByName, updatePageManifest } from '@/lib/api/page-manifests'
import { fetchDoctypes } from '@/lib/api/system'
import type { DocType } from '@/types/kora'
import {
  PAGE_COMPONENT_LIBRARY,
  createBlankPageManifest,
  normalizePageManifest,
  removeManifestComponent,
  validatePageManifestContract,
  type OfflinePolicy,
  type PageComponent,
  type PageLayoutType,
  type PageManifest,
  type PageResource,
  type PageAction,
} from '@/manifest/schema/page'
import {
  buildPublishPreflight,
  buildResourceBindingOptions,
  clearManifestDraft,
  readManifestDraft,
  previewViewportClass,
  previewViewportOptions,
  writeManifestDraft,
} from './editor-helpers'
import {
  addBoundComponent,
  componentAvailability,
  getPrimaryDoctypeName,
  moveManifestComponent,
  withDoctypeDefaults,
} from './editor-builders'
import {
  ManifestRenderer,
  createSimulatedResourceState,
  type ResourceSimulationKind,
} from '@/manifest/runtime/ManifestRenderer'
import {
  STANDARD_PAGE_KINDS,
  bindComponentToPrimaryResource,
  createStandardPageManifest,
  selectListFields,
  type StandardPageKind,
} from '@/manifest/runtime/standard-pages'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/components/ui/Toast'
import { cn } from '@/lib/utils'
import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  Boxes,
  ChevronRight,
  CheckCircle2,
  Copy,
  Database,
  FileJson2,
  GitBranch,
  Keyboard,
  Layers3,
  Plus,
  Redo2,
  Save,
  Search,
  ShieldCheck,
  Trash2,
  Undo2,
  WifiOff,
} from 'lucide-react'

const LAYOUTS: PageLayoutType[] = ['single', 'two_panel', 'three_panel', 'grid']
const OFFLINE_POLICIES: OfflinePolicy[] = ['unsupported', 'read_only', 'queue_writes', 'full_slice']
const RESOURCE_STATES: ResourceSimulationKind[] = ['normal', 'loading', 'empty', 'error', 'permission_denied', 'offline', 'conflict', 'stale']
type WorkbenchMode = 'design' | 'preview' | 'source'

export default function PageManifestWorkbench() {
  const params = useParams({ strict: false }) as { name?: string }
  const name = params.name
  const isNew = !name
  const navigate = useNavigate()
  const [manifest, setManifest] = useState<PageManifest>(() => createBlankPageManifest())
  const [selectedComponentId, setSelectedComponentId] = useState<string | null>(null)
  const [sourceDraft, setSourceDraft] = useState('')
  const [sourceError, setSourceError] = useState<string | null>(null)
  const [undoStack, setUndoStack] = useState<PageManifest[]>([])
  const [redoStack, setRedoStack] = useState<PageManifest[]>([])
  const [mode, setMode] = useState<WorkbenchMode>('design')
  const [resourceState, setResourceState] = useState<ResourceSimulationKind>('normal')
  const [previewViewport, setPreviewViewport] = useState<'desktop' | 'tablet' | 'mobile'>('desktop')
  const [saving, setSaving] = useState(false)
  const [draftStatus, setDraftStatus] = useState<'idle' | 'restored' | 'saved' | 'error'>('idle')

  const { data: existingManifest, isLoading, error } = useQuery({
    queryKey: ['page-manifest', name],
    queryFn: () => fetchPageManifestByName(name!),
    enabled: !!name,
  })

  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
    staleTime: 60_000,
  })

  useEffect(() => {
    if (existingManifest) {
      const next = normalizePageManifest(existingManifest)
      const restored = readManifestDraft(name)
      if (restored) {
        const restoredManifest = normalizePageManifest(restored.manifest)
        setManifest(restoredManifest)
        setSourceDraft(JSON.stringify(restoredManifest, null, 2))
        setDraftStatus('restored')
      } else {
        setManifest(next)
        setSourceDraft(JSON.stringify(next, null, 2))
      }
      setUndoStack([])
      setRedoStack([])
      setSelectedComponentId(null)
      return
    }
    if (isNew) {
      const next = createBlankPageManifest()
      setManifest(next)
      setSourceDraft(JSON.stringify(next, null, 2))
      setUndoStack([])
      setRedoStack([])
      setSelectedComponentId(null)
    }
  }, [existingManifest, isNew])

  useEffect(() => {
    setSourceDraft(JSON.stringify(manifest, null, 2))
  }, [manifest])

  useEffect(() => {
    const nextDraft = {
      manifest: normalizePageManifest(manifest),
      savedAt: new Date().toISOString(),
      source: mode === 'source' ? 'source' : 'editor',
    } as const
    const timeout = window.setTimeout(() => {
      writeManifestDraft(name, nextDraft)
      setDraftStatus('saved')
    }, 300)
    return () => window.clearTimeout(timeout)
  }, [manifest, mode, name])

  const selectedComponent = useMemo(
    () => manifest.spec.layout.children.find((component) => component.id === selectedComponentId) ?? null,
    [manifest.spec.layout.children, selectedComponentId],
  )
  const issues = useMemo(() => validatePageManifestContract(manifest), [manifest])
  const publishPreflight = useMemo(() => buildPublishPreflight(manifest), [manifest])
  const resourceBindingOptions = useMemo(() => buildResourceBindingOptions(manifest), [manifest])
  const primaryDoctypeName = getPrimaryDoctypeName(manifest)
  const primaryDoctype = useMemo(
    () => doctypes?.find((doctype) => doctype.name === primaryDoctypeName) ?? null,
    [doctypes, primaryDoctypeName],
  )

  const updateManifest = (updater: (current: PageManifest) => PageManifest) => {
    setManifest((current) => {
      const next = updater(current)
      if (JSON.stringify(next) === JSON.stringify(current)) return current
      setUndoStack((stack) => [...stack.slice(-29), cloneManifest(current)])
      setRedoStack([])
      return next
    })
    setSourceError(null)
  }

  const undo = () => {
    setUndoStack((stack) => {
      if (stack.length === 0) return stack
      const previous = stack[stack.length - 1]
      setRedoStack((redo) => [...redo.slice(-29), cloneManifest(manifest)])
      setManifest(previous)
      setSelectedComponentId(null)
      return stack.slice(0, -1)
    })
  }

  const redo = () => {
    setRedoStack((stack) => {
      if (stack.length === 0) return stack
      const next = stack[stack.length - 1]
      setUndoStack((undo) => [...undo.slice(-29), cloneManifest(manifest)])
      setManifest(next)
      setSelectedComponentId(null)
      return stack.slice(0, -1)
    })
  }

  const removeSelectedComponent = () => {
    if (!selectedComponentId) return
    updateManifest((current) => removeManifestComponent(current, selectedComponentId))
    setSelectedComponentId(null)
  }

  const duplicateSelectedComponent = () => {
    if (!selectedComponent) return
    const duplicate = duplicateComponent(selectedComponent, manifest.spec.layout.children.length)
    updateManifest((current) => ({
      ...current,
      spec: {
        ...current.spec,
        layout: {
          ...current.spec.layout,
          children: [...current.spec.layout.children, duplicate],
        },
      },
    }))
    setSelectedComponentId(duplicate.id)
  }

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const key = event.key.toLowerCase()
      const mod = event.metaKey || event.ctrlKey
      const editable = isEditableTarget(event.target)

      if (mod && key === 's') {
        event.preventDefault()
        void saveDraft()
        return
      }
      if (editable) return
      if (key === 'escape') {
        setSelectedComponentId(null)
        return
      }
      if ((event.key === 'Backspace' || event.key === 'Delete') && selectedComponentId) {
        event.preventDefault()
        removeSelectedComponent()
        return
      }
      if (mod && key === 'd' && selectedComponent) {
        event.preventDefault()
        duplicateSelectedComponent()
        return
      }
      if (mod && key === 'z' && event.shiftKey) {
        event.preventDefault()
        redo()
        return
      }
      if (mod && key === 'z') {
        event.preventDefault()
        undo()
      }
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [manifest, selectedComponent, selectedComponentId, undoStack.length, redoStack.length])

  const saveDraft = async () => {
    const currentIssues = validatePageManifestContract(manifest)
    if (currentIssues.length > 0 || !publishPreflight.canPublish) {
      toast('error', 'Fix manifest validation issues before saving.')
      return
    }

    setSaving(true)
    try {
      if (isNew) {
        await createPageManifest(normalizePageManifest(manifest))
        toast('success', 'Draft page manifest saved.')
        navigate({ to: '/workspace/admin/page-manifests/$name', params: { name: manifest.metadata.name } })
      } else {
        await updatePageManifest(name!, normalizePageManifest(manifest))
        toast('success', 'Draft page manifest saved.')
      }
      clearManifestDraft(name)
      setDraftStatus('saved')
    } catch (err) {
      setDraftStatus('error')
      toast('error', err instanceof Error ? err.message : 'Failed to save manifest.')
    } finally {
      setSaving(false)
    }
  }

  const applySource = () => {
    try {
      const parsed = JSON.parse(sourceDraft) as PageManifest
      const issues = validatePageManifestContract(parsed)
      if (issues.length > 0) {
        setSourceError(issues[0].message)
        return
      }
      setManifest(normalizePageManifest(parsed))
      setSourceError(null)
    } catch (err) {
      setSourceError(err instanceof Error ? err.message : 'Invalid JSON.')
    }
  }

  if (isLoading && !isNew) {
    return <div className="p-8 text-sm text-muted-foreground">Loading page manifest...</div>
  }

  if (error && !isNew) {
    return (
      <div className="p-8">
        <Card className="border-destructive/40">
          <CardHeader>
            <CardTitle>Could not load page manifest</CardTitle>
            <CardDescription>{error instanceof Error ? error.message : 'The server returned an error.'}</CardDescription>
          </CardHeader>
        </Card>
      </div>
    )
  }

  return (
    <div className="flex h-[calc(100vh-3rem)] flex-col bg-muted/20">
      <header className="flex shrink-0 flex-wrap items-center gap-3 border-b bg-background px-4 py-3">
        <div className="min-w-0">
          <p className="text-[11px] font-semibold uppercase tracking-[0.18em] text-muted-foreground">Page Manifest Workbench</p>
          <h1 className="truncate text-lg font-semibold">{manifest.metadata.name || 'New screen'}</h1>
        </div>
        <Badge variant="secondary">{manifest.apiVersion}</Badge>
        <Badge variant={issues.length ? 'destructive' : 'default'} className="gap-1">
          {issues.length ? <AlertTriangle className="h-3.5 w-3.5" /> : <CheckCircle2 className="h-3.5 w-3.5" />}
          {issues.length ? `${issues.length} issue${issues.length === 1 ? '' : 's'}` : 'Valid'}
        </Badge>
        <Badge variant="outline" className="gap-1">
          <WifiOff className="h-3.5 w-3.5" />
          {manifest.spec.offline}
        </Badge>
        <Badge variant={draftStatus === 'error' ? 'destructive' : draftStatus === 'restored' ? 'secondary' : 'outline'}>
          Draft {draftStatus}
        </Badge>
        <div className="flex items-center gap-1 rounded-lg border bg-muted/20 p-0.5">
          {previewViewportOptions().map((option) => (
            <Button
              key={option.value}
              type="button"
              variant={previewViewport === option.value ? 'secondary' : 'ghost'}
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={() => setPreviewViewport(option.value as 'desktop' | 'tablet' | 'mobile')}
            >
              {option.label}
            </Button>
          ))}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <div className="flex items-center rounded-lg border bg-muted/20 p-0.5" aria-label="Workbench mode">
            {(['design', 'preview', 'source'] as const).map((nextMode) => (
              <Button
                key={nextMode}
                type="button"
                variant={mode === nextMode ? 'secondary' : 'ghost'}
                size="sm"
                className="h-7 px-2 text-xs capitalize"
                onClick={() => setMode(nextMode)}
              >
                {nextMode}
              </Button>
            ))}
          </div>
          <Button variant="ghost" size="sm" disabled={undoStack.length === 0} onClick={undo} title="Undo">
            <Undo2 className="mr-2 h-4 w-4" />
            Undo
          </Button>
          <Button variant="ghost" size="sm" disabled={redoStack.length === 0} onClick={redo} title="Redo">
            <Redo2 className="mr-2 h-4 w-4" />
            Redo
          </Button>
          <Button size="sm" disabled={saving} onClick={saveDraft}>
            <Save className="mr-2 h-4 w-4" />
            {saving ? 'Saving...' : 'Save draft'}
          </Button>
        </div>
      </header>

      <main className="grid min-h-0 flex-1 grid-cols-1 overflow-hidden lg:grid-cols-[280px_minmax(0,1fr)_360px]">
        <aside className="min-h-0 overflow-y-auto border-r bg-background">
          <StandardPagePanel
            manifest={manifest}
            doctypes={doctypes ?? []}
            primaryDoctype={primaryDoctype}
            updateManifest={updateManifest}
            onSelectComponent={setSelectedComponentId}
          />
          <ManifestSettings manifest={manifest} updateManifest={updateManifest} />
          <LayersPanel
            manifest={manifest}
            selectedComponentId={selectedComponentId}
            onSelect={setSelectedComponentId}
            onMove={(componentID, targetIndex) => updateManifest((current) => moveManifestComponent(current, componentID, targetIndex))}
            onDuplicate={(component) => {
              const duplicate = duplicateComponent(component, manifest.spec.layout.children.length)
              updateManifest((current) => ({
                ...current,
                spec: {
                  ...current.spec,
                  layout: {
                    ...current.spec.layout,
                    children: [...current.spec.layout.children, duplicate],
                  },
                },
              }))
              setSelectedComponentId(duplicate.id)
            }}
            onRemove={(id) => {
              updateManifest((current) => removeManifestComponent(current, id))
              if (selectedComponentId === id) setSelectedComponentId(null)
            }}
          />
          <ValidationSummary issues={issues} />
          <PublishPreflightPanel preflight={publishPreflight} />
          <ComponentPalette
            primaryDoctype={primaryDoctype}
            onAdd={(component) => {
              updateManifest((current) => addBoundComponent(current, component, primaryDoctype))
            }}
          />
        </aside>

        <section className="min-h-0 overflow-y-auto p-4">
          {mode === 'source' ? (
            <SourcePanel sourceDraft={sourceDraft} sourceError={sourceError} setSourceDraft={setSourceDraft} applySource={applySource} large />
          ) : (
            <ManifestCanvas
              manifest={manifest}
              mode={mode}
              viewport={previewViewport}
              resourceState={resourceState}
              selectedComponentId={selectedComponentId}
              onSelect={setSelectedComponentId}
              onDuplicate={(component) => {
                const duplicate = duplicateComponent(component, manifest.spec.layout.children.length)
                updateManifest((current) => ({
                  ...current,
                  spec: {
                    ...current.spec,
                    layout: {
                      ...current.spec.layout,
                      children: [...current.spec.layout.children, duplicate],
                    },
                  },
                }))
                setSelectedComponentId(duplicate.id)
              }}
              onRemove={(id) => {
                updateManifest((current) => removeManifestComponent(current, id))
                if (selectedComponentId === id) setSelectedComponentId(null)
              }}
            />
          )}
        </section>

        <aside className="min-h-0 overflow-y-auto border-l bg-background">
          <PreviewStatePanel value={resourceState} onChange={setResourceState} disabled={mode === 'source'} />
          <ResourceActionPanel manifest={manifest} updateManifest={updateManifest} />
          <ShortcutPanel />
          <ComponentInspector
            component={selectedComponent}
            resourceOptions={resourceBindingOptions}
            updateComponent={(patch) => updateManifest((current) => ({
              ...current,
              spec: {
                ...current.spec,
                layout: {
                  ...current.spec.layout,
                  children: current.spec.layout.children.map((component) =>
                    component.id === selectedComponentId ? { ...component, ...patch } : component,
                  ),
                },
              },
            }))}
          />
          {mode !== 'source' && <SourcePanel sourceDraft={sourceDraft} sourceError={sourceError} setSourceDraft={setSourceDraft} applySource={applySource} />}
        </aside>
      </main>
    </div>
  )
}

function ManifestSettings({
  manifest,
  updateManifest,
}: {
  manifest: PageManifest
  updateManifest: (updater: (current: PageManifest) => PageManifest) => void
}) {
  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={FileJson2} title="Manifest" />
      <TextField label="Name" value={manifest.metadata.name} onChange={(name) => updateManifest((current) => ({
        ...current,
        metadata: { ...current.metadata, name },
      }))} />
      <TextField label="Package" value={manifest.metadata.package} onChange={(pkg) => updateManifest((current) => ({
        ...current,
        metadata: { ...current.metadata, package: pkg },
      }))} />
      <TextField label="Route" value={manifest.spec.route} onChange={(route) => updateManifest((current) => ({
        ...current,
        spec: { ...current.spec, route },
      }))} />
      <div className="grid grid-cols-2 gap-2">
        <SelectField label="Layout" value={manifest.spec.layout.type} options={LAYOUTS} onChange={(layout) => updateManifest((current) => ({
          ...current,
          spec: { ...current.spec, layout: { ...current.spec.layout, type: layout as PageLayoutType } },
        }))} />
        <SelectField label="Offline" value={manifest.spec.offline} options={OFFLINE_POLICIES} onChange={(offline) => updateManifest((current) => ({
          ...current,
          spec: { ...current.spec, offline: offline as OfflinePolicy },
        }))} />
      </div>
      <TokenField
        label="Capabilities"
        value={manifest.spec.capabilities}
        onChange={(capabilities) => updateManifest((current) => ({ ...current, spec: { ...current.spec, capabilities } }))}
      />
      <TokenField
        label="Permissions"
        value={manifest.spec.permissions}
        onChange={(permissions) => updateManifest((current) => ({ ...current, spec: { ...current.spec, permissions } }))}
      />
    </div>
  )
}

function ValidationSummary({ issues }: { issues: ReturnType<typeof validatePageManifestContract> }) {
  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={ShieldCheck} title="Preflight" />
      {issues.length === 0 ? (
        <p className="rounded-lg border bg-muted/30 p-3 text-xs text-muted-foreground">Schema, lifecycle, resource, action, and component references are valid.</p>
      ) : (
        <div className="space-y-2">
          {issues.slice(0, 6).map((issue) => (
            <div key={`${issue.path}:${issue.message}`} className="rounded-lg border border-destructive/30 bg-destructive/5 p-2 text-xs">
              <p className="font-mono text-[10px] text-destructive">{issue.path}</p>
              <p>{issue.message}</p>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function StandardPagePanel({
  manifest,
  doctypes,
  primaryDoctype,
  updateManifest,
  onSelectComponent,
}: {
  manifest: PageManifest
  doctypes: DocType[]
  primaryDoctype: DocType | null
  updateManifest: (updater: (current: PageManifest) => PageManifest) => void
  onSelectComponent: (id: string | null) => void
}) {
  const [selectedDoctype, setSelectedDoctype] = useState(primaryDoctype?.name || '')

  useEffect(() => {
    if (primaryDoctype?.name) setSelectedDoctype(primaryDoctype.name)
  }, [primaryDoctype?.name])

  const doctype = doctypes.find((entry) => entry.name === selectedDoctype) ?? primaryDoctype
  const fields = doctype ? selectListFields(doctype) : []
  const hasDataContract = !!getPrimaryDoctypeName(manifest)

  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={ShieldCheck} title="Screen pipeline" />
      <p className="text-xs text-muted-foreground">
        Start from a standard doctype screen, then customize safely. Components added after this are bound to the selected data source.
      </p>
      <div className="space-y-1.5">
        <Label className="text-xs">Data source</Label>
        <Select value={selectedDoctype} onValueChange={(value) => { if (value) setSelectedDoctype(value) }}>
          <SelectTrigger className="h-8 text-xs">
            <SelectValue placeholder="Choose a doctype..." />
          </SelectTrigger>
          <SelectContent>
            {doctypes.filter((entry) => !entry.is_child_table).map((entry) => (
              <SelectItem key={entry.name} value={entry.name}>{entry.name}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {doctype ? (
        <>
          <div className="grid grid-cols-2 gap-2">
            {STANDARD_PAGE_KINDS.map((preset) => (
              <Button
                key={preset.kind}
                type="button"
                variant="outline"
                size="sm"
                className="h-auto justify-start whitespace-normal py-2 text-left text-xs"
                onClick={() => {
                  const next = createStandardPageManifest(doctype, preset.kind)
                  updateManifest(() => next)
                  onSelectComponent(next.spec.layout.children[0]?.id ?? null)
                }}
                title={preset.description}
              >
                {preset.label}
              </Button>
            ))}
          </div>
          <div className="rounded-lg border bg-muted/20 p-2 text-xs">
            <p className="font-medium">{hasDataContract ? 'Data contract ready' : 'Choose a standard screen to create the data contract'}</p>
            <p className="mt-1 text-muted-foreground">Resource: <span className="font-mono">primary</span> {'->'} {doctype.name}</p>
            {fields.length > 0 && (
              <p className="mt-1 text-muted-foreground">Usable fields: {fields.slice(0, 5).join(', ')}{fields.length > 5 ? '...' : ''}</p>
            )}
          </div>
        </>
      ) : (
        <div className="rounded-lg border border-dashed p-3 text-xs text-muted-foreground">
          Create or activate a doctype first, then generate a standard screen from it.
        </div>
      )}
    </div>
  )
}

function LayersPanel({
  manifest,
  selectedComponentId,
  onSelect,
  onMove,
  onDuplicate,
  onRemove,
}: {
  manifest: PageManifest
  selectedComponentId: string | null
  onSelect: (id: string) => void
  onMove: (id: string, targetIndex: number) => void
  onDuplicate: (component: PageComponent) => void
  onRemove: (id: string) => void
}) {
  const components = manifest.spec.layout.children

  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={Layers3} title="Layers" />
      {components.length === 0 ? (
        <p className="rounded-lg border border-dashed p-3 text-xs text-muted-foreground">
          Components you add appear here as an ordered layer list.
        </p>
      ) : (
        <div className="space-y-1">
          {components.map((component, index) => {
            const selected = selectedComponentId === component.id
            return (
              <div
                key={component.id}
                draggable
                onDragStart={(event) => {
                  event.dataTransfer.effectAllowed = 'move'
                  event.dataTransfer.setData('text/kora-component', component.id)
                }}
                onDragOver={(event) => {
                  event.preventDefault()
                  event.dataTransfer.dropEffect = 'move'
                }}
                onDrop={(event) => {
                  event.preventDefault()
                  const draggedID = event.dataTransfer.getData('text/kora-component')
                  if (draggedID && draggedID !== component.id) onMove(draggedID, index)
                }}
                className={cn(
                  'group flex items-center gap-2 rounded-lg border px-2 py-2 text-xs transition-colors',
                  selected ? 'border-primary bg-primary/5' : 'bg-card hover:bg-muted/50',
                )}
              >
                <button
                  type="button"
                  className="flex min-w-0 flex-1 items-center gap-2 text-left"
                  onClick={() => onSelect(component.id)}
                >
                  <ChevronRight className={cn('h-3.5 w-3.5 shrink-0 text-muted-foreground', selected && 'text-primary')} />
                  <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">{index + 1}</span>
                  <span className="min-w-0 flex-1 truncate">{String(component.props.title || component.component)}</span>
                  <Badge variant="outline" className="hidden font-mono text-[10px] sm:inline-flex">{component.region}</Badge>
                </button>
                <div className="flex items-center gap-0.5" aria-label={`Move ${component.id}`}>
                  <button
                    type="button"
                    className="rounded-md p-1 text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-30"
                    aria-label={`Move ${component.id} up`}
                    disabled={index === 0}
                    onClick={() => onMove(component.id, index - 1)}
                  >
                    <ArrowUp className="h-3.5 w-3.5" />
                  </button>
                  <button
                    type="button"
                    className="rounded-md p-1 text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-30"
                    aria-label={`Move ${component.id} down`}
                    disabled={index === components.length - 1}
                    onClick={() => onMove(component.id, index + 1)}
                  >
                    <ArrowDown className="h-3.5 w-3.5" />
                  </button>
                </div>
                <button
                  type="button"
                  className="rounded-md p-1 text-muted-foreground opacity-100 hover:bg-muted hover:text-foreground md:opacity-0 md:group-hover:opacity-100"
                  aria-label={`Duplicate ${component.id}`}
                  onClick={() => onDuplicate(component)}
                >
                  <Copy className="h-3.5 w-3.5" />
                </button>
                <button
                  type="button"
                  className="rounded-md p-1 text-muted-foreground opacity-100 hover:bg-destructive/10 hover:text-destructive md:opacity-0 md:group-hover:opacity-100"
                  aria-label={`Remove ${component.id}`}
                  onClick={() => onRemove(component.id)}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </button>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

function ComponentPalette({ primaryDoctype, onAdd }: { primaryDoctype: DocType | null; onAdd: (component: string) => void }) {
  const [query, setQuery] = useState('')
  const grouped = PAGE_COMPONENT_LIBRARY.reduce<Record<string, typeof PAGE_COMPONENT_LIBRARY[number][]>>((acc, entry) => {
    const normalizedQuery = query.trim().toLowerCase()
    const matches = !normalizedQuery ||
      entry.label.toLowerCase().includes(normalizedQuery) ||
      entry.component.toLowerCase().includes(normalizedQuery) ||
      entry.capabilities.some((capability) => capability.toLowerCase().includes(normalizedQuery))
    if (matches) acc[entry.group] = [...(acc[entry.group] ?? []), entry]
    return acc
  }, {})

  return (
    <div className="space-y-4 p-4">
      <SectionTitle icon={Boxes} title="Components" />
      <p className="rounded-lg border bg-muted/20 p-2 text-xs text-muted-foreground">
        {primaryDoctype
          ? `Adding data components will bind them to ${primaryDoctype.name}.`
          : 'Generate a standard screen first so added components have a data source.'}
      </p>
      <div className="relative">
        <Search className="pointer-events-none absolute left-2 top-2 h-3.5 w-3.5 text-muted-foreground" />
        <Input
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          placeholder="Search components..."
          className="h-8 pl-7 text-xs"
        />
      </div>
      {Object.entries(grouped).map(([group, entries]) => (
        <div key={group}>
          <p className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{group}</p>
          <div className="space-y-1">
            {entries.map((entry) => (
              (() => {
                const availability = componentAvailability(entry.component, primaryDoctype)
                return (
              <button
                key={entry.component}
                type="button"
                disabled={!availability.enabled}
                className={cn(
                  'flex w-full items-center justify-between rounded-lg border px-3 py-2 text-left text-xs transition-colors hover:bg-muted disabled:cursor-not-allowed disabled:opacity-50',
                  availability.enabled && isDataDisplayComponent(entry.component) && primaryDoctype ? 'border-primary/30 bg-primary/5' : 'bg-card',
                )}
                onClick={() => onAdd(entry.component)}
                title={availability.reason}
              >
                <span>
                  {entry.label}
                  {availability.enabled && isDataDisplayComponent(entry.component) && primaryDoctype && (
                    <span className="mt-0.5 block font-mono text-[10px] text-muted-foreground">primary.data</span>
                  )}
                  {!availability.enabled && (
                    <span className="mt-0.5 block text-[10px] text-muted-foreground">{availability.reason}</span>
                  )}
                </span>
                <Plus className="h-3.5 w-3.5 text-muted-foreground" />
              </button>
                )
              })()
            ))}
          </div>
        </div>
      ))}
      {Object.keys(grouped).length === 0 && (
        <p className="rounded-lg border border-dashed p-3 text-xs text-muted-foreground">No components match that search.</p>
      )}
    </div>
  )
}

function ManifestCanvas({
  manifest,
  mode,
  viewport,
  resourceState,
  selectedComponentId,
  onSelect,
  onDuplicate,
  onRemove,
}: {
  manifest: PageManifest
  mode: Exclude<WorkbenchMode, 'source'>
  viewport: 'desktop' | 'tablet' | 'mobile'
  resourceState: ResourceSimulationKind
  selectedComponentId: string | null
  onSelect: (id: string | null) => void
  onDuplicate: (component: PageComponent) => void
  onRemove: (id: string) => void
}) {
  return (
    <div className="mx-auto max-w-6xl space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>{mode === 'design' ? 'Design canvas' : 'Preview runtime'}</CardTitle>
      <CardDescription>
        {mode === 'design'
          ? `Editor chrome wraps the real registered Kora components without changing the manifest tree. ${viewport === 'desktop' ? 'Desktop' : viewport === 'tablet' ? 'Tablet' : 'Mobile'} preview keeps the same manifest tree and only changes the rendering width.`
          : `Preview renders the same manifest path without editor controls or unsafe actions. ${viewport === 'desktop' ? 'Desktop' : viewport === 'tablet' ? 'Tablet' : 'Mobile'} preview keeps the same manifest tree and only changes the rendering width.`}
      </CardDescription>
      </CardHeader>
    </Card>
      <div className={previewViewportClass(viewport)}>
        <ManifestRenderer
          manifest={manifest}
          mode={mode === 'design' ? 'editor' : 'preview'}
          resourceState={createSimulatedResourceState(manifest, resourceState)}
          selectedComponentId={selectedComponentId}
          onSelectComponent={onSelect}
          onDuplicateComponent={onDuplicate}
          onRemoveComponent={onRemove}
          className="rounded-xl border bg-background p-4"
        />
      </div>
    </div>
  )
}

function PreviewStatePanel({
  value,
  onChange,
  disabled,
}: {
  value: ResourceSimulationKind
  onChange: (value: ResourceSimulationKind) => void
  disabled: boolean
}) {
  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={AlertTriangle} title="Runtime state" />
      <Select value={value} onValueChange={(next) => onChange(next as ResourceSimulationKind)} disabled={disabled}>
        <SelectTrigger className="h-8 text-xs"><SelectValue /></SelectTrigger>
        <SelectContent>
          {RESOURCE_STATES.map((state) => (
            <SelectItem key={state} value={state}>{state.replace('_', ' ')}</SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="text-xs text-muted-foreground">
        Simulates backend-owned resource states for loading, empty, errors, permissions, offline, stale, and conflict preflight.
      </p>
    </div>
  )
}

function ResourceActionPanel({
  manifest,
  updateManifest,
}: {
  manifest: PageManifest
  updateManifest: (updater: (current: PageManifest) => PageManifest) => void
}) {
  return (
    <div className="space-y-4 border-b p-4">
      <SectionTitle icon={Database} title="Resources" />
      {manifest.spec.resources.map((resource, index) => (
        <ResourceEditor key={index} resource={resource} update={(patch) => updateManifest((current) => ({
          ...current,
          spec: {
            ...current.spec,
            resources: current.spec.resources.map((entry, i) => i === index ? { ...entry, ...patch } : entry),
          },
        }))} remove={() => updateManifest((current) => ({
          ...current,
          spec: { ...current.spec, resources: current.spec.resources.filter((_, i) => i !== index) },
        }))} />
      ))}
      <Button variant="outline" size="sm" className="w-full" onClick={() => updateManifest((current) => ({
        ...current,
        spec: {
          ...current.spec,
          resources: [...current.spec.resources, { id: `resource_${current.spec.resources.length + 1}`, query: 'document.list', params: { limit: 50 } }],
        },
      }))}>
        <Plus className="mr-2 h-4 w-4" />
        Add resource
      </Button>

      <SectionTitle icon={GitBranch} title="Commands" />
      {manifest.spec.actions.map((action, index) => (
        <ActionEditor key={index} action={action} update={(patch) => updateManifest((current) => ({
          ...current,
          spec: {
            ...current.spec,
            actions: current.spec.actions.map((entry, i) => i === index ? { ...entry, ...patch } : entry),
          },
        }))} remove={() => updateManifest((current) => ({
          ...current,
          spec: { ...current.spec, actions: current.spec.actions.filter((_, i) => i !== index) },
        }))} resources={manifest.spec.resources} />
      ))}
      <Button variant="outline" size="sm" className="w-full" onClick={() => updateManifest((current) => ({
        ...current,
        spec: {
          ...current.spec,
          actions: [...current.spec.actions, { id: `command_${current.spec.actions.length + 1}`, command: 'document.create', input: {}, invalidate: [] }],
        },
      }))}>
        <Plus className="mr-2 h-4 w-4" />
        Add command
      </Button>
    </div>
  )
}

function ResourceEditor({ resource, update, remove }: { resource: PageResource; update: (patch: Partial<PageResource>) => void; remove: () => void }) {
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <Input value={resource.id} onChange={(event) => update({ id: event.target.value })} className="h-8 text-xs" />
        <Button variant="ghost" size="icon" className="h-8 w-8 text-destructive" onClick={remove}><Trash2 className="h-4 w-4" /></Button>
      </div>
      <SelectField
        label="Query"
        value={resource.query}
        options={['document.list', 'analytics.insights']}
        onChange={(query) => update({ query })}
      />
      <Textarea value={JSON.stringify(resource.params, null, 2)} onChange={(event) => update({ params: parseJsonObject(event.target.value) })} className="min-h-20 font-mono text-xs" />
    </div>
  )
}

function ActionEditor({
  action,
  update,
  remove,
  resources,
}: {
  action: PageAction
  update: (patch: Partial<PageAction>) => void
  remove: () => void
  resources: PageResource[]
}) {
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <Input value={action.id} onChange={(event) => update({ id: event.target.value })} className="h-8 text-xs" />
        <Button variant="ghost" size="icon" className="h-8 w-8 text-destructive" onClick={remove}><Trash2 className="h-4 w-4" /></Button>
      </div>
      <SelectField
        label="Command"
        value={action.command}
        options={['document.create', 'document.update', 'document.delete', 'document.submit', 'document.cancel', 'workflow.transition']}
        onChange={(command) => update({ command })}
      />
      <ActionInvalidatePicker action={action} resources={resources} update={update} />
    </div>
  )
}

function ActionInvalidatePicker({
  action,
  resources,
  update,
}: {
  action: PageAction
  resources: PageResource[]
  update: (patch: Partial<PageAction>) => void
}) {
  const [selected, setSelected] = useState(action.invalidate[0] || '')
  const options = useMemo(() => resources.map((resource) => ({
    value: resource.id,
    label: resource.id,
    description: `${resource.query}${resource.params?.doctype ? ` for ${String(resource.params.doctype)}` : ''}`,
  })), [resources])
  useEffect(() => {
    if (action.invalidate[0]) setSelected(action.invalidate[0])
  }, [action.invalidate])
  return (
    <div className="space-y-1.5">
      <Label className="text-xs">Invalidate</Label>
      <div className="flex gap-2">
        <Select value={selected} onValueChange={(next) => { setSelected(next || ''); update({ invalidate: next ? [next] : [] }) }}>
          <SelectTrigger className="h-8 text-xs">
            <SelectValue placeholder="Choose resource..." />
          </SelectTrigger>
          <SelectContent>
            {options.map((option) => (
              <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  )
}

function ShortcutPanel() {
  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={Keyboard} title="Shortcuts" />
      <div className="grid grid-cols-2 gap-2 text-xs">
        <ShortcutHint label="Save" keys="Ctrl/⌘ S" />
        <ShortcutHint label="Undo" keys="Ctrl/⌘ Z" />
        <ShortcutHint label="Redo" keys="Ctrl/⌘ ⇧ Z" />
        <ShortcutHint label="Duplicate" keys="Ctrl/⌘ D" />
        <ShortcutHint label="Delete" keys="Del" />
        <ShortcutHint label="Deselect" keys="Esc" />
      </div>
    </div>
  )
}

function PublishPreflightPanel({ preflight }: { preflight: ReturnType<typeof buildPublishPreflight> }) {
  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={ShieldCheck} title="Publish preflight" />
      <p className={cn('rounded-lg border p-3 text-xs', preflight.canPublish ? 'bg-emerald-50 text-emerald-900' : 'bg-amber-50 text-amber-900')}>
        {preflight.canPublish
          ? `Ready to publish: ${preflight.resourceCount} resource(s), ${preflight.actionCount} action(s).`
          : `Fix manifest issues before publishing: ${preflight.issues.length} validation problem(s).`}
      </p>
      {(preflight.unsupportedResources.length > 0 || preflight.unsupportedActions.length > 0) && (
        <div className="space-y-2 text-xs">
          {preflight.unsupportedResources.length > 0 && (
            <p className="text-destructive">Unsupported resources: {preflight.unsupportedResources.join(', ')}</p>
          )}
          {preflight.unsupportedActions.length > 0 && (
            <p className="text-destructive">Unsupported actions: {preflight.unsupportedActions.join(', ')}</p>
          )}
        </div>
      )}
    </div>
  )
}

function ShortcutHint({ label, keys }: { label: string; keys: string }) {
  return (
    <div className="flex items-center justify-between gap-2 rounded-lg border bg-muted/20 px-2 py-1.5">
      <span className="text-muted-foreground">{label}</span>
      <kbd className="rounded border bg-background px-1.5 py-0.5 font-mono text-[10px]">{keys}</kbd>
    </div>
  )
}

function ComponentInspector({
  component,
  resourceOptions,
  updateComponent,
}: {
  component: PageComponent | null
  resourceOptions: Array<{ value: string; label: string; description?: string }>
  updateComponent: (patch: Partial<PageComponent>) => void
}) {
  if (!component) {
    return (
      <div className="border-b p-4">
        <SectionTitle icon={Boxes} title="Component" />
        <p className="text-sm text-muted-foreground">Select a component to edit its manifest contract.</p>
      </div>
    )
  }

  return (
    <div className="space-y-3 border-b p-4">
      <SectionTitle icon={Boxes} title="Component" />
      <TextField label="ID" value={component.id} onChange={(id) => updateComponent({ id })} />
      <TextField label="Type" value={component.component} onChange={(value) => updateComponent({ component: value })} />
      <TextField label="Title prop" value={String(component.props.title || '')} onChange={(title) => updateComponent({ props: { ...component.props, title } })} />
      <SelectField
        label="Data binding"
        value={component.data || ''}
        options={resourceOptions.map((option) => option.value)}
        onChange={(data) => updateComponent({ data })}
      />
      <SelectField label="Region" value={component.region} options={['main', 'side', 'left', 'right', 'header', 'footer']} onChange={(region) => updateComponent({ region })} />
      <TokenField label="Required capabilities" value={component.required_capabilities || []} onChange={(required_capabilities) => updateComponent({ required_capabilities })} />
      <TokenField label="Actions" value={component.actions || []} onChange={(actions) => updateComponent({ actions })} />
    </div>
  )
}

function SourcePanel({
  sourceDraft,
  sourceError,
  setSourceDraft,
  applySource,
  large = false,
}: {
  sourceDraft: string
  sourceError: string | null
  setSourceDraft: (value: string) => void
  applySource: () => void
  large?: boolean
}) {
  return (
    <div className="space-y-3 p-4">
      <SectionTitle icon={FileJson2} title="Source" />
      <Textarea
        value={sourceDraft}
        onChange={(event) => setSourceDraft(event.target.value)}
        className={cn('min-h-72 font-mono text-xs', large && 'min-h-[calc(100vh-14rem)]')}
        spellCheck={false}
      />
      {sourceError && <p className="text-xs text-destructive">{sourceError}</p>}
      <Button variant="outline" size="sm" className="w-full" onClick={applySource}>Apply JSON</Button>
    </div>
  )
}

function SectionTitle({ icon: Icon, title }: { icon: typeof FileJson2; title: string }) {
  return (
    <h2 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.16em] text-muted-foreground">
      <Icon className="h-4 w-4" />
      {title}
    </h2>
  )
}

function TextField({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs">{label}</Label>
      <Input value={value} onChange={(event) => onChange(event.target.value)} className="h-8 text-xs" />
    </div>
  )
}

function SelectField({ label, value, options, onChange }: { label: string; value: string; options: string[]; onChange: (value: string) => void }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs">{label}</Label>
      <Select value={value} onValueChange={(next) => { if (next) onChange(next) }}>
        <SelectTrigger className="h-8 text-xs"><SelectValue /></SelectTrigger>
        <SelectContent>{options.map((option) => <SelectItem key={option} value={option}>{option}</SelectItem>)}</SelectContent>
      </Select>
    </div>
  )
}

function TokenField({ label, value, onChange }: { label: string; value: string[]; onChange: (value: string[]) => void }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs">{label}</Label>
      <Input
        value={value.join(', ')}
        onChange={(event) => onChange(event.target.value.split(',').map((entry) => entry.trim()).filter(Boolean))}
        className="h-8 text-xs"
        placeholder="comma,separated,values"
      />
    </div>
  )
}

function parseJsonObject(value: string): Record<string, unknown> {
  try {
    const parsed = JSON.parse(value)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {}
  } catch {
    return {}
  }
}

function isDataDisplayComponent(component: string): boolean {
  return ['record_table', 'record_list', 'record_cards', 'record_form', 'record_detail', 'chart'].includes(component)
}

function cloneManifest(manifest: PageManifest): PageManifest {
  return JSON.parse(JSON.stringify(manifest)) as PageManifest
}

function duplicateComponent(component: PageComponent, position: number): PageComponent {
  const clone = JSON.parse(JSON.stringify(component)) as PageComponent
  const id = `${component.component}_${Date.now()}`
  return {
    ...clone,
    id,
    position,
    props: {
      ...clone.props,
      title: `${String(clone.props.title || clone.component)} copy`,
    },
  }
}

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  const tag = target.tagName.toLowerCase()
  return tag === 'input' || tag === 'textarea' || tag === 'select' || target.isContentEditable
}
