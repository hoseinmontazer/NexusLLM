'use client';

import React, { useState, useEffect } from 'react';

interface GrantedModel {
  name: string;
  display_name: string;
  backend_type: string;
  source: string;
}

interface ProjectOption {
  id: string;
  name: string;
  environment: string;
}

export default function ScopedModelsPage() {
  const [projects, setProjects] = useState<ProjectOption[]>([]);
  const [projectId, setProjectId] = useState('');
  const [models, setModels] = useState<GrantedModel[]>([]);
  const [loading, setLoading] = useState(false);

  const fetchModels = async (pid: string) => {
    if (!pid) return;
    setLoading(true);
    try {
      const res = await fetch(`/portal/v1/models?project_id=${pid}`);
      if (res.ok) {
        const data = await res.json();
        setModels(data.data || []);
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    const fetchProjects = async () => {
      try {
        const res = await fetch('/portal/v1/projects');
        if (res.ok) {
          const data = await res.json();
          const list = data.data || [];
          setProjects(list);
          if (list.length > 0) {
            setProjectId(list[0].id);
            fetchModels(list[0].id);
          }
        }
      } catch (err) {
        console.error(err);
      }
    };
    fetchProjects();
  }, []);

  const handleProjectChange = (pid: string) => {
    setProjectId(pid);
    fetchModels(pid);
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Granted Models & Providers (Scoped View)</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Inspect authorized Public Models and Virtual Catalog Models granted to your project. Hidden or unauthorized models are automatically omitted (zero catalog leakage).
      </p>

      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', marginBottom: '32px', display: 'flex', gap: '16px', alignItems: 'end' }}>
        <div style={{ flex: 1 }}>
          <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Select Project</label>
          {projects.length > 0 ? (
            <select
              value={projectId}
              onChange={(e) => handleProjectChange(e.target.value)}
              style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
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
              placeholder="Enter your Project ID..."
              style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
            />
          )}
        </div>
        <button
          onClick={() => fetchModels(projectId)}
          style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', height: '42px' }}
        >
          Load Granted Models
        </button>
      </div>

      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>Model Name / ID</th>
              <th style={{ padding: '14px 20px' }}>Display Name</th>
              <th style={{ padding: '14px 20px' }}>Backend Engine</th>
              <th style={{ padding: '14px 20px' }}>Source Type</th>
              <th style={{ padding: '14px 20px' }}>Status</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>Loading granted models...</td></tr>
            ) : models.length === 0 ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>Enter a valid Project ID above to inspect authorized models.</td></tr>
            ) : (
              models.map((m, idx) => (
                <tr key={idx} style={{ borderBottom: '1px solid #334155' }}>
                  <td style={{ padding: '14px 20px', fontWeight: 600, color: '#60a5fa' }}>{m.name}</td>
                  <td style={{ padding: '14px 20px' }}>{m.display_name || m.name}</td>
                  <td style={{ padding: '14px 20px', fontFamily: 'monospace' }}>{m.backend_type}</td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{ padding: '4px 10px', borderRadius: '12px', fontSize: '12px', fontWeight: 600, background: 'rgba(59, 130, 246, 0.2)', color: '#60a5fa' }}>
                      {m.source.toUpperCase()}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{ padding: '4px 10px', borderRadius: '12px', fontSize: '12px', fontWeight: 600, background: 'rgba(16, 185, 129, 0.2)', color: '#34d399' }}>
                      AUTHORIZED
                    </span>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
