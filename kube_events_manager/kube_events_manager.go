package kube_events_manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/romana/rlog"
	"gopkg.in/satori/go.uuid.v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	appsV1 "k8s.io/client-go/informers/apps/v1"
	batchV1 "k8s.io/client-go/informers/batch/v1"
	batchV2Alpha1 "k8s.io/client-go/informers/batch/v2alpha1"
	coreV1 "k8s.io/client-go/informers/core/v1"
	extensionsV1Beta1 "k8s.io/client-go/informers/extensions/v1beta1"
	storageV1 "k8s.io/client-go/informers/storage/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/flant/antiopa/kube"
	"github.com/flant/antiopa/utils"
)

var (
	KubeEventCh chan string
)

type InformerType int

const (
	OnAdd InformerType = iota
	OnUpdate
	OnDelete
)

type KubeEventsManager interface {
	Run(informerType InformerType, kind, namespace string, labelSelector *metaV1.LabelSelector, jqFilter string) (string, error)
	Stop(configId string) error
}

type MainKubeEventsManager struct {
	KubeEventsInformersByConfigId map[string]*KubeEventsInformer
}

func NewMainKubeEventsManager() *MainKubeEventsManager {
	em := &MainKubeEventsManager{}
	em.KubeEventsInformersByConfigId = make(map[string]*KubeEventsInformer)
	return em
}

func Init() (KubeEventsManager, error) {
	em := NewMainKubeEventsManager()
	KubeEventCh = make(chan string, 1)
	return em, nil
}

func (em *MainKubeEventsManager) Run(informerType InformerType, kind, namespace string, labelSelector *metaV1.LabelSelector, jqFilter string) (string, error) {
	kubeEventsInformer, err := em.addKubeEventsInformer(kind, namespace, labelSelector, jqFilter, func(kubeEventsInformer *KubeEventsInformer) cache.ResourceEventHandlerFuncs {
		return cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				objectId, err := runtimeResourceId(obj)
				if err != nil {
					rlog.Errorf("failed to get object id: %s", err)
					return
				}

				filtered, err := resourceFilter(obj, jqFilter)
				if err != nil {
					rlog.Error("Kube events manager: informer %s object %s: %s", kubeEventsInformer.ConfigId, objectId, err)
					return
				}

				checksum := utils.CalculateChecksum(filtered)

				rlog.Debugf("Kube events manager: informer %s: add object %s: jqFilter '%s': calculated checksum '%s' of object being watched:\n%s",
					kubeEventsInformer.ConfigId, objectId, jqFilter, checksum, utils.FormatJsonDataOrError(utils.FormatPrettyJson(filtered)))

				err = kubeEventsInformer.HandleKubeEvent(obj, checksum, informerType == OnAdd)
				if err != nil {
					rlog.Error("Kube events manager: %s", err)
					return
				}
			},
			UpdateFunc: func(_ interface{}, obj interface{}) {
				objectId, err := runtimeResourceId(obj)
				if err != nil {
					rlog.Errorf("failed to get object id: %s", err)
					return
				}

				filtered, err := resourceFilter(obj, jqFilter)
				if err != nil {
					rlog.Error("Kube events manager: informer %s object %s: %s", kubeEventsInformer.ConfigId, objectId, err)
					return
				}

				checksum := utils.CalculateChecksum(filtered)

				rlog.Debugf("Kube events manager: informer %s: update object %s: jqFilter '%s': calculated checksum '%s' of object being watched:\n%s",
					kubeEventsInformer.ConfigId, objectId, jqFilter, checksum, utils.FormatJsonDataOrError(utils.FormatPrettyJson(filtered)))

				err = kubeEventsInformer.HandleKubeEvent(obj, checksum, informerType == OnUpdate)
				if err != nil {
					rlog.Error("Kube events manager: %s", err)
					return
				}
			},
			DeleteFunc: func(obj interface{}) {
				objectId, err := runtimeResourceId(obj)
				if err != nil {
					rlog.Errorf("failed to get object id: %s", err)
					return
				}

				rlog.Debugf("Kube events manager: informer %s: delete object %s", kubeEventsInformer.ConfigId, objectId)

				err = kubeEventsInformer.HandleKubeEvent(obj, "", informerType == OnDelete)
				if err != nil {
					rlog.Error("Kube events manager: %s", err)
					return
				}
			},
		}
	})

	if err != nil {
		return "", err
	}

	go kubeEventsInformer.Run()

	return kubeEventsInformer.ConfigId, nil
}

func (em *MainKubeEventsManager) addKubeEventsInformer(kind, namespace string, labelSelector *metaV1.LabelSelector, jqFilter string, resourceEventHandlerFuncs func(kubeEventsInformer *KubeEventsInformer) cache.ResourceEventHandlerFuncs) (*KubeEventsInformer, error) {
	kubeEventsInformer := NewKubeEventsInformer()

	formatSelector, err := formatLabelSelector(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed format label selector '%s'", labelSelector.String())
	}

	indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	resyncPeriod := time.Duration(15) * time.Second
	tweakListOptions := func(options *metaV1.ListOptions) {
		if formatSelector != "" {
			options.LabelSelector = formatSelector
		}
	}

	listOptions := metaV1.ListOptions{}
	if formatSelector != "" {
		listOptions.LabelSelector = formatSelector
	}

	var sharedInformer cache.SharedIndexInformer

	switch kind {
	case "cronjob":
		sharedInformer = batchV2Alpha1.NewFilteredCronJobInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.BatchV2alpha1().CronJobs(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "daemonset":
		sharedInformer = appsV1.NewFilteredDaemonSetInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.AppsV1().DaemonSets(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "deployment":
		sharedInformer = appsV1.NewFilteredDeploymentInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.AppsV1().Deployments(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "job":
		sharedInformer = batchV1.NewFilteredJobInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.BatchV1().Jobs(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "pod":
		sharedInformer = coreV1.NewFilteredPodInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().Pods(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "replicaset":
		sharedInformer = appsV1.NewFilteredReplicaSetInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.AppsV1().ReplicaSets(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "replicationcontroller":
		sharedInformer = coreV1.NewFilteredReplicationControllerInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().ReplicationControllers(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "statefulset":
		sharedInformer = appsV1.NewFilteredStatefulSetInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.AppsV1().StatefulSets(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "endpoints":
		sharedInformer = coreV1.NewFilteredEndpointsInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().Endpoints(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "ingress":
		sharedInformer = extensionsV1Beta1.NewFilteredIngressInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.ExtensionsV1beta1().Ingresses(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "service":
		sharedInformer = coreV1.NewFilteredServiceInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().Services(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "configmap":
		sharedInformer = coreV1.NewFilteredConfigMapInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().ConfigMaps(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "secret":
		sharedInformer = coreV1.NewFilteredSecretInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().Secrets(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "persistentvolumeclaim":
		sharedInformer = coreV1.NewFilteredPersistentVolumeClaimInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().PersistentVolumeClaims(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "storageclass":
		sharedInformer = storageV1.NewFilteredStorageClassInformer(kube.Kubernetes, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.StorageV1().StorageClasses().List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "node":
		sharedInformer = coreV1.NewFilteredNodeInformer(kube.Kubernetes, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().Nodes().List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	case "serviceaccount":
		sharedInformer = coreV1.NewFilteredServiceAccountInformer(kube.Kubernetes, namespace, resyncPeriod, indexers, tweakListOptions)

		list, err := kube.Kubernetes.CoreV1().ServiceAccounts(namespace).List(listOptions)
		if err != nil {
			return nil, fmt.Errorf("failed list resources: %s", err)
		}

		objects := make([]ListItemObject, 0)
		for _, obj := range list.Items {
			objects = append(objects, &obj)
		}

		err = kubeEventsInformer.InitializeItemsList(objects, jqFilter)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("kind '%s' isn't supported", kind)
	}

	kubeEventsInformer.SharedInformer = sharedInformer
	kubeEventsInformer.SharedInformer.AddEventHandler(resourceEventHandlerFuncs(kubeEventsInformer))
	kubeEventsInformer.ConfigId = uuid.NewV4().String()

	em.KubeEventsInformersByConfigId[kubeEventsInformer.ConfigId] = kubeEventsInformer

	return kubeEventsInformer, nil
}

func formatLabelSelector(selector *metaV1.LabelSelector) (string, error) {
	res, err := metaV1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", err
	}

	return res.String(), nil
}

func resourceFilter(obj interface{}, jqFilter string) (res string, err error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}

	if jqFilter != "" {
		stdout, stderr, err := execJq(jqFilter, data)
		if err != nil {
			return "", fmt.Errorf("failed exec jq: \nerr: '%s'\nstderr: '%s'", err, stderr)
		}

		res = stdout
	} else {
		res = string(data)
	}
	return
}

func (em *MainKubeEventsManager) Stop(configId string) error {
	kubeEventsInformer, ok := em.KubeEventsInformersByConfigId[configId]
	if ok {
		kubeEventsInformer.Stop()
	} else {
		rlog.Errorf("Kube events informer '%s' not found!", configId)
	}
	return nil
}

type KubeEventsInformer struct {
	ConfigId           string
	Checksum           map[string]string
	SharedInformer     cache.SharedInformer
	SharedInformerStop chan struct{}
}

func NewKubeEventsInformer() *KubeEventsInformer {
	kubeEventsInformer := &KubeEventsInformer{}
	kubeEventsInformer.Checksum = make(map[string]string)
	kubeEventsInformer.SharedInformerStop = make(chan struct{}, 1)
	return kubeEventsInformer
}

type ListItemObject interface {
	GetName() string
	GetNamespace() string
}

func (ei *KubeEventsInformer) InitializeItemsList(objects []ListItemObject, jqFilter string) error {
	for _, obj := range objects {
		resourceId := generateChecksumId(obj.GetName(), obj.GetNamespace())

		filtered, err := resourceFilter(obj, jqFilter)
		if err != nil {
			return err
		}

		ei.Checksum[resourceId] = utils.CalculateChecksum(filtered)

		rlog.Debugf("Kube events manager: informer %s: object %s initialization: jqFilter '%s': calculated checksum '%s' of object being watched:\n%s",
			ei.ConfigId,
			resourceId,
			jqFilter,
			ei.Checksum[resourceId],
			utils.FormatJsonDataOrError(utils.FormatPrettyJson(filtered)))
	}

	return nil
}

func (ei *KubeEventsInformer) HandleKubeEvent(obj interface{}, newChecksum string, sendSignal bool) error {
	objectId, err := runtimeResourceId(obj.(runtime.Object))
	if err != nil {
		return fmt.Errorf("failed to get object id: %s", err)
	}

	if ei.Checksum[objectId] != newChecksum {
		ei.Checksum[objectId] = newChecksum

		if sendSignal {
			rlog.Debugf("Kube events manager: informer %s: object %s CHANGED", ei.ConfigId, objectId)
			KubeEventCh <- ei.ConfigId
		}
	}

	return nil
}

func runtimeResourceId(obj interface{}) (string, error) {
	runtimeObj := obj.(runtime.Object)
	accessor := meta.NewAccessor()

	name, err := accessor.Name(runtimeObj)
	if err != nil {
		return "", err
	}

	namespace, err := accessor.Namespace(runtimeObj)
	if err != nil {
		return "", err
	}

	return generateChecksumId(name, namespace), nil
}

func generateChecksumId(name, namespace string) string {
	return fmt.Sprintf("name=%s namespace=%s", name, namespace)
}

func (ei *KubeEventsInformer) Run() {
	rlog.Debugf("Kube events manager: run informer %s", ei.ConfigId)
	ei.SharedInformer.Run(ei.SharedInformerStop)
}

func (ei *KubeEventsInformer) Stop() {
	rlog.Debugf("Kube events manager: stop informer %s", ei.ConfigId)
	close(ei.SharedInformerStop)
}

func execJq(jqFilter string, jsonData []byte) (stdout string, stderr string, err error) {
	cmd := exec.Command("/usr/bin/jq", jqFilter)

	var stdinBuf bytes.Buffer
	_, err = stdinBuf.WriteString(string(jsonData))
	if err != nil {
		panic(err)
	}
	cmd.Stdin = &stdinBuf
	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	err = cmd.Run()
	stdout = strings.TrimSpace(stdoutBuf.String())
	stderr = strings.TrimSpace(stderrBuf.String())

	return
}
