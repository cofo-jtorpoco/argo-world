// match-watcher is the nervous system's sensor. Every POLL_INTERVAL it reads the
// normalized match from match-feed, diffs it against the last state it saw, classifies
// the change as GOAL / ANOMALY / NO_CHANGE, and POSTs GOAL and ANOMALY events to the
// Argo Events webhook. It keeps authoritative state in memory (single replica) and
// mirrors it to the match-state ConfigMap for observability via the API server, so the
// binary stays tiny (no client-go).
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type Match struct {
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

// event is the body POSTed to the Argo Events webhook. It lands at data.body.* in the
// Sensor, so the Sensor filters on body.type and reads body.score / body.revision.
type event struct {
	Type      string `json:"type"` // GOAL | ANOMALY
	MatchID   string `json:"match_id"`
	Home      string `json:"home"`
	Away      string `json:"away"`
	Score     string `json:"score"`
	HomeScore int    `json:"home_score"`
	AwayScore int    `json:"away_score"`
	Minute    int    `json:"minute"`
	Revision  int    `json:"revision"`
	Reason    string `json:"reason"`
	TS        string `json:"ts"`
}

var (
	feedURL  = env("MATCH_FEED_URL", "http://match-feed.worldcup.svc.cluster.local:8080/match/current")
	eventURL = env("EVENTSOURCE_URL", "http://match-eventsource-svc.argo-events.svc.cluster.local:12000/match")
	interval = envDuration("POLL_INTERVAL", 30*time.Second)
	cmName   = env("STATE_CONFIGMAP", "match-state")
	httpc    = &http.Client{Timeout: 12 * time.Second}
)

func main() {
	log.Printf("match-watcher up: feed=%s event=%s interval=%s", feedURL, eventURL, interval)
	var prev *Match
	// Seed from the current reading without emitting, so a restart doesn't replay the
	// whole score as a burst of goals.
	if m, err := readMatch(); err == nil {
		prev = m
		mirror(m, "SEED")
		log.Printf("seed: %s %d-%d rev=%d", label(m), m.HomeScore, m.AwayScore, m.Revision)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for range tick.C {
		m, err := readMatch()
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		kind, reason := classify(prev, m)
		log.Printf("poll: %s %d-%d rev=%d min=%d src=%s -> %s (%s)",
			label(m), m.HomeScore, m.AwayScore, m.Revision, m.Minute, m.Source, kind, reason)
		mirror(m, kind)
		emitted := true
		switch kind {
		case "GOAL", "ANOMALY":
			emitted = emit(m, kind, reason)
		}
		// Advance prev only when there's nothing left to deliver. If a GOAL emit failed
		// (e.g. the EventSource was momentarily down), keep prev so the next poll
		// re-detects and re-emits instead of silently losing the goal. Never advance
		// past an anomaly: the corrupted read is not the new truth.
		if kind == "GOAL" && !emitted {
			log.Printf("holding prev: GOAL emit failed, will retry next poll")
		} else if kind != "ANOMALY" {
			prev = m
		}
	}
}

// classify compares the previous sane state to the new reading.
func classify(prev, m *Match) (string, string) {
	// Structural anomalies: missing fields the source should always have during play.
	if m.Home == "" || m.Away == "" {
		return "ANOMALY", "null team name"
	}
	if m.HomeScore < 0 || m.AwayScore < 0 {
		return "ANOMALY", "negative score"
	}
	if prev == nil {
		return "NO_CHANGE", "no prior state"
	}
	// A DIFFERENT MATCH is not corruption. set-match repoints the feed at another fixture,
	// whose score legitimately starts below the previous one — without this check that
	// looks exactly like a regression, so every poll fires ANOMALY, the rollback aborts
	// the Rollout, and because prev never advances past an anomaly the loop never ends.
	// Re-baseline on the new match instead.
	if prev.MatchID != m.MatchID {
		return "NO_CHANGE", fmt.Sprintf("match switched %s -> %s, re-baselining", prev.MatchID, m.MatchID)
	}
	// A regression in the total goals or the revision counter can't happen in a real
	// match — treat it as data corruption and roll back.
	if total(m) < total(prev) || m.Revision < prev.Revision {
		return "ANOMALY", fmt.Sprintf("score/revision regressed %d-%d(rev%d) -> %d-%d(rev%d)",
			prev.HomeScore, prev.AwayScore, prev.Revision, m.HomeScore, m.AwayScore, m.Revision)
	}
	if total(m) > total(prev) {
		return "GOAL", fmt.Sprintf("%d-%d -> %d-%d",
			prev.HomeScore, prev.AwayScore, m.HomeScore, m.AwayScore)
	}
	return "NO_CHANGE", "steady"
}

// emit POSTs the event to the Argo Events webhook. Returns true only on a 2xx, so the
// caller can decide whether to hold state for a retry.
func emit(m *Match, kind, reason string) bool {
	ev := event{
		Type: kind, MatchID: m.MatchID, Home: m.Home, Away: m.Away,
		Score:     fmt.Sprintf("%d-%d", m.HomeScore, m.AwayScore),
		HomeScore: m.HomeScore, AwayScore: m.AwayScore,
		Minute: m.Minute, Revision: m.Revision, Reason: reason, TS: m.TS,
	}
	body, _ := json.Marshal(ev)
	resp, err := httpc.Post(eventURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("emit %s failed: %v", kind, err)
		return false
	}
	defer resp.Body.Close()
	log.Printf("emit %s -> %d (%s)", kind, resp.StatusCode, ev.Score)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func readMatch() (*Match, error) {
	resp, err := httpc.Get(feedURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed returned %d", resp.StatusCode)
	}
	var m Match
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func total(m *Match) int { return m.HomeScore + m.AwayScore }
func label(m *Match) string {
	return fmt.Sprintf("%s vs %s", orDash(m.Home), orDash(m.Away))
}
func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// --- ConfigMap mirror via the in-cluster API server (no client-go) ---

func mirror(m *Match, kind string) {
	ns, token, ca, ok := saCreds()
	if !ok {
		return // not in-cluster (e.g. local run) — skip silently
	}
	data := map[string]string{
		"match_id":   m.MatchID,
		"home":       m.Home,
		"away":       m.Away,
		"score":      fmt.Sprintf("%d-%d", m.HomeScore, m.AwayScore),
		"revision":   itoa(m.Revision),
		"minute":     itoa(m.Minute),
		"status":     m.Status,
		"source":     m.Source,
		"last_kind":  kind,
		"updated_at": m.TS,
	}
	patch, _ := json.Marshal(map[string]any{"data": data})
	url := fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/configmaps/%s", ns, cmName)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca}},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("mirror patch failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("mirror patch status %d", resp.StatusCode)
	}
}

func saCreds() (ns, token string, ca *x509.CertPool, ok bool) {
	const base = "/var/run/secrets/kubernetes.io/serviceaccount"
	nb, err1 := os.ReadFile(base + "/namespace")
	tb, err2 := os.ReadFile(base + "/token")
	cb, err3 := os.ReadFile(base + "/ca.crt")
	if err1 != nil || err2 != nil || err3 != nil {
		return "", "", nil, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cb) {
		return "", "", nil, false
	}
	return strings.TrimSpace(string(nb)), strings.TrimSpace(string(tb)), pool, true
}

// helpers

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
func itoa(n int) string { return fmt.Sprintf("%d", n) }
