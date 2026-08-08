'use client';

import React, { useState, useEffect } from 'react';
import { useAuth } from '@/lib/auth-context';

interface ProfileData {
  id: string;
  email: string;
  name: string;
  role: string;
  org_id: string;
  org_name: string;
  active: boolean;
}

export default function DeveloperSettingsPage() {
  const { user } = useAuth();
  const [profile, setProfile] = useState<ProfileData | null>(null);
  const [loading, setLoading] = useState(true);

  // Form states
  const [name, setName] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ text: string; error: boolean } | null>(null);

  const fetchProfile = async () => {
    try {
      const res = await fetch('/portal/v1/profile');
      if (res.ok) {
        const data = await res.json();
        setProfile(data);
        setName(data.name || '');
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchProfile();
  }, [user]);

  const handleUpdateProfile = async (e: React.FormEvent) => {
    e.preventDefault();
    setMsg(null);

    if (newPassword && newPassword !== confirmPassword) {
      setMsg({ text: 'Passwords do not match.', error: true });
      return;
    }

    setSaving(true);
    try {
      const res = await fetch('/portal/v1/profile', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: name,
          new_password: newPassword || undefined,
        }),
      });

      if (res.ok) {
        setMsg({ text: 'Profile updated successfully!', error: false });
        setNewPassword('');
        setConfirmPassword('');
        fetchProfile();
      } else {
        const errData = await res.json();
        setMsg({ text: errData.error || 'Failed to update profile.', error: true });
      }
    } catch (e: any) {
      setMsg({ text: 'Error: ' + e.message, error: true });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      <h1 style={{ fontSize: '24px', fontWeight: 700, marginBottom: '8px' }}>Developer Profile & Account Settings</h1>
      <p style={{ color: '#94a3b8', fontSize: '14px', marginBottom: '32px' }}>
        Manage your profile preferences, display name, account password, and RBAC credentials.
      </p>

      {msg && (
        <div style={{
          background: msg.error ? 'rgba(239, 68, 68, 0.15)' : 'rgba(16, 185, 129, 0.15)',
          border: '1px solid ' + (msg.error ? '#ef4444' : '#10b981'),
          color: msg.error ? '#fca5a5' : '#34d399',
          padding: '14px 20px',
          borderRadius: '10px',
          marginBottom: '28px',
          fontSize: '14px'
        }}>
          {msg.text}
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 2fr', gap: '32px' }}>
        {/* Profile Card */}
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '28px' }}>
          <h2 style={{ fontSize: '18px', fontWeight: 600, marginTop: 0, marginBottom: '20px' }}>Account Details</h2>
          {loading ? (
            <div style={{ color: '#94a3b8' }}>Loading profile...</div>
          ) : (
            <div style={{ display: 'grid', gap: '16px', fontSize: '14px' }}>
              <div>
                <div style={{ color: '#94a3b8', fontSize: '12px', fontWeight: 600 }}>Email Address</div>
                <div style={{ fontWeight: 600, color: '#60a5fa', marginTop: '4px' }}>{profile?.email}</div>
              </div>
              <div>
                <div style={{ color: '#94a3b8', fontSize: '12px', fontWeight: 600 }}>RBAC Role</div>
                <div style={{ marginTop: '4px' }}>
                  <span style={{ padding: '4px 10px', borderRadius: '12px', fontSize: '12px', fontWeight: 600, background: '#334155', color: '#a855f7' }}>
                    {profile?.role?.toUpperCase() || 'MEMBER'}
                  </span>
                </div>
              </div>
              <div>
                <div style={{ color: '#94a3b8', fontSize: '12px', fontWeight: 600 }}>Organization</div>
                <div style={{ fontWeight: 500, color: '#e2e8f0', marginTop: '4px' }}>{profile?.org_name || 'Organization'}</div>
                <div style={{ fontSize: '11px', color: '#64748b', fontFamily: 'monospace', marginTop: '2px' }}>{profile?.org_id}</div>
              </div>
            </div>
          )}
        </div>

        {/* Update Form */}
        <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '28px' }}>
          <h2 style={{ fontSize: '18px', fontWeight: 600, marginTop: 0, marginBottom: '20px' }}>Edit Profile & Password</h2>
          <form onSubmit={handleUpdateProfile} style={{ display: 'grid', gap: '20px' }}>
            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Display Name</label>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Your full name"
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>

            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>New Password (optional)</label>
              <input
                type="password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                placeholder="Enter new password to change..."
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>

            <div>
              <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Confirm New Password</label>
              <input
                type="password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                placeholder="Confirm new password..."
                style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
              />
            </div>

            <button
              type="submit"
              disabled={saving}
              style={{ justifySelf: 'start', background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '12px 24px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer' }}
            >
              {saving ? 'Saving...' : 'Save Profile Changes'}
            </button>
          </form>
        </div>
      </div>
    </div>
  );
}
