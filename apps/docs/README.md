# @kubehero/docs

Docs site for `docs.kubehero.io`. Fumadocs on Next.js 15.

## Run locally

```bash
pnpm -F @kubehero/docs dev   # :3002
```

## Deploy to Vercel

1. Create a new Vercel project, point it at this repo, **root directory**
   `apps/docs`. Framework auto-detects as Next.js.
2. Add domain `docs.kubehero.io` in project settings.
3. Content edits land immediately — any `.mdx` change in `content/docs`
   rebuilds on push.

`vercel.json` in this directory is already configured with the
monorepo build + install commands (uses pnpm workspace filter).
