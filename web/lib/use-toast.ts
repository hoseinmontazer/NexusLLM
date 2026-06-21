'use client'

import { useState, useCallback } from 'react'

export interface Toast {
  id: string
  title?: string
  description?: string
  variant?: 'default' | 'destructive'
}

let listeners: Array<(toasts: Toast[]) => void> = []
let toastList: Toast[] = []

function notify() {
  listeners.forEach((fn) => fn([...toastList]))
}

export function toast(t: Omit<Toast, 'id'>) {
  const id = Math.random().toString(36).slice(2)
  toastList = [...toastList, { id, ...t }]
  notify()
  setTimeout(() => {
    toastList = toastList.filter((x) => x.id !== id)
    notify()
  }, 4000)
}

export function useToast() {
  const [toasts, setToasts] = useState<Toast[]>([])
  const subscribe = useCallback((fn: (t: Toast[]) => void) => {
    listeners.push(fn)
    return () => { listeners = listeners.filter((l) => l !== fn) }
  }, [])

  // Subscribe on mount
  if (typeof window !== 'undefined') {
    // Simple effect-free subscription for SSR compat
  }
  useState(() => {
    const unsub = subscribe(setToasts)
    return unsub
  })

  return { toasts, toast }
}
