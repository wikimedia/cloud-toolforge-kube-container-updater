//   Copyright 2022 Taavi Väänänen <hi@taavi.wtf>
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package main

import (
	"context"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// containerReference is a simple struct to hold data about the pods that possibly will be restarted.
type containerReference struct {
	Namespace string
	PodName   string
	ImageID   string
	CreatedAt int64
}

// config is the kube-container-updater configuration
type config struct {
	// KubeConfigPath is the path to the kubeconfig file to use, if any. If empty,
	// will use in-cluster config instead.
	KubeConfigPath string `envconfig:"KUBE_CONFIG_PATH" default:""`

	// MaxPodsToRestart specifies the maximum amount of pods to restart per execution.
	MaxPodsToRestart int `envconfig:"MAX_PODS_TO_RESTART" default:"25"`

	// NamespacePrefix is a simple filter to the namespaces being processed.
	NamespacePrefix string `envconfig:"NAMESPACE_PREFIX" default:""`

	// DryRun allows testing the tool before actually deleting pods.
	DryRun bool `envconfig:"DRY_RUN" default:"true"`
}

func (conf config) getKubeConfig() (*rest.Config, error) {
	if conf.KubeConfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", conf.KubeConfigPath)
	}

	return rest.InClusterConfig()
}

func main() {
	var conf config
	err := envconfig.Process("CONTAINER_UPDATER", &conf)
	if err != nil {
		logrus.Fatalln(err)
	}

	if conf.DryRun {
		logrus.Infof("In dry-run mode, not actually deleting anything")
	}

	config, err := conf.getKubeConfig()
	if err != nil {
		logrus.Fatalln(err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.Fatalln(err)
	}

	pods, err := client.CoreV1().
		Pods(""). // empty string means all namespaces, filtering is done below
		List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logrus.Fatalln(err)
	}

	byImage := map[string][]containerReference{}

	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Namespace, conf.NamespacePrefix) {
			continue
		}

		hasOwnerReferences := false
		isOwnedByJob := false
		for _, ref := range pod.OwnerReferences {
			// Ensure we don't end up deleting unmanaged pods
			hasOwnerReferences = true

			// jobs will eventually finish, so don't bother manually killing them
			if ref.Kind == "Job" {
				isOwnedByJob = true
			}
		}
		if !hasOwnerReferences || isOwnedByJob {
			continue
		}

		for i, container := range pod.Spec.Containers {
			if container.ImagePullPolicy != "Always" {
				continue
			}

			status := pod.Status.ContainerStatuses[i]
			reference := containerReference{
				Namespace: pod.Namespace,
				PodName:   pod.Name,
				ImageID:   status.ImageID,
				CreatedAt: pod.CreationTimestamp.Unix(),
			}

			list, ok := byImage[status.Image]
			if ok {
				byImage[status.Image] = append(list, reference)
			} else {
				byImage[status.Image] = []containerReference{reference}
			}
		}
	}

	var toRestart []containerReference

	for image, containers := range byImage {
		ref, err := name.ParseReference(image)
		if err != nil {
			logrus.Fatalln(err)
		}

		desc, err := remote.Get(ref)
		if err != nil {
			logrus.Fatalln(err)
		}

		latestDigest := desc.Digest.String()
		logrus.Infof("Latest hash for image %v (used by %v pods) is %v",
			ref.Name(), len(containers), latestDigest)

		for _, container := range containers {
			runningDigest := strings.Split(container.ImageID, "@")[1]
			if runningDigest != latestDigest {
				toRestart = append(toRestart, container)
			}
		}
	}

	// Restart the oldest pods first, in case we end up hitting the numerical limit
	if len(toRestart) >= conf.MaxPodsToRestart {
		sort.Slice(toRestart, func(i, j int) bool {
			return toRestart[i].CreatedAt < toRestart[j].CreatedAt
		})
	}

	if conf.DryRun {
		logrus.Infof("Reminder: In dry-run mode, not actually deleting anything")
	}

	for i, toRestart := range toRestart {
		if i >= conf.MaxPodsToRestart {
			logrus.Warnf("Hit the limit of %v pods, not restarting any more", conf.MaxPodsToRestart)
			break
		}

		logrus.Infof("Restarting pod %v/%v to pick up container updates", toRestart.Namespace, toRestart.PodName)

		if !conf.DryRun {
			// TODO: investigate less disruptive ways to do this for tools with multiple replicas
			err := client.CoreV1().
				Pods(toRestart.Namespace).
				Delete(
					context.TODO(),
					toRestart.PodName,
					metav1.DeleteOptions{},
				)
			if err != nil {
				logrus.Fatalln(err)
			}
		}
	}
}
