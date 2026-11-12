#!/bin/sh
# Applies the security settings every db-branch repository should have.
# Safe to run again. Needs `gh` logged in as an owner of the organization.
#
# usage: scripts/apply-github-settings.sh <owner/repo>...
#        scripts/apply-github-settings.sh --org <org>
#
# Branch protection and rulesets are free only for public repositories (or a
# paid plan). Until then GitHub answers 403 and the script says so and goes on;
# run it again after the repository is made public.
#
# Not settable through the API, do it in the web UI once:
#   - the organization requires two-factor authentication
#     (Organization settings -> Authentication security)
set -u
here=$(cd "$(dirname "$0")/.." && pwd)

if [ "${1:-}" = "--org" ]; then
	org=${2:?usage: apply-github-settings.sh --org <org>}
	gh api -X PATCH "orgs/$org" \
		-F default_repository_permission=none \
		-F members_can_create_repositories=false \
		-F members_can_fork_private_repositories=false \
		--jq '"org " + .login + ": default permission " + .default_repository_permission' || echo "org settings: failed"
	exit 0
fi

[ $# -gt 0 ] || { echo "usage: apply-github-settings.sh <owner/repo>... | --org <org>"; exit 2; }
for repo in "$@"; do
	echo "== $repo"
	# Workflows get a read-only token unless a job asks for more; they cannot approve pull requests.
	gh api -X PUT "repos/$repo/actions/permissions/workflow" -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false >/dev/null && echo "  workflow token: read-only"
	# Only actions published by GitHub itself may run.
	gh api -X PUT "repos/$repo/actions/permissions" -F enabled=true -f allowed_actions=selected >/dev/null &&
		gh api -X PUT "repos/$repo/actions/permissions/selected-actions" -F github_owned_allowed=true -F verified_allowed=false >/dev/null &&
		echo "  actions: GitHub-owned only"
	# Dependency alerts and automatic security updates.
	gh api -X PUT "repos/$repo/vulnerability-alerts" >/dev/null && echo "  dependabot alerts: on"
	gh api -X PUT "repos/$repo/automated-security-fixes" >/dev/null && echo "  dependabot security updates: on"
	# No wiki or projects; merged branches are deleted.
	gh api -X PATCH "repos/$repo" -F has_wiki=false -F has_projects=false -F delete_branch_on_merge=true >/dev/null && echo "  wiki/projects off"
	# Rulesets: main cannot be deleted or force-pushed (direct pushes still work); release tags cannot be moved or deleted.
	for f in "$here"/.github/rulesets/*.json; do
		name=$(sed -n 's/.*"name": *"\([^"]*\)".*/\1/p' "$f" | head -1)
		id=$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name==\"$name\") | .id" 2>/dev/null | head -1)
		case $id in '' | *[!0-9]*) id= ;; esac # an error body is not an id
		if [ -n "$id" ]; then
			gh api -X PUT "repos/$repo/rulesets/$id" --input "$f" >/dev/null 2>&1 && echo "  ruleset $name: updated" || echo "  ruleset $name: could not update"
		else
			out=$(gh api -X POST "repos/$repo/rulesets" --input "$f" 2>&1) && echo "  ruleset $name: created" || echo "  ruleset $name: not available on this plan ($(echo "$out" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p' | head -1))"
		fi
	done
done
