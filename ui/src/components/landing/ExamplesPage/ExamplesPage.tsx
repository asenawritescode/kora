import { useState, useMemo } from 'react'
import { FilterBar } from './FilterBar'
import { TemplateCard } from './TemplateCard'
import { TEMPLATES, type Template } from './data'

export default function ExamplesPage() {
  const [activeCategory, setActiveCategory] = useState('All')
  const [search, setSearch] = useState('')

  const filtered = useMemo(() => {
    return TEMPLATES.filter((t) => {
      const cat = activeCategory === 'All' || t.category === activeCategory
      const q = !search || t.name.toLowerCase().includes(search.toLowerCase())
        || t.description.toLowerCase().includes(search.toLowerCase())
      return cat && q
    })
  }, [activeCategory, search])

  return (
    <section className="max-w-[960px] mx-auto px-6 pb-[120px]">
      {/* Hero */}
      <div className="mb-[120px] text-center">
        <h1 className="text-[72px] leading-[80px] font-bold text-black mb-4 tracking-[-0.04em]">
          16 Industry Templates
        </h1>
        <p className="text-lg leading-7 text-[#5d5f5f] max-w-2xl mx-auto">
          Accelerate your build with production-ready schemas, workflows, and UI patterns tailored for specific operational domains.
        </p>
      </div>

      <FilterBar
        activeCategory={activeCategory}
        onCategoryChange={setActiveCategory}
        search={search}
        onSearchChange={setSearch}
      />

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-8">
        {filtered.map((template) => (
          <TemplateCard key={template.name} template={template} />
        ))}
      </div>
    </section>
  )
}
