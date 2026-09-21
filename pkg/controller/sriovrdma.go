package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var nadGVR = schema.GroupVersionResource{
	Group:    "k8s.cni.cncf.io",
	Version:  "v1",
	Resource: "network-attachment-definitions",
}

type sriovRDMAPlan struct {
	resource string
	network  string
}

// resolveSRIOVRDMA pairs shared openshift.io/*rdma resources with a NAD.
// Skipped when jobs config already sets either half; never fails the run.
func (c *Controller) resolveSRIOVRDMA(ctx context.Context) {
	if c.dynamic == nil || config.ResourceConfigHasSRIOVRDMA(c.cfg.Jobs) {
		return
	}

	selected := c.selectedSRIOVRDMAResources()
	shared := config.IntersectSRIOVRDMAResources(selected)
	if len(shared) == 0 {
		for _, names := range selected {
			if len(names) > 0 {
				fmt.Fprintf(c.output, "  SR-IOV RDMA: skipping auto-detection, no resource is advertised by all %d selected node(s)\n",
					len(selected))
				break
			}
		}
		return
	}

	nadNamespaces := []string{c.opts.Namespace}
	if ns := c.cfg.Jobs.SRIOVRDMANADNamespace; ns != "" && ns != c.opts.Namespace {
		nadNamespaces = append(nadNamespaces, ns)
	}

	byResource, err := c.nadsByResource(ctx, nadNamespaces)
	if err != nil {
		fmt.Fprintf(c.output, "  SR-IOV RDMA: skipping auto-detection, cannot list NetworkAttachmentDefinitions: %v\n", err)
		return
	}

	var plans []sriovRDMAPlan
	var unmatched, ambiguous []string
	for _, res := range shared {
		switch networks := byResource[res]; len(networks) {
		case 0:
			unmatched = append(unmatched, res)
		case 1:
			plans = append(plans, sriovRDMAPlan{resource: res, network: networks[0]})
		default:
			sort.Strings(networks)
			ambiguous = append(ambiguous, fmt.Sprintf("%s (%s)", res, strings.Join(networks, ", ")))
		}
	}

	if len(unmatched) > 0 {
		fmt.Fprintf(c.output, "  SR-IOV RDMA: no NetworkAttachmentDefinition in %s for %s — create one, or set jobs.sriov_rdma_nad_namespace if it lives elsewhere\n",
			strings.Join(nadNamespaces, "/"), strings.Join(unmatched, ", "))
	}
	if len(ambiguous) > 0 {
		fmt.Fprintf(c.output, "  SR-IOV RDMA: multiple NetworkAttachmentDefinitions match %s — set jobs.requests and jobs.annotations explicitly to choose\n",
			strings.Join(ambiguous, "; "))
	}
	if len(plans) == 0 {
		return
	}

	c.sriovRDMAPlans = plans
	resources := make([]string, 0, len(plans))
	networks := make([]string, 0, len(plans))
	for _, p := range plans {
		resources = append(resources, p.resource)
		networks = append(networks, p.network)
	}
	fmt.Fprintf(c.output, "  SR-IOV RDMA: auto-detected %d rail(s): %s\n", len(plans), strings.Join(resources, ", "))
	fmt.Fprintf(c.output, "  SR-IOV RDMA: attaching %s\n", strings.Join(networks, ", "))
}

func (c *Controller) selectedSRIOVRDMAResources() map[string][]string {
	selected := make(map[string][]string, len(c.gpuNodes))
	for _, node := range c.gpuNodes {
		selected[node] = c.sriovRDMAResources[node]
	}
	return selected
}

func (c *Controller) nadsByResource(ctx context.Context, namespaces []string) (map[string][]string, error) {
	byResource := make(map[string][]string)
	for _, ns := range namespaces {
		list, err := c.dynamic.Resource(nadGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			res := item.GetAnnotations()[config.NADResourceAnnotation]
			if res == "" {
				continue
			}
			ref := item.GetName()
			if ns != c.opts.Namespace {
				ref = ns + "/" + item.GetName()
			}
			byResource[res] = append(byResource[res], ref)
		}
	}
	return byResource, nil
}

func (c *Controller) applySRIOVRDMA(container *corev1.Container, podAnnotations map[string]string) []string {
	if len(c.sriovRDMAPlans) == 0 {
		return nil
	}
	resources := make([]string, 0, len(c.sriovRDMAPlans))
	networks := make([]string, 0, len(c.sriovRDMAPlans))
	for _, p := range c.sriovRDMAPlans {
		setContainerResource(container, corev1.ResourceName(p.resource), 1)
		resources = append(resources, p.resource)
		networks = append(networks, p.network)
	}
	podAnnotations[config.MultusNetworksAnnotation] = strings.Join(networks, ",")
	return resources
}
