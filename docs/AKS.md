# Running on real GPUs (Azure Kubernetes Service)

> Status: written from the AKS and NVIDIA GPU Operator documentation. The project
> has been validated with simulated GPUs (unit, envtest, kind) but **not yet on a
> physical GPU node**. Update this note once you run it.

GPU VMs cost money by the hour. Delete the node pool as soon as you finish (last step).

```bash
RG=sentinel-rg; CLUSTER=sentinel-aks; LOC=eastus
az group create -n $RG -l $LOC
az aks create -g $RG -n $CLUSTER --node-count 1 --generate-ssh-keys
az aks get-credentials -g $RG -n $CLUSTER

# A small GPU pool. Check your subscription's GPU quota first.
az aks nodepool add -g $RG --cluster-name $CLUSTER -n gpu \
  --node-count 1 --node-vm-size Standard_NC4as_T4_v3 \
  --node-taints sku=gpu:NoSchedule

# NVIDIA GPU Operator installs the driver, the device plugin, GPU Feature
# Discovery (the nvidia.com/gpu.present label) and dcgm-exporter.
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
helm install gpu-operator nvidia/gpu-operator -n gpu-operator --create-namespace

# Sentinel with the real DCGM source. If dcgm-exporter is not reachable on the
# node IP, set agent.dcgmURL to its pod or Service address instead.
helm install sentinel deploy/helm/gpu-fleet-sentinel -n gpu-sentinel --create-namespace \
  --set agent.source=dcgm --set image.tag=<release>

kubectl get nodes -L nvidia.com/gpu.present
kubectl describe node <gpu-node> | grep -A3 GPUHealthy
```

Optional incident summaries via Azure OpenAI:

```bash
kubectl -n gpu-sentinel create secret generic llm --from-literal=LLM_API_KEY=<key>
helm upgrade sentinel deploy/helm/gpu-fleet-sentinel -n gpu-sentinel --reuse-values \
  --set controller.llm.baseURL=https://<resource>.openai.azure.com \
  --set controller.llm.azureDeployment=<deployment> \
  --set controller.llm.apiKeySecret=llm
```

Clean up:

```bash
az group delete -n $RG --yes --no-wait
```
