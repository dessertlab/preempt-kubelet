# preempt-kubelet (Kubernetes v1.29.15)

Kubelet modificata, basata su Kubernetes `v1.29.15`. Ordina i pod in arrivo in
base alla label `criticality` (da `1` a `80`, dove `1` è il più critico) ed
esegue il loro `SyncPod` in `SCHED_FIFO` con priorità `81 - criticality`. I pod
senza label restano invariati.

## Modifiche

| file | modifica |
|------|----------|
| `pkg/kubelet/rtsched/rtsched.go` | **nuovo**: lettura della label `criticality`, ordinamento dei pod, esecuzione di una funzione in `SCHED_FIFO` su un thread OS bloccato |
| `pkg/kubelet/config/apiserver.go` | ordinamento per criticità dei pod in arrivo dall'API server; gating dei pod di test; parametri letti da variabili d'ambiente |
| `pkg/kubelet/kubelet.go` | `HandlePodAdditions` non riordina più i pod per data di creazione |
| `pkg/kubelet/pod_workers.go` | `SyncPod` eseguito tramite `rtsched.RunWithCriticality` |
| `pkg/kubelet/config/apiserver_env_test.go` | **nuovo**: test della lettura delle variabili d'ambiente |

Il **gating** trattiene i pod del namespace di test finché non sono arrivati
tutti quelli attesi (`pod-prio<P>-<R>`), poi li consegna insieme alla kubelet,
così l'ordinamento agisce sull'intero gruppo.

## Compilazione e installazione

Serve un cluster con control plane **1.29** e un worker su cui è già installata
la kubelet 1.29 standard (join fatto con kubeadm).

```bash
make WHAT=cmd/kubelet                # Go 1.23.6 (.go-version)
_output/bin/kubelet --version        # Kubernetes v1.29.15
```

Sul worker:

```bash
sudo systemctl stop kubelet
sudo cp /usr/bin/kubelet /usr/bin/kubelet.orig       # backup
sudo cp ~/kubelet /usr/bin/kubelet
sudo systemctl start kubelet
```

## Test ripetuti

I parametri del test si impostano in `/etc/default/kubelet` sul worker, senza
ricompilare. Si applicano con `sudo systemctl restart kubelet`.

```bash
KUBELET_EXTRA_ARGS=--v=4      # necessario per i log di ordinamento e gating
RT_ORDERING=true              # ordinamento per criticità
RT_GATING=true                # gating dei pod di test
RT_NAMESPACE=test-priority    # namespace del test
RT_PRIORITIES=8               # livelli di criticità (P)
RT_REPLICAS=5                 # repliche per livello (R)
```

I valori mostrati sono quelli di default. All'avvio la kubelet scrive nel log i
parametri in uso (`RT test settings`).

**1. Creare i pod** (`P × R` pod, assegnati al worker):

```bash
NODE=<nome-worker>; P=8; R=5
kubectl create namespace test-priority
for p in $(seq 1 $P); do for r in $(seq 1 $R); do cat <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: pod-prio$p-$r, namespace: test-priority, labels: {criticality: "$p"}}
spec: {nodeName: $NODE, containers: [{name: c, image: busybox, command: [sleep, "3600"]}]}
EOF
done; done | kubectl apply -f -
```

**2. Osservare**, sul worker:

```bash
journalctl -u kubelet -f | grep -E "Releasing apiserver|SyncLoop ADD|Failed to set FIFO"
watch -n0.2 "ps -L -o tid,cls,rtprio,comm -p \$(pidof kubelet) | grep -w FF"
```

- `Releasing apiserver pod update`: sono arrivati tutti i pod, il gate si apre.
- `SyncLoop ADD`: i pod nell'ordine in cui la kubelet li processa.
- thread `FF` con `rtprio` da 80 (criticità 1) in giù: `SyncPod` in `SCHED_FIFO`.

**3. Pulire prima del test successivo:**

```bash
kubectl delete pods -n test-priority --all --grace-period=0 --force
```

La cancellazione forzata evita che i pod restino bloccati: con il gating attivo,
durante la cancellazione la kubelet può perdere di vista i pod rimasti.

**4. Cambiare parametri** (opzionale): modifica `/etc/default/kubelet`,
lancia `sudo systemctl restart kubelet` e ricomincia dal punto 1, con `P` e `R`
uguali a `RT_PRIORITIES` e `RT_REPLICAS`.
