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

	"k8s.io/api/apps/v1beta1"
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
	annotation       = "iam.cloud.google.com/account-name"
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

	// Watch uninitialized Deployments in all namespaces.
	restClient := clientset.AppsV1beta1().RESTClient()
	watchlist := cache.NewListWatchFromClient(restClient,
		"deployments", corev1.NamespaceAll, fields.Everything())

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
		&v1beta1.Deployment{},
		resyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				err := initializeDeployment(obj.(*v1beta1.Deployment), clientset)
				if err != nil {
					log.Println(err)
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

func initializeDeployment(deployment *v1beta1.Deployment, clientset *kubernetes.Clientset) error {
	if deployment.ObjectMeta.GetInitializers() != nil {
		pendingInitializers := deployment.ObjectMeta.GetInitializers().Pending

		if initializerName == pendingInitializers[0].Name {
			log.Printf("Initializing deployment/%s", deployment.Name)

			o, err := runtime.NewScheme().DeepCopy(deployment)
			if err != nil {
				return fmt.Errorf("failed to create a deepcopy of deployment: %+v", err)
			}
			initializedDeployment := o.(*v1beta1.Deployment)

			// Remove self from the list of pending Initializers while preserving ordering.
			if len(pendingInitializers) == 1 {
				initializedDeployment.ObjectMeta.Initializers = nil
			} else {
				initializedDeployment.ObjectMeta.Initializers.Pending = append(pendingInitializers[:0],
					pendingInitializers[1:]...)
			}

			serviceAccountName, ok := deployment.ObjectMeta.GetAnnotations()[annotation]
			if !ok {
				log.Printf("Required '%s' annotation missing; skipping initialization", annotation)
				_, err = clientset.AppsV1beta1().Deployments(deployment.Namespace).Update(initializedDeployment)
				if err != nil {
					return fmt.Errorf("failed to update initializers.pending field: %+v", err)
				}
				return nil
			}

			// Modify the Deployment's Pod template to inject an environment variable
			for i, c := range initializedDeployment.Spec.Template.Spec.Containers {
				volName := fmt.Sprintf("gcp-%s", serviceAccountName)
				mountPath := path.Join(secretMountPath, serviceAccountName)
				keyPath := path.Join(mountPath, serviceAccountFile)

				// volume
				initializedDeployment.Spec.Template.Spec.Volumes = append(initializedDeployment.Spec.Template.Spec.Volumes,
					corev1.Volume{
						Name: volName,
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: serviceAccountName,
								Items: []corev1.KeyToPath{
									{
										Key:  "key.json",
										Path: "key.json",
									},
								},
							},
						},
					})

				// volume mount
				initializedDeployment.Spec.Template.Spec.Containers[i].VolumeMounts = append(initializedDeployment.Spec.Template.Spec.Containers[i].VolumeMounts,
					corev1.VolumeMount{
						Name:      volName,
						MountPath: mountPath,
						SubPath:   "",
						ReadOnly:  true,
					})

				// env
				initializedDeployment.Spec.Template.Spec.Containers[i].Env = append(c.Env, corev1.EnvVar{
					Name:  "GOOGLE_APPLICATION_CREDENTIALS",
					Value: keyPath,
				})
			}

			oldData, err := json.Marshal(deployment)
			if err != nil {
				return fmt.Errorf("failed to marshal old deployment: %+v", err)
			}

			newData, err := json.Marshal(initializedDeployment)
			if err != nil {
				return fmt.Errorf("failed to marshal new deployment: %+v", err)
			}

			patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, v1beta1.Deployment{})
			if err != nil {
				return fmt.Errorf("failed to create 2-way merge patch: %+v", err)
			}

			if _, err = clientset.AppsV1beta1().Deployments(deployment.Namespace).Patch(deployment.Name, types.StrategicMergePatchType, patchBytes); err != nil {
				return fmt.Errorf("failed to patch deployment: %+v", err)
			}
			log.Printf("Patched deployment/%s", deployment.Name)
		}
	}
	return nil
}
