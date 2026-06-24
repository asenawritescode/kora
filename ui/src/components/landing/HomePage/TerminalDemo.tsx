import { useState, useEffect } from 'react'
import { Check } from 'lucide-react'

const TERMINAL_LINES = [
  { text: '$ kora init', delay: 400, color: 'text-[#c6c6c7]' },
  { text: 'AI: What kind of application are you building?', delay: 300, color: 'text-[#FF6B35]' },
  { text: '> I need a kiosk app for a veterinary clinic to manage pet check-ins and owner details.', delay: 200, color: 'text-[#10B981]' },
  { spacer: true },
  { text: 'Generating schema...', delay: 400, color: 'text-[#c6c6c7]' },
]

const CHECK_ITEMS = [
  'Created table: Pets (id, name, species, owner_id)',
  'Created table: Owners (id, name, phone)',
  'Created table: CheckIns (id, pet_id, time, reason)',
  'Scaffolded React frontend',
]

export function TerminalDemo() {
  const [visibleLines, setVisibleLines] = useState(0)
  const [showChecks, setShowChecks] = useState(false)
  const [checkIndex, setCheckIndex] = useState(0)
  const [showFinal, setShowFinal] = useState(false)

  useEffect(() => {
    if (visibleLines < TERMINAL_LINES.length) {
      const timer = setTimeout(() => setVisibleLines((v) => v + 1), 400)
      return () => clearTimeout(timer)
    } else if (!showChecks) {
      const timer = setTimeout(() => setShowChecks(true), 300)
      return () => clearTimeout(timer)
    }
  }, [visibleLines, showChecks])

  useEffect(() => {
    if (showChecks && checkIndex < CHECK_ITEMS.length + 2) {
      const timer = setTimeout(() => setCheckIndex((i) => i + 1), 500)
      return () => clearTimeout(timer)
    } else if (checkIndex >= CHECK_ITEMS.length + 2 && !showFinal) {
      const timer = setTimeout(() => setShowFinal(true), 300)
      return () => clearTimeout(timer)
    }
  }, [showChecks, checkIndex, showFinal])

  return (
    <div className="bg-black text-white rounded-lg p-6 max-w-4xl mx-auto text-left shadow-2xl overflow-hidden border border-[#444748]">
      {/* Traffic lights */}
      <div className="flex gap-2 mb-4">
        <div className="w-3 h-3 rounded-full bg-[#DC2626]" />
        <div className="w-3 h-3 rounded-full bg-[#FF6B35]" />
        <div className="w-3 h-3 rounded-full bg-[#10B981]" />
      </div>

      <div className="space-y-4" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
        <div className="text-sm space-y-4">
          {TERMINAL_LINES.map((line, i) => {
            if (line.spacer && i < visibleLines) return <div key={i} className="mt-4" />
            if (i >= visibleLines) return null
            return (
              <div key={i} className={line.color}>
                {line.text}
              </div>
            )
          })}
        </div>

        {/* Generating UI text */}
        {checkIndex >= CHECK_ITEMS.length && (
          <div className="text-[#c6c6c7] text-sm mt-4">Generating UI...</div>
        )}

        {/* Check items */}
        {showChecks && CHECK_ITEMS.map((item, i) => {
          if (i >= checkIndex && (checkIndex < CHECK_ITEMS.length || i > checkIndex - CHECK_ITEMS.length)) return null
          const isShown = checkIndex >= CHECK_ITEMS.length
            ? true
            : i < checkIndex
          if (!isShown) return null
          return (
            <div key={item} className="flex items-center gap-2 text-[#10B981] text-sm animate-in fade-in">
              <Check className="h-4 w-4" />
              <span>{item}</span>
            </div>
          )
        })}

        {/* Final line */}
        {showFinal && (
          <div className="text-white font-bold text-sm mt-4 animate-in fade-in">
            App is running at http://localhost:8080
          </div>
        )}
      </div>
    </div>
  )
}
