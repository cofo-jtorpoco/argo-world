// scoreboard is the "hello world with dignity": a full-screen live match scoreboard
// whose score is baked into the pod via env (MATCH_SCORE, MATCH_REVISION), set by the
// promote-score Workflow through a git commit. Because each Rollout revision bakes a
// different score, the page can poll /api/whoami across pods and render the canary
// traffic split live — 20% -> 50% -> 100%, or a snap back to 0% on rollback.
//
// /consistency is the source of truth for the automatic rollback: the pod compares its
// own baked score against the live feed and returns {"consistent": bool}. The Rollouts
// web-provider AnalysisTemplate reads $.consistent; false aborts the canary.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	matchScore = env("MATCH_SCORE", "0-0")
	matchRev   = envInt("MATCH_REVISION", 0)
	home       = env("MATCH_HOME", "Home")
	away       = env("MATCH_AWAY", "Away")
	minute     = envInt("MATCH_MINUTE", 0)
	podName    = env("POD_NAME", "local")
	feedURL    = env("MATCH_FEED_URL", "http://match-feed.worldcup.svc.cluster.local:8080/match/current")
	listenAddr = env("LISTEN_ADDR", ":8080")
	httpc      = &http.Client{Timeout: 6 * time.Second}
)

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/whoami", handleWhoami)
	http.HandleFunc("/consistency", handleConsistency)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	log.Printf("scoreboard up: %s %s-? rev=%d pod=%s", scoreLabel(), matchScore, matchRev, podName)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"pod":      podName,
		"revision": matchRev,
		"score":    matchScore,
		"home":     home,
		"away":     away,
		"minute":   minute,
	})
}

// handleConsistency compares the baked score against the live source. A legit promote
// bakes exactly the live score, so it passes; an inflated, negative, or regressed score
// (from chaos or a bad manual deploy) exceeds or falls below live and fails, which makes
// the AnalysisRun fail and Rollouts auto-abort the canary.
func handleConsistency(w http.ResponseWriter, r *http.Request) {
	bh, ba, ok := parseScore(matchScore)
	res := map[string]any{
		"pod": podName, "baked_score": matchScore, "baked_revision": matchRev,
	}
	if !ok || bh < 0 || ba < 0 {
		res["consistent"] = false
		res["reason"] = "baked score malformed or negative"
		writeJSON(w, res)
		return
	}
	live, err := fetchLive()
	if err != nil {
		// Fail closed: if we can't confirm against the source, we can't promote.
		res["consistent"] = false
		res["reason"] = "live source unreachable: " + err.Error()
		writeJSON(w, res)
		return
	}
	res["live_score"] = fmt.Sprintf("%d-%d", live.HomeScore, live.AwayScore)
	res["live_revision"] = live.Revision
	consistent := bh <= live.HomeScore && ba <= live.AwayScore && matchRev <= live.Revision
	res["consistent"] = consistent
	if !consistent {
		res["reason"] = "baked score exceeds or diverges from live truth"
	}
	writeJSON(w, res)
}

type liveMatch struct {
	HomeScore int `json:"home_score"`
	AwayScore int `json:"away_score"`
	Revision  int `json:"revision"`
}

func fetchLive() (*liveMatch, error) {
	resp, err := httpc.Get(feedURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed %d", resp.StatusCode)
	}
	var m liveMatch
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

func parseScore(s string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	a, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return h, a, true
}

func scoreLabel() string { return home + " vs " + away }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// page polls /api/whoami through the root Service (which spans stable + canary pods),
// tallies the score each pod reports, and renders the live traffic split as a bar. The
// split IS the canary weight: new-revision pods are the canary.
const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Argo World · Live Scoreboard</title>
<style>
  :root{color-scheme:dark}
  *{box-sizing:border-box;margin:0;padding:0}
  body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0a0e14;color:#e6edf3;
       min-height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:2rem;padding:2rem}
  .card{background:#12181f;border:1px solid #222c38;border-radius:20px;padding:2.5rem 3.5rem;text-align:center;
        box-shadow:0 20px 60px rgba(0,0,0,.4);min-width:min(560px,92vw)}
  .tag{font-size:.72rem;letter-spacing:.18em;text-transform:uppercase;color:#7d8896;margin-bottom:1.2rem}
  .teams{display:flex;align-items:center;justify-content:center;gap:1.4rem;font-weight:700}
  .team{font-size:1.5rem;color:#c9d4e0;min-width:8rem}
  .score{font-size:4.5rem;font-variant-numeric:tabular-nums;line-height:1;color:#fff}
  .dash{font-size:2.4rem;color:#4a5666}
  .min{margin-top:1rem;color:#7d8896;font-size:.95rem}
  .min b{color:#3fb950}
  .split{width:min(720px,92vw)}
  .split h2{font-size:.72rem;letter-spacing:.18em;text-transform:uppercase;color:#7d8896;margin-bottom:.8rem;text-align:center}
  .bar{display:flex;height:44px;border-radius:12px;overflow:hidden;border:1px solid #222c38}
  .seg{display:flex;align-items:center;justify-content:center;font-size:.85rem;font-weight:700;color:#0a0e14;
       transition:width .6s ease;white-space:nowrap;overflow:hidden}
  .legend{display:flex;flex-wrap:wrap;gap:.6rem 1.4rem;justify-content:center;margin-top:.9rem;font-size:.82rem;color:#9aa7b4}
  .legend span{display:inline-flex;align-items:center;gap:.45rem}
  .dot{width:.7rem;height:.7rem;border-radius:3px}
  .meta{color:#5a6674;font-size:.78rem;margin-top:.4rem}
</style></head>
<body>
  <div class="card">
    <div class="tag">Argo World · FIFA World Cup 2026</div>
    <div class="teams">
      <span class="team" id="home">—</span>
      <span class="score" id="hs">0</span><span class="dash">:</span><span class="score" id="as">0</span>
      <span class="team" id="away">—</span>
    </div>
    <div class="min">minute <b id="min">0'</b> · scoreboard revision <b id="rev">0</b></div>
  </div>

  <div class="split">
    <h2>Canary traffic split · live from /api/whoami</h2>
    <div class="bar" id="bar"></div>
    <div class="legend" id="legend"></div>
    <div class="meta" id="meta">sampling…</div>
  </div>

<script>
const palette=["#3fb950","#58a6ff","#d29922","#bc8cff","#f85149","#39c5cf"];
async function sample(){
  const counts={}, meta={};
  const N=24;
  const reqs=[];
  for(let i=0;i<N;i++) reqs.push(fetch('/api/whoami',{cache:'no-store'}).then(r=>r.json()).catch(()=>null));
  const rows=(await Promise.all(reqs)).filter(Boolean);
  let latest=null;
  for(const r of rows){
    const key=r.revision+" · "+r.score;
    counts[key]=(counts[key]||0)+1;
    meta[key]=r;
    if(!latest||r.revision>=latest.revision) latest=r;
  }
  if(latest){
    document.getElementById('home').textContent=latest.home||'Home';
    document.getElementById('away').textContent=latest.away||'Away';
    const sc=(latest.score||'0-0').split('-');
    document.getElementById('hs').textContent=sc[0]||'0';
    document.getElementById('as').textContent=sc[1]||'0';
    document.getElementById('min').textContent=(latest.minute||0)+"'";
    document.getElementById('rev').textContent=latest.revision;
  }
  const keys=Object.keys(counts).sort((a,b)=>meta[a].revision-meta[b].revision);
  const total=rows.length||1;
  const bar=document.getElementById('bar'); bar.innerHTML='';
  const legend=document.getElementById('legend'); legend.innerHTML='';
  keys.forEach((k,i)=>{
    const pct=Math.round(counts[k]/total*100);
    const c=palette[i%palette.length];
    const seg=document.createElement('div');
    seg.className='seg'; seg.style.width=pct+'%'; seg.style.background=c;
    seg.textContent=pct>=8?pct+'%':'';
    bar.appendChild(seg);
    const lg=document.createElement('span');
    lg.innerHTML='<span class="dot" style="background:'+c+'"></span>rev '+meta[k].revision+' ('+meta[k].score+') · '+pct+'%';
    legend.appendChild(lg);
  });
  document.getElementById('meta').textContent=keys.length>1
    ? 'canary in progress · '+keys.length+' revisions serving traffic'
    : 'stable · single revision';
}
sample(); setInterval(sample,1500);
</script>
</body></html>`
