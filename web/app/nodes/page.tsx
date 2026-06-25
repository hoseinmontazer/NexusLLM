// Nodes page redirects to /cluster — the unified infrastructure view.
'use client'
import { useEffect } from 'react'
import { useRouter } from 'next/navigation'

export default function NodesRedirect() {
  const router = useRouter()
  useEffect(() => { router.replace('/cluster') }, [router])
  return null
}
