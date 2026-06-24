import type { ReactNode } from 'react'
import { Navigation } from './Navigation'
import { Footer } from './Footer'
import { DocsSidebar } from './DocsPage/Sidebar'

export default function DocsLayout({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen flex flex-col bg-[#fdf8f8] text-[#1c1b1b] antialiased" style={{ fontFamily: "'Geist Variable', sans-serif" }}>
      <Navigation />
      <div className="flex-grow pt-16 flex max-w-[960px] mx-auto w-full px-6 md:px-0">
        <DocsSidebar />
        <main className="flex-grow pt-8 pb-[120px] md:pl-8 max-w-3xl">
          {children}
        </main>
      </div>
      <Footer />
    </div>
  )
}
