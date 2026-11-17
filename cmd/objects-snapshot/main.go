// Command objects-snapshot lists every object of one cluster the way the object-state
// carrier sees it and prints one row per object, with the hash the carrier stores for it.
// It uses the carrier's own discovery, own-store filter and normalization, so a row's
// object.hash equals the carrier's for the same version of the object. Every request other
// than GET is refused before it leaves the machine.
//
// Secrets are not listed as objects; see secrets.go.
//
// Output is tab-separated: a header, then one row per object sorted by kind, group,
// namespace and name. group is empty for the core group. Each kind whose list failed is
// printed to stderr, and the exit status is then 2.
//
// Usage: objects-snapshot -context <ctx> [-carrier-namespace newrelic-objects] [-out file]
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/newrelic/nri-kube-events/pkg/objects"
)

const (
	// listLimit is the page size of every list.
	listLimit = 500

	defaultQPS   = 20
	defaultBurst = 40
	// outFileMode is the mode of an -out file.
	outFileMode = 0o600

	exitError      = 1
	exitListFailed = 2
)

// row is one object of the snapshot. Secret rows have no uid and no hash, and rows of a
// non-core kind named Secret have no hash.
type row struct {
	uid, kind, group, namespace, name, hash string
}

func main() {
	kubeconfig := flag.String("kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig file")
	kubeContext := flag.String("context", "", "kubeconfig context (required)")
	carrierNamespace := flag.String("carrier-namespace", "newrelic-objects", "namespace of the installed carrier, whose own store objects are left out")
	qps := flag.Float64("qps", defaultQPS, "client QPS limit")
	burst := flag.Int("burst", defaultBurst, "client burst limit")
	outPath := flag.String("out", "", "output file (default stdout)")
	flag.Parse()
	if *kubeContext == "" {
		log.Fatal("-context is required")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := writeSnapshot(*outPath, func(out io.Writer) int {
		return run(ctx, dyn, disc, clientset, *carrierNamespace, out, os.Stderr)
	})
	stop()
	os.Exit(code)
}

// writeSnapshot runs write against path, or stdout when path is empty, and returns its exit
// status. The output is buffered and flushed once write returns.
func writeSnapshot(path string, write func(io.Writer) int) int {
	f := os.Stdout
	if path != "" {
		var err error
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, outFileMode); err != nil {
			fmt.Fprintf(os.Stderr, "opening %s: %v\n", path, err)
			return exitError
		}
	}
	buf := bufio.NewWriter(f)
	code := write(buf)
	err := buf.Flush()
	if path != "" {
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "writing snapshot: %v\n", err)
		return exitError
	}
	return code
}

// run writes the snapshot of the cluster to out: every object snapshot lists, then every
// Secret by name. It prints each failed list to errOut and returns the exit status.
func run(ctx context.Context, dyn dynamic.Interface, disc discovery.DiscoveryInterface, cs kubernetes.Interface, carrierNamespace string, out, errOut io.Writer) int {
	rows, failed, err := snapshot(ctx, dyn, disc, carrierNamespace)
	if err != nil {
		fmt.Fprintf(errOut, "objects-snapshot: %v\n", err)
		return exitError
	}
	namespaces, err := listNamespaces(ctx, cs)
	if err != nil {
		log.WithError(err).Warn("secrets not listed")
		failed = append(failed, objects.GVRString(secretsGVR))
	}
	for _, ns := range namespaces {
		names, err := listSecretNames(ctx, cs.CoreV1().RESTClient(), ns)
		if err != nil {
			log.WithError(err).Warn("secrets not listed")
			failed = append(failed, objects.GVRString(secretsGVR)+" in "+ns)
			continue
		}
		rows = append(rows, secretRows(carrierNamespace, ns, names)...)
	}
	if err := writeRows(out, rows); err != nil {
		fmt.Fprintf(errOut, "objects-snapshot: writing rows: %v\n", err)
		return exitError
	}
	for _, f := range failed {
		fmt.Fprintf(errOut, "list failed: %s\n", f)
	}
	if len(failed) > 0 {
		return exitListFailed
	}
	return 0
}

// snapshot lists every kind the carrier watches, found with the carrier's own discovery
// with core v1 Secrets skipped, and returns a row per object with the carrier's hash. The
// carrier's own hash-store objects in carrierNamespace are left out. failed names each kind
// whose list failed, and each group whose discovery failed; a failed kind has no rows.
func snapshot(ctx context.Context, dyn dynamic.Interface, disc discovery.DiscoveryInterface, carrierNamespace string) (rows []row, failed []string, err error) {
	found, failedGroups, err := objects.Discover(disc, map[schema.GroupVersionResource]bool{secretsGVR: true})
	if err != nil {
		return nil, nil, fmt.Errorf("discovery: %w", err)
	}
	for g := range failedGroups {
		failed = append(failed, "discovery of group "+g)
	}
	gvrs := make([]schema.GroupVersionResource, 0, len(found))
	for gvr := range found {
		gvrs = append(gvrs, gvr)
	}
	sort.Slice(gvrs, func(i, j int) bool { return objects.GVRString(gvrs[i]) < objects.GVRString(gvrs[j]) })
	for _, gvr := range gvrs {
		got, err := listKind(ctx, dyn, gvr, found[gvr], carrierNamespace)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			log.WithError(err).WithField("gvr", objects.GVRString(gvr)).Warn("list failed")
			failed = append(failed, objects.GVRString(gvr))
			continue
		}
		rows = append(rows, got...)
	}
	sort.Strings(failed)
	return rows, failed, nil
}

// listKind lists gvr page by page and returns a row per object, each hashed as the carrier
// hashes it. kind is the kind discovery reports for gvr.
func listKind(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, kind, carrierNamespace string) ([]row, error) {
	var rows []row
	opts := metav1.ListOptions{Limit: listLimit}
	for {
		list, err := dyn.Resource(gvr).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			obj := list.Items[i].Object
			if objects.IsOwnStoreObject(carrierNamespace, gvr, obj) {
				continue
			}
			// Only Secrets use the redaction key, and core Secrets are never listed here.
			red, hash := objects.PrepareObject(gvr, kind, obj, nil)
			meta, _ := red["metadata"].(map[string]any)
			r := row{group: gvr.Group, hash: hash}
			r.kind, _ = red["kind"].(string)
			// Redaction treats any kind named Secret as one, in every group, and the
			// carrier redacts it with its own key, which the snapshot does not have. Its
			// hash cannot match the carrier's, so the row carries none, like a core Secret.
			if r.kind == "Secret" {
				r.hash = ""
			}
			r.uid, _ = meta["uid"].(string)
			r.namespace, _ = meta["namespace"].(string)
			r.name, _ = meta["name"].(string)
			rows = append(rows, r)
		}
		if list.GetContinue() == "" {
			return rows, nil
		}
		opts.Continue = list.GetContinue()
	}
}

func listNamespaces(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	var out []string
	opts := metav1.ListOptions{Limit: listLimit}
	for {
		list, err := cs.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing namespaces: %w", err)
		}
		for i := range list.Items {
			out = append(out, list.Items[i].Name)
		}
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

// writeRows writes the header and rows to w, sorted by kind, group, namespace, name and uid.
func writeRows(w io.Writer, rows []row) error {
	sorted := append([]row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		for _, p := range [][2]string{{a.kind, b.kind}, {a.group, b.group}, {a.namespace, b.namespace}, {a.name, b.name}} {
			if p[0] != p[1] {
				return p[0] < p[1]
			}
		}
		return a.uid < b.uid
	})
	var sb strings.Builder
	sb.WriteString("uid\tkind\tgroup\tnamespace\tname\tobject.hash\n")
	for _, r := range sorted {
		sb.WriteString(strings.Join([]string{r.uid, r.kind, r.group, r.namespace, r.name, r.hash}, "\t"))
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(w, sb.String())
	return err
}
