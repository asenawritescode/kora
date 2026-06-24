import { X } from 'lucide-react'

const PROBLEMS = [
  {
    title: 'Spreadsheets',
    items: [
      'Data gets corrupted easily',
      'Hard to build distinct UIs for different roles',
      'Zero API integrations out of the box',
    ],
  },
  {
    title: 'SaaS',
    items: [
      'You must adapt your workflow to their model',
      'Expensive per-seat pricing',
      'Your data is locked in their cloud',
    ],
  },
  {
    title: 'Custom Build',
    items: [
      'Months of development time',
      'High upfront and maintenance costs',
      'Requires dedicated engineering team',
    ],
  },
]

export function ProblemSection() {
  return (
    <section className="max-w-[960px] mx-auto px-6 py-[120px]">
      <h2 className="text-[48px] leading-[56px] font-semibold text-center mb-[120px] tracking-[-0.03em]">
        Software should adapt to your business.<br />Not the other way around.
      </h2>
      <div className="grid md:grid-cols-3 gap-6">
        {PROBLEMS.map((problem) => (
          <div key={problem.title} className="bg-[#FAFAFA] border border-outline-variant p-8 rounded-sm">
            <h3 className="text-[30px] leading-[38px] font-semibold mb-4">{problem.title}</h3>
            <ul className="space-y-4 text-base text-[#444748]">
              {problem.items.map((item) => (
                <li key={item} className="flex items-start gap-2">
                  <X className="h-4 w-4 text-[#DC2626] mt-1 shrink-0" />
                  <span>{item}</span>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </div>
    </section>
  )
}
