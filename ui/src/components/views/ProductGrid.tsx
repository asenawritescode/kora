import { useState } from 'react'
import type { ViewComponentProps } from './registry'
import { Loader2, Plus } from 'lucide-react'
import { Badge } from '@/components/ui/badge'

/**
 * ProductGrid renders tappable product tiles with image, name, price, and
 * stock badge. Tapping a product fires a 'tap' action for adding to cart.
 */
export default function ProductGrid(props: ViewComponentProps) {
  const { data, isLoading, config, onAction } = props

  const titleField = config.bindings?.title
  const priceField = config.bindings?.price
  const badgeField = config.bindings?.badge
  const imageField = config.bindings?.image
  const skuField = config.bindings?.sku
  const subtitleField = config.bindings?.subtitle

  const products = data?.data || []

  if (!titleField || !priceField) {
    return (
      <div className="rounded-lg border p-4 text-sm text-muted-foreground">
        Configure title and price field bindings for this product grid.
      </div>
    )
  }

  if (isLoading) {
    return (
      <div className="flex items-center justify-center p-8">
        <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
      </div>
    )
  }

  if (products.length === 0) {
    return (
      <div className="rounded-lg border p-8 text-center text-sm text-muted-foreground">
        No products found
      </div>
    )
  }

  return (
    <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5">
      {products.map((product: any) => (
        <button
          key={product.name}
          className="flex flex-col items-center rounded-lg border bg-card p-3 text-center hover:bg-muted/30 hover:shadow-sm transition-all active:scale-95"
          onClick={() => onAction('tap', {
            name: product.name,
            product: product.name,
            product_name: product[titleField],
            rate: parseFloat(product[priceField]) || 0,
            quantity: 1,
            amount: parseFloat(product[priceField]) || 0,
            row: product,
          })}
        >
          {imageField && product[imageField] ? (
            <img src={product[imageField]} alt={product[titleField]} className="h-16 w-16 object-cover rounded-md mb-2" />
          ) : (
            <div className="h-16 w-16 rounded-md bg-muted flex items-center justify-center mb-2">
              <Plus className="h-6 w-6 text-muted-foreground" />
            </div>
          )}
          <span className="text-xs font-medium line-clamp-2">{product[titleField] || product.name}</span>
          {skuField && product[skuField] && (
            <span className="text-[10px] text-muted-foreground">{product[skuField]}</span>
          )}
          {subtitleField && product[subtitleField] && (
            <span className="text-[10px] text-muted-foreground">{product[subtitleField]}</span>
          )}
          <span className="text-sm font-bold mt-1">
            {product[priceField] != null ? Number(product[priceField]).toLocaleString() : '—'}
          </span>
          {badgeField && product[badgeField] != null && (
            <Badge variant={product[badgeField] < 10 ? 'destructive' : 'secondary'} className="mt-1 text-[10px]">
              {product[badgeField]} in stock
            </Badge>
          )}
        </button>
      ))}
    </div>
  )
}
