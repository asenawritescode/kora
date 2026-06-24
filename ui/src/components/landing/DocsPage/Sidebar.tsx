import { Search } from 'lucide-react'

const SECTIONS = [
  {
    title: 'Introduction',
    links: [
      { label: 'Getting Started', active: true },
      { label: 'Installation', active: false },
      { label: 'Core Concepts', active: false },
    ],
  },
  {
    title: 'Platform',
    links: [
      { label: 'Architecture', active: false },
      { label: 'Data Models', active: false },
      { label: 'Security', active: false },
    ],
  },
  {
    title: 'Configuration',
    links: [
      { label: 'YAML Configuration', active: false },
      { label: 'Environment Variables', active: false },
    ],
  },
  {
    title: 'Reference',
    links: [
      { label: 'API Reference', active: false },
      { label: 'CLI Commands', active: false },
    ],
  },
]

export function DocsSidebar() {
  return (
    <aside className="hidden md:block w-64 shrink-0 border-r border-outline-variant pt-8 pr-8 overflow-y-auto h-[calc(100vh-64px)] sticky top-16">
      {/* Search */}
      <div className="relative mb-8">
        <Search className="absolute left-3 top-1/2 -translate-y-1/2 text-[#5d5f5f] h-3.5 w-3.5" />
        <input
          className="w-full pl-9 pr-3 py-2 bg-[#f1edec] border border-outline-variant rounded-sm text-sm text-black focus:border-black focus:ring-0 focus:outline-none placeholder-[#5d5f5f] transition-colors"
          placeholder="Search docs..."
          type="text"
          style={{ fontFamily: "'Geist Mono Variable', monospace" }}
        />
      </div>

      <nav className="space-y-6">
        {SECTIONS.map((section) => (
          <div key={section.title}>
            <h3 className="text-[10px] font-medium text-[#5d5f5f] uppercase mb-2 tracking-[0.15em]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
              {section.title}
            </h3>
            <ul className="space-y-0.5">
              {section.links.map((link) => (
                <li key={link.label}>
                  <a
                    href="#"
                    className={`block px-2 py-1.5 text-sm rounded-sm transition-colors ${
                      link.active
                        ? 'text-black font-medium bg-[#f1edec]'
                        : 'text-[#5d5f5f] hover:text-black hover:bg-gray-50'
                    }`}
                  >
                    {link.label}
                  </a>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </nav>
    </aside>
  )
}
