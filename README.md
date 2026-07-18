# argo-world

Un ecosistema Argo completo —**CD · Workflows · Events · Rollouts**— en un clúster local,
gobernado por un **partido del Mundial 2026 real** (API pública `worldcup26.ir`).

Cada **gol** dispara un despliegue progresivo (canary). Cada **anomalía en los datos** dispara
un rollback de verdad: aborta el canary *y* revierte el commit en git para que Argo CD no
reaplique la versión mala.

```
worldcup26.ir ──30s──► match-watcher ──POST──► Argo Events (bus + 2 sensors)
                       diff + clasifica                │ submit
   GOAL │ ANOMALY │ NO_CHANGE   └─► cm/match-state     ▼
                                    (espejo del feed)  Argo Workflows
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

## La fuente de datos

[`worldcup26.ir`](https://github.com/rezarahiminia/worldcup2026) — API pública, sin token en las
rutas `/get/*`. Cuatro endpoints vivos:

| Endpoint | Qué da | Se usa hoy |
|---|---|---|
| `/get/games` | 104 partidos: marcador, `home_scorers`/`away_scorers`, `time_elapsed`, `finished`, `group`, `matchday` | ✅ `match-feed`, `match-signals` |
| `/get/groups` | clasificación viva: `pts, mp, w, d, l, gf, ga` | ✅ `sync-groups` |
| `/get/teams` | 48 selecciones + URL de bandera (flagcdn) | ✅ `sync-groups` (join de nombres) |
| `/get/stadiums` | 16 sedes, ciudad, capacidad | ❌ |

**Límites**: 120 req/60s en `/get/*`, respuestas cacheadas 30s upstream. `match-feed` cachea
15s, así que el consumo total ronda **7 req/min (6% del límite)** *sin importar cuántas pestañas
del marcador haya abiertas*. Sin esa caché, tres pestañas bastaban para rozar el límite: la
página sondea `/api/live` cada 1.5s y cada request iba a la API.

**No existen** eventos de juego: córners, tarjetas, faltas, cambios y alineaciones dan 404
(`/get/events`, `/get/players`). El detalle más fino disponible es el marcador de penalti en los
goleadores (`"Breel Embolo 17' (p)"`) y el descuento (`90'+5'`). Cualquier "evento" más allá de
eso tendría que inventárselo la app, y este repo no lo hace.

⚠️ La doc del upstream dice que `/get/*` requiere JWT; en la práctica responde sin token. Si
activan la autenticación durante el torneo habrá que montar un secret — el feed se caería sin
aviso previo.

## Los cuatro Argo, y qué hace cada uno

Los cuatro controladores se instalan por Helm desde `bootstrap/` (charts anclados en
`bootstrap/versions.env`), uno por namespace. A partir de ahí, **todo lo demás es un CRD que
vive en git** y que Argo CD sincroniza.

| Capa | Chart · app | Namespace | CRDs que usa este repo |
|---|---|---|---|
| **Argo CD** | 10.1.3 · v3.4.5 | `argocd` | `Application` ×3, **`ApplicationSet`**, **Notifications** |
| **Argo Events** | 2.4.23 · v1.9.11 | `argo-events` | `EventBus`, `EventSource`, `Sensor` ×3 |
| **Argo Workflows** | 1.0.19 · v4.0.7 | `argo` | `WorkflowTemplate` ×7, **`CronWorkflow` ×3** |
| **Argo Rollouts** | 2.41.0 · v1.9.0 | `worldcup` | `Rollout` ×2 (canary + **blue-green**), `AnalysisTemplate`, **`Experiment`** |

### Argo CD — el que reconcilia

Patrón **app-of-apps**: `bootstrap/root-app.yaml` apunta a `gitops/root/`, que contiene dos
Applications más — `platform` (el cableado de Events, los WorkflowTemplates, el
AnalysisTemplate, el RBAC) y `apps` (los 3 servicios). Las dos con `automated: {prune, selfHeal}`
y `ServerSideApply`.

Y un **`ApplicationSet`**, que es donde la cosa se pone interesante: su generador `git`
directory vigila `gitops/groups/*`, directorios que escribe el Workflow `sync-groups` desde la
clasificación en vivo. La estructura del torneo genera la topología de despliegue — 12
Applications, ninguna escrita a mano. Ojo: el chart instala el CRD `applicationsets` **aunque el
controlador esté desactivado**, así que un ApplicationSet se aplica "con éxito" y no hace
absolutamente nada. Por eso `applicationSet.enabled` y `notifications.enabled` están ahora a
`true` en `bootstrap/values/argocd.yaml` (medido: el nodo asigna ~7.9Gi y el stack pide ~1.7Gi).

El `selfHeal` es el que da los dientes al sistema: un `kubectl set env` a mano se revierte solo.
Por eso el rollback **no** parchea el `spec` del Rollout — pelearía con Argo CD — sino que hace
`git revert`, que es el único canal legítimo de cambio.

### Argo Events — el que escucha

Cadena de tres piezas, ordenadas con `sync-wave` (`-3` bus → `-1` source → `0` sensors) porque
un Sensor no arranca si su bus no está listo:

- **`EventBus`** (`jetstream`, 1 réplica) — la cola. `version` fijada y `replicas: 1` **también**
  dentro del `streamConfig`, que es un ajuste distinto al del bus.
- **`EventSource`** (`webhook`, `:12000/match`) — el controller le crea un Service
  `match-eventsource-svc`; ahí POSTea `match-watcher`. El body HTTP aterriza en `data.body.*`.
- **`Sensor` ×3** — `on-goal` y `on-anomaly`, cada uno filtrando `body.type` con un regex
  anclado (`^GOAL$`). El trigger es `argoWorkflow: submit`, y mapea campos del payload
  (`body.score`, `body.revision`, …) a los parámetros del Workflow con `dest:
  spec.arguments.parameters.N.value`.
- **`on-signals`** — un solo Sensor con **tres dependencias** y `conditions` por trigger, para
  que cada señal despierte solo lo suyo. Enseña además un **segundo tipo de trigger**: el
  penalti usa `k8s: create` (el Sensor crea el recurso directamente) en vez de `argoWorkflow`.

### Argo Workflows — el que actúa

Tres `WorkflowTemplate`, cada uno con su SA propia (`workflow-sa`, con Roles separados para
ejecutar, abortar Rollouts y escribir el ConfigMap):

- **`promote-score`** — un `sed` en un `alpine/git` reescribe las entradas `# promote-field:`
  del Rollout y hace push. Los parámetros llegan como **env vars**, no interpolados en la línea
  de comando, para que un nombre de equipo con comillas no rompa el shell. `retryStrategy`
  con backoff, porque dos goles seguidos compiten por el mismo push.
- **`rollback-score`** — dos pasos: `kubectl patch` de `status.abort` (imagen `alpine/k8s`,
  que trae shell *y* kubectl) y `git revert` del último commit `promote:`.
- **`set-match`** — cambia de partido (y por tanto de países) reescribiendo `MODE`/`MATCH_ID`
  en git. Valida `mode` y el rango `1..104` **antes** de commitear.
- **`backfill-history`** — un **DAG** de demostración: fetch → fan-out sobre los 104 partidos
  con `withParam` y `parallelism: 6` → reduce.
- **`match-signals`** — deriva penalti/descanso/final del feed y los emite al webhook. La
  clasificación vive **aquí, en un manifiesto**, no en Go: añadir una señal no reconstruye
  ninguna imagen.
- **`halftime-gate`** — nodo **`suspend`**: el pipeline se para en el descanso y espera que un
  humano pulse Resume en la UI. Es la única capacidad que no se puede fingir con capturas.
- **`sync-groups`** — escribe `gitops/groups/A..L/` desde `/get/groups`. Valida el payload
  **antes** de clonar: un 429 commitearía una clasificación vacía sobre los datos buenos.
- **`tick-minute`** — **el reloj del partido**. La API manda `time_elapsed: "live"` durante el
  juego (una palabra, no un número), así que el minuto sencillamente no está en la fuente. En
  vez de calcularlo dentro de un servicio, un `CronWorkflow` dispara una vez por minuto e
  incrementa un contador en el ConfigMap `match-clock`: **la programación *es* el mecanismo**.
  El scoreboard lo lee y nunca lo calcula. Resetea al cambiar de partido o si no está `live`.
  Y se **ancla a datos reales**: los `scorers` sí traen el minuto de cada gol (`"Azri Knsa
  18'"`), así que el último gol actúa de suelo y cada gol re-sincroniza el reloj. Sin ese
  ancla, un contador que arranca tarde se queda tarde el resto del partido.
- **`fulltime-archive`** — archiva el resultado en `results/` y demuestra un **`onExit`**
  (exit handler), que corre tanto si el archivado va bien como si falla.

Tres **`CronWorkflow`** de 1 minuto (el suelo de la primitiva) mueven el sondeo lento —y el
reloj del partido— a infraestructura Argo. Los goles **no** pasan por ahí: siguen en el bucle de 30s de
`match-watcher`, porque un gol que dispara un canary no puede esperar un minuto, y cada tick de
`CronWorkflow` cuesta un pod.

Nada cambia el estado del clúster a mano. `kubectl set env` sobre el feed *parece* funcionar
—el Deployment rueda— y el `selfHeal` lo revierte al valor de git en la siguiente
reconciliación, sirviendo el partido viejo sin un solo error visible. Por eso hasta cambiar de
partido es un commit.

### Argo Rollouts — el que arriesga

El `Rollout` del scoreboard con estrategia **canary** y 3 Services: `scoreboard` (sin
pod-template-hash, abarca ambos ReplicaSets — es el que muestrea la UI) más
`scoreboard-stable` / `scoreboard-canary`, a los que el controller sí inyecta el hash.

`replicas: 5` no es arbitrario: sin traffic router el peso se cuantiza a `1/réplicas`, así que
`setWeight: 20` cae en exactamente un pod. La escalera es
`20% → pause → analysis → 50% → pause → 100%`.

Al lado vive un segundo Rollout, **`standings`**, con estrategia **blue-green** y la misma
imagen (el objeto de la demo es la estrategia, no el workload). El contraste es el que se
explica solo en una presentación: el canary reparte tráfico gradualmente entre dos ReplicaSets;
el blue-green levanta la versión nueva entera en el Service `preview` y **espera** —
`autoPromotionEnabled: false`— hasta que alguien la promociona, y entonces el 100% del tráfico
salta de golpe. Se pueden abrir `active` y `preview` en dos pestañas y verlos distintos.

Un penalti crea además un **`Experiment`**: un ReplicaSet efímero que corre 5 minutos junto al
estable sin tocar el Rollout.

El **`AnalysisTemplate`** es la puerta del rollback automático: provider `web` contra
`/consistency` del Service canary, `jsonPath {$.consistent}` → `successCondition: result == true`.
El pod compara su score horneado contra el feed en vivo, así que un score inflado o regresado
falla y Rollouts aborta solo. `initialDelay: 15s` + `failureLimit: 1` son imprescindibles: a t=0
el Service canary aún no tiene endpoints y el default abortaría en el camino feliz.

## Puesta en marcha

```bash
make image-local SVC=match-feed     # build + inyecta en el nodo (sin GHCR, sin scopes)
make image-local SVC=match-watcher
make image-local SVC=scoreboard
make bootstrap                      # instala las 4 capas + la Application raíz (app-of-apps)
make ui                             # port-forwards: Argo CD, Workflows, Rollouts, marcador
```

`buildx` en Docker Desktop escribe en el **containerd que comparte el nodo de Kubernetes**, y
los manifiestos usan `imagePullPolicy: IfNotPresent` a propósito: todo corre con imágenes
locales, sin publicar nada. `make images` (push a GHCR) solo hace falta para **portar** a otro
clúster, y requiere `write:packages` en el token de gh
(`gh auth refresh -h github.com -s write:packages`). Ojo: `docker build` a secas **no** es
visible para el nodo — solo `buildx`.

## Demos

```bash
make backfill                 # DAG que abanica sobre los 104 partidos
make demo-replay MATCH=64     # revive un partido jugado, gol a gol (a cualquier hora)
make set-match MODE=live MATCH=103   # cambia de partido/países vía commit (no kubectl)
make demo-goal                # fuerza un gol → canary
make demo-chaos               # corrompe el feed → rollback (abort + git revert)
make demo-live MATCH=101      # Francia–España (semi) · MATCH=104 = la final
make rollout                  # sigue la marcha del canary paso a paso
make status                   # salud de todas las capas (+ cm/match-state)
```

## El marcador

`http://localhost:8090` no es un adorno: es la evidencia visual de las dos mitades del sistema.

- **Fila superior — el feed en vivo** (`/api/live`, proxy directo a `match-feed`): banderas,
  `status`, minuto y `feed revision`. Es la verdad *antes* de pasar por git.
- **`deployed revision`** — la revisión realmente horneada en los pods, muestreada vía
  `/api/whoami`. El desfase entre ambas es exactamente la latencia del pipeline GitOps.
- **Barra de tráfico** — muestrea 24 requests cada 1.5 s y colorea por revisión: durante un
  canary se ve el split 20/80 → 50/50 → 100 en tiempo real.
- **Clasificación por grupos** (`/api/standings`) — cierra el círculo que abre el
  ApplicationSet: API → Workflow → git → 12 Applications → ConfigMaps → pantalla. El pod las
  lee con el token proyectado de su SA (sin client-go, la imagen sigue distroless) y cachea
  15 s, porque la página sondea cada 1.5 s desde 5 pods.
- Un gol dispara flash + mensaje; cambiar de partido (`demo-live`, `demo-replay`) lo anuncia solo.

## Diseño

`src/` es solo código de app (3 servicios Go, imágenes distroless ~10 Mi). `gitops/` es lo único
que Argo CD sincroniza; `bootstrap/` instala los controladores.

Dos detalles de resiliencia que no se ven en el diagrama:

- El watcher **solo avanza su estado previo si el emit devolvió 2xx**. Si Argo Events está caído
  un instante, el gol se re-detecta y se re-emite en el siguiente poll en vez de perderse.
- `cm/match-state` es telemetría de runtime escrita por el watcher; la Application la gestiona
  pero **ignora el drift de `/data`** (`ignoreDifferences`), o el selfHeal pelearía con cada poll.

## Estado de verificación

Verificado en el clúster, no solo validado:

| Comprobación | Resultado |
|---|---|
| `make demo-live MATCH=103` cambia el feed | ✅ `MATCH_ID=103`, sirve `France 0-0 England`, `source: live` |
| `CronWorkflow` tickea y commitea | ✅ `standings synced` → `gitops/groups/A..L/` en git |
| ApplicationSet genera las Applications | ✅ 12/12 `Synced` + `Healthy`, 12 ConfigMaps |
| Cadena de señales completa | ✅ `match-signals` → Sensor → `fulltime-archive` → commit `result:` |
| Rollout blue-green desplegado | ✅ `standings` 2/2 |
| Clasificación en pantalla | ✅ `/api/standings` sirve los 12 grupos con nombres y puntos |
| Gol REAL del Mundial extremo a extremo | ✅ `France 0-1 England` → watcher → Events → `promote-score` → commit → Argo CD → canary |
| Reloj por CronWorkflow | ✅ `clock: match 103 status=live minute=1`, leído por la web |
| Rollout tras cambio de partido | ✅ `Healthy`, `/consistency` → `consistent: true` |

Los `CronWorkflow` corren cada minuto, así que el repo acumula commits `standings:` y `result:`
por sí solo — son la evidencia durable de que el sistema está vivo.

Las decisiones no obvias y las minas del stack (filtro `body.type`, `jetstream.version` fijada,
`replicas: 1` en el `streamConfig` además de en el bus, imagen `alpine/k8s` para el abort,
`rollbackWindow`, cuantización del peso del canary a `1/réplicas`, RBAC de
`workflowtaskresults`) están documentadas en cada manifiesto y resumidas en
[AGENTS.md](AGENTS.md) — léelo antes de tocar infra o el flujo de eventos.
