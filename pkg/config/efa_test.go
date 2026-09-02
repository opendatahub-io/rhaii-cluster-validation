package config

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGetEFACount(t *testing.T) {
	key := string(EFAResourceName)
	tests := []struct {
		name string
		cfg  ResourceConfig
		want int64
	}{
		{"requests", ResourceConfig{Requests: map[string]string{key: "32"}}, 32},
		{"limits only", ResourceConfig{Limits: map[string]string{key: "8"}}, 8},
		{"requests beat limits", ResourceConfig{
			Requests: map[string]string{key: "16"},
			Limits:   map[string]string{key: "32"},
		}, 16},
		{"malformed", ResourceConfig{Requests: map[string]string{key: "bad"}}, 0},
		{"fractional", ResourceConfig{Limits: map[string]string{key: "32.5"}}, 0},
		{"negative", ResourceConfig{Requests: map[string]string{key: "-1"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.GetEFACount(); got != tt.want {
				t.Errorf("GetEFACount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAutoEFACountHonorsLimitOnlyConfig(t *testing.T) {
	cfg := ResourceConfig{Limits: map[string]string{string(EFAResourceName): "8"}}
	alloc := corev1.ResourceList{
		EFAResourceName: *resource.NewQuantity(32, resource.DecimalSI),
	}
	got := AutoEFACount(alloc, ResourceConfigHasEFA(cfg), cfg.GetEFACount())
	if got != 8 {
		t.Errorf("AutoEFACount = %d, want configured limit 8", got)
	}
}
