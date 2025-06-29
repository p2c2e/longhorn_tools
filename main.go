package main

// Make the copy command take into account the src/dst namespaces AI?

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var version = "dev"

type VolumeManager struct {
	clientset     *kubernetes.Clientset
	dynamicClient dynamic.Interface
}

type LonghornVolume struct {
	Name   string `json:"name"`
	Size   string `json:"size"`
	State  string `json:"state"`
	PVName string `json:"kubernetesStatus.pvName"`
}

func NewVolumeManager() (*VolumeManager, error) {
	config, err := (&VolumeManager{}).getConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	return &VolumeManager{
		clientset:     clientset,
		dynamicClient: dynamicClient,
	}, nil
}

func (vm *VolumeManager) ListVolumes(namespace string) error {
	volumes, err := vm.getLonghornVolumes()
	if err != nil {
		return fmt.Errorf("failed to list Longhorn volumes: %v", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tSIZE\tPV_BOUND")

	for _, volume := range volumes {
		pvBound := "No"
		if volume.PVName != "" {
			pvBound = "Yes"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			volume.Name,
			volume.State,
			volume.Size,
			pvBound)
	}

	w.Flush()
	return nil
}

func (vm *VolumeManager) isVolumeInUse(pvName, namespace string) (bool, error) {
	// Get all PVCs in the namespace
	pvcs, err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list PVCs: %v", err)
	}

	// Find PVC bound to this PV
	var targetPVC string
	for _, pvc := range pvcs.Items {
		if pvc.Spec.VolumeName == pvName && pvc.Status.Phase == corev1.ClaimBound {
			targetPVC = pvc.Name
			break
		}
	}

	if targetPVC == "" {
		return false, nil // No PVC bound to this PV
	}

	// Check if any running pod is using this PVC
	pods, err := vm.clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == targetPVC {
					return true, nil // Volume is in use
				}
			}
		}
	}

	return false, nil // Volume is not in use
}

func (vm *VolumeManager) findExistingPodForVolume(pvName, namespace string) (podName, mountPath, containerName string, err error) {
	// Get all PVCs in the namespace
	pvcs, err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to list PVCs: %v", err)
	}

	// Find PVC bound to this PV
	var targetPVC string
	for _, pvc := range pvcs.Items {
		if pvc.Spec.VolumeName == pvName && pvc.Status.Phase == corev1.ClaimBound {
			targetPVC = pvc.Name
			break
		}
	}

	if targetPVC == "" {
		return "", "", "", fmt.Errorf("no PVC found for PV %s", pvName)
	}

	// Find the pod using this PVC
	pods, err := vm.clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == targetPVC {
					// Find the mount path and container
					for _, container := range pod.Spec.Containers {
						for _, mount := range container.VolumeMounts {
							if mount.Name == volume.Name {
								return pod.Name, mount.MountPath, container.Name, nil
							}
						}
					}
				}
			}
		}
	}

	return "", "", "", fmt.Errorf("no running pod found using PVC %s", targetPVC)
}

func (vm *VolumeManager) createSnapshotBasedAccess(volumeName, namespace, storageClass string) (podName, mountPath, containerName string, err error) {
	// For now, we'll create a temporary volume with ReadWriteMany access mode
	// In a full implementation, this would create a Longhorn snapshot and restore it to a new volume

	fmt.Printf("Creating temporary RWX volume for multi-attach access to %s...\n", volumeName)

	// Create a temporary volume name
	tempVolumeName := fmt.Sprintf("lhc-temp-rwx-%s", volumeName)

	// Get original volume info for sizing
	volume, err := vm.getLonghornVolume(volumeName)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get original volume info: %v", err)
	}

	// Create temporary PV with RWX access mode
	_, err = vm.createTemporaryRWXPV(tempVolumeName, namespace, storageClass, volume.Size)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create temporary RWX PV: %v", err)
	}

	// Create temporary pod using the RWX volume
	return vm.createTemporaryPodForRWXVolume(tempVolumeName, namespace, storageClass, volume.Size)
}

func (vm *VolumeManager) createTemporaryRWXPV(volumeName, namespace, storageClass, size string) (string, error) {
	pvName := fmt.Sprintf("lhc-temp-pv-%s", volumeName)

	// Check if PV already exists
	_, err := vm.clientset.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err == nil {
		return pvName, nil // PV already exists
	}

	// Create temporary PV with ReadWriteMany access mode
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"app": "lhc-temp",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(size),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany, // Use RWX to avoid multi-attach issues
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "driver.longhorn.io",
					VolumeHandle: volumeName, // Create a new volume handle
					FSType:       "ext4",
					VolumeAttributes: map[string]string{
						"numberOfReplicas":    "1", // Use fewer replicas for temp volume
						"staleReplicaTimeout": "2880",
					},
				},
			},
		},
	}

	_, err = vm.clientset.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create temporary RWX PV: %v", err)
	}

	return pvName, nil
}

func (vm *VolumeManager) createTemporaryPodForRWXVolume(volumeName, namespace, storageClass, size string) (podName, mountPath, containerName string, err error) {
	// Create a temporary PVC for this volume if it doesn't exist
	pvcName := fmt.Sprintf("lhc-temp-pvc-%s", volumeName)
	mountPath = "/mnt/volume"
	containerName = "temp-container"
	podName = fmt.Sprintf("lhc-temp-pod-%s", volumeName)
	pvName := fmt.Sprintf("lhc-temp-pv-%s", volumeName)

	// Check if temporary PVC already exists
	_, err = vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		// Create temporary PVC with ReadWriteMany access mode
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: namespace,
				Labels: map[string]string{
					"app": "lhc-temp",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany, // Use RWX access mode
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
				StorageClassName: func() *string { return &storageClass }(),
				VolumeName:       pvName, // Bind to specific PV
			},
		}

		_, err = vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
		if err != nil {
			return "", "", "", fmt.Errorf("failed to create temporary PVC: %v", err)
		}

		// Wait for PVC to be bound
		fmt.Printf("Waiting for PVC %s to be bound...\n", pvcName)
		for i := 0; i < 60; i++ { // Wait up to 60 seconds
			pvc, err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvcName, metav1.GetOptions{})
			if err != nil {
				return "", "", "", fmt.Errorf("failed to get PVC status: %v", err)
			}

			if pvc.Status.Phase == corev1.ClaimBound {
				fmt.Printf("PVC %s is now bound to PV %s\n", pvcName, pvc.Spec.VolumeName)
				break
			}

			time.Sleep(1 * time.Second)
		}
	}

	// Check if temporary pod already exists and is running
	existingPod, err := vm.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err == nil && existingPod.Status.Phase == corev1.PodRunning {
		return podName, mountPath, containerName, nil
	}

	// Create temporary pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "lhc-temp",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  containerName,
					Image: "busybox:latest",
					Command: []string{
						"sleep",
						"3600", // Sleep for 1 hour
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "volume",
							MountPath: mountPath,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "volume",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err = vm.clientset.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create temporary pod: %v", err)
	}

	// Wait for pod to be running
	fmt.Printf("Waiting for temporary pod %s to be ready...\n", podName)
	for i := 0; i < 120; i++ { // Wait up to 2 minutes
		pod, err := vm.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return "", "", "", fmt.Errorf("failed to get pod status: %v", err)
		}

		if pod.Status.Phase == corev1.PodRunning {
			return podName, mountPath, containerName, nil
		}

		time.Sleep(1 * time.Second)
	}

	return "", "", "", fmt.Errorf("temporary pod %s did not become ready in time", podName)
}

func (vm *VolumeManager) CleanupTemporaryResources(namespace string) error {
	fmt.Printf("Searching for temporary resources with 'lhc-temp-' prefix in namespace '%s'...\n\n", namespace)

	// Find temporary pods
	pods, err := vm.clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=lhc-temp",
	})
	if err != nil {
		return fmt.Errorf("failed to list temporary pods: %v", err)
	}

	// Find temporary PVCs
	pvcs, err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=lhc-temp",
	})
	if err != nil {
		return fmt.Errorf("failed to list temporary PVCs: %v", err)
	}

	// Find temporary PVs (cluster-wide)
	pvs, err := vm.clientset.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=lhc-temp",
	})
	if err != nil {
		return fmt.Errorf("failed to list temporary PVs: %v", err)
	}

	// Check if any resources were found
	totalResources := len(pods.Items) + len(pvcs.Items) + len(pvs.Items)
	if totalResources == 0 {
		fmt.Println("No temporary resources found.")
		return nil
	}

	// Display found resources
	fmt.Printf("Found %d temporary resources:\n\n", totalResources)

	if len(pods.Items) > 0 {
		fmt.Println("Pods:")
		for _, pod := range pods.Items {
			fmt.Printf("  - %s (Status: %s)\n", pod.Name, pod.Status.Phase)
		}
		fmt.Println()
	}

	if len(pvcs.Items) > 0 {
		fmt.Println("PersistentVolumeClaims:")
		for _, pvc := range pvcs.Items {
			fmt.Printf("  - %s (Status: %s)\n", pvc.Name, pvc.Status.Phase)
		}
		fmt.Println()
	}

	if len(pvs.Items) > 0 {
		fmt.Println("PersistentVolumes:")
		for _, pv := range pvs.Items {
			fmt.Printf("  - %s (Status: %s)\n", pv.Name, pv.Status.Phase)
		}
		fmt.Println()
	}

	// Ask for confirmation
	fmt.Print("Do you want to delete these resources? (y/N): ")
	var response string
	fmt.Scanln(&response)

	if response != "y" && response != "Y" {
		fmt.Println("Cleanup cancelled.")
		return nil
	}

	// Delete resources
	fmt.Println("\nDeleting resources...")

	// Delete pods first
	for _, pod := range pods.Items {
		fmt.Printf("Deleting pod %s...\n", pod.Name)
		err := vm.clientset.CoreV1().Pods(namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Warning: failed to delete pod %s: %v\n", pod.Name, err)
		}
	}

	// Delete PVCs
	for _, pvc := range pvcs.Items {
		fmt.Printf("Deleting PVC %s...\n", pvc.Name)
		err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Warning: failed to delete PVC %s: %v\n", pvc.Name, err)
		}
	}

	// Delete PVs
	for _, pv := range pvs.Items {
		fmt.Printf("Deleting PV %s...\n", pv.Name)
		err := vm.clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), pv.Name, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Warning: failed to delete PV %s: %v\n", pv.Name, err)
		}
	}

	fmt.Println("\nCleanup completed.")
	return nil
}

func (vm *VolumeManager) ListVolumeContents(volumeName, namespace, storageClass string) error {
	// Use the getVolumeInfo method that works with Longhorn volumes
	targetPod, mountPath, containerName, err := vm.getVolumeInfo(volumeName, namespace, storageClass)
	if err != nil {
		return fmt.Errorf("failed to get volume info: %v", err)
	}

	fmt.Printf("Volume: %s\n", volumeName)
	fmt.Printf("Pod: %s\n", targetPod)
	fmt.Printf("Container: %s\n", containerName)
	fmt.Printf("Mount Path: %s\n\n", mountPath)

	// Execute find command to recursively list all files and folders
	fmt.Println("Contents (recursive):")
	return vm.execInPod(namespace, targetPod, containerName, []string{"find", mountPath, "-type", "f", "-exec", "ls", "-la", "{}", ";"})
}

func (vm *VolumeManager) DownloadVolume(volumeName, namespace, outputFile, storageClass string) error {
	// Use the getVolumeInfo method that works with Longhorn volumes
	targetPod, mountPath, containerName, err := vm.getVolumeInfo(volumeName, namespace, storageClass)
	if err != nil {
		return fmt.Errorf("failed to get volume info: %v", err)
	}

	fmt.Printf("Volume: %s\n", volumeName)
	fmt.Printf("Pod: %s\n", targetPod)
	fmt.Printf("Container: %s\n", containerName)
	fmt.Printf("Mount Path: %s\n", mountPath)
	fmt.Printf("Output File: %s\n\n", outputFile)

	fmt.Println("Creating tar.gz archive...")

	// Create output file
	outFile, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer outFile.Close()

	// Execute tar command in the pod and stream output to file
	return vm.execInPodWithOutput(namespace, targetPod, containerName,
		[]string{"tar", "-czf", "-", "-C", mountPath, "."}, outFile)
}

func (vm *VolumeManager) CopyVolume(sourceVolume, destVolume, namespace, storageClass string) error {
	// Verify both volumes exist and get their pod/mount info
	sourcePod, sourceMountPath, sourceContainer, err := vm.getVolumeInfo(sourceVolume, namespace, storageClass)
	if err != nil {
		return fmt.Errorf("source volume error: %v", err)
	}

	destPod, destMountPath, destContainer, err := vm.getVolumeInfo(destVolume, namespace, storageClass)
	if err != nil {
		return fmt.Errorf("destination volume error: %v", err)
	}

	fmt.Printf("Source Volume: %s\n", sourceVolume)
	fmt.Printf("Source Pod: %s, Container: %s, Mount: %s\n", sourcePod, sourceContainer, sourceMountPath)
	fmt.Printf("Destination Volume: %s\n", destVolume)
	fmt.Printf("Destination Pod: %s, Container: %s, Mount: %s\n\n", destPod, destContainer, destMountPath)

	fmt.Println("Copying volume contents...")

	// Create a pipe to stream data from source to destination
	// First, clear the destination directory
	fmt.Println("Clearing destination directory...")
	err = vm.execInPod(namespace, destPod, destContainer,
		[]string{"sh", "-c", fmt.Sprintf("rm -rf %s/* %s/.[^.] %s/..?*", destMountPath, destMountPath, destMountPath)})
	if err != nil {
		return fmt.Errorf("failed to clear destination: %v", err)
	}

	// Use tar to copy from source to destination via streaming
	fmt.Println("Streaming data from source to destination...")

	// First, let's verify the source has data
	fmt.Println("Checking source volume contents...")
	err = vm.execInPod(namespace, sourcePod, sourceContainer, []string{"ls", "-la", sourceMountPath})
	if err != nil {
		fmt.Printf("Warning: failed to list source contents: %v\n", err)
	}

	// Create a pipe to stream tar data from source to destination
	err = vm.streamCopyBetweenPods(namespace, sourcePod, sourceContainer, sourceMountPath,
		destPod, destContainer, destMountPath)
	if err != nil {
		return fmt.Errorf("failed to copy data: %v", err)
	}

	// Verify the copy worked
	fmt.Println("Verifying destination volume contents...")
	err = vm.execInPod(namespace, destPod, destContainer, []string{"ls", "-la", destMountPath})
	if err != nil {
		fmt.Printf("Warning: failed to list destination contents: %v\n", err)
	}

	return nil
}

func (vm *VolumeManager) getVolumeInfo(volumeName, namespace, storageClass string) (podName, mountPath, containerName string, err error) {
	// First, verify the Longhorn volume exists
	volume, err := vm.getLonghornVolume(volumeName)
	if err != nil {
		return "", "", "", fmt.Errorf("Longhorn volume %s not found: %v", volumeName, err)
	}

	// Check if volume already has a PV bound and is in use
	var pvName string
	var volumeInUse bool

	if volume.PVName != "" {
		pvName = volume.PVName
		// Check if this PV is currently bound to a PVC and in use by a pod
		volumeInUse, err = vm.isVolumeInUse(pvName, namespace)
		if err != nil {
			return "", "", "", fmt.Errorf("failed to check if volume is in use: %v", err)
		}
	}

	// If volume is in use, we need to handle the multi-attach scenario
	if volumeInUse {
		fmt.Printf("Volume %s is currently in use. Checking for existing access pod...\n", volumeName)

		// Try to find the existing pod that's using this volume
		podName, mountPath, containerName, err = vm.findExistingPodForVolume(pvName, namespace)
		if err == nil {
			fmt.Printf("Found existing pod %s using volume %s\n", podName, volumeName)
			return podName, mountPath, containerName, nil
		}

		// If we can't find or use the existing pod, we need to create a snapshot-based copy
		fmt.Printf("Cannot access volume %s directly (multi-attach limitation). Creating temporary snapshot-based access...\n", volumeName)
		return vm.createSnapshotBasedAccess(volumeName, namespace, storageClass)
	}

	// If volume is not in use, proceed with normal temporary PV creation
	if pvName == "" {
		// Create temporary PV for this Longhorn volume
		pvName, err = vm.createTemporaryPV(volumeName, namespace, storageClass)
		if err != nil {
			return "", "", "", fmt.Errorf("failed to create temporary PV: %v", err)
		}
	}

	// Create temporary pod to access the volume
	return vm.createTemporaryPodForLonghorn(volumeName, namespace, storageClass)
}

func (vm *VolumeManager) execInPod(namespace, podName, containerName string, command []string) error {
	req := vm.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	config, err := vm.getConfig()
	if err != nil {
		return fmt.Errorf("failed to get config: %v", err)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %v", err)
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("failed to execute command: %v", err)
	}

	return nil
}

func (vm *VolumeManager) execInPodWithOutput(namespace, podName, containerName string, command []string, output io.Writer) error {
	req := vm.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	config, err := vm.getConfig()
	if err != nil {
		return fmt.Errorf("failed to get config: %v", err)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %v", err)
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: output,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("failed to execute command: %v", err)
	}

	return nil
}

func (vm *VolumeManager) streamCopyBetweenPods(namespace, sourcePod, sourceContainer, sourcePath, destPod, destContainer, destPath string) error {
	// Create a pipe for streaming data
	reader, writer := io.Pipe()

	// Error channel to capture errors from goroutines
	errChan := make(chan error, 2)

	// Start tar creation in source pod (producer)
	go func() {
		defer writer.Close()
		err := vm.execInPodWithOutput(namespace, sourcePod, sourceContainer,
			[]string{"tar", "-cf", "-", "-C", sourcePath, "."}, writer)
		errChan <- err
	}()

	// Start tar extraction in destination pod (consumer)
	go func() {
		err := vm.execInPodWithInput(namespace, destPod, destContainer,
			[]string{"tar", "-xf", "-", "-C", destPath}, reader)
		errChan <- err
	}()

	// Wait for both operations to complete
	for i := 0; i < 2; i++ {
		if err := <-errChan; err != nil {
			return fmt.Errorf("stream copy failed: %v", err)
		}
	}

	return nil
}

func (vm *VolumeManager) execInPodWithInput(namespace, podName, containerName string, command []string, input io.Reader) error {
	req := vm.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	config, err := vm.getConfig()
	if err != nil {
		return fmt.Errorf("failed to get config: %v", err)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %v", err)
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  input,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("failed to execute command: %v", err)
	}

	return nil
}

func (vm *VolumeManager) getConfig() (*rest.Config, error) {
	var config *rest.Config
	var err error

	// Try to use in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig file, respecting KUBECONFIG env var
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}

		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		config, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to build config: %v", err)
		}
	}

	return config, nil
}

func (vm *VolumeManager) createTemporaryPodForLonghorn(volumeName, namespace, storageClass string) (podName, mountPath, containerName string, err error) {
	// Get volume info to determine size
	volume, err := vm.getLonghornVolume(volumeName)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get Longhorn volume info: %v", err)
	}

	// Create temporary PV if it doesn't exist
	_, err = vm.createTemporaryPV(volumeName, namespace, storageClass)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create temporary PV: %v", err)
	}

	// Create a temporary PVC for this volume if it doesn't exist
	pvcName := fmt.Sprintf("lhc-temp-pvc-%s", volumeName)
	mountPath = "/mnt/volume"
	containerName = "temp-container"
	podName = fmt.Sprintf("lhc-temp-pod-%s", volumeName)
	pvName := fmt.Sprintf("lhc-temp-pv-%s", volumeName)

	// Check if temporary PVC already exists
	_, err = vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		// Create temporary PVC that specifically binds to our temporary PV
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: namespace,
				Labels: map[string]string{
					"app": "lhc-temp",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(volume.Size),
					},
				},
				StorageClassName: func() *string { return &storageClass }(),
				VolumeName:       pvName, // Bind to specific PV
			},
		}

		_, err = vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
		if err != nil {
			return "", "", "", fmt.Errorf("failed to create temporary PVC: %v", err)
		}

		// Wait for PVC to be bound
		fmt.Printf("Waiting for PVC %s to be bound...\n", pvcName)
		for i := 0; i < 60; i++ { // Wait up to 60 seconds
			pvc, err := vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvcName, metav1.GetOptions{})
			if err != nil {
				return "", "", "", fmt.Errorf("failed to get PVC status: %v", err)
			}

			if pvc.Status.Phase == corev1.ClaimBound {
				fmt.Printf("PVC %s is now bound to PV %s\n", pvcName, pvc.Spec.VolumeName)
				break
			}

			time.Sleep(1 * time.Second)
		}
	}

	// Check if temporary pod already exists and is running
	existingPod, err := vm.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err == nil && existingPod.Status.Phase == corev1.PodRunning {
		return podName, mountPath, containerName, nil
	}

	// Create temporary pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "lhc-temp",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  containerName,
					Image: "busybox:latest",
					Command: []string{
						"sleep",
						"3600", // Sleep for 1 hour
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "volume",
							MountPath: mountPath,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "volume",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err = vm.clientset.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create temporary pod: %v", err)
	}

	// Wait for pod to be running
	fmt.Printf("Waiting for temporary pod %s to be ready...\n", podName)
	for i := 0; i < 120; i++ { // Wait up to 2 minutes
		pod, err := vm.clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return "", "", "", fmt.Errorf("failed to get pod status: %v", err)
		}

		if pod.Status.Phase == corev1.PodRunning {
			return podName, mountPath, containerName, nil
		}

		time.Sleep(1 * time.Second)
	}

	return "", "", "", fmt.Errorf("temporary pod %s did not become ready in time", podName)
}

func (vm *VolumeManager) getLonghornVolumes() ([]LonghornVolume, error) {
	// Use dynamic client to get Longhorn volumes
	gvr := schema.GroupVersionResource{
		Group:    "longhorn.io",
		Version:  "v1beta2",
		Resource: "volumes",
	}

	result, err := vm.dynamicClient.Resource(gvr).Namespace("longhorn-system").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list Longhorn volumes: %v", err)
	}

	var volumes []LonghornVolume
	for _, item := range result.Items {
		volume := LonghornVolume{
			Name:  item.GetName(),
			State: "Unknown",
			Size:  "Unknown",
		}

		// Extract status
		if status, found, err := unstructured.NestedMap(item.Object, "status"); found && err == nil {
			if state, found, err := unstructured.NestedString(status, "state"); found && err == nil {
				volume.State = state
			}
		}

		// Extract spec
		if spec, found, err := unstructured.NestedMap(item.Object, "spec"); found && err == nil {
			if size, found, err := unstructured.NestedString(spec, "size"); found && err == nil {
				volume.Size = size
			}
		}

		// Extract PV name from kubernetesStatus
		if status, found, err := unstructured.NestedMap(item.Object, "status"); found && err == nil {
			if kubernetesStatus, found, err := unstructured.NestedMap(status, "kubernetesStatus"); found && err == nil {
				if pvName, found, err := unstructured.NestedString(kubernetesStatus, "pvName"); found && err == nil {
					volume.PVName = pvName
				}
			}
		}

		volumes = append(volumes, volume)
	}

	return volumes, nil
}

func (vm *VolumeManager) getLonghornVolume(volumeName string) (*LonghornVolume, error) {
	volumes, err := vm.getLonghornVolumes()
	if err != nil {
		return nil, err
	}

	for _, volume := range volumes {
		if volume.Name == volumeName {
			return &volume, nil
		}
	}

	return nil, fmt.Errorf("Longhorn volume %s not found", volumeName)
}

func (vm *VolumeManager) createTemporaryPV(volumeName, namespace, storageClass string) (string, error) {
	pvName := fmt.Sprintf("lhc-temp-pv-%s", volumeName)

	// Check if PV already exists
	_, err := vm.clientset.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err == nil {
		return pvName, nil // PV already exists
	}

	// Get volume info
	volume, err := vm.getLonghornVolume(volumeName)
	if err != nil {
		return "", fmt.Errorf("failed to get Longhorn volume info: %v", err)
	}

	// Create temporary PV that references the existing Longhorn volume
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"app": "lhc-temp",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(volume.Size),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "driver.longhorn.io",
					VolumeHandle: volumeName, // This should match the Longhorn volume name exactly
					FSType:       "ext4",
					VolumeAttributes: map[string]string{
						"numberOfReplicas":    "3",
						"staleReplicaTimeout": "2880",
					},
				},
			},
		},
	}

	_, err = vm.clientset.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create temporary PV: %v", err)
	}

	return pvName, nil
}

func (vm *VolumeManager) cleanupTemporaryResources(volumeName, namespace string) error {
	pvcName := fmt.Sprintf("lhc-temp-pvc-%s", volumeName)
	podName := fmt.Sprintf("lhc-temp-pod-%s", volumeName)
	pvName := fmt.Sprintf("lhc-temp-pv-%s", volumeName)

	// Delete temporary pod
	err := vm.clientset.CoreV1().Pods(namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{})
	if err != nil {
		fmt.Printf("Warning: failed to delete temporary pod %s: %v\n", podName, err)
	}

	// Delete temporary PVC
	err = vm.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(context.TODO(), pvcName, metav1.DeleteOptions{})
	if err != nil {
		fmt.Printf("Warning: failed to delete temporary PVC %s: %v\n", pvcName, err)
	}

	// Delete temporary PV
	err = vm.clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), pvName, metav1.DeleteOptions{})
	if err != nil {
		fmt.Printf("Warning: failed to delete temporary PV %s: %v\n", pvName, err)
	}

	return nil
}

func printUsage() {
	fmt.Printf("Longhorn Volume Manager v%s\n", version)
	fmt.Println("Usage:")
	fmt.Println("  go run main.go <command> [flags]")
	fmt.Println("")
	fmt.Println("Commands:")
	fmt.Println("  list      - List all Longhorn volumes")
	fmt.Println("  contents  - Show volume contents recursively")
	fmt.Println("  download  - Download volume as tar.gz")
	fmt.Println("  copy      - Copy source volume to destination volume")
	fmt.Println("  cleanup   - Clean up temporary resources (lhc-temp-* prefixed)")
	fmt.Println("")
	fmt.Println("Flags:")
	fmt.Println("  -v          Volume name (required for contents/download)")
	fmt.Println("  -s          Source volume name (required for copy)")
	fmt.Println("  -d          Destination volume name (required for copy)")
	fmt.Println("  -o          Output file path (required for download)")
	fmt.Println("  -n          Kubernetes namespace (default: 'default')")
	fmt.Println("  -c          Storage class name (default: 'longhorn')")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run main.go list")
	fmt.Println("  go run main.go list -n kube-system")
	fmt.Println("  go run main.go contents -v pvc-12345")
	fmt.Println("  go run main.go contents -v pvc-12345 -n default")
	fmt.Println("  go run main.go download -v pvc-12345 -o backup.tar.gz")
	fmt.Println("  go run main.go download -v pvc-12345 -o backup.tar.gz -n default")
	fmt.Println("  go run main.go copy -s pvc-source -d pvc-dest")
	fmt.Println("  go run main.go copy -s pvc-source -d pvc-dest -n default")
	fmt.Println("  go run main.go copy -s pvc-source -d pvc-dest -c longhorn")
	fmt.Println("  go run main.go cleanup -n default")
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	// Create a new flag set for the subcommand
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.Usage = printUsage

	// Define command line flags with single character versions
	var (
		volume       = fs.String("v", "", "Volume name")
		source       = fs.String("s", "", "Source volume name")
		dest         = fs.String("d", "", "Destination volume name")
		output       = fs.String("o", "", "Output file path")
		namespace    = fs.String("n", "default", "Kubernetes namespace")
		storageClass = fs.String("c", "longhorn", "Storage class name")
	)

	// Parse flags for the subcommand
	fs.Parse(os.Args[2:])

	vm, err := NewVolumeManager()
	if err != nil {
		log.Fatalf("Failed to initialize volume manager: %v", err)
	}

	switch command {
	case "list":
		if err := vm.ListVolumes(*namespace); err != nil {
			log.Fatalf("Failed to list volumes: %v", err)
		}

	case "contents":
		if *volume == "" {
			fmt.Println("Error: -v (volume) flag is required for contents command")
			printUsage()
			os.Exit(1)
		}
		if err := vm.ListVolumeContents(*volume, *namespace, *storageClass); err != nil {
			log.Fatalf("Failed to get volume contents: %v", err)
		}

	case "download":
		if *volume == "" {
			fmt.Println("Error: -v (volume) flag is required for download command")
			printUsage()
			os.Exit(1)
		}
		if *output == "" {
			fmt.Println("Error: -o (output) flag is required for download command")
			printUsage()
			os.Exit(1)
		}
		if err := vm.DownloadVolume(*volume, *namespace, *output, *storageClass); err != nil {
			log.Fatalf("Failed to download volume: %v", err)
		}
		fmt.Printf("\nDownload completed: %s\n", *output)

	case "copy":
		if *source == "" {
			fmt.Println("Error: -s (source) flag is required for copy command")
			printUsage()
			os.Exit(1)
		}
		if *dest == "" {
			fmt.Println("Error: -d (dest) flag is required for copy command")
			printUsage()
			os.Exit(1)
		}
		if err := vm.CopyVolume(*source, *dest, *namespace, *storageClass); err != nil {
			log.Fatalf("Failed to copy volume: %v", err)
		}

		// Cleanup any temporary resources
		vm.cleanupTemporaryResources(*source, *namespace)
		vm.cleanupTemporaryResources(*dest, *namespace)

		fmt.Printf("\nCopy completed: %s -> %s\n", *source, *dest)

	case "cleanup":
		if err := vm.CleanupTemporaryResources(*namespace); err != nil {
			log.Fatalf("Failed to cleanup temporary resources: %v", err)
		}

	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}
