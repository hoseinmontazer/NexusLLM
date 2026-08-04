'use client';

import React, { useState, useEffect } from 'react';

interface PendingRequest {
  id: string;
  project_id: string;
  project_name: string;
  status: string;
  requested_models: string;
  requested_providers: string;
  business_use_case: string;
  expected_rpm: number;
  expected_tpm: number;
  created_at: string;
}

export default function AdminReviewQueuePage() {
  const [pending, setPending] = useState<PendingRequest[]>([]);
  const [loading, setLoading] = useState(true);
  const [reviewingId, setReviewingId] = useState<string | null>(null);
  const [notes, setNotes] = useState('');
  const [provisionResult, setProvisionResult] = useState<any>(null);

  const fetchPending = async () => {
    try {
      const res = await fetch('/admin/v1/portal/requests/pending');
      if (res.ok) {
        const data = await res.json();
        setPending(data.data || []);
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchPending();
  }, []);

  const handleReview = async (id: string, action: 'approve' | 'reject') => {
    setReviewingId(id);
    setProvisionResult(null);
    try {
      const res = await fetch(`/admin/v1/portal/requests/${id}/review`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          action: action,
          review_notes: notes || 'Admin approved access request',
        }),
      });
      if (res.ok) {
        const data = await res.json();
        setProvisionResult(data);
        fetchPending();
      }
    } catch (e) {
      console.error(e);
    } finally {
      setReviewingId(null);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Admin Approval & Automatic Provisioning Queue</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Review pending developer access requests, inspect risk indicators, and trigger one-click automatic permission, policy, and API key provisioning.
      </p>

      {provisionResult && (
        <div style={{ background: 'rgba(16, 185, 129, 0.15)', border: '1px solid #10b981', color: '#34d399', padding: '20px', borderRadius: '12px', marginBottom: '28px' }}>
          <div style={{ fontWeight: 700, fontSize: '16px', marginBottom: '6px' }}>⚡ Automatic Provisioning Complete</div>
          <p style={{ fontSize: '13px', color: '#e2e8f0', margin: '0 0 12px 0' }}>
            Permissions granted, rate limits applied, and Project API key generated automatically.
          </p>
          <div style={{ fontSize: '13px', color: '#cbd5e1' }}>
            <div><strong>Provisioned Key ID:</strong> {provisionResult.provisioned_api_key_id}</div>
            <div><strong>Key Prefix:</strong> {provisionResult.api_key_prefix}</div>
            <div><strong>RPM Limit:</strong> {provisionResult.rpm_limit} | <strong>TPM Limit:</strong> {provisionResult.tpm_limit}</div>
          </div>
        </div>
      )}

      {/* Pending Table */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>Project Name</th>
              <th style={{ padding: '14px 20px' }}>Requested Models & Providers</th>
              <th style={{ padding: '14px 20px' }}>Use Case & Requirements</th>
              <th style={{ padding: '14px 20px' }}>Requested Limits</th>
              <th style={{ padding: '14px 20px' }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>Loading review queue...</td></tr>
            ) : pending.length === 0 ? (
              <tr><td colSpan={5} style={{ padding: '20px', textAlign: 'center', color: '#94a3b8' }}>No pending access requests in queue.</td></tr>
            ) : (
              pending.map((item) => (
                <tr key={item.id} style={{ borderBottom: '1px solid #334155' }}>
                  <td style={{ padding: '14px 20px', fontWeight: 600 }}>{item.project_name}</td>
                  <td style={{ padding: '14px 20px' }}>
                    <div style={{ color: '#60a5fa', fontWeight: 500 }}>Models: {item.requested_models}</div>
                    <div style={{ color: '#a855f7', fontSize: '12px', marginTop: '4px' }}>Providers: {item.requested_providers}</div>
                  </td>
                  <td style={{ padding: '14px 20px', color: '#cbd5e1', maxWidth: '300px' }}>{item.business_use_case || 'Standard LLM Integration'}</td>
                  <td style={{ padding: '14px 20px' }}>{item.expected_rpm} RPM / {item.expected_tpm} TPM</td>
                  <td style={{ padding: '14px 20px' }}>
                    <div style={{ display: 'flex', gap: '8px' }}>
                      <button
                        onClick={() => handleReview(item.id, 'approve')}
                        disabled={reviewingId === item.id}
                        style={{ background: 'linear-gradient(135deg, #10b981 0%, #059669 100%)', color: '#fff', border: 'none', padding: '6px 14px', borderRadius: '6px', fontSize: '12px', fontWeight: 600, cursor: 'pointer' }}
                      >
                        {reviewingId === item.id ? 'Provisioning...' : 'Approve & Provision'}
                      </button>
                      <button
                        onClick={() => handleReview(item.id, 'reject')}
                        disabled={reviewingId === item.id}
                        style={{ background: '#ef4444', color: '#fff', border: 'none', padding: '6px 14px', borderRadius: '6px', fontSize: '12px', fontWeight: 600, cursor: 'pointer' }}
                      >
                        Reject
                      </button>
                    </div>
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
