'use client';

import React, { useState, useEffect } from 'react';

interface AccessRequest {
  id: string;
  project_id: string;
  status: string;
  requested_models: string;
  requested_providers: string;
  business_use_case: string;
  expected_rpm: number;
  expected_tpm: number;
  created_at: string;
}

export default function AccessRequestsPage() {
  const [requests, setRequests] = useState<AccessRequest[]>([]);
  const [loading, setLoading] = useState(true);

  // Form state
  const [projectId, setProjectId] = useState('');
  const [selectedModels, setSelectedModels] = useState<string[]>(['gemma-2', 'llama-3']);
  const [selectedProviders, setSelectedProviders] = useState<string[]>(['openrouter', 'openai/gpt-5']);
  const [useCase, setUseCase] = useState('');
  const [expectedRPM, setExpectedRPM] = useState(600);
  const [expectedTPM, setExpectedTPM] = useState(50000);
  const [submitting, setSubmitting] = useState(false);
  const [successMsg, setSuccessMsg] = useState('');

  const fetchRequests = async () => {
    try {
      const res = await fetch('/portal/v1/requests');
      if (res.ok) {
        const data = await res.json();
        setRequests(data.data || []);
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchRequests();
  }, []);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setSuccessMsg('');
    try {
      const res = await fetch('/portal/v1/requests', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_id: projectId || 'demo-project-id',
          requested_models: selectedModels,
          requested_providers: selectedProviders,
          business_use_case: useCase,
          expected_rpm: Number(expectedRPM),
          expected_tpm: Number(expectedTPM),
        }),
      });
      if (res.ok) {
        setSuccessMsg('Access Request submitted successfully! Sent to Admin Review Queue.');
        fetchRequests();
      }
    } catch (e) {
      console.error(e);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '24px' }}>Self-Service Model & Provider Access Request</h1>

      {/* Access Request Form */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '28px', marginBottom: '40px' }}>
        <h2 style={{ fontSize: '18px', fontWeight: 600, marginTop: 0, marginBottom: '16px' }}>New Access Request</h2>
        {successMsg && (
          <div style={{ background: 'rgba(16, 185, 129, 0.15)', border: '1px solid #10b981', color: '#34d399', padding: '12px 16px', borderRadius: '8px', marginBottom: '20px', fontSize: '14px' }}>
            {successMsg}
          </div>
        )}
        <form onSubmit={handleSubmit} style={{ display: 'grid', gap: '20px' }}>
          <div>
            <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Target Project ID</label>
            <input
              type="text"
              value={projectId}
              onChange={(e) => setProjectId(e.target.value)}
              placeholder="e.g. 550e8400-e29b-41d4-a716-446655440000"
              style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              required
            />
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '20px' }}>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Requested Public Models</label>
              <input
                type="text"
                value={selectedModels.join(', ')}
                onChange={(e) => setSelectedModels(e.target.value.split(',').map((s) => s.trim()))}
                placeholder="gemma-2, llama-3, whisper"
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Requested Cloud Providers / Virtual Models</label>
              <input
                type="text"
                value={selectedProviders.join(', ')}
                onChange={(e) => setSelectedProviders(e.target.value.split(',').map((s) => s.trim()))}
                placeholder="openrouter, openai/gpt-5, claude-sonnet"
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Business Use Case & Traffic Requirements</label>
            <textarea
              rows={3}
              value={useCase}
              onChange={(e) => setUseCase(e.target.value)}
              placeholder="Describe your AI feature, target throughput, context size requirement..."
              style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
            />
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '20px' }}>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Expected RPM (Requests Per Minute)</label>
              <input
                type="number"
                value={expectedRPM}
                onChange={(e) => setExpectedRPM(Number(e.target.value))}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Expected TPM (Tokens Per Minute)</label>
              <input
                type="number"
                value={expectedTPM}
                onChange={(e) => setExpectedTPM(Number(e.target.value))}
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>
          </div>

          <button
            type="submit"
            disabled={submitting}
            style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '12px 24px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', justifySelf: 'start' }}
          >
            {submitting ? 'Submitting...' : 'Submit Request for Admin Review'}
          </button>
        </form>
      </div>

      {/* Request History Table */}
      <h2 style={{ fontSize: '18px', fontWeight: 600, marginBottom: '16px' }}>Request History & Approval Status</h2>
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>Request ID</th>
              <th style={{ padding: '14px 20px' }}>Project ID</th>
              <th style={{ padding: '14px 20px' }}>Requested Models</th>
              <th style={{ padding: '14px 20px' }}>Status</th>
              <th style={{ padding: '14px 20px' }}>RPM / TPM</th>
              <th style={{ padding: '14px 20px' }}>Submitted At</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={6} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>Loading requests...</td></tr>
            ) : requests.length === 0 ? (
              <tr><td colSpan={6} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>No access requests submitted yet.</td></tr>
            ) : (
              requests.map((r) => (
                <tr key={r.id} style={{ borderBottom: '1px solid #334155' }}>
                  <td style={{ padding: '14px 20px', fontFamily: 'monospace' }}>{r.id.slice(0, 8)}...</td>
                  <td style={{ padding: '14px 20px', fontFamily: 'monospace' }}>{r.project_id.slice(0, 8)}...</td>
                  <td style={{ padding: '14px 20px' }}>{r.requested_models || 'None'}</td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{
                      padding: '4px 10px',
                      borderRadius: '12px',
                      fontSize: '12px',
                      fontWeight: 600,
                      background: r.status === 'approved' ? 'rgba(16, 185, 129, 0.2)' : r.status === 'rejected' ? 'rgba(239, 68, 68, 0.2)' : 'rgba(245, 158, 11, 0.2)',
                      color: r.status === 'approved' ? '#34d399' : r.status === 'rejected' ? '#f87171' : '#fbbf24',
                    }}>
                      {r.status.toUpperCase()}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px' }}>{r.expected_rpm} / {r.expected_tpm}</td>
                  <td style={{ padding: '14px 20px', color: '#94a3b8' }}>{new Date(r.created_at).toLocaleString()}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
