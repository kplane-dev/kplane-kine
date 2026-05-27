// bench hammers a kplane apiserver with CREATE / GET workloads and
// reports throughput + latency percentiles. One invocation = one
// backend = one workload. scripts/bench.sh orchestrates the matrix
// (each backend × each workload, sequential) so the two backends
// never contend for the same node CPU/disk at the same time.
//
// Multi-CP: ops are spread across the configured --cps list. Each
// op picks a CP at random and routes the request to that CP's
// keyspace via the /clusters/{cp}/control-plane URL prefix.
//
// Auth: --certs points at the directory gen-certs.sh wrote — we
// read ca.crt and admin.token from there.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	endpoint := flag.String("endpoint", "https://localhost:6443", "apiserver base URL (no /clusters/{cp}/control-plane suffix)")
	certsDir := flag.String("certs", "", "directory containing ca.crt + admin.token (from gen-certs.sh)")
	backend := flag.String("backend", "", "human label for the backend in the output JSON (e.g. kine-postgres, etcd)")
	workload := flag.String("workload", "create", "workload: create | get")
	totalOps := flag.Int("ops", 100, "total operations to perform")
	concurrency := flag.Int("concurrency", 4, "parallel workers")
	cpsCSV := flag.String("cps", "cp-a,cp-b,cp-c", "comma-separated control plane IDs to spread ops across")
	payloadBytes := flag.Int("payload-bytes", 4096, "ConfigMap data payload size in bytes")
	namespace := flag.String("namespace", "default", "namespace in each CP to operate in")
	keyPrefix := flag.String("key-prefix", "bench", "object name prefix; full name is {prefix}-{n}")
	outPath := flag.String("out", "", "write JSON results here (default: stdout)")
	cleanup := flag.Bool("cleanup", true, "delete created objects at the end (set false to keep around for a later get run)")
	flag.Parse()

	if *certsDir == "" {
		die("--certs is required (the dir gen-certs.sh wrote)")
	}
	if *backend == "" {
		die("--backend is required (label for the output JSON, e.g. kine-postgres)")
	}
	if *totalOps < 1 || *concurrency < 1 {
		die("--ops and --concurrency must be ≥ 1")
	}

	cps := strings.Split(*cpsCSV, ",")
	for i := range cps {
		cps[i] = strings.TrimSpace(cps[i])
	}
	if len(cps) == 0 || cps[0] == "" {
		die("--cps must list at least one control plane")
	}

	ca, err := os.ReadFile(filepath.Join(*certsDir, "ca.crt"))
	must(err, "read ca.crt")
	tokenBytes, err := os.ReadFile(filepath.Join(*certsDir, "admin.token"))
	must(err, "read admin.token")
	token := strings.TrimSpace(string(tokenBytes))

	// One clientset per CP — each pinned to its /clusters/{cp}/control-plane
	// prefix. Sharing a transport pool would be possible but per-CP clients
	// keeps the result lines per-CP attributable.
	clients := make(map[string]kubernetes.Interface, len(cps))
	for _, cp := range cps {
		cfg := &rest.Config{
			Host:        fmt.Sprintf("%s/clusters/%s/control-plane", *endpoint, cp),
			BearerToken: token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
			QPS:   float32(*concurrency * 10), // raise above default 5 so we can drive load
			Burst: *concurrency * 20,
		}
		cs, err := kubernetes.NewForConfig(cfg)
		must(err, "build clientset for "+cp)
		clients[cp] = cs
	}

	payload := strings.Repeat("x", *payloadBytes)

	// Warmup: hit each CP once to trigger the apiserver's
	// per-cluster-autoip bootstrap (default 'kubernetes' Service
	// creation in each virtual CP). First requests to a fresh CP can
	// 500 during that bootstrap race; we don't want that cost in the
	// timed section.
	warmupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	for _, cp := range cps {
		for attempt := 0; attempt < 15; attempt++ {
			_, err := clients[cp].CoreV1().Namespaces().Get(warmupCtx, *namespace, metav1.GetOptions{})
			if err == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	cancel()

	res := Result{
		Backend:        *backend,
		Workload:       *workload,
		Ops:            *totalOps,
		Concurrency:    *concurrency,
		ControlPlanes:  cps,
		PayloadBytes:   *payloadBytes,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		PerCPCounts:    map[string]int{},
	}

	ctx := context.Background()
	var start time.Time
	switch *workload {
	case "create":
		start = time.Now()
		runCreate(ctx, clients, cps, *namespace, *keyPrefix, payload, *totalOps, *concurrency, &res)
	case "get":
		// GET expects objects to exist — seed first OUTSIDE the timed
		// section so OpsPerSec reflects only the GET phase. Tolerates
		// AlreadyExists for re-runs.
		ensureSeed(ctx, clients, cps, *namespace, *keyPrefix, payload, *totalOps)
		start = time.Now()
		runGet(ctx, clients, cps, *namespace, *keyPrefix, *totalOps, *concurrency, &res)
	default:
		die("unknown --workload: " + *workload + " (must be 'create' or 'get')")
	}
	elapsed := time.Since(start)
	res.DurationMS = elapsed.Milliseconds()
	res.OpsPerSec = float64(res.Ops-res.Errors) / elapsed.Seconds()
	if *cleanup {
		cleanupAll(ctx, clients, *namespace, *keyPrefix, *totalOps)
	}

	enc, _ := json.MarshalIndent(res, "", "  ")
	if *outPath == "" {
		fmt.Println(string(enc))
	} else {
		must(os.WriteFile(*outPath, append(enc, '\n'), 0o644), "write results")
		fmt.Fprintln(os.Stderr, *outPath)
	}
}

type Result struct {
	Backend       string         `json:"backend"`
	Workload      string         `json:"workload"`
	Ops           int            `json:"ops"`
	Concurrency   int            `json:"concurrency"`
	ControlPlanes []string       `json:"control_planes"`
	PayloadBytes  int            `json:"payload_bytes"`
	StartedAt     string         `json:"started_at"`
	DurationMS    int64          `json:"duration_ms"`
	OpsPerSec     float64        `json:"ops_per_sec"`
	Latency       LatencyStats   `json:"latency_ms"`
	Errors        int            `json:"errors"`
	PerCPCounts   map[string]int `json:"per_cp_counts"`
}

type LatencyStats struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

func runCreate(
	ctx context.Context,
	clients map[string]kubernetes.Interface,
	cps []string,
	namespace, keyPrefix, payload string,
	totalOps, concurrency int,
	res *Result,
) {
	work := make(chan int, totalOps)
	for i := 0; i < totalOps; i++ {
		work <- i
	}
	close(work)

	var (
		latencies = make([]float64, 0, totalOps)
		latMu     sync.Mutex
		errs      atomic.Int64
		counts    sync.Map // cp → int64
		wg        sync.WaitGroup
	)

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for n := range work {
				cp := cps[rng.Intn(len(cps))]
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-%d", keyPrefix, n),
						Namespace: namespace,
					},
					Data: map[string]string{"p": payload},
				}
				t0 := time.Now()
				_, err := clients[cp].CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
				elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
				if err != nil {
					// AlreadyExists from a re-run isn't a "real" error; the
					// op still cost the apiserver work, so count it but mark
					// it for transparency.
					if !apierrors.IsAlreadyExists(err) {
						errs.Add(1)
						continue
					}
				}
				latMu.Lock()
				latencies = append(latencies, elapsed)
				latMu.Unlock()
				v, _ := counts.LoadOrStore(cp, new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
			}
		}(w)
	}
	wg.Wait()

	res.Errors = int(errs.Load())
	res.Latency = computeStats(latencies)
	counts.Range(func(k, v any) bool {
		res.PerCPCounts[k.(string)] = int(v.(*atomic.Int64).Load())
		return true
	})
}

func runGet(
	ctx context.Context,
	clients map[string]kubernetes.Interface,
	cps []string,
	namespace, keyPrefix string,
	totalOps, concurrency int,
	res *Result,
) {
	// Pre-built op list — each op's (cp, name) is fixed up-front so we
	// don't measure RNG cost inside the timed section.
	type opT struct{ cp, name string }
	ops := make([]opT, totalOps)
	for i := range ops {
		cp := cps[i%len(cps)]
		ops[i] = opT{cp: cp, name: fmt.Sprintf("%s-%d", keyPrefix, i)}
	}
	work := make(chan opT, totalOps)
	for _, o := range ops {
		work <- o
	}
	close(work)

	var (
		latencies = make([]float64, 0, totalOps)
		latMu     sync.Mutex
		errs      atomic.Int64
		counts    sync.Map
		wg        sync.WaitGroup
	)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := range work {
				t0 := time.Now()
				_, err := clients[op.cp].CoreV1().ConfigMaps(namespace).Get(ctx, op.name, metav1.GetOptions{})
				elapsed := float64(time.Since(t0).Microseconds()) / 1000.0
				if err != nil {
					errs.Add(1)
					continue
				}
				latMu.Lock()
				latencies = append(latencies, elapsed)
				latMu.Unlock()
				v, _ := counts.LoadOrStore(op.cp, new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
			}
		}()
	}
	wg.Wait()

	res.Errors = int(errs.Load())
	res.Latency = computeStats(latencies)
	counts.Range(func(k, v any) bool {
		res.PerCPCounts[k.(string)] = int(v.(*atomic.Int64).Load())
		return true
	})
}

func ensureSeed(
	ctx context.Context,
	clients map[string]kubernetes.Interface,
	cps []string,
	namespace, keyPrefix, payload string,
	totalOps int,
) {
	// Re-uses the create path but doesn't time it — bench's GET workload
	// needs objects to exist. Tolerates AlreadyExists.
	for i := 0; i < totalOps; i++ {
		cp := cps[i%len(cps)]
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", keyPrefix, i), Namespace: namespace},
			Data:       map[string]string{"p": payload},
		}
		_, err := clients[cp].CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			die(fmt.Sprintf("seed for get: cp=%s name=%s: %v", cp, cm.Name, err))
		}
	}
}

func cleanupAll(
	ctx context.Context,
	clients map[string]kubernetes.Interface,
	namespace, keyPrefix string,
	totalOps int,
) {
	cps := make([]string, 0, len(clients))
	for k := range clients {
		cps = append(cps, k)
	}
	// Sequential — cleanup is throwaway; not measuring its cost.
	for _, cp := range cps {
		for i := 0; i < totalOps; i++ {
			_ = clients[cp].CoreV1().ConfigMaps(namespace).Delete(
				ctx, fmt.Sprintf("%s-%d", keyPrefix, i), metav1.DeleteOptions{},
			)
		}
	}
}

func computeStats(latencies []float64) LatencyStats {
	if len(latencies) == 0 {
		return LatencyStats{}
	}
	sort.Float64s(latencies)
	pick := func(p float64) float64 {
		idx := int(float64(len(latencies))*p) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}
	return LatencyStats{
		P50: pick(0.50),
		P90: pick(0.90),
		P99: pick(0.99),
		Min: latencies[0],
		Max: latencies[len(latencies)-1],
	}
}

func must(err error, what string) {
	if err != nil {
		die(what + ": " + err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "bench: "+msg)
	os.Exit(1)
}
