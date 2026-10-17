---
name: release-devsy-fork
description: Devsy fork の採用 PR を追加・除外し、upstream 追随、競合解決、GitHub Actions による CLI／agent release、mise 取得確認、upstream への復帰を行う。atty303/devsy の fork release 運用を依頼されたときに使う。
---

# Release the Devsy fork

## Establish the requested operation

Use this skill for requests such as「この PR を追加して release」「upstream に追随して release」「merge 済み PR を外して release」「競合を解決して再実行」「全 PR が merge されたので upstream に戻したい」. Ordinary Devsy workspace operations and upstream feature development do not trigger this workflow.

The user initiates each operation through the skill; the agent edits configuration, invokes Actions, investigates failures, resolves integration conflicts, and verifies the published result. Do not schedule releases. A request to edit or inspect configuration alone does not authorize publication. A request to release authorizes the required management commit/push and workflow execution; do not ask again before each step.

Fetch and inspect the current checkout, local changes, remotes and the fork's current default branch. Use the current checkout and preserve unrelated changes and PR branches. Work on `fork-release` for management changes; do not create another worktree. Inspect `hack/fork_release/config.json`, the helper's current schema, and `.github/workflows/fork-release.yml` on that branch. Do not rely on branch names, versions, SHAs, or success recorded in an earlier conversation.

## Change the selection

Edit the ordered `branches` array only for the user's requested additions/removals. Resolve the named repository and full branch ref for inspection. Store only the branch name in `branches[].branch`, without `refs/heads/` or a remote prefix; a branch name itself may contain `/`. If local and remote tips differ, establish which source the user intends before publishing. Do not rewrite a submitted PR branch to match a local tip.

Check dependencies and the source diff for each added branch. Configure its required `checks`. The helper rejects unknown checks: implement and validate the corresponding release gate before admitting a new check name. Keep the existing selected changes unless removal is requested. Do not automatically remove merged PRs. For a request to remove merged PRs, confirm their changes are present in the chosen upstream source before editing the selection.

Keep fork-only adjustments under `hack/fork_release/patches`; they are separate from submitted PR branches. Do not duplicate build or publication logic in this skill. The helper and workflow own execution and configuration validation.

## Run and observe

Validate changed helper code and configuration, follow the repository's commit/review rules, and push management changes non-force to `atty303/devsy:fork-release`. The fork's default branch must be `fork-release` for workflow discovery. Initial setup or changing the default branch requires a request covering that repository change.

For a requested release, run `gh workflow run fork-release.yml --repo atty303/devsy --ref fork-release`. Identify the resulting workflow-dispatch run by workflow, branch, management SHA, actor and invocation time, then retain its URL. Follow that exact run to completion; do not infer success from a previous run or successful dispatch.

Inspect the assembly's `build-info.json` and operation records. Require the pinned upstream/selected SHAs and patch hashes, the intended check selection, successful Linux/macOS unit gates, CLI CI lint, selected E2Es and all four builds. Check a `Required check passed: <name>` marker for every entry in the manifest's `checks`, rather than relying only on the E2E job status. Missing, skipped or failed required jobs/checks are not successful validation. Never remove a gate or skip a failing test to publish; diagnose the failure and fix its owning source within the authorized scope.

The workflow serializes releases, reserves a new `v0.0.0-atty.N` tag, uploads all assets to a draft, and only then publishes a regular latest release. A failed run can leave a reserved tag or draft. Inspect it, preserve it, and rerun with a new version; do not overwrite or delete published assets/tags or reuse a reserved version. If publication succeeded while a required check or mise verification remains unconfirmed, report and diagnose that boundary before considering another release. `v0.0.0-atty.1` lacks pre-publication GH relay E2E evidence; exclude it from verified rollback candidates.

## Resolve an integration conflict

Use the failed run's pinned input record and operation log to reconstruct the exact parents in a task-owned temporary clone. Do not change the submitted branches. Attempt a semantics-preserving resolution; ask only when product behavior or another user-owned decision is ambiguous.

Create the complete resolved merge diff against its first parent with `git diff --binary <first-parent>` after resolving and staging all files. Store that patch in the management branch. Set the affected branch's `resolution` to `{"before_tree":"<first-parent tree SHA>","incoming":"<incoming commit SHA>","patch":"hack/fork_release/patches/<name>.patch"}`. It represents the entire resolved merge tree, including nonconflicting incoming changes, rather than only the conflict hunks. The helper recreates a two-parent merge and refuses changed parents, invalid patches, or a resolution whose merge no longer conflicts.

Replay the patch and relevant tests, review the management change, push it, and rerun Actions. New branch tips may invalidate a resolution; never silently adapt or force an obsolete patch. Clean up only the temporary resources created by this operation.

## Verify consumption

Confirm the final release is public, non-draft, non-prerelease and GitHub's latest release. Verify its tag SHA, four binary asset names, checksums and `build-info.json` against the completed run. Use isolated mise config/data/cache directories outside the repository to check `mise latest github:atty303/devsy` and execute `devsy --version` from that version. Do not modify the user's global installation or another repository's settings.

Immediate verification sets `MISE_MINIMUM_RELEASE_AGE=0s` only in the disposable environment because mise normally hides releases younger than 24 hours. Report this condition; consumer settings remain user-owned. If publication succeeded but this post-publication verification failed, rerun only verification with `gh workflow run fork-release.yml --repo atty303/devsy --ref fork-release -f verify_version=<published-version>`. Follow the exact resulting run and require its `verify-published` job to succeed. This mode neither assembles nor publishes a release; do not claim its skipped build/E2E jobs provide new validation. Retain the original release run as their evidence.

The binary's default agent URL points to the same fork release. Explicit `DEVSY_AGENT_URL` and context `AGENT_URL` retain precedence; check their presence/source without disclosing credentials. If an override points to an older release, explain the mismatch and only change consumer configuration when requested. Do not claim a real provider workspace was exercised from source tests or isolated mise consumption.

Report the release URL/version, integration SHA, Actions URL/result, mise-resolved version and any unverified boundary.

## Retire

For an upstream-return request, verify the selected changes are in upstream. Empty the selection when requested and push that management change; an empty selection stops publication. Keep existing tags/releases. Identify the upstream version containing the required changes and explain switching the consumer backend back to `github:devsy-org/devsy` and removing fork-specific agent overrides. Change consumer files only when explicitly included in the request. Do not delete the fork, branches, release history, or cloud resources.

## Diagnostic records

The helper records public Git refs, selected commands and operation outcomes to `operations.jsonl` beside its output. `DEVSY_FORK_RECORDING=0` disables this local record without changing release behavior. Workflow artifacts retain assembly and build evidence for seven days; GitHub's run logs provide job status. Delete local task-owned output directories after collecting required evidence. Never include credentials, raw authentication streams or a full environment dump in artifacts or release notes.
