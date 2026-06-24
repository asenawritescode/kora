import type { Template } from './data'

const ICON_MAP: Record<string, string> = {
  Store: '🏪',
  Users: '👥',
  HeartPulse: '🏥',
  GraduationCap: '🏫',
  Building2: '🏠',
  Landmark: '💰',
  Truck: '🚚',
  Calendar: '🎫',
}

const FEATURE_ICON_MAP: Record<string, string> = {
  Database: '🗄️',
  Layout: '📋',
  RefreshCw: '🔄',
}

export function TemplateCard({ template }: { template: Template }) {
  return (
    <div className="bg-white border border-outline-variant rounded-sm p-8 hover:border-black transition-colors group cursor-pointer flex flex-col h-full">
      {/* Icon */}
      <div className="bg-[#f1edec] p-3 rounded-sm w-max mb-4 group-hover:bg-[#FF6B35] group-hover:text-white transition-colors">
        <span className="text-2xl">{ICON_MAP[template.icon] || '📦'}</span>
      </div>

      <h3 className="text-[24px] leading-tight font-semibold text-black mb-2">
        {template.name}
      </h3>
      <p className="text-base text-[#5d5f5f] mb-4 flex-grow">
        {template.description}
      </p>

      {/* Feature list */}
      <div className="border-t border-outline-variant pt-2 mt-auto">
        <ul className="space-y-2">
          {template.features.map((f) => (
            <li key={f.label} className="flex items-center text-[#5d5f5f] text-sm" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
              <span className="mr-2 text-xs">{FEATURE_ICON_MAP[f.iconType] || '•'}</span>
              {f.label}
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}
