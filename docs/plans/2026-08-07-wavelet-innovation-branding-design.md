# Wavelet Innovation API Platform Branding Design

## Objective

Present this deployment as the private **Wavelet Innovation API Platform** while retaining Sub2API's technical compatibility and upstream attribution.

## User Experience

The browser title, login and registration surfaces, setup wizard, navigation branding, public legal wrapper, and onboarding tours use **Wavelet Innovation API Platform**. The default subtitle is **Internal access to approved AI models and services**. English and Chinese descriptions position the service as a private company platform for approved AI access, upstream account management, model routing, API-key issuance, quotas, and operational monitoring. Public-sales language such as VIP packages and free trials is removed from onboarding defaults.

Administrators can still override the site name, subtitle, and logo from Site Settings. Branded source defaults ensure that slow configuration loads, new installations, email fallbacks, and public pages do not flash or fall back to the upstream project name.

## Compatibility Boundary

The rebrand changes presentation only. It does not rename the Go module, package names, container/service names, PostgreSQL database, local-storage keys, WebSocket subprotocols, data import/export formats, environment variables such as `SUB2API_API_KEY`, or upstream URLs and compliance-document attribution. Provider-specific descriptions may retain “Sub2API” when they explicitly describe interoperability with another Sub2API instance.

## Deployment

The existing Compose stack currently pulls `weishaw/sub2api:latest`, so source changes require a locally built, versioned Wavelet image. The application, PostgreSQL, and Redis topology and persisted data remain unchanged. Before the container is replaced, the database-backed `site_name` and `site_subtitle` settings will be inspected and updated through the supported application interface or a narrowly scoped database update. The rebuilt service will remain bound to `127.0.0.1:18443` behind the existing Cloudflare Tunnel and Access policy.

## Verification

Focused regression tests will assert branded defaults and prevent visible fallback copy from reverting. Verification includes focused tests, type-checking, production frontend build, a repository scan that distinguishes allowed technical references, container health, local HTML/public-settings checks, and the Access-protected remote management hostname.
