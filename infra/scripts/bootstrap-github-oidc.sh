#!/usr/bin/env bash
#
# bootstrap-github-oidc.sh — one-time setup for GitHub Actions deployments.
#
# Creates (or updates, idempotently) the AWS IAM OIDC identity provider for
# GitHub Actions and the deploy role that the "Infrastructure" workflow assumes.
# When the GitHub CLI is available and authenticated, it also sets the two
# repository variables the workflow needs.
#
# This is a privileged, account-level bootstrap: run it once, by someone with
# IAM admin permissions, against the correct AWS account. It is NOT something to
# run on every push — see the pre-push hook, which only reminds.
#
# Idempotent: every step checks before it creates, so re-running is safe and
# converges the role's trust and permissions to what this script declares.
#
# Usage:
#   PROJECT=aiap REGION=us-east-1 infra/scripts/bootstrap-github-oidc.sh
#
# Environment:
#   PROJECT       project prefix / resource-name scope   (default: aiap)
#   REGION        default region stored in AWS_REGION    (default: us-east-1)
#   GITHUB_REPO   owner/name; derived from origin if unset
#   ROLE_NAME     deploy role name          (default: ${PROJECT}-github-deploy)

set -euo pipefail

PROJECT="${PROJECT:-aiap}"
REGION="${AWS_REGION:-${REGION:-us-east-1}}"
ROLE_NAME="${ROLE_NAME:-${PROJECT}-github-deploy}"

OIDC_HOST="token.actions.githubusercontent.com"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_TEMPLATE="${SCRIPT_DIR}/deploy-role-policy.json"

say() { printf '  %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v aws >/dev/null || die "the AWS CLI is required"
command -v jq  >/dev/null || die "jq is required"
[[ -f "$POLICY_TEMPLATE" ]] || die "policy template not found: $POLICY_TEMPLATE"

# Derive owner/repo from the git remote unless GITHUB_REPO was supplied.
REPO="${GITHUB_REPO:-}"
if [[ -z "$REPO" ]]; then
  url="$(git config --get remote.origin.url || true)"
  REPO="$(printf '%s' "$url" | sed -E 's#^(git@|https://|ssh://git@)github\.com[:/]+([^/]+/[^/.]+)(\.git)?/?$#\2#')"
fi
[[ "$REPO" == */* ]] || die "cannot determine owner/repo; set GITHUB_REPO=owner/name"

ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)" \
  || die "could not read the AWS account; are credentials configured?"
PROVIDER_ARN="arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_HOST}"
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"

echo "Bootstrapping GitHub Actions deployment"
say "account : ${ACCOUNT_ID}"
say "repo    : ${REPO}"
say "role    : ${ROLE_NAME}"
say "region  : ${REGION}"
echo

# ---------------------------------------------------------------------------
# 1. OIDC identity provider (idempotent)
# ---------------------------------------------------------------------------
if aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$PROVIDER_ARN" >/dev/null 2>&1; then
  say "OIDC provider already exists"
else
  say "creating OIDC provider for ${OIDC_HOST}"
  # AWS validates GitHub's token against a trusted CA and effectively ignores
  # this thumbprint for well-known IdPs, but the API still requires a value.
  # Compute it from the live certificate, with the long-documented fallback.
  thumbprint="$(echo | openssl s_client -servername "$OIDC_HOST" \
      -connect "${OIDC_HOST}:443" 2>/dev/null \
      | openssl x509 -fingerprint -sha1 -noout 2>/dev/null \
      | sed 's/^.*=//; s/://g' | tr '[:upper:]' '[:lower:]')"
  thumbprint="${thumbprint:-6938fd4d98bab03faadb97b34396831e3780aea1}"
  aws iam create-open-id-connect-provider \
    --url "https://${OIDC_HOST}" \
    --client-id-list "sts.amazonaws.com" \
    --thumbprint-list "$thumbprint" >/dev/null
  say "OIDC provider created"
fi

# ---------------------------------------------------------------------------
# 2. Deploy role (idempotent create-or-update of the trust policy)
# ---------------------------------------------------------------------------
# Trust is scoped to this repository. Tighten the "sub" further to specific
# branches or environments once the workflow's needs are settled, e.g.
# "repo:OWNER/REPO:environment:prod".
trust_policy="$(jq -n --arg provider "$PROVIDER_ARN" --arg repo "$REPO" --arg host "$OIDC_HOST" '{
  Version: "2012-10-17",
  Statement: [{
    Effect: "Allow",
    Principal: { Federated: $provider },
    Action: "sts:AssumeRoleWithWebIdentity",
    Condition: {
      StringEquals: { ($host + ":aud"): "sts.amazonaws.com" },
      StringLike:   { ($host + ":sub"): ("repo:" + $repo + ":*") }
    }
  }]
}')"

if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  say "updating the role's trust policy"
  aws iam update-assume-role-policy --role-name "$ROLE_NAME" \
    --policy-document "$trust_policy"
else
  say "creating role ${ROLE_NAME}"
  aws iam create-role --role-name "$ROLE_NAME" \
    --assume-role-policy-document "$trust_policy" \
    --description "GitHub Actions deploy role for ${REPO}" \
    --tags "Key=Project,Value=${PROJECT}" "Key=ManagedBy,Value=bootstrap-github-oidc" >/dev/null
fi

# ---------------------------------------------------------------------------
# 3. Deploy permissions (idempotent put-role-policy)
# ---------------------------------------------------------------------------
# Substitute the account and project into the starter policy, strip the human
# "Comment" field IAM would reject, and attach it inline.
permissions="$(sed -e "s/__ACCOUNT_ID__/${ACCOUNT_ID}/g" -e "s/__PROJECT__/${PROJECT}/g" "$POLICY_TEMPLATE" \
  | jq 'del(.Comment)')"
say "attaching the deploy permissions policy"
aws iam put-role-policy --role-name "$ROLE_NAME" \
  --policy-name "${PROJECT}-deploy" \
  --policy-document "$permissions"

echo
say "role ARN: ${ROLE_ARN}"

# ---------------------------------------------------------------------------
# 4. GitHub repository variables (optional; needs an authenticated gh CLI)
# ---------------------------------------------------------------------------
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  say "setting repository variables via gh"
  gh variable set AWS_DEPLOY_ROLE_ARN --repo "$REPO" --body "$ROLE_ARN" >/dev/null
  gh variable set AWS_REGION --repo "$REPO" --body "$REGION" >/dev/null
  say "set AWS_DEPLOY_ROLE_ARN and AWS_REGION"
else
  echo
  echo "The GitHub CLI is not available or not authenticated. Set these"
  echo "repository variables by hand (Settings -> Secrets and variables -> Actions):"
  echo "    AWS_DEPLOY_ROLE_ARN = ${ROLE_ARN}"
  echo "    AWS_REGION          = ${REGION}"
fi

echo
echo "Done. The Infrastructure workflow's deploy job can now assume this role."
