'use client';

import React, { useState, useEffect } from 'react';

interface ProjectOption {
  id: string;
  name: string;
  environment: string;
}

export default function DeveloperAPIKeysPage() {
  const [projects, setProjects] = useState<ProjectOption[]>([]);
  const [projectId, setProjectId] = useState('');
  const [keyName, setKeyName] = useState('');
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [rotatedSecret, setRotatedSecret] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    const fetchProjects = async () => {
      try {
        const res = await fetch('/portal/v1/projects');
        if (res.ok) {
          const data = await res.json();
          const list = data.data || [];
          setProjects(list);
          if (list.length > 0 && !projectId) {
            setProjectId(list[0].id);
          }
        }
      } catch (err) {
        console.error(err);
      }
    };
    fetchProjects();
  }, []);

  const handleCreateKey = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setCreatedSecret(null);
    const targetProjId = projectId || (projects.length > 0 ? projects[0].id : 'demo-project-id');
    try {
      const res = await fetch(`/portal/v1/projects/${targetProjId}/api-keys`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: keyName || 'Developer Key' }),
      });
      if (res.ok) {
        const data = await res.json();
        setCreatedSecret(data.api_key_secret);
        setKeyName('');
      }
    } catch (err) {
      console.error(err);
    } finally {
      setLoading(false);
    }
  };

  const handleRotateKey = async (keyId: string) => {
    setLoading(true);
    setRotatedSecret(null);
    try {
      const res = await fetch(`/portal/v1/api-keys/${keyId}/rotate`, { method: 'POST' });
      if (res.ok) {
        const data = await res.json();
        setRotatedSecret(data.new_api_key_secret);
      }
    } catch (err) {
      console.error(err);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Developer API Key Management</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Generate project keys, perform zero-downtime key rotation (24h grace period), and manage key access. Plaintext secrets are revealed ONCE.
      </p>

      {/* Secret Reveal Banner */}
      {createdSecret && (
        <div style={{ background: 'rgba(16, 185, 129, 0.15)', border: '1px solid #10b981', color: '#34d399', padding: '20px', borderRadius: '12px', marginBottom: '28px' }}>
          <div style={{ fontWeight: 700, fontSize: '16px', marginBottom: '6px' }}>⚠️ Save Your API Key Secret Immediately</div>
          <p style={{ fontSize: '13px', color: '#e2e8f0', margin: '0 0 12px 0' }}>
            This secret will NEVER be displayed again. If lost, you must rotate the key.
          </p>
          <div style={{ background: '#0f172a', border: '1px solid #334155', padding: '12px', borderRadius: '8px', fontFamily: 'monospace', fontSize: '15px', color: '#60a5fa', wordBreak: 'break-all' }}>
            {createdSecret}
          </div>
        </div>
      )}

      {rotatedSecret && (
        <div style={{ background: 'rgba(59, 130, 246, 0.15)', border: '1px solid #3b82f6', color: '#60a5fa', padding: '20px', borderRadius: '12px', marginBottom: '28px' }}>
          <div style={{ fontWeight: 700, fontSize: '16px', marginBottom: '6px' }}>🔄 Zero-Downtime Key Rotation Successful</div>
          <p style={{ fontSize: '13px', color: '#e2e8f0', margin: '0 0 12px 0' }}>
            Your old API key remains active for a 24-hour grace period. Copy your new secret below:
          </p>
          <div style={{ background: '#0f172a', border: '1px solid #334155', padding: '12px', borderRadius: '8px', fontFamily: 'monospace', fontSize: '15px', color: '#a855f7', wordBreak: 'break-all' }}>
            {rotatedSecret}
          </div>
        </div>
      )}

      {/* Create Key Card */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '28px', marginBottom: '40px' }}>
        <h2 style={{ fontSize: '18px', fontWeight: 600, marginTop: 0, marginBottom: '16px' }}>Create Additional API Key</h2>
        <form onSubmit={handleCreateKey} style={{ display: 'grid', gridTemplateColumns: '1fr 1fr auto', gap: '16px', alignItems: 'end' }}>
          <div>
            <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Target Project</label>
            {projects.length > 0 ? (
              <select
                value={projectId}
                onChange={(e) => setProjectId(e.target.value)}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                required
              >
                {projects.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name} ({p.environment}) — {p.id.slice(0, 8)}...
                  </option>
                ))}
              </select>
            ) : (
              <input
                type="text"
                value={projectId}
                onChange={(e) => setProjectId(e.target.value)}
                placeholder="Enter Project ID or create a project first..."
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                required
              />
            )}
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Key Label / Name</label>
            <input
              type="text"
              value={keyName}
              onChange={(e) => setKeyName(e.target.value)}
              placeholder="e.g. Staging Backend Pipeline"
              style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              required
            />
          </div>
          <button
            type="submit"
            disabled={loading}
            style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', height: '42px' }}
          >
            {loading ? 'Creating...' : '+ Create Key'}
          </button>
        </form>
      </div>

      {/* Active Keys List */}
      <h2 style={{ fontSize: '18px', fontWeight: 600, marginBottom: '16px' }}>Active API Keys & Rotation</h2>
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>Key Name</th>
              <th style={{ padding: '14px 20px' }}>Masked Key Prefix</th>
              <th style={{ padding: '14px 20px' }}>Status</th>
              <th style={{ padding: '14px 20px' }}>Created At</th>
              <th style={{ padding: '14px 20px' }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr style={{ borderBottom: '1px solid #334155' }}>
              <td style={{ padding: '14px 20px', fontWeight: 600 }}>Default Auto-Provisioned Key</td>
              <td style={{ padding: '14px 20px', fontFamily: 'monospace', color: '#60a5fa' }}>nxs_a1b2c3d4...</td>
              <td style={{ padding: '14px 20px' }}>
                <span style={{ padding: '4px 10px', borderRadius: '12px', fontSize: '12px', fontWeight: 600, background: 'rgba(16, 185, 129, 0.2)', color: '#34d399' }}>ACTIVE</span>
              </td>
              <td style={{ padding: '14px 20px', color: '#94a3b8' }}>Just now</td>
              <td style={{ padding: '14px 20px' }}>
                <button
                  onClick={() => handleRotateKey('demo-key-id')}
                  style={{ background: '#334155', color: '#f8fafc', border: 'none', padding: '6px 14px', borderRadius: '6px', fontSize: '12px', fontWeight: 600, cursor: 'pointer' }}
                >
                  🔄 Rotate (Zero-Downtime)
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  );
}
