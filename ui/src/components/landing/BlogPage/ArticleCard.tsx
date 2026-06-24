import type { Article } from './data'

export function ArticleCard({ article }: { article: Article }) {
  return (
    <article className="group cursor-pointer flex flex-col border border-outline-variant bg-white hover:border-black transition-colors">
      {/* Image */}
      <div className="h-48 border-b border-outline-variant overflow-hidden bg-[#f1edec] relative">
        <div className="absolute top-0 left-0 w-full h-1 bg-[#FF6B35] z-10 scale-x-0 group-hover:scale-x-100 origin-left transition-transform duration-500" />
        <div className="w-full h-full bg-gradient-to-br from-gray-200 via-gray-100 to-white opacity-80 group-hover:opacity-100 group-hover:scale-105 transition-all duration-700" />
      </div>

      <div className="p-6 flex flex-col flex-grow gap-4">
        <div className="flex items-center gap-2 text-sm text-[#5d5f5f]" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          <span className="text-[#FF6B35]">■</span> {article.category}
        </div>
        <h4 className="text-[24px] leading-tight font-semibold text-black">
          {article.title}
        </h4>
        <p className="text-base text-[#444748] flex-grow line-clamp-2">
          {article.description}
        </p>
        <div className="text-xs text-[#5d5f5f] pt-4 flex justify-between items-center" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
          <span>{article.readTime}</span>
          <span className="group-hover:text-black transition-colors">↗</span>
        </div>
      </div>
    </article>
  )
}
