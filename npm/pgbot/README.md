# pgbot

**In-database observability for PostgreSQL.** One static binary connects
read-only, reads Postgres's own statistics views, and prints a findings-first
health report.

```bash
npx @pgbot/cli inspect "postgres://pgbot_ro@host:5432/db"
```

No prior install. `npx` downloads the wrapper and the prebuilt binary for your
platform (shipped as an `optionalDependency`, so npm installs only the one that
matches your OS/CPU) and runs it. Point it at your database with an argument or
`$DATABASE_URL`; use a role holding `pg_monitor` with no write grants.

## npm, pnpm, Yarn, bun

All four work, and each downloads only your platform's binary:

| | no install | project | global |
|---|---|---|---|
| npm | `npx @pgbot/cli` | `npm i @pgbot/cli` | `npm i -g @pgbot/cli` |
| pnpm | `pnpm dlx @pgbot/cli` | `pnpm add @pgbot/cli` | `pnpm add -g @pgbot/cli` |
| Yarn | `yarn dlx @pgbot/cli` | `yarn add @pgbot/cli` | `yarn global add @pgbot/cli` |
| bun | `bunx @pgbot/cli` | `bun add @pgbot/cli` | `bun add -g @pgbot/cli` |

The wrapper locates the binary at run time and this package ships **no install
scripts**, so it works wherever lifecycle scripts are disabled — `npm ci
--ignore-scripts`, pnpm's blocked-by-default builds, bun. Two Yarn notes:
`yarn dlx` is Berry-only (Classic uses `yarn global add`), and under Berry's
default Plug'n'Play linker there is no `node_modules/.bin`, so invoke it as
`yarn pgbot`.

The package is **scoped** — the bare name `pgbot` is blocked by npm's
package-name-similarity rule, so `npx pgbot` returns `E404`. Use `@pgbot/cli`.

## What npm verifies (and what it doesn't)

The npm packages carry registry integrity hashes and npm **provenance** — a
verifiable link to the GitHub Actions workflow that built them. That is a
*different and weaker* guarantee than the release's **cosign** signature: it
attests where the package came from, not that the artifact was signed.

For the verified path, install the binary with the script and require the
signature:

```bash
PGBOT_REQUIRE_SIGNATURE=1 curl -fsSL https://pgbot.dev/install | sh
```

Full documentation: <https://github.com/pgrundev/pgbot#readme>
