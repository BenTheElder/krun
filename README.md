# krun

Execute commands in parallel in Pods

## Example

```sh
$ kubectl apply -f examples/statefulset.yaml
statefulset.apps/krun-test-web created
```

```sh
$ kubectl wait --for=condition=Ready pod/krun-test-web-0 pod/krun-test-web-1 --timeout=60s
pod/krun-test-web-0 condition met
pod/krun-test-web-1 condition met
```

```sh
$ make build
Building all binaries...
GOOS= GOARCH= go build -o ./bin/krun ./krun
```

```sh
./bin/krun   --namespace="default"   --label-selector="app=krun-test"   --command="hostname"
[krun-test-web-1] krun-test-web-1
[krun-test-web-0] krun-test-web-0
```