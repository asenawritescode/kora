import { BadgeCheck, Terminal, Scale } from 'lucide-react'
import { TerminalDemo } from './TerminalDemo'

export function HeroSection() {
  return (
    <section className="max-w-[960px] mx-auto px-6 py-[120px] text-center">
      <h1 className="text-[72px] leading-[80px] font-bold text-black mb-4 tracking-[-0.04em]">
        Custom business software,<br />without custom development.
      </h1>
      <p className="text-lg leading-7 text-[#444748] max-w-2xl mx-auto mb-8">
        For developers and businesses that need custom software without custom engineering.
        Describe your business in plain English, and Kora builds the database, API, and UI instantly.
      </p>

      {/* CTAs */}
      <div className="flex justify-center items-center gap-4 mb-8">
        <button className="bg-[#FF6B35] text-white text-xs font-medium px-8 py-3 rounded-sm flex items-center gap-2 hover:bg-[#E55B25] transition-colors" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          Start Building Free
          <ArrowRight className="h-4 w-4" />
        </button>
        <button className="bg-transparent text-black border border-outline-variant text-xs font-medium px-8 py-3 rounded-sm flex items-center gap-2 hover:bg-gray-50 transition-colors" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          Read the Docs
          <ArrowRight className="h-4 w-4" />
        </button>
      </div>

      {/* Trust signals */}
      <div className="flex justify-center gap-6 text-xs text-[#5d5f5f] mb-[120px]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
        <div className="flex items-center gap-2"><BadgeCheck className="h-4 w-4" /><span>Open source</span></div>
        <div className="flex items-center gap-2"><Terminal className="h-4 w-4" /><span>Single 30MB binary</span></div>
        <div className="flex items-center gap-2"><Scale className="h-4 w-4" /><span>MIT licensed</span></div>
      </div>

      <TerminalDemo />
    </section>
  )
}

// Small helper to avoid extra file for a one-line icon wrapper
function ArrowRight({ className }: { className?: string }) {
  return (
    <svg className={className} width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M5 12h14M12 5l7 7-7 7" />
    </svg>
  )
}
