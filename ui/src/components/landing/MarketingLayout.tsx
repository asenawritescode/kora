import type { ReactNode } from 'react'
import { Navigation } from './Navigation'
import { Footer } from './Footer'

export default function MarketingLayout({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen flex flex-col bg-[#fdf8f8] text-[#1c1b1b] antialiased" style={{ fontFamily: "'Geist Variable', sans-serif" }}>
      <Navigation />
      <main className="flex-grow pt-24">
        {children}
      </main>
      <Footer />
    </div>
  )
}
