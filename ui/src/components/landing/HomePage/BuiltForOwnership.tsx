import { Scale, Server, Database, GitBranch } from 'lucide-react'

const TRUST_ITEMS = [
  {
    icon: Scale,
    title: 'MIT License',
    description: 'Free for commercial use, forever.',
  },
  {
    icon: Server,
    title: 'Self-Host Anywhere',
    description: 'Deploy to AWS, GCP, or your own metal.',
  },
  {
    icon: Database,
    title: 'Direct SQL Access',
    description: 'No proprietary formats. Just standard PostgreSQL.',
  },
  {
    icon: GitBranch,
    title: 'Git-Ops Ready',
    description: 'Manage your schema as code in version control.',
  },
]

export function BuiltForOwnership() {
  return (
    <section className="bg-[#f1edec] py-[120px]">
      <div className="max-w-[960px] mx-auto px-6 text-center">
        <h2 className="text-[48px] leading-[56px] font-semibold mb-8 tracking-[-0.03em]">
          Built for Ownership
        </h2>
        <p className="text-lg leading-7 text-[#444748] max-w-2xl mx-auto mb-[120px]">
          We believe you should own your infrastructure. Kora is designed for maximum portability and developer control.
        </p>
        <div className="grid md:grid-cols-4 gap-6">
          {TRUST_ITEMS.map((item) => (
            <div key={item.title} className="p-6">
              <item.icon className="h-10 w-10 text-[#FF6B35] mb-4 mx-auto" />
              <h4 className="text-xs font-medium uppercase mb-2" style={{ fontFamily: "'Geist Mono Variable', monospace" }}>
                {item.title}
              </h4>
              <p className="text-base text-[#444748]">{item.description}</p>
            </div>
          ))}
        </div>
      </div>
    </section>
  )
}
