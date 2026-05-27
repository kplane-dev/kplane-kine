// e2e is the correctness counterpart to the bench harness. Bench
// measures throughput; e2e proves that CRUD / list / watch behaves
// correctly across multiple control planes on whatever backend is
// configured. One invocation = one backend; runs the same suite
// against both via scripts/ci.sh.
//
// What it verifies:
//   1. CRUD-in-cp-a: create → get → update → delete.
//   2. LIST scope: 5 objects in cp-a → LIST returns 5; LIST in cp-b
//      returns 0 (isolation).
//   3. WATCH ordering: open a watch on cp-a, then create/update/
//      delete; observe ADDED → MODIFIED → DELETED in order.
//   4. WATCH isolation: a watch on cp-a receives no events when
//      changes happen in cp-b.
//
// If any check fails, prints a diagnostic to stderr and exits 1.
// Designed to be safe to re-run — every step creates uniquely-named
// objects and cleans up after itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	endpoint := flag.String("endpoint", "https://localhost:6443", "apiserver base URL")
	certsDir := flag.String("certs", "", "directory containing ca.crt + admin.token")
	backend := flag.String("backend", "", "human label for the backend in log lines")
	namespace := flag.String("namespace", "default", "namespace within each CP")
	flag.Parse()

	if *certsDir == "" || *backend == "" {
		fmt.Fprintln(os.Stderr, "e2e: --certs and --backend are required")
		os.Exit(2)
	}

	ca, err := os.ReadFile(filepath.Join(*certsDir, "ca.crt"))
	check(err, "read ca.crt")
	tokenBytes, err := os.ReadFile(filepath.Join(*certsDir, "admin.token"))
	check(err, "read admin.token")
	token := strings.TrimSpace(string(tokenBytes))

	cps := []string{"cp-a", "cp-b", "cp-c"}
	clients := map[string]kubernetes.Interface{}
	for _, cp := range cps {
		cfg := &rest.Config{
			Host:        fmt.Sprintf("%s/clusters/%s/control-plane", *endpoint, cp),
			BearerToken: token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
		}
		cs, err := kubernetes.NewForConfig(cfg)
		check(err, "build clientset for "+cp)
		clients[cp] = cs
	}

	// Warmup: trigger per-cluster-autoip bootstrap for each CP so the
	// first real op doesn't race that.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, cp := range cps {
		for i := 0; i < 15; i++ {
			if _, err := clients[cp].CoreV1().Namespaces().Get(ctx, *namespace, metav1.GetOptions{}); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
	}

	fmt.Fprintf(os.Stderr, "e2e [%s]: starting\n", *backend)

	checks := []struct {
		name string
		run  func() error
	}{
		{"crud-in-cp-a", func() error { return testCRUD(ctx, clients["cp-a"], *namespace) }},
		{"list-scope", func() error { return testListScope(ctx, clients, *namespace) }},
		{"watch-ordering", func() error { return testWatchOrdering(ctx, clients["cp-a"], *namespace) }},
		{"watch-isolation", func() error { return testWatchIsolation(ctx, clients["cp-a"], clients["cp-b"], *namespace) }},
	}

	for _, c := range checks {
		t0 := time.Now()
		if err := c.run(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e [%s]: FAIL %s: %v\n", *backend, c.name, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "e2e [%s]: PASS %s (%dms)\n", *backend, c.name, time.Since(t0).Milliseconds())
	}
	fmt.Fprintf(os.Stderr, "e2e [%s]: all checks passed\n", *backend)
}

// testCRUD: create → get → update → delete on a single CP.
func testCRUD(ctx context.Context, c kubernetes.Interface, ns string) error {
	name := "e2e-crud"
	cmAPI := c.CoreV1().ConfigMaps(ns)

	// Idempotent: clear any prior remnant.
	_ = cmAPI.Delete(ctx, name, metav1.DeleteOptions{})

	created, err := cmAPI.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string]string{"k": "v1"},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if created.Data["k"] != "v1" {
		return fmt.Errorf("create: round-trip mismatch — got %q, want v1", created.Data["k"])
	}

	got, err := cmAPI.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if got.Data["k"] != "v1" {
		return fmt.Errorf("get: data drift — got %q, want v1", got.Data["k"])
	}

	got.Data["k"] = "v2"
	upd, err := cmAPI.Update(ctx, got, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if upd.Data["k"] != "v2" {
		return fmt.Errorf("update: round-trip mismatch — got %q, want v2", upd.Data["k"])
	}

	if err := cmAPI.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if _, err := cmAPI.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return fmt.Errorf("delete: get after delete returned no error")
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete: get after delete returned non-404: %w", err)
	}
	return nil
}

// testListScope: write 5 in cp-a, 2 in cp-b. Each CP's LIST must
// see only its own. Proves the multicluster keyspace isolates LIST
// at the apiserver level (not the client — we don't filter on the
// client side).
func testListScope(ctx context.Context, clients map[string]kubernetes.Interface, ns string) error {
	prefix := "e2e-list-"
	// Clean up any prior remnants in all CPs.
	for _, c := range clients {
		existing, _ := c.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		for _, cm := range existing.Items {
			if strings.HasPrefix(cm.Name, prefix) {
				_ = c.CoreV1().ConfigMaps(ns).Delete(ctx, cm.Name, metav1.DeleteOptions{})
			}
		}
	}

	cpaN, cpbN, cpcN := 5, 2, 0
	for i := 0; i < cpaN; i++ {
		if _, err := clients["cp-a"].CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%sa-%d", prefix, i)},
			Data:       map[string]string{"i": fmt.Sprint(i)},
		}, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create cp-a #%d: %w", i, err)
		}
	}
	for i := 0; i < cpbN; i++ {
		if _, err := clients["cp-b"].CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%sb-%d", prefix, i)},
			Data:       map[string]string{"i": fmt.Sprint(i)},
		}, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create cp-b #%d: %w", i, err)
		}
	}

	count := func(cp string) (int, error) {
		l, err := clients[cp].CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, err
		}
		n := 0
		for _, cm := range l.Items {
			if strings.HasPrefix(cm.Name, prefix) {
				n++
			}
		}
		return n, nil
	}
	a, err := count("cp-a")
	if err != nil {
		return fmt.Errorf("list cp-a: %w", err)
	}
	b, err := count("cp-b")
	if err != nil {
		return fmt.Errorf("list cp-b: %w", err)
	}
	c, err := count("cp-c")
	if err != nil {
		return fmt.Errorf("list cp-c: %w", err)
	}
	if a != cpaN || b != cpbN || c != cpcN {
		return fmt.Errorf("list isolation broken: cp-a=%d (want %d), cp-b=%d (want %d), cp-c=%d (want %d)",
			a, cpaN, b, cpbN, c, cpcN)
	}

	// Cleanup
	for _, cp := range []string{"cp-a", "cp-b"} {
		existing, _ := clients[cp].CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		for _, cm := range existing.Items {
			if strings.HasPrefix(cm.Name, prefix) {
				_ = clients[cp].CoreV1().ConfigMaps(ns).Delete(ctx, cm.Name, metav1.DeleteOptions{})
			}
		}
	}
	return nil
}

// testWatchOrdering: open WATCH, then create / update / delete a CM
// and observe ADDED, MODIFIED, DELETED in order.
func testWatchOrdering(ctx context.Context, c kubernetes.Interface, ns string) error {
	name := "e2e-watch-ord"
	cmAPI := c.CoreV1().ConfigMaps(ns)
	_ = cmAPI.Delete(ctx, name, metav1.DeleteOptions{})

	// Drain any stale list before starting the watch (so we don't see
	// pre-existing objects through the initial sync).
	list, err := cmAPI.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("pre-watch list: %w", err)
	}

	w, err := cmAPI.Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.ResourceVersion,
		FieldSelector:   fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	defer w.Stop()

	// Triggers
	go func() {
		time.Sleep(200 * time.Millisecond)
		cm, _ := cmAPI.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Data:       map[string]string{"k": "a"},
		}, metav1.CreateOptions{})
		time.Sleep(200 * time.Millisecond)
		cm.Data["k"] = "b"
		_, _ = cmAPI.Update(ctx, cm, metav1.UpdateOptions{})
		time.Sleep(200 * time.Millisecond)
		_ = cmAPI.Delete(ctx, name, metav1.DeleteOptions{})
	}()

	want := []watch.EventType{watch.Added, watch.Modified, watch.Deleted}
	got := make([]watch.EventType, 0, len(want))

	deadline := time.NewTimer(20 * time.Second) // Kine's poll-loop floor is ~1s
	defer deadline.Stop()

	for len(got) < len(want) {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch closed early (received: %v, expected: %v)", got, want)
			}
			if ev.Type == watch.Bookmark {
				continue
			}
			got = append(got, ev.Type)
		case <-deadline.C:
			return fmt.Errorf("watch timed out after 20s (received: %v, expected: %v)", got, want)
		}
	}

	for i, w := range want {
		if got[i] != w {
			return fmt.Errorf("watch event %d: got %s, want %s (full: %v)", i, got[i], w, got)
		}
	}
	return nil
}

// testWatchIsolation: a watch on cp-a should NOT receive events when
// equivalent objects mutate in cp-b. Same name, same namespace —
// different CP. If kplane's PathExtractor leaked, we'd see the cp-b
// events here.
func testWatchIsolation(ctx context.Context, cpa, cpb kubernetes.Interface, ns string) error {
	name := "e2e-watch-iso"
	_ = cpa.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})
	_ = cpb.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})

	list, err := cpa.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("pre-watch list (cp-a): %w", err)
	}
	w, err := cpa.CoreV1().ConfigMaps(ns).Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.ResourceVersion,
		FieldSelector:   fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return fmt.Errorf("watch on cp-a: %w", err)
	}
	defer w.Stop()

	// Mutate in cp-b. cp-a's watch should see nothing.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cm, _ := cpb.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Data:       map[string]string{"side": "b"},
		}, metav1.CreateOptions{})
		time.Sleep(200 * time.Millisecond)
		if cm != nil {
			cm.Data["side"] = "b-updated"
			_, _ = cpb.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
		}
		time.Sleep(200 * time.Millisecond)
		_ = cpb.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}()

	// Listen for 3 seconds — long enough to be past kine's ~1s poll
	// floor for all 3 ops. If we see ANY non-bookmark event on cp-a
	// during this window, isolation is broken.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil // channel closed without leaking events — fine
			}
			if ev.Type == watch.Bookmark {
				continue
			}
			return fmt.Errorf("watch isolation broken: cp-a watch saw a cp-b event: %v", ev)
		case <-deadline.C:
			return nil // 3s passed with no leak — isolation holds
		}
	}
}

func check(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %s: %v\n", what, err)
		os.Exit(2)
	}
}
