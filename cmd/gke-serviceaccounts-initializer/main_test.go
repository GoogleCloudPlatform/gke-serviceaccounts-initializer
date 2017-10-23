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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

func Test_needsInitialization(t *testing.T) {
	tests := []struct {
		name string
		in   *corev1.Pod
		want bool
	}{
		{"initialized pod", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo"}}, false},
		{"empty pending list", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo",
				Initializers: &metav1.Initializers{
					Pending: []metav1.Initializer{}}}}, false},
		{"uninitialized, but not our turn", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo",
				Initializers: &metav1.Initializers{Pending: []metav1.Initializer{{Name: "a.b.c"}}}}}, false},
		{"uninitialized, our turn", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo",
				Initializers: &metav1.Initializers{Pending: []metav1.Initializer{
					{Name: "serviceaccounts.cloud.google.com"}}}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsInitialization(tt.in); got != tt.want {
				t.Errorf("needsInitialization() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_removeSelfPendingInitializer(t *testing.T) {
	tests := []struct {
		name string
		in   *corev1.Pod
		want *corev1.Pod
	}{
		{"nil initializers",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}},
		{"nil pending",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{Pending: nil}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{Pending: nil}}}},
		{"1 pending becomes nil",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{
				Pending: []metav1.Initializer{
					{Name: "a.b.c"}}}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{
				Pending: nil}}}},
		{"first one removed",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{
				Pending: []metav1.Initializer{
					{Name: "a.b.c"},
					{Name: "d.e.f"}}}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Initializers: &metav1.Initializers{
				Pending: []metav1.Initializer{
					{Name: "d.e.f"}}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removeSelfPendingInitializer(tt.in)
			assert.Equal(t, tt.want, tt.in)
		})
	}
}

func Test_modifyPodSpec(t *testing.T) {

	tests := []struct {
		name     string
		in       *corev1.Pod
		want     *corev1.Pod
		modified bool
	}{
		{"nil pod", nil, nil, false},
		{"no container pod",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}, false},
		{"1 container pod, no annotation",
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c1",
						Image: "i1"}}}},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c1",
						Image: "i1"}}}}, false},
		{"1 container pod, with annotation",
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
					Annotations: map[string]string{
						"foo": "bar",
						"iam.cloud.google.com/service-account": "sa-1",
					}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c1",
						Image: "i1"}}}},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
					Annotations: map[string]string{
						"foo": "bar",
						"iam.cloud.google.com/service-account": "sa-1",
					}},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "gcp-sa-1",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "sa-1",
									Items: []corev1.KeyToPath{
										{
											Key:  "key.json",
											Path: "key.json",
										},
									}},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "i1",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "gcp-sa-1",
									ReadOnly:  true,
									MountPath: "/var/run/secrets/gcp/sa-1",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "GOOGLE_APPLICATION_CREDENTIALS",
									Value: "/var/run/secrets/gcp/sa-1/key.json",
								},
							},
						},
					}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modifyPodSpec(tt.in); got != tt.modified {
				t.Errorf("modifyPodSpec() = %v, want %v", got, tt.modified)
			}
			assert.Equal(t, tt.in, tt.want, "wrong injection")
		})
	}
}
