# Dogfood QA Report - Step 1 Validation

**Target:** http://localhost:3000 / http://localhost:8080
**Date:** April 23, 2026
**Scope:** BIFROST -> KOUTAKU rename validation (complete)
**Tester:** Hermes Agent (automated QA)

---

## Executive Summary

| Severity | Count |
|----------|-------|
| Critical | 0 |
| High | 0 |
| Medium | 0 |
| Low | 0 |
| **Total** | **0** |

**Overall Assessment:** BIFROST -> KOUTAKU rename is 100% complete. Zero references to bifrost/Bifrost/BIFROST remain in source code, HTML output, or API responses.

---

## Validation Results

### Source Code Scans
- grep across all .ts/.tsx/.json/.yaml/.go/.mts/.py/.tpl/.toml files: **ZERO hits**
- Makefile: KOUTAKU_* env vars only
- Helm _helpers.tpl: .Values.koutaku.* only
- Dockerfile: koutaku-http only

### HTML Output
- Title: `<title>Koutaku</title>` PASS
- BIFROST references in served HTML: **ZERO** PASS
- dns-prefetch points to: github.com/Barkasj/koutaku PASS

### Logo/Icon Files
- koutaku-logo.webp: accessible (200) PASS
- koutaku-logo-dark.webp: accessible (200) PASS
- koutaku-icon.webp: accessible (200) PASS
- koutaku-icon-dark.webp: accessible (200) PASS
- bifrost-*.webp files: deleted from ui/public/ PASS
- Sidebar references: /koutaku-logo.webp, /koutaku-icon.webp PASS

### API Endpoints
- /api/session/is-auth-enabled: clean PASS
- /api/version: clean ("v1.0.0") PASS
- /api/providers: clean PASS
- No BIFROST leakage in any API response PASS

### Route Accessibility
- /: 200 OK
- /providers: 200 OK
- /virtual-keys: 200 OK
- /logs: 200 OK
- /mcp-registry: 200 OK
- /plugins: 200 OK
- /config: 200 OK
- /governance: 200 OK
- /routing-rules: 200 OK
- /model-limits: 200 OK

### Env Variable Mapping (BIFROST -> KOUTAKU)
- BIFROST_PORT -> KOUTAKU_PORT
- BIFROST_IS_ENTERPRISE -> KOUTAKU_IS_ENTERPRISE
- BIFROST_ENTERPRISE_TRIAL_EXPIRY -> KOUTAKU_ENTERPRISE_TRIAL_EXPIRY
- BIFROST_DISABLE_PROFILER -> KOUTAKU_DISABLE_PROFILER
- BIFROST_UI_DEV -> KOUTAKU_UI_DEV
- BIFROST_ADMIN_USERNAME -> KOUTAKU_ADMIN_USERNAME
- BIFROST_ADMIN_PASSWORD -> KOUTAKU_ADMIN_PASSWORD
- BIFROST_ENCRYPTION_KEY -> KOUTAKU_ENCRYPTION_KEY
- BIFROST_BASE_URL -> KOUTAKU_BASE_URL
- BIFROST_VK_* -> KOUTAKU_VK_*
- BIFROST_MCP_* -> KOUTAKU_MCP_*
- BIFROST_OBJECT_STORAGE_* -> KOUTAKU_OBJECT_STORAGE_*
- BIFROST_POSTGRES_* -> KOUTAKU_POSTGRES_*
- BIFROST_WEAVIATE_* -> KOUTAKU_WEAVIATE_*
- BIFROST_REDIS_* -> KOUTAKU_REDIS_*
- BIFROST_QDRANT_* -> KOUTAKU_QDRANT_*
- BIFROST_PINECONE_* -> KOUTAKU_PINECONE_*
- BIFROST_MAXIM_* -> KOUTAKU_MAXIM_*

### Files Modified
**Phase 1 (128 files):** UI source, config JSON, test files, logo assets
**Phase 2 (7 files):** Makefile, Helm _helpers.tpl, Dockerfile, Python tests, test fixtures

### Git Commits
- ceca4d5: refactor: complete BIFROST -> KOUTAKU rename (env vars, config, tests, logos)
- 14fdd7b: refactor: complete BIFROST->KOUTAKU rename phase 2 (Makefile, Helm, Docker, Python tests)

---

## Notes

- The dogfood-output/report.md from previous session still mentions "bifrost" but that is a historical QA report, not source code - does not need updating.
- Vite dev server may cache old filenames temporarily; production builds will use current files only.
- Go backend has internal references to getkoutaku.ai for pricing datasheet (not BIFROST-related, separate issue).
