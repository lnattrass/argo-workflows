package controller

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-workflows/v3/errors"
	"github.com/argoproj/argo-workflows/v3/pkg/apis/workflow"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
)

func (woc *wfOperationCtx) getAgentPodName() string {
	return woc.wf.NodeID("agent") + "-agent"
}

func (woc *wfOperationCtx) isAgentPod(pod *apiv1.Pod) bool {
	return pod.Name == woc.getAgentPodName()
}

func (woc *wfOperationCtx) reconcileAgentPod(ctx context.Context) error {
	woc.log.Infof("reconcileAgentPod")
	if len(woc.taskSet) == 0 {
		return nil
	}
	pod, err := woc.createAgentPod(ctx)
	if err != nil {
		return err
	}
	// Check Pod is just created
	if pod.Status.Phase != "" {
		woc.updateAgentPodStatus(ctx, pod)
	}
	return nil
}

func (woc *wfOperationCtx) updateAgentPodStatus(ctx context.Context, pod *apiv1.Pod) {
	woc.log.Infof("updateAgentPodStatus")
	newPhase, message := assessAgentPodStatus(pod)
	if newPhase == wfv1.WorkflowFailed || newPhase == wfv1.WorkflowError {
		woc.markWorkflowError(ctx, fmt.Errorf("agent pod failed with reason %s", message))
	}
}

func assessAgentPodStatus(pod *apiv1.Pod) (wfv1.WorkflowPhase, string) {
	var newPhase wfv1.WorkflowPhase
	var message string
	log.Infof("assessAgentPodStatus")
	switch pod.Status.Phase {
	case apiv1.PodSucceeded, apiv1.PodRunning, apiv1.PodPending:
		return "", ""
	case apiv1.PodFailed:
		newPhase = wfv1.WorkflowFailed
		message = pod.Status.Message
	default:
		newPhase = wfv1.WorkflowError
		message = fmt.Sprintf("Unexpected pod phase for %s: %s", pod.ObjectMeta.Name, pod.Status.Phase)
	}
	return newPhase, message
}

func (woc *wfOperationCtx) createAgentPod(ctx context.Context) (*apiv1.Pod, error) {
	podName := woc.getAgentPodName()

	obj, exists, err := woc.controller.podInformer.GetStore().Get(cache.ExplicitKey(woc.wf.Namespace + "/" + podName))
	if err != nil {
		return nil, fmt.Errorf("failed to get pod from informer store: %w", err)
	}
	if exists {
		existing, ok := obj.(*apiv1.Pod)
		if ok {
			woc.log.WithField("podPhase", existing.Status.Phase).Debugf("Skipped pod %s  creation: already exists", podName)
			return existing, nil
		}
	}

	pluginSidecars, pluginAddresses, err := woc.getAgentPlugins()
	if err != nil {
		return nil, err
	}

	pod := &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: woc.wf.ObjectMeta.Namespace,
			Labels: map[string]string{
				common.LabelKeyWorkflow:  woc.wf.Name, // Allows filtering by pods related to specific workflow
				common.LabelKeyCompleted: "false",     // Allows filtering by incomplete workflow pods
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(woc.wf, wfv1.SchemeGroupVersion.WithKind(workflow.WorkflowKind)),
			},
		},
		Spec: apiv1.PodSpec{
			RestartPolicy:    apiv1.RestartPolicyOnFailure,
			ImagePullSecrets: woc.execWf.Spec.ImagePullSecrets,
			Containers: append(
				pluginSidecars,
				apiv1.Container{
					Name:    "main",
					Command: []string{"argoexec"},
					Args:    []string{"agent"},
					Image:   woc.controller.executorImage(),
					Env: []apiv1.EnvVar{
						{Name: common.EnvVarWorkflowName, Value: woc.wf.Name},
						{Name: common.EnvVarPluginAddresses, Value: wfv1.MustMarshallJSON(pluginAddresses)},
					},
				},
			),
		},
	}

	if woc.controller.Config.InstanceID != "" {
		pod.ObjectMeta.Labels[common.LabelKeyControllerInstanceID] = woc.controller.Config.InstanceID
	}
	if woc.wf.Spec.ServiceAccountName != "" {
		pod.Spec.ServiceAccountName = woc.wf.Spec.ServiceAccountName
	}

	woc.log.Debugf("Creating Agent Pod: %s", podName)

	created, err := woc.controller.kubeclientset.CoreV1().Pods(woc.wf.ObjectMeta.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if apierr.IsAlreadyExists(err) {
			woc.log.Infof("Agent Pod %s  creation: already exists", podName)
			return created, nil
		}
		woc.log.Infof("Failed to create Agent pod %s: %v", podName, err)
		return nil, errors.InternalWrapError(fmt.Errorf("failed to create Agent pod. Reason: %v", err))
	}
	woc.log.Infof("Created Agent pod: %s", created.Name)
	return created, nil
}

func (woc *wfOperationCtx) getAgentPlugins() ([]apiv1.Container, []string, error) {
	var sidecars []apiv1.Container
	var addresses []string
	if woc.controller.plugins {
		namespaces := map[string]bool{}
		namespaces[woc.controller.namespace] = true
		namespaces[woc.wf.Namespace] = true
		for namespace := range namespaces {
			cms, err := woc.controller.getConfigMaps(namespace, "AgentPlugin")
			if err != nil {
				return nil, nil, err
			}
			for _, cm := range cms {
				var command, args []string
				if v, ok := cm.Data["command"]; ok {
					if err := yaml.Unmarshal([]byte(v), &command); err != nil {
						return nil, nil, fmt.Errorf("failed to parse %q: %w", v, err)
					}
				}
				if v, ok := cm.Data["args"]; ok {
					if err := yaml.Unmarshal([]byte(v), &args); err != nil {
						return nil, nil, fmt.Errorf("failed to parse %q: %w", v, err)
					}
				}
				image, address := cm.Data["image"], cm.Data["address"]
				log.WithField("command", command).
					WithField("args", args).
					WithField("image", image).
					WithField("address", address).
					Debug("adding agent plugins sidecar")
				sidecars = append(sidecars, apiv1.Container{
					Name:    cm.Name,
					Image:   image,
					Command: command,
					Args:    args,
				})
				addresses = append(addresses, address)
			}
		}
	}
	return sidecars, addresses, nil
}
