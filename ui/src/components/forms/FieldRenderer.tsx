import type { Field, DocType } from '@/types/kora'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { Switch } from '@/components/ui/switch'
import { Label } from '@/components/ui/label'
import { LinkField } from './LinkField'
import { ChildTableEditor } from './ChildTableEditor'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { cn } from '@/lib/utils'

interface FieldRendererProps {
  field: Field
  value: any
  onChange: (fieldname: string, value: any) => void
  onRowsChange?: (rows: Record<string, any>[]) => void
  disabled: boolean
  error?: string
  compact?: boolean
}

export function FieldRenderer({ field, value, onChange, onRowsChange, disabled, error, compact }: FieldRendererProps) {
  const fieldname = field.fieldname
  const label = field.label + (field.reqd ? ' *' : '')
  const id = `field-${fieldname}`
  const labelClass = cn(field.bold && 'font-semibold', compact && 'text-xs')
  const gapClass = compact ? 'space-y-0.5' : 'space-y-1.5'

  switch (field.fieldtype) {
    // --- Text inputs ---
    case 'Data': {
      const type =
        field.options === 'Email' ? 'email' :
        field.options === 'Phone' ? 'tel' :
        field.options === 'URL' ? 'url' : 'text'
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type={type}
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
            placeholder={field.description || field.label}
          />
        </div>
      )
    }

    case 'Text':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Textarea
            id={id}
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
            placeholder={field.description || field.label}
            rows={4}
          />
        </div>
      )

    // --- Numbers ---
    case 'Int':
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="number"
            step="1"
            className="[&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none [-moz-appearance:textfield]"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value === '' ? null : parseInt(e.target.value))}
            disabled={disabled || field.read_only}
          />
          {error && <p className="text-sm text-destructive mt-1">{error}</p>}
        </div>
      )

    case 'Float':
    case 'Currency':
    case 'Percent': {
      const displayValue = value != null && value !== '' ? Number(value).toFixed(2) : ''
      return (
        <div className={gapClass}>
          <Label htmlFor={id} className={labelClass}>{label}</Label>
          <Input
            id={id}
            type="number"
            step="any"
            className="[&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none [-moz-appearance:textfield]"
            value={displayValue}
            onChange={(e) => onChange(fieldname, e.target.value === '' ? null : parseFloat(e.target.value))}
            disabled={disabled || field.read_only}
          />
          {error && <p className="text-sm text-destructive mt-1">{error}</p>}
        </div>
      )}

    // --- Boolean ---
    case 'Check':
      return (
        <div className="flex items-center gap-3 space-y-0">
          <Switch
            id={id}
            checked={!!value}
            onCheckedChange={(checked) => onChange(fieldname, checked)}
            disabled={disabled || field.read_only}
          />
          <Label htmlFor={id}>{label}</Label>
        </div>
      )

    // --- Select ---
    case 'Select': {
      const options = field.options
        ? field.options.split('\n').filter((o) => o.trim() !== '')
        : []
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Select
            value={value ?? ''}
            onValueChange={(v) => onChange(fieldname, v)}
            disabled={disabled || field.read_only}
          >
            <SelectTrigger id={id}>
              <SelectValue placeholder={`Select ${field.label}...`} />
            </SelectTrigger>
            <SelectContent>
              {options.map((opt) => (
                <SelectItem key={opt} value={opt.trim()}>
                  {opt.trim()}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )
    }

    // --- Date / Time ---
    case 'Date':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="date"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {error && <p className="text-sm text-destructive mt-1">{error}</p>}
        </div>
      )

    case 'Datetime':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="datetime-local"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {error && <p className="text-sm text-destructive mt-1">{error}</p>}
        </div>
      )

    case 'Time':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="time"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          {error && <p className="text-sm text-destructive mt-1">{error}</p>}
        </div>
      )

    // --- Password ---
    case 'Password':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="password"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
            placeholder="••••••••"
          />
        </div>
      )

    // --- JSON ---
    case 'JSON':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Textarea
            id={id}
            value={typeof value === 'string' ? value : JSON.stringify(value ?? {}, null, 2)}
            onChange={(e) => {
              try { onChange(fieldname, JSON.parse(e.target.value)) }
              catch { onChange(fieldname, e.target.value) }
            }}
            disabled={disabled || field.read_only}
            rows={6}
            className="font-mono text-xs"
          />
        </div>
      )

    // --- Link (searchable autocomplete) ---
    case 'Link':
    case 'Dynamic Link':
      return (
        <LinkField
          field={field}
          value={value}
          onChange={onChange}
          disabled={disabled || field.read_only}
          error={error}
          compact={compact}
        />
      )

    // --- Attach (placeholder) ---
    case 'Attach':
    case 'Attach Image':
      return (
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="text"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
            placeholder="File path or URL"
          />
        </div>
      )

    // --- Layout fields — not rendered as inputs ---
    case 'Section Break':
      return (
        <div className="pt-4 pb-2">
          <h3 className="text-lg font-semibold border-b pb-1">{field.label || 'Section'}</h3>
        </div>
      )

    case 'Column Break':
      return <div className="w-4" /> // handled by layout engine

    case 'Heading':
      return <h4 className="text-base font-semibold text-muted-foreground pt-3">{field.label}</h4>

    // --- Table (child table) — inline grid editor ---
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
        <div className="space-y-1.5">
          <Label htmlFor={id}>{label}</Label>
          <Input
            id={id}
            type="text"
            value={value ?? ''}
            onChange={(e) => onChange(fieldname, e.target.value)}
            disabled={disabled || field.read_only}
          />
          <p className="text-xs text-muted-foreground">Type: {field.fieldtype}</p>
        </div>
      )
  }
}
