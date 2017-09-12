# GKE Service Accounts Initializer

This add-on configures Kubernetes Pods with the Google Cloud Service Accounts
credentials that you previously imported to the cluster based on an annotation.

Install this initializer to your cluster and add the following annotation to
metadata.annotations field:

```yaml
annotations:
  iam.cloud.google.com/service-account: "[SECRET-NAME]"
```

## Quickstart

Create an alpha cluster on GKE (Initializers feature is not beta until v1.9):

    gcloud container clusters create test-cluster \
        --enable-kubernetes-alpha \
        --cluster-version 1.7.5

Clone this repository, and deploy the initializer to `kube-system` namespace:

```sh
kubectl apply -f kube/
```
```
deployment "gke-serviceaccounts-initializer" created
initializerconfiguration "gke-serviceaccounts" created
```

Import a _fake_ service account file as a Secret named `foo`:

    kubectl create secret generic foo --from-literal=key.json=I_AM_FAKE

Next, create a Deployment that specified the annotation in the Pod spec:

```yaml
apiVersion: apps/v1beta1
kind: Deployment
metadata:
  name: nginx-inject-demo
spec:
  template:
    metadata:
      annotations:
        iam.cloud.google.com/service-account: foo
      labels:
        app: nginx
    spec:
      containers:
      - name: web
        image: nginx
```

Save this to `nginx.yaml` and run:

```
kubectl apply -f nginx.yaml
```
```
deployment "nginx-inject-demo" created
```

Query the pods, verify the pod has started:

```
kubectl get pods
```
```
nginx-inject-demo-6577b68687-2lvj8   1/1       Running   0          25s
```

Query the pod object and note that:
- the Secret has been injected as a volume
- the volume has been mounted into the Pod
- `GOOGLE_APPLICATION_CREDENTIALS` environment variable is created to point
  to the GCP Service Account credentials file:

```
kubectl get pods -l app=nginx -o=yaml
```
```yaml
- apiVersion: v1
  kind: Pod
  metadata:
    name: nginx-inject-demo-6577b68687-2lvj8
    namespace: default
    annotations:
      iam.cloud.google.com/service-account: foo
    labels:
      app: nginx
  # ...
  # ...
  spec:
    containers:
    - name: web
      image: nginx
    volumes:
    - name: gcp-foo
      secret:
        secretName: foo
        defaultMode: 420
      # (... + default volumes)
    volumeMounts:
      - mountPath: /var/run/secrets/gcp/foo
        name: gcp-foo
        readOnly: true
      # (... + default volumeMounts)
    env:
    - name: GOOGLE_APPLICATION_CREDENTIALS
      value: /var/run/secrets/gcp/foo/key.json
    # ...
    # ...
```
