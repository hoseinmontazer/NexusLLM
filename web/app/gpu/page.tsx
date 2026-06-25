// GPU Inventory is now under /cluster → GPU Inventory tab.
'use client'
import { useEffect } from 'react'
import { useRouter } from 'next/navigation'

export default function GpuRedirect() {
  const router = useRouter()
  useEffect(() => { router.replace('/cluster?tab=gpu') }, [router])
  return null
}
