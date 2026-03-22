// Package cmn provides common low-level types and utilities for all aistore projects.
/*
 * Copyright (c) 2018-2026, NVIDIA CORPORATION. All rights reserved.
 */
package cmn_test

import (
	"testing"

	"github.com/NVIDIA/aistore/cmn"
)

func TestETLConfValidateDisabled(t *testing.T) {
	// Disabled config should always pass validation, even with empty fields
	c := cmn.ETLConf{Enabled: false}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled ETLConf should validate: %v", err)
	}
}

func TestETLConfValidateDefaults(t *testing.T) {
	// Valid config with all fields set
	c := cmn.ETLConf{
		Enabled:       true,
		MaxCPU:        "4",
		MaxMemory:     "8Gi",
		DefaultCPU:    "1",
		DefaultMemory: "512Mi",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid ETLConf should validate: %v", err)
	}
}

func TestETLConfValidateDefaultsExceedMax(t *testing.T) {
	// Default CPU exceeds max CPU — should fail
	c := cmn.ETLConf{
		Enabled:       true,
		MaxCPU:        "2",
		MaxMemory:     "8Gi",
		DefaultCPU:    "4",
		DefaultMemory: "512Mi",
	}
	if err := c.Validate(); err == nil {
		t.Error("ETLConf with default_cpu > max_cpu should fail validation")
	}

	// Default memory exceeds max memory — should fail
	c = cmn.ETLConf{
		Enabled:       true,
		MaxCPU:        "4",
		MaxMemory:     "1Gi",
		DefaultCPU:    "1",
		DefaultMemory: "2Gi",
	}
	if err := c.Validate(); err == nil {
		t.Error("ETLConf with default_memory > max_memory should fail validation")
	}
}

func TestETLConfValidateInvalidQuantities(t *testing.T) {
	tests := []struct {
		name string
		conf cmn.ETLConf
	}{
		{
			name: "invalid max_cpu",
			conf: cmn.ETLConf{Enabled: true, MaxCPU: "not-a-cpu", MaxMemory: "1Gi", DefaultCPU: "1", DefaultMemory: "512Mi"},
		},
		{
			name: "invalid max_memory",
			conf: cmn.ETLConf{Enabled: true, MaxCPU: "4", MaxMemory: "bad", DefaultCPU: "1", DefaultMemory: "512Mi"},
		},
		{
			name: "invalid default_cpu",
			conf: cmn.ETLConf{Enabled: true, MaxCPU: "4", MaxMemory: "1Gi", DefaultCPU: "xyz", DefaultMemory: "512Mi"},
		},
		{
			name: "invalid default_memory",
			conf: cmn.ETLConf{Enabled: true, MaxCPU: "4", MaxMemory: "1Gi", DefaultCPU: "1", DefaultMemory: "abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.conf.Validate(); err == nil {
				t.Errorf("%s: expected validation error for invalid quantity", tt.name)
			}
		})
	}
}

func TestETLConfValidatePartialFields(t *testing.T) {
	// Only max fields set (no defaults) — should be valid
	c := cmn.ETLConf{
		Enabled:   true,
		MaxCPU:    "4",
		MaxMemory: "8Gi",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("ETLConf with only max fields should validate: %v", err)
	}

	// Only default fields set (no max) — should be valid
	c = cmn.ETLConf{
		Enabled:       true,
		DefaultCPU:    "1",
		DefaultMemory: "512Mi",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("ETLConf with only default fields should validate: %v", err)
	}
}

func TestETLConfValidateMilliCPU(t *testing.T) {
	// Milli-CPU values (e.g., "500m")
	c := cmn.ETLConf{
		Enabled:       true,
		MaxCPU:        "2000m",
		MaxMemory:     "4Gi",
		DefaultCPU:    "500m",
		DefaultMemory: "256Mi",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("ETLConf with milli-CPU should validate: %v", err)
	}
}
