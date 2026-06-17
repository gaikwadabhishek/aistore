// Package etl provides ETL (Extract-Transform-Load) protocol support.
/*
 * Copyright (c) 2018-2026, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

//
// applyETLResourceDefaults
//

func TestApplyDefaultsNoContainers(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:       true,
		DefaultCPU:    "1",
		DefaultMemory: "512Mi",
	}
	pod := &corev1.Pod{}
	applyETLResourceDefaults(pod, etlConf)
	// No containers — should not panic
	if len(pod.Spec.Containers) != 0 {
		t.Error("expected no containers")
	}
}

func TestApplyDefaultsEmptyResources(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:       true,
		DefaultCPU:    "1",
		DefaultMemory: "512Mi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "transform"},
			},
		},
	}
	applyETLResourceDefaults(pod, etlConf)

	res := pod.Spec.Containers[0].Resources
	// Check requests
	cpuReq := res.Requests[corev1.ResourceCPU]
	if cpuReq.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("expected CPU request '1', got %s", cpuReq.String())
	}
	memReq := res.Requests[corev1.ResourceMemory]
	if memReq.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("expected memory request '512Mi', got %s", memReq.String())
	}
	// Check limits
	cpuLim := res.Limits[corev1.ResourceCPU]
	if cpuLim.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("expected CPU limit '1', got %s", cpuLim.String())
	}
	memLim := res.Limits[corev1.ResourceMemory]
	if memLim.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("expected memory limit '512Mi', got %s", memLim.String())
	}
}

func TestApplyDefaultsPreservesExisting(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:       true,
		DefaultCPU:    "1",
		DefaultMemory: "512Mi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}
	applyETLResourceDefaults(pod, etlConf)

	res := pod.Spec.Containers[0].Resources
	// Existing CPU request should be preserved
	cpuReq := res.Requests[corev1.ResourceCPU]
	if cpuReq.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("existing CPU request should be preserved, got %s", cpuReq.String())
	}
	// Missing memory request should get default
	memReq := res.Requests[corev1.ResourceMemory]
	if memReq.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("expected default memory request '512Mi', got %s", memReq.String())
	}
	// Existing memory limit should be preserved
	memLim := res.Limits[corev1.ResourceMemory]
	if memLim.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("existing memory limit should be preserved, got %s", memLim.String())
	}
	// Missing CPU limit should get default
	cpuLim := res.Limits[corev1.ResourceCPU]
	if cpuLim.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("expected default CPU limit '1', got %s", cpuLim.String())
	}
}

func TestApplyDefaultsOnlyDefaults(t *testing.T) {
	// No defaults configured — should not modify
	etlConf := &cmn.ETLConf{
		Enabled: true,
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "transform"},
			},
		},
	}
	applyETLResourceDefaults(pod, etlConf)

	res := pod.Spec.Containers[0].Resources
	if len(res.Requests) != 0 {
		t.Errorf("no defaults configured, requests should be empty, got %v", res.Requests)
	}
	if len(res.Limits) != 0 {
		t.Errorf("no defaults configured, limits should be empty, got %v", res.Limits)
	}
}

func TestApplyDefaultsMultipleContainers(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:       true,
		DefaultCPU:    "500m",
		DefaultMemory: "256Mi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "transform1"},
				{Name: "transform2"},
			},
		},
	}
	applyETLResourceDefaults(pod, etlConf)

	for i, c := range pod.Spec.Containers {
		cpuReq := c.Resources.Requests[corev1.ResourceCPU]
		if cpuReq.Cmp(resource.MustParse("500m")) != 0 {
			t.Errorf("container %d: expected CPU request '500m', got %s", i, cpuReq.String())
		}
		memReq := c.Resources.Requests[corev1.ResourceMemory]
		if memReq.Cmp(resource.MustParse("256Mi")) != 0 {
			t.Errorf("container %d: expected memory request '256Mi', got %s", i, memReq.String())
		}
	}
}

//
// validateETLResourceLimits
//

func TestValidateLimitsWithinBounds(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}
	if err := validateETLResourceLimits(pod, etlConf); err != nil {
		t.Errorf("resources within limits should pass: %v", err)
	}
}

func TestValidateLimitsExceedCPU(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}
	if err := validateETLResourceLimits(pod, etlConf); err == nil {
		t.Error("CPU exceeding max should fail validation")
	}
}

func TestValidateLimitsExceedMemory(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "1Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}
	if err := validateETLResourceLimits(pod, etlConf); err == nil {
		t.Error("memory exceeding max should fail validation")
	}
}

func TestValidateLimitsNoMaxConfigured(t *testing.T) {
	// No max limits set — should always pass
	etlConf := &cmn.ETLConf{
		Enabled: true,
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100"),
							corev1.ResourceMemory: resource.MustParse("100Gi"),
						},
					},
				},
			},
		},
	}
	if err := validateETLResourceLimits(pod, etlConf); err != nil {
		t.Errorf("no max configured, should pass: %v", err)
	}
}

func TestValidateLimitsNoResources(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "transform"},
			},
		},
	}
	// No resources specified — should pass (nothing to validate against)
	if err := validateETLResourceLimits(pod, etlConf); err != nil {
		t.Errorf("no resources specified should pass: %v", err)
	}
}

func TestValidateLimitsExactlyAtMax(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
					},
				},
			},
		},
	}
	// Exactly at max — should pass
	if err := validateETLResourceLimits(pod, etlConf); err != nil {
		t.Errorf("resources exactly at max should pass: %v", err)
	}
}

func TestValidateLimitsRequestsExceedMax(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
			},
		},
	}
	// Requests exceeding max should also fail
	if err := validateETLResourceLimits(pod, etlConf); err == nil {
		t.Error("CPU requests exceeding max should fail validation")
	}
}

func TestValidateLimitsMultipleContainers(t *testing.T) {
	etlConf := &cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "transform1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
				{
					Name: "transform2",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}
	// Second container exceeds CPU max
	if err := validateETLResourceLimits(pod, etlConf); err == nil {
		t.Error("second container exceeding max CPU should fail")
	}
}
