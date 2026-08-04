'use client'

import { useAuth } from '@/lib/auth-context'
import { usePathname, useRouter } from 'next/navigation'
import { useEffect } from 'react'

const PUBLIC_PATHS = ['/login', '/register']

export function AuthGuard({ children }: { children: React.ReactNode }) {
  const { user, loading } = useAuth()
  const pathname = usePathname()
  const router = useRouter()

  const isPublicPath = PUBLIC_PATHS.includes(pathname)

  useEffect(() => {
    if (loading) return

    if (!user && !isPublicPath) {
      router.replace('/login')
      return
    }

    if (user && isPublicPath) {
      if (user.role === 'admin') {
        router.replace('/')
      } else {
        router.replace('/portal')
      }
      return
    }

    // Role-Based Access Control (RBAC):
    // Non-admin users attempting to access admin routes get redirected to the developer portal (/portal)
    if (user && user.role !== 'admin' && !pathname.startsWith('/portal')) {
      router.replace('/portal')
      return
    }
  }, [user, loading, pathname, isPublicPath, router])

  if (loading) {
    return (
      <div className="min-h-screen bg-gray-950 text-white flex flex-col items-center justify-center gap-3">
        <div className="w-8 h-8 border-4 border-blue-600 border-t-transparent rounded-full animate-spin" />
        <p className="text-sm text-gray-400 font-medium">Verifying session...</p>
      </div>
    )
  }

  if (!user && !isPublicPath) {
    return null
  }

  if (user && isPublicPath) {
    return null
  }

  if (user && user.role !== 'admin' && !pathname.startsWith('/portal')) {
    return null
  }

  return <>{children}</>
}
