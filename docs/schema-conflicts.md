# Database conflicts between open PRs

Two PRs can each pass CI and still break once both are merged: one renames
`users.email` while the other indexes it, or both create `invoices`. The full
test suite of the second PR finds out only after it runs, and only after the
first PR merged. `pgbot schema conflicts` finds out in the first minute of CI,
while both PRs are still open.

It works on what each PR's migrations actually did to a database, not on the
migration files, so it is the same for Rails, Django, Prisma, Flyway or plain
SQL.

## How it works

1. **Changeset.** In each PR's CI, take a catalog snapshot of the target
   branch's schema, run the PR's migrations on top, take another snapshot.
   `pgbot schema changeset before.json after.json` lists every object the
   migrations created, dropped, renamed or altered: tables, columns, indexes,
   constraints, extensions. Catalog only, no table data.
2. **Exchange.** The changeset is stored as a workflow artifact,
   `pgbot-changeset-pr-<number>`. A PR's check downloads the newest changeset
   of every other open PR into the same base branch.
3. **Check.** `pgbot schema conflicts mine.json others/ --focus '#12'` reports:

   | | when | what breaks |
   |---|---|---|
   | **conflict** | both create one name; one drops or renames what the other changes; one drops a table or column the other's new foreign key or index uses | the second PR to merge: its migration fails ("already exists", "does not exist") or lands on the wrong object |
   | **overlap** | both change the same table, in different objects | nothing yet; whichever merges second must re-run its migrations on top of the first and regenerate `schema.rb` / `structure.sql` |

4. **Report.** A PR comment (posted only when there is something to say, then
   edited in place, including to "none" once it clears), one annotation per
   item, the job summary, and outputs `conflicts` / `overlaps` / `changes`.
   The comment names the other PR (`#11`), so GitHub links it from that PR's
   timeline too.

## GitHub Actions

Run it as its own small workflow, so it reports while the test suite is still
running. A Rails example; any stack works the same way: load the base branch's
schema, snapshot, migrate, snapshot.

```yaml
# .github/workflows/db-conflicts.yml
name: db conflicts
on:
  pull_request:

permissions:
  contents: read
  actions: read        # download other PRs' changesets
  pull-requests: write # comment

concurrency:
  group: db-conflicts-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  db-conflicts:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    services:
      postgres:
        image: postgres:18
        env: { POSTGRES_PASSWORD: postgres }
        ports: ["5432:5432"]
        options: --health-cmd pg_isready --health-interval 2s --health-retries 30
    env:
      RAILS_ENV: test
      DATABASE_URL: postgres://postgres:postgres@localhost:5432/app_conflicts
    steps:
      - uses: actions/checkout@v7
        with: { fetch-depth: 0 }
      - uses: ruby/setup-ruby@v1
        with: { bundler-cache: true }
      - name: install pgbot
        run: |
          curl -fsSL https://pgbot.dev/install | PGBOT_INSTALL_DIR="$HOME/.local/bin" sh
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"

      # 1. The target branch's schema. Check out the base commit itself, not just
      #    its schema file: Rails marks every migration file up to the schema
      #    version as already run, which would skip a PR migration whose
      #    timestamp is older than the newest one on main.
      - name: schema of ${{ github.base_ref }}
        run: |
          git checkout --quiet --detach ${{ github.event.pull_request.base.sha }}
          bin/rails db:create db:schema:load
          pgbot schema snapshot > "$RUNNER_TEMP/before.json"
      # 2. This PR's migrations on top (the merge commit, as the tests see it).
      - name: migrations of this PR
        run: |
          git checkout --quiet --detach ${{ github.sha }}
          bin/rails db:migrate
          pgbot schema snapshot > "$RUNNER_TEMP/after.json"

      - uses: pgrundev/pgbot/conflicts@v1
        with:
          before: ${{ runner.temp }}/before.json
          after: ${{ runner.temp }}/after.json
          # fail-on: conflict   # block the merge instead of only reporting
```

`pgbot schema snapshot` reads `$DATABASE_URL`. If a PR changes
`Gemfile.lock`, run `bundle install` after each checkout.

The database can be anything Postgres-compatible that the job can reach: a
service container (above), a [PGX](https://pgx.dev) database on the runner, or
a PGRun branch. Sizes do not matter for conflicts, so an empty database loaded
from the schema is enough and takes seconds.

### Inputs

| input | default | |
|---|---|---|
| `before` | (required) | target branch schema: snapshot file or connection string |
| `after` | (required) | schema after this PR's migrations: snapshot file or connection string |
| `label` | `#<PR number>` | how this PR is named in reports |
| `fail-on` | `none` | `conflict` fails the job on a conflict, `overlap` on an overlap too |
| `comment` | `true` | post / update the PR comment |
| `others-dir` | | read other changesets from a directory instead of open PRs' artifacts |
| `artifact-prefix` | `pgbot-changeset` | to run two independent checks (two databases) in one repository |
| `retention-days` | `30` | changeset artifact lifetime; a PR idle longer drops out of others' checks |
| `version` | `latest` | pgbot to install when it is not on PATH |

PRs from forks get a read-only token: the check still runs and annotates, but
cannot comment.

## Limits

- A PR's changeset is as fresh as its last CI run. When PR #11 is pushed and
  now collides with #12, #11's check reports it right away; #12's comment
  updates on #12's next run.
- Only the schema is compared. A data migration (`UPDATE` in a migration) or
  application code that reads a column another PR renames is invisible.
- A clean result means no two PRs touch the same objects, not that they work
  together; the full test suite of the merged result is still the proof.

## Without GitHub

The commands are offline and work anywhere:

```sh
pgbot schema snapshot "$BASE_URL" > before.json
# … run the branch's migrations …
pgbot schema changeset before.json "$BRANCH_URL" --label feature-x > feature-x.json
pgbot schema conflicts feature-x.json other-changesets/ --focus feature-x --fail-on conflict
```

Formats: `--format text|json|markdown|github`. Exit codes with `--fail-on`:
0 clean, 1 overlap, 2 conflict.
