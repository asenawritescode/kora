import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchSecrets, setSecret, deleteSecret } from '@/lib/api/system'
import type { SecretEntry } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { KeyRound, Plus, Pencil, Trash2, Loader2, AlertCircle, Sparkles } from 'lucide-react'

const AI_PROVIDERS = [
  { key: 'openai_api_key', label: 'OpenAI', description: 'Used for GPT-4o, GPT-4.1, etc.' },
  { key: 'deepseek_api_key', label: 'DeepSeek', description: 'Used for DeepSeek V4 Pro' },
  { key: 'anthropic_api_key', label: 'Anthropic', description: 'Used for Claude Sonnet, Opus, Haiku' },
]

export default function AdminSecretsPage() {
  const queryClient = useQueryClient()

  const { data: secrets, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['admin', 'secrets'],
    queryFn: fetchSecrets,
  })

  const existingKeys = new Set(secrets?.map((s) => s.key_name) || [])

  // Dialog state.
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editKey, setEditKey] = useState('')
  const [secretValue, setSecretValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  // Delete state.
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)

  // AI provider quick-set state.
  const [selectedProvider, setSelectedProvider] = useState('')
  const [providerKeyValue, setProviderKeyValue] = useState('')
  const [providerSaving, setProviderSaving] = useState(false)
  const [providerError, setProviderError] = useState('')

  const openAdd = (prefillKey?: string) => {
    setEditKey('')
    setSecretValue('')
    setFormError('')
    if (prefillKey) setEditKey(prefillKey)
    setDialogOpen(true)
  }

  const openEdit = (s: SecretEntry) => {
    setEditKey(s.key_name)
    setSecretValue('')
    setFormError('')
    setDialogOpen(true)
  }

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setFormError('')

    if (!editKey.trim()) {
      setFormError('Key name is required.')
      return
    }
    if (!secretValue) {
      setFormError('Value is required.')
      return
    }

    setSaving(true)
    try {
      await setSecret({ key: editKey.trim(), value: secretValue })
      setDialogOpen(false)
      queryClient.invalidateQueries({ queryKey: ['admin', 'secrets'] })
    } catch (err: any) {
      setFormError(err.message || 'Failed to save secret')
    }
    setSaving(false)
  }

  const handleDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await deleteSecret(deleteTarget)
      setDeleteOpen(false)
      queryClient.invalidateQueries({ queryKey: ['admin', 'secrets'] })
    } catch (err: any) {
      setDeleting(false)
    }
  }

  const handleSaveProvider = async () => {
    if (!selectedProvider || !providerKeyValue.trim()) return
    setProviderError('')
    setProviderSaving(true)
    try {
      await setSecret({ key: selectedProvider, value: providerKeyValue.trim() })
      setProviderKeyValue('')
      queryClient.invalidateQueries({ queryKey: ['admin', 'secrets'] })
    } catch (err: any) {
      setProviderError(err.message || 'Failed to save API key')
    }
    setProviderSaving(false)
  }

  const formatDate = (d: string) => {
    if (!d) return '—'
    return new Date(d).toLocaleString()
  }

  return (
    <div className="p-8 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Secrets</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage API keys and configuration secrets</p>
        </div>
        <Button onClick={() => openAdd()} size="sm">
          <Plus className="h-4 w-4 mr-1" />
          Add Secret
        </Button>
      </div>

      <div className="rounded-lg border border-blue-200 bg-blue-50 p-3 text-sm text-blue-800">
        <p className="mb-0.5 text-xs font-medium uppercase tracking-wide">AI key naming convention</p>
        <p>
          AI providers require exact key names:{' '}
          <code className="rounded bg-blue-100 px-1 font-mono text-xs">openai_api_key</code>,{' '}
          <code className="rounded bg-blue-100 px-1 font-mono text-xs">deepseek_api_key</code>, or{' '}
          <code className="rounded bg-blue-100 px-1 font-mono text-xs">anthropic_api_key</code>.
          Use the quick-configure card above — it sets the correct name automatically.
        </p>
      </div>

      {/* AI Provider — single dropdown + key input */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <Sparkles className="h-4 w-4" />
            AI Provider
          </CardTitle>
          <CardDescription>Select your AI provider and enter the API key. Only one provider is active at a time.</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex flex-col sm:flex-row gap-3">
            <Select value={selectedProvider} onValueChange={(v) => { setSelectedProvider(v || ''); setProviderKeyValue(''); setProviderError('') }}>
              <SelectTrigger className="sm:w-[200px]">
                <SelectValue placeholder="Select provider..." />
              </SelectTrigger>
              <SelectContent>
                {AI_PROVIDERS.map((p) => {
                  const configured = existingKeys.has(p.key)
                  return (
                    <SelectItem key={p.key} value={p.key}>
                      <span className="flex items-center gap-2">
                        {p.label}
                        {configured && <Badge variant="secondary" className="text-[10px] px-1.5 py-0 leading-normal">Configured</Badge>}
                      </span>
                    </SelectItem>
                  )
                })}
              </SelectContent>
            </Select>
            {selectedProvider && (
              <>
                <Input
                  type="password"
                  value={providerKeyValue}
                  onChange={(e) => setProviderKeyValue(e.target.value)}
                  placeholder={`Enter ${AI_PROVIDERS.find(p => p.key === selectedProvider)?.label} API key...`}
                  className="flex-1"
                />
                <Button onClick={handleSaveProvider} disabled={providerSaving || !providerKeyValue.trim()}>
                  {providerSaving ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
                  Save
                </Button>
              </>
            )}
          </div>
          {providerError && (
            <p className="text-sm text-destructive bg-destructive/10 rounded-md px-3 py-2 mt-3">{providerError}</p>
          )}
        </CardContent>
      </Card>

      {/* Secrets Table */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <KeyRound className="h-4 w-4" />
            All Secrets
          </CardTitle>
          <CardDescription>{secrets?.length || 0} secret(s) configured</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3">
              {[1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : isError ? (
            <div className="flex flex-col items-center gap-3 py-8 text-center border-2 border-dashed rounded-lg">
              <AlertCircle className="h-8 w-8 text-destructive" />
              <p className="text-sm text-destructive">{(error as Error)?.message || 'Failed to load secrets'}</p>
              <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
            </div>
          ) : !secrets || secrets.length === 0 ? (
            <div className="flex flex-col items-center gap-3 py-10 text-center border-2 border-dashed rounded-lg">
              <KeyRound className="h-10 w-10 text-muted-foreground/50" />
              <div>
                <p className="text-sm font-medium">No secrets configured</p>
                <p className="text-xs text-muted-foreground mt-1">Add an AI provider key to enable AI chat, or add custom configuration secrets.</p>
              </div>
              <Button variant="outline" size="sm" onClick={() => openAdd()}>
                <Plus className="h-4 w-4 mr-1" /> Add Secret
              </Button>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Key Name</TableHead>
                  <TableHead>Value</TableHead>
                  <TableHead>Last Updated</TableHead>
                  <TableHead className="w-[100px]">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {secrets.map((s) => (
                  <TableRow key={s.key_name}>
                    <TableCell className="font-mono text-xs">{s.key_name}</TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground tracking-widest select-none">
                        {'●'.repeat(16)}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDate(s.updated_at)}</TableCell>
                    <TableCell>
                      <div className="flex items-center gap-1">
                        <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => openEdit(s)} title="Update">
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-destructive hover:text-destructive" onClick={() => { setDeleteTarget(s.key_name); setDeleteOpen(true) }} title="Delete">
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Add / Edit Dialog */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent>
          <form onSubmit={handleSave}>
            <DialogHeader>
              <DialogTitle>{editKey && existingKeys.has(editKey) ? 'Update Secret' : 'Add Secret'}</DialogTitle>
              <DialogDescription>
                Secret values are encrypted at rest using AES-256-GCM. Values are never returned by the API.
              </DialogDescription>
            </DialogHeader>

            <div className="space-y-4 mt-4">
              <div className="space-y-2">
                <Label htmlFor="key">Key Name *</Label>
                <Input
                  id="key"
                  value={editKey}
                  onChange={(e) => setEditKey(e.target.value)}
                  placeholder="e.g. openai_api_key"
                  disabled={!!editKey && existingKeys.has(editKey)}
                  required
                />
                <p className="text-[11px] text-muted-foreground">
                  AI providers require exact key names:{' '}
                  <code className="font-mono">openai_api_key</code>, <code className="font-mono">deepseek_api_key</code>, or{' '}
                  <code className="font-mono">anthropic_api_key</code>.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="value">Value *</Label>
                <Input
                  id="value"
                  type="password"
                  value={secretValue}
                  onChange={(e) => setSecretValue(e.target.value)}
                  placeholder={editKey && existingKeys.has(editKey) ? 'Enter new value (overwrites existing)' : 'Enter secret value'}
                  required
                />
              </div>
              {formError && (
                <p className="text-sm text-destructive bg-destructive/10 rounded-md px-3 py-2">{formError}</p>
              )}
            </div>

            <DialogFooter className="mt-6">
              <Button type="button" variant="outline" onClick={() => setDialogOpen(false)}>Cancel</Button>
              <Button type="submit" disabled={saving}>
                {saving ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
                Save Secret
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete Confirmation */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete Secret</DialogTitle>
            <DialogDescription>
              Are you sure you want to delete <strong className="font-mono">{deleteTarget}</strong>?
              Any functionality depending on this secret will stop working.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter className="mt-4">
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" onClick={handleDelete} disabled={deleting}>
              {deleting ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
              Delete Secret
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
