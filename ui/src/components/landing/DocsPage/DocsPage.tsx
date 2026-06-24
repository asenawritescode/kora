import { QuickStartSection } from './QuickStartSection'
import { ArchitectureSection } from './ArchitectureSection'
import { YamlConfigSection } from './YamlConfigSection'
import { ApiReferenceSection } from './ApiReferenceSection'

export default function DocsPage() {
  return (
    <>
      {/* Breadcrumbs */}
      <div className="flex items-center gap-2 text-xs text-[#5d5f5f] mb-8" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
        <a href="#" className="hover:text-black transition-colors">Docs</a>
        <span className="text-sm">›</span>
        <span className="text-black">Getting Started</span>
      </div>

      {/* Page Header */}
      <div className="mb-12 border-b border-outline-variant pb-8">
        <h1 className="text-[48px] leading-[56px] font-semibold text-black mb-4 tracking-[-0.03em]">
          Getting Started with Kora
        </h1>
        <p className="text-lg leading-7 text-[#5d5f5f]">
          A comprehensive guide to initializing your infrastructure, configuring your first cluster, and deploying standard workloads using the Kora CLI.
        </p>
      </div>

      <QuickStartSection />
      <ArchitectureSection />
      <YamlConfigSection />
      <ApiReferenceSection />
    </>
  )
}
