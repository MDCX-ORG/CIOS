# P793 — Component mTLS Design Dossier (H2 / H3)

> **Status:** **CONFIRMED** Yuri 2026-07-16 (`ok` on channel A defaults) · Phase 1–2 code landing same day.  
> **Owner:** Architect (design) → implementation on `main` / M4 lines.  
> **Sources:** CODE-SCAN-2026-07-16 §2.2 H2/H3 · L34 · L104 · spec-006 §5.0bis / §5.1 · M4 plan P793.  
> **Channel lock:** **A — native app mTLS** (not mesh-only, not private-net-only).

---

## 0. Decision needed (stop line)

spec-006 §5.0bis allows **either**:

1. **Native mTLS** per §5.1 CA hierarchy, **or**
2. An **architect-approved equivalent channel** (service mesh / private network + network policy), registered as an **L**.

Until Yuri picks a channel class, **no implementation PRMT may mint server-side TLS or rewrite the `X-CIOS-Tenant` trust model**.

| Option | Summary | Recommend? |
|--------|---------|------------|
| **A — Native app mTLS** | Each process serves TLS + verifies peer client certs; fleet root → site intermediate → component leaf (L34) | **Yes — default for v0 cloud pre-prod** |
| **B — Service mesh** | Sidecar mTLS (Istio/Linkerd/CM); apps stay HTTP on localhost | Later / only if cloud platform already mandates mesh |
| **C — Private net only** | VPC + security groups; no peer identity in process | **No** as sole H2/H3 fix (fails header-spoof defense) |

**Recommendation: A (native), phased.** Matches L34 wording already locked; works for compose lab and cloud VMs without requiring a mesh control plane; leaves B as a future **equivalence L** if a customer standardizes on mesh.

---

## 1. As-is (empirical)

| Surface | Reality today |
|---------|----------------|
| Server TLS | No `ListenAndServeTLS` / `tls.Config.ClientAuth` in core or apigw |
| Client TLS | apigw optional **upstream** client cert (`CIOS_APIGW_TLS_{CA,CERT,KEY}`) — one-way to HTTPS upstream only |
| Edge deploy | `127.0.0.1` binds; compose has no TLS termination (CODE-SCAN §2.2 H2) |
| Tenant header | `X-CIOS-Tenant` set by apigw (`pkg/tenant/propagate.go`); **core trusts it if present** (`core/authmw.go` ~162–176) with comments claiming mTLS — **false** (H3) |
| Lab opt-out | H1 closed: apigw refuses silent no-auth; `ALLOW_NO_AUTH` / `DEV_NO_AUTH` explicit |

**Lab posture (acceptable under L104):** loopback + explicit no-auth flags.  
**Customer-facing cloud (blocked):** H2+H3 open.

---

## 2. Trust boundaries (what must get identity)

```text
                    ┌─ public / customer ─┐
 browser/portal ──► │ cios-apigw          │  northbound: OIDC/STS (human/machine)
                    │  (experience plane) │  NOT "component mTLS" for browsers
                    └─────────┬───────────┘
                              │  ★ H2/H3 focus: peer identity required
                              ▼
                    ┌─────────────────────┐
                    │ cios-core (/v1)     │  reads X-CIOS-Tenant today
                    └─────────┬───────────┘
           ┌──────────────────┼──────────────────┐
           ▼                  ▼                  ▼
        Postgres           NATS JS              VictoriaMetrics
     (data plane)       (bus)                (TSDB)
```

| Hop | Priority for H2/H3 | Notes |
|-----|--------------------|-------|
| **apigw → core** | **P0** | Carries `X-CIOS-Tenant`; spoofing = cross-tenant | 
| core → PG / NATS / VM | P1 | Use product TLS or mesh; less about tenant header |
| edge gateway → site | P1 | Already “edge zero-inbound” (outbond TLS); separate from site mTLS |
| browser → apigw | Out of P793 | TLS termination + OIDC; client certs for humans optional later |

**H3 closes only when** the process that injects `X-CIOS-Tenant` is cryptographically bound as a **known component identity** (or the header is removed and replaced by a signed claim).

---

## 3. Option analysis

### A — Native app mTLS (recommended)

**Shape**

```text
fleet root CA (offline or HSM later)
 └─ site intermediate (per site / cloud region)
     └─ leaf: cios://{site}/apigw
     └─ leaf: cios://{site}/core
     └─ leaf: cios://{site}/gateway   (edge, later)
```

- **Server:** `ListenAndServeTLS` with `ClientAuth = RequireAndVerifyClientCert`, `ClientCAs = site pool`.
- **Client:** existing apigw upstream TLS triple becomes **required** when `CIOS_*_MTLS=on` (not optional).
- **SPIFFE-ish URI SAN** preferred identity string: `cios://{site}/{component}` (spec-006 §5.1); CN fallback allowed in v0 if SAN tooling lags.
- **Rotation:** 90-day leaf lifetime (L34); reload via SIGHUP or file watch — implement in PRMT, not this doc.
- **Lab:** `CIOS_MTLS_MODE=off` + loopback still allowed (H1-style explicit); cloud profile forbids off.

**Pros:** Spec-aligned; no new runtime; works on bare VMs and compose.  
**Cons:** Cert bootstrap/ops burden; multi-replica needs shared or per-pod cert issue path.

### B — Service mesh equivalence

- Apps listen HTTP on `127.0.0.1`; sidecar does mTLS.
- **H3:** core must still **not** trust `X-CIOS-Tenant` from non-mesh peers — enforce via mesh identity policy (only `apigw` SA may call core) **and** optional still-verify SPIFFE SPIFFE ID in middleware if exposed.
- **Requires L:** “mesh X is equivalent to §5.1 for site plane” before shipping customer cloud.

**Pros:** Ops standard in some clouds; cert rotation free.  
**Cons:** Not in tree today; edge pods may not run mesh; accidental plain-HTTP binding is a footgun.

### C — Private network only (rejected as sole fix)

- VPC + SG: only apigw SG → core SG on :8090.
- **Fails H3 intent:** any compromised process in the SG can spoof `X-CIOS-Tenant`.
- Acceptable as **defense in depth**, never as the only control.

---

## 4. Recommended target architecture (Option A, phased)

### Phase 0 — this dossier + L decision

- Yuri locks **channel = A** (or B with named mesh).  
- Register short L if B; if A, L34 already covers hierarchy — implementation PRMTs only.

### Phase 1 — apigw ↔ core mTLS (closes H2 for northbound path + enables H3)

| Work | Notes |
|------|--------|
| `cios-core` TLS server + client-cert require | env: cert/key/CA paths; fail-closed if `CIOS_CORE_MTLS=on` and missing |
| `cios-apigw` upstream always uses client cert when core MTLS on | reuse existing TLS env; tighten boot gate |
| Identity map | peer cert URI/CN → component allow-list (`apigw` only for public /v1) |
| Deploy profile | compose `apps` + cloud sample manifests with cert volume mounts |
| Dev ergonomics | `scripts/gen-dev-mtls-certs.sh` (local CA, 365d lab leaves) — **lab only** |

### Phase 2 — H3 trust rewrite (depends on Phase 1)

| Work | Notes |
|------|--------|
| **Do not trust bare header alone** | On allow path: require `PeerComponent == apigw` (from mTLS) **before** accepting `X-CIOS-Tenant` |
| Absent mTLS peer | Reject tenant header (403/400) unless explicit lab `CIOS_TRUST_TENANT_HEADER=1` **and** loopback (fail-closed in cloud profile) |
| Optional hardening | Prefer tenant from STS claims when present; header only as gateway-stamped scope consistent with claims |
| Tests | Spoof header without client cert → denied; with apigw cert + header → ok |

### Phase 3 — data plane TLS (PG / NATS / VM) ✅ **landed 2026-07-16**

- Prefer **product-native TLS** (Postgres `sslmode=verify-full`, NATS TLS, VM HTTPS) over inventing a second CA for every hop.
- Same site intermediate can sign DB/NATS client certs if desired; not blocking customer portal if Phase 1–2 done and DB is private.
- **Code:** `pkg/mtls/dataplane.go` (`PGDSNApplyTLS`, `OutboundTLS`, `RequireHTTPS`); `cios-core` flags/env `CIOS_DATA_PLANE_TLS=require|off`, `CIOS_PG_TLS_*`, `CIOS_NATS_TLS_*`, `CIOS_VM_TLS_*`; `Server.SetVMHTTPClient` for VM HTTPS verify.

### Phase 4 — edge gateway leafs + fleet CA automation

- Outbound site→cloud already TLS; add client identity for multi-tenant cloud ingest.
- Auto-issue + 90d rotation (L34) — separate PRMT series; not v0 cloud portal blocker if site plane is locked.
- **Partial (same day):** lab leaf `gateway` in `gen-dev-mtls-certs.sh` + manual 90d runbook `docs/P793-cert-rotation-runbook.md` (automation still open).

---

## 5. Env / config sketch (implementation contract — not yet code)

| Env | Role |
|-----|------|
| `CIOS_MTLS_MODE` | `off` \| `permit` \| `require` — cloud profile uses `require` |
| `CIOS_CORE_TLS_CERT` / `_KEY` / `_CLIENT_CA` | core server identity + trusted clients |
| `CIOS_APIGW_TLS_CERT` / `_KEY` / `_CA` | already exist for upstream; become mandatory under `require` |
| `CIOS_TRUST_TENANT_HEADER` | lab-only override; forbidden when `MODE=require` |

Boot rules (mirror H1):

- `MODE=require` + missing material → **process exit non-zero**.
- `MODE=off` → current loopback lab behavior; log WARN once.

---

## 6. PRMT mint plan (after channel L)

Suggested numbering continues M4 sequence (adjust if 216+ taken):

| PRMT | Branch | Content |
|------|--------|---------|
| **PRMT-216** | `main` or `feature/m4-energy` hygiene → prefer **`main`/hotfix if pre-prod** | Dev cert generator script + docs only |
| **PRMT-217** | pre-prod line | core `ListenAndServeTLS` + client auth + tests |
| **PRMT-218** | pre-prod line | apigw require upstream mTLS; compose/cloud samples |
| **PRMT-219** | pre-prod line | H3: peer gate on `X-CIOS-Tenant` + tests (closes SCAN #4) |

Do **not** combine 217–219 into one mega-PRMT (blast radius / review).

---

## 7. Acceptance criteria (definition of done for P793 **implementation**)

- [x] Under cloud profile (`MODE=require`), apigw without client cert cannot call core `/v1` (connection / boot fails without material).
- [x] Under cloud profile, core rejects `X-CIOS-Tenant` unless peer is authenticated apigw (`SetTenantHeaderRequiresMTLSPeer`).
- [x] Lab profile still boots with `MODE=off` default (no silent require).
- [x] Unit tests: `pkg/mtls`, `core` tenant gate, apigw require boot.
- [x] Full e2e automated: `make mtls-e2e` (`scripts/mtls-e2e.sh`) — not manual.
- [x] CODE-SCAN H2/H3 disposition → FIXED (e2e + unit evidence).
- [x] Channel A confirmed by Yuri (2026-07-16).

---

## 8. Risks & honesty

1. **Cert ops is the real cost** — without bootstrap automation, “mTLS on” becomes a paper gate. Phase 1 must ship a lab generator; production issue path can be manual runbook for first customer.
2. **Header + mTLS is not end-to-end tenant auth** — STS still owns human/machine principal; mTLS only authenticates **components**.
3. **Mesh customers** will want B; plan an equivalence L rather than forking the codebase.
4. **This dossier does not implement anything** — claiming H2 closed without PRMT-217+ is a governance failure.

---

## 9. Ask Yuri (minimal)

1. **Channel:** confirm **A native** for v0 cloud pre-prod? (or name mesh product for B)
2. **Scope freeze for first customer:** Phase 1–2 only (apigw↔core + H3), data-plane TLS can lag if DB is private VPC-only?
3. **Lab:** keep permanent `MODE=off` for compose demos?

Default if no answer within kickoff tempo: **proceed to mint PRMT-216/217 design-locked on A**, hold merge of `MODE=require` defaults until Q1 confirmed.

---

## 10. Status update targets

| Artifact | Update |
|----------|--------|
| M4 plan P793 | 🟢 design dossier landed; implementation gated on Yuri Q1 |
| CODE-SCAN §3 #3–4 | OPEN → design ready; impl pending |
| Discussion / LOCKED | only if Yuri picks B (new L) or freezes A explicitly |

**File:** `docs/runbooks/P793-mtls-design-dossier.md` (this document).
