---
title: Releasing
lead: A release is a version tag on a release branch. The tag starts the only workflow that builds - checks, three Linux binaries, checksums, SBOMs, attestations, the release page, and a Slack message.
description: How theLB is released - the end-to-end test on a development machine, the release branch and tag, the Release workflow, SBOMs and attestations, and the Slack notification.
section: Technical
order: 4
nav: Releasing
---

## What runs when

| Event | What runs |
|---|---|
| A push to `main` or any branch | **nothing** - documentation and code go to `main` as they are written |
| A pull request | nothing |
| A tag `vX.Y.Z` | the **Release** workflow, and **Notify Slack** |

So a documentation change is just a commit to `main`: the site updates within a minute or two, and no workflow runs - see [This documentation](tech-docs.md).

## Cutting a release

1. **Run the end-to-end test** on a Linux development machine, on the commit you will release. It must pass:

   ```bash
   make check
   make remote-e2e REMOTE=<host>      # or make e2e on the machine itself
   ```

2. **Write the changelog.** `CHANGELOG.md` must have a section `## vX.Y.Z - <date>` with what changed. Its text becomes the release page.

3. **Create or update the release branch** `release/vX.Y` from `main`, and push it:

   ```bash
   git switch -c release/v{{minor}} main      # the first release of a line; later: git switch release/v{{minor}} && git merge --ff-only main
   git push origin release/v{{minor}}
   ```

4. **Tag the release on that branch**, and push the tag:

   ```bash
   git tag -a v{{version}} -m "theLB v{{version}}"
   git push origin v{{version}}
   ```

The tag must be on its `release/vX.Y` branch: the workflow checks, and stops if it is not.

## The Release workflow

`.github/workflows/release.yaml` has three jobs:

| Job | What it does |
|---|---|
| **check** | the tag is on its release branch; `make check` (with fixed versions of golangci-lint and govulncheck); the scripts' tests; `docs/build.py --check` |
| **build** (amd64, arm, arm64) | builds the binary with `make dist-one`; attests its build provenance; writes its `.sha256`; writes its SBOMs (CycloneDX 1.6 and SPDX 2.3) and attests both |
| **publish** | collects every file, writes `SHA256SUMS`, and creates the GitHub release with the changelog's section as its notes |

Every third-party action is pinned to a full commit hash. Jobs get only the permissions they need: the build jobs may sign attestations, and only the publish job may write the release.

### The SBOMs

`.github/scripts/sbom.py` reads the build information Go embeds in every binary (`go version -m`) - the Go release, the main module and every dependency with its checksum - and writes it as CycloneDX and SPDX. Standard library only. Each SBOM is attested, so anyone can check that it describes that exact file:

```bash
gh attestation verify thelb-{{version}}-linux-amd64 --repo dmartsapp/thelb --predicate-type https://cyclonedx.org/bom
```

## The Slack notification

`.github/workflows/notify-slack.yaml` starts with the same tag. It waits for the Release run to finish, then posts one message to Slack:

- **Released**: the version, a link to the release page, and how many files it has.
- **Failed**: which job and which step failed, and the last lines of that step's log, so you can see why without opening GitHub.

It reads the webhook from the repository secret `SLACK_WEBHOOK_URL`. Without the secret, it says so and succeeds. It publishes nothing and changes nothing, so if it fails, the release is not affected.

To set the secret:

```bash
gh secret set SLACK_WEBHOOK_URL --repo dmartsapp/thelb
```

(It asks for the webhook URL; paste it there, not on the command line.)

## After a release

- Check the release page: three binaries, three `.sha256` files, six SBOMs, and `SHA256SUMS`.
- Check one download: `sha256sum -c SHA256SUMS --ignore-missing` and `gh attestation verify` - see [Security](security.md#check-a-release).
- Development goes on on `main`. Fixes for a released line are merged to its `release/vX.Y` branch and tagged `vX.Y.Z+1`.
