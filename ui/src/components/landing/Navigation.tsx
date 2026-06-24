import { useState, useEffect } from 'react'
import { Link } from '@tanstack/react-router'
import { Menu, X } from 'lucide-react'

const NAV_LINKS = [
  { label: 'Product', href: '/' },
  { label: 'Examples', href: '/examples' },
  { label: 'Docs', href: '/docs' },
  { label: 'Blog', href: '/blog' },
]

export function Navigation() {
  const [scrolled, setScrolled] = useState(false)
  const [mobileOpen, setMobileOpen] = useState(false)

  useEffect(() => {
    const onScroll = () => setScrolled(window.scrollY > 80)
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [])

  // Determine active link from current path
  const currentPath = window.location.pathname

  return (
    <nav
      className={`fixed top-0 w-full z-50 border-b border-outline-variant transition-all duration-200 ${
        scrolled ? 'bg-white/80 backdrop-blur-md' : 'bg-white/90 backdrop-blur-md'
      }`}
    >
      <div className="max-w-[960px] mx-auto px-6 flex justify-between items-center h-16">
        {/* Logo + Links */}
        <div className="flex items-center gap-8">
          <Link to="/" className="text-[30px] leading-[38px] font-semibold text-black tracking-tight">
            Kora
          </Link>
          <div className="hidden md:flex gap-4">
            {NAV_LINKS.map((link) => {
              const isActive = link.href === '/'
                ? currentPath === '/'
                : currentPath.startsWith(link.href)
              return (
                <Link
                  key={link.label}
                  to={link.href as any}
                  className={`text-xs font-medium uppercase tracking-wider px-3 py-2 rounded-sm transition-colors ${
                    isActive
                      ? 'text-black border-b-2 border-black'
                      : 'text-[#5d5f5f] hover:text-black hover:bg-gray-100'
                  }`}
                  style={{ fontFamily: "'Geist Mono Variable', monospace" }}
                >
                  {link.label}
                </Link>
              )
            })}
          </div>
        </div>

        {/* CTA */}
        <div className="hidden md:flex items-center">
          <Link
            to="/workspace"
            className="bg-black text-white text-xs font-medium px-4 py-2 rounded-sm hover:opacity-90 transition-opacity"
            style={{ fontFamily: "'Geist Mono Variable', monospace" }}
          >
            Start Building
          </Link>
        </div>

        {/* Mobile menu button */}
        <button
          className="md:hidden p-2 text-black"
          onClick={() => setMobileOpen(!mobileOpen)}
        >
          {mobileOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
        </button>
      </div>

      {/* Mobile menu */}
      {mobileOpen && (
        <div className="md:hidden border-t border-outline-variant bg-white">
          <div className="px-6 py-4 space-y-2">
            {NAV_LINKS.map((link) => {
              const isActive = link.href === '/'
                ? currentPath === '/'
                : currentPath.startsWith(link.href)
              return (
                <Link
                  key={link.label}
                  to={link.href as any}
                  onClick={() => setMobileOpen(false)}
                  className={`block text-sm font-medium px-3 py-2 rounded-sm transition-colors ${
                    isActive
                      ? 'text-black bg-gray-100'
                      : 'text-[#5d5f5f] hover:text-black hover:bg-gray-50'
                  }`}
                >
                  {link.label}
                </Link>
              )
            })}
            <Link
              to="/workspace"
              onClick={() => setMobileOpen(false)}
              className="block w-full text-center bg-black text-white text-sm font-medium px-4 py-2 rounded-sm mt-4"
            >
              Start Building
            </Link>
          </div>
        </div>
      )}
    </nav>
  )
}
