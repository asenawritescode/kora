import { useEffect } from 'react'
import { Loader2 } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'

const cloudSignInURL = 'https://kora.mradiafrica.com/workspace/auth/login'

export default function LoginPage() {
  useEffect(() => {
    window.location.replace(cloudSignInURL)
  }, [])

  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardContent className="flex flex-col items-center gap-4 py-12 text-center">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" aria-hidden="true" />
          <div className="space-y-1">
            <h1 className="text-xl font-semibold tracking-tight">Continue to Kora Cloud</h1>
            <p className="text-sm text-muted-foreground">Sign in to Cloud to open your workspace.</p>
          </div>
          <a className="text-sm font-medium text-primary underline underline-offset-4" href={cloudSignInURL}>
            Continue to sign in
          </a>
        </CardContent>
      </Card>
    </div>
  )
}
