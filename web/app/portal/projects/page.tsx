'use client';

import React, { useState, useEffect } from 'react';
import { useAuth } from '@/lib/auth-context';

interface ProjectItem {
  id: string;
  name: string;
  environment: string;
  expected_monthly_requests: number;
  expected_monthly_tokens: number;
  status: string;
  created_at: string;
}

export default function PortalProjectsPage() {
  const { user } = useAuth();
  const [projects, setProjects] = useState<ProjectItem[]>([]);
  const [loading, setLoading] = useState(true);

  // Form state
  const [orgId, setOrgId] = useState('');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [environment, setEnvironment] = useState('development');
  const [expectedReqs, setExpectedReqs] = useState(100000);
  const [expectedTokens, setExpectedTokens] = useState(5000000);
  const [creating, setCreating] = useState(false);

  useEffect(() => {
    if (user?.org_id) {
      setOrgId(user.org_id);
    }
  }, [user]);

  const fetchProjects = async () => {
    try {
      const url = user?.org_id ? `/portal/v1/projects?org_id=${user.org_id}` : '/portal/v1/projects';
      const res = await fetch(url);
      if (res.ok) {
        const data = await res.json();
        setProjects(data.data || []);
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchProjects();
  }, [user]);

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    setCreating(true);
    const targetOrgId = orgId || user?.org_id || 'default-org-id';
    try {
      const res = await fetch('/portal/v1/projects', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          organization_id: targetOrgId,
          name: name,
          description: description,
          environment: environment,
          expected_monthly_requests: Number(expectedReqs),
          expected_monthly_tokens: Number(expectedTokens),
        }),
      });
      if (res.ok) {
        setName('');
        setDescription('');
        fetchProjects();
      }
    } catch (e) {
      console.error(e);
    } finally {
      setCreating(false);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Developer Projects & Environment Management</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Organize your applications by environment (Development, Staging, Production), track monthly token expectations, and request model access.
      </p>

      {/* User Org Context Banner */}
      {user && (
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '16px 24px', marginBottom: '28px', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <div>
            <span style={{ color: '#94a3b8', fontSize: '13px', fontWeight: 600 }}>Logged in as: </span>
            <span style={{ color: '#60a5fa', fontWeight: 600, fontSize: '14px' }}>{user.email}</span>
          </div>
          <div style={{ background: '#0f172a', border: '1px solid #334155', padding: '6px 14px', borderRadius: '8px', fontSize: '13px' }}>
            <span style={{ color: '#94a3b8' }}>Organization ID: </span>
            <span style={{ color: '#a855f7', fontFamily: 'monospace', fontWeight: 600 }}>{user.org_id || 'Default Org'}</span>
          </div>
        </div>
      )}

      {/* Project Form */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '28px', marginBottom: '40px' }}>
        <h2 style={{ fontSize: '18px', fontWeight: 600, marginTop: 0, marginBottom: '16px' }}>Create New Project</h2>
        <form onSubmit={handleCreate} style={{ display: 'grid', gap: '20px' }}>
          <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: '20px' }}>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Project Name</label>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Customer Support AI Assistant"
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                required
              />
            </div>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Environment</label>
              <select
                value={environment}
                onChange={(e) => setEnvironment(e.target.value)}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              >
                <option value="development">Development</option>
                <option value="staging">Staging</option>
                <option value="production">Production</option>
              </select>
            </div>
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '20px' }}>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Expected Monthly Reqs</label>
              <input
                type="number"
                value={expectedReqs}
                onChange={(e) => setExpectedReqs(Number(e.target.value))}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Expected Monthly Tokens</label>
              <input
                type="number"
                value={expectedTokens}
                onChange={(e) => setExpectedTokens(Number(e.target.value))}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
          </div>

          <button
            type="submit"
            disabled={creating}
            style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '12px 24px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', justifySelf: 'start' }}
          >
            {creating ? 'Creating...' : '+ Create Project'}
          </button>
        </form>
      </div>

      {/* Projects List */}
      <h2 style={{ fontSize: '18px', fontWeight: 600, marginBottom: '16px' }}>Your Projects</h2>
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>Project Name</th>
              <th style={{ padding: '14px 20px' }}>Environment</th>
              <th style={{ padding: '14px 20px' }}>Monthly Expectation</th>
              <th style={{ padding: '14px 20px' }}>Status</th>
              <th style={{ padding: '14px 20px' }}>Created At</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>Loading projects...</td></tr>
            ) : projects.length === 0 ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>No projects created yet.</td></tr>
            ) : (
              projects.map((p) => (
                <tr key={p.id} style={{ borderBottom: '1px solid #334155' }}>
                  <td style={{ padding: '14px 20px', fontWeight: 600 }}>{p.name}</td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{ padding: '4px 10px', borderRadius: '12px', fontSize: '12px', fontWeight: 600, background: '#334155', color: '#e2e8f0' }}>
                      {p.environment.toUpperCase()}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px' }}>{p.expected_monthly_requests} reqs / {p.expected_monthly_tokens} tokens</td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{
                      padding: '4px 10px',
                      borderRadius: '12px',
                      fontSize: '12px',
                      fontWeight: 600,
                      background: p.status === 'active' ? 'rgba(16, 185, 129, 0.2)' : 'rgba(245, 158, 11, 0.2)',
                      color: p.status === 'active' ? '#34d399' : '#fbbf24',
                    }}>
                      {p.status.toUpperCase()}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px', color: '#94a3b8' }}>{new Date(p.created_at).toLocaleDateString()}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
