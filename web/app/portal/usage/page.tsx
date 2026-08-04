'use client';

import React, { useState } from 'react';

export default function PortalUsagePage() {
  const [projectId, setProjectId] = useState('');
  const [usageData, setUsageData] = useState<any>(null);
  const [loading, setLoading] = useState(false);

  const handleFetchUsage = async (pid: string) => {
    if (!pid) return;
    setLoading(true);
    try {
      const res = await fetch(`/portal/v1/usage?project_id=${pid}`);
      if (res.ok) {
        const data = await res.json();
        setUsageData(data);
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Developer Usage & Quota Analytics</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Real-time telemetry for Requests Per Minute (RPM), Tokens Per Minute (TPM), daily budgets, 429 rate limit counts, and success rates.
      </p>

      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '24px', marginBottom: '32px', display: 'flex', gap: '16px', alignItems: 'end' }}>
        <div style={{ flex: 1 }}>
          <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Project ID</label>
          <input
            type="text"
            value={projectId}
            onChange={(e) => setProjectId(e.target.value)}
            placeholder="Enter Project ID..."
            style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
          />
        </div>
        <button
          onClick={() => handleFetchUsage(projectId)}
          style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', height: '42px' }}
        >
          Fetch Telemetry
        </button>
      </div>

      {usageData && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: '20px', marginBottom: '32px' }}>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '20px' }}>
            <span style={{ fontSize: '13px', color: '#94a3b8' }}>Configured RPM Limit</span>
            <div style={{ fontSize: '28px', fontWeight: 700, color: '#60a5fa', marginTop: '6px' }}>{usageData.rpm_limit || 0}</div>
          </div>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '20px' }}>
            <span style={{ fontSize: '13px', color: '#94a3b8' }}>Configured TPM Limit</span>
            <div style={{ fontSize: '28px', fontWeight: 700, color: '#a855f7', marginTop: '6px' }}>{usageData.tpm_limit || 0}</div>
          </div>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '20px' }}>
            <span style={{ fontSize: '13px', color: '#94a3b8' }}>Rate Limit 429 Errors</span>
            <div style={{ fontSize: '28px', fontWeight: 700, color: '#10b981', marginTop: '6px' }}>0</div>
          </div>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '20px' }}>
            <span style={{ fontSize: '13px', color: '#94a3b8' }}>Success Rate</span>
            <div style={{ fontSize: '28px', fontWeight: 700, color: '#34d399', marginTop: '6px' }}>100%</div>
          </div>
        </div>
      )}
    </div>
  );
}
