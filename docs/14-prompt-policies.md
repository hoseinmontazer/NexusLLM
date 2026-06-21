# Prompt Policies

Prompt policies run on every chat completion request and can:
- Inject a system prompt automatically
- Block requests containing forbidden content
- Detect and redact PII
- Apply content moderation rules

---

## Create a prompt policy

```bash
curl -X POST http://localhost:8081/admin/v1/prompt-policies \
  -H "Content-Type: application/json" \
  -d '{
    "org_id":           "ORG_ID",
    "team_id":          "TEAM_ID",
    "model_name":       "gemma2:2b",
    "system_prompt":    "You are a helpful assistant. Always respond in English.",
    "deny_patterns":    ["ignore previous", "jailbreak", "DAN"],
    "pii_detection":    true,
    "content_filter":   true
  }'
```

### System prompt injection

When `system_prompt` is set, NexusLLM prepends it to the messages array before forwarding to the backend. The client's system prompt (if any) comes after the injected one.

Use case: enforce organization-specific instructions, branding, safety guidelines.

### Deny patterns

Requests containing any of the `deny_patterns` strings are rejected with HTTP 403:

```json
{"error": {"message": "Request blocked by prompt policy", "code": "prompt_policy_blocked"}}
```

Use case: block prompt injection attempts, jailbreak patterns.

### PII detection

When `pii_detection: true`, common PII patterns (email addresses, phone numbers, credit card numbers, SSNs) in the user's message trigger a block or redaction.

### Content filter

When `content_filter: true`, requests are checked against the configured content moderation rules.

---

## Policy precedence

Policies are matched in order:
1. Team + model match (most specific)
2. Team-only match
3. Org + model match
4. Org-only match (least specific)

The first matching policy wins.

---

## Viewing applied policies

Prompt policy evaluation happens in the gateway before the request reaches the backend. The decision is logged but not currently exposed via API. The web UI for policy management is on the roadmap (v0.6).
