import { Download, Key, Copy } from 'lucide-react'

export function QuickStartSection() {
  return (
    <section className="mb-[120px]">
      <h2 className="text-[30px] leading-[38px] font-semibold text-black mb-8 flex items-center gap-2">
        <span className="text-[#FF6B35]">⚡</span> Quick Start
      </h2>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Install Card */}
        <div className="bg-[#FAFAFA] border border-outline-variant rounded-sm p-6 flex flex-col justify-between hover:border-black transition-colors group">
          <div>
            <div className="flex items-center gap-2 mb-2 text-black">
              <Download className="h-4 w-4" />
              <h3 className="font-bold text-sm">1. Install the CLI</h3>
            </div>
            <p className="text-[#5d5f5f] text-sm mb-4">
              Download and install the Kora CLI for your operating system to begin managing your infrastructure.
            </p>
          </div>
          <div className="bg-black text-white p-3 rounded-sm text-sm flex justify-between items-center group-hover:bg-[#1a1a1a] transition-colors" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
            <code>curl -sL https://kora.io/install | bash</code>
            <button className="text-[#5d5f5f] hover:text-white transition-colors" title="Copy to clipboard">
              <Copy className="h-3.5 w-3.5" />
            </button>
          </div>
        </div>

        {/* Auth Card */}
        <div className="bg-[#FAFAFA] border border-outline-variant rounded-sm p-6 flex flex-col justify-between hover:border-black transition-colors group">
          <div>
            <div className="flex items-center gap-2 mb-2 text-black">
              <Key className="h-4 w-4" />
              <h3 className="font-bold text-sm">2. Authenticate</h3>
            </div>
            <p className="text-[#5d5f5f] text-sm mb-4">
              Log in to your Kora account or configure your API keys for CI/CD environments.
            </p>
          </div>
          <div className="bg-black text-white p-3 rounded-sm text-sm flex justify-between items-center group-hover:bg-[#1a1a1a] transition-colors" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
            <code>kora auth login</code>
            <button className="text-[#5d5f5f] hover:text-white transition-colors" title="Copy to clipboard">
              <Copy className="h-3.5 w-3.5" />
            </button>
          </div>
        </div>
      </div>
    </section>
  )
}
