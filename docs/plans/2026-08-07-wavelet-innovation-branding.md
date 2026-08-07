# Wavelet Innovation API Platform Rebrand Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Rebrand every relevant user-visible frontend default as Wavelet Innovation API Platform without changing compatibility-sensitive Sub2API identifiers.

**Architecture:** Centralize branded fallback values in a small frontend configuration module and use them wherever runtime site settings are unavailable. Update static metadata and locale copy directly, retain technical protocol names, then build a custom application image and redeploy the existing Compose service without replacing persisted data.

**Tech Stack:** Vue 3, TypeScript, vue-i18n, Vitest, Vite, Go embedded frontend, Docker Compose, PostgreSQL, Redis.

### Task 1: Establish branding regression coverage

**Files:**
- Create: `frontend/src/config/branding.ts`
- Create: `frontend/src/config/__tests__/branding.spec.ts`

1. Write tests for the approved site name, subtitle, and browser-title helper.
2. Run the focused test and confirm it fails because the branding module does not exist.
3. Add the minimal branding constants and helper.
4. Run the focused test and confirm it passes.

### Task 2: Replace visible frontend fallbacks

**Files:**
- Modify: `frontend/index.html`
- Modify: `frontend/public/logo.svg`
- Modify: `frontend/src/main.ts`
- Modify: `frontend/src/stores/app.ts`
- Modify: `frontend/src/router/title.ts`
- Modify: relevant auth, home, key-usage, public-legal, settings, and setup views

1. Extend tests to cover static title and the primary fallback consumers.
2. Confirm the new expectations fail against upstream defaults.
3. Import and use the centralized branding constants; replace visible placeholders.
4. Run focused tests and existing router/store tests.

### Task 3: Rewrite onboarding and settings copy

**Files:**
- Modify: `frontend/src/i18n/locales/en/misc.ts`
- Modify: `frontend/src/i18n/locales/zh/misc.ts`
- Modify: `frontend/src/i18n/locales/en/landing.ts`
- Modify: `frontend/src/i18n/locales/zh/landing.ts`
- Modify: `frontend/src/i18n/locales/en/admin/settings.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/settings.ts`

1. Add locale assertions for the approved internal-platform wording.
2. Confirm assertions fail with upstream copy.
3. Replace product-brand and public-sales wording while retaining legitimate interoperability references.
4. Run locale compilation and focused branding tests.

### Task 4: Align backend presentation defaults

**Files:**
- Modify: backend setting and email fallback files identified by focused search
- Test: relevant backend service tests

1. Add or adjust tests for site-name presentation defaults.
2. Confirm they fail with `Sub2API` defaults.
3. Update only presentation-related fallbacks; retain logs, protocol headers, and internal project identifiers.
4. Run the relevant Go test packages.

### Task 5: Verify and deploy

**Files:**
- Modify: `deploy/docker-compose.local.yml` only if needed to select the custom image/build

1. Run focused frontend tests, type-checking, locale tests, and production build.
2. Run relevant backend tests and document unrelated baseline failures separately.
3. Build a versioned local Docker image.
4. Update database-backed site settings without exposing secrets.
5. Recreate the application container while preserving PostgreSQL and Redis data.
6. Verify container health, local branded responses, and the Cloudflare-protected management hostname.
