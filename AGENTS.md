# AGENTS.md

Guidance for AI agents (Claude Code, Codex, etc.) working in this repo.

## This is a fork

- `origin` → `git@github.com:svetzal/whatsapp-cli.git` (our fork; default push target)
- `upstream` → `git@github.com:vicentereig/whatsapp-cli.git` (original project)

We forked because upstream is slow to merge fixes. Land bug fixes here on `main`
(or feature branches off it) and push to `origin`. Open PRs back to `upstream`
opportunistically, but don't block on them.

### Syncing from upstream

```
git fetch upstream
git merge upstream/main        # or: git rebase upstream/main
git push origin main
```

Resolve conflicts in favour of our local fixes; upstream's `main` may regress
things we've already patched.

## Build and install

The local binary is installed to `~/bin/whatsapp-cli` (which is on `$PATH`
ahead of `/opt/homebrew/bin`). Rebuild and reinstall with:

```
./install.sh
```

`install.sh` runs `go build` and copies the binary to `~/bin/`. Override the
destination with `DEST=/somewhere/else ./install.sh`.

### Do not

- Do **not** symlink or copy binaries into `/opt/homebrew/bin/` or anywhere
  else under `/opt/homebrew/`. That tree is managed by Homebrew.
- Do **not** reinstall the upstream Homebrew formula
  (`vicentereig/tap/whatsapp-cli`) — it would shadow our local build only if
  `~/bin` were removed from PATH, but it also confuses which version is
  running. We removed it intentionally.

## Logging fork-only changes

`CHANGELOG.md` is upstream's. Don't edit it for fork-only work — every
upstream release would create a merge conflict. Instead, add entries to
`CHANGELOG.fork.md` with date headings (the fork has no version numbers;
we install from `main`). When an entry lands upstream, delete it from
`CHANGELOG.fork.md` rather than reconciling it into upstream's file.

## Testing

```
go test ./...
```

There's no Makefile, no lint config, no CI script beyond the GitHub Actions
in `.github/workflows/`. Keep it that way unless adding one solves a real
problem.
