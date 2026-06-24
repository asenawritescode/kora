import { Search } from 'lucide-react'

const CATEGORIES = ['All', 'Operations', 'Finance', 'Education', 'Retail']

interface FilterBarProps {
  activeCategory: string
  onCategoryChange: (cat: string) => void
  search: string
  onSearchChange: (q: string) => void
}

export function FilterBar({ activeCategory, onCategoryChange, search, onSearchChange }: FilterBarProps) {
  return (
    <div className="flex justify-between items-center mb-8 border-b border-outline-variant pb-2">
      {/* Category pills */}
      <div className="flex gap-2 overflow-x-auto pb-2">
        {CATEGORIES.map((cat) => (
          <button
            key={cat}
            onClick={() => onCategoryChange(cat)}
            className={`text-xs font-medium px-4 py-2 rounded-sm transition-colors border ${
              cat === activeCategory
                ? 'bg-black text-white border-black'
                : 'bg-transparent text-[#5d5f5f] border-outline-variant hover:border-black'
            }`}
            style={{ fontFamily: "'Geist Mono Variable', monospace" }}
          >
            {cat}
          </button>
        ))}
      </div>

      {/* Search */}
      <div className="hidden md:flex relative">
        <Search className="absolute left-3 top-1/2 -translate-y-1/2 text-[#5d5f5f] h-3.5 w-3.5" />
        <input
          className="pl-10 pr-4 py-2 bg-transparent border border-outline-variant rounded-sm text-sm text-black focus:outline-none focus:border-black focus:ring-0 placeholder-[#5d5f5f] transition-colors w-64"
          placeholder="Search templates..."
          type="text"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
          style={{ fontFamily: "'Geist Mono Variable', monospace" }}
        />
      </div>
    </div>
  )
}
