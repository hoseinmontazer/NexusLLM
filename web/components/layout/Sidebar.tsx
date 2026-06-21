'use client'

import Link from 'next/link'
import { usePathname } from 'next/navigation'
import {
  LayoutDashboard, Building2, Users, KeyRound,
  Cpu, Server, Gauge, BarChart3, Settings, Zap,
  Network, Box, MapPin,
} from 'lucide-react'
import { cn } from '@/lib/utils'

const nav = [
  { href: '/',           label: 'Dashboard',     icon: LayoutDashboard },
  { href: '/orgs',       label: 'Organizations',  icon: Building2 },
  { href: '/teams',      label: 'Teams',          icon: Users },
  { href: '/api-keys',   label: 'API Keys',       icon: KeyRound },
  { href: '/models',     label: 'Models',         icon: Cpu },
  { href: '/services',   label: 'AI Services',    icon: Box },
  { href: '/nodes',      label: 'Cluster Nodes',  icon: Network },
  { href: '/placement',  label: 'Placement',      icon: MapPin },
  { href: '/gpu',        label: 'GPU Inventory',  icon: Server },
  { href: '/usage',      label: 'Usage',          icon: BarChart3 },
  { href: '/settings',   label: 'Settings',       icon: Settings },
]

export function Sidebar() {
  const path = usePathname()
  return (
    <aside className="w-60 bg-gray-900 text-white flex flex-col">
      {/* Logo */}
      <div className="flex items-center gap-2 px-6 py-5 border-b border-gray-700">
        <Zap className="w-6 h-6 text-blue-400" />
        <span className="font-bold text-lg tracking-tight">NexusLLM</span>
      </div>

      {/* Navigation */}
      <nav className="flex-1 px-3 py-4 space-y-0.5">
        {nav.map(({ href, label, icon: Icon }) => {
          const active = path === href || (href !== '/' && path.startsWith(href))
          return (
            <Link
              key={href}
              href={href}
              className={cn(
                'flex items-center gap-3 px-3 py-2 rounded-md text-sm font-medium transition-colors',
                active
                  ? 'bg-blue-600 text-white'
                  : 'text-gray-400 hover:bg-gray-800 hover:text-white'
              )}
            >
              <Icon className="w-4 h-4" />
              {label}
            </Link>
          )
        })}
      </nav>

      {/* Footer */}
      <div className="px-6 py-4 border-t border-gray-700">
        <div className="flex items-center gap-2">
          <Gauge className="w-4 h-4 text-gray-500" />
          <a
            href="http://localhost:9100"
            target="_blank"
            rel="noreferrer"
            className="text-xs text-gray-500 hover:text-gray-300"
          >
            Prometheus →
          </a>
        </div>
      </div>
    </aside>
  )
}
