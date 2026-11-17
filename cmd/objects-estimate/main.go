// Command objects-estimate measures what the object-state carrier would send for one
// cluster, without installing anything. It runs the carrier's watcher from this machine
// under a kubeconfig context, keeps the hash store in memory, and counts each record's bytes
// as the infra sink would post it, in place of sending it. Every request other than GET is
// refused before it leaves the machine, and nothing is written to the cluster.
//
// Secrets are not watched as objects; see secrets.go.
//
// Usage: objects-estimate -context <ctx> -cluster <name> -duration 1h -out <dir>
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

// integrationVersion is set at build time with the carrier's own ldflag,
// -X 'main.integrationVersion=$(TAG)' (Makefile, Dockerfile), so the sized events carry the
// version the carrier image writes. Unset, the estimate uses chartAppVersion.
var integrationVersion string

const (
	// chartAppVersion is the chart's appVersion (charts/nri-kube-events/Chart.yaml).
	chartAppVersion = "2.25.0"
	defaultQPS      = 20
	defaultBurst    = 40
	// hmacKeyBytes is the length of the random key of the run's Secret redaction.
	hmacKeyBytes    = 32
	outDirMode      = 0o755
	summaryFileMode = 0o600
)

type result struct {
	Start      time.Time     `json:"start"`
	End        time.Time     `json:"end"`
	Cluster    string        `json:"cluster"`
	Context    string        `json:"context"`
	FailedGVRs []string      `json:"failedGVRs"`
	Objects    sinkSummary   `json:"objects"`
	Secrets    secretSummary `json:"secrets"`
	// ChangeWindowSeconds is the time from the end of the first full list to End: the window
	// the change counts cover.
	ChangeWindowSeconds float64 `json:"changeWindowSeconds"`
	Final               bool    `json:"final"`
}

func main() {
	kubeconfig := flag.String("kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig file")
	kubeContext := flag.String("context", "", "kubeconfig context (required)")
	cluster := flag.String("cluster", "", "cluster name written into the sized records (default: the context)")
	duration := flag.Duration("duration", time.Hour, "how long to watch after starting")
	outDir := flag.String("out", ".", "directory for summary.json, rewritten every minute; created if missing")
	version := flag.String("integration-version", defaultIntegrationVersion(), "integrationVersion written into the sized events; set it to the deployed image's build TAG. Only its length reaches the sizes")
	qps := flag.Float64("qps", defaultQPS, "client QPS limit")
	burst := flag.Int("burst", defaultBurst, "client burst limit")
	// The hash store's client is a fake, so the namespace needs no counterpart on the
	// cluster. Set it to an installed carrier's namespace to leave that carrier's own store
	// objects out, as that carrier does.
	storeNamespace := flag.String("carrier-namespace", "objects-estimate", "namespace whose carrier store objects are not counted")
	flag.Parse()
	if *kubeContext == "" {
		log.Fatal("-context is required")
	}
	if *cluster == "" {
		*cluster = *kubeContext
	}
	if err := os.MkdirAll(*outDir, outDirMode); err != nil {
		log.Fatalf("creating %s: %v", *outDir, err)
	}

	conf, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: *kubeContext},
	).ClientConfig()
	if err != nil {
		log.Fatalf("loading context %s: %v", *kubeContext, err)
	}
	conf.QPS, conf.Burst = float32(*qps), *burst
	conf.Wrap(objects.ReadOnlyTransport)

	clientset, err := kubernetes.NewForConfig(conf)
	if err != nil {
		log.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(conf)
	if err != nil {
		log.Fatal(err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(conf)
	if err != nil {
		log.Fatal(err)
	}

	// The hash store lives in a fake clientset, so nothing is written to the cluster.
	store := objects.NewStore(fake.NewClientset(), *storeNamespace)
	if err := store.Load(context.Background()); err != nil {
		log.Fatalf("in-memory hash store: %v", err)
	}
	key := make([]byte, hmacKeyBytes)
	if _, err := rand.Read(key); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("signal: stopping early")
		cancel()
	}()

	sink := newCountingSink(*cluster, *version)
	// ResendAfter stays zero: the estimate measures the first send and the changes.
	w := objects.NewWatcher(objects.Config{
		ClusterName: *cluster,
		Namespace:   *storeNamespace,
		Skip:        map[schema.GroupVersionResource]bool{secretsGVR: true},
	}, dyn, disc, store, sink, key)

	start := time.Now().UTC()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			log.WithError(err).Warn("watcher stopped")
		}
	}()

	secrets := newSecretCounter(*cluster, *version)
	go func() {
		namespaces, err := listNamespaces(ctx, clientset)
		if err == nil {
			var rv string
			if rv, err = secrets.listInitial(ctx, clientset.CoreV1().RESTClient(), namespaces, time.Now()); err == nil {
				secrets.watchChanges(ctx, clientset.CoreV1().RESTClient(), rv)
				return
			}
		}
		log.WithError(err).Warn("secrets not counted")
		secrets.fail(err)
	}()

	write := func(final bool) {
		r := result{
			Cluster: *cluster, Context: *kubeContext, Start: start, End: time.Now().UTC(), Final: final,
			FailedGVRs: w.FailedGVRs(), Objects: sink.summary(), Secrets: secrets.summary(),
		}
		if r.Objects.FullListAt != nil {
			r.ChangeWindowSeconds = r.End.Sub(*r.Objects.FullListAt).Seconds()
		}
		if err := writeJSON(filepath.Join(*outDir, "summary.json"), r); err != nil {
			log.WithError(err).Error("writing summary")
		}
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			write(false)
		case <-done:
			write(true)
			log.Infof("wrote %s", filepath.Join(*outDir, "summary.json"))
			return
		}
	}
}

// defaultIntegrationVersion is the build-time integrationVersion, or chartAppVersion when the
// binary was built without it.
func defaultIntegrationVersion() string {
	if integrationVersion != "" {
		return integrationVersion
	}
	return chartAppVersion
}

func listNamespaces(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	var out []string
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := cs.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing namespaces: %w", err)
		}
		for _, ns := range list.Items {
			out = append(out, ns.Name)
		}
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

// writeJSON replaces path atomically, so a reader never sees a half-written summary. It
// creates path's directory if it is missing.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), outDirMode); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, summaryFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
