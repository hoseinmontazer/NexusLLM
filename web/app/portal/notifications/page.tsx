'use client'

import React, { useState, useEffect } from 'react'
import { Bell, Check, Info, ShieldCheck, AlertTriangle } from 'lucide-react'

interface NotificationItem {
  id: string
  title: string
  message: string
  type: string
  read: boolean
  created_at: string
}

export default function NotificationsPage() {
  const [notifications, setNotifications] = useState<NotificationItem[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    fetchNotifications()
  }, [])

  const fetchNotifications = async () => {
    try {
      const res = await fetch('/api/admin/portal/v1/notifications')
      if (res.ok) {
        const data = await res.json()
        setNotifications(data.data || [])
      }
    } catch (e) {
      console.error(e)
    } finally {
      setLoading(false)
    }
  }

  const markAsRead = async (id: string) => {
    try {
      await fetch(`/api/admin/portal/v1/notifications/${id}/read`, { method: 'POST' })
      setNotifications((prev) =>
        prev.map((n) => (n.id === id ? { ...n, read: true } : n))
      )
    } catch (e) {
      console.error(e)
    }
  }

  return (
    <div className="max-w-5xl mx-auto p-6 space-y-6">
      <div className="flex items-center justify-between border-b border-gray-200 pb-4">
        <div>
          <h1 className="text-2xl font-bold text-gray-900 tracking-tight flex items-center gap-2">
            <Bell className="w-6 h-6 text-blue-600" />
            Developer Notifications
          </h1>
          <p className="text-xs text-gray-500 mt-1">
            System alerts, project provisioning updates, and request approvals.
          </p>
        </div>
      </div>

      {loading ? (
        <div className="p-8 text-center text-sm text-gray-500">Loading notifications...</div>
      ) : notifications.length === 0 ? (
        <div className="p-12 text-center border-2 border-dashed border-gray-200 rounded-2xl bg-white">
          <Bell className="w-8 h-8 text-gray-400 mx-auto mb-2" />
          <h3 className="text-sm font-semibold text-gray-900">No Notifications</h3>
          <p className="text-xs text-gray-500 mt-1">You will receive notifications when your access requests are reviewed or keys are updated.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {notifications.map((item) => (
            <div
              key={item.id}
              className={`p-4 rounded-xl border transition-all flex items-start gap-4 ${
                item.read ? 'bg-gray-50 border-gray-200' : 'bg-blue-50/40 border-blue-200 shadow-sm'
              }`}
            >
              <div className="p-2 rounded-lg bg-blue-100 text-blue-600 mt-0.5">
                {item.type === 'request_approved' ? (
                  <ShieldCheck className="w-5 h-5 text-emerald-600" />
                ) : item.type === 'request_rejected' ? (
                  <AlertTriangle className="w-5 h-5 text-red-600" />
                ) : (
                  <Info className="w-5 h-5" />
                )}
              </div>
              <div className="flex-1 min-w-0">
                <div className="flex items-center justify-between">
                  <h4 className="text-sm font-semibold text-gray-900">{item.title}</h4>
                  <span className="text-[11px] text-gray-400">
                    {new Date(item.created_at).toLocaleString()}
                  </span>
                </div>
                <p className="text-xs text-gray-600 mt-1 leading-relaxed">{item.message}</p>
              </div>
              {!item.read && (
                <button
                  onClick={() => markAsRead(item.id)}
                  className="px-2.5 py-1 text-xs text-blue-600 hover:bg-blue-100 rounded-md font-medium flex items-center gap-1 transition-colors"
                >
                  <Check className="w-3.5 h-3.5" />
                  Mark Read
                </button>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
