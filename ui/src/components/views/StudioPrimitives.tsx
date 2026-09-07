import { useEffect, useRef } from 'react'
import type { ButtonHTMLAttributes, HTMLAttributes, InputHTMLAttributes, LabelHTMLAttributes, ReactNode, Ref, TextareaHTMLAttributes } from 'react'
import { cn } from '@/lib/utils'

export function Badge({ className, variant = 'default', ...props }: HTMLAttributes<HTMLSpanElement> & { variant?: 'default' | 'secondary' | 'outline' | 'destructive' }) {
  const variantClass = variant === 'destructive' ? 'uk-label-danger' : variant === 'outline' ? 'uk-label-warning' : 'uk-label'
  return <span className={cn(variantClass, className)} {...props} />
}

export function Card({ className, size = 'default', ...props }: HTMLAttributes<HTMLDivElement> & { size?: 'default' | 'sm' }) {
  return <section data-size={size} className={cn('uk-card uk-card-default', size === 'sm' && 'uk-card-small', className)} {...props} />
}

export function CardHeader({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('uk-card-header', className)} {...props} />
}

export function CardContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('uk-card-body', className)} {...props} />
}

export function CardTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h3 className={cn('uk-card-title', className)} {...props} />
}

export function CardDescription({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn('uk-text-meta', className)} {...props} />
}

export function CardAction({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('uk-card-badge', className)} {...props} />
}

export function CardFooter({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('uk-card-footer uk-flex uk-flex-right uk-flex-middle uk-grid-small', className)} {...props} />
}

export function Button({ className, variant = 'default', size = 'default', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { ref?: Ref<HTMLButtonElement>; variant?: 'default' | 'secondary' | 'outline' | 'ghost' | 'destructive'; size?: 'default' | 'sm' | 'icon' | 'icon-lg' }) {
  const variantClass = variant === 'destructive'
    ? 'uk-button-danger bg-red-600 text-white hover:bg-red-500'
    : variant === 'outline' || variant === 'ghost'
      ? 'uk-button-default border border-border bg-transparent text-foreground hover:bg-muted'
      : 'uk-button-default bg-slate-950 text-white hover:bg-slate-800 dark:bg-slate-100 dark:text-slate-950 dark:hover:bg-white'
  const sizeClass = size === 'icon' ? 'h-8 w-8 px-0' : size === 'icon-lg' ? 'h-11 w-11 px-0' : size === 'sm' ? 'min-h-8 px-2.5' : 'min-h-9 px-3'
  return <button className={cn('uk-button inline-flex items-center justify-center gap-2 rounded-md py-1.5 text-sm font-medium transition-colors disabled:pointer-events-none disabled:opacity-50', 'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-orange-400', sizeClass, variantClass, className)} {...props} />
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn('uk-textarea', className)} {...props} />
}

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement> & { ref?: Ref<HTMLInputElement> }) {
  return <input className={cn('uk-input', 'focus-visible:outline focus-visible:outline-2 focus-visible:outline-orange-400', className)} {...props} />
}

export function Label({ className, ...props }: LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn('uk-form-label', className)} {...props} />
}

export function FieldGroup({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('flex flex-col gap-6', className)} {...props} />
}

export function Field({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('flex flex-col gap-2', className)} {...props} />
}

export function FieldDescription({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn('uk-text-meta', className)} {...props} />
}

export function FieldSeparator({ className, children, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('uk-flex uk-flex-middle uk-grid-small uk-text-meta', className)} {...props}><span className="uk-flex-1 uk-border-bottom" />{children && <span>{children}</span>}<span className="uk-flex-1 uk-border-bottom" /></div>
}

export function ConfirmDialog({ open, onOpenChange, title, description, confirmLabel = 'Confirm', confirmVariant = 'default', onConfirm }: { open: boolean; onOpenChange: (open: boolean) => void; title: string; description: ReactNode; confirmLabel?: string; confirmVariant?: 'default' | 'destructive'; onConfirm: () => void | Promise<void> }) {
  const cancelRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    if (!open) return
    cancelRef.current?.focus()
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onOpenChange(false)
    }
    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [open, onOpenChange])

  if (!open) return null
  return (
    <div className="uk-modal uk-open" role="presentation" onClick={() => onOpenChange(false)}>
      <div className="uk-modal-dialog uk-modal-body" role="dialog" aria-modal="true" aria-labelledby="studio-confirm-title" aria-describedby="studio-confirm-description" onClick={(event) => event.stopPropagation()}>
        <h2 id="studio-confirm-title" className="uk-modal-title">{title}</h2>
        <p id="studio-confirm-description">{description}</p>
        <div className="uk-flex uk-flex-right uk-flex-middle uk-grid-small" data-uk-grid>
          <Button ref={cancelRef} variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant={confirmVariant} onClick={() => void onConfirm()}>{confirmLabel}</Button>
        </div>
      </div>
    </div>
  )
}
