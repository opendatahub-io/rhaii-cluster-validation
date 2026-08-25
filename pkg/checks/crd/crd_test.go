package crd

import "testing"

func TestCompareAPIVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1", "v1", 0},
		{"v1", "v1beta1", 1},
		{"v1beta1", "v1", -1},
		{"v1beta2", "v1beta1", 1},
		{"v1beta1", "v1alpha1", 1},
		{"v1alpha2", "v1alpha1", 1},
		{"v2", "v1", 1},
		{"v2alpha1", "v1", 1},
		{"v1", "v1alpha1", 1},
		{"v1", "v1alpha2", 1},
		{"v1beta1", "v1beta1", 0},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			got := compareAPIVersions(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("compareAPIVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.4.0", "v1.3.1", 1},
		{"v1.3.1", "v1.4.0", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"1.4.0", "1.3.0", 1}, // no v prefix
		{"v1.4", "v1.3.0", 1}, // missing patch
		{"v1.0", "v1.0.0", 0}, // missing patch = 0
		{"v1.3.1", "v1.3.1", 0},
		{"v0.1.0", "v0.0.9", 1},
		{"v10.0.0", "v9.0.0", 1},
		// Pre-release handling
		{"v1.4.0", "v1.4.0-rc1", 1},  // release > pre-release
		{"v1.4.0-rc1", "v1.4.0", -1}, // pre-release < release
		{"v1.4.0-rc1", "v1.4.0-rc1", 0},
		{"v1.4.0-rc1", "v1.4.0-rc2", -1},
		{"v1.4.0-rc2", "v1.4.0-rc1", 1},
		{"v1.4.0-alpha1", "v1.4.0-beta1", -1}, // alpha < beta lexically
		{"v1.4.0-beta1", "v1.4.0-rc1", -1},    // beta < rc lexically
		{"v1.4.0-rc1", "v1.3.0", 1},           // higher numeric wins regardless of pre-release
		{"v1.3.0-rc1", "v1.4.0", -1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			got := compareSemver(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("compareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestExtractBundleVersion(t *testing.T) {
	crdJSON := []byte(`{
		"metadata": {
			"annotations": {
				"gateway.networking.k8s.io/bundle-version": "v1.4.0",
				"other-annotation": "foo"
			},
			"labels": {
				"app.kubernetes.io/version": "v2.1.0"
			}
		}
	}`)

	tests := []struct {
		name     string
		jsonPath string
		want     string
		wantErr  bool
	}{
		{
			name:     "annotation with dots",
			jsonPath: `{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}`,
			want:     "v1.4.0",
		},
		{
			name:     "simple annotation",
			jsonPath: `{.metadata.annotations.other-annotation}`,
			want:     "foo",
		},
		{
			name:     "label",
			jsonPath: `{.metadata.labels.app\.kubernetes\.io/version}`,
			want:     "v2.1.0",
		},
		{
			name:     "missing annotation",
			jsonPath: `{.metadata.annotations.nonexistent}`,
			want:     "",
		},
		{
			name:     "invalid jsonpath",
			jsonPath: `{invalid`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractBundleVersion(crdJSON, tt.jsonPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("extractBundleVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
