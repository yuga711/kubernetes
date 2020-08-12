/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduling

import (
	"context"
	"fmt"
	"math"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

var _ = SIGDescribe("Multi-AZ Clusters", func() {
	f := framework.NewDefaultFramework("multi-az")
	var zoneCount int
	var err error
	image := framework.ServeHostnameImage
	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessProviderIs("gce", "gke", "aws")
		if zoneCount <= 0 {
			zoneCount, err = getZoneCount(f.ClientSet)
			framework.ExpectNoError(err)
		}
		ginkgo.By(fmt.Sprintf("Checking for multi-zone cluster.  Zone count = %d", zoneCount))
		msg := fmt.Sprintf("Zone count is %d, only run for multi-zone clusters, skipping test", zoneCount)
		e2eskipper.SkipUnlessAtLeast(zoneCount, 2, msg)
		// TODO: SkipUnlessDefaultScheduler() // Non-default schedulers might not spread
	})
	ginkgo.It("should spread the pods of a service across zones", func() {
		SpreadServiceOrFail(f, 10*zoneCount, image)
	})

	ginkgo.It("should spread the pods of a replication controller across zones", func() {
		SpreadRCOrFail(f, int32(10*zoneCount), image, []string{"serve-hostname"})
	})
})

// SpreadServiceOrFail check that the pods comprising a service
// get spread evenly across available zones
func SpreadServiceOrFail(f *framework.Framework, replicaCount int, image string) {
	// First create the service
	serviceName := "test-service"
	serviceSpec := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: f.Namespace.Name,
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"service": serviceName,
			},
			Ports: []v1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt(80),
			}},
		},
	}
	_, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), serviceSpec, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	// Now create some pods behind the service
	podSpec := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   serviceName,
			Labels: map[string]string{"service": serviceName},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test",
					Image: imageutils.GetPauseImageName(),
				},
			},
		},
	}

	// Caution: StartPods requires at least one pod to replicate.
	// Based on the callers, replicas is always positive number: zoneCount >= 0 implies (2*zoneCount)+1 > 0.
	// Thus, no need to test for it. Once the precondition changes to zero number of replicas,
	// test for replicaCount > 0. Otherwise, StartPods panics.
	framework.ExpectNoError(testutils.StartPods(f.ClientSet, replicaCount, f.Namespace.Name, serviceName, *podSpec, false, framework.Logf))

	// Wait for all of them to be scheduled
	selector := labels.SelectorFromSet(labels.Set(map[string]string{"service": serviceName}))
	pods, err := e2epod.WaitForPodsWithLabelScheduled(f.ClientSet, f.Namespace.Name, selector)
	framework.ExpectNoError(err)

	// Now make sure they're spread across zones
	zoneNames, err := e2enode.GetClusterZones(f.ClientSet)
	framework.ExpectNoError(err)
	checkZoneSpreading(f.ClientSet, pods, zoneNames.List())
}

// Find the name of the zone in which a Node is running
func getZoneNameForNode(node v1.Node) (string, error) {
	for key, value := range node.Labels {
		if key == v1.LabelZoneFailureDomain {
			return value, nil
		}
	}
	return "", fmt.Errorf("Zone name for node %s not found. No label with key %s",
		node.Name, v1.LabelZoneFailureDomain)
}

// getNodeCount returns the number of nodes in this cluster.
func getNodeCount(c clientset.Interface) (int, error) {
	nodes, err := c.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("Error getting nodes: %w", err)
	}
	return len(nodes.Items), nil
}

// Return the number of zones in which we have nodes in this cluster.
func getZoneCount(c clientset.Interface) (int, error) {
	zoneNames, err := e2enode.GetClusterZones(c)
	if err != nil {
		return -1, err
	}
	return len(zoneNames), nil
}

// Find the name of the zone in which the pod is scheduled
func getZoneNameForPod(c clientset.Interface, pod v1.Pod) (string, error) {
	ginkgo.By(fmt.Sprintf("Getting zone name for pod %s, on node %s", pod.Name, pod.Spec.NodeName))
	node, err := c.CoreV1().Nodes().Get(context.TODO(), pod.Spec.NodeName, metav1.GetOptions{})
	framework.ExpectNoError(err)
	return getZoneNameForNode(*node)
}

// Determine whether a set of pods are approximately evenly spread
// across a given set of zones
func checkZoneSpreading(c clientset.Interface, pods *v1.PodList, zoneNames []string) {
	podsPerZone := make(map[string]int)
	for _, zoneName := range zoneNames {
		podsPerZone[zoneName] = 0
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		zoneName, err := getZoneNameForPod(c, pod)
		framework.ExpectNoError(err)
		podsPerZone[zoneName] = podsPerZone[zoneName] + 1
	}
	minPodsPerZone := math.MaxInt32
	maxPodsPerZone := 0
	for _, podCount := range podsPerZone {
		if podCount < minPodsPerZone {
			minPodsPerZone = podCount
		}
		if podCount > maxPodsPerZone {
			maxPodsPerZone = podCount
		}
	}

	// In all tests, we schedule 30 pods with zone spreading across three zones.
	//
	// The 100 node test uses 99 nodes. With such small clusters, the scoring
	// phase looks at all nodes to calculate spreading scores, which should
	// ideally result in a perfect 10/10/10 distribution across all three zones.
	// However, to account for unreliability in scoring due to external factors
	// (such as image locality), we allow for a 14/8/8 distribution; hence the
	// max skew of 6.
	//
	// Currently, the 5k test uses 4,998 nodes, which has an adaptive scoring
	// window size of 11% (see generic_scheduler.go). With 30 pods to schedule,
	// the first 30% of the nodes get 3 additional pods scheduled compared to the
	// rest of the cluster. This could potentially result in a pod distribution
	// of 12/9/9 (skew=3) across three zones. Similar to the 100 node test, to
	// account for unreliability in scoring, we allow for a 16/7/7 distribution;
	// hence the max skew of 9.
	//
	// This maxSkew calculation is specific to the cluster sizes we test with.
	// Redo this calculation if the node size changes in the future.
	var maxSkew int
	numNodes, err := getNodeCount(c)
	framework.ExpectNoError(err)
	if numNodes <= 100 {
		maxSkew = 6
	} else {
		maxSkew = 9
	}
	gomega.Expect(minPodsPerZone).To(gomega.BeNumerically("~", maxPodsPerZone, maxSkew),
		"Pods were not evenly spread across zones.  %d in one zone and %d in another zone",
		minPodsPerZone, maxPodsPerZone)
}

// SpreadRCOrFail Check that the pods comprising a replication
// controller get spread evenly across available zones
func SpreadRCOrFail(f *framework.Framework, replicaCount int32, image string, args []string) {
	name := "ubelite-spread-rc-" + string(uuid.NewUUID())
	ginkgo.By(fmt.Sprintf("Creating replication controller %s", name))
	controller, err := f.ClientSet.CoreV1().ReplicationControllers(f.Namespace.Name).Create(context.TODO(), &v1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: f.Namespace.Name,
			Name:      name,
		},
		Spec: v1.ReplicationControllerSpec{
			Replicas: &replicaCount,
			Selector: map[string]string{
				"name": name,
			},
			Template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"name": name},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  name,
							Image: image,
							Args:  args,
							Ports: []v1.ContainerPort{{ContainerPort: 9376}},
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	// Cleanup the replication controller when we are done.
	defer func() {
		// Resize the replication controller to zero to get rid of pods.
		if err := e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, controller.Name); err != nil {
			framework.Logf("Failed to cleanup replication controller %v: %v.", controller.Name, err)
		}
	}()
	// List the pods, making sure we observe all the replicas.
	selector := labels.SelectorFromSet(labels.Set(map[string]string{"name": name}))
	_, err = e2epod.PodsCreated(f.ClientSet, f.Namespace.Name, name, replicaCount)
	framework.ExpectNoError(err)

	// Wait for all of them to be scheduled
	ginkgo.By(fmt.Sprintf("Waiting for %d replicas of %s to be scheduled.  Selector: %v", replicaCount, name, selector))
	pods, err := e2epod.WaitForPodsWithLabelScheduled(f.ClientSet, f.Namespace.Name, selector)
	framework.ExpectNoError(err)

	// Now make sure they're spread across zones
	zoneNames, err := e2enode.GetClusterZones(f.ClientSet)
	framework.ExpectNoError(err)
	checkZoneSpreading(f.ClientSet, pods, zoneNames.List())
}
