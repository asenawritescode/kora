import { cn } from '@/lib/utils'

interface ProgressBarProps {
  filled: number
  total: number
  className?: string
}

export function ProgressBar({ filled, total, className }: ProgressBarProps) {
  if (total === 0) return null

  const pct = Math.round((filled / total) * 100)
  const color = pct >= 80 ? 'bg-green-500' : pct >= 50 ? 'bg-amber-500' : 'bg-red-500'

  return (
    <div className={cn('space-y-1', className)}>
      <div className="flex items-center justify-between text-xs">
        <span className="text-muted-foreground">
          {filled}/{total} required fields
        </span>
        <span className={cn('font-medium',
          pct >= 80 ? 'text-green-600' : pct >= 50 ? 'text-amber-600' : 'text-red-600'
        )}>
          {pct}%
        </span>
      </div>
      <div className="h-1.5 bg-muted rounded-full overflow-hidden">
        <div
          className={cn('h-full rounded-full transition-all duration-500', color)}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}
