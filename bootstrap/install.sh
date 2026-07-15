#!/usr/bin/env bash
# Installs the four Argo control planes via Helm, then hands the platform + apps over
# to Argo CD (app-of-apps). The four controllers are the bootstrap layer; everything
# under gitops/ is GitOps-managed from that point on.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/versions.env"

echo "==> helm repo"
helm repo add argo "$ARGO_HELM_REPO" >/dev/null 2>&1 || true
helm repo update argo >/dev/null

install() {
  local release="$1" chart="$2" ns="$3" ver="$4" values="$5"
  echo "==> $release ($chart $ver) -> ns/$ns"
  helm upgrade --install "$release" "argo/$chart" \
    --namespace "$ns" --create-namespace \
    --version "$ver" \
    -f "$HERE/values/$values" \
    --wait --timeout 6m
}

install argocd         argo-cd        "$NS_ARGOCD"    "$ARGOCD_CHART_VERSION"    argocd.yaml
install argo-workflows argo-workflows "$NS_WORKFLOWS" "$WORKFLOWS_CHART_VERSION" workflows.yaml
install argo-events    argo-events    "$NS_EVENTS"    "$EVENTS_CHART_VERSION"    events.yaml
install argo-rollouts  argo-rollouts  "$NS_ROLLOUTS"  "$ROLLOUTS_CHART_VERSION"  rollouts.yaml

# App namespace up front so the ConfigMap seed and RBAC bindings resolve.
kubectl create namespace "$NS_APP" --dry-run=client -o yaml | kubectl apply -f -

echo "==> git push credentials for the rollback/promote workflows"
# Workflows clone+push over HTTPS using a GitHub token. Taken from the gh CLI so no
# secret is hardcoded. Needs 'repo' scope (already present on the active gh account).
if command -v gh >/dev/null 2>&1; then
  TOKEN="$(gh auth token 2>/dev/null || true)"
fi
TOKEN="${TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -z "$TOKEN" ]]; then
  echo "!! No GitHub token (gh auth token / GITHUB_TOKEN). Skipping github-creds secret."
  echo "   Create it later: kubectl -n $NS_WORKFLOWS create secret generic github-creds \\"
  echo "     --from-literal=token=\$TOKEN --from-literal=username=cofo-jtorpoco"
else
  kubectl -n "$NS_WORKFLOWS" create secret generic github-creds \
    --from-literal=token="$TOKEN" \
    --from-literal=username=cofo-jtorpoco \
    --dry-run=client -o yaml | kubectl apply -f -
  echo "   github-creds secret ready in ns/$NS_WORKFLOWS"
fi

echo "==> root Application (app-of-apps)"
kubectl apply -f "$HERE/root-app.yaml"

echo
echo "Done. Argo CD admin password:"
echo "  kubectl -n $NS_ARGOCD get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo"
echo "Watch the platform + apps sync:  kubectl -n $NS_ARGOCD get applications -w"
