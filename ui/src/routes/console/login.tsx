import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { AlertCircle, Loader2 } from 'lucide-react'
import { LogoMark } from '@/components/ui/LogoMark'

export default function ConsoleLoginPage() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const { login, isLoading: loginLoading, error: loginError, clearError } = useConsoleAuthStore()
  const navigate = useNavigate()

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await login(email, password)
      navigate({ to: '/console' })
    } catch { /* error is set in store */ }
  }


  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <LogoMark size={36} />
          </div>
          <CardTitle className="text-2xl">Kigae Console</CardTitle>
          <CardDescription>Launch and manage your Kigae sites</CardDescription>

        </CardHeader>

        <CardContent className="pt-4">
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
                <PasswordInput
                  id="password" placeholder="Password"
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

        </CardContent>
      </Card>
    </div>
  )
}
