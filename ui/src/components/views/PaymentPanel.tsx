import { useState, type ReactNode } from 'react'
import type { ViewComponentProps } from './registry'
import { Button } from '@/components/ui/button'
import { useCartStore } from '@/lib/cart-store'
import { Banknote, CreditCard, Smartphone, Loader2, Unlock } from 'lucide-react'

const PAYMENT_ICONS: Record<string, ReactNode> = {
  cash: <Banknote className="h-5 w-5" />,
  card: <CreditCard className="h-5 w-5" />,
  mobile_money: <Smartphone className="h-5 w-5" />,
  mobile: <Smartphone className="h-5 w-5" />,
  mpesa: <Smartphone className="h-5 w-5" />,
}

export default function PaymentPanel(props: ViewComponentProps) {
  const { config, onAction } = props
  const [processing, setProcessing] = useState(false)
  const [selectedMethod, setSelectedMethod] = useState<string | null>(null)
  const [phoneNumber, setPhoneNumber] = useState('')
  const [operationId, setOperationId] = useState('')
  const [paymentStatus, setPaymentStatus] = useState('')
  const [phoneError, setPhoneError] = useState('')
  const [openingCash, setOpeningCash] = useState('0')
  const [activeShift, setActiveShift] = useState(() => sessionStorage.getItem('kora-pos-active-shift') || '')
  const [activeTillSession, setActiveTillSession] = useState(() => sessionStorage.getItem('kora-pos-active-till-session') || '')
  const { items, total, clearCart } = useCartStore()
  const cartTotal = total()

  const methods = (config.bindings?.methods || '')
    .split(',').map((s) => s.trim()).filter(Boolean)
  const register = config.bindings?.register || ''
  const supportsOnlineMobilePayment = Boolean(config.actions?.some((item) => item.type === 'initiate_external_operation'))
  const hasOpenShiftAction = Boolean(config.actions?.some((item) => item.id === 'open_shift'))
  const hasOpenTillAction = Boolean(config.actions?.some((item) => item.id === 'open_till_session'))
  const requiresTill = hasOpenShiftAction || hasOpenTillAction
  const tillReady = !requiresTill || (hasOpenTillAction ? Boolean(activeTillSession) : Boolean(activeShift))

  const transactionContext = (method: string) => ({
    reference: `POS-${Date.now()}`,
    cart: items,
    customer: config.bindings?.customer || '',
    invoice_date: config.bindings?.invoice_date || new Date().toISOString().slice(0, 10),
    due_date: config.bindings?.due_date || new Date().toISOString().slice(0, 10),
    customer_name: config.bindings?.customer_name || '',
    register,
    ...(activeShift ? { shift: activeShift } : {}),
    ...(activeTillSession ? { till_session: activeTillSession } : {}),
    ...(isMobilePayment(method) ? { customer_phone: phoneNumber.trim() } : {}),
    payment_status: 'Paid',
    payment_method: normalizePaymentMethod(method),
    status: 'Paid',
    total: cartTotal,
  })

  const openShiftAndTill = async () => {
    setProcessing(true)
    try {
      let shiftName = activeShift
      if (!shiftName) {
        const action = config.actions?.find((item) => item.id === 'open_shift')
        if (action) {
          const reference = `SHIFT-${new Date().toISOString().replace(/[-:.TZ]/g, '').slice(0, 14)}`
          const result = await onAction(action.id, {
            reference,
            status: 'Open',
            started_at: new Date().toISOString(),
          })
          const created = (result as any)?.data || result
          shiftName = created?.name || created?.reference || reference
          if (shiftName) {
            setActiveShift(shiftName)
            sessionStorage.setItem('kora-pos-active-shift', shiftName)
          }
        }
      }

      if (!activeTillSession) {
        const action = config.actions?.find((item) => item.id === 'open_till_session')
        if (action) {
          if (!shiftName) return
          const reference = `TS-${Date.now()}`
          const result = await onAction(action.id, {
            reference,
            register,
            shift: shiftName,
            opening_cash: Number(openingCash || 0),
            expected_cash: Number(openingCash || 0),
            status: 'Open',
          })
          const created = (result as any)?.data || result
          const tillName = created?.name || created?.reference || reference
          if (tillName) {
            setActiveTillSession(tillName)
            sessionStorage.setItem('kora-pos-active-till-session', tillName)
          }
        }
      }
    } finally {
      setProcessing(false)
    }
  }

  const completeSale = async (method: string, externalOperation?: string) => {
    const action = config.actions?.find((item) => item.type === 'create_transaction' && (externalOperation ? item.config?.requires_operation_status : !item.config?.requires_operation_status))
    if (!action) return
    await onAction(action.id, { ...transactionContext(method), ...(externalOperation ? { external_operation: externalOperation } : {}) })
    clearCart()
    resetPayment()
  }

  const handlePayment = async (method: string) => {
    if (items.length === 0) return
    if (!tillReady) return
    if (isMobilePayment(method) && supportsOnlineMobilePayment) {
      setSelectedMethod(method)
      setPhoneError('')
      return
    }
    setProcessing(true)
    try { await completeSale(method) } finally { setProcessing(false) }
  }

  const initiateMobilePayment = async () => {
    if (!selectedMethod || !isKenyanPhone(phoneNumber)) {
      setPhoneError('Enter a valid Kenyan phone number, for example 0712 345 678.')
      return
    }
    const action = config.actions?.find((item) => item.type === 'initiate_external_operation')
    if (!action) {
      setPhoneError('Mobile-money initiation is not configured for this POS.')
      return
    }
    setProcessing(true)
    try {
      const result = await onAction(action.id, {
        ...transactionContext(selectedMethod),
        client_reference: `pos-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      })
      const operation = result?.data || result
      setOperationId(operation?.name || '')
      setPaymentStatus(operation?.status || 'Pending')
    } finally { setProcessing(false) }
  }

  const validateMobilePayment = async () => {
    if (!operationId) return
    const action = config.actions?.find((item) => item.type === 'validate_external_operation')
    if (!action) return
    setProcessing(true)
    try {
      const result = await onAction(action.id, { operation_id: operationId })
      const operation = result?.data || result
      const status = operation?.status || 'Pending'
      setPaymentStatus(status)
      if (status === 'Succeeded') await completeSale(selectedMethod || 'mobile_money', operationId)
    } finally { setProcessing(false) }
  }

  const resetPayment = () => {
    setSelectedMethod(null)
    setPhoneNumber('')
    setOperationId('')
    setPaymentStatus('')
    setPhoneError('')
  }

  return (
    <div className="space-y-3 rounded-lg border p-4">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold">Payment</h3>
        <span className="text-lg font-bold">{cartTotal.toLocaleString()}</span>
      </div>
      {requiresTill && (
        <div className="rounded-md border bg-muted/20 p-3">
          <div className="flex items-center justify-between gap-3">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Shift</p>
              <p className="text-sm font-medium">{activeTillSession ? `Till open · ${activeTillSession}` : activeShift ? `Shift open · ${activeShift}` : 'Open a shift before taking payment'}</p>
            </div>
            {!tillReady && <Unlock className="h-4 w-4 text-muted-foreground" />}
          </div>
          {!tillReady && (
            <div className="mt-3 flex gap-2">
              <input
                type="number"
                min="0"
                inputMode="decimal"
                value={openingCash}
                onChange={(event) => setOpeningCash(event.target.value)}
                className="h-9 min-w-0 flex-1 rounded-md border border-input bg-background px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
                placeholder="Opening cash"
              />
              <Button size="sm" disabled={processing} onClick={openShiftAndTill}>
                {processing ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
                Open shift
              </Button>
            </div>
          )}
        </div>
      )}
      <div className="grid grid-cols-3 gap-2">
        {methods.map((method) => (
          <Button key={method} variant="outline" className="flex h-auto flex-col items-center gap-1 py-3" disabled={items.length === 0 || processing || !tillReady} onClick={() => handlePayment(method)}>
            {processing ? <Loader2 className="h-5 w-5 animate-spin" /> : PAYMENT_ICONS[method] || <Banknote className="h-5 w-5" />}
            <span className="text-xs capitalize">{method.replace('_', ' ')}</span>
          </Button>
        ))}
      </div>
      {selectedMethod && isMobilePayment(selectedMethod) && (
        <div className="space-y-2 rounded-md bg-muted/40 p-3">
          <label htmlFor="pos-payment-phone" className="text-sm font-medium">M-Pesa phone number</label>
          <input id="pos-payment-phone" type="tel" inputMode="tel" autoComplete="tel" placeholder="0712 345 678" value={phoneNumber} disabled={Boolean(operationId) || processing} onChange={(event) => { setPhoneNumber(event.target.value); setPhoneError('') }} className="flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none ring-offset-background focus-visible:ring-2 focus-visible:ring-ring" />
          <p className="text-xs text-muted-foreground">Send the prompt, then validate the provider response.</p>
          {phoneError && <p className="text-xs text-destructive">{phoneError}</p>}
          {!operationId ? (
            <Button className="w-full" disabled={processing || !phoneNumber.trim()} onClick={initiateMobilePayment}>Send payment prompt</Button>
          ) : (
            <div className="space-y-2">
              <p className="text-sm">Payment status: <span className="font-medium">{paymentStatus || 'Pending'}</span></p>
              <div className="flex gap-2">
                <Button className="flex-1" disabled={processing} onClick={validateMobilePayment}>{processing ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}Validate payment</Button>
                <Button variant="outline" disabled={processing} onClick={resetPayment}>Cancel</Button>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function isMobilePayment(method: string) {
  return ['mpesa', 'mobile_money', 'mobile'].includes(method.toLowerCase().replace(/\s+/g, '_'))
}

function isKenyanPhone(phone: string) {
  return /^(?:\+254|254|0)(?:1|7)\d{8}$/.test(phone.replace(/[\s-]/g, ''))
}

function normalizePaymentMethod(method: string) {
  switch (method) {
    case 'cash': return 'Cash'
    case 'card': return 'Card'
    case 'mobile_money':
    case 'mobile': return 'Mobile Money'
    case 'mpesa': return 'Mpesa'
    default: return method.split('_').map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(' ')
  }
}
