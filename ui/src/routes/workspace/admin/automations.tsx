import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Activity, ArrowDown, ArrowRight, Check, CircleAlert, Clock3, GitBranch, History, Loader2, Mail, Plus, RefreshCw, ShieldCheck, Sparkles, Workflow } from 'lucide-react'
import { Link } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { fetchDoctypes, fetchRoles } from '@/lib/api/system'
import { getWorkflowDefinition, getWorkflowHistory, getWorkflowCapabilities, listWorkflowDefinitions, listWorkflowInstances, listWorkflowPreviews, previewWorkflow, publishWorkflow, validateWorkflow } from '@/lib/api/workflows'
import type { WorkflowDefinition, WorkflowHistoryEntry, WorkflowPreview, WorkflowRef } from '@/lib/api/workflows'
import { buildAutomationDefinition, createAutomationSample, emptyAutomationEditor, type AutomationEditor } from '@/lib/workflow-builder'

export default function AdminAutomationsPage() {
  const [editor, setEditor] = useState<AutomationEditor | null>(null)
  const [previewResult, setPreviewResult] = useState<WorkflowPreview | null>(null)
  const [issues, setIssues] = useState<Array<{ path: string; detail: string }>>([])
  const [busy, setBusy] = useState<'test' | 'publish' | 'edit' | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [sampleJSON, setSampleJSON] = useState('{\n  "name": "SAMPLE-1"\n}')
  const [history, setHistory] = useState<{ id: string; entries: WorkflowHistoryEntry[] } | null>(null)
  const [selectedPreview, setSelectedPreview] = useState<WorkflowPreview | null>(null)
  const { data: tables = [], isLoading: loadingTables } = useQuery({ queryKey: ['automations', 'tables'], queryFn: fetchDoctypes })
  const { data: roles = [] } = useQuery({ queryKey: ['automations', 'roles'], queryFn: fetchRoles })
  const { data: definitions = [], isLoading: loadingDefinitions, refetch } = useQuery({ queryKey: ['automations', 'definitions'], queryFn: listWorkflowDefinitions })
  const { data: previews = [] } = useQuery({ queryKey: ['automations', 'previews'], queryFn: listWorkflowPreviews })
  const { data: instances = [] } = useQuery({ queryKey: ['automations', 'instances'], queryFn: listWorkflowInstances })
  const { data: capability } = useQuery({ queryKey: ['automations', 'capability', editor?.table], queryFn: () => getWorkflowCapabilities(editor!.table), enabled: Boolean(editor?.table) })
  const tableOptions = tables.filter((table) => !table.is_child_table)
  const definition = useMemo(() => editor?.table ? buildAutomationDefinition(editor, capability) : null, [editor, capability])
  const [sampleCustomized, setSampleCustomized] = useState(false)
  useEffect(() => {
    if (!editor?.table || !capability || sampleCustomized) return
    setSampleJSON(JSON.stringify(createAutomationSample(editor, capability), null, 2))
  }, [editor?.table, editor?.goal, editor?.transitionAction, capability, sampleCustomized])

  const startNew = () => {
    const initial = emptyAutomationEditor()
    setEditor(initial); setPreviewResult(null); setIssues([]); setNotice(null); setHistory(null)
    setSampleCustomized(false)
    setSampleJSON('{\n  "name": "SAMPLE-1"\n}')
  }
  const update = (patch: Partial<AutomationEditor>) => {
    setEditor((current) => current ? { ...current, ...patch } : current)
    setPreviewResult(null); setIssues([]); setNotice(null)
  }

  const editDefinition = async (ref: WorkflowRef) => {
    setBusy('edit'); setNotice(null)
    try {
      const { definition: saved } = await getWorkflowDefinition(ref.definition_id, ref.version)
      const action = saved.steps.find((step: any) => step.kind === 'action') as any
      const human = saved.steps.find((step: any) => step.kind === 'human') as any
      const condition = saved.steps.find((step: any) => step.kind === 'condition') as any
      const trigger = saved.trigger
      if (!trigger) throw new Error('This definition does not contain a record-triggered automation.')
      const table = trigger.table
      const input = action?.action?.input ?? {}
      const next: AutomationEditor = {
        ...emptyAutomationEditor(), id: saved.id, version: saved.version + 1, title: saved.id.replaceAll('_', ' '), table,
        trigger: trigger.kind || 'record.updated', watchedField: trigger.watched_fields?.[0] ?? '',
        goal: human ? 'approval' : action?.action?.name === 'record.update_status' ? 'status' : 'notify',
        recipientField: input.recipient_field ?? '', subject: human?.human?.request?.title ?? input.subject ?? '',
        message: human?.human?.request?.message ?? input.message ?? '', transitionAction: input.transition_action ?? '',
        stateField: input.state_field ?? '', initialState: input.initial_state ?? '', transitionFrom: input.from ?? '', transitionTo: input.to ?? '',
        transitionRoles: input.allowed_roles ?? [], requiredFields: input.required_fields ?? [], transitionCondition: input.condition ?? '', docStatus: input.doc_status ?? 0,
        policyID: human?.human?.request?.policy_id ?? '', conditionOn: Boolean(condition), conditionField: condition?.condition?.field?.field ?? '',
        conditionOperator: condition?.condition?.field?.operator ?? 'equals', conditionValue: String(condition?.condition?.field?.value ?? ''),
      }
      setEditor(next); setPreviewResult(null); setIssues([]); setHistory(null); setSampleCustomized(false)
      const loadedCapability = await getWorkflowCapabilities(trigger.table)
      setSampleJSON(JSON.stringify(createAutomationSample(next, loadedCapability), null, 2))
    } catch (error) { setNotice((error as Error).message) }
    finally { setBusy(null) }
  }

  const testFlow = async () => {
    if (!definition) return
    setBusy('test'); setIssues([]); setNotice(null)
    try {
      const checked = await validateWorkflow(definition)
      if (!checked.valid) { setIssues(checked.issues); return }
      const parsed = JSON.parse(sampleJSON) as Record<string, unknown>
      const result = await previewWorkflow(definition, parsed)
      setPreviewResult(result)
      if (result.status !== 'succeeded') setNotice(result.failure || 'The safe test did not pass.')
    } catch (error) { setNotice((error as Error).message) }
    finally { setBusy(null) }
  }

  const publish = async () => {
    if (!definition || previewResult?.status !== 'succeeded') return
    setBusy('publish'); setNotice(null)
    try {
      const checked = await validateWorkflow(definition)
      if (!checked.valid) { setIssues(checked.issues); setNotice('Fix the items below and run the safe test again.'); return }
      const published = await publishWorkflow(definition)
      setNotice(`Published version ${published.version}. It will run when a matching record change is saved.`)
      setEditor(null); setPreviewResult(null); await refetch()
    } catch (error) { setNotice((error as Error).message) }
    finally { setBusy(null) }
  }

  const showHistory = async (id: string) => {
    try { setHistory({ id, entries: await getWorkflowHistory(id) }) }
    catch (error) { setNotice((error as Error).message) }
  }

  if (editor) {
    const emailFields = capability?.fields.filter((field) => field.type === 'Email') ?? []
    const actionsReady = capability?.actions.some((action) => action.name === (editor.goal === 'notify' ? 'email.send' : editor.goal === 'status' ? 'record.update_status' : '')) ?? false
    return <main className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
      <header className="flex flex-wrap items-start justify-between gap-3 border-b pb-5">
        <div><button onClick={() => { setEditor(null); setPreviewResult(null) }} className="mb-2 text-sm text-muted-foreground hover:text-foreground">← All automations</button><p className="text-xs font-medium uppercase tracking-widest text-muted-foreground">Automation setup</p><h1 className="mt-1 text-2xl font-semibold tracking-tight">{editor.id ? 'Edit automation' : 'Create an automation'}</h1><p className="mt-1 text-sm text-muted-foreground">Choose what starts it, then add one clear action.</p></div>
        <div className="flex items-center gap-2 rounded-full border px-3 py-1.5 text-xs text-muted-foreground"><ShieldCheck className="h-4 w-4" /> Changes are tested before publishing</div>
      </header>

      <div className="mt-6 grid gap-6 lg:grid-cols-[minmax(0,1fr)_320px]">
        <section className="min-w-0">
          <div className="mb-5 grid gap-4 sm:grid-cols-2">
            <div><Label htmlFor="automation-title">Name this automation</Label><Input id="automation-title" className="mt-1" value={editor.title} onChange={(event) => update({ title: event.target.value })} placeholder="For example, Notify the sales team" /></div>
            <div><Label htmlFor="automation-table">When should it run?</Label><Select value={editor.table} onValueChange={(table) => { update({ table: table || '', recipientField: '', conditionField: '', watchedField: '', transitionAction: '', stateField: '', initialState: '', transitionFrom: '', transitionTo: '', transitionRoles: [], requiredFields: [], transitionCondition: '', policyID: '' }); setSampleCustomized(false); setSampleJSON('{\n  "name": "SAMPLE-1"\n}') }}><SelectTrigger id="automation-table" className="mt-1"><SelectValue placeholder={loadingTables ? 'Loading tables…' : 'Choose a table'} /></SelectTrigger><SelectContent>{tableOptions.map((table) => <SelectItem key={table.name} value={table.name}>{table.name}</SelectItem>)}</SelectContent></Select></div>
          </div>

          <div className="relative space-y-0">
            <div className="absolute bottom-12 left-[19px] top-7 w-px bg-border" aria-hidden="true" />
            <FlowNode icon={<Sparkles className="h-4 w-4" />} title="When this happens" detail="A record is saved in your table" number="1">
              <div className="grid gap-3 sm:grid-cols-2">
                <div><Label>Change</Label><Select disabled={editor.goal === 'status'} value={editor.goal === 'status' ? 'record.updated' : editor.trigger} onValueChange={(value) => update({ trigger: (value || 'record.created') as AutomationEditor['trigger'], watchedField: '' })}><SelectTrigger className="mt-1"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="record.created">A record is created</SelectItem><SelectItem value="record.updated">A record is updated</SelectItem></SelectContent></Select>{editor.goal === 'status' && <p className="mt-1 text-xs text-muted-foreground">Status changes start this flow; a built-in state check prevents it from repeating.</p>}</div>
                {editor.trigger === 'record.updated' && editor.goal !== 'status' && <div><Label>Only when this field changes <span className="font-normal text-muted-foreground">(optional)</span></Label><Select value={editor.watchedField || 'any'} onValueChange={(value) => update({ watchedField: !value || value === 'any' ? '' : value })}><SelectTrigger className="mt-1"><SelectValue placeholder="Any field" /></SelectTrigger><SelectContent><SelectItem value="any">Any field</SelectItem>{capability?.fields.map((field) => <SelectItem key={field.name} value={field.name}>{field.label}</SelectItem>)}</SelectContent></Select></div>}
              </div>
            </FlowNode>

            {editor.conditionOn && <FlowNode icon={<GitBranch className="h-4 w-4" />} title={editor.goal === 'status' ? 'Check the current status' : 'Only continue when…'} detail={editor.goal === 'status' ? 'The status check prevents this transition from repeating' : 'Records that do not match will stop here'} number="2" onRemove={editor.goal === 'status' ? undefined : () => update({ conditionOn: false })}>
              <div className="grid gap-3 sm:grid-cols-[1fr_1fr_1fr]">
                <div><Label>Field</Label><Select disabled={editor.goal === 'status'} value={editor.conditionField} onValueChange={(value) => update({ conditionField: value || '' })}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose a field" /></SelectTrigger><SelectContent>{capability?.fields.map((field) => <SelectItem key={field.name} value={field.name}>{field.label}</SelectItem>)}</SelectContent></Select></div>
                <div><Label>Rule</Label><Select disabled={editor.goal === 'status'} value={editor.conditionOperator} onValueChange={(value) => update({ conditionOperator: value || 'equals' })}><SelectTrigger className="mt-1"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="equals">is</SelectItem><SelectItem value="not_equals">is not</SelectItem><SelectItem value="is_empty">is empty</SelectItem><SelectItem value="is_not_empty">is not empty</SelectItem><SelectItem value="greater_than">is greater than</SelectItem><SelectItem value="at_least">is at least</SelectItem><SelectItem value="less_than">is less than</SelectItem><SelectItem value="at_most">is at most</SelectItem></SelectContent></Select></div>
                {!editor.conditionOperator.startsWith('is_') && <div><Label>Value</Label><Input readOnly={editor.goal === 'status'} className="mt-1" value={editor.conditionValue} onChange={(event) => update({ conditionValue: event.target.value })} placeholder="Enter a value" /> </div>}
              </div>
            </FlowNode>}

            <FlowNode icon={editor.goal === 'notify' ? <Mail className="h-4 w-4" /> : editor.goal === 'approval' ? <ShieldCheck className="h-4 w-4" /> : <RefreshCw className="h-4 w-4" />} title="Then do this" detail="Pick the result you want" number={editor.conditionOn ? '3' : '2'}>
              <div className="mb-4 grid gap-2 sm:grid-cols-3">
                {([
                  ['notify', 'Send an email', 'Tell someone when a record changes'],
                  ['approval', 'Ask for approval', 'Route a decision through your policy'],
                  ['status', 'Move to a status', 'Use an allowed status transition'],
                ] as const).map(([value, title, description]) => <button key={value} type="button" onClick={() => update({ goal: value })} className={`rounded-xl border p-3 text-left transition-colors ${editor.goal === value ? 'border-primary bg-primary/5 ring-1 ring-primary/20' : 'hover:bg-muted/60'}`}><span className="block text-sm font-medium">{title}</span><span className="mt-1 block text-xs leading-5 text-muted-foreground">{description}</span></button>)}
              </div>
              {editor.goal === 'notify' && <div className="grid gap-3">
                <div><Label>Send to the email address in</Label><Select value={editor.recipientField} onValueChange={(value) => update({ recipientField: value || '' })}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose an Email field" /></SelectTrigger><SelectContent>{emailFields.map((field) => <SelectItem key={field.name} value={field.name}>{field.label}</SelectItem>)}</SelectContent></Select></div>
                <div><Label>Email subject</Label><Input className="mt-1" value={editor.subject} onChange={(event) => update({ subject: event.target.value })} maxLength={200} /></div>
                <div><Label>Message</Label><Textarea className="mt-1 min-h-20" value={editor.message} onChange={(event) => update({ message: event.target.value })} maxLength={8192} /></div>
                {editor.extraEmails.map((email, index) => <div key={index} className="rounded-lg border-t pt-4"><div className="mb-2 flex items-center justify-between"><p className="text-sm font-medium">Then send another email</p><button className="text-xs text-muted-foreground" onClick={() => update({ extraEmails: editor.extraEmails.filter((_, i) => i !== index) })}>Remove</button></div><Label>Send to the email address in</Label><Select value={email.recipientField} onValueChange={(value) => update({ extraEmails: editor.extraEmails.map((item, i) => i === index ? { ...item, recipientField: value || '' } : item) })}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose an Email field" /></SelectTrigger><SelectContent>{emailFields.map((field) => <SelectItem key={field.name} value={field.name}>{field.label}</SelectItem>)}</SelectContent></Select><Label className="mt-3 block">Email subject</Label><Input className="mt-1" value={email.subject} onChange={(event) => update({ extraEmails: editor.extraEmails.map((item, i) => i === index ? { ...item, subject: event.target.value } : item) })} /><Label className="mt-3 block">Message</Label><Textarea className="mt-1 min-h-16" value={email.message} onChange={(event) => update({ extraEmails: editor.extraEmails.map((item, i) => i === index ? { ...item, message: event.target.value } : item) })} /></div>)}
                {editor.extraEmails.length < 3 && <button type="button" onClick={() => update({ extraEmails: [...editor.extraEmails, { recipientField: '', subject: 'A record was updated', message: 'A record needs your attention.' }] })} className="flex items-center gap-2 text-sm font-medium text-primary"><Plus className="h-4 w-4" />Add another email step</button>}
              </div>}
              {editor.goal === 'approval' && <div className="grid gap-3">
                <div><Label>Approval policy</Label><Select value={editor.policyID} onValueChange={(value) => update({ policyID: value || '' })}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose an approval policy" /></SelectTrigger><SelectContent>{capability?.approval_policies.map((policy) => <SelectItem key={policy} value={policy}>{policy}</SelectItem>)}</SelectContent></Select>{!capability?.approval_policies.length && <p className="mt-2 text-sm text-amber-700">No approval policy is set up for this table. Add one in Settings before publishing.</p>}</div>
                <div><Label>Request title</Label><Input className="mt-1" value={editor.subject} onChange={(event) => update({ subject: event.target.value })} maxLength={160} placeholder="Review this request" /></div>
                <div><Label>What should the approver know?</Label><Textarea className="mt-1 min-h-20" value={editor.message} onChange={(event) => update({ message: event.target.value })} maxLength={8192} /></div>
              </div>}
              {editor.goal === 'status' && <div className="space-y-4">
                <StatusActionEditor editor={editor} fields={capability?.fields || []} roles={roles.map((role) => role.name)} adminRole={capability?.admin_role || 'Administrator'} onChange={update} />
              </div>}
            </FlowNode>

            {!editor.conditionOn && <button type="button" onClick={() => update({ conditionOn: true })} className="relative ml-1 flex items-center gap-2 py-3 pl-8 text-sm font-medium text-muted-foreground hover:text-foreground"><span className="absolute left-[11px] grid h-5 w-5 place-items-center rounded-full border bg-background"><Plus className="h-3 w-3" /></span>Add a condition</button>}
            <FlowNode icon={<Check className="h-4 w-4" />} title="Done" detail="Kora records the result in run history" number={editor.conditionOn ? '4' : '3'} terminal />
          </div>

          {capability?.blockers?.length ? <div className="mt-5 space-y-2 rounded-xl border border-amber-300 bg-amber-50 p-4 text-sm text-amber-950">{capability.blockers.map((blocker) => <p key={blocker} className="flex gap-2"><CircleAlert className="mt-0.5 h-4 w-4 shrink-0" />{blocker}</p>)}</div> : null}
          {capability && editor.goal === 'notify' && !emailFields.length && <div className="mt-5 rounded-xl border border-amber-300 bg-amber-50 p-4 text-sm text-amber-950"><p className="font-medium">This table needs an Email field first.</p><p className="mt-1">Add one to the table, then return here to choose who receives the notification.</p></div>}
          {capability && ['notify', 'status'].includes(editor.goal) && !actionsReady && <div className="mt-5 rounded-xl border border-amber-300 bg-amber-50 p-4 text-sm text-amber-950">This action is unavailable for this workspace. Ask an administrator to enable the required workflow capability.</div>}
          {issues.length > 0 && <div className="mt-5 rounded-xl border border-destructive/30 bg-destructive/5 p-4"><h3 className="text-sm font-semibold">Fix these items to continue</h3><ul className="mt-2 space-y-1 text-sm text-destructive">{issues.map((issue, index) => <li key={`${issue.path}-${index}`}>{issue.detail} <span className="text-xs opacity-70">({issue.path})</span></li>)}</ul></div>}
          {notice && <div className={`mt-5 rounded-xl border p-4 text-sm ${previewResult?.status === 'succeeded' && notice.startsWith('Published') ? 'border-emerald-300 bg-emerald-50 text-emerald-900' : 'border-muted bg-muted/40'}`}>{notice}</div>}
        </section>

        <aside className="space-y-4 lg:sticky lg:top-5 lg:self-start">
          <section className="rounded-xl border bg-card p-4">
            <div className="flex items-center gap-2"><Activity className="h-4 w-4 text-primary" /><h2 className="font-semibold">Try it safely</h2></div>
            <p className="mt-1 text-sm text-muted-foreground">Use an example record. Kora checks the path without sending emails or changing data.</p>
            <Textarea aria-label="Example record" className="mt-3 min-h-36 font-mono text-xs" value={sampleJSON} onChange={(event) => { setSampleJSON(event.target.value); setSampleCustomized(true); setPreviewResult(null) }} />
            <Button className="mt-3 w-full" onClick={testFlow} disabled={!definition || !editor.table || busy !== null || (editor.goal === 'approval' && !editor.policyID) || (editor.goal === 'status' && (!editor.stateField || !editor.initialState || !editor.transitionAction || !editor.transitionFrom || !editor.transitionTo || !editor.transitionRoles.length || !actionsReady)) || (editor.goal === 'notify' && (!editor.recipientField || !actionsReady || editor.extraEmails.some((item) => !item.recipientField)))}>{busy === 'test' ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Activity className="mr-2 h-4 w-4" />}Test this automation</Button>
            {previewResult && <div className={`mt-3 rounded-lg p-3 text-sm ${previewResult.status === 'succeeded' ? 'bg-emerald-50 text-emerald-950' : 'bg-destructive/10 text-destructive'}`}><p className="font-medium">{previewResult.status === 'succeeded' ? 'Test passed' : 'Test needs attention'}</p>{previewResult.steps.map((step) => <p key={step.step_id} className="mt-1 text-xs">{step.result}</p>)}{previewResult.failure && <p className="mt-1 text-xs">{previewResult.failure}</p>}</div>}
            <Button className="mt-3 w-full" variant="secondary" onClick={publish} disabled={previewResult?.status !== 'succeeded' || busy !== null || !editor.title.trim()}>{busy === 'publish' ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Check className="mr-2 h-4 w-4" />}Publish automation</Button>
            <p className="mt-2 text-center text-xs text-muted-foreground">Publishing starts a new version. Existing runs keep their original version.</p>
          </section>
          <section className="rounded-xl border p-4"><h2 className="flex items-center gap-2 font-semibold"><History className="h-4 w-4" />Recent safe tests</h2>{previews.slice(0, 4).map((preview) => <button key={preview.id} onClick={() => setSelectedPreview(preview)} className="mt-3 flex w-full items-start justify-between gap-2 text-left text-sm"><span className="truncate">{preview.definition_id} · v{preview.definition_version}</span><span className={preview.status === 'succeeded' ? 'text-emerald-700' : 'text-destructive'}>{preview.status}</span></button>)}{!previews.length && <p className="mt-2 text-sm text-muted-foreground">Tests will appear here.</p>}</section>
          {selectedPreview && <section className="rounded-xl border p-4"><button className="float-right text-xs text-muted-foreground" onClick={() => setSelectedPreview(null)}>Close</button><h3 className="font-medium">Test details</h3><p className="mt-1 text-xs text-muted-foreground">{selectedPreview.definition_id} · version {selectedPreview.definition_version}</p>{selectedPreview.steps.map((step) => <p className="mt-2 text-xs" key={step.step_id}>{step.result}</p>)}</section>}
        </aside>
      </div>
    </main>
  }

  return <main className="mx-auto w-full max-w-6xl px-4 py-7 sm:px-6 lg:px-8">
    <header className="flex flex-wrap items-end justify-between gap-4 border-b pb-5"><div><p className="text-xs font-medium uppercase tracking-[0.16em] text-muted-foreground">Workspace</p><h1 className="mt-1 text-3xl font-semibold tracking-tight">Automations</h1><p className="mt-2 max-w-2xl text-sm text-muted-foreground">Tell Kora what to do when records change. Test every flow safely before it goes live.</p></div><Button onClick={startNew}><Plus className="mr-2 h-4 w-4" />Create automation</Button></header>
    {history && <section className="mt-5 rounded-xl border bg-card p-4"><div className="flex items-center justify-between"><h2 className="font-semibold">Run history · {history.id}</h2><button onClick={() => setHistory(null)} className="text-sm text-muted-foreground">Close</button></div>{history.entries.map((entry) => <div key={entry.sequence} className="mt-3 flex items-start gap-3 border-t pt-3 text-sm"><span className="grid h-7 w-7 place-items-center rounded-full bg-muted text-xs">{entry.sequence}</span><div><p className="font-medium">{entry.step_id} · {entry.status}</p><p className="text-xs text-muted-foreground">{entry.detail || entry.kind} · {new Date(entry.created_at).toLocaleString()}</p></div></div>)}</section>}
    {notice && !history && <p className="mt-4 rounded-lg border bg-muted/40 p-3 text-sm">{notice}</p>}
    <section className="mt-7">
      <div className="mb-3 flex items-center justify-between"><h2 className="text-lg font-semibold">Published automations</h2><span className="text-sm text-muted-foreground">{definitions.length} total</span></div>
      {loadingDefinitions ? <p className="py-8 text-sm text-muted-foreground">Loading automations…</p> : definitions.length === 0 ? <div className="rounded-xl border border-dashed px-5 py-12 text-center"><Workflow className="mx-auto h-9 w-9 text-muted-foreground" /><h3 className="mt-3 font-semibold">Nothing is automated yet</h3><p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">Start with one helpful task, such as emailing a teammate when a record is added.</p><Button className="mt-5" onClick={startNew}><Plus className="mr-2 h-4 w-4" />Create your first automation</Button></div> : <div className="divide-y rounded-xl border">{definitions.map((ref) => <DefinitionRow key={`${ref.definition_id}:${ref.version}`} refItem={ref} onEdit={editDefinition} busy={busy === 'edit'} />)}</div>}
    </section>
    <section className="mt-9 grid gap-6 lg:grid-cols-2">
      <div><h2 className="mb-3 flex items-center gap-2 text-lg font-semibold"><Clock3 className="h-4 w-4" />Recent runs</h2>{instances.slice(0, 5).map((instance) => <div key={instance.id} className="flex items-center justify-between gap-3 border-b py-3 text-sm"><span className="truncate">{instance.definition_id} · {instance.subject.kind} {instance.subject.id}</span><span className="shrink-0 text-muted-foreground">{instance.status}</span><Button variant="ghost" size="sm" onClick={() => showHistory(instance.id)}>History</Button></div>)}{!instances.length && <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">Runs appear here after an automation is triggered.</p>}</div>
      <div><h2 className="mb-3 flex items-center gap-2 text-lg font-semibold"><History className="h-4 w-4" />Safe test history</h2>{previews.slice(0, 5).map((preview) => <div key={preview.id} className="flex items-center justify-between gap-3 border-b py-3 text-sm"><span className="truncate">{preview.definition_id} · v{preview.definition_version}</span><span className={preview.status === 'succeeded' ? 'text-emerald-700' : 'text-destructive'}>{preview.status}</span></div>)}{!previews.length && <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">Tests appear here before an automation is published.</p>}</div>
    </section>
  </main>
}

function FlowNode({ icon, title, detail, number, terminal, onRemove, children }: { icon: React.ReactNode; title: string; detail: string; number: string; terminal?: boolean; onRemove?: () => void; children?: React.ReactNode }) {
  return <div className="relative pb-3 pl-10"><span className={`absolute left-0 top-5 z-10 grid h-10 w-10 place-items-center rounded-full border bg-background ${terminal ? 'text-emerald-700' : 'text-primary'}`}>{icon}</span><section className="rounded-xl border bg-card p-4 shadow-sm sm:p-5"><div className="flex items-start justify-between gap-3"><div><p className="text-xs font-medium uppercase tracking-wider text-muted-foreground">Step {number}</p><h2 className="mt-1 font-semibold">{title}</h2><p className="mt-1 text-sm text-muted-foreground">{detail}</p></div>{onRemove && <button onClick={onRemove} className="text-xs text-muted-foreground hover:text-foreground">Remove</button>}</div>{children && <div className="mt-4 border-t pt-4">{children}</div>}</section>{!terminal && <span className="absolute bottom-0 left-[18px] flex h-4 items-center justify-center bg-background px-0.5"><ArrowDown className="h-3 w-3 text-muted-foreground" /></span>}</div>
}

function StatusActionEditor({ editor, fields, roles, adminRole, onChange }: {
  editor: AutomationEditor
  fields: Array<{ name: string; label: string; type: string; options?: string }>
  roles: string[]
  adminRole: string
  onChange: (patch: Partial<AutomationEditor>) => void
}) {
  const stateFields = fields.filter((field) => field.type === 'Select')
  const stateField = stateFields.find((field) => field.name === editor.stateField)
  const values = (stateField?.options || '').split(/\r?\n/).map((value) => value.trim()).filter(Boolean)
  const roleOptions = Array.from(new Set([adminRole, ...roles]))
  const availableFields = fields.filter((field) => field.type !== 'Section Break' && field.type !== 'Column Break')

  const chooseStateField = (fieldName: string) => {
    const selected = stateFields.find((field) => field.name === fieldName)
    const options = (selected?.options || '').split(/\r?\n/).map((value) => value.trim()).filter(Boolean)
    const initialState = options[0] || ''
    const transitionFrom = options[0] || ''
    const transitionTo = options.find((value) => value !== transitionFrom) || ''
    onChange({ stateField: fieldName, initialState, transitionFrom, transitionTo, transitionAction: transitionTo ? `Move to ${transitionTo}` : '', transitionRoles: editor.transitionRoles.length ? editor.transitionRoles : [adminRole], requiredFields: [], conditionOn: true, conditionField: fieldName, conditionOperator: 'equals', conditionValue: transitionFrom })
  }

  const updateFrom = (from: string) => {
    const transitionTo = values.find((value) => value !== from) || ''
    onChange({ transitionFrom: from, transitionTo, conditionOn: true, conditionField: editor.stateField, conditionOperator: 'equals', conditionValue: from })
  }

  return <div className="space-y-4">
    <div><Label>Status field</Label><Select value={editor.stateField} onValueChange={(value) => chooseStateField(value || '')}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose a Select field" /></SelectTrigger><SelectContent>{stateFields.map((field) => <SelectItem key={field.name} value={field.name}>{field.label}</SelectItem>)}</SelectContent></Select>
      {!stateFields.length && <p className="mt-2 text-sm text-amber-800">Add a Select field with status options first. <Link className="font-medium underline" to="/workspace/admin/doctypes/$name" params={{ name: editor.table }}>Open {editor.table} fields</Link></p>}
    </div>
    {stateField && values.length > 0 && <>
      <div className="grid gap-3 sm:grid-cols-2">
        <div><Label>Starting status</Label><Select value={editor.initialState} onValueChange={(value) => onChange({ initialState: value || '' })}><SelectTrigger className="mt-1"><SelectValue placeholder="Choose the initial status" /></SelectTrigger><SelectContent>{values.map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}</SelectContent></Select></div>
        <div><Label>Button name</Label><Input className="mt-1" value={editor.transitionAction} onChange={(event) => onChange({ transitionAction: event.target.value })} placeholder="For example, Approve" /></div>
        <div><Label>From</Label><Select value={editor.transitionFrom} onValueChange={(value) => updateFrom(value || '')}><SelectTrigger className="mt-1"><SelectValue placeholder="Current status" /></SelectTrigger><SelectContent>{values.filter((value) => value !== editor.transitionTo).map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}</SelectContent></Select></div>
        <div><Label>To</Label><Select value={editor.transitionTo} onValueChange={(value) => onChange({ transitionTo: value || '', transitionAction: editor.transitionAction || (value ? `Move to ${value}` : '') })}><SelectTrigger className="mt-1"><SelectValue placeholder="Next status" /></SelectTrigger><SelectContent>{values.filter((value) => value !== editor.transitionFrom).map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}</SelectContent></Select></div>
      </div>
      <fieldset><legend className="text-sm font-medium">Who can use this move?</legend><div className="mt-2 flex flex-wrap gap-x-4 gap-y-2">{roleOptions.map((role) => <label key={role} className="flex items-center gap-2 text-sm"><input type="checkbox" checked={editor.transitionRoles.includes(role)} onChange={(event) => onChange({ transitionRoles: event.target.checked ? [...editor.transitionRoles, role] : editor.transitionRoles.filter((value) => value !== role) })} />{role}</label>)}</div>{!editor.transitionRoles.length && <p className="mt-1 text-xs text-destructive">Choose at least one role.</p>}</fieldset>
      <fieldset><legend className="text-sm font-medium">Require these fields first <span className="font-normal text-muted-foreground">(optional)</span></legend><div className="mt-2 flex flex-wrap gap-x-4 gap-y-2">{availableFields.filter((field) => field.name !== editor.stateField).map((field) => <label key={field.name} className="flex items-center gap-2 text-sm"><input type="checkbox" checked={editor.requiredFields.includes(field.name)} onChange={(event) => onChange({ requiredFields: event.target.checked ? [...editor.requiredFields, field.name] : editor.requiredFields.filter((value) => value !== field.name) })} />{field.label}</label>)}</div></fieldset>
      <p className="text-xs text-muted-foreground">The status change runs only when the record is in the selected “From” state. Kora checks access and required fields on the server.</p>
    </>}
    {stateField && !values.length && <p className="text-sm text-amber-800">Add status choices to {stateField.label} in the table fields, then return here.</p>}
  </div>
}

function DefinitionRow({ refItem, onEdit, busy }: { refItem: WorkflowRef; onEdit: (ref: WorkflowRef) => void; busy: boolean }) {
  const { data } = useQuery({ queryKey: ['automations', 'definition', refItem.definition_id, refItem.version], queryFn: () => getWorkflowDefinition(refItem.definition_id, refItem.version) })
  const editable = Boolean(data?.definition.trigger)
  return <div className="flex flex-wrap items-center justify-between gap-3 p-4 sm:px-5"><div className="min-w-0"><p className="truncate font-medium">{refItem.definition_id.replaceAll('_', ' ')}</p><p className="mt-1 text-xs text-muted-foreground">Published version {refItem.version} · {refItem.definition_id}</p></div><Button variant="outline" size="sm" onClick={() => onEdit(refItem)} disabled={busy || !editable}>{busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <ArrowRight className="mr-2 h-4 w-4" />}{editable ? 'Edit next version' : 'Loading definition'}</Button></div>
}
