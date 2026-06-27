'use client'

import Link from 'next/link'
import { usePathname } from 'next/navigation'
import {
  LayoutDashboard, Building2, Users, KeyRound,
  Cpu, Gauge, BarChart3, Settings, Zap,
  Network, Box, FolderKanban, Shield, Activity,
} from 'lucide-react'
import { cn } from '@/lib/utils'

import type { LucideIcon } from 'lucide-react'

type NavItem =
  | { section: 'header'; label: string }
  | { section?: undefined; href: string; label: string; icon: LucideIcon }

const nav: NavItem[] = [
  { href: '/',          label: 'Dashboard',     icon: LayoutDashboard },

  { section: 'header',  label: 'INFERENCE' },
  { href: '/models',    label: 'Models',         icon: Cpu },
  { href: '/runtimes',  label: 'Runtimes',       icon: Activity },
  { href: '/ha',        label: 'High Availability', icon: Shield },
  { href: '/services',  label: 'AI Services',    icon: Box },

  { section: 'header',  label: 'ACCESS' },
  { href: '/orgs',      label: 'Organizations',  icon: Building2 },
  { href: '/teams',     label: 'Teams',          icon: Users },
  { href: '/projects',  label: 'Projects',       icon: FolderKanban },
  { href: '/api-keys',  label: 'API Keys',       icon: KeyRound },

  { section: 'header',  label: 'INFRASTRUCTURE' },
  { href: '/cluster',   label: 'Cluster',        icon: Network },

  { section: 'header',  label: 'MONITORING' },
  { href: '/usage',     label: 'Usage',          icon: BarChart3 },

  { href: '/settings',  label: 'Settings',       icon: Settings },
]

export function Sidebar() {
  const path = usePathname()
  return (
    <aside className="w-56 bg-gray-900 text-white flex flex-col shrink-0">
      {/* Logo */}
      <div className="flex items-center gap-2 px-5 py-4 border-b border-gray-700/60">
        <div className="w-7 h-7 rounded-md bg-blue-600 flex items-center justify-center shrink-0">
          <Zap className="w-4 h-4 text-white" />
        </div>
        <div className="flex flex-col leading-none">
          <span className="font-bold text-base tracking-tight">NexusLLM</span>
          <span className="text-[10px] text-gray-500">AI Infrastructure</span>
        </div>
      </div>

      {/* Navigation */}
      <nav className="flex-1 px-2 py-3 overflow-y-auto">
        {nav.map((item, i) => {
          if (item.section === 'header') {
            return (
              <p key={i} className="px-3 pt-4 pb-1 text-[10px] font-semibold tracking-widest text-gray-500 uppercase">
                {item.label}
              </p>
            )
          }
          const { href, label, icon: Icon } = item
          const active = path === href || (href !== '/' && path.startsWith(href))
          return (
            <Link
              key={href}
              href={href}
              className={cn(
                'group flex items-center gap-2.5 px-3 py-2 rounded-md text-sm font-medium transition-colors',
                active
                  ? 'bg-blue-600 text-white'
                  : 'text-gray-400 hover:bg-gray-800 hover:text-white'
              )}
            >
              {Icon && <Icon className="w-4 h-4 shrink-0" />}
              {label}
            </Link>
          )
        })}
      </nav>

      {/* Footer */}
      <div className="px-3 py-3 border-t border-gray-700/60 space-y-1">
        <a
          href="http://localhost:9100"
          target="_blank"
          rel="noreferrer"
          className="flex items-center gap-2 px-2 py-1.5 rounded-md text-xs text-gray-500 hover:text-gray-300 hover:bg-gray-800 transition-colors"
        >
          <Gauge className="w-3.5 h-3.5" />
          Prometheus
          <span className="ml-auto text-gray-600">→</span>
        </a>
        <p className="px-2 pt-1 text-[10px] text-gray-600">v0.1.0 · admin panel</p>
      </div>
    </aside>
  )
}
