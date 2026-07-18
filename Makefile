# argo-world — one ecosystem, driven by a World Cup match.
# Layers: Argo CD (truth) · Workflows (compute) · Events (nerves) · Rollouts (delivery).

IMAGE_REPO ?= ghcr.io/cofo-jtorpoco/argo-world
IMAGE_TAG  ?= v1
NS_APP     ?= worldcup
NS_ARGO    ?= argocd
NS_WF      ?= argo
SERVICES   := match-feed match-watcher scoreboard
MATCH      ?= 64

.PHONY: help
help:
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ---- setup ----

.PHONY: login-ghcr
login-ghcr: ## Log docker into GHCR using the gh token
	gh auth token | docker login ghcr.io -u cofo-jtorpoco --password-stdin

.PHONY: images
images: ## Build + push the 3 images (linux/arm64) to GHCR (needs write:packages)
	@for svc in $(SERVICES); do \
	  echo "==> $$svc"; \
	  docker buildx build --platform linux/arm64 \
	    -t $(IMAGE_REPO)/$$svc:$(IMAGE_TAG) \
	    --push src/$$svc || exit 1; \
	done

# Local path that needs NO GHCR scope: build and inject straight into the kind node's
# containerd (Docker Desktop shares it), then restart the deployment. Use SVC=<name>.
.PHONY: image-local
image-local: ## Build one service and load it into the node (SVC=match-feed|match-watcher|scoreboard)
	docker buildx build --platform linux/arm64 --provenance=false \
	  -t $(IMAGE_REPO)/$(SVC):$(IMAGE_TAG) --load src/$(SVC)
	docker save $(IMAGE_REPO)/$(SVC):$(IMAGE_TAG) | \
	  docker exec -i desktop-control-plane ctr -n k8s.io images import -
	kubectl -n $(NS_APP) rollout restart deploy/$(SVC) 2>/dev/null || \
	  kubectl -n $(NS_APP) rollout restart rollout/$(SVC)

.PHONY: bootstrap
bootstrap: ## Install the 4 Argo control planes + the root Application
	bootstrap/install.sh

.PHONY: pw
pw: ## Print the Argo CD admin password
	@kubectl -n $(NS_ARGO) get secret argocd-initial-admin-secret \
	  -o jsonpath='{.data.password}' | base64 -d; echo

## ---- demo drivers ----

.PHONY: backfill
backfill: ## Fan out a DAG over all 104 matches (Argo Workflows)
	@printf '%s\n' \
	  'apiVersion: argoproj.io/v1alpha1' \
	  'kind: Workflow' \
	  'metadata: {generateName: backfill-, namespace: argo}' \
	  'spec: {serviceAccountName: workflow-sa, workflowTemplateRef: {name: backfill-history}}' \
	  | kubectl -n $(NS_WF) create -f -

# Switching matches goes through git (set-match Workflow), never `kubectl set env`: an
# imperative env change is reverted by Argo CD's selfHeal and the demo silently keeps
# serving the old match. set-match commits, Argo CD syncs, the feed rolls.
.PHONY: set-match
set-match:
	@printf '%s\n' \
	  'apiVersion: argoproj.io/v1alpha1' \
	  'kind: Workflow' \
	  'metadata: {generateName: set-match-, namespace: argo}' \
	  'spec:' \
	  '  serviceAccountName: workflow-sa' \
	  '  workflowTemplateRef: {name: set-match}' \
	  '  arguments:' \
	  '    parameters:' \
	  '      - {name: mode, value: "$(MODE)"}' \
	  '      - {name: match, value: "$(MATCH)"}' \
	  | kubectl -n $(NS_WF) create -f - -o name | xargs -I{} \
	    kubectl -n $(NS_WF) wait {} --for=condition=Completed --timeout=120s
	@# Argo CD polls every 3min by default; nudge it so the demo doesn't look hung. Then
	@# poll the LIVE deployment until the new MATCH_ID actually lands — waiting on
	@# `rollout status` alone would pass instantly against the pre-sync ReplicaSet.
	@kubectl -n $(NS_ARGO) annotate app apps argocd.argoproj.io/refresh=hard --overwrite >/dev/null
	@echo "waiting for Argo CD to sync match $(MATCH)..."
	@for i in $$(seq 1 60); do \
	  cur=$$(kubectl -n $(NS_APP) get deploy/match-feed \
	    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MATCH_ID")].value}' 2>/dev/null); \
	  [ "$$cur" = "$(MATCH)" ] && break; \
	  sleep 2; \
	done
	@kubectl -n $(NS_APP) rollout status deploy/match-feed --timeout=120s

.PHONY: demo-replay
demo-replay: ## Replay a finished match goal-by-goal (MATCH=<1..100>)
	@$(MAKE) --no-print-directory set-match MODE=replay MATCH=$(MATCH)
	@$(MAKE) --no-print-directory chaos-reset
	@echo "replaying match $(MATCH); watch: kubectl argo rollouts get rollout scoreboard -n $(NS_APP) --watch"

.PHONY: demo-live
demo-live: ## Track a real match live (MATCH=101 semi, 104 final)
	@$(MAKE) --no-print-directory set-match MODE=live MATCH=$(MATCH)
	@$(MAKE) --no-print-directory chaos-reset
	@echo "tracking LIVE match $(MATCH)"

.PHONY: demo-goal
demo-goal: ## Force a goal -> promote -> canary
	@$(MAKE) --no-print-directory curlpod EP=/chaos/goal

.PHONY: demo-chaos
demo-chaos: ## Corrupt the next read -> anomaly -> rollback
	@$(MAKE) --no-print-directory curlpod EP=/chaos/anomaly

.PHONY: chaos-reset
chaos-reset:
	@$(MAKE) --no-print-directory curlpod EP=/chaos/reset >/dev/null 2>&1 || true

# Throwaway curl pod (match-feed image is distroless, no shell/curl of its own).
.PHONY: curlpod
curlpod:
	@kubectl -n $(NS_APP) run curl-$$RANDOM --rm -i --restart=Never \
	  --image=curlimages/curl:8.10.1 --quiet -- \
	  -s -X POST http://match-feed:8080$(EP) ; echo

## ---- observe ----

.PHONY: rollout
rollout: ## Watch the canary march
	kubectl argo rollouts get rollout scoreboard -n $(NS_APP) --watch

.PHONY: ui
ui: ## Port-forward all UIs (Argo CD 8080 · Workflows 2746 · Rollouts 3100 · Scoreboard 8090)
	@echo "Argo CD    https://localhost:8080  (admin / \`make pw\`)"
	@echo "Workflows  https://localhost:2746"
	@echo "Rollouts   http://localhost:3100/rollouts"
	@echo "Scoreboard http://localhost:8090"
	@trap 'kill 0' EXIT; \
	 kubectl -n $(NS_ARGO) port-forward svc/argocd-server 8080:443 >/dev/null 2>&1 & \
	 kubectl -n $(NS_WF)   port-forward svc/argo-workflows-server 2746:2746 >/dev/null 2>&1 & \
	 kubectl -n argo-rollouts port-forward svc/argo-rollouts-dashboard 3100:3100 >/dev/null 2>&1 & \
	 kubectl -n $(NS_APP)  port-forward svc/scoreboard 8090:8080 >/dev/null 2>&1 & \
	 wait

.PHONY: status
status: ## Quick health across all layers
	@echo "== applications =="; kubectl -n $(NS_ARGO) get applications 2>/dev/null
	@echo "== rollout =="; kubectl -n $(NS_APP) get rollout scoreboard 2>/dev/null
	@echo "== match-state =="; kubectl -n $(NS_APP) get cm match-state -o jsonpath='{.data}' 2>/dev/null; echo
	@echo "== recent workflows =="; kubectl -n $(NS_WF) get wf --sort-by=.metadata.creationTimestamp 2>/dev/null | tail -6

## ---- teardown ----

.PHONY: nuke
nuke: ## Remove everything
	-kubectl delete -f bootstrap/root-app.yaml
	-helm uninstall argocd -n $(NS_ARGO)
	-helm uninstall argo-workflows -n $(NS_WF)
	-helm uninstall argo-events -n argo-events
	-helm uninstall argo-rollouts -n argo-rollouts
	-kubectl delete ns $(NS_APP) argo-events argo-rollouts $(NS_WF) $(NS_ARGO)
