'use client'

import React, { useState } from 'react'
import { useAuth } from '@/lib/auth-context'
import { useRouter } from 'next/navigation'
import Link from 'next/link'
import { Zap, Shield, User, ArrowRight, Lock, KeyRound, AlertCircle } from 'lucide-react'

export default function LoginPage() {
  const { login } = useAuth()
  const router = useRouter()

  const [isAdmin, setIsAdmin] = useState(false)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSubmitting(true)
    try {
      await login(email, password, isAdmin)
      if (isAdmin) {
        router.push('/')
      } else {
        router.push('/portal')
      }
    } catch (err: any) {
      setError(err.message || 'Invalid credentials. Please check email and password.')
    } finally {
      setSubmitting(false)
    }
  }

  const handleFillAdmin = () => {
    setIsAdmin(true)
    setEmail('admin@nexusllm.io')
    setPassword('admin123')
  }

  return (
    <div className="min-h-screen bg-gray-950 text-white flex flex-col justify-center items-center px-4 py-12 relative overflow-hidden">
      {/* Background Glows */}
      <div className="absolute top-1/4 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[500px] h-[500px] bg-blue-600/10 rounded-full blur-3xl pointer-events-none" />
      <div className="absolute bottom-10 right-10 w-[300px] h-[300px] bg-indigo-600/10 rounded-full blur-3xl pointer-events-none" />

      {/* Main Card */}
      <div className="w-full max-w-md bg-gray-900/90 border border-gray-800 rounded-2xl shadow-2xl backdrop-blur-xl p-8 z-10">
        {/* Header Branding */}
        <div className="flex flex-col items-center mb-8 text-center">
          <div className="w-12 h-12 rounded-xl bg-gradient-to-tr from-blue-600 to-indigo-500 flex items-center justify-center shadow-lg shadow-blue-500/20 mb-3">
            <Zap className="w-6 h-6 text-white" />
          </div>
          <h1 className="text-2xl font-bold tracking-tight text-white">NexusLLM</h1>
          <p className="text-xs text-gray-400 mt-1">Enterprise AI Gateway & Infrastructure Platform</p>
        </div>

        {/* Role Toggle Tabs */}
        <div className="grid grid-cols-2 p-1 bg-gray-950 rounded-xl mb-6 border border-gray-800/80">
          <button
            type="button"
            onClick={() => { setIsAdmin(false); setError(null) }}
            className={`flex items-center justify-center gap-2 py-2 px-3 text-xs font-semibold rounded-lg transition-all ${
              !isAdmin
                ? 'bg-blue-600 text-white shadow-md'
                : 'text-gray-400 hover:text-white hover:bg-gray-900/60'
            }`}
          >
            <User className="w-3.5 h-3.5" />
            Developer Portal
          </button>
          <button
            type="button"
            onClick={() => { setIsAdmin(true); setError(null) }}
            className={`flex items-center justify-center gap-2 py-2 px-3 text-xs font-semibold rounded-lg transition-all ${
              isAdmin
                ? 'bg-blue-600 text-white shadow-md'
                : 'text-gray-400 hover:text-white hover:bg-gray-900/60'
            }`}
          >
            <Shield className="w-3.5 h-3.5" />
            Administrator
          </button>
        </div>

        {/* Error Alert */}
        {error && (
          <div className="mb-6 p-3.5 rounded-xl bg-red-500/10 border border-red-500/30 text-red-400 text-xs flex items-start gap-2.5">
            <AlertCircle className="w-4 h-4 shrink-0 mt-0.5" />
            <span>{error}</span>
          </div>
        )}

        {/* Login Form */}
        <form onSubmit={handleSubmit} className="space-y-4">
          <div>
            <label className="block text-xs font-medium text-gray-300 mb-1.5">
              Email Address
            </label>
            <div className="relative">
              <input
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder={isAdmin ? 'admin@nexusllm.io' : 'developer@company.com'}
                className="w-full bg-gray-950 border border-gray-800 rounded-xl px-4 py-2.5 text-sm text-white placeholder-gray-500 focus:outline-none focus:border-blue-500 focus:ring-1 focus:ring-blue-500 transition-colors"
              />
            </div>
          </div>

          <div>
            <label className="block text-xs font-medium text-gray-300 mb-1.5">
              Password
            </label>
            <div className="relative">
              <input
                type="password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                className="w-full bg-gray-950 border border-gray-800 rounded-xl px-4 py-2.5 text-sm text-white placeholder-gray-500 focus:outline-none focus:border-blue-500 focus:ring-1 focus:ring-blue-500 transition-colors"
              />
            </div>
          </div>

          <button
            type="submit"
            disabled={submitting}
            className="w-full mt-2 bg-blue-600 hover:bg-blue-500 active:bg-blue-700 text-white font-semibold py-2.5 px-4 rounded-xl text-sm transition-colors flex items-center justify-center gap-2 shadow-lg shadow-blue-600/20 disabled:opacity-50"
          >
            {submitting ? (
              <div className="w-4 h-4 border-2 border-white border-t-transparent rounded-full animate-spin" />
            ) : (
              <>
                Sign In to {isAdmin ? 'Admin Console' : 'Developer Portal'}
                <ArrowRight className="w-4 h-4" />
              </>
            )}
          </button>
        </form>

        {/* Quick Demo Hint */}
        {isAdmin && (
          <div className="mt-6 pt-4 border-t border-gray-800 text-center">
            <p className="text-[11px] text-gray-400 mb-2">Default Platform Admin Credentials:</p>
            <button
              type="button"
              onClick={handleFillAdmin}
              className="inline-flex items-center gap-1.5 text-xs text-blue-400 hover:text-blue-300 font-mono bg-blue-500/10 border border-blue-500/20 rounded-lg px-3 py-1.5 transition-colors"
            >
              <KeyRound className="w-3.5 h-3.5" />
              admin@nexusllm.io / admin123
            </button>
          </div>
        )}

        {/* Footer Link */}
        {!isAdmin && (
          <div className="mt-6 pt-4 border-t border-gray-800 text-center">
            <p className="text-xs text-gray-400">
              Don&apos;t have a workspace account?{' '}
              <Link href="/register" className="text-blue-400 hover:text-blue-300 font-semibold transition-colors">
                Register Developer Account
              </Link>
            </p>
          </div>
        )}
      </div>
    </div>
  )
}
