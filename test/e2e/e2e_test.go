// Package e2e_test implements simple integration test against a local cluster, whose config is loaded from the kubeconfig file.
package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	sdkEvent "github.com/newrelic/infra-integrations-sdk/data/event"
	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/newrelic/nri-kube-events/test/e2e"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// We must have a global MockedAgentSink because the infrastructure-sdk attempts to register global flags when the
// agent sink is created, which results in a panic if multiple instantiations are attempted.
var globalMockedAgentSink *e2e.MockedAgentSink

// TestPodCreation checks that events related to pod creation are received.
func TestPodCreation(t *testing.T) {
	client, agentMock := initialize(t)

	t.Log("Creating test namespace...")
	ns, err := client.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-" + strings.ToLower(t.Name()) + "-",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("could not create %s namespace: %v", ns, err)
	}

	defer func() {
		t.Log("Cleaning up test namespace...")
		_ = client.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	}()

	t.Log("Creating test pod...")
	testpod, err := client.CoreV1().Pods(ns.Name).Create(context.Background(), &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nginx-e2e",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "nginx",
					Image: "nginx",
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("could not create test pod: %v", err)
		return
	}

	t.Log("Waiting for events to show up...")
	agentMock.Wait(10*time.Second, 1*time.Minute)
	for _, event := range []sdkEvent.Event{
		// All strings are matched in a very relaxed way, using strings.Contains(real, test)
		{
			Summary:  "Successfully assigned " + ns.Name + "/" + testpod.Name + " to ",
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Scheduled",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Successfully assigned " + ns.Name + "/" + testpod.Name + " to ",
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
		{
			Summary:  "Pulling image \"" + testpod.Spec.Containers[0].Image + "\"",
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Pulling",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Pulling image \"" + testpod.Spec.Containers[0].Image + "\"",
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
		{
			Summary:  "Successfully pulled image \"" + testpod.Spec.Containers[0].Image + "\"",
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Pulled",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Successfully pulled image \"" + testpod.Spec.Containers[0].Image + "\"",
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
		{
			Summary:  "Created container " + testpod.Spec.Containers[0].Name,
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Created",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Created container " + testpod.Spec.Containers[0].Name,
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
		{
			Summary:  "Started container " + testpod.Spec.Containers[0].Name,
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Started",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Started container " + testpod.Spec.Containers[0].Name,
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
	} {
		if agentMock.Has(&event) {
			continue
		}

		e := json.NewEncoder(os.Stderr)
		t.Log("Expected:")
		_ = e.Encode(event)
		t.Log("Have:")
		_ = e.Encode(agentMock.Events())
		t.Fatalf("Event was not captured")
	}
}

func TestPodDeletion(t *testing.T) {
	client, agentMock := initialize(t)

	t.Log("Creating test namespace...")
	ns, err := client.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-" + strings.ToLower(t.Name()) + "-",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("could not create %s namespace: %v", ns, err)
	}

	defer func() {
		t.Log("Cleaning up test namespace...")
		_ = client.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	}()

	t.Log("Creating test pod...")
	testpod, err := client.CoreV1().Pods(ns.Name).Create(context.Background(), &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nginx-e2e-killable",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "nginx",
					Image: "nginx",
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("could not create test pod: %v", err)
		return
	}

	time.Sleep(7 * time.Second)

	err = client.CoreV1().Pods(ns.Name).Delete(context.Background(), testpod.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("could not create test pod: %v", err)
		return
	}

	t.Log("Waiting for events to show up...")
	agentMock.Wait(15*time.Second, 1*time.Minute)
	for _, event := range []sdkEvent.Event{
		{
			Summary:  "Stopping container " + testpod.Spec.Containers[0].Name,
			Category: "kubernetes",
			Attributes: map[string]interface{}{
				"event.metadata.name":             testpod.Name + ".",
				"event.metadata.namespace":        ns.Name,
				"event.reason":                    "Killing",
				"clusterName":                     "",
				"event.involvedObject.apiVersion": "",
				"event.involvedObject.kind":       "Pod",
				"event.involvedObject.name":       testpod.Name,
				"event.message":                   "Stopping container " + testpod.Spec.Containers[0].Name,
				"event.type":                      "Normal",
				"verb":                            "ADDED",
			},
		},
	} {
		if agentMock.Has(&event) {
			continue
		}

		e := json.NewEncoder(os.Stderr)
		t.Log("Expected:")
		e.Encode(event) // nolint:errcheck
		t.Fatalf("Event was not captured")
	}
}

// initialize returns a kubernets client and a mocked agent sink ready to receive events
func initialize(t *testing.T) (*kubernetes.Clientset, *e2e.MockedAgentSink) {
	t.Helper()

	conf, err := restConfig()
	if err != nil {
		t.Fatalf("could not build kubernetes config: %v", err)
	}

	client, err := kubernetes.NewForConfig(conf)
	if err != nil {
		t.Fatalf("could not build kubernetes client: %v", err)
	}

	sharedInformers := informers.NewSharedInformerFactory(client, time.Duration(0))
	eventsInformer := sharedInformers.Core().V1().Events().Informer()
	sharedInformers.Start(nil)
	sharedInformers.WaitForCacheSync(nil)
	for _, obj := range eventsInformer.GetStore().List() {
		_ = eventsInformer.GetStore().Delete(obj)
	}

	if globalMockedAgentSink == nil {
		globalMockedAgentSink = e2e.NewMockedAgentSink()
	}
	globalMockedAgentSink.ForgetEvents()

	router := events.NewRouter(eventsInformer, map[string]events.Sink{"mock": globalMockedAgentSink})
	go router.Run(nil)

	return client, globalMockedAgentSink
}

// restConfig attempts to build a k8s config from the environment, or the default kubeconfig path
func restConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, err
	}

	config, err = clientcmd.BuildConfigFromFlags("", path.Join(os.ExpandEnv("$HOME"), ".kube", "config"))
	if err == nil {
		return config, err
	}

	return nil, err
}
