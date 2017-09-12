// Copyright 2017 Google Inc. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	annotation       = "iam.cloud.google.com/service-account"
	initializerName  = "serviceaccounts.cloud.google.com"
	defaultNamespace = "default"
	resyncPeriod     = 30 * time.Second

	secretMountPath    = "/var/run/secrets/gcp/"
	serviceAccountFile = "key.json"
)

type config struct {
	Containers []corev1.Container
	Volumes    []corev1.Volume
}

func main() {
	log.Println("Starting the GCP Service accounts initializer...")

	log.Println("Using in-cluster token discovery")
	clusterConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("failed to use in-cluster token: %+v", err)
		kubecfg := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		log.Printf("Using kubeconfig file at %s", kubecfg)
		clusterConfig, err = clientcmd.BuildConfigFromFlags("", kubecfg)
		if err != nil {
			log.Printf("failed to find kubeconfig file: %+v", err)
			log.Fatal("No authentication is available.")
		}
	}

	clientset, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		log.Fatalf("failed to initialize kubernetes client: %+v", err)
	}

	// Watch uninitialized Pods in all namespaces.
	restClient := clientset.CoreV1().RESTClient()
	watchlist := cache.NewListWatchFromClient(restClient,
		"pods", corev1.NamespaceAll, fields.Everything())

	// Wrap the returned watchlist to workaround the inability to include
	// the `IncludeUninitialized` list option when setting up watch clients.
	includeUninitializedWatchlist := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.IncludeUninitialized = true
			return watchlist.List(options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.IncludeUninitialized = true
			return watchlist.Watch(options)
		},
	}

	_, controller := cache.NewInformer(includeUninitializedWatchlist,
		&corev1.Pod{},
		resyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					log.Fatalf("watch returned non-pod object: %T", pod)
				}

				if !needsInitialization(pod) {
					log.Printf("skipping pod/%s", pod.GetName())
					return
				}

				modifiedPod, err := clonePod(pod)
				if err != nil {
					log.Printf("error cloning pod object: %+v", err)
				}

				if !modifyPodSpec(modifiedPod) {
					log.Printf("no injection in pod/%s", pod.GetName())
				}

				removeSelfPendingInitializer(modifiedPod)

				if err := patchPod(pod, modifiedPod, clientset); err != nil {
					log.Printf("error saving pod/%s: %+v", pod.GetName(), err)
				} else {
					log.Printf("initialized pod/%s", pod.GetName())
				}
			},
		},
	)

	stop := make(chan struct{})
	go controller.Run(stop)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	log.Println("Shutdown signal received, exiting...")
	close(stop)
}

// needsInitialization determines if the pod is required to be initialized
// currently by this initializer.
func needsInitialization(pod *corev1.Pod) bool {
	initializers := pod.ObjectMeta.GetInitializers()
	return initializers != nil &&
		len(initializers.Pending) > 0 &&
		initializers.Pending[0].Name == initializerName
}

// clonePod creates a deep copy of pod for modification.
func clonePod(pod *corev1.Pod) (*corev1.Pod, error) {
	o, err := runtime.NewScheme().DeepCopy(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to deepcopy: %+v", err)
	}
	p, ok := o.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("cloned object is not a Pod: %T", p)
	}
	return p, nil
}

// removeSelfPendingInitializer removes the first element from pending
// initializers list of in-memory pod value.
func removeSelfPendingInitializer(pod *corev1.Pod) {
	if pod.ObjectMeta.GetInitializers() == nil {
		return
	}
	pendingInitializers := pod.ObjectMeta.GetInitializers().Pending
	if len(pendingInitializers) == 1 {
		pod.ObjectMeta.Initializers.Pending = nil
	} else if len(pendingInitializers) > 1 {
		pod.ObjectMeta.Initializers.Pending = append(
			pendingInitializers[:0], pendingInitializers[1:]...)
	}
}

// patchPod saves the pod to the API using a strategic 2-way JSON merge patch.
func patchPod(origPod, newPod *corev1.Pod, clientset *kubernetes.Clientset) error {
	origData, err := json.Marshal(origPod)
	if err != nil {
		return fmt.Errorf("failed to marshal original pod: %+v", err)
	}

	newData, err := json.Marshal(newPod)
	if err != nil {
		return fmt.Errorf("failed to marshal modified pod: %+v", err)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(origData, newData, corev1.Pod{})
	if err != nil {
		return fmt.Errorf("failed to create 2-way merge patch: %+v", err)
	}

	if _, err = clientset.CoreV1().Pods(origPod.GetNamespace()).Patch(
		origPod.GetName(), types.StrategicMergePatchType, patch); err != nil {
		return fmt.Errorf("failed to patch pod/%s: %+v", origPod.GetName(), err)
	}
	return nil
}

// modifyPodSpec makes modifications to in-memory pod value to inject the
// service account. Returns whether any modifications have been made.
func modifyPodSpec(pod *corev1.Pod) bool {
	if pod == nil || pod.ObjectMeta.Annotations == nil {
		return false
	}
	serviceAccountName, ok := pod.ObjectMeta.Annotations[annotation]
	if !ok {
		return false
	}

	for i, c := range pod.Spec.Containers {
		volName := fmt.Sprintf("gcp-%s", serviceAccountName)
		mountPath := path.Join(secretMountPath, serviceAccountName)
		keyPath := path.Join(mountPath, serviceAccountFile)

		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: serviceAccountName,
						Items: []corev1.KeyToPath{{
							Key:  "key.json",
							Path: "key.json",
						}}}}})

		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      volName,
				MountPath: mountPath,
				SubPath:   "",
				ReadOnly:  true})

		pod.Spec.Containers[i].Env = append(c.Env, corev1.EnvVar{
			Name:  "GOOGLE_APPLICATION_CREDENTIALS",
			Value: keyPath})
	}

	return true
}
