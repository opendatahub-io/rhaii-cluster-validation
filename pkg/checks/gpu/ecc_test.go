package gpu

import "testing"

func TestParseECCOutput(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantErrors int
		wantGPUs   int
		wantErr    bool
	}{
		{
			name:       "single GPU no errors",
			input:      "0, 0",
			wantErrors: 0,
			wantGPUs:   1,
		},
		{
			name: "four GPUs no errors",
			input: `0, 0
1, 0
2, 0
3, 0`,
			wantErrors: 0,
			wantGPUs:   4,
		},
		{
			name: "one GPU with errors",
			input: `0, 0
1, 5
2, 0
3, 0`,
			wantErrors: 1,
			wantGPUs:   4,
		},
		{
			name: "multiple GPUs with errors",
			input: `0, 3
1, 0
2, 12
3, 0`,
			wantErrors: 2,
			wantGPUs:   4,
		},
		{
			name:       "ECC not supported (N/A)",
			input:      "0, N/A",
			wantErrors: 0,
			wantGPUs:   1,
		},
		{
			name: "mixed N/A and zero",
			input: `0, N/A
1, 0
2, N/A
3, 0`,
			wantErrors: 0,
			wantGPUs:   4,
		},
		{
			name: "N/A with real errors on another GPU",
			input: `0, N/A
1, 7`,
			wantErrors: 1,
			wantGPUs:   2,
		},
		{
			name:       "empty error count treated as no error",
			input:      "0, ",
			wantErrors: 0,
			wantGPUs:   1,
		},
		{
			name:    "malformed CSV",
			input:   "\"unterminated",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors, gpuCount, err := parseECCOutput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("got %d errors %v, want %d", len(errors), errors, tt.wantErrors)
			}
			if gpuCount != tt.wantGPUs {
				t.Errorf("gpuCount = %d, want %d", gpuCount, tt.wantGPUs)
			}
		})
	}
}
