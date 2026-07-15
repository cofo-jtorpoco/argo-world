# argo-world

Un ecosistema Argo completo —**CD · Workflows · Events · Rollouts**— en un clúster local,
gobernado por un **partido del Mundial 2026 real** (API pública `worldcup26.ir`).

Cada **gol** dispara un despliegue progresivo (canary). Cada **anomalía en los datos** dispara
un rollback de verdad: aborta el canary *y* revierte el commit en git para que Argo CD no
reaplique la versión mala.

```
worldcup26.ir ──30s──► match-watcher ──POST──► Argo Events (bus + 2 sensors)
                          diff + clasifica              │ submit
   GOAL │ ANOMALY │ NO_CHANGE                           ▼
                                              Argo Workflows
                        promote-score:  git commit del score → push
                        rollback-score: abort status + git revert → push
                        backfill-history: DAG sobre los 104 partidos
                                                        │ git
                                                        ▼
                                              Argo CD (auto-sync + selfHeal)
                                                        ▼
                                              Argo Rollouts
                        canary 20% → analysis(web /consistency) → 50% → 100%
                        análisis falla → auto-abort → vuelve a stable
```

## Puesta en marcha

```bash
make login-ghcr    # docker login a GHCR con el token de gh
make images        # build+push de match-feed, match-watcher, scoreboard (arm64)
make bootstrap     # instala las 4 capas + la Application raíz (app-of-apps)
make ui            # port-forwards: Argo CD, Workflows, Rollouts, marcador
```

Requiere `write:packages` en el token de gh para publicar imágenes:
`gh auth refresh -s write:packages` (abre el navegador — es el único paso interactivo).

## Demos

```bash
make backfill                 # DAG que abanica sobre los 104 partidos
make demo-replay MATCH=64     # revive un partido jugado, gol a gol (a cualquier hora)
make demo-goal                # fuerza un gol → canary
make demo-chaos               # corrompe el feed → rollback (abort + git revert)
make demo-live MATCH=101      # Francia–España (semi) · MATCH=104 = la final
make status                   # salud de todas las capas
```

## Diseño

`src/` es solo código de app (3 servicios Go, imágenes distroless ~10 Mi). `gitops/` es lo único
que Argo CD sincroniza; `bootstrap/` instala los controladores. Las decisiones no obvias y las
minas del stack (filtro `body.type`, JetStream 1-réplica, `rollbackWindow`, cuantización del peso
del canary a `1/réplicas`, RBAC de `workflowtaskresults`) están documentadas en cada manifiesto.
