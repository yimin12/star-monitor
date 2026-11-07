# star-monitor

Ranks GitHub and GitLab repositories that are gaining attention now and already
had an audience, over a 15-day window. It collects star counts every 6 hours.

## Score

```text
Score = A × B × M × P
A attention   = Δ / √(S0 + K)        between absolute gain and growth rate
B base        = S0 / (S0 + K)        0 for a repository that started from nothing
M momentum    = gain in the last 5 days / its expected share, clamped to 0.5..1.5
P persistence = √(Neff / days), Neff = 1 / Σ(daily share²)
```

`S0` is the star count at the start of the window and `Δ` the gain within it.
`K` is 1000 for GitHub and 100 for GitLab. See `internal/score`.

## Data

- **GitHub:** search by star range (search returns at most 1000 results per query),
  repositories with at least `GITHUB_MIN_STARS` (300) stars, no forks or archived ones.
- **GitLab:** public gitlab.com projects by star count, down to `GITLAB_MIN_STARS` (50).
- **History:** a full 15-day window takes 15 days of collection. Backfilling from
  stargazer dates is off (`BACKFILL_REPOS=0`): since July 2026 GitHub lists stargazers
  only to a repository's admins and collaborators.

One SQLite file holds one count per repository per UTC day, kept for 45 days.

## Configuration

| Variable | Default |
|---|---|
| `GITHUB_TOKEN` | empty: 10 searches a minute |
| `DB_PATH` | `star-monitor.db` |
| `INTERVAL` | `6h` |
| `WINDOW_DAYS` | `15` |
| `GITHUB_MIN_STARS`, `GITLAB_MIN_STARS` | `300`, `50` |
| `GITHUB_K`, `GITLAB_K` | `1000`, `100` |
| `BACKFILL_REPOS`, `BACKFILL_REQUESTS`, `BACKFILL_MAX_STARS` | `0` (off), `2000`, `40000` |

`star-monitor -once` runs one collection pass and exits.

## Delivery

The primary repository is `https://git.dev.topymh.org/yimin12/star-monitor` (Forgejo in the
homelab); GitHub is a push mirror and its Actions are off. Forgejo Actions tests every push,
builds `git.dev.topymh.org/yimin12/star-monitor:sha-<commit>` for amd64 and arm64, scans it
with Trivy, then commits the new tag to `deploy/kustomization.yaml`. Argo CD gets Forgejo's
push webhook and syncs `deploy/` to the `star-monitor` namespace. Roll back by reverting the
tag commit (`git revert <Deploy commit>`). The namespace, volume, route and network policy are
managed by Terraform in the homelab repository.

## Development

Enable the git hooks once per clone: `git config core.hooksPath .githooks`.
`pre-commit` checks gofmt and vet on staged Go files; `pre-push` runs the tests in Docker.

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26.10 go test ./...
docker build -t star-monitor .
```
