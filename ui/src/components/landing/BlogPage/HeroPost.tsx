import { User } from 'lucide-react'

export function HeroPost() {
  return (
    <section className="group cursor-pointer border border-outline-variant bg-white overflow-hidden flex flex-col md:flex-row hover:border-black transition-colors duration-300">
      {/* Image */}
      <div className="md:w-3/5 border-b md:border-b-0 md:border-r border-outline-variant bg-[#f1edec] overflow-hidden h-[300px] md:h-auto">
        <div className="w-full h-full bg-gradient-to-br from-gray-200 via-gray-100 to-white opacity-80 group-hover:opacity-100 group-hover:scale-105 transition-all duration-700" />
      </div>

      {/* Content */}
      <div className="md:w-2/5 p-8 md:p-10 flex flex-col justify-between gap-8 bg-white">
        <div className="space-y-4">
          <div className="flex items-center gap-3">
            <span className="text-xs font-medium bg-[#f1edec] text-black px-2 py-1 rounded-sm uppercase tracking-wider" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
              Infrastructure
            </span>
            <span className="text-sm text-[#5d5f5f]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
              Oct 24, 2024
            </span>
          </div>
          <h2 className="text-[48px] leading-[56px] font-semibold text-black group-hover:text-black/80 transition-colors tracking-[-0.03em]">
            Building Infrastructure from Africa for the World
          </h2>
          <p className="text-base text-[#444748] line-clamp-3">
            An architectural breakdown of how we engineered a multi-region distributed system capable of handling high-throughput financial transactions, leveraging global edge networks while maintaining core integrity.
          </p>
        </div>
        <div className="flex items-center gap-3 pt-4 border-t border-outline-variant">
          <div className="w-8 h-8 rounded-full bg-[#e5e2e1] border border-outline flex items-center justify-center overflow-hidden">
            <User className="h-4 w-4 text-[#5d5f5f]" />
          </div>
          <div className="text-sm text-black" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
            By Engineering Team
          </div>
          <div className="ml-auto text-black group-hover:translate-x-2 transition-transform duration-300">
            <ArrowRight />
          </div>
        </div>
      </div>
    </section>
  )
}

function ArrowRight() {
  return (
    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M5 12h14M12 5l7 7-7 7" />
    </svg>
  )
}
