#!/usr/bin/env python3
"""
NexusLLM Priority Scheduling Test
===================================
Tests that requests from a high-priority project (weight=1000) complete
before requests from a low-priority project (weight=500) when the gateway
is under concurrent load with limited concurrency.

HOW PRIORITY WORKS IN NEXUSLLM:
  - `X-Nexus-Project: <name>` header resolves a project row
  - Project's `priority_weight` becomes `effectivePriority`
  - When `max_concurrent` limit is reached, requests get HTTP 429 + Retry-After
  - The test retries 429s — high priority requests should get through faster
    because the gateway processes the request queue with priority ordering

WHAT THIS TEST DOES:
  1. Runs setup: creates/updates projects + sets low max_concurrent on the team
  2. Fires N requests for "high" and N for "low" simultaneously
  3. Records when each request COMPLETES (not starts — we can't control start)
  4. Validates that high-priority requests have lower average latency

Run:
  pip install httpx
  python3 test_priority.py           # run test only
  python3 test_priority.py --setup   # create projects + set policy, then test
"""

import asyncio
import time
import sys
import statistics
from datetime import datetime, timezone

import httpx

# ─── Configuration ─────────────────────────────────────────────────────────────
GATEWAY   = "http://192.168.0.200:8880"
ADMIN     = "http://192.168.0.200:8881"
API_KEY   = "nxs_a2ff8c5a55a4bb8dd71d6055f200a23be29814cd32e061b043905450ace3f4bc"
MODEL     = "qwythos-9b"

N_REQUESTS   = 8     # concurrent requests per project (16 total)
MAX_TOKENS   = 20    # keep responses short so the test finishes quickly
MAX_RETRIES  = 30    # retry 429s up to this many times
RETRY_DELAY  = 1.0   # seconds between retries on 429

HIGH_PROJECT = "high"
LOW_PROJECT  = "low"
HIGH_WEIGHT  = 1000
LOW_WEIGHT   = 100   # use a big gap to make the difference clear

# Team concurrency limit — must be low enough to force queuing
# With N_REQUESTS=8 per project and max_concurrent=2, most requests will queue
TEAM_MAX_CONCURRENT = 2

# ─── Setup ─────────────────────────────────────────────────────────────────────

def setup():
    """Create projects, set priorities. Only lowers concurrency if --force-concurrency is passed."""
    import requests

    force_concurrency = "--force-concurrency" in sys.argv

    print("Setting up...")

    # Get teams
    r = requests.get(f"{ADMIN}/admin/v1/teams")
    teams = r.json().get("data", [])
    if not teams:
        print("ERROR: No teams found.")
        sys.exit(1)
    team = teams[0]
    tid  = team["id"]
    print(f"  Team: {team['name']} ({tid})")

    # Get orgs
    orgs = requests.get(f"{ADMIN}/admin/v1/orgs").json().get("data", [])
    oid  = orgs[0]["id"] if orgs else None

    # Get existing projects
    existing = {p["name"]: p
                for p in requests.get(f"{ADMIN}/admin/v1/projects").json().get("data", [])}

    for name, weight in [(HIGH_PROJECT, HIGH_WEIGHT), (LOW_PROJECT, LOW_WEIGHT)]:
        if name in existing:
            p = existing[name]
            if p["priority_weight"] != weight:
                requests.post(f"{ADMIN}/admin/v1/projects/{p['id']}/priority",
                              json={"priority_weight": weight})
                print(f"  Updated '{name}' → weight={weight}")
            else:
                print(f"  Project '{name}' weight={weight} ✓")
        else:
            body = {"organization_id": oid, "team_id": tid,
                    "name": name, "priority_weight": weight, "status": "active"}
            r2 = requests.post(f"{ADMIN}/admin/v1/projects", json=body)
            if r2.status_code in (200, 201):
                print(f"  Created project '{name}' weight={weight} ✓")
            else:
                print(f"  ERROR creating '{name}': {r2.text}")
                sys.exit(1)

    # Only touch the team policy if explicitly requested
    if force_concurrency:
        r3 = requests.put(f"{ADMIN}/admin/v1/teams/{tid}/policy",
                          json={"max_concurrent":    TEAM_MAX_CONCURRENT,
                                "max_context_tokens": 32768,
                                "rpm":                600,
                                "tpd":                0})
        print(f"  Team policy → max_concurrent={TEAM_MAX_CONCURRENT} (forced for test)")
    else:
        # Just bump max_context_tokens so Cline works — leave rpm/concurrency alone
        current = requests.get(f"{ADMIN}/admin/v1/teams/{tid}/policy").json()
        cur_concurrent = current.get("max_concurrent", 10)
        cur_rpm        = current.get("rpm", 100)
        cur_tpd        = current.get("tpd", 0)
        print(f"  Current policy: rpm={cur_rpm}, max_concurrent={cur_concurrent} (not changing)")
        print(f"  NOTE: run with --force-concurrency to set max_concurrent={TEAM_MAX_CONCURRENT} for the test")
        print(f"        run with --restore to put it back after")

    # Ensure model is accessible to the team
    requests.post(f"{ADMIN}/admin/v1/teams/{tid}/models",
                  json={"model_name": MODEL})

    print("Setup complete.\n")


def restore():
    """Restore team policy to sane production defaults."""
    import requests

    print("Restoring team policy...")
    teams = requests.get(f"{ADMIN}/admin/v1/teams").json().get("data", [])
    if not teams:
        print("ERROR: No teams found.")
        sys.exit(1)
    tid = teams[0]["id"]

    r = requests.put(f"{ADMIN}/admin/v1/teams/{tid}/policy",
                     json={"max_concurrent":    30,
                           "max_context_tokens": 32768,
                           "rpm":                100,
                           "tpd":                0})
    print(f"  Restored: rpm=100, max_concurrent=30, max_context_tokens=32768")
    print(f"  Response: {r.status_code}")
    print("Done.\n")


# ─── Single request with retry ─────────────────────────────────────────────────

async def send_with_retry(
    client:   httpx.AsyncClient,
    project:  str,
    req_idx:  int,
    results:  list,
    barrier:  asyncio.Event,
):
    """
    Wait for the barrier (all tasks created), then fire the request.
    Retry on 429 with a short delay.
    Record when the RESPONSE is received.
    """
    await barrier.wait()

    headers = {
        "Authorization":  f"Bearer {API_KEY}",
        "Content-Type":   "application/json",
        "X-Nexus-Project": project,
    }
    payload = {
        "model":     MODEL,
        "messages":  [{"role": "user",
                        "content": f"Say: project={project} seq={req_idx}"}],
        "max_tokens": MAX_TOKENS,
    }

    t_start = time.monotonic()
    wall_start = datetime.now(timezone.utc).isoformat(timespec="milliseconds")

    for attempt in range(MAX_RETRIES):
        try:
            resp = await client.post(
                f"{GATEWAY}/v1/chat/completions",
                headers=headers,
                json=payload,
                timeout=120.0,
            )

            if resp.status_code == 200:
                latency_ms = (time.monotonic() - t_start) * 1000
                data = resp.json()
                content = data.get("choices", [{}])[0].get("message", {}).get("content", "")
                results.append({
                    "project":   project,
                    "req_idx":   req_idx,
                    "status":    "ok",
                    "latency_ms": latency_ms,
                    "wall_start": wall_start,
                    "wall_end":   datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
                    "content":   content[:60],
                    "retries":   attempt,
                })
                return

            elif resp.status_code == 429:
                # Priority is expressed here: high-priority requests should
                # get 429 fewer times than low-priority ones
                await asyncio.sleep(RETRY_DELAY)
                continue

            else:
                results.append({
                    "project":    project,
                    "req_idx":    req_idx,
                    "status":     f"http_{resp.status_code}",
                    "latency_ms": (time.monotonic() - t_start) * 1000,
                    "wall_start": wall_start,
                    "error":      resp.text[:300],
                    "retries":    attempt,
                })
                return

        except Exception as e:
            results.append({
                "project":    project,
                "req_idx":    req_idx,
                "status":     "exception",
                "latency_ms": (time.monotonic() - t_start) * 1000,
                "wall_start": wall_start,
                "error":      str(e)[:200],
                "retries":    attempt,
            })
            return

    results.append({
        "project":    project,
        "req_idx":    req_idx,
        "status":     "max_retries_exceeded",
        "latency_ms": (time.monotonic() - t_start) * 1000,
        "wall_start": wall_start,
        "retries":    MAX_RETRIES,
    })


# ─── Main test ─────────────────────────────────────────────────────────────────

async def run_test():
    print("=" * 65)
    print("NEXUSLLM PRIORITY SCHEDULING TEST")
    print("=" * 65)
    print(f"  Gateway          : {GATEWAY}")
    print(f"  Model            : {MODEL}")
    print(f"  Requests/project : {N_REQUESTS}")
    print(f"  Total concurrent : {N_REQUESTS * 2}")
    print(f"  Team max_concurrent: {TEAM_MAX_CONCURRENT} (forces queuing)")
    print(f"  High project weight: {HIGH_WEIGHT}")
    print(f"  Low  project weight: {LOW_WEIGHT}")
    print(f"  Ratio            : {HIGH_WEIGHT/LOW_WEIGHT:.0f}:1")
    print("=" * 65 + "\n")

    results  = []
    barrier  = asyncio.Event()

    # Create all tasks first, then release them simultaneously
    async with httpx.AsyncClient() as client:
        tasks = []
        for i in range(N_REQUESTS):
            tasks.append(send_with_retry(client, HIGH_PROJECT, i, results, barrier))
            tasks.append(send_with_retry(client, LOW_PROJECT,  i, results, barrier))

        # Release the barrier — all requests fire at the same instant
        print(f"Releasing {len(tasks)} requests simultaneously at "
              f"{datetime.now().isoformat(timespec='milliseconds')}...")
        barrier.set()
        await asyncio.gather(*tasks)

    # ─── Analysis ───────────────────────────────────────────────────────────

    high_ok  = [r for r in results if r["project"] == HIGH_PROJECT and r["status"] == "ok"]
    low_ok   = [r for r in results if r["project"] == LOW_PROJECT  and r["status"] == "ok"]
    high_err = [r for r in results if r["project"] == HIGH_PROJECT and r["status"] != "ok"]
    low_err  = [r for r in results if r["project"] == LOW_PROJECT  and r["status"] != "ok"]

    # Sort completed requests by latency (proxy for completion order)
    all_ok = sorted(high_ok + low_ok, key=lambda r: r["latency_ms"])

    print("\n" + "─" * 65)
    print("INDIVIDUAL RESULTS (sorted by completion order)")
    print("─" * 65)
    print(f"{'#':<4} {'Project':<8} {'Latency ms':>12} {'Retries':>8}  Content")
    print("-" * 65)
    for rank, r in enumerate(all_ok, 1):
        marker = " ← HIGH" if r["project"] == HIGH_PROJECT else ""
        print(f"{rank:<4} {r['project']:<8} {r['latency_ms']:>12.0f} "
              f"{r.get('retries',0):>8}  {r.get('content','')[:30]}{marker}")

    if high_err:
        print(f"\n  High errors ({len(high_err)}):")
        for e in high_err[:3]:
            print(f"    req {e['req_idx']}: {e['status']} — {e.get('error','')[:100]}")
    if low_err:
        print(f"\n  Low errors ({len(low_err)}):")
        for e in low_err[:3]:
            print(f"    req {e['req_idx']}: {e['status']} — {e.get('error','')[:100]}")

    print("\n" + "─" * 65)
    print("LATENCY STATISTICS")
    print("─" * 65)

    def stats(label, rows):
        if not rows:
            print(f"  {label:8s}: no successful requests")
            return None, None
        lats = sorted(r["latency_ms"] for r in rows)
        avg  = statistics.mean(lats)
        p50  = statistics.median(lats)
        p95  = lats[max(0, int(len(lats) * 0.95) - 1)]
        retries = sum(r.get("retries", 0) for r in rows)
        print(f"  {label:8s}: ok={len(rows):2d}  "
              f"avg={avg:7.0f}ms  p50={p50:7.0f}ms  p95={p95:7.0f}ms  "
              f"retries={retries:3d}")
        return avg, retries

    avg_high, retries_high = stats(HIGH_PROJECT, high_ok)
    avg_low,  retries_low  = stats(LOW_PROJECT,  low_ok)

    # Completion order analysis
    if all_ok:
        first_half = all_ok[:len(all_ok)//2]
        high_in_first = sum(1 for r in first_half if r["project"] == HIGH_PROJECT)
        total_first   = len(first_half)
        print(f"\n  First {total_first} completions: "
              f"{high_in_first} high, {total_first-high_in_first} low")

    # ─── Verdict ────────────────────────────────────────────────────────────
    print("\n" + "─" * 65)
    print("VERDICT")
    print("─" * 65)

    if avg_high is None or avg_low is None:
        print("  ⚠  Not enough successful responses to evaluate.")
        return

    total_retries = (retries_high or 0) + (retries_low or 0)

    # With max_concurrent >= N_REQUESTS*2, there's no queuing and priority has
    # no observable effect — latency differences are just model variance.
    if total_retries == 0:
        print(f"  ~  No queuing occurred (0 retries total).")
        print(f"     max_concurrent={TEAM_MAX_CONCURRENT} was not saturated by {N_REQUESTS*2} requests.")
        print(f"     Latency diff ({abs(avg_high-avg_low):.0f}ms) is random, not priority.")
        print(f"     High: {avg_high:.0f}ms  Low: {avg_low:.0f}ms")
        print()
        print(f"  ══ OVERALL: INCONCLUSIVE ══")
        print(f"     Priority is only observable under contention.")
        print(f"     To force queuing: python3 test_priority.py --setup --force-concurrency")
        print(f"     To restore after:  python3 test_priority.py --restore")
        return

    # There was queuing — now evaluate priority correctness
    latency_diff  = avg_low - avg_high
    latency_ratio = avg_high / avg_low if avg_low > 0 else 1.0

    passed = True
    issues = []

    if avg_high < avg_low:
        print(f"  ✓  High is faster: {avg_high:.0f}ms vs {avg_low:.0f}ms "
              f"(diff={latency_diff:.0f}ms, ratio={latency_ratio:.2f})")
    else:
        passed = False
        issues.append(f"High ({avg_high:.0f}ms) is NOT faster than low ({avg_low:.0f}ms)")
        print(f"  ✗  Priority inversion: high={avg_high:.0f}ms, low={avg_low:.0f}ms")

    if retries_high is not None and retries_low is not None:
        if retries_high <= retries_low:
            print(f"  ✓  High got fewer/equal retries: {retries_high} vs {retries_low}")
        else:
            passed = False
            issues.append(f"High got MORE retries ({retries_high}) than low ({retries_low})")
            print(f"  ✗  High got more retries than low: {retries_high} vs {retries_low}")

    if all_ok:
        first_half = all_ok[:N_REQUESTS]
        high_in_first = sum(1 for r in first_half if r["project"] == HIGH_PROJECT)
        expected_min = N_REQUESTS // 2
        if high_in_first >= expected_min:
            print(f"  ✓  High dominated first {N_REQUESTS} completions: "
                  f"{high_in_first}/{N_REQUESTS}")
        else:
            passed = False
            issues.append(f"High only had {high_in_first}/{N_REQUESTS} of first completions")
            print(f"  ✗  High only had {high_in_first}/{N_REQUESTS} of first completions")

    print()
    if passed:
        print("  ══ OVERALL: PASS — Priority scheduling is working correctly ══")
    else:
        print("  ══ OVERALL: FAIL ══")
        for issue in issues:
            print(f"     • {issue}")
        print()
        print("  Possible causes:")
        print("  - Gateway not rebuilt with the policy bug fix (rebuild + redeploy)")
        print("  - Projects have wrong weights (run --setup)")
        print("  - Redis still has stale policy (gateway reads Redis over DB)")
        print()
        print("  Possible causes:")
        print("  - max_concurrent not low enough (run with --setup to fix)")
        print("  - Gateway not rebuilt with the bug fix (rebuild + redeploy)")
        print("  - Model is too fast — all requests complete before queuing matters")
        print("  - Projects don't exist or have wrong weights (run --setup)")

    print("=" * 65 + "\n")


# ─── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    if "--restore" in sys.argv or "-r" in sys.argv:
        restore()
        sys.exit(0)
    if "--setup" in sys.argv or "-s" in sys.argv:
        setup()
    asyncio.run(run_test())
