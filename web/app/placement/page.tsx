// Placement page redirects to /cluster#placement
'use client'
import { useEffect } from 'react'
import { useRouter } from 'next/navigation'

export default function PlacementRedirect() {
  const router = useRouter()
  useEffect(() => { router.replace('/cluster?tab=placement') }, [router])
  return null
}
