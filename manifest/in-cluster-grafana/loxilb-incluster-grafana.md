## How to deploy Grafana/Prometheus-based LoxiLB Monitoring System

### Prerequisites

Ensure you have a Kubernetes cluster running and `kubectl` configured to interact with it.

### Steps to Deploy

1. **Create the Monitoring Namespace**

    ```sh
    kubectl apply -f https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/in-cluster-grafana/loxilb-collector.yaml
    ```


2. **Deploy Grafana**

    ```sh
    kubectl apply -f https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/in-cluster-grafana/loxilb-grafana.yaml
    ```

3. **Deploy Prometheus**

    ```sh
    kubectl apply -f https://github.com/loxilb-io/kube-loxilb/raw/main/manifest/in-cluster-grafana/loxilb-collector.yaml
    ```

4. **Verify Deployments**

    Check the status of the deployments to ensure all pods are running:

    ```sh
    kubectl get pods -n monitoring
    ```

    You should see pods for Grafana, Loki, and Prometheus running.

5. **Access Grafana**

    Grafana is exposed via a LoadBalancer service. You can get the external IP of the Grafana service using:

    ```sh
    kubectl get svc -n monitoring grafana-llb
    ```

    Open the external IP in your browser to access the Grafana dashboard.

### Configuring Dashboards

Grafana is pre-configured with dashboards for monitoring LoxiLB. You can find these dashboards under the "Dashboards" section in Grafana.

### Conclusion

By following these steps, you should have a fully functional Grafana/Prometheus-based monitoring system for LoxiLB running in your Kubernetes cluster.
