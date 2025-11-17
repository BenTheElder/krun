#!/usr/bin/env bats

load 'common'

E2E_TEST_NAMESPACE="ingress-e2e-test"
export E2E_TEST_NAMESPACE

setup_file() {
  export REGISTRY="registry.k8s.io/networking"
  export IMAGE_NAME="kube-ingress"
  export TAG="test"

  # Build the image for the standard binary and amd64 architecture
  (
    cd "$BATS_TEST_DIRNAME"/..
    TAG="$TAG" make image-build
  )

  # Load the Docker image into the kind cluster
  kind load docker-image "$REGISTRY/$IMAGE_NAME:$TAG" --name "$CLUSTER_NAME"

  _install=$(sed -e "s#$REGISTRY/$IMAGE_NAME.*#$REGISTRY/$IMAGE_NAME:$TAG#" -e "s/--v=2/--v=4/" < "$BATS_TEST_DIRNAME"/../install.yaml)
  printf '%s' "${_install}" | kubectl apply -f -

  echo "Waiting for kube-ingress deployment to be created..."
  kubectl wait --for=condition=Available --timeout=60s deployment/kube-ingress -n kube-ingress || true
  
  # Then wait for pods to be ready
  echo "Waiting for kube-ingress pods to be ready..."
  kubectl wait --for=condition=ready --timeout=60s pods --namespace=kube-ingress -l app=kube-ingress

  kubectl create namespace "$E2E_TEST_NAMESPACE"
  
  kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/00-base-setup.yaml"
  
  echo "Waiting for foo-app, bar-app, and curl-pod to be ready..."
  wait_for_pod_ready "$E2E_TEST_NAMESPACE" "foo-app"
  wait_for_pod_ready "$E2E_TEST_NAMESPACE" "bar-app"
  wait_for_pod_ready "$E2E_TEST_NAMESPACE" "curl-pod"

  echo "Finding kube-ingress ClusterIP..."
  run kubectl get service kube-ingress -n kube-ingress -o jsonpath='{.spec.clusterIP}'
  [ "$status" -eq 0 ]
  export INGRESS_SVC_IP="$output"
  echo "Ingress Controller ClusterIP: $INGRESS_SVC_IP"
}

teardown_file() {
  sleep 2000
  # Delete the test namespace
  kubectl delete namespace "$E2E_TEST_NAMESPACE" --wait=true
  _install=$(sed -e "s#$REGISTRY/$IMAGE_NAME.*#$REGISTRY/$IMAGE_NAME:$TAG#" -e "s/--v=2/--v=4/" < "$BATS_TEST_DIRNAME"/../install.yaml)
  printf '%s' "${_install}" | kubectl delete -f -
}

@test "[E2E] PathType: Prefix" {
    echo "Applying Ingress with PathTypePrefix..."
    run kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/01-ingress-prefix.yaml"
    [ "$status" -eq 0 ]
    
    # Give the controller time to reconcile
    echo "Waiting for reconciliation..."
    sleep 5 

    # Test /foo prefix
    echo "Testing /foo prefix (should match foo-app)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s "http://$INGRESS_SVC_IP/foo/some/path"
    [ "$status" -eq 0 ]
    [[ "$output" == "foo-app" ]]

    # Test /bar prefix
    echo "Testing /bar prefix (should match bar-app)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s "http://$INGRESS_SVC_IP/bar/another"
    [ "$status" -eq 0 ]
    [[ "$output" == "bar-app" ]]
    
    kubectl delete -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/01-ingress-prefix.yaml"
}

@test "[E2E] PathType: Exact" {
    echo "Applying Ingress with PathTypeExact..."
    run kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/02-ingress-exact.yaml"
    [ "$status" -eq 0 ]

    echo "Waiting for reconciliation..."
    sleep 5

    # Test exact path
    echo "Testing /foo-exact path (should match foo-app)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s "http://$INGRESS_SVC_IP/foo-exact"
    [ "$status" -eq 0 ]
    [[ "$output" == "foo-app" ]]

    # Test that a sub-path does NOT match (should 404)
    echo "Testing /foo-exact/subpath (should 404)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s -o /dev/null -w "%{http_code}" "http://$INGRESS_SVC_IP/foo-exact/subpath"
    [ "$status" -eq 0 ]
    [[ "$output" == "404" ]]

    kubectl delete -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/02-ingress-exact.yaml"
}

@test "[E2E] PathType: ImplementationSpecific" {
    echo "Applying Ingress with PathTypeImplementationSpecific..."
    run kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/03-ingress-impl-specific.yaml"
    [ "$status" -eq 0 ]

    echo "Waiting for reconciliation..."
    sleep 5

    # Our controller explicitly skips this path type, so it should 404
    echo "Testing /impl-specific path (should 404)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s -o /dev/null -w "%{http_code}" "http://$INGRESS_SVC_IP/impl-specific"
    [ "$status" -eq 0 ]
    [[ "$output" == "404" ]]
    
    kubectl delete -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/03-ingress-impl-specific.yaml"
}

@test "[E2E] TLS Termination" {
    echo "Generating self-signed certificate for TLS test..."
    run openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/tls.key -out /tmp/tls.crt -subj "/CN=tls.example.com"
    [ "$status" -eq 0 ]

    echo "Creating TLS secret..."
    run kubectl create secret tls tls-secret --key /tmp/tls.key --cert /tmp/tls.crt -n "$E2E_TEST_NAMESPACE"
    [ "$status" -eq 0 ]

    echo "Applying Ingress with TLS..."
    run kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/04-ingress-tls.yaml"
    [ "$status" -eq 0 ]
    
    echo "Waiting for reconciliation..."
    sleep 10 # Give more time for TLS setup

    echo "Testing HTTPS endpoint for tls.example.com..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s --insecure --resolve "tls.example.com:443:$INGRESS_SVC_IP" "https://tls.example.com"
    [ "$status" -eq 0 ]
    [[ "$output" == "foo-app" ]]

    echo "Cleaning up TLS resources..."
    kubectl delete ingress tls-ingress -n "$E2E_TEST_NAMESPACE"
    kubectl delete secret tls-secret -n "$E2E_TEST_NAMESPACE"
    rm /tmp/tls.key /tmp/tls.crt
}

@test "[E2E] Wildcard Host" {
    echo "Applying Ingress with wildcard host..."
    run kubectl apply -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/05-ingress-wildcard.yaml"
    [ "$status" -eq 0 ]

    echo "Waiting for reconciliation..."
    sleep 5

    echo "Testing foo.wild.example.com (should match bar-app)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s -H "Host: foo.wild.example.com" "http://$INGRESS_SVC_IP/"
    [ "$status" -eq 0 ]
    [[ "$output" == "bar-app" ]]

    echo "Testing another.wild.example.com (should match bar-app)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s -H "Host: another.wild.example.com" "http://$INGRESS_SVC_IP/"
    [ "$status" -eq 0 ]
    [[ "$output" == "bar-app" ]]

    echo "Testing non.matching.com (should 404)..."
    run kubectl exec -n "$E2E_TEST_NAMESPACE" curl-pod -- curl -s -o /dev/null -w "%{http_code}" -H "Host: non.matching.com" "http://$INGRESS_SVC_IP/"
    [ "$status" -eq 0 ]
    [[ "$output" == "404" ]]
    
    kubectl delete -n "$E2E_TEST_NAMESPACE" -f "$BATS_TEST_DIRNAME/05-ingress-wildcard.yaml"
}
