// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

func sizingContainer(name, cpu, memory string) computev1alpha.SandboxContainer {
	c := computev1alpha.SandboxContainer{Name: name, Image: "docker.io/library/nginx:latest"}
	if cpu != "" || memory != "" {
		c.Resources = &computev1alpha.ContainerResourceRequirements{Limits: core.ResourceList{}}
		if cpu != "" {
			c.Resources.Limits[core.ResourceCPU] = resource.MustParse(cpu)
		}
		if memory != "" {
			c.Resources.Limits[core.ResourceMemory] = resource.MustParse(memory)
		}
	}
	return c
}

func TestReconcile_ContainerRequestsMatchInstanceBudget(t *testing.T) {
	tests := []struct {
		name       string
		containers []computev1alpha.SandboxContainer
		published  bool
		requests   core.ResourceList
		wantCPU    []string
		wantMemory []string
	}{
		{
			name:       "one container keeps the complete tier",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "", "")},
			wantCPU:    []string{"1"}, wantMemory: []string{"2Gi"},
		},
		{
			name:       "two containers share one tier",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "", ""), sizingContainer("sidecar", "", "")},
			wantCPU:    []string{"500m", "500m"}, wantMemory: []string{"1Gi", "1Gi"},
		},
		{
			name:       "published tier shares indivisible remainder deterministically",
			containers: []computev1alpha.SandboxContainer{sizingContainer("a", "", ""), sizingContainer("b", "", ""), sizingContainer("c", "", "")},
			published:  true,
			wantCPU:    []string{"334m", "334m", "333m"}, wantMemory: []string{"3Mi", "2Mi", "2Mi"},
		},
		{
			name:       "partial limits reserve each dimension before dividing",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "250m", ""), sizingContainer("sidecar", "", "512Mi")},
			wantCPU:    []string{"250m", "750m"}, wantMemory: []string{"1536Mi", "512Mi"},
		},
		{
			name:       "complete explicit limits determine quota instead of tier",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "100m", "128Mi"), sizingContainer("sidecar", "300m", "256Mi")},
			published:  true,
			wantCPU:    []string{"100m", "300m"}, wantMemory: []string{"128Mi", "256Mi"},
			requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100"), core.ResourceMemory: resource.MustParse("100Gi")},
		},
		{
			name:       "complete CPU limits can share the memory budget",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "250m", ""), sizingContainer("sidecar", "750m", "")},
			wantCPU:    []string{"250m", "750m"}, wantMemory: []string{"1Gi", "1Gi"},
		},
		{
			name:       "stored instance requests take precedence over tier",
			containers: []computev1alpha.SandboxContainer{sizingContainer("app", "", ""), sizingContainer("sidecar", "", "")},
			requests:   core.ResourceList{core.ResourceCPU: resource.MustParse("1500m"), core.ResourceMemory: resource.MustParse("3Gi")},
			wantCPU:    []string{"750m", "750m"}, wantMemory: []string{"1536Mi", "1536Mi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance()
			instance.Spec.Runtime.Sandbox.Containers = tt.containers
			instance.Spec.Runtime.Resources.Requests = tt.requests
			objects := []client.Object{instance}
			if tt.published {
				typ := publishedInstanceType()
				typ.Spec.Resources.CPU = resource.MustParse("1001m")
				typ.Spec.Resources.Memory = resource.MustParse("7Mi")
				objects = append(objects, typ)
			}
			r, c := newReconciler(t, nil, objects...)
			before := instance.DeepCopy()
			if _, err := r.resolveInstanceTypeSizing(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(context.Background(), instanceRequest()); err != nil {
				t.Fatal(err)
			}
			pod, found := getPod(t, c)
			if !found || len(pod.Spec.Containers) != len(tt.wantCPU) {
				t.Fatal("expected all containers in backing Pod")
			}
			var cpuTotal, memoryTotal, wantCPUTotal, wantMemoryTotal resource.Quantity
			for i, container := range pod.Spec.Containers {
				cpu, memory := resource.MustParse(tt.wantCPU[i]), resource.MustParse(tt.wantMemory[i])
				assertLimitEqual(t, container.Resources.Limits, core.ResourceCPU, cpu)
				assertLimitEqual(t, container.Resources.Limits, core.ResourceMemory, memory)
				if container.Resources.Requests.Cpu().Cmp(cpu) != 0 || container.Resources.Requests.Memory().Cmp(memory) != 0 {
					t.Errorf("container %s requests do not match allocation", container.Name)
				}
				cpuTotal.Add(*container.Resources.Requests.Cpu())
				memoryTotal.Add(*container.Resources.Requests.Memory())
				wantCPUTotal.Add(cpu)
				wantMemoryTotal.Add(memory)
			}
			if cpuTotal.Cmp(wantCPUTotal) != 0 || memoryTotal.Cmp(wantMemoryTotal) != 0 {
				t.Errorf("Pod totals = %s/%s, want instance budget %s/%s", cpuTotal.String(), memoryTotal.String(), wantCPUTotal.String(), wantMemoryTotal.String())
			}
			if !reflect.DeepEqual(instance, before) {
				t.Fatal("sizing mutated the caller's instance")
			}
			var stored computev1alpha.Instance
			if err := c.Get(context.Background(), instanceRequest().NamespacedName, &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec, before.Spec) {
				t.Fatal("sizing changed the stored instance spec")
			}
		})
	}
}

func TestResolveInstanceTypeSizing_RejectsUnaccountedAllocations(t *testing.T) {
	tests := []struct {
		name, wantError string
		containers      []computev1alpha.SandboxContainer
	}{
		{"explicit limit exceeds tier", "exceeds the remaining", []computev1alpha.SandboxContainer{sizingContainer("a", "1200m", ""), sizingContainer("b", "", "")}},
		{"no CPU budget left for sidecar", "too small", []computev1alpha.SandboxContainer{sizingContainer("a", "1", ""), sizingContainer("b", "", "")}},
		{"partial limits leave unused CPU", "do not match", []computev1alpha.SandboxContainer{sizingContainer("a", "250m", ""), sizingContainer("b", "250m", "")}},
		{"explicit zero is not a fallback", "below the supported", []computev1alpha.SandboxContainer{sizingContainer("a", "0", "1Gi")}},
		{"negative limit is rejected", "below the supported", []computev1alpha.SandboxContainer{sizingContainer("a", "-1", "1Gi")}},
		{"CPU rounding would exceed explicit quota", "exceeds the remaining", []computev1alpha.SandboxContainer{sizingContainer("a", "0.5m", "1Mi"), sizingContainer("b", "0.5m", "1Mi")}},
		{"memory rounding would understate explicit quota", "do not match", []computev1alpha.SandboxContainer{sizingContainer("a", "1m", "1536Ki"), sizingContainer("b", "1m", "1536Ki")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance()
			instance.Spec.Runtime.Sandbox.Containers = tt.containers
			r, c := newReconciler(t, nil, instance)
			before := instance.DeepCopy()
			if result, err := r.Reconcile(context.Background(), instanceRequest()); err != nil || result.RequeueAfter != 0 {
				t.Fatalf("permanent allocation refusal must not retry: result=%v error=%v", result, err)
			}
			var stored computev1alpha.Instance
			if err := c.Get(context.Background(), instanceRequest().NamespacedName, &stored); err != nil {
				t.Fatal(err)
			}
			for _, conditionType := range []string{computev1alpha.InstanceProgrammed, computev1alpha.InstanceAvailable} {
				condition := apimeta.FindStatusCondition(stored.Status.Conditions, conditionType)
				if condition == nil || condition.Status != metav1.ConditionFalse ||
					condition.Reason != computev1alpha.InstanceProgrammedReasonConfigurationError ||
					!strings.Contains(condition.Message, tt.wantError) || !strings.Contains(condition.Message, "Adjust container limits") {
					t.Fatalf("%s must explain how to fix allocation: %+v", conditionType, condition)
				}
			}
			if _, found := getPod(t, c); found {
				t.Fatal("invalid sizing created a Pod")
			}
			if !reflect.DeepEqual(instance, before) {
				t.Fatal("failed sizing mutated the caller's instance")
			}
		})
	}
}

func TestResolveInstanceTypeSizing_CompleteLimitsNeedNoCatalog(t *testing.T) {
	instance := newTestInstance()
	instance.Spec.Runtime.Resources.InstanceType = "custom-unavailable"
	instance.Spec.Runtime.Sandbox.Containers = []computev1alpha.SandboxContainer{sizingContainer("a", "100m", "64Mi")}
	r, _ := newInterceptedReconciler(t, nil, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*computev1alpha.InstanceType); ok {
				t.Fatal("complete explicit limits must not read a catalog quota did not use")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, instance)
	if _, err := r.resolveInstanceTypeSizing(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
}
