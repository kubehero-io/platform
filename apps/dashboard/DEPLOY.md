# Deploying the dashboard to Vercel

The marketing site (kubehero.io) and the dashboard live in different
Vercel projects and different repos. The marketing site auto-detects
itself from `kubehero-web`. This dashboard needs a one-time setup
because it lives inside a pnpm monorepo.

## One-time setup (5 minutes)

1. **Create a new Vercel project** pointed at the
   `kubehero/kubehero-platform` repo.

2. **Settings → Build & Development Settings**:
   - **Root Directory**: `apps/dashboard`
   - **Framework Preset**: `Next.js` (auto-detected)
   - **Include source files outside of the Root Directory**: ON
     (the dashboard imports from `packages/proto/gen/ts/...`)
   - Build / Install commands: leave blank — `vercel.json` in this
     directory overrides them with the pnpm-workspace forms.

3. **Settings → Environment Variables** — paste the values from
   `.env.example`:
   - `CONTROL_PLANE_URL` (required for live mode; if unset, dashboard
     stays in demo mode and `DataSourceBadge` reads "demo")
   - `CONTROL_PLANE_TOKEN` (only needed once `KUBEHERO_REQUIRE_AUTH=true`
     on the cp)

4. **Settings → Domains** — point e.g. `app.kubehero.io` at the project.

5. **First deploy** — push to main (or click "Redeploy" in Vercel).
   The build runs:
   ```
   pnpm install --frozen-lockfile --filter @kubehero/dashboard...
   pnpm --filter @kubehero/dashboard build
   ```

## Live vs demo mode

The dashboard works in both modes; the difference is what
`DataSourceBadge` shows in each page header:

- **`live`** — `CONTROL_PLANE_URL` is set and the cp responds. Every
  page (`/fleet`, `/waste`, `/budgets`, `/ceilings`, `/chargeback`,
  `/posture`, `/workloads/...`) fetches from the cp.
- **`demo`** — `CONTROL_PLANE_URL` is unset OR the cp is unreachable.
  Pages render the same shape from the in-app demo set so users
  can poke around without a live backend.

## Authentication

Demo-mode session: any email at `/login`, no password. The session
cookie is `kh_session` (JSON, 7-day TTL). When real auth lands, that
cookie's logic is the only seam that changes — see
`lib/session.ts` for details.

## Rollback

```
vercel rollback <deployment-url>
```

or click the previous deployment in the Vercel dashboard → Promote.
