'use client'

import Link from 'next/link'
import { usePathname, useRouter } from 'next/navigation'
import {
  LayoutDashboard, Building2, Users, KeyRound,
  Cpu, Gauge, BarChart3, Settings, Zap,
  Network, Shield, Activity, Globe, FolderKanban, LogOut, Bell, FileText
} from 'lucide-react'
import { cn } from '@/lib/utils'
import { useAuth } from '@/lib/auth-context'

import type { LucideIcon } from 'lucide-react'

type NavItem =
  | { section: 'header'; label: string }
  | { section?: undefined; href: string; label: string; icon: LucideIcon }

const adminNav: NavItem[] = [
  { href: '/',          label: 'Dashboard',     icon: LayoutDashboard },

  { section: 'header',  label: 'INFERENCE' },
  { href: '/models',     label: 'Models',           icon: Cpu },
  { href: '/providers',  label: 'Cloud Providers',  icon: Globe },
  { href: '/runtimes',   label: 'Runtimes',         icon: Activity },
  { href: '/ha',         label: 'High Availability', icon: Shield },

  { section: 'header',  label: 'EXECUTION' },
  { href: '/projects',  label: 'Projects',       icon: FolderKanban },
  { href: '/api-keys',  label: 'API Keys',       icon: KeyRound },

  { section: 'header',  label: 'DEVELOPER PORTAL' },
  { href: '/portal',            label: 'Portal Overview',   icon: FolderKanban },
  { href: '/portal/requests',   label: 'Access Requests',   icon: Zap },
  { href: '/portal/admin-queue',label: 'Admin Review Queue',icon: Shield },

  { section: 'header',  label: 'GOVERNANCE' },
  { href: '/orgs',      label: 'Organizations',  icon: Building2 },
  { href: '/teams',     label: 'Teams',          icon: Users },

  { section: 'header',  label: 'INFRASTRUCTURE' },
  { href: '/cluster',   label: 'Cluster',        icon: Network },

  { section: 'header',  label: 'MONITORING' },
  { href: '/usage',     label: 'Usage',          icon: BarChart3 },

  { href: '/settings',  label: 'Settings',       icon: Settings },
]

const developerNav: NavItem[] = [
  { section: 'header',  label: 'DEVELOPER PORTAL' },
  { href: '/portal',            label: 'Portal Overview',   icon: LayoutDashboard },
  { href: '/portal/requests',   label: 'Access Requests',   icon: Zap },
  { href: '/portal/projects',   label: 'My Projects',       icon: FolderKanban },
  { href: '/portal/api-keys',   label: 'My API Keys',       icon: KeyRound },
  { href: '/portal/models',     label: 'Granted Models',    icon: Cpu },
  { href: '/portal/usage',      label: 'Usage Analytics',   icon: BarChart3 },
  { href: '/portal/notifications', label: 'Notifications',  icon: Bell },
]

export function Sidebar() {
  const path = usePathname()
  const router = useRouter()
  const { user, logout } = useAuth()

  const isAdmin = user?.role === 'admin'
  const navItems = isAdmin ? adminNav : developerNav

  const handleLogout = () => {
    logout()
    router.push('/login')
  }

  // Hide sidebar on login/register pages
  if (['/login', '/register'].includes(path)) {
    return null
  }

  return (
    <aside className="w-56 bg-gray-900 text-white flex flex-col shrink-0 border-r border-gray-800">
      {/* Logo */}
      <div className="flex items-center gap-2 px-5 py-4 border-b border-gray-700/60">
        <div className="w-7 h-7 rounded-md bg-blue-600 flex items-center justify-center shrink-0">
          <Zap className="w-4 h-4 text-white" />
        </div>
        <div className="flex flex-col leading-none">
          <span className="font-bold text-base tracking-tight">NexusLLM</span>
          <span className="text-[10px] text-gray-400">
            {isAdmin ? 'Platform Admin' : 'Developer Portal'}
          </span>
        </div>
      </div>

      {/* Navigation */}
      <nav className="flex-1 px-2 py-3 overflow-y-auto">
        {navItems.map((item, i) => {
          if (item.section === 'header') {
            return (
              <p key={i} className="px-3 pt-4 pb-1 text-[10px] font-semibold tracking-widest text-gray-500 uppercase">
                {item.label}
              </p>
            )
          }
          const { href, label, icon: Icon } = item
          const active = path === href || (href !== '/' && path.startsWith(href))
          const IconComponent = Icon as React.ElementType
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
              {IconComponent && <IconComponent className="w-4 h-4 shrink-0" />}
              {label}
            </Link>
          )
        })}
      </nav>

      {/* User Footer Profile & Logout */}
      <div className="px-3 py-3 border-t border-gray-700/60 space-y-2">
        {user && (
          <div className="flex items-center justify-between px-2 py-1 bg-gray-950/60 border border-gray-800 rounded-lg">
            <div className="flex flex-col min-w-0 pr-1">
              <span className="text-xs font-semibold text-gray-200 truncate">{user.email}</span>
              <span className={cn(
                "text-[10px] font-semibold uppercase tracking-wider",
                isAdmin ? "text-amber-400" : "text-emerald-400"
              )}>
                {user.role || 'Member'}
              </span>
            </div>
            <button
              onClick={handleLogout}
              title="Sign Out"
              className="p-1.5 rounded-md text-gray-400 hover:text-red-400 hover:bg-gray-800 transition-colors"
            >
              <LogOut className="w-4 h-4" />
            </button>
          </div>
        )}

        {isAdmin && (
          <a
            href="http://localhost:9100"
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-2 px-2 py-1.5 rounded-md text-xs text-gray-500 hover:text-gray-300 hover:bg-gray-800 transition-colors"
          >
            <Gauge className="w-3.5 h-3.5" />
            Prometheus Metrics
            <span className="ml-auto text-gray-600">→</span>
          </a>
        )}
      </div>
    </aside>
  )
}
