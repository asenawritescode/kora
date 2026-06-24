import { useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { cn } from '@/lib/utils'

interface FormSectionProps {
  title: string
  children: React.ReactNode
  defaultOpen?: boolean
  badge?: string
}

export function FormSection({ title, children, defaultOpen = true, badge }: FormSectionProps) {
  const [open, setOpen] = useState(defaultOpen)

  return (
    <div className="border rounded-lg overflow-hidden">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        className="w-full flex items-center gap-2 px-4 py-2.5 bg-muted/30 hover:bg-muted/50 transition-colors text-left"
      >
        {open ? <ChevronDown className="h-4 w-4 shrink-0" /> : <ChevronRight className="h-4 w-4 shrink-0" />}
        <span className="text-sm font-semibold">{title}</span>
        {badge && (
          <span className="ml-auto text-xs text-muted-foreground">{badge}</span>
        )}
      </button>
      {open && (
        <div className="p-4 space-y-4">
          {children}
        </div>
      )}
    </div>
  )
}

export function isLayoutField(fieldtype: string): boolean {
  return fieldtype === 'Section Break' || fieldtype === 'Column Break' || fieldtype === 'Heading'
}
