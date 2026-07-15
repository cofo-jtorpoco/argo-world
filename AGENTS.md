# AGENTS.md — argo-world

Guía para agentes que toquen este repo. Léela antes de cambiar infra o el flujo de eventos.

## Qué es

Ecosistema Argo completo (CD · Workflows · Events · Rollouts) en un clúster local
(Docker Desktop, nodo kind `desktop-control-plane`, arm64, ~7.7 Gi). Un partido del
Mundial 2026 real (`worldcup26.ir`) lo atraviesa: gol → canary; anomalía → rollback
(abort del Rollout + `git revert`). Arquitectura y demos: [README.md](README.md).

- `src/` — 3 servicios Go (solo código de app). Imágenes distroless ~10 Mi.
- `gitops/` — lo ÚNICO que Argo CD sincroniza (`root` → `platform` + `apps`).
- `bootstrap/` — instala los 4 controladores por Helm; **no** lo gestiona Argo CD.

## Regla de oro del flujo de estado

- El score del scoreboard vive en el **env del Rollout en git**
  (`gitops/apps/scoreboard/rollout.yaml`), en entradas flow de una sola línea marcadas
  `# promote-field: <key>`. El workflow `promote-score` las edita con `sed`. **No las
  reformatees a multilínea** o el sed deja de encontrarlas.
- Un gol **no** reconstruye imagen: cambia el env → nuevo pod-template hash → canary.
- El rollback **solo** parchea `status.abort` del Rollout, nunca el `spec` (el `spec` es
  100% de git; tocarlo pelea con el selfHeal de Argo CD). La parte durable es el
  `git revert`, que devuelve el estado deseado.

## Imágenes: el gotcha más sutil de este entorno

`docker buildx` en Docker Desktop escribe en el **containerd que comparte el nodo de
Kubernetes**. Por eso los pods corren las imágenes locales aunque el `--push` a GHCR
**falle** (el token de `gh` no tiene `write:packages`). Consecuencias:

- Los manifiestos usan `imagePullPolicy: IfNotPresent` a propósito: el nodo usa la imagen
  local sin ir a GHCR. Funciona sin publicar nada.
- Para **portar** a otro clúster sí hace falta poblar GHCR:
  `gh auth refresh -h github.com -s write:packages,read:packages` y luego `make images`.
- `docker build`/`docker tag` "legacy" **no** son visibles para el nodo (dan
  `ErrImageNeverPull`); solo `buildx`. Para actualizar un servicio:
  `docker buildx build --platform linux/arm64 -t <img>:v1 --load src/<svc>` y luego
  `kubectl -n worldcup rollout restart deploy/<svc>`.

## Minas que ya explotaron (y su fix) — no las repitas

El bring-up destapó cinco bugs reales, todos corregidos y commiteados. Si tocas esa zona,
respétalos:

1. **EventBus sin `spec.jetstream.version`** → el reconciler rechaza el spec y el
   sync-wave se atasca (los sensors nunca se crean, sin error visible). Fijado a
   `2.10.10` (de la lista soportada por el controller). — `eventbus.yaml`
2. **`streamConfig` con `replicas` por defecto (3)** → JetStream de 1 nodo rechaza el
   stream (`replicas > 1 not supported in non-clustered mode`); el sensor no se suscribe.
   Hay que fijar `replicas: 1` en el streamConfig (ajuste **separado** de
   `jetstream.replicas`). Tras cambiarlo, **el controller debe regenerar los deployments
   de los sensors** (`EVENTBUS_CONFIG` va embebido en el pod; borrar el pod no basta —
   borra el deployment o reinicia el controller). — `eventbus.yaml`
3. **Filtro del sensor**: es `body.type`, no `data.type` (el webhook anida el body en
   `data.body`); y `type: string` se evalúa como **regex** → anclar `^GOAL$`. — sensors
4. **Imágenes de kubectl**: `bitnami/kubectl:<tag>` fue retirado en 2025 (`not found`);
   `rancher/kubectl` **no trae shell** (`sh not found`). Usar `alpine/k8s` (shell +
   kubectl). — `wft-rollback-score.yaml`
5. **`CronWorkflow` no baja de 1 min** y cada tick lanza un pod → el sondeo lo hace
   `match-watcher` (Deployment con bucle), no un CronWorkflow.

## Trampas al verificar demos

- **No hagas `curl` a `/match/current`** entre disparar `/chaos/anomaly` y el poll del
  watcher: `corrupt_next` es one-shot y tu curl lo consume, "robando" la anomalía.
- Los cambios imperativos al Deployment (`kubectl set env deploy/match-feed`) **los
  revierte el selfHeal de Argo CD**. Para cambiar config de app, cambia git.
- El `git revert` del rollback solo actúa si `HEAD` es un commit `promote:`. Secuencia
  correcta del demo: gol (HEAD=promote) → anomalía (revierte ese promote).
- Los workflows se GC-ean rápido (`ttlStrategy` 180-600s). Observa pronto o mira los
  commits en git como evidencia durable.

## Contrato de despliegue (dev local)

Namespaces: `argocd`, `argo` (workflows), `argo-events`, `argo-rollouts`, `worldcup`
(apps). Secret `github-creds` en `argo` (token de `gh`) para que los workflows hagan
push. Versiones de charts ancladas en `bootstrap/versions.env`.
