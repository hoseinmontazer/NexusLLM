# Web Admin UI & Self-Service Developer Portal

The web UI is a Next.js app running on port 3001 that provides a visual interface for both platform administrators and self-service developers.

**URL:** http://localhost:3001

---

## Starting the Web UI

```bash
# First time only
make web-install

# Start
make run-web
```

The UI proxies all API calls to `http://localhost:8081/admin/v1/*` and `/portal/v1/*` via Next.js rewrites. No CORS configuration needed.

---

## Developer Self-Service Portal Pages (`/portal/*`)

### Developer Dashboard (`/portal`)
- Summary stats: Active Projects count, Pending Access Requests count, Granted Models count, and Monthly Quota status.
- Quick navigation cards to projects, access requests, API key management, granted models, and usage analytics.

### Developer Projects (`/portal/projects`)
- Create projects for Development, Staging, or Production environments.
- Specify expected monthly request and token volumes.
- List existing projects and track request/policy statuses.

### Access Requests (`/portal/requests`)
- Self-service form to select Public Models (`gemma-2`, `llama-3`, `whisper`) and Cloud Providers (`openrouter`, `openai/gpt-5`).
- Submit business use cases, expected RPM, expected TPM, and required context size.
- Track approval progress (`Pending Review`, `Approved`, `Rejected`, `Cancelled`).

### API Key Management (`/portal/api-keys`)
- Create project-scoped API keys with automatic secret masking (`nxs_xxxxxxxxx1234`).
- One-click **Zero-Downtime Key Rotation**: generates a new key while maintaining a 24-hour grace period on the old key.
- Instant key revocation.

### Granted Models (`/portal/models`)
- Displays ONLY models and cloud providers explicitly granted to the active project.
- No global catalog leakage.

### Usage & Analytics (`/portal/usage`)
- Real-time charts for RPM, TPM, daily token consumption, 429 rate limit counts, and latency.

### Notifications (`/portal/notifications`)
- Developer alert history for request approvals, rejections, and key expirations.

### Developer Settings (`/portal/settings`)
- View user credentials, update display name, and update account password.

---

## Admin Pages

### Admin Review Queue (`/portal/admin-queue`)
- List pending developer access requests.
- Inspect risk indicators and business rationale.
- One-click **Approve & Provision**: automatically provisions model permissions, provider access, rate limits, and API keys.

### User Management & Directory (`/users`)
- User directory with filtering by Organization, Role (`admin`, `member`, `viewer`), or Status (`Active`/`Deactivated`).
- Create users, edit user info (name, email, role, org), and toggle account activation state.

### Dashboard (`/`)
- Platform overview: organizations, teams, LLM models, AI services, cluster nodes, GPU nodes, and model health.

### Organizations (`/orgs`)
- List, create, and deactivate organizations.

### Teams (`/teams`)
- Manage teams, rate limits, token quotas, model permissions, and team API keys.

### Models (`/models`)
- Deploy local models or register cloud providers.
