'use client'
// This page has been merged into /models.
// All model types (LLM, STT, TTS, OCR, Embedding, Rerank, ...) are managed
// from the unified Models page. This redirect exists for backward compatibility.
import { useEffect } from 'react'
import { useRouter } from 'next/navigation'

export default function ServicesRedirect() {
  const router = useRouter()
  useEffect(() => { router.replace('/models') }, [router])
  return (
    <div className="flex items-center justify-center h-full text-muted-foreground text-sm">
      Redirecting to Models…
    </div>
  )
}
