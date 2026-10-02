import type { Field } from '@/types/kora'
import { useEffect, useState } from 'react'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Textarea } from '@/components/ui/textarea'
import { Switch } from '@/components/ui/switch'
import { Label } from '@/components/ui/label'
import { LinkField } from './LinkField'
import { ChildTableEditor } from './ChildTableEditor'
import { AttachmentField } from './AttachmentField'
import { fetchUsers } from '@/lib/api/system'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { cn } from '@/lib/utils'
import {
  getFieldConstraintHint,
  isFieldRequired,
  isFieldVisible,
} from './form-runtime'

interface FieldRendererProps {
  field: Field
  value: any
  onChange: (fieldname: string, value: any) => void
  onRowsChange?: (fieldname: string, rows: Record<string, any>[]) => void
  disabled: boolean
  error?: string
  compact?: boolean
  formData?: Record<string, any>
}

export function FieldRenderer({
  field,
  value,
  onChange,
  onRowsChange,
  disabled,
  error,
  compact,
  formData = {},
}: FieldRendererProps) {
  const fieldname = field.fieldname
  const required = isFieldRequired(field, formData)
  const label = field.label + (required ? ' *' : '')
  const id = `field-${fieldname}`
  const labelClass = cn(field.bold && 'font-semibold', compact && 'text-xs')
  const gapClass = compact ? 'space-y-0.5' : 'space-y-1.5'
  const hint = error || getFieldConstraintHint(field, value, formData)

  if (!isFieldVisible(field, formData)) {
    return null
  }

  switch (field.fieldtype) {
    case 'User':
      return <UserField field={field} value={value} onChange={onChange} disabled={disabled} error={hint || undefined} compact={compact} />
    case 'Data': {
      const type =
        field.options === 'Email' ? 'email' :
        field.options === 'Phone' ? 'tel' :
        field.options === 'URL' ? 'url' : 'text'
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          {field.read_only ? (
            <div className="flex min-h-[2.5rem] items-center rounded-md border border-transparent bg-muted/30 px-3 py-2 text-sm">
              {value != null && value !== '' ? (
                field.options === 'URL' ? (
                  <a href={String(value)} target="_blank" rel="noreferrer" className="truncate text-primary underline underline-offset-2">
                    {String(value)}
                  </a>
                ) : String(value)
              ) : <span className="text-muted-foreground">—</span>}
            </div>
          ) : (
            <Input
              id={id}
              type={type}
              value={value ?? ''}
              onChange={(e) => onChange(fieldname, e.target.value)}
              disabled={disabled}
              placeholder={field.description || field.label}
            />
          )}
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )
    }

    case 'Text':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          {field.read_only ? (
            <div className="min-h-[2.5rem] rounded-md border border-transparent bg-muted/30 px-3 py-2 text-sm">
              {value != null && value !== '' ? String(value) : <span className="text-muted-foreground">—</span>}
            </div>
          ) : (
            <Textarea
              id={id}
              value={value ?? ''}
              onChange={(e) => onChange(fieldname, e.target.value)}
              disabled={disabled}
              placeholder={field.description || field.label}
              rows={4}
            />
          )}
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Int':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          {field.read_only ? (
            <div className="flex min-h-[2.5rem] items-center rounded-md border border-transparent bg-muted/30 px-3 py-2 text-sm">
              {value != null && value !== '' ? String(value) : <span className="text-muted-foreground">—</span>}
            </div>
          ) : (
            <Input
              id={id}
              type="number"
              step="1"
              className="[&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none [-moz-appearance:textfield]"
              value={value ?? ''}
              onChange={(e) => onChange(fieldname, e.target.value === '' ? null : parseInt(e.target.value, 10))}
              disabled={disabled}
            />
          )}
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Float':
    case 'Currency':
    case 'Percent': {
      const displayValue = value != null && value !== '' ? Number(value).toFixed(2) : ''
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          {field.read_only ? (
            <div className="flex min-h-[2.5rem] items-center rounded-md border border-transparent bg-muted/30 px-3 py-2 text-sm">
              {value != null && value !== '' ? String(value) : <span className="text-muted-foreground">—</span>}
            </div>
          ) : (
            <Input
              id={id}
              type="number"
              step="any"
              className="[&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none [-moz-appearance:textfield]"
              value={displayValue}
              onChange={(e) => onChange(fieldname, e.target.value === '' ? null : parseFloat(e.target.value))}
              disabled={disabled}
            />
          )}
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )
    }

    case 'Check':
      return (
        <div className="space-y-1.5">
          <div className="flex items-center gap-3">
            <Switch
              id={id}
              checked={!!value}
              onCheckedChange={(checked) => onChange(fieldname, checked)}
              disabled={disabled || field.read_only}
            />
            <Label htmlFor={id} className={labelClass}>{label}</Label>
          </div>
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Select': {
      const options = field.options
        ? field.options.split('\n').filter((o) => o.trim() !== '')
        : []
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Select
            value={value ?? ''}
            onValueChange={(v) => onChange(fieldname, v === '__empty__' ? '' : v)}
            disabled={disabled || field.read_only}
          >
            <SelectTrigger id={id}>
              <SelectValue placeholder={`Select ${field.label}...`} />
            </SelectTrigger>
            <SelectContent>
              {!required && <SelectItem value="__empty__">None</SelectItem>}
              {options.map((opt) => (
                <SelectItem key={opt} value={opt.trim()}>
                  {opt.trim()}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )
    }

    case 'Date':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="date"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Datetime':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="datetime-local"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Time':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="time"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Password':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <PasswordInput
            id={id}
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
            placeholder="••••••••"
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'JSON':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Textarea
            id={id}
            value={typeof value === 'string' ? value : JSON.stringify(value ?? {}, null, 2)}
            onChange={(e) => {
              try {
                onChange(fieldname, JSON.parse(e.target.value))
              } catch {
                onChange(fieldname, e.target.value)
              }
            }}
            disabled={disabled || field.read_only}
            rows={6}
            className="font-mono text-xs"
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
        </div>
      )

    case 'Link':
    case 'Dynamic Link':
      return (
        <LinkField
          field={field}
          value={value}
          onChange={onChange}
          disabled={disabled || field.read_only}
          error={hint || undefined}
          compact={compact}
        />
      )

    case 'Attach':
    case 'Attach Image':
    case 'Attach Audio':
      return (
        <AttachmentField
          field={field}
          value={value}
          onChange={onChange}
          disabled={disabled || field.read_only}
          error={hint || undefined}
          label={label}
          id={id}
          labelClass={labelClass}
          gapClass={gapClass}
        />
      )

    case 'Section Break':
      return (
        <div className="pb-2 pt-4">
          <h3 className="border-b pb-1 text-lg font-semibold">{field.label || 'Section'}</h3>
        </div>
      )

    case 'Column Break':
      return <div className="w-4" />

    case 'Heading':
      return <h4 className="pt-3 text-base font-semibold text-muted-foreground">{field.label}</h4>

    case 'Table':
      return (
        <ChildTableEditor
          field={field}
          value={Array.isArray(value) ? value : []}
          onChange={onChange}
          onRowsChange={onRowsChange}
          disabled={disabled}
        />
      )

    default:
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="text"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {hint && <p className="mt-1 text-sm text-destructive">{hint}</p>}
          <p className="text-xs text-muted-foreground">Type: {field.fieldtype}</p>
        </div>
      )
  }
}

function UserField({ field, value, onChange, disabled, error, compact }: FieldRendererProps) {
  const [users, setUsers] = useState<Array<{ name: string; full_name?: string; email?: string }> | null>(null)
  useEffect(() => { fetchUsers().then(setUsers).catch(() => setUsers([])) }, [])
  return <div className={compact ? 'space-y-0.5' : 'space-y-1.5'}>
    <Label htmlFor={`field-${field.fieldname}`}>{field.label}{field.reqd && <span className="ml-1 text-destructive">*</span>}</Label>
    <select id={`field-${field.fieldname}`} className="uk-select" value={value ?? ''} onChange={(event) => onChange(field.fieldname, event.target.value)} disabled={disabled}>
      <option value="">Choose a user</option>
      {(users ?? []).map((user) => <option key={user.name} value={user.name}>{user.full_name || user.email || user.name}</option>)}
    </select>
    {error && <p className="mt-1 text-sm text-destructive">{error}</p>}
  </div>
}
