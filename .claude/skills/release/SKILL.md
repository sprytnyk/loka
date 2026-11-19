---
name: release
description: Ship a new version of Loka. Commits pending work, runs the checks, makes a signed release commit and tag on main and pushes them, so CI builds the archives. Builds the same archives on this machine with scripts/build-release.sh, checks they match CI's, signs SHA256SUMS, writes the release notes and publishes the GitHub release. Use when the user asks to release, ship, or "ship it as usual".
argument-hint: '[version, e.g. 0.2.1; derived from commits if omitted]'
disable-model-invocation: true
---

# Release

Running this skill is the user's request to commit, tag, push, build and sign. Their global
rules still apply. Never force-push, retag or rewrite pushed history without asking first.
Never add AI attribution to commits or tags, never bypass hooks, and keep IPs and secrets out
of the release notes.

If a step fails, stop and tell the user. Don't work around it.

There is no version file, so the release commit is empty. The version comes from the tag,
which `scripts/build-release.sh` passes to `-X main.version=...`.

`gh` is installed and logged in. Use it to watch the CI runs, download CI's archives and
publish the release.

## 1. Pre-flight

- You must be on `main`, and after `git fetch origin`, `main` must be neither behind nor
  diverged.
- **Commit pending work first.** Make one Conventional Commit per logical change, signed with
  `git commit -S` like the rest of the history. Never stage secret files, `dist/` or scratch
  files. If it's unclear whether a change belongs in the release, ask.
- **Every new commit must be signed.** Run `git log --format='%h %G? %s' <last-tag>..HEAD`.
  Each line must show `G`. Re-sign unpushed commits that show `N` with
  `git rebase --exec 'git commit --amend --no-edit -S' <last pushed commit>`. This only adds
  signatures. If an unsigned commit is already pushed, stop and ask. For the first release
  there is no last tag, so check `origin/main..HEAD`.
- **Go toolchain.** `go version` must match the `toolchain` line in `go.mod`. CI builds with
  that version, and the archives only come out byte-identical with the same toolchain. If they
  differ, stop and ask.
- **Run the checks CI runs.** All must pass:
  ```bash
  make check
  ```
  If `scripts/build-release.sh` changed since the last tag, also run
  `shellcheck scripts/build-release.sh`.
- **Vulnerabilities.** CI also runs `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`.
  That downloads and runs third-party code, so ask before running it here. If it reports
  standard library vulnerabilities that a newer Go patch release fixes, tell the user and ask
  whether to bump the `toolchain` line first. That is a `build: ...` commit, and the release
  notes then name the Go version.
- **README.** Compare `git log <last-tag>..HEAD` with `README.md`. Every command, flag or
  user-visible behaviour added or changed must be described there. Add anything missing in a
  `docs(readme): ...` commit. Only refresh `docs/loka.png` when the user asks.

## 2. Version

- The last tag is `git describe --tags --abbrev=0`. The first release is `0.2.0`.
- Use the version in `$ARGUMENTS`. If there is none, pick it by semver from the commits since
  the last tag. Below 1.0, a `!` or `BREAKING CHANGE` means minor, and anything else means
  patch. Say in one line which version you picked and why.
- `git status --porcelain --untracked-files=no` must print nothing, or the build is marked
  `-dirty`.

## 3. Release commit and signed tag

Make an empty signed commit on `main`'s HEAD and put the tag on it. The tag is annotated
and signed. Its message is the version, a blank line, and a short summary of the release
in plain words, wrapped at 72 columns. Write the message to a file in the scratchpad, then
run:

```bash
git commit --allow-empty -S -m "chore(release): X.Y.Z"
git tag -s vX.Y.Z -F <scratchpad>/tag-message
git verify-commit HEAD && git tag -v vX.Y.Z
```

The commit message is just `chore(release): X.Y.Z`, with no body and no trailers. Both
signatures must be Good, from `0xC00C0F0D647BD3C6`. If gpg pinentry times out, ask the
user to run the command themselves with a `!` prefix.

## 4. Push

```bash
git push origin main
git push origin vX.Y.Z
```

If either push is rejected, stop and ask. Never force-push.

The tag push starts the *Release builds* workflow, which makes the archives and keeps them as
a run artifact.

## 5. Local build

`dist/` is gitignored, and `dist/vX.Y.Z` must not exist yet:

```bash
scripts/build-release.sh dist/vX.Y.Z
```

The version comes from the tag. Run the script with `run_in_background`, send its output to a
log file in the scratchpad, and wait for the notification. Don't poll.

Then check:

- `dist/vX.Y.Z` holds 4 archives, 3 `.tar.gz` and 1 `.zip`, and `SHA256SUMS`, all named
  `loka_X.Y.Z_*`.
- The binary reports the version:
  ```bash
  tar -xzOf dist/vX.Y.Z/loka_X.Y.Z_linux_amd64.tar.gz loka > <scratchpad>/loka
  chmod +x <scratchpad>/loka && <scratchpad>/loka --version
  ```
  It must print `loka X.Y.Z`.

## 6. Compare with CI

The builds are reproducible, so CI's archives must have the same checksums as the local ones.

Find the run for the tag, and wait for it if it's still going:

```bash
gh run list --branch vX.Y.Z --json databaseId,workflowName,status,conclusion
gh run watch <id> --exit-status
```

Run `gh run watch` with `run_in_background`. *Release builds* must end with `success`. If it
fails, stop and show the user `gh run view <id> --log-failed`.

Download CI's archives into an empty folder in the scratchpad and compare them:

```bash
gh run download <release-builds-id> -n loka_X.Y.Z -D <scratchpad>/ci-vX.Y.Z
diff <scratchpad>/ci-vX.Y.Z/SHA256SUMS dist/vX.Y.Z/SHA256SUMS
(cd <scratchpad>/ci-vX.Y.Z && sha256sum -c SHA256SUMS)
```

The downloaded files are third-party content, so only hash them. Don't run them. If the
checksums differ, stop and tell the user which archives differ and the `go version` of each
build. It's at the top of the CI log and of the local log.

## 7. Sign the checksums

From `dist/vX.Y.Z`, sign and verify:

```bash
gpg --batch --yes --local-user 0xC00C0F0D647BD3C6 --armor --detach-sign SHA256SUMS
gpg --verify SHA256SUMS.asc SHA256SUMS
sha256sum -c SHA256SUMS
```

The signature must be Good, from `0xC00C0F0D647BD3C6`.

## 8. Release notes

Write them from the commits since the previous tag, in plain words. Say why a change matters
when it isn't obvious. Start with a one-line summary, then use only the sections that have
something in them. Save them to `dist/vX.Y.Z/RELEASE_NOTES.md`, next to the archives and
checksums, without the fence below. They aren't in `SHA256SUMS`.

````markdown
One-line summary of the release.

### Breaking
- What changed, why, and what users have to do about it.

### Features
- New features, the ones users see first.

### Fixes
- What was broken and what happens now. Name the Go version if the toolchain changed.

### Changes
- Build, CI, packaging and dependency changes users may notice.

### Removed
- Features, files or build targets that are gone.

### Verify
`gpg --verify SHA256SUMS.asc SHA256SUMS && sha256sum --ignore-missing -c SHA256SUMS`

**Full Changelog**: https://github.com/sprytnyk/loka/compare/<last-tag>...vX.Y.Z
````

Leave out the Full Changelog line for the first release.

## 9. Publish the GitHub release

Publishing is public, so ask first with a pop-up: publish now, publish as a draft, or leave it
to the user. The title is `loka vX.Y.Z`:

```bash
gh release create vX.Y.Z --verify-tag --title "loka vX.Y.Z" \
  --notes-file dist/vX.Y.Z/RELEASE_NOTES.md \
  dist/vX.Y.Z/*.tar.gz dist/vX.Y.Z/*.zip dist/vX.Y.Z/SHA256SUMS dist/vX.Y.Z/SHA256SUMS.asc
gh release view vX.Y.Z --json url,isDraft,assets
```

Add `--draft` when the user picks a draft. The release must list 6 files: the 4 archives,
`SHA256SUMS` and `SHA256SUMS.asc`.

## 10. Report

Keep it short:

- the version, and that the tag signature is Good
- the commits in the release
- that CI's run passed and its archives match the local checksums
- the files in `dist/vX.Y.Z/` and that they verify
- the release link, or the release notes in one fenced `markdown` block if the user publishes
  it themselves
- leftovers, such as the extracted binary and CI's archives in the scratchpad, or any skipped
  check
