import { Link } from '@tanstack/react-router'
import { ChevronRight, Home } from 'lucide-react'
import { cn } from '@/lib/utils'

interface Crumb {
  label: string
  to?: string
}

export function Breadcrumbs({ items, className }: { items: Crumb[]; className?: string }) {
  return (
    <nav className={cn('flex items-center gap-1 text-sm text-muted-foreground flex-wrap', className)}>
      <Link to="/workspace" className="hover:text-foreground transition-colors flex items-center gap-1">
        <Home className="h-3.5 w-3.5" />
      </Link>
      {items.map((item, i) => (
        <span key={i} className="flex items-center gap-1">
          <ChevronRight className="h-3.5 w-3.5 shrink-0" />
          {item.to ? (
            <Link to={item.to as any} className="hover:text-foreground transition-colors truncate max-w-[150px]">
              {item.label}
            </Link>
          ) : (
            <span className="text-foreground font-medium truncate max-w-[150px]">{item.label}</span>
          )}
        </span>
      ))}
    </nav>
  )
}
