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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
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
	http.HandleFunc("/api/live", handleLive)
	http.HandleFunc("/api/standings", handleStandings)
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

// --- standings -------------------------------------------------------------------
//
// The sync-groups Workflow writes one standings-<g> ConfigMap per group into git; the
// ApplicationSet's 12 Applications sync them into the cluster. This reads them back so the
// live table actually reaches the screen, closing the loop API -> git -> Argo CD -> UI.
//
// Talks to the API server directly over the projected ServiceAccount token instead of
// pulling in client-go: it keeps go.mod empty and the image distroless.

type groupTable struct {
	Group string           `json:"group"`
	Teams []map[string]any `json:"teams"`
}

var (
	k8sNS    = env("POD_NAMESPACE", "worldcup")
	stMu     sync.Mutex
	stCache  []groupTable
	stExpiry time.Time
)

// standings caches for 15s: the page polls every 1.5s across N pods, and standings only
// move when a match ends — without the cache this would hammer the API server.
func standings() ([]groupTable, error) {
	stMu.Lock()
	defer stMu.Unlock()
	if time.Now().Before(stExpiry) && stCache != nil {
		return stCache, nil
	}

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in-cluster")
	}
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(sa + "/token")
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	ca, err := os.ReadFile(sa + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("bad ca bundle")
	}

	endpoint := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/configmaps?labelSelector=%s",
		host, port, url.PathEscape(k8sNS), url.QueryEscape("app=standings"))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	cl := &http.Client{
		Timeout:   6 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("api server %d: %s", resp.StatusCode, body)
	}

	var list struct {
		Items []struct {
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}

	out := make([]groupTable, 0, len(list.Items))
	for _, it := range list.Items {
		g := it.Data["group"]
		if g == "" {
			continue
		}
		var teams []map[string]any
		// A malformed table must not sink the whole board: skip that group, keep the rest.
		if err := json.Unmarshal([]byte(it.Data["table"]), &teams); err != nil {
			log.Printf("standings: group %s has an unparseable table: %v", g, err)
			continue
		}
		out = append(out, groupTable{Group: g, Teams: teams})
	}
	sortGroups(out)

	stCache, stExpiry = out, time.Now().Add(15*time.Second)
	return out, nil
}

func sortGroups(g []groupTable) {
	for i := 1; i < len(g); i++ {
		for j := i; j > 0 && g[j].Group < g[j-1].Group; j-- {
			g[j], g[j-1] = g[j-1], g[j]
		}
	}
}

func handleStandings(w http.ResponseWriter, r *http.Request) {
	st, err := standings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, st)
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	live, err := fetchLive()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, live)
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
	MatchID   string `json:"match_id"`
	Home      string `json:"home"`
	Away      string `json:"away"`
	HomeScore int    `json:"home_score"`
	AwayScore int    `json:"away_score"`
	Minute    int    `json:"minute"`
	Revision  int    `json:"revision"`
	Status    string `json:"status"`
	Source    string `json:"source"`
	TS        string `json:"ts"`
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
	:root{
		color-scheme:dark;
		--bg0:#05070b;
		--bg1:#0e1420;
		--panel:#11192aee;
		--line:#25324a;
		--text:#e8edf7;
		--muted:#93a4bf;
		--gold:#f7cf5f;
		--mint:#5cf2aa;
	}
	*{box-sizing:border-box;margin:0;padding:0}
	body{
		font-family:system-ui,-apple-system,"Segoe UI",sans-serif;
		background:
			radial-gradient(1200px 700px at 8% -5%, #22335c66 0%, transparent 58%),
			radial-gradient(1200px 700px at 88% 5%, #33604c66 0%, transparent 55%),
			linear-gradient(180deg,var(--bg1),var(--bg0));
		color:var(--text);
		min-height:100vh;
		display:flex;
		flex-direction:column;
		align-items:center;
		justify-content:center;
		gap:1.1rem;
		padding:1rem;
		overflow:hidden;
	}
	.goalflash{
		position:fixed;
		inset:0;
		background:radial-gradient(circle at 50% 50%, #fff2 0%, #fff0 60%);
		opacity:0;
		pointer-events:none;
		z-index:20;
	}
	.goalflash.boom{animation:boom .8s ease-out}
	@keyframes boom{0%{opacity:0}20%{opacity:.95}100%{opacity:0}}

	.switcher{
		position:fixed;
		top:1.1rem;
		left:50%;
		transform:translateX(-50%) translateY(-16px);
		background:#0f1a2edc;
		border:1px solid #2a3a59;
		color:#d7e4ff;
		border-radius:999px;
		padding:.48rem .9rem;
		font-size:.82rem;
		opacity:0;
		z-index:21;
	}
	.switcher.on{animation:switchIn 2.6s ease}
	@keyframes switchIn{0%{opacity:0;transform:translateX(-50%) translateY(-18px)}10%{opacity:1;transform:translateX(-50%) translateY(0)}85%{opacity:1}100%{opacity:0}}

	.card{
		width:min(980px,98vw);
		background:var(--panel);
		border:1px solid var(--line);
		border-radius:24px;
		padding:1.3rem 1.2rem 1.1rem;
		box-shadow:0 16px 56px #0008;
		backdrop-filter:blur(8px);
		position:relative;
	}
	.tag{font-size:.72rem;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin-bottom:.7rem;text-align:center}
	.teams{display:grid;grid-template-columns:1fr auto 1fr;gap:.8rem;align-items:center}
	.side{display:flex;align-items:center;gap:.52rem;min-width:0}
	.side.right{justify-content:flex-end}
	.flag{font-size:2.1rem;filter:drop-shadow(0 2px 8px #0008)}
	.team{font-size:clamp(1.2rem,2.9vw,2rem);font-weight:750;letter-spacing:.01em;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
	.scoreboard{display:flex;align-items:flex-end;gap:.45rem}
	.score{font-size:clamp(3rem,9vw,5.9rem);font-variant-numeric:tabular-nums;line-height:.92;color:#fff}
	.dash{font-size:clamp(2rem,5.5vw,3.3rem);color:#6f7f99;padding-bottom:.58rem}
	.livebar{margin-top:.8rem;display:flex;justify-content:center;gap:.7rem;align-items:center;flex-wrap:wrap;color:var(--muted);font-size:.92rem}
	.pill{padding:.26rem .56rem;border:1px solid #31425f;border-radius:999px;background:#0b1322b8}
	.pill b{color:var(--mint)}
	.meme{margin-top:.65rem;text-align:center;font-size:.95rem;color:#d9e9ff;min-height:1.4rem;letter-spacing:.01em}
	.meme.goal{color:var(--gold);animation:pulse .62s ease}
	@keyframes pulse{0%{transform:scale(1)}45%{transform:scale(1.07)}100%{transform:scale(1)}}

	.split{width:min(980px,98vw);background:#0a1220cc;border:1px solid #263654;border-radius:18px;padding:.9rem 1rem .92rem}
	.split h2{font-size:.72rem;letter-spacing:.17em;text-transform:uppercase;color:var(--muted);margin-bottom:.72rem;text-align:center}
	.bar{display:flex;height:40px;border-radius:11px;overflow:hidden;border:1px solid #2a3a5a;background:#0f1627}
	.seg{display:flex;align-items:center;justify-content:center;font-size:.83rem;font-weight:740;color:#0d1118;transition:width .55s ease;white-space:nowrap;overflow:hidden}
	.legend{display:flex;flex-wrap:wrap;gap:.45rem 1rem;justify-content:center;margin-top:.72rem;font-size:.81rem;color:#a7b4c9}
	.legend span{display:inline-flex;align-items:center;gap:.4rem}
	.dot{width:.66rem;height:.66rem;border-radius:3px}
	.meta{color:#7f90ad;font-size:.78rem;margin-top:.44rem;text-align:center}

	.groups{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:.6rem}
	.grp{background:#0d1526cc;border:1px solid #24344f;border-radius:12px;padding:.55rem .6rem}
	.grp h3{font-size:.72rem;letter-spacing:.15em;text-transform:uppercase;color:var(--gold);margin-bottom:.4rem}
	.grp table{width:100%;border-collapse:collapse;font-size:.8rem}
	.grp td{padding:.16rem .2rem;color:#c6d3e6}
	.grp td.pts{text-align:right;font-variant-numeric:tabular-nums;color:var(--mint);font-weight:700}
	.grp td.tm{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:9.5rem}
	.grp tr+tr td{border-top:1px solid #1b2740}

	@media (max-width:720px){
		.teams{grid-template-columns:1fr;gap:.18rem}
		.side,.side.right{justify-content:center}
		.scoreboard{justify-content:center}
		.tag{margin-bottom:.46rem}
	}
</style></head>
<body>
	<div class="goalflash" id="goalflash"></div>
	<div class="switcher" id="switcher"></div>

	<div class="card">
		<div class="tag">Argo World · Match Feed + Canary Runtime</div>
		<div class="teams">
			<div class="side"><span class="flag" id="hflag">🏳️</span><span class="team" id="home">Home</span></div>
			<div class="scoreboard"><span class="score" id="hs">0</span><span class="dash">:</span><span class="score" id="as">0</span></div>
			<div class="side right"><span class="team" id="away">Away</span><span class="flag" id="aflag">🏳️</span></div>
		</div>
		<div class="livebar">
			<span class="pill">status <b id="status">init</b></span>
			<span class="pill">minute <b id="min">0'</b></span>
			<span class="pill">feed revision <b id="feedrev">0</b></span>
			<span class="pill">deployed revision <b id="rev">0</b></span>
		</div>
		<div class="meme" id="meme">warming up...</div>
	</div>

	<div class="split" id="standingsPanel" hidden>
		<h2>Group standings · API → Workflow → git → ApplicationSet → 12 Applications</h2>
		<div class="groups" id="groups"></div>
		<div class="meta" id="stmeta"></div>
	</div>

	<div class="split">
		<h2>Canary traffic split · sampled from /api/whoami</h2>
		<div class="bar" id="bar"></div>
		<div class="legend" id="legend"></div>
		<div class="meta" id="meta">sampling...</div>
	</div>

<script>
const palette=['#5cf2aa','#63b3ff','#f7cf5f','#f08dff','#ff6f61','#6ce4da'];
const goalMemes=[
	'GOAL! The canary just did a backflip.',
	'Goal detected. Pipelines celebrating quietly.',
	'Net shaken. Git prepared. Rollout watching.',
	'That was clean. Even Argo CD smiled.',
	'Live football meets declarative destiny.'
];
const steadyMemes=[
	'Platform calm. Signals clean. Traffic steady.',
	'No drama. Just healthy reconciliation.',
	'Everything green. Coffee approved.',
	'Cluster breathing normally.',
	'Still stable. Canary waiting for action.'
];

const flags={
	'Argentina':'🇦🇷','Australia':'🇦🇺','Austria':'🇦🇹','Belgium':'🇧🇪','Bosnia and Herzegovina':'🇧🇦','Brazil':'🇧🇷',
	'Canada':'🇨🇦','Cape Verde':'🇨🇻','Colombia':'🇨🇴','Croatia':'🇭🇷','Democratic Republic of the Congo':'🇨🇩',
	'Egypt':'🇪🇬','England':'🏴','France':'🇫🇷','Germany':'🇩🇪','Ghana':'🇬🇭','Haiti':'🇭🇹','Iran':'🇮🇷','Iraq':'🇮🇶',
	'Ivory Coast':'🇨🇮','Japan':'🇯🇵','Jordan':'🇯🇴','Mexico':'🇲🇽','Morocco':'🇲🇦','Netherlands':'🇳🇱','New Zealand':'🇳🇿',
	'Norway':'🇳🇴','Panama':'🇵🇦','Paraguay':'🇵🇾','Portugal':'🇵🇹','Qatar':'🇶🇦','Saudi Arabia':'🇸🇦','Scotland':'🏴',
	'Senegal':'🇸🇳','South Africa':'🇿🇦','South Korea':'🇰🇷','Spain':'🇪🇸','Sweden':'🇸🇪','Switzerland':'🇨🇭',
	'Tunisia':'🇹🇳','Turkey':'🇹🇷','United States':'🇺🇸','Uruguay':'🇺🇾','Algeria':'🇩🇿'
};

const state={matchKey:'',total:-1,memeTick:0};

function flagFor(name){ return flags[name] || '🏳️'; }
function meme(arr){ return arr[Math.floor(Math.random()*arr.length)]; }
function parseIntSafe(v){ const n=parseInt(v,10); return Number.isFinite(n)?n:0; }

function showSwitcher(msg){
	const el=document.getElementById('switcher');
	el.textContent=msg;
	el.classList.remove('on');
	void el.offsetWidth;
	el.classList.add('on');
}

function boomGoal(){
	const flash=document.getElementById('goalflash');
	const memeEl=document.getElementById('meme');
	flash.classList.remove('boom');
	void flash.offsetWidth;
	flash.classList.add('boom');
	memeEl.textContent=meme(goalMemes);
	memeEl.classList.remove('goal');
	void memeEl.offsetWidth;
	memeEl.classList.add('goal');
}

// Standings come from ConfigMaps the ApplicationSet syncs, so they move only when a match
// ends — poll far slower than the traffic split, and never let a failure blank the board.
async function loadStandings(){
	try{
		const res=await fetch('/api/standings',{cache:'no-store'});
		if(!res.ok) return;
		const groups=await res.json();
		if(!Array.isArray(groups)||!groups.length) return;
		const box=document.getElementById('groups');
		box.innerHTML='';
		let teams=0;
		for(const g of groups){
			const rows=(g.teams||[]).map(t=>
				'<tr><td class="tm">'+(t.code||'')+' '+(t.name||'')+'</td>'+
				'<td class="pts">'+(t.pts||'0')+'</td></tr>').join('');
			teams+=(g.teams||[]).length;
			const el=document.createElement('div');
			el.className='grp';
			el.innerHTML='<h3>Group '+g.group+'</h3><table>'+rows+'</table>';
			box.appendChild(el);
		}
		document.getElementById('stmeta').textContent=
			groups.length+' groups · '+teams+' teams · one Argo CD Application per group';
		document.getElementById('standingsPanel').hidden=false;
	}catch(e){ /* keep the last good board */ }
}

async function sample(){
	const counts={},meta={};
	const N=24;
	const reqs=[];
	for(let i=0;i<N;i++) reqs.push(fetch('/api/whoami',{cache:'no-store'}).then(r=>r.json()).catch(()=>null));
	const [live,rowsRaw]=await Promise.all([
		fetch('/api/live',{cache:'no-store'}).then(r=>r.ok?r.json():null).catch(()=>null),
		Promise.all(reqs)
	]);
	const rows=rowsRaw.filter(Boolean);

	let latest=null;
	for(const r of rows){
		const key=r.revision+' · '+r.score;
		counts[key]=(counts[key]||0)+1;
		meta[key]=r;
		if(!latest||r.revision>=latest.revision) latest=r;
	}

	if(live){
		const home=live.home||'Home';
		const away=live.away||'Away';
		const hs=parseIntSafe(live.home_score);
		const as=parseIntSafe(live.away_score);
		const total=hs+as;
		const key=(live.match_id||'?')+'|'+home+'|'+away;

		document.getElementById('home').textContent=home;
		document.getElementById('away').textContent=away;
		document.getElementById('hflag').textContent=flagFor(home);
		document.getElementById('aflag').textContent=flagFor(away);
		document.getElementById('hs').textContent=hs;
		document.getElementById('as').textContent=as;
		document.getElementById('min').textContent=parseIntSafe(live.minute)+"'";
		document.getElementById('feedrev').textContent=parseIntSafe(live.revision);
		document.getElementById('status').textContent=live.status||'unknown';

		if(state.matchKey && state.matchKey!==key){
			showSwitcher('Now tracking: '+home+' vs '+away);
			document.getElementById('meme').textContent='Match switch complete. New timeline loaded.';
		}
		if(state.total>=0 && total>state.total){
			boomGoal();
		} else if((state.memeTick++%10)===0){
			const memeEl=document.getElementById('meme');
			memeEl.textContent=meme(steadyMemes);
			memeEl.classList.remove('goal');
		}
		state.matchKey=key;
		state.total=total;
	}

	if(latest){
		document.getElementById('rev').textContent=latest.revision;
	}

	const keys=Object.keys(counts).sort((a,b)=>meta[a].revision-meta[b].revision);
	const totalRows=rows.length||1;
	const bar=document.getElementById('bar'); bar.innerHTML='';
	const legend=document.getElementById('legend'); legend.innerHTML='';
	keys.forEach((k,i)=>{
		const pct=Math.round(counts[k]/totalRows*100);
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
loadStandings(); setInterval(loadStandings,20000);
</script>
</body></html>`
