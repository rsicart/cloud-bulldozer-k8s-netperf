package k8s

import (
	"context"
	"fmt"
	"os"

	b64 "encoding/base64"

	"github.com/cloud-bulldozer/k8s-netperf/pkg/config"
	kubevirtv1 "github.com/cloud-bulldozer/k8s-netperf/pkg/kubevirt/client-go/clientset/versioned/typed/core/v1"
	log "github.com/cloud-bulldozer/k8s-netperf/pkg/logging"
	"github.com/melbahja/goph"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	v1 "kubevirt.io/api/core/v1"
)

var (
	sshPort = uint(32022)
)

func SSHConnect(conf *config.PerfScenarios) error {
	dirname, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	auth, err := goph.Key(fmt.Sprintf("%s/.ssh/id_rsa.pub", dirname), "")
	if err != nil {
		log.Fatal(err)
	}
	user := "fedora"
	addr := conf.VMHost

	client, err := goph.NewConn(&goph.Config{
		User:     user,
		Addr:     addr,
		Port:     sshPort,
		Auth:     auth,
		Callback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return err
	}
	defer client.Close()
	out, err := client.Run("ls /etc")
	if err != nil {
		return err
	}
	log.Info(string(out))

	return nil
}

func createCommService(client *kubernetes.Clientset, label map[string]string, name string) error {
	log.Infof("🚀 Creating service for %s in namespace %s", name, namespace)
	sc := client.CoreV1().Services(namespace)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       fmt.Sprintf("%s", name),
					Protocol:   corev1.ProtocolTCP,
					NodePort:   int32(sshPort),
					TargetPort: intstr.Parse(fmt.Sprintf("%d", 22)),
					Port:       22,
				},
			},
			Type:     corev1.ServiceType("NodePort"),
			Selector: label,
		},
	}
	_, err := sc.Create(context.TODO(), service, metav1.CreateOptions{})
	return err
}

func exposeService(client *kubernetes.Clientset, dynamicClient *dynamic.DynamicClient, svcName string) (string, error) {
	gvr := schema.GroupVersionResource{
		Group:    "route.openshift.io",
		Version:  "v1",
		Resource: "routes",
	}

	route := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "route.openshift.io/v1",
			"kind":       "Route",
			"metadata": map[string]interface{}{
				"name":      fmt.Sprintf("svc-%s-route", svcName),
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"port": map[string]interface{}{
					"targetPort": 22,
				},
				"to": map[string]interface{}{
					"kind":   "Service",
					"name":   svcName,
					"weight": 100,
				},
				"wildcardPolicy": "None",
			},
		},
	}
	route, err := dynamicClient.Resource(gvr).Namespace(namespace).Create(context.TODO(), route, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create route: %v", err)
	}
	retrievedRoute, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(context.TODO(), route.GetName(), metav1.GetOptions{})
	if err != nil {
		log.Fatalf("error retrieving route: %v", err)
	}
	spec, ok := retrievedRoute.Object["spec"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("error extracting spec from route")
	}
	host, ok := spec["host"].(string)
	if !ok {
		return "", fmt.Errorf("host not found in route spec")
	}
	return host, nil
}

func CreateVMClient(kclient *kubevirtv1.KubevirtV1Client, client *kubernetes.Clientset,
	dyn *dynamic.DynamicClient, role string, name string,
	podAff *corev1.PodAntiAffinity,
	nodeAff *corev1.NodeAffinity) (string, error) {
	label := map[string]string{
		"app":  name,
		"role": role,
	}
	dirname, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	ssh, err := os.ReadFile(fmt.Sprintf("%s/.ssh/id_rsa.pub", dirname))
	if err != nil {
		return "", err
	}
	data := fmt.Sprintf(`#cloud-config
users:
  - name: fedora
    groups: sudo
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
ssh_deletekeys: false
password: fedora
chpasswd: { expire: False }
runcmd:
  - dnf install -y uperf iperf3 git ethtool
`, string(ssh))
	_, err = CreateVMI(kclient, name, label, b64.StdEncoding.EncodeToString([]byte(data)), *podAff, *nodeAff)
	if err != nil {
		return "", err
	}
	err = createCommService(client, label, fmt.Sprintf("%s-svc", name))
	if err != nil {
		return "", err
	}
	host, err := exposeService(client, dyn, fmt.Sprintf("%s-svc", name))
	if err != nil {
		return "", err
	}
	return host, nil
}

func CreateVMServer(client *kubevirtv1.KubevirtV1Client, name string, role string,
	podAff corev1.PodAntiAffinity,
	nodeAff corev1.NodeAffinity) (*v1.VirtualMachineInstance, error) {
	label := map[string]string{
		"app":  name,
		"role": role,
	}
	dirname, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	ssh, err := os.ReadFile(fmt.Sprintf("%s/.ssh/id_rsa.pub", dirname))
	if err != nil {
		return nil, err
	}
	data := fmt.Sprintf(`#cloud-config
users:
  - name: fedora
    groups: sudo
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
ssh_deletekeys: false
password: fedora
chpasswd: { expire: False }
runcmd:
  - dnf install -y uperf iperf3 git ethtool
  - uperf -s -v &
  - iperf3 -s &
`, string(ssh))
	return CreateVMI(client, name, label, b64.StdEncoding.EncodeToString([]byte(data)), podAff, nodeAff)
}

func CreateVMI(client *kubevirtv1.KubevirtV1Client, name string, label map[string]string, b64data string,
	podAff corev1.PodAntiAffinity,
	nodeAff corev1.NodeAffinity) (*v1.VirtualMachineInstance, error) {
	delSeconds := int64(0)
	mutliQ := true
	vmi, err := client.VirtualMachineInstances(namespace).Create(context.TODO(), &v1.VirtualMachineInstance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1.GroupVersion.String(),
			Kind:       "VirtualMachineInstance",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    label,
		},
		Spec: v1.VirtualMachineInstanceSpec{
			Affinity: &k8sv1.Affinity{
				PodAntiAffinity: &podAff,
				NodeAffinity:    &nodeAff,
			},
			TerminationGracePeriodSeconds: &delSeconds,
			Domain: v1.DomainSpec{
				Resources: v1.ResourceRequirements{
					Requests: k8sv1.ResourceList{
						k8sv1.ResourceMemory: resource.MustParse("4096Mi"),
						k8sv1.ResourceCPU:    resource.MustParse("500m"),
					},
				},
				CPU: &v1.CPU{
					Sockets: 2,
					Cores:   2,
					Threads: 1,
				},
				Devices: v1.Devices{
					NetworkInterfaceMultiQueue: &mutliQ,
					Disks: []v1.Disk{
						v1.Disk{
							Name: "disk0",
							DiskDevice: v1.DiskDevice{
								Disk: &v1.DiskTarget{
									Bus: "virtio",
								},
							},
						},
					},
				},
			},
			Volumes: []v1.Volume{
				v1.Volume{
					Name: "disk0",
					VolumeSource: v1.VolumeSource{
						ContainerDisk: &v1.ContainerDiskSource{
							Image: "kubevirt/fedora-cloud-container-disk-demo:latest",
						},
					},
				},
				v1.Volume{
					Name: "cloudinit",
					VolumeSource: v1.VolumeSource{
						CloudInitNoCloud: &v1.CloudInitNoCloudSource{
							UserDataBase64: b64data,
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return vmi, err
	}
	return vmi, nil
}

func WaitForVMI(client *kubevirtv1.KubevirtV1Client, name string) error {
	log.Infof("Wating for VMI (%s) to be in state running", name)
	vmw, err := client.VirtualMachineInstances(namespace).Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	defer vmw.Stop()
	for event := range vmw.ResultChan() {
		d, ok := event.Object.(*v1.VirtualMachineInstance)
		if !ok {
			return fmt.Errorf("Unable to watch VMI %s", name)
		}
		if d.Name == name {
			log.Infof("Found in state (%s)", d.Status.Phase)
			if d.Status.Phase == "Running" {
				return nil
			}
		}
	}
	return nil
}
