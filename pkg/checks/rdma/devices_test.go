package rdma

import "testing"

func TestParseIBVDevices(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name: "single device",
			input: `    device                 node GUID
    ------              ----------------
    mlx5_0              0017a4fffea6c26a`,
			want: []string{"mlx5_0"},
		},
		{
			name: "multiple devices",
			input: `    device                 node GUID
    ------              ----------------
    mlx5_0              0017a4fffea6c26a
    mlx5_1              0017a4fffea6c26b
    mlx5_2              0017a4fffea6c26c
    mlx5_3              0017a4fffea6c26d`,
			want: []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"},
		},
		{
			name: "header only no devices",
			input: `    device                 node GUID
    ------              ----------------`,
			want: nil,
		},
		{
			name:  "empty output",
			input: "",
			want:  nil,
		},
		{
			name: "eight NICs (typical H100 node)",
			input: `    device                 node GUID
    ------              ----------------
    mlx5_0              0017a4fffea6c260
    mlx5_1              0017a4fffea6c261
    mlx5_2              0017a4fffea6c262
    mlx5_3              0017a4fffea6c263
    mlx5_4              0017a4fffea6c264
    mlx5_5              0017a4fffea6c265
    mlx5_6              0017a4fffea6c266
    mlx5_7              0017a4fffea6c267`,
			want: []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3", "mlx5_4", "mlx5_5", "mlx5_6", "mlx5_7"},
		},
		{
			name: "device line without GUID",
			input: `    device                 node GUID
    ------              ----------------
    mlx5_0`,
			want: []string{"mlx5_0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIBVDevices(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d devices %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, d := range got {
				if d != tt.want[i] {
					t.Errorf("device[%d] = %q, want %q", i, d, tt.want[i])
				}
			}
		})
	}
}
