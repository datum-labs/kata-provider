// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// allocationError is permanent for a given instance spec. Reporting it as a
// configuration condition gives customers a remedy instead of an endless retry.
type allocationError struct {
	message string
}

func (e *allocationError) Error() string { return e.message }

func invalidAllocation(format string, args ...any) error {
	return &allocationError{message: fmt.Sprintf(format, args...)}
}

// explicitInstanceBudget follows Compute quota's precedence: complete limits
// on every container, then complete instance-level requests. These forms can
// appear on stored/internal instances even though customer admission currently
// disallows explicit resource requirements.
func explicitInstanceBudget(instance *computev1alpha.Instance) (cpu, memory resource.Quantity, ok bool) {
	complete := true
	for _, container := range instance.Spec.Runtime.Sandbox.Containers {
		if container.Resources == nil {
			complete = false
			break
		}
		c, hasCPU := container.Resources.Limits[core.ResourceCPU]
		m, hasMemory := container.Resources.Limits[core.ResourceMemory]
		if !hasCPU || !hasMemory {
			complete = false
			break
		}
		cpu.Add(c)
		memory.Add(m)
	}
	if complete {
		return cpu, memory, true
	}
	cpu, hasCPU := instance.Spec.Runtime.Resources.Requests[core.ResourceCPU]
	memory, hasMemory := instance.Spec.Runtime.Resources.Requests[core.ResourceMemory]
	return cpu, memory, hasCPU && hasMemory
}

func allocateInstanceBudget(instance *computev1alpha.Instance, cpu, memory resource.Quantity) (*computev1alpha.Instance, error) {
	sized := instance.DeepCopy()
	for i := range sized.Spec.Runtime.Sandbox.Containers {
		container := &sized.Spec.Runtime.Sandbox.Containers[i]
		if container.Resources == nil {
			container.Resources = &computev1alpha.ContainerResourceRequirements{}
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = core.ResourceList{}
		}
	}
	// The shared Pod builder and quota resolve CPU in millicores and memory in
	// MiB. Split in those same units so rounding cannot invent extra requests.
	if err := allocateContainerResource(sized, core.ResourceCPU, cpu.MilliValue()); err != nil {
		return nil, err
	}
	if err := allocateContainerResource(sized, core.ResourceMemory, memory.Value()/(1024*1024)); err != nil {
		return nil, err
	}
	return sized, nil
}

// Reserve explicit limits first and divide the rest equally. List order
// determines who receives each leftover unit, keeping Pod builds repeatable.
// Reject an impossible split rather than overwrite an explicit limit or exceed
// the instance's quota. Zero allocations would trigger the builder's fallback.
func allocateContainerResource(instance *computev1alpha.Instance, name core.ResourceName, budget int64) error {
	containers := instance.Spec.Runtime.Sandbox.Containers
	remaining := budget
	unspecified := make([]int, 0, len(containers))
	for i, container := range containers {
		limit, exists := container.Resources.Limits[name]
		if !exists {
			unspecified = append(unspecified, i)
			continue
		}
		units := limit.MilliValue()
		if name == core.ResourceMemory {
			units = limit.Value() / (1024 * 1024)
		}
		if limit.Sign() <= 0 || units <= 0 {
			return invalidAllocation("container %q has a %s limit below the supported allocation unit", container.Name, name)
		}
		if units > remaining {
			return invalidAllocation("container %q %s limit exceeds the remaining instance allocation", container.Name, name)
		}
		remaining -= units
	}
	if len(unspecified) == 0 {
		if remaining != 0 {
			return invalidAllocation("explicit container %s limits do not match the instance allocation", name)
		}
		return nil
	}
	count := int64(len(unspecified))
	if remaining < count {
		return invalidAllocation("instance %s allocation is too small for %d unspecified containers", name, count)
	}
	share, extra := remaining/count, remaining%count
	for position, index := range unspecified {
		units := share
		if int64(position) < extra {
			units++
		}
		quantity := resource.NewMilliQuantity(units, resource.DecimalSI)
		if name == core.ResourceMemory {
			quantity = resource.NewQuantity(units*1024*1024, resource.BinarySI)
		}
		containers[index].Resources.Limits[name] = *quantity
	}
	return nil
}
