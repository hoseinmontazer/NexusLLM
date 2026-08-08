# 29. Self-Service Developer Portal & User Management API Documentation

The **Self-Service Developer Portal** and **User RBAC Management System** in NexusLLM provide automated developer workflows, access request lifecycle management, zero-downtime API key rotation, administrative review queues, and multi-tenant user access control.

---

## 1. Role-Based Access Control (RBAC) Matrix

| Action / Capability | Platform Admin (`admin`) | Member / Developer (`member`) | Viewer (`viewer`) |
| :--- | :---: | :---: | :---: |
| **Register & Login** | ✅ | ✅ | ✅ |
| **Create Project** | ✅ | ✅ | ❌ |
| **Submit Model Access Request** | ✅ | ✅ | ❌ |
| **Manage Project API Keys** | ✅ | ✅ (Own Project) | ❌ |
| **Rotate API Key (Zero Downtime)** | ✅ | ✅ (Own Project) | ❌ |
| **View Granted Models & Usage** | ✅ | ✅ (Scoped) | ✅ (Scoped) |
| **Review / Provision Access Requests** | ✅ | ❌ | ❌ |
| **User Directory & RBAC Editing** | ✅ | ❌ | ❌ |
| **Activate / Deactivate Users** | ✅ | ❌ | ❌ |
| **Developer Self-Profile Editing** | ✅ | ✅ | ✅ |

---

## 2. Developer Self-Service Portal APIs (`/portal/v1`)

### Authentication & Registration

#### `POST /portal/v1/auth/register`
Self-register a developer account and automatically initialize an organization.
- **Request Body**:
  ```json
  {
    "email": "developer@company.com",
    "password": "SecretPassword123!",
    "name": "Jane Developer",
    "org_name": "Acme AI Labs"
  }
  ```
- **Response** (`201 Created`):
  ```json
  {
    "message": "registration successful",
    "user_id": "8f9a2b1c-...",
    "org_id": "123e4567-...",
    "email": "developer@company.com",
    "role": "member",
    "token": "eyJhbGciOi..."
  }
  ```

#### `POST /portal/v1/auth/login`
Authenticate developer credentials and return a signed JWT.
- **Request Body**:
  ```json
  {
    "email": "developer@company.com",
    "password": "SecretPassword123!"
  }
  ```

---

### Project Management

#### `POST /portal/v1/projects`
Create a new developer project associated with an environment.
- **Request Body**:
  ```json
  {
    "organization_id": "123e4567-...",
    "name": "Customer Support Assistant",
    "description": "Production support LLM agent",
    "environment": "production",
    "expected_monthly_requests": 500000,
    "expected_monthly_tokens": 100000000
  }
  ```

#### `GET /portal/v1/projects`
List projects belonging to the developer's organization.

---

### Access Requests & State Machine

#### Access Request State Machine
```
[Draft] ➔ [Submitted] ➔ [Pending Review] ➔ [Approved] (Triggers Automatic Provisioning)
                                        ➔ [Rejected] (Rationale Provided)
                                        ➔ [Cancelled]
```

#### `POST /portal/v1/requests`
Submit a model & provider access request.
- **Request Body**:
  ```json
  {
    "project_id": "proj-uuid-1234",
    "requested_models": ["gemma-2", "llama-3"],
    "requested_providers": ["openrouter", "openai/gpt-5"],
    "business_use_case": "Automated ticket categorization and draft responses",
    "expected_rpm": 600,
    "expected_tpm": 50000,
    "required_context_size": 32768
  }
  ```

#### `GET /portal/v1/requests`
List access request history and approval statuses.

---

### API Key Lifecycle & Zero-Downtime Rotation

#### `POST /portal/v1/projects/:id/api-keys`
Create a new API key scoped to the specified project. Raw key is revealed **ONCE**.
- **Response**:
  ```json
  {
    "id": "key-uuid-9876",
    "name": "Production Key A",
    "key_prefix": "nxs_8f9a2b1c",
    "api_key_secret": "nxs_8f9a2b1c4e5f6g7h8i9j0k1l2m3n4o5p"
  }
  ```

#### `POST /portal/v1/api-keys/:key_id/rotate`
Perform zero-downtime key rotation. Keeps the old key valid for a 24-hour grace period while issuing the new key.
- **Response**:
  ```json
  {
    "message": "key rotated successfully (old key remains active for 24h grace period)",
    "old_key_id": "key-uuid-9876",
    "old_key_grace_expires": "2026-08-07T12:00:00Z",
    "new_key_id": "key-uuid-5432",
    "new_key_prefix": "nxs_1a2b3c4d",
    "new_api_key_secret": "nxs_1a2b3c4d5e6f7g8h9i0j1k2l3m4n5o6p"
  }
  ```

#### `DELETE /portal/v1/api-keys/:key_id`
Revoke key immediately and evict `nexus:apikey:{hash}` from Redis cache.

---

## 3. Admin User Management APIs (`/admin/v1/users`)

#### `GET /admin/v1/users`
List users with optional filtering by `org_id`, `role`, or `active` status.

#### `POST /admin/v1/users`
Admin creates a new platform user.
- **Request Body**:
  ```json
  {
    "org_id": "123e4567-...",
    "email": "user@company.com",
    "name": "Jane Doe",
    "role": "admin",
    "password": "SecurePassword123!"
  }
  ```

#### `PUT /admin/v1/users/:id`
Admin updates user details (`name`, `email`, `role`, `org_id`, `active`).

#### `POST /admin/v1/users/:id/activate` & `/deactivate`
Toggle user account activation status.

---

## 4. Admin Review Queue & Automatic Provisioning (`/admin/v1/portal`)

#### `GET /admin/v1/portal/requests/pending`
List all pending access requests awaiting review.

#### `POST /admin/v1/portal/requests/:id/review`
Execute review action (`approve` or `reject`). On approval, the Automatic Provisioning Engine executes in a single database transaction:
1. Updates project status to `active`.
2. Inserts public model grants into `team_model_permissions`.
3. Inserts cloud provider grants into `project_provider_access`.
4. Configures rate limits (`rpm_limit`, `tpm_limit`) in `project_policies`.
5. Auto-generates a project API key (`nxs_...`).
6. Invalidates model catalog Redis caches.
7. Dispatches developer notifications and writes `audit_logs`.
