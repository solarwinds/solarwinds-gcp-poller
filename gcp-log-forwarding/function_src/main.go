// go:build !ignore
package gcp_logs_oltp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	cloudevents "github.com/cloudevents/sdk-go/v2"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	semconv "go.opentelemetry.io/collector/semconv/v1.5.0"

	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

//
// ─────────────────────────── Config ───────────────────────────
//

const (
	envOTLPEndpoint = "OTLP_ENDPOINT"
	envAPIToken     = "API_TOKEN"
	envLogLevel     = "LOG_LEVEL"

	defaultMaxBatchRecords = 2000
	defaultMaxBatchBytes   = 1_500_000
	defaultExportTimeout   = 7 * time.Second
	defaultMaxRetries      = 3
	defaultWorkers         = 4 // request-scoped parallel exports
)

type cfg struct {
	Endpoint      string
	APIToken      string
	Debug         bool
	MaxBatchRecs  int
	MaxBatchBytes int
	ExportTimeout time.Duration
	MaxRetries    int
	Workers       int // parallel export concurrency (request-scoped)
}

func mustEnv(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		log.Fatalf("missing required env %s", k)
	}
	return v
}

func getenvInt(k string, def int) int {
	if s := strings.TrimSpace(os.Getenv(k)); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func getenvDur(k string, def time.Duration) time.Duration {
	if s := strings.TrimSpace(os.Getenv(k)); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func loadConfig() cfg {
	return cfg{
		Endpoint:      mustEnv(envOTLPEndpoint),
		APIToken:      mustEnv(envAPIToken),
		Debug:         strings.EqualFold(os.Getenv(envLogLevel), "DEBUG"),
		MaxBatchRecs:  getenvInt("MAX_BATCH_RECORDS", defaultMaxBatchRecords),
		MaxBatchBytes: getenvInt("MAX_BATCH_BYTES", defaultMaxBatchBytes),
		ExportTimeout: getenvDur("EXPORT_TIMEOUT", defaultExportTimeout),
		MaxRetries:    getenvInt("MAX_RETRIES", defaultMaxRetries),
		Workers:       getenvInt("WORKERS", defaultWorkers),
	}
}

//
// ───────────────────── Pub/Sub CloudEvent types ─────────────────────
//

type MessagePublishedData struct {
	Message      PubSubMessage `json:"message"`
	Subscription string        `json:"subscription"`
}

type PubSubMessage struct {
	Data       []byte            `json:"data"`       // base64 → []byte auto-decoded by encoding/json
	Attributes map[string]string `json:"attributes"` // optional
}

//
// ───────────────────────── Export pipeline ─────────────────────────
//

type anyMap = map[string]interface{}

type resourceKey struct {
	Service string
	Plat    string
	Host    string
	Region  string
}

type exporter struct {
	c      cfg
	conn   *grpc.ClientConn
	client plogotlp.GRPCClient
}

var (
	globalExporter *exporter
	globalCfg      cfg
	initOnce       sync.Once
)

func newExporter(c cfg) (*exporter, error) {
	kp := keepalive.ClientParameters{
		Time:                60 * time.Second,
		Timeout:             10 * time.Second,
		PermitWithoutStream: true,
	}
	retryBackoff := backoff.Config{
		BaseDelay:  200 * time.Millisecond,
		Multiplier: 1.6,
		MaxDelay:   3 * time.Second,
	}
	conn, err := grpc.Dial(
		c.Endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithKeepaliveParams(kp),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           retryBackoff,
			MinConnectTimeout: 5 * time.Second,
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, err
	}
	return &exporter{
		c:      c,
		conn:   conn,
		client: plogotlp.NewGRPCClient(conn),
	}, nil
}

func (e *exporter) exportWithRetry(ctx context.Context, req plogotlp.ExportRequest) error {
	var err error
	for attempt := 0; attempt <= e.c.MaxRetries; attempt++ {
		// Nudge unhealthy channel
		if e.conn.GetState() == connectivity.TransientFailure {
			e.conn.Connect()
		}
		rctx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+e.c.APIToken)
		if _, err = e.client.Export(rctx, req); err == nil {
			return nil
		}
		if attempt == e.c.MaxRetries {
			break
		}
		time.Sleep(backoffJitter(200*time.Millisecond, attempt))
	}
	return err
}

func backoffJitter(base time.Duration, n int) time.Duration {
	ms := float64(base.Milliseconds()) * math.Pow(1.6, float64(n))
	j := ms * (0.2 + rand.Float64()*0.4)
	return time.Duration(ms+j) * time.Millisecond
}

func estimateEntryBytes(e anyMap) int {
	if b, err := json.Marshal(e); err == nil {
		return len(b)
	}
	return 256
}

//
// ─────────────────────────── Helpers ───────────────────────────
//

func extractLogEntries(raw []byte) []anyMap {
	var arr []anyMap
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var one anyMap
	if err := json.Unmarshal(raw, &one); err == nil {
		if v, ok := one["entries"]; ok {
			if list, ok := v.([]interface{}); ok {
				out := make([]anyMap, 0, len(list))
				for _, it := range list {
					if m, ok := it.(map[string]interface{}); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		return []anyMap{one}
	}
	return nil
}

func getString(m anyMap, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func chooseBody(e anyMap) string {
	if s := getString(e, "textPayload"); s != "" {
		return s
	}
	if jp, ok := e["jsonPayload"]; ok && jp != nil {
		if b, err := json.Marshal(jp); err == nil {
			return string(b)
		}
	}
	if pp, ok := e["protoPayload"]; ok && pp != nil {
		if b, err := json.Marshal(pp); err == nil {
			return string(b)
		}
	}
	if b, err := json.Marshal(e); err == nil {
		return string(b)
	}
	return ""
}

func parseRFC3339Nanos(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixNano()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixNano()
	}
	return 0
}

func attrsFromEntry(entry anyMap) map[string]string {
	defaults := map[string]string{
		"service.name":   "compute.googleapis.com",
		"cloud.provider": "gcp",
		"host.id":        "unknown",
		"cloud.platform": "gcp_compute_engine",
	}
	attrs := map[string]string{"cloud.provider": "gcp"}

	zoneToRegion := func(val string) string {
		if val == "" {
			return ""
		}
		parts := strings.Split(val, "-")
		if len(parts) >= 2 {
			return strings.Join(parts[:len(parts)-1], "-")
		}
		return val
	}

	if pp, ok := entry["protoPayload"].(map[string]interface{}); ok {
		if s, ok := pp["serviceName"].(string); ok && s != "" {
			attrs["service.name"] = s
		}
	} else if s := getString(entry, "serviceName"); s != "" {
		attrs["service.name"] = s
	}

	var mr map[string]interface{}
	if x, ok := entry["resource"].(map[string]interface{}); ok {
		mr = x
	} else if x, ok := entry["monitoredResource"].(map[string]interface{}); ok {
		mr = x
	}
	rtype := strings.ToLower(getString(mr, "type"))
	var labels map[string]interface{}
	if v, ok := mr["labels"].(map[string]interface{}); ok {
		labels = v
	} else {
		labels = map[string]interface{}{}
	}

	getLabel := func(k string) string {
		if v, ok := labels[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}

	switch {
	case rtype == "gce_instance":
		attrs["cloud.platform"] = "gcp_compute_engine"
		if id := getLabel("instance_id"); id != "" {
			attrs["host.id"] = id
		}
		if z := getLabel("zone"); z != "" {
			attrs["cloud.availability_zone"] = z
			attrs["cloud.region"] = zoneToRegion(z)
		}
	case strings.Contains(rtype, "sql"):
		attrs["cloud.platform"] = "gcp_cloud_sql"
		if id := getLabel("database_id"); id != "" {
			attrs["host.id"] = id
		}
		if r := getLabel("region"); r != "" {
			attrs["cloud.region"] = r
		} else if loc := getLabel("location"); loc != "" {
			attrs["cloud.region"] = loc
		}
	case strings.Contains(rtype, "gcs"), strings.Contains(rtype, "storage"):
		attrs["cloud.platform"] = "gcp_cloud_storage"
		if b := getLabel("bucket_name"); b != "" {
			attrs["gcp.bucket.name"] = b
			attrs["host.id"] = b
		}
		if loc := getLabel("location"); loc != "" {
			attrs["cloud.region"] = zoneToRegion(loc)
		}
	}

	if sev := getString(entry, "severity"); strings.TrimSpace(sev) != "" {
		attrs["SeverityText"] = strings.ToUpper(strings.TrimSpace(sev))
	}

	for k, v := range defaults {
		if _, ok := attrs[k]; !ok {
			attrs[k] = v
		}
	}
	return attrs
}

//
// ────────────────────── CloudEvent entrypoint ──────────────────────
//

func init() {
	rand.Seed(time.Now().UnixNano())

	initOnce.Do(func() {
		globalCfg = loadConfig()
		exp, err := newExporter(globalCfg)
		if err != nil {
			log.Fatalf("exporter init failed: %v", err)
		}
		globalExporter = exp
		log.Printf("gcp-logs-otlp (CF Gen2, Pub/Sub trigger) ready: endpoint=%s workers=%d batch=%d/%d timeout=%s",
			globalCfg.Endpoint, globalCfg.Workers, globalCfg.MaxBatchRecs, globalCfg.MaxBatchBytes, globalCfg.ExportTimeout)
	})

	functions.CloudEvent("ForwardLogs", ForwardLogs)
}

// ForwardLogs: parse → group → chunk → export in parallel (bounded).
// Any export error → return error (NACK) so Pub/Sub retries.
func ForwardLogs(ctx context.Context, e cloudevents.Event) error {
	if globalExporter == nil {
		return fmt.Errorf("exporter not initialized")
	}
	start := time.Now()

	var d MessagePublishedData
	if err := e.DataAs(&d); err != nil {
		return fmt.Errorf("cloudevent decode failed: %w", err) // NACK
	}

	raw := d.Message.Data
	if len(raw) == 0 {
		log.Printf("forwardlogs: empty payload (event=%s, sub=%s, attrs=%v)", e.ID(), d.Subscription, d.Message.Attributes)
		return nil // ACK, nothing to do
	}

	entries := extractLogEntries(raw)
	if len(entries) == 0 {
		log.Printf("forwardlogs: parsed 0 entries (event=%s, bytes=%d, attrs=%v)", e.ID(), len(raw), d.Message.Attributes)
		return nil // ACK
	}

	// Group by resource identity
	buckets := map[resourceKey][]anyMap{}
	for _, ent := range entries {
		a := attrsFromEntry(ent)
		k := resourceKey{
			Service: a["service.name"],
			Plat:    a["cloud.platform"],
			Host:    a["host.id"],
			Region:  a["cloud.region"],
		}
		buckets[k] = append(buckets[k], ent)
	}

	// Export concurrently (bounded by WORKERS)
	exported, err := exportBucketsConcurrently(ctx, buckets)
	if err != nil {
		return err // NACK → Pub/Sub retry
	}

	log.Printf("forwardlogs: OK exported=%d buckets=%d entries_in=%d bytes_in=%d dur=%s event=%s",
		exported, len(buckets), len(entries), len(raw), time.Since(start), e.ID())
	return nil
}

//
// ────────────────────── Request-scoped concurrency ──────────────────────
//

// exportBucketsConcurrently splits each bucket into size-bounded chunks
// and exports chunks in parallel, bounded by cfg.Workers.
// Returns total records exported or error (to trigger Pub/Sub retry).
func exportBucketsConcurrently(ctx context.Context, buckets map[resourceKey][]anyMap) (int, error) {
	maxConc := globalCfg.Workers
	if maxConc < 1 {
		maxConc = 1
	}
	sem := make(chan struct{}, maxConc)
	g, gctx := errgroup.WithContext(ctx)

	var exported int64

	// Iterate buckets
	for rk, list := range buckets {
		// Chunk by count and approx bytes
		for i := 0; i < len(list); {
			count, approxBytes := 0, 0
			j := i
			for j < len(list) && count < globalCfg.MaxBatchRecs {
				est := estimateEntryBytes(list[j])
				if count > 0 && (approxBytes+est) > globalCfg.MaxBatchBytes {
					break
				}
				approxBytes += est
				count++
				j++
			}
			chunk := list[i:j]
			i = j

			// Acquire a parallel slot or abort on context cancel
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return int(atomic.LoadInt64(&exported)), gctx.Err()
			}

			// Capture variables for goroutine
			rk = rk
			chunk = chunk
			approxBytes = approxBytes

			g.Go(func() error {
				defer func() { <-sem }()

				// Build OTLP logs for this chunk
				logs := buildOTLPLogs(rk, chunk)

				// Export with timeout + retry
				req := plogotlp.NewExportRequestFromLogs(logs)
				ctxExp, cancel := context.WithTimeout(gctx, globalCfg.ExportTimeout)
				err := globalExporter.exportWithRetry(ctxExp, req)
				cancel()
				if err != nil {
					log.Printf("forwardlogs: OTLP export failed: %v (svc=%s region=%s approx=%dB n=%d)",
						err, rk.Service, rk.Region, approxBytes, len(chunk))
					return fmt.Errorf("otlp export failed: %w", err)
				}

				atomic.AddInt64(&exported, int64(len(chunk)))
				return nil
			})
		}
	}

	if err := g.Wait(); err != nil {
		return int(atomic.LoadInt64(&exported)), err
	}
	return int(atomic.LoadInt64(&exported)), nil
}

// buildOTLPLogs constructs a plog.Logs payload for a single resource chunk.
func buildOTLPLogs(k resourceKey, chunk []anyMap) plog.Logs {
	logs := plog.NewLogs()
	rl := logs.ResourceLogs().AppendEmpty()
	rl.SetSchemaUrl(semconv.SchemaURL)
	rattrs := rl.Resource().Attributes()
	rattrs.PutStr("cloud.provider", "gcp")
	rattrs.PutStr("service.name", k.Service)
	rattrs.PutStr("cloud.platform", k.Plat)
	rattrs.PutStr("host.id", k.Host)
	if k.Region != "" {
		rattrs.PutStr("cloud.region", k.Region)
	}

	sl := rl.ScopeLogs().AppendEmpty()
	now := time.Now().UnixNano()
	for _, ent := range chunk {
		lr := sl.LogRecords().AppendEmpty()
		if ts := parseRFC3339Nanos(getString(ent, "timestamp")); ts > 0 {
			lr.SetTimestamp(pcommon.Timestamp(ts))
		} else {
			lr.SetTimestamp(pcommon.Timestamp(now))
		}
		lr.Body().SetStr(chooseBody(ent))
		if sev := getString(ent, "severity"); sev != "" {
			lr.SetSeverityText(strings.ToUpper(strings.TrimSpace(sev)))
		}
	}
	return logs
}
