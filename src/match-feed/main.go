// match-feed wraps the public worldcup26.ir API and exposes a single normalized
// /match/current endpoint. It smooths over the source's dirty data (string scores,
// the literal string "null", TRUE/FALSE, smart-quoted scorer lists) and can run in
// two modes: live (poll the real API) or replay (revive a finished match minute by
// minute so the demo works at any hour). A chaos overlay can force goals or corrupt
// the next read on demand, which is what drives the rollback path end to end.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Match is the normalized shape every other service consumes. revision is monotonic
// in the number of goals scored, so the watcher can treat a revision regression as an
// anomaly without knowing anything about football.
type Match struct {
	MatchID   string `json:"match_id"`
	Home      string `json:"home"`
	Away      string `json:"away"`
	HomeScore int    `json:"home_score"`
	AwayScore int    `json:"away_score"`
	Minute    int    `json:"minute"`
	Revision  int    `json:"revision"`
	Status    string `json:"status"` // notstarted | live | finished
	Source    string `json:"source"` // live | replay | chaos
	TS        string `json:"ts"`
}

// rawGame mirrors the worldcup26.ir /get/games element. Every field is a string in
// the source, including scores and the literal "null".
type rawGame struct {
	ID           string `json:"id"`
	HomeName     string `json:"home_team_name_en"`
	AwayName     string `json:"away_team_name_en"`
	HomeScore    string `json:"home_score"`
	AwayScore    string `json:"away_score"`
	HomeScorers  string `json:"home_scorers"`
	AwayScorers  string `json:"away_scorers"`
	Finished     string `json:"finished"`
	TimeElapsed  string `json:"time_elapsed"`
	LocalDate    string `json:"local_date"`
	Type         string `json:"type"`
}

type gamesResponse struct {
	Games []rawGame `json:"games"`
}

var (
	apiBase     = env("API_BASE", "https://worldcup26.ir")
	mode        = env("MODE", "replay")
	matchID     = env("MATCH_ID", "64")
	listenAddr  = env("LISTEN_ADDR", ":8080")
	// secondsPerMinute compresses match time: how many real seconds equal one match
	// minute in replay mode. 1s/min plays a 90' match in ~90s, spacing goals far
	// enough apart that the 30s watcher sees each one distinctly.
	secondsPerMinute = envFloat("REPLAY_SECONDS_PER_MINUTE", 1.0)

	minuteRE = regexp.MustCompile(`([0-9]{1,3})\s*['’]`)
)

// chaos is the on-demand overlay. Guarded by mu.
type chaosState struct {
	mu         sync.Mutex
	extraGoals int  // added to home score, sticky (forces GOAL events)
	corruptNext bool // corrupt the very next read (drives an ANOMALY)
}

var chaos chaosState

// replay anchors the wall-clock start so the same match always plays from 0'.
var replayStart = time.Now()

func main() {
	http.HandleFunc("/match/current", handleCurrent)
	http.HandleFunc("/chaos/goal", handleChaosGoal)
	http.HandleFunc("/chaos/anomaly", handleChaosAnomaly)
	http.HandleFunc("/chaos/reset", handleChaosReset)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	log.Printf("match-feed up: mode=%s match_id=%s api=%s listen=%s", mode, matchID, apiBase, listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func handleCurrent(w http.ResponseWriter, r *http.Request) {
	m, err := current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	applyChaos(m)
	writeJSON(w, m)
}

// current fetches the target game from the source and normalizes it. In replay mode
// it reveals only the goals that have "happened" by the current simulated minute.
func current() (*Match, error) {
	g, err := fetchGame(matchID)
	if err != nil {
		return nil, err
	}
	home := cleanTeam(g.HomeName)
	away := cleanTeam(g.AwayName)

	if mode == "replay" {
		return replayMatch(g, home, away), nil
	}

	hs := atoi(g.HomeScore)
	as := atoi(g.AwayScore)
	status := "live"
	switch {
	case strings.EqualFold(g.Finished, "TRUE"):
		status = "finished"
	case strings.EqualFold(g.TimeElapsed, "notstarted"):
		status = "notstarted"
	}
	minute := parseMinute(g.TimeElapsed)
	return &Match{
		MatchID: g.ID, Home: home, Away: away,
		HomeScore: hs, AwayScore: as, Minute: minute,
		Revision: hs + as, Status: status, Source: "live",
		TS: nowRFC(),
	}, nil
}

// replayMatch turns a finished game into a live-looking one by walking a simulated
// clock and only counting goals whose minute has already elapsed.
func replayMatch(g *rawGame, home, away string) *Match {
	elapsed := time.Since(replayStart).Seconds()
	simMinute := int(elapsed / secondsPerMinute)
	if simMinute > 90 {
		simMinute = 90
	}
	homeGoals := goalsUpTo(g.HomeScorers, simMinute)
	awayGoals := goalsUpTo(g.AwayScorers, simMinute)
	status := "live"
	if simMinute >= 90 {
		status = "finished"
	}
	return &Match{
		MatchID: g.ID, Home: home, Away: away,
		HomeScore: homeGoals, AwayScore: awayGoals, Minute: simMinute,
		Revision: homeGoals + awayGoals, Status: status, Source: "replay",
		TS: nowRFC(),
	}
}

// goalsUpTo parses a scorer string like {"J. Quiñones 9'","R. Jiménez 67'"} and
// returns how many goals were scored at or before minute cutoff. The source uses
// smart quotes and the literal "null", both handled here.
func goalsUpTo(scorers string, cutoff int) int {
	mins := goalMinutes(scorers)
	n := 0
	for _, mm := range mins {
		if mm <= cutoff {
			n++
		}
	}
	return n
}

func goalMinutes(scorers string) []int {
	s := strings.TrimSpace(scorers)
	if s == "" || strings.EqualFold(s, "null") {
		return nil
	}
	var out []int
	for _, m := range minuteRE.FindAllStringSubmatch(s, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// applyChaos overlays forced goals / corruption on a fresh read so a rollback can be
// triggered at will regardless of the underlying source.
func applyChaos(m *Match) {
	chaos.mu.Lock()
	defer chaos.mu.Unlock()
	if chaos.extraGoals > 0 {
		m.HomeScore += chaos.extraGoals
		m.Revision += chaos.extraGoals
		m.Source = "chaos"
	}
	if chaos.corruptNext {
		chaos.corruptNext = false
		// Regress the score below zero-sum reality: a decreasing total and a nulled
		// team name. The watcher classifies this as ANOMALY.
		if m.HomeScore > 0 {
			m.HomeScore--
		} else {
			m.HomeScore = -1
		}
		m.Revision--
		m.Away = ""
		m.Source = "chaos"
	}
}

func handleChaosGoal(w http.ResponseWriter, r *http.Request) {
	chaos.mu.Lock()
	chaos.extraGoals++
	n := chaos.extraGoals
	chaos.mu.Unlock()
	log.Printf("chaos: forced goal, extraGoals=%d", n)
	writeJSON(w, map[string]any{"ok": true, "extra_goals": n})
}

func handleChaosAnomaly(w http.ResponseWriter, r *http.Request) {
	chaos.mu.Lock()
	chaos.corruptNext = true
	chaos.mu.Unlock()
	log.Printf("chaos: next read will be corrupted (anomaly)")
	writeJSON(w, map[string]any{"ok": true, "corrupt_next": true})
}

func handleChaosReset(w http.ResponseWriter, r *http.Request) {
	chaos.mu.Lock()
	chaos.extraGoals = 0
	chaos.corruptNext = false
	chaos.mu.Unlock()
	replayStart = time.Now()
	log.Printf("chaos: reset, replay clock restarted")
	writeJSON(w, map[string]any{"ok": true})
}

var httpc = &http.Client{Timeout: 12 * time.Second}

func fetchGame(id string) (*rawGame, error) {
	resp, err := httpc.Get(apiBase + "/get/games")
	if err != nil {
		return nil, fmt.Errorf("fetch games: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source returned %d", resp.StatusCode)
	}
	var gr gamesResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("decode games: %w", err)
	}
	for i := range gr.Games {
		if gr.Games[i].ID == id {
			return &gr.Games[i], nil
		}
	}
	return nil, fmt.Errorf("match id %s not found in source", id)
}

// helpers

func cleanTeam(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "null") || s == "None" {
		return ""
	}
	return s
}

func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "null") {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func parseMinute(elapsed string) int {
	e := strings.TrimSpace(elapsed)
	if e == "" || strings.EqualFold(e, "notstarted") {
		return 0
	}
	if strings.EqualFold(e, "finished") {
		return 90
	}
	// live values look like "67" or "67'"
	if m := minuteRE.FindStringSubmatch(e); len(m) == 2 {
		return atoi(m[1])
	}
	return atoi(strings.TrimRight(e, "'’"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
