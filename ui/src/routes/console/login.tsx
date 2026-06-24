import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { AlertCircle, Loader2, CheckCircle } from 'lucide-react'
import { LogoMark } from '@/components/ui/LogoMark'

export default function ConsoleLoginPage() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const { login, isLoading: loginLoading, error: loginError, clearError } = useConsoleAuthStore()
  const navigate = useNavigate()

  // Onboard state
  const [tab, setTab] = useState<'signin' | 'onboard'>('signin')
  const [hostname, setHostname] = useState('')
  const [onboardEmail, setOnboardEmail] = useState('')
  const [onboardPassword, setOnboardPassword] = useState('')
  const [onboardLoading, setOnboardLoading] = useState(false)
  const [onboardError, setOnboardError] = useState<string | null>(null)
  const [onboardSuccess, setOnboardSuccess] = useState<string | null>(null)

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await login(email, password)
      navigate({ to: '/console' })
    } catch { /* error is set in store */ }
  }

  const handleOnboard = async (e: React.FormEvent) => {
    e.preventDefault()
    setOnboardError(null)
    setOnboardLoading(true)
    try {
      const res = await fetch('/api/console/sites/onboard', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          hostname: hostname + '.local',
          admin_email: onboardEmail,
          admin_password: onboardPassword,
        }),
      })
      const data = await res.json()
      if (!res.ok) {
        setOnboardError(typeof data.error === 'string' ? data.error : data.error?.message || 'Something went wrong.')
        return
      }
      setOnboardSuccess(data.data?.workspace_url || `/s/${hostname}/workspace`)
    } catch {
      setOnboardError('Network error. Please try again.')
    } finally {
      setOnboardLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <LogoMark size={36} />
          </div>
          <CardTitle className="text-2xl">Kora Console</CardTitle>
          <CardDescription>System administration</CardDescription>

          {/* Tabs */}
          <div className="flex border-b border-border mt-4">
            <button
              onClick={() => setTab('signin')}
              className={`flex-1 pb-2 text-sm font-medium border-b-2 transition-colors ${
                tab === 'signin' ? 'border-[#FF6B35] text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              Sign In
            </button>
            <button
              onClick={() => setTab('onboard')}
              className={`flex-1 pb-2 text-sm font-medium border-b-2 transition-colors ${
                tab === 'onboard' ? 'border-[#FF6B35] text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              Create your site
            </button>
          </div>
        </CardHeader>

        <CardContent className="pt-4">
          {tab === 'signin' ? (
            <form onSubmit={handleLogin} className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="email">Email</Label>
                <Input
                  id="email" type="email" placeholder="admin@kora.local"
                  value={email} onChange={(e) => { setEmail(e.target.value); if (loginError) clearError() }}
                  required autoFocus
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="password">Password</Label>
                <Input
                  id="password" type="password" placeholder="••••••••"
                  value={password} onChange={(e) => { setPassword(e.target.value); if (loginError) clearError() }}
                  required
                />
              </div>
              {loginError && (
                <div className="flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm">
                  <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
                  <p className="text-destructive">{loginError}</p>
                </div>
              )}
              <Button type="submit" className="w-full bg-[#FF6B35] hover:bg-[#E55B25]" disabled={loginLoading}>
                {loginLoading ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
                Sign in
              </Button>
            </form>
          ) : onboardSuccess ? (
            <div className="text-center py-4 space-y-4">
              <CheckCircle className="h-10 w-10 text-green-600 mx-auto" />
              <p className="text-sm font-medium">Your application is ready.</p>
              <a href={onboardSuccess} className="inline-block w-full bg-[#FF6B35] hover:bg-[#E55B25] text-white text-sm font-medium px-6 py-2.5 rounded-md transition-colors">
                Open your workspace →
              </a>
            </div>
          ) : (
            <form onSubmit={handleOnboard} className="space-y-4">
              <p className="text-xs text-muted-foreground">
                Create a site with your own database, admin UI, and API — instantly.
              </p>
              <div className="space-y-2">
                <Label htmlFor="hostname">Site name</Label>
                <div className="flex items-center rounded-md border border-input focus-within:ring-2 focus-within:ring-ring focus-within:ring-offset-2">
                  <input
                    id="hostname" type="text" required minLength={3} maxLength={50} pattern="[a-z0-9-]+"
                    placeholder="mybusiness"
                    value={hostname}
                    onChange={(e) => setHostname(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ''))}
                    className="flex-1 bg-transparent px-3 py-2 text-sm outline-none font-mono"
                  />
                  <span className="pr-3 text-xs text-muted-foreground font-mono">.local</span>
                </div>
              </div>
              <div className="space-y-2">
                <Label htmlFor="onboard-email">Admin email</Label>
                <Input id="onboard-email" type="email" required placeholder="you@example.com"
                  value={onboardEmail} onChange={(e) => { setOnboardEmail(e.target.value); if (onboardError) setOnboardError(null) }}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="onboard-password">Password</Label>
                <Input id="onboard-password" type="password" required minLength={8} placeholder="Minimum 8 characters"
                  value={onboardPassword} onChange={(e) => { setOnboardPassword(e.target.value); if (onboardError) setOnboardError(null) }}
                />
              </div>
              {onboardError && (
                <div className="flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm">
                  <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
                  <p className="text-destructive">{onboardError}</p>
                </div>
              )}
              <Button type="submit" className="w-full bg-[#FF6B35] hover:bg-[#E55B25]" disabled={onboardLoading}>
                {onboardLoading ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
                Create my application
              </Button>
              <p className="text-[10px] text-muted-foreground text-center">
                Your data lives in your own database. No vendor lock-in.
              </p>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
