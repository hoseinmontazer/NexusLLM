'use client';

import React, { useState, useEffect } from 'react';
import { useAuth } from '@/lib/auth-context';

interface UserItem {
  id: string;
  org_id: string;
  org_name: string;
  email: string;
  name: string;
  role: string;
  active: boolean;
  created_at: string;
}

export default function AdminUsersPage() {
  const { user: currentUser } = useAuth();
  const [users, setUsers] = useState<UserItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [errorMsg, setErrorMsg] = useState('');
  const [successMsg, setSuccessMsg] = useState('');

  // Search & Filter
  const [orgFilter, setOrgFilter] = useState('');
  const [roleFilter, setRoleFilter] = useState('');
  const [activeFilter, setActiveFilter] = useState('');

  // Create User Form State
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [newEmail, setNewEmail] = useState('');
  const [newName, setNewName] = useState('');
  const [newOrgId, setNewOrgId] = useState('');
  const [newRole, setNewRole] = useState('member');
  const [newPassword, setNewPassword] = useState('');
  const [creating, setCreating] = useState(false);

  // Edit User State
  const [editingUser, setEditingUser] = useState<UserItem | null>(null);
  const [editName, setEditName] = useState('');
  const [editEmail, setEditEmail] = useState('');
  const [editRole, setEditRole] = useState('');
  const [editOrgId, setEditOrgId] = useState('');
  const [updating, setUpdating] = useState(false);

  const fetchUsers = async () => {
    setLoading(true);
    try {
      const queryParams = new URLSearchParams();
      if (orgFilter) queryParams.set('org_id', orgFilter);
      if (roleFilter) queryParams.set('role', roleFilter);
      if (activeFilter) queryParams.set('active', activeFilter);

      const res = await fetch(`/admin/v1/users?${queryParams.toString()}`);
      if (res.ok) {
        const data = await res.json();
        setUsers(data.data || []);
      } else {
        setErrorMsg('Failed to fetch user directory.');
      }
    } catch (e: any) {
      setErrorMsg('Error loading users: ' + e.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchUsers();
  }, [orgFilter, roleFilter, activeFilter]);

  const handleCreateUser = async (e: React.FormEvent) => {
    e.preventDefault();
    setCreating(true);
    setErrorMsg('');
    setSuccessMsg('');

    const targetOrg = newOrgId || currentUser?.org_id;
    if (!targetOrg) {
      setErrorMsg('Organization ID is required.');
      setCreating(false);
      return;
    }

    try {
      const res = await fetch('/admin/v1/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          org_id: targetOrg,
          email: newEmail,
          name: newName,
          role: newRole,
          password: newPassword,
        }),
      });

      if (res.ok) {
        setSuccessMsg(`User ${newEmail} created successfully!`);
        setShowCreateModal(false);
        setNewEmail('');
        setNewName('');
        setNewPassword('');
        fetchUsers();
      } else {
        const data = await res.json();
        setErrorMsg(data.error || 'Failed to create user.');
      }
    } catch (e: any) {
      setErrorMsg('Error creating user: ' + e.message);
    } finally {
      setCreating(false);
    }
  };

  const handleUpdateUser = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!editingUser) return;
    setUpdating(true);
    setErrorMsg('');
    setSuccessMsg('');

    try {
      const res = await fetch(`/admin/v1/users/${editingUser.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: editName,
          email: editEmail,
          role: editRole,
          org_id: editOrgId,
        }),
      });

      if (res.ok) {
        setSuccessMsg(`User ${editEmail} updated successfully!`);
        setEditingUser(null);
        fetchUsers();
      } else {
        const data = await res.json();
        setErrorMsg(data.error || 'Failed to update user.');
      }
    } catch (e: any) {
      setErrorMsg('Error updating user: ' + e.message);
    } finally {
      setUpdating(false);
    }
  };

  const toggleUserActive = async (id: string, currentlyActive: boolean) => {
    setErrorMsg('');
    setSuccessMsg('');
    const actionEndpoint = currentlyActive ? 'deactivate' : 'activate';
    try {
      const res = await fetch(`/admin/v1/users/${id}/${actionEndpoint}`, { method: 'POST' });
      if (res.ok) {
        setSuccessMsg(`User status updated to ${currentlyActive ? 'Inactive' : 'Active'}.`);
        fetchUsers();
      } else {
        const data = await res.json();
        setErrorMsg(data.error || 'Failed to change user status.');
      }
    } catch (e: any) {
      setErrorMsg('Error changing status: ' + e.message);
    }
  };

  const openEditModal = (u: UserItem) => {
    setEditingUser(u);
    setEditName(u.name);
    setEditEmail(u.email);
    setEditRole(u.role);
    setEditOrgId(u.org_id);
  };

  return (
    <div style={{ minHeight: '100vh', background: '#0b0f19', color: '#f8fafc', fontFamily: 'Inter, sans-serif', padding: '32px 48px' }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '32px' }}>
        <div>
          <h1 style={{ fontSize: '28px', fontWeight: 700, margin: 0, background: 'linear-gradient(135deg, #60a5fa 0%, #a855f7 100%)', WebkitBackgroundClip: 'text', WebkitTextFillColor: 'transparent' }}>
            User Management & RBAC Directory
          </h1>
          <p style={{ color: '#94a3b8', fontSize: '14px', marginTop: '6px' }}>
            Manage platform users, update user info, configure RBAC roles (Admin, Member, Viewer), and toggle account access.
          </p>
        </div>
        <button
          onClick={() => setShowCreateModal(true)}
          style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer', boxShadow: '0 4px 14px rgba(59, 130, 246, 0.35)' }}
        >
          + Add New User
        </button>
      </div>

      {/* Notifications */}
      {errorMsg && (
        <div style={{ background: 'rgba(239, 68, 68, 0.15)', border: '1px solid #ef4444', color: '#fca5a5', padding: '14px 20px', borderRadius: '10px', marginBottom: '24px', fontSize: '14px' }}>
          {errorMsg}
        </div>
      )}
      {successMsg && (
        <div style={{ background: 'rgba(16, 185, 129, 0.15)', border: '1px solid #10b981', color: '#34d399', padding: '14px 20px', borderRadius: '10px', marginBottom: '24px', fontSize: '14px' }}>
          {successMsg}
        </div>
      )}

      {/* Filter Bar */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', padding: '20px', marginBottom: '32px', display: 'flex', gap: '16px', alignItems: 'center' }}>
        <div style={{ flex: 1 }}>
          <label style={{ display: 'block', fontSize: '12px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Filter by Role</label>
          <select
            value={roleFilter}
            onChange={(e) => setRoleFilter(e.target.value)}
            style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '9px 12px', borderRadius: '8px', fontSize: '13px' }}
          >
            <option value="">All Roles</option>
            <option value="admin">Platform Admin</option>
            <option value="member">Developer / Member</option>
            <option value="viewer">Viewer</option>
          </select>
        </div>

        <div style={{ flex: 1 }}>
          <label style={{ display: 'block', fontSize: '12px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Filter by Status</label>
          <select
            value={activeFilter}
            onChange={(e) => setActiveFilter(e.target.value)}
            style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '9px 12px', borderRadius: '8px', fontSize: '13px' }}
          >
            <option value="">All Statuses</option>
            <option value="true">Active Only</option>
            <option value="false">Deactivated Only</option>
          </select>
        </div>

        <div style={{ flex: 1 }}>
          <label style={{ display: 'block', fontSize: '12px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Filter by Organization ID</label>
          <input
            type="text"
            value={orgFilter}
            onChange={(e) => setOrgFilter(e.target.value)}
            placeholder="Enter Org UUID..."
            style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '9px 12px', borderRadius: '8px', fontSize: '13px' }}
          />
        </div>
      </div>

      {/* Users Table */}
      <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '12px', overflow: 'hidden' }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', textAlign: 'left', fontSize: '14px' }}>
          <thead>
            <tr style={{ background: '#0f172a', borderBottom: '1px solid #334155', color: '#94a3b8' }}>
              <th style={{ padding: '14px 20px' }}>User Details</th>
              <th style={{ padding: '14px 20px' }}>Organization</th>
              <th style={{ padding: '14px 20px' }}>Role</th>
              <th style={{ padding: '14px 20px' }}>Status</th>
              <th style={{ padding: '14px 20px' }}>Created At</th>
              <th style={{ padding: '14px 20px' }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={6} style={{ padding: '24px', textAlign: 'center', color: '#94a3b8' }}>Loading user directory...</td></tr>
            ) : users.length === 0 ? (
              <tr><td colSpan={6} style={{ padding: '24px', textAlign: 'center', color: '#94a3b8' }}>No users match the search criteria.</td></tr>
            ) : (
              users.map((u) => (
                <tr key={u.id} style={{ borderBottom: '1px solid #334155' }}>
                  <td style={{ padding: '14px 20px' }}>
                    <div style={{ fontWeight: 600, color: '#f8fafc' }}>{u.name || 'Unnamed User'}</div>
                    <div style={{ color: '#60a5fa', fontSize: '13px' }}>{u.email}</div>
                  </td>
                  <td style={{ padding: '14px 20px' }}>
                    <div style={{ fontWeight: 500, color: '#e2e8f0' }}>{u.org_name || 'Organization'}</div>
                    <div style={{ fontSize: '11px', color: '#94a3b8', fontFamily: 'monospace' }}>{u.org_id.slice(0, 8)}...</div>
                  </td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{
                      padding: '4px 10px',
                      borderRadius: '12px',
                      fontSize: '12px',
                      fontWeight: 600,
                      background: u.role === 'admin' ? 'rgba(168, 85, 247, 0.2)' : 'rgba(59, 130, 246, 0.2)',
                      color: u.role === 'admin' ? '#c084fc' : '#60a5fa',
                    }}>
                      {u.role.toUpperCase()}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px' }}>
                    <span style={{
                      padding: '4px 10px',
                      borderRadius: '12px',
                      fontSize: '12px',
                      fontWeight: 600,
                      background: u.active ? 'rgba(16, 185, 129, 0.2)' : 'rgba(239, 68, 68, 0.2)',
                      color: u.active ? '#34d399' : '#f87171',
                    }}>
                      {u.active ? 'ACTIVE' : 'DEACTIVATED'}
                    </span>
                  </td>
                  <td style={{ padding: '14px 20px', color: '#94a3b8', fontSize: '13px' }}>
                    {new Date(u.created_at).toLocaleDateString()}
                  </td>
                  <td style={{ padding: '14px 20px' }}>
                    <div style={{ display: 'flex', gap: '8px' }}>
                      <button
                        onClick={() => openEditModal(u)}
                        style={{ background: '#334155', color: '#fff', border: 'none', padding: '6px 12px', borderRadius: '6px', fontSize: '12px', fontWeight: 600, cursor: 'pointer' }}
                      >
                        Edit Info
                      </button>
                      <button
                        onClick={() => toggleUserActive(u.id, u.active)}
                        style={{
                          background: u.active ? 'rgba(239, 68, 68, 0.2)' : 'rgba(16, 185, 129, 0.2)',
                          color: u.active ? '#f87171' : '#34d399',
                          border: '1px solid ' + (u.active ? '#ef4444' : '#10b981'),
                          padding: '6px 12px',
                          borderRadius: '6px',
                          fontSize: '12px',
                          fontWeight: 600,
                          cursor: 'pointer',
                        }}
                      >
                        {u.active ? 'Deactivate' : 'Activate'}
                      </button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Create User Modal */}
      {showCreateModal && (
        <div style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', display: 'flex', justifyContent: 'center', alignItems: 'center', zIndex: 100 }}>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '16px', padding: '32px', width: '480px' }}>
            <h2 style={{ fontSize: '20px', fontWeight: 700, margin: '0 0 20px 0' }}>Create New Platform User</h2>
            <form onSubmit={handleCreateUser} style={{ display: 'grid', gap: '16px' }}>
              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Email Address</label>
                <input
                  type="email"
                  value={newEmail}
                  onChange={(e) => setNewEmail(e.target.value)}
                  placeholder="developer@company.com"
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                  required
                />
              </div>

              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Full Name</label>
                <input
                  type="text"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  placeholder="Jane Doe"
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                />
              </div>

              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>RBAC Role</label>
                <select
                  value={newRole}
                  onChange={(e) => setNewRole(e.target.value)}
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                >
                  <option value="member">Developer / Member</option>
                  <option value="admin">Platform Admin</option>
                  <option value="viewer">Viewer</option>
                </select>
              </div>

              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Initial Password</label>
                <input
                  type="password"
                  value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)}
                  placeholder="Secure password..."
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                  required
                />
              </div>

              <div style={{ display: 'flex', gap: '12px', justifyContent: 'flex-end', marginTop: '12px' }}>
                <button
                  type="button"
                  onClick={() => setShowCreateModal(false)}
                  style={{ background: '#334155', color: '#cbd5e1', border: 'none', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer' }}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={creating}
                  style={{ background: 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)', color: '#fff', border: 'none', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer' }}
                >
                  {creating ? 'Creating...' : 'Create User'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Edit User Modal */}
      {editingUser && (
        <div style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', display: 'flex', justifyContent: 'center', alignItems: 'center', zIndex: 100 }}>
          <div style={{ background: '#1e293b', border: '1px solid #334155', borderRadius: '16px', padding: '32px', width: '480px' }}>
            <h2 style={{ fontSize: '20px', fontWeight: 700, margin: '0 0 20px 0' }}>Edit User Info</h2>
            <form onSubmit={handleUpdateUser} style={{ display: 'grid', gap: '16px' }}>
              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Full Name</label>
                <input
                  type="text"
                  value={editName}
                  onChange={(e) => setEditName(e.target.value)}
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                />
              </div>

              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Email Address</label>
                <input
                  type="email"
                  value={editEmail}
                  onChange={(e) => setEditEmail(e.target.value)}
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                  required
                />
              </div>

              <div>
                <label style={{ display: 'block', fontSize: '13px', fontWeight: 600, color: '#94a3b8', marginBottom: '6px' }}>Role</label>
                <select
                  value={editRole}
                  onChange={(e) => setEditRole(e.target.value)}
                  style={{ width: '100%', background: '#0f172a', border: '1px solid #334155', color: '#fff', padding: '10px 14px', borderRadius: '8px', fontSize: '14px' }}
                >
                  <option value="member">Developer / Member</option>
                  <option value="admin">Platform Admin</option>
                  <option value="viewer">Viewer</option>
                </select>
              </div>

              <div style={{ display: 'flex', gap: '12px', justifyContent: 'flex-end', marginTop: '12px' }}>
                <button
                  type="button"
                  onClick={() => setEditingUser(null)}
                  style={{ background: '#334155', color: '#cbd5e1', border: 'none', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer' }}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={updating}
                  style={{ background: 'linear-gradient(135deg, #10b981 0%, #059669 100%)', color: '#fff', border: 'none', padding: '10px 18px', borderRadius: '8px', fontWeight: 600, fontSize: '14px', cursor: 'pointer' }}
                >
                  {updating ? 'Saving...' : 'Save Changes'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
}
