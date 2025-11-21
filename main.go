package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config via env
var (
	portEnv             = envOr("PORT", "8080")
	targetWebhookEnv    = envOr("TARGET_WEBHOOK_URL", "")
	redisURLEnv         = os.Getenv("REDIS_URL")
	dedupTTLEnv         = envOr("DEDUP_TTL_SECONDS", "600")
	forwardTimeoutEnv   = envOr("FORWARD_TIMEOUT_MS", "5000")
	maxCacheEntriesEnv  = envOr("MAX_CACHE_ENTRIES", "10000") // not used by go-cache
	logLevelEnv         = envOr("LOG_LEVEL", "info")
	useRedis            bool
)

var (
	forwardedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dedupe_forwarded_total",
		Help: "Number of alerts forwarded to target",
	})
	suppressedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dedupe_suppressed_total",
		Help: "Number of alerts suppressed by dedupe",
	})
	forwardErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dedupe_forward_errors_total",
		Help: "Number of forward errors",
	})
	cacheTypeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dedupe_cache_type",
		Help: "0=in-memory,1=redis",
	})
)

type AlertManagerPayload struct {
	Alerts []Alert `json:"alerts"`
}

type Alert struct {
	Status       string            `json:"status,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     string            `json:"startsAt,omitempty"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

var (
	memCache *cache.Cache
	redisCli *redis.Client
	httpClient *http.Client
	dedupTTLSeconds int
	targetWebhook string
)

func envOr(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func initProm() {
	prometheus.MustRegister(forwardedCounter, suppressedCounter, forwardErrors, cacheTypeGauge)
}

func initCache() {
	ttl, err := time.ParseDuration(fmt.Sprintf("%ss", dedupTTLEnv))
	_ = err // not used
	dedupTTLSecondsParsed, _ := time.ParseDuration(dedupTTLEnv + "s")
	dedupTTLSeconds = int(dedupTTLSecondsParsed.Seconds())

	if redisURLEnv != "" {
		// init redis
		opt, err := redis.ParseURL(redisURLEnv)
		if err != nil {
			log.Fatalf("invalid REDIS_URL: %v", err)
		}
		redisCli = redis.NewClient(opt)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := redisCli.Ping(ctx).Err(); err != nil {
			log.Fatalf("redis ping failed: %v", err)
		}
		useRedis = true
		cacheTypeGauge.Set(1)
		log.Printf("Using Redis for cache")
	} else {
		// in-memory cache with TTL
		// default cleanup interval = 1 minute
		memCache = cache.New(time.Duration(dedupTTLSecondsParsed), 1*time.Minute)
		useRedis = false
		cacheTypeGauge.Set(0)
		log.Printf("Using in-memory cache (not shared across replicas)")
	}
}

func main() {
	flag.Parse()
	initProm()

	if targetWebhookEnv == "" {
		log.Fatalf("TARGET_WEBHOOK_URL must be set")
	}
	targetWebhook = targetWebhookEnv

	// parse TTL
	ttlInt, err := time.ParseDuration(dedupTTLEnv + "s")
	if err != nil {
		log.Printf("invalid DEDUP_TTL_SECONDS, defaulting to 600s")
		dedupTTLSeconds = 600
	} else {
		dedupTTLSeconds = int(ttlInt.Seconds())
	}

	initCache()

	to, _ := time.ParseDuration(forwardTimeoutEnv + "ms")
	httpClient = &http.Client{
		Timeout: to,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request){ w.WriteHeader(200); w.Write([]byte("ok"))})
	mux.HandleFunc("/ready", handleReady)
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    ":" + portEnv,
		Handler: loggingMiddleware(mux),
	}

	log.Printf("Starting dedupe-proxy on %s, forwarding to %s, TTL=%ds, redis=%v", portEnv, targetWebhook, dedupTTLSeconds, useRedis)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	if useRedis {
		if redisCli == nil {
			http.Error(w, "redis-not-ready", 503)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := redisCli.Ping(ctx).Err(); err != nil {
			http.Error(w, "redis-not-ready", 503)
			return
		}
	}
	w.WriteHeader(200)
	w.Write([]byte("ready"))
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request){
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var payload AlertManagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	alerts := payload.Alerts
	if len(alerts) == 0 {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"no_alerts"}`))
		return
	}

	toForward := make([]Alert, 0, len(alerts))
	suppressed := 0

	for _, a := range alerts {
		key := fingerprint(a)
		ok, err := setIfNotExists(r.Context(), key, dedupTTLSeconds)
		if err != nil {
			// fail-open on storage error
			log.Printf("storage error, fail-open: %v", err)
			toForward = append(toForward, a)
			continue
		}
		if ok {
			toForward = append(toForward, a)
		} else {
			suppressed++
		}
	}

	if len(toForward) == 0 {
		suppressedCounter.Add(float64(suppressed))
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"status":"suppressed","suppressed":suppressed})
		return
	}

	if err := forwardAlerts(toForward); err != nil {
		forwardErrors.Inc()
		log.Printf("forward error: %v", err)
		http.Error(w, "forward_error", http.StatusBadGateway)
		return
	}

	forwardedCounter.Add(float64(len(toForward)))
	if suppressed > 0 {
		suppressedCounter.Add(float64(suppressed))
	}
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]interface{}{"status":"forwarded","forwarded":len(toForward),"suppressed":suppressed})
}

func fingerprint(a Alert) string {
	// build stable key: alertname|job|instance|startsAt|severity
	name := a.Labels["alertname"]
	if name == "" {
		name = a.Labels["rule"]
	}
	parts := []string{
		name,
		a.Labels["job"],
		a.Labels["instance"],
		a.StartsAt,
		a.Labels["severity"],
	}
	concat := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(concat))
	return hex.EncodeToString(sum[:])
}

func setIfNotExists(ctx context.Context, key string, ttl int) (bool, error) {
	if useRedis {
		if redisCli == nil {
			return false, errors.New("redis not initialized")
		}
		ok, err := redisCli.SetNX(ctx, key, "1", time.Duration(ttl)*time.Second).Result()
		return ok, err
	}
	// mem cache
	_, found := memCache.Get(key)
	if found {
		return false, nil
	}
	memCache.Set(key, true, time.Duration(ttl)*time.Second)
	return true, nil
}

func forwardAlerts(alerts []Alert) error {
	payload := buildRocketPayload(alerts)
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, targetWebhook, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("bad status %d: %s", resp.StatusCode, string(b))
}

func buildRocketPayload(alerts []Alert) map[string]interface{} {
	lines := make([]string, 0, len(alerts))
	for _, a := range alerts {
		l := a.Labels
		ann := a.Annotations
		sev := l["severity"]
		if sev == "" {
			sev = ann["severity"]
		}
		title := l["alertname"]
		if title == "" {
			title = ann["summary"]
		}
		job := ""
		if j := l["job"]; j != "" {
			job = fmt.Sprintf(" job=%s", j)
		}
		site := ""
		if s := l["site"]; s != "" {
			site = fmt.Sprintf(" site=%s", s)
		}
		inst := ""
		if i := l["instance"]; i != "" {
			inst = fmt.Sprintf(" instance=%s", i)
		}
		desc := ann["description"]
		if desc == "" {
			desc = ann["summary"]
		}
		line := fmt.Sprintf("*%s* (%s)%s%s%s\n%s", title, sev, job, site, inst, desc)
		lines = append(lines, line)
	}
	return map[string]interface{}{"text": strings.Join(lines, "\n\n")}
}
