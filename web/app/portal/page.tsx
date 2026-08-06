'use client';

import React, { useState, useEffect } from 'react';
import Link from 'next/link';
import { useAuth } from '@/lib/auth-context';

export default function DeveloperPortalDashboard() {
  const { user } = useAuth();
  const [projectsCount, setProjectsCount] = useState(0);
  const [pendingRequestsCount, setPendingRequestsCount] = useState(0);
  const [grantedModelsCount, setGrantedModelsCount] = useState(0);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    async function fetchOverviewStats() {
      setLoading(true);
      try {
        const token = typeof window !== 'undefined' ? localStorage.getItem('nexus_token') : null;
        const headers: Record<string, string> = {};
        if (token) {
          headers['Authorization'] = `Bearer ${token}`;
        }

        const orgParam = user?.org_id ? `?org_id=${user.org_id}` : '';

        const [projRes, reqRes] = await Promise.all([
          fetch(`/portal/v1/projects${orgParam}`, { headers }),
          fetch(`/portal/v1/requests${orgParam}`, { headers }),
        ]);

        let projectList: any[] = [];
        if (projRes.ok) {
          const projData = await projRes.json();
          projectList = projData.data || [];
          setProjectsCount(projectList.length);
        } else {
          setProjectsCount(0);
        }

        if (reqRes.ok) {
          const reqData = await reqRes.json();
          const reqList = reqData.data || [];
          const pending = reqList.filter(
            (r: any) => r.status === 'pending' || r.status === 'submitted' || r.status === 'pending_review'
          );
          setPendingRequestsCount(pending.length);
        } else {
          setPendingRequestsCount(0);
        }

        if (projectList.length > 0) {
          const modelPromises = projectList.map((p: any) =>
            fetch(`/portal/v1/models?project_id=${p.id}`, { headers })
              .then((res) => (res.ok ? res.json() : { data: [] }))
              .catch(() => ({ data: [] }))
          );
          const modelResults = await Promise.all(modelPromises);
          const uniqueModels = new Set<string>();
          modelResults.forEach((res: any) => {
            (res.data || []).forEach((m: any) => {
              if (m.name) uniqueModels.add(m.name);
            });
          });
          setGrantedModelsCount(uniqueModels.size);
        } else {
          setGrantedModelsCount(0);
        }
      } catch (e) {
        console.error('Failed to fetch portal overview metrics:', e);
      } finally {
        setLoading(false);
      }
    }

    fetchOverviewStats();
  }, [user]);

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '40px' }}>
        <div>
          <h1 style={{ fontSize: '28px', fontWeight: 700, margin: 0, background: 'linear-gradient(135deg, #60a5fa 0%, #a855f7 100%)', WebkitBackgroundClip: 'text', WebkitTextFillColor: 'transparent' }}>
            Developer Self-Service Portal
          </h1>
          <p style={{ color: '#94a3b8', fontSize: '14px', marginTop: '6px' }}>
            Manage projects, request model access, rotate API keys, and monitor real-time AI usage.
          </p>
        </div>
        <div style={{ display: 'flex', gap: '12px' }}>
          <Link href="/portal/requests" style={{ textDecoration: 'none', background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', boxShadow: '0 4px 14px rgba(59, 130, 246, 0.35)' }}>
            + Request Model Access
          </Link>
          <Link href="/portal/projects" style={{ textDecoration: 'none', background: '#1e293b', color: '#cbd5e1', border: '1px solid #334155', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px' }}>
            New Project
          </Link>
        </div>
      </div>

      {/* Metrics Row */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: '20px', marginBottom: '40px' }}>
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px' }}>
          <span style={{ fontSize: '13px', color: '#94a3b8', fontWeight: 600 }}>Active Projects</span>
          <div style={{ fontSize: '32px', fontWeight: 700, color: '#f8fafc', marginTop: '8px' }}>{loading ? '...' : projectsCount}</div>
        </div>
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px' }}>
          <span style={{ fontSize: '13px', color: '#94a3b8', fontWeight: 600 }}>Pending Access Requests</span>
          <div style={{ fontSize: '32px', fontWeight: 700, color: '#f59e0b', marginTop: '8px' }}>{loading ? '...' : pendingRequestsCount}</div>
        </div>
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px' }}>
          <span style={{ fontSize: '13px', color: '#94a3b8', fontWeight: 600 }}>Granted Models</span>
          <div style={{ fontSize: '32px', fontWeight: 700, color: '#10b981', marginTop: '8px' }}>{loading ? '...' : grantedModelsCount}</div>
        </div>
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px' }}>
          <span style={{ fontSize: '13px', color: '#94a3b8', fontWeight: 600 }}>Monthly Quota Status</span>
          <div style={{ fontSize: '32px', fontWeight: 700, color: '#60a5fa', marginTop: '8px' }}>Healthy</div>
        </div>
      </div>

      {/* Quick Navigation Cards */}
      <h2 style={{ fontSize: '18px', fontWeight: 600, color: '#e2e8f0', marginBottom: '20px' }}>Self-Service Modules</h2>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: '24px' }}>
        <Link href="/portal/projects" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>📁 Projects & Environments</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Create projects (Development, Staging, Production), track monthly token expectations, and organize keys.
          </p>
        </Link>
        <Link href="/portal/requests" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>🚀 Access Requests</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Submit self-service requests for Public Models and Cloud Providers with use cases and RPM/TPM limits.
          </p>
        </Link>
        <Link href="/portal/api-keys" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>🔑 API Key Management</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Generate API keys, perform zero-downtime key rotation with 24h grace periods, and view masked keys.
          </p>
        </Link>
        <Link href="/portal/models" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>🤖 Granted Models & Providers</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Inspect authorized Public Models and Virtual Catalog Models granted specifically to your projects.
          </p>
        </Link>
        <Link href="/portal/usage" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>📊 Usage & Quota Analytics</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Real-time charts for RPM, TPM, daily/monthly budgets, 429 rate-limit counts, and response latency.
          </p>
        </Link>
        <Link href="/portal/admin-queue" style={{ textDecoration: 'none', background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', transition: 'all 0.2s' }}>
          <div style={{ fontSize: '20px', marginBottom: '8px' }}>🛡️ Admin Approval Queue</div>
          <p style={{ color: '#94a3b8', fontSize: '13px', lineHeight: 1.5 }}>
            Review pending developer access requests, inspect risk indicators, and trigger automatic provisioning.
          </p>
        </Link>
      </div>
    </div>
  );
}
