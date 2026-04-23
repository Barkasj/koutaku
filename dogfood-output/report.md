# Dogfood QA Report

**Target:** http://134.199.150.205:8080
**Date:** 2026-04-23
**Scope:** Full UI + API exploratory QA testing of Koutaku AI Gateway local dev instance
**Tester:** Hermes Agent (automated exploratory QA)

---

## Executive Summary

| Severity | Count |
|----------|-------|
| 🔴 Critical | 0 |
| 🟠 High | 3 |
| 🟡 Medium | 4 |
| 🔵 Low | 3 |
| **Total** | **10** |

**Overall Assessment:** All pages render and load without crashes or JS errors. The main issues are stale Bifrost-era references (domain getkoutaku.ai, logo filenames, GitHub org) that need updating to Koutaku branding. No critical blockers found.

---

## Issues

### Issue #1: Sidebar logo and icon images return 404 (bifrost files not renamed)

| Field | Value |
|-------|-------|
| **Severity** | 🟠 High |
| **Category** | Visual |
| **URL** | All pages |

**Description:**
The sidebar component (`ui/components/sidebar.tsx`) references `/koutaku-logo.webp`, `/koutaku-icon.webp`, and their dark variants. However, the actual files in `ui/public/` are still named `bifrost-logo.webp`, `bifrost-icon.webp`, etc. This causes 404 errors on every page load, resulting in broken logo/icon images in the sidebar.

**Steps to Reproduce:**
1. Open any page on http://134.199.150.205:8080
2. Check browser console - two 404 errors for `koutaku-logo.webp` and `koutaku-icon.webp`
3. Observe broken/missing logo in sidebar

**Expected Behavior:**
Sidebar logo and icon should render correctly using renamed Koutaku-branded files.

**Actual Behavior:**
Logo and icon files are 404 because they were never renamed from `bifrost-*` to `koutaku-*`.

**Fix:** Rename files in `ui/public/`:
- `bifrost-logo.webp` → `koutaku-logo.webp`
- `bifrost-logo-dark.webp` → `koutaku-logo-dark.webp`
- `bifrost-icon.webp` → `koutaku-icon.webp`
- `bifrost-icon-dark.webp` → `koutaku-icon-dark.webp`
- Also: `maxim-logo.webp` / `maxim-logo-dark.webp` appear to be stale too

---

### Issue #2: "Evals" sidebar link points to dead domain getkoutaku.ai

| Field | Value |
|-------|-------|
| **Severity** | 🟠 High |
| **Category** | Functional |
| **URL** | All pages (sidebar) |

**Description:**
The "Evals" navigation item in the sidebar links to `https://www.getkoutaku.ai`, which is a stale domain from the Bifrost era that no longer resolves. Clicking this link leads to a browser error page.

**Steps to Reproduce:**
1. Expand sidebar
2. Click "Evals" link
3. Browser navigates to `https://www.getkoutaku.ai` which fails to load

**Expected Behavior:**
Link should either point to a valid Koutaku URL, a local route (e.g. `/workspace/evals`), or be removed/hidden if the Evals feature isn't available yet.

**Actual Behavior:**
Link points to dead domain, user gets browser error.

**Source:** `ui/components/sidebar.tsx` line 710

---

### Issue #3: Sidebar "Full Documentation" link points to dead domain docs.getkoutaku.ai

| Field | Value |
|-------|-------|
| **Severity** | 🟠 High |
| **Category** | Functional |
| **URL** | All pages (sidebar footer) |

**Description:**
The "Full Documentation" link in the sidebar footer points to `https://docs.getkoutaku.ai`, which is a stale domain that doesn't resolve. This affects all pages.

**Steps to Reproduce:**
1. Scroll to bottom of sidebar
2. Click "Full Documentation"
3. Browser navigates to dead domain

**Expected Behavior:**
Documentation link should point to valid docs URL or local docs page.

**Actual Behavior:**
Dead link.

**Source:** `ui/components/sidebar.tsx` line 122

---

### Issue #4: Multiple "Read more" links point to docs.getkoutaku.ai (dead domain)

| Field | Value |
|-------|-------|
| **Severity** | 🟡 Medium |
| **Category** | Functional |
| **URL** | Multiple empty-state pages |

**Description:**
At least 15 empty-state "Read more" links across the UI point to `https://docs.getkoutaku.ai/*` paths. These are found in:
- Virtual Keys, Teams, Customers, Routing Rules, Model Limits, Pricing Overrides, MCP Registry
- Guardrails Configuration, Guardrails Providers, Cluster, Adaptive Routing, RBAC, Access Profiles, Audit Logs, API Keys, SCIM, etc.

All of these lead to dead pages.

**Steps to Reproduce:**
1. Navigate to any empty-state page (e.g. `/workspace/governance/virtual-keys`)
2. Click "Read more" button
3. Browser navigates to `docs.getkoutaku.ai` which doesn't resolve

**Expected Behavior:**
Read more links should point to valid documentation.

**Actual Behavior:**
All dead links.

**Affected files:** Multiple files under `ui/app/_fallbacks/enterprise/components/` and `ui/app/workspace/*/views/*`

---

### Issue #5: "Report a bug" sidebar link has inconsistent GitHub org

| Field | Value |
|-------|-------|
| **Severity** | 🟡 Medium |
| **Category** | Content |
| **URL** | All pages (sidebar) |

**Description:**
The "Report a bug" link uses URL `https://github.com/koutaku/koutaku/issues/new?...&projects=koutakuhq/1`. The repo path references `koutaku` org but the project references `koutakuhq` org. This is inconsistent and likely broken.

**Steps to Reproduce:**
1. Click "Report a bug" in sidebar
2. Observe URL contains both `github.com/koutaku/koutaku` and `projects=koutakuhq/1`

**Expected Behavior:**
Consistent GitHub org reference matching the actual repository location.

**Actual Behavior:**
Mixed org references that may lead to 404 on GitHub.

**Source:** `ui/components/sidebar.tsx` line 116

---

### Issue #6: "GitHub Repository" sidebar link points to github.com/koutaku/koutaku (may be stale)

| Field | Value |
|-------|-------|
| **Severity** | 🟡 Medium |
| **Category** | Content |
| **URL** | All pages (sidebar) |

**Description:**
The "GitHub Repository" link points to `https://github.com/koutaku/koutaku`, but the actual repo remote is `github.com/Barkasj/koutaku`. If the `koutaku` org doesn't exist on GitHub, this is a dead link.

**Steps to Reproduce:**
1. Click "GitHub Repository" in sidebar
2. If `github.com/koutaku/koutaku` doesn't exist, get 404

**Expected Behavior:**
Link should point to the actual repository URL.

**Actual Behavior:**
Potentially dead link.

**Source:** `ui/components/sidebar.tsx` line 111

---

### Issue #7: "Book a demo" Calendly link uses stale koutakuai username

| Field | Value |
|-------|-------|
| **Severity** | 🟡 Medium |
| **Category** | Content |
| **URL** | All pages (sidebar + enterprise features) |

**Description:**
The "Book a demo" link and sidebar production setup banner use `https://calendly.com/koutakuai/koutaku-demo?utm_source=bfd_sdbr`. The `bfd_sdbr` UTM source is a Bifrost reference, and the Calendly username may be stale.

**Steps to Reproduce:**
1. Click "Book a demo" on Cluster Config or Adaptive Routing page
2. Or click "here" in sidebar production setup banner

**Expected Behavior:**
Calendly link should use current organization's booking page.

**Actual Behavior:**
Potentially broken Calendly link with Bifrost UTM tags.

**Source:** `ui/components/sidebar.tsx` line 139

---

### Issue #8: API server logs warn about failed DNS lookup for getkoutaku.ai on startup

| Field | Value |
|-------|-------|
| **Severity** | 🔵 Low |
| **Category** | Console |
| **URL** | Server startup |

**Description:**
On server startup, the Go API server attempts to download model parameters and pricing data from `https://getkoutaku.ai/datasheet/*`. DNS lookup fails with "no such host". The server retries in background, generating repeated warnings.

**Steps to Reproduce:**
1. Start `make dev`
2. Observe warn logs about `getkoutaku.ai` DNS failure

**Expected Behavior:**
Default datasheet URLs should use a valid domain or be configurable/optional for local dev.

**Actual Behavior:**
Repeated DNS failure warnings every hour on retry.

---

### Issue #9: Stale `bifrost-*` and `maxim-*` files remain in ui/public/

| Field | Value |
|-------|-------|
| **Severity** | 🔵 Low |
| **Category** | Content |
| **URL** | N/A (file system) |

**Description:**
Files `bifrost-logo.webp`, `bifrost-icon.webp`, `bifrost-logo-dark.webp`, `bifrost-icon-dark.webp`, `maxim-logo.webp`, and `maxim-logo-dark.webp` remain in `ui/public/` even though the sidebar now references `koutaku-*` filenames. These stale files are dead weight and indicate incomplete branding migration.

**Steps to Reproduce:**
1. List `ui/public/` directory
2. Observe `bifrost-*` and `maxim-*` files still present

**Expected Behavior:**
Only `koutaku-*` branded files should exist after migration.

**Actual Behavior:**
Old branded files still present, new named files missing.

---

### Issue #10: Sidebar "Prompt Repository" link missing from some collapsed states

| Field | Value |
|-------|-------|
| **Severity** | 🔵 Low |
| **Category** | Visual |
| **URL** | Various pages |

**Description:**
When navigating to some pages (e.g. `/workspace/prompt-repo`), the "Prompt Repository" sidebar link disappears from the snapshot while other section links remain. This may be a rendering quirk where the item is scrolled out of view or collapsed differently. Needs verification with visual inspection.

**Steps to Reproduce:**
1. Navigate to `/workspace/prompt-repo`
2. Observe sidebar snapshot - "Prompt Repository" link not visible in some states

**Expected Behavior:**
Sidebar navigation should be consistent across all pages.

**Actual Behavior:**
Minor inconsistency in sidebar item visibility.

---

## Issues Summary Table

| # | Title | Severity | Category | URL |
|---|-------|----------|----------|-----|
| 1 | Sidebar logo/icon 404 (bifrost files not renamed) | 🟠 High | Visual | All pages |
| 2 | "Evals" link → dead getkoutaku.ai | 🟠 High | Functional | All pages (sidebar) |
| 3 | "Full Documentation" → dead docs.getkoutaku.ai | 🟠 High | Functional | All pages (sidebar) |
| 4 | 15+ "Read more" links → dead docs.getkoutaku.ai | 🟡 Medium | Functional | Multiple empty-state pages |
| 5 | "Report a bug" link has inconsistent GitHub org | 🟡 Medium | Content | All pages (sidebar) |
| 6 | "GitHub Repository" → possibly wrong org | 🟡 Medium | Content | All pages (sidebar) |
| 7 | Calendly "Book a demo" link uses stale username + Bifrost UTM | 🟡 Medium | Content | All pages (sidebar) |
| 8 | Server startup DNS failure warnings for getkoutaku.ai | 🔵 Low | Console | Server startup |
| 9 | Stale bifrost-* and maxim-* files in ui/public/ | 🔵 Low | Content | N/A (filesystem) |
| 10 | Sidebar "Prompt Repository" visibility inconsistency | 🔵 Low | Visual | Various pages |

## Testing Coverage

### Pages Tested (all return HTTP 200)
- /workspace/dashboard (with all 5 tabs: Overview, Provider Usage, Model Rankings, MCP usage, User Rankings)
- /workspace/logs
- /workspace/mcp-logs
- /workspace/observability
- /workspace/config/logging
- /workspace/providers
- /workspace/model-catalog
- /workspace/model-limits
- /workspace/routing-rules
- /workspace/custom-pricing/overrides
- /workspace/custom-pricing
- /workspace/mcp-registry
- /workspace/mcp-tool-groups
- /workspace/mcp-auth-config
- /workspace/mcp-settings
- /workspace/governance/virtual-keys
- /workspace/governance/users
- /workspace/governance/teams
- /workspace/governance/business-units
- /workspace/governance/customers
- /workspace/scim
- /workspace/governance/rbac
- /workspace/governance/access-profiles
- /workspace/audit-logs
- /workspace/guardrails/configuration
- /workspace/guardrails/providers
- /workspace/plugins
- /workspace/cluster
- /workspace/adaptive-routing
- /workspace/prompt-repo
- /workspace/config/client-settings
- /workspace/config/compatibility
- /workspace/config/caching
- /workspace/config/security
- /workspace/config/api-keys
- /workspace/config/performance-tuning

### API Endpoints Tested
- GET /v1/health → 200 ✅
- GET /v1/models → 200 ✅
- POST /v1/chat/completions → 400 (expected - no providers)
- GET /v1/providers → 200 ✅
- GET /v1/mcp/clients → 200 ✅
- GET /v1/plugins → 200 ✅
- GET /v1/config → 200 ✅
- GET /v1/virtualKeys → 200 ✅
- GET /v1/teams → 200 ✅
- GET /v1/labels → 200 ✅
- GET /v1/cache → 200 ✅

### Features Tested
- Sidebar navigation (all sections expand/collapse correctly)
- Dashboard tabs switching
- Empty state pages with CTAs
- Console error monitoring (no JS errors on any page)
- API health check
- Theme toggle button present

### Not Tested / Out of Scope
- Provider creation flow (no API keys configured)
- Virtual key creation flow
- MCP server addition flow
- Chat completions with actual providers
- Form validation on settings pages
- Authentication/login flow
- Responsive design (mobile)
- Performance/load testing

### Blockers
- No providers configured, so most CRUD flows show empty states only
- No admin credentials set up, so API Keys page shows alert

---

## Notes

The core finding is that the Bifrost → Koutaku branding migration is incomplete. The application itself is functional - all routes load, all APIs respond, no JS errors. The issues are predominantly about stale external links pointing to dead `getkoutaku.ai` domains and missing logo file renames.

Priority fix order:
1. Rename logo files (Issue #1) - quick fix, high visual impact
2. Update or remove "Evals" link (Issue #2) - dead link in main navigation
3. Update documentation links (Issues #3, #4) - systematic replacement of `docs.getkoutaku.ai`
4. Fix GitHub org references (Issues #5, #6)
5. Update Calendly link (Issue #7)
6. Configure datasheet URL override for local dev (Issue #8)
