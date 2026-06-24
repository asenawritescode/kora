import { HeroPost } from './HeroPost'
import { ArticleGrid } from './ArticleGrid'

export default function BlogPage() {
  return (
    <div className="max-w-[960px] mx-auto px-6 pb-[120px] space-y-[120px]">
      {/* Page Header */}
      <header className="space-y-4 pt-8">
        <div className="flex items-center gap-2 text-sm text-[#5d5f5f] uppercase tracking-[0.15em]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          <span className="w-8 h-[1px] bg-outline-variant" />
          <span>Engineering Log</span>
        </div>
        <h1 className="text-[72px] leading-[80px] font-bold text-black tracking-[-0.04em]">
          Insights &amp; Architecture
        </h1>
        <p className="text-lg leading-7 text-[#444748] max-w-2xl">
          Technical deep dives, architectural patterns, and updates from the Kora engineering team building global infrastructure.
        </p>
      </header>

      <HeroPost />
      <ArticleGrid />
    </div>
  )
}
