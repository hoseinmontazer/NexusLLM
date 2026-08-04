'use client'

import React, { createContext, useContext, useState, useEffect } from 'react'
import { AuthUser, loginUser as apiLogin, registerUser as apiRegister } from '@/lib/api'

interface AuthContextType {
  user: AuthUser | null
  loading: boolean
  login: (email: string, password: string, isAdmin?: boolean) => Promise<void>
  register: (email: string, password: string, name?: string, orgName?: string) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthContextType | undefined>(undefined)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    try {
      const storedToken = localStorage.getItem('nexus_token')
      const storedUser = localStorage.getItem('nexus_user')
      if (storedToken && storedUser) {
        setUser(JSON.parse(storedUser))
      }
    } catch {
      localStorage.removeItem('nexus_token')
      localStorage.removeItem('nexus_user')
    } finally {
      setLoading(false)
    }
  }, [])

  const login = async (email: string, password: string, isAdmin = false) => {
    const userData = await apiLogin(email, password, isAdmin)
    localStorage.setItem('nexus_token', userData.token)
    localStorage.setItem('nexus_user', JSON.stringify(userData))
    setUser(userData)
  }

  const register = async (email: string, password: string, name?: string, orgName?: string) => {
    const userData = await apiRegister(email, password, name, orgName)
    localStorage.setItem('nexus_token', userData.token)
    localStorage.setItem('nexus_user', JSON.stringify(userData))
    setUser(userData)
  }

  const logout = () => {
    localStorage.removeItem('nexus_token')
    localStorage.removeItem('nexus_user')
    setUser(null)
  }

  return (
    <AuthContext.Provider value={{ user, loading, login, register, logout }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const context = useContext(AuthContext)
  if (!context) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return context
}
