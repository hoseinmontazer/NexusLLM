import type { Metadata } from 'next'
import './globals.css'
import { Sidebar } from '@/components/layout/Sidebar'
import { QueryProvider } from '@/components/layout/QueryProvider'
import { Toaster } from '@/components/ui/toaster'
import { AuthProvider } from '@/lib/auth-context'
import { AuthGuard } from '@/components/layout/AuthGuard'

export const metadata: Metadata = {
  title: 'NexusLLM — AI Infrastructure & Developer Portal',
  description: 'Enterprise LLM Gateway, Developer Portal & Infrastructure Platform',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="bg-background text-foreground antialiased subpixel-antialiased">
        <QueryProvider>
          <AuthProvider>
            <AuthGuard>
              <div className="flex h-screen overflow-hidden">
                <Sidebar />
                <main className="flex-1 overflow-y-auto bg-gray-50">
                  {children}
                </main>
              </div>
            </AuthGuard>
          </AuthProvider>
          <Toaster />
        </QueryProvider>
      </body>
    </html>
  )
}
