#!/usr/bin/env bash

# Helper function to wait for a pod to be in Ready state
# Usage: wait_for_pod_ready <namespace> <pod_name>
wait_for_pod_ready() {
    local ns="$1"
    local pod_name="$2"
    echo -n "Waiting for pod $pod_name in $ns to be ready..."
    
    # Use kubectl wait for robust checking
    run kubectl wait --for=condition=Ready pod "$pod_name" -n "$ns" --timeout=120s
    
    if [ "$status" -ne 0 ]; then
        echo "FAIL"
        echo "Pod $pod_name failed to get ready:"
        kubectl describe pod "$pod_name" -n "$ns"
        return 1
    else
        echo "OK"
        return 0
    fi
}
