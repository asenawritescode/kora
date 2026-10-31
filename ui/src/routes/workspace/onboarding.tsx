import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { api } from '@/lib/api/client'
import { readOnboardingConversationID, saveOnboardingConversationID } from '@/lib/onboarding-session'
import { Button, Card, CardContent, CardHeader, CardTitle, Input, Textarea } from '@/components/views/StudioPrimitives'
import { Check, HelpCircle, Loader2, Send, Sparkles } from 'lucide-react'

type Topic = 'business' | 'goal' | 'records' | 'process' | 'team' | 'ready'
type Conversation = { id: string; current_topic: Topic; status: string }
type Message = { id: string; actor: string; text: string; interpretation?: string }
type State = { conversation: Conversation; messages: Message[]; progress: { current: Topic; completed: Topic[] } }

const prompts: Record<Topic, string> = { business: 'What would you like to make easier in your day-to-day work?', goal: 'What is the main outcome you want from that work?', records: 'What information or records do you need to keep track of?', process: 'What are the usual steps, including anything that needs a review or approval?', team: 'Who should be able to view, create, approve, or manage this work?', ready: 'Your workspace is ready to review.' }
const topics: Topic[] = ['business', 'goal', 'records', 'process', 'team', 'ready']
const stepLabels: Record<Topic, string> = { business: 'Understand your work', goal: 'Describe the outcome', records: 'Explore system ideas', process: 'Shape the workflow', team: 'Review your setup', ready: 'Review your setup' }

function WorkspacePreview({ state }: { state: State }) {
  const completed = state.progress.completed
  const active = state.progress.current
  const rows = [
    { name: 'New request', status: 'Needs review', tone: 'amber' },
    { name: 'Customer follow-up', status: 'In progress', tone: 'blue' },
    { name: 'Weekly stock check', status: 'Complete', tone: 'green' },
    { name: 'Supplier delivery', status: 'In progress', tone: 'blue' },
    { name: 'Team handover', status: 'Complete', tone: 'green' },
  ]
  return (
    <div className="w-full max-w-[900px] [perspective:1400px]" aria-label="Live workspace preview">
      <div className="relative mx-auto w-full overflow-hidden rounded-t-[18px] border-[5px] border-[#33343a] border-b-0 bg-[#101114] p-[7px] shadow-[0_28px_70px_rgba(0,0,0,0.48)] sm:rounded-t-[22px] sm:border-[7px] sm:p-[9px]">
        <div className="absolute left-1/2 top-0 z-10 h-1.5 w-16 -translate-x-1/2 rounded-b-full bg-[#34353a] sm:w-20" />
        <div className="flex h-[clamp(300px,49vh,570px)] min-h-[300px] overflow-hidden rounded-t-[11px] border border-white/10 bg-[#0b0c0e] text-left text-white sm:rounded-t-[14px]">
          <aside className="hidden w-[26%] min-w-[150px] border-r border-white/10 bg-[#101114] p-4 md:block">
            <div className="mb-6 flex items-center gap-2 text-sm font-semibold"><span className="grid h-7 w-7 place-items-center rounded-lg bg-[#34445b] text-blue-100"><Sparkles className="h-4 w-4" /></span>My workspace</div>
            <div className="space-y-1 text-[12px]">
              {(['business','goal','records','process','team'] as Topic[]).map((topic) => <div key={topic} className={`flex items-center gap-2 rounded-md px-2.5 py-2 ${active === topic ? 'bg-white/10 text-white' : 'text-white/55'}`}><span className={`h-1.5 w-1.5 rounded-full ${completed.includes(topic) ? 'bg-emerald-400' : active === topic ? 'bg-blue-400' : 'bg-white/20'}`} />{stepLabels[topic]}</div>)}
            </div>
            <div className="mt-7 border-t border-white/10 pt-4 text-[10px] font-medium uppercase tracking-[0.14em] text-white/40">Your systems</div>
            <div className="mt-3 rounded-md bg-white/[0.06] px-2.5 py-2 text-xs text-white/70">▦ Requests</div>
            <div className="mt-1 rounded-md px-2.5 py-2 text-xs text-white/45">▦ Team tasks</div>
          </aside>
          <div className="min-w-0 flex-1">
            <header className="flex h-11 items-center justify-between border-b border-white/10 px-4 sm:px-5"><span className="truncate text-xs font-medium text-white/75">{state.progress.current === 'ready' ? 'Workspace overview' : 'Workspace preview'}</span><span className="flex gap-1.5"><i className="h-2 w-2 rounded-full bg-red-400"/><i className="h-2 w-2 rounded-full bg-amber-400"/><i className="h-2 w-2 rounded-full bg-emerald-400"/></span></header>
            <div className="flex h-10 items-center gap-5 border-b border-white/10 px-4 text-[11px] text-white/55 sm:px-5"><span className="border-b border-blue-400 py-3 text-white/90">Requests</span><span>My work</span><span className="hidden sm:inline">Needs review</span><span className="ml-auto">Filter&nbsp;&nbsp; Sort</span></div>
            <div className="overflow-hidden">
              <div className="grid grid-cols-[1.3fr_1fr_0.8fr] border-b border-white/10 bg-white/[0.035] px-4 py-2.5 text-[10px] font-medium text-white/45 sm:px-5 sm:text-[11px]"><span>Name</span><span>Status</span><span className="text-right">Updated</span></div>
              {rows.map((row, index) => <div key={row.name} className="grid grid-cols-[1.3fr_1fr_0.8fr] items-center border-b border-white/[0.07] px-4 py-3 text-[10px] sm:px-5 sm:py-4 sm:text-xs"><span className="truncate pr-2 text-white/85">{index === 0 && completed.includes('records') ? 'Supplier request' : row.name}</span><span><span className={`inline-flex rounded-full px-2 py-1 text-[9px] sm:text-[10px] ${row.tone === 'amber' ? 'bg-amber-400/10 text-amber-200' : row.tone === 'green' ? 'bg-emerald-400/10 text-emerald-200' : 'bg-blue-400/10 text-blue-200'}`}>{row.status}</span></span><span className="text-right text-white/40">{index < 2 ? 'Today' : 'Yesterday'}</span></div>)}
            </div>
            <div className="px-4 py-4 text-[10px] text-white/40 sm:px-5 sm:text-xs">A clear place for your team’s work, ready to shape together.</div>
          </div>
        </div>
      </div>
      <div className="relative mx-auto h-[14px] w-[108%] -translate-x-[3.7%] rounded-b-[18px] border border-[#77777c] bg-gradient-to-b from-[#8b8b90] via-[#48484d] to-[#27272b] shadow-[0_14px_26px_rgba(0,0,0,.32)] sm:h-[18px] sm:rounded-b-[24px]">
        <div className="absolute left-[40%] top-0 h-[5px] w-[20%] rounded-b-md bg-[#b5b5b9]/60 sm:h-[7px]" />
      </div>
      <p className="mt-5 text-center text-xs text-muted-foreground">A preview of the workspace Kora is preparing</p>
    </div>
  )
}

export default function OnboardingPage() {
  const [state, setState] = useState<State | null>(null); const [text, setText] = useState(''); const [pending, setPending] = useState(false); const [error, setError] = useState(''); const [support, setSupport] = useState(false); const [supportText, setSupportText] = useState('')
  const load = async (id: string) => setState(await api.get<State>(`/api/cloud/conversations/${id}`))
  useEffect(() => { const id = readOnboardingConversationID(); if (id) void load(id).catch(() => setError('We couldn’t load your saved conversation. Try refreshing.')); else void api.post<Conversation>('/api/cloud/conversations', { channel: 'web', correlation_id: crypto.randomUUID() }).then((c) => { if (!saveOnboardingConversationID(c.id)) setError('This conversation is open, but browser storage is unavailable. Refreshing may start a new conversation.'); return load(c.id) }).catch(() => setError('We couldn’t start that conversation yet. Please try again.')) }, [])
  const send = async (event: FormEvent) => { event.preventDefault(); if (!state || !text.trim()) return; setPending(true); setError(''); try { await api.post(`/api/cloud/conversations/${state.conversation.id}/messages`, { text }); setText(''); await load(state.conversation.id) } catch { setError('We couldn’t save that answer yet. Your previous answers are safe. Try again.') } finally { setPending(false) } }
  const confirm = async () => { if (!state) return; setPending(true); try { await api.post(`/api/cloud/conversations/${state.conversation.id}/answers/${state.conversation.current_topic}/confirm`); await load(state.conversation.id) } catch { setError('We couldn’t save that confirmation yet. Try again.') } finally { setPending(false) } }
  const sendSupport = async () => { if (!state) return; setPending(true); try { await api.post(`/api/cloud/conversations/${state.conversation.id}/support-ticket`, { reason: supportText, confirm: true }); setSupport(false); setSupportText('') } catch { setError('We couldn’t send the support request yet. Try again.') } finally { setPending(false) } }
  if (error && !state) return <div className="mx-auto max-w-xl p-8"><Card><CardContent className="space-y-4"><p>{error}</p><Button onClick={() => window.location.reload()}>Try again</Button></CardContent></Card></div>
  if (!state) return <div className="flex min-h-svh items-center justify-center"><Loader2 className="animate-spin" aria-label="Loading conversation" /></div>
  const current = state.progress.current; const last = [...state.messages].reverse().find((m) => m.actor === 'assistant'); const completedCount = state.progress.completed.length
  return <main className="min-h-svh bg-background text-foreground lg:grid lg:grid-cols-[minmax(440px,0.88fr)_minmax(0,1.12fr)] xl:grid-cols-[minmax(500px,0.9fr)_minmax(0,1.1fr)]">
    <section className="flex min-h-svh flex-col px-5 py-6 sm:px-10 sm:py-9 xl:px-16">
      <header className="mx-auto flex w-full max-w-2xl items-center justify-between"><a href="/workspace" className="flex items-center gap-2 font-semibold tracking-tight"><span className="grid h-8 w-8 place-items-center rounded-lg bg-primary text-primary-foreground"><Sparkles className="h-4 w-4" /></span>Kora</a><span className="text-xs font-medium text-muted-foreground">Workspace setup · {Math.min(completedCount + 1, 4)} of 4</span></header>
      <div className="mx-auto flex w-full max-w-[560px] flex-1 flex-col pt-12 sm:pt-16 lg:pt-[clamp(48px,9vh,112px)]">
        <p className="text-sm font-medium text-primary">A little context</p><h1 className="mt-3 text-[clamp(28px,3vw,38px)] font-semibold leading-tight tracking-tight">Let’s build a workspace around your work.</h1><p className="mt-3 max-w-lg text-[15px] leading-6 text-muted-foreground">Kora’s AI will help shape a practical system with your team. You can review every suggestion before it’s created.</p>
        <nav aria-label="Setup progress" className="mt-8 grid grid-cols-4 gap-2">{(['business','goal','records','process'] as Topic[]).map((topic, index) => { const done = state.progress.completed.includes(topic); const active = current === topic; return <div key={topic} className="min-w-0"><div className={`mb-2 h-1 rounded-full ${done || active ? 'bg-primary' : 'bg-muted'}`} /><p className={`truncate text-[10px] sm:text-xs ${active ? 'font-medium text-foreground' : done ? 'text-foreground' : 'text-muted-foreground'}`}>{done && <Check className="mr-1 inline h-3 w-3" />}{stepLabels[topic]}</p><span className="sr-only">Step {index + 1}</span></div> })}</nav>
        <section aria-label="Kora setup conversation" className="mt-7 flex flex-1 flex-col">
          <div className="mb-4 flex items-center gap-2 text-xs font-medium text-muted-foreground"><Sparkles className="h-3.5 w-3.5 text-primary" />Kora AI <span className="text-muted-foreground/50">·</span> {stepLabels[current]}</div>
          <h2 className="text-xl font-semibold leading-7 tracking-tight">{current === 'ready' ? prompts.ready : prompts[current]}</h2>
          <div className="mt-4 max-h-[28vh] space-y-3 overflow-y-auto pr-1 sm:max-h-[32vh]" aria-live="polite">{state.messages.map((message) => <div key={message.id} className={`max-w-[95%] rounded-xl px-4 py-3 text-sm leading-6 ${message.actor === 'assistant' ? 'border bg-card text-card-foreground' : 'ml-auto bg-primary text-primary-foreground'}`}>{message.text}</div>)}</div>
          {last?.interpretation && current !== 'ready' && <div className="mt-4 rounded-xl border border-primary/20 bg-primary/[0.04] p-4 text-sm"><p className="font-medium">Here’s what I heard</p><p className="mt-1 leading-6 text-muted-foreground">{last.interpretation}</p><div className="mt-3 flex flex-wrap gap-2"><Button size="sm" onClick={() => void confirm()} disabled={pending}>That’s right</Button><Button size="sm" variant="outline" onClick={() => setText('Please change that: ')} disabled={pending}>Change this</Button><Button size="sm" variant="ghost" onClick={() => setSupport(true)}><HelpCircle className="h-4 w-4" />Get help</Button></div></div>}
          {current !== 'ready' && <form onSubmit={send} className="mt-4 flex items-end gap-2"><Textarea value={text} onChange={(e) => setText(e.target.value)} placeholder="Tell Kora in your own words…" aria-label={prompts[current]} rows={3} className="min-h-14 flex-1 resize-y" /><Button type="submit" disabled={pending || !text.trim()} aria-label="Send answer" className="h-11 w-11 shrink-0 active:scale-[0.97]"><Send className="h-4 w-4" /></Button></form>}
          {error && <p role="alert" className="mt-3 text-sm text-red-600">{error}</p>}
          <Button variant="ghost" size="sm" className="mt-3 w-fit text-muted-foreground" onClick={() => setSupport(true)}><HelpCircle className="h-4 w-4" />Need help? Contact Kora support.</Button>
          {support && <Card className="mt-4"><CardHeader><CardTitle>Ask Kora support</CardTitle></CardHeader><CardContent className="space-y-3"><p className="text-sm text-muted-foreground">We’ll include a short summary so you don’t need to repeat your answers.</p><Input value={supportText} onChange={(e) => setSupportText(e.target.value)} placeholder="What do you need help with?" /><div className="flex gap-2"><Button onClick={() => void sendSupport()} disabled={pending}>Send request</Button><Button variant="outline" onClick={() => setSupport(false)}>Cancel</Button></div></CardContent></Card>}
        </section>
        <p className="mt-8 border-t pt-4 text-center text-xs text-muted-foreground">Your setup stays in draft until you review and confirm it.</p>
      </div>
    </section>
    <aside className="hidden min-h-svh flex-col justify-center overflow-hidden border-l bg-muted/25 px-[clamp(24px,4vw,72px)] py-12 lg:flex"><div className="mx-auto w-full max-w-[960px]"><div className="mb-8"><p className="text-xs font-medium uppercase tracking-[0.16em] text-muted-foreground">Your setup</p><h2 className="mt-2 text-lg font-semibold tracking-tight">A workspace shaped around your work</h2><p className="mt-1 text-sm text-muted-foreground">See how your team’s work could come together.</p></div><WorkspacePreview state={state} /></div></aside>
    <section aria-label="Mobile workspace preview" className="border-t bg-muted/20 px-5 py-10 lg:hidden"><div className="mx-auto max-w-2xl"><div className="mb-6"><p className="text-xs font-medium uppercase tracking-[0.14em] text-muted-foreground">Your setup</p><h2 className="mt-2 text-lg font-semibold">A workspace shaped around your work</h2></div><WorkspacePreview state={state} /></div></section>
  </main>
}
