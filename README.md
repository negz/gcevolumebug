# GCE Volume Bug

This code attempts to trigger a sporadic bug we've observed on CoreOS in which
SCSI attached GCE local SSDs begin timing out all writes. This bug is seemingly
triggered by the Kubernetes Kubelet attaching and mounting pod volumes.
