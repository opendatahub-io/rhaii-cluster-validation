package rdma

import "testing"

func TestParseIBStat(t *testing.T) {
	tests := []struct {
		name string
		input string
		want []NICInfo
	}{
		{
			name: "single CA single port active",
			input: `CA 'mlx5_0'
	CA type: MT4123
	Number of ports: 1
	Firmware version: 20.31.1014
	Hardware version: 0
	Node GUID: 0x0017a4fffea6c260
	System image GUID: 0x0017a4fffea6c260
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 200
		Base lid: 0
		LMC: 0
		SM lid: 0
		Capability mask: 0x00010000
		Port GUID: 0x0017a4fffea6c260
		Link layer: InfiniBand`,
			want: []NICInfo{
				{Name: "mlx5_0/port1", State: "Active", Rate: "200"},
			},
		},
		{
			name: "single CA two ports",
			input: `CA 'mlx5_0'
	CA type: MT4123
	Number of ports: 2
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 200
	Port 2:
		State: Down
		Physical state: Disabled
		Rate: 10`,
			want: []NICInfo{
				{Name: "mlx5_0/port1", State: "Active", Rate: "200"},
				{Name: "mlx5_0/port2", State: "Down", Rate: "10"},
			},
		},
		{
			name: "multiple CAs",
			input: `CA 'mlx5_0'
	CA type: MT4123
	Number of ports: 1
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 200

CA 'mlx5_1'
	CA type: MT4123
	Number of ports: 1
	Port 1:
		State: Active
		Physical state: LinkUp
		Rate: 400`,
			want: []NICInfo{
				{Name: "mlx5_0/port1", State: "Active", Rate: "200"},
				{Name: "mlx5_1/port1", State: "Active", Rate: "400"},
			},
		},
		{
			name: "all ports down",
			input: `CA 'mlx5_0'
	CA type: MT4123
	Number of ports: 1
	Port 1:
		State: Down
		Physical state: Disabled
		Rate: 10

CA 'mlx5_1'
	CA type: MT4123
	Number of ports: 1
	Port 1:
		State: Down
		Physical state: Disabled
		Rate: 10`,
			want: []NICInfo{
				{Name: "mlx5_0/port1", State: "Down", Rate: "10"},
				{Name: "mlx5_1/port1", State: "Down", Rate: "10"},
			},
		},
		{
			name:  "empty output",
			input: "",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIBStat(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d NICs %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i, nic := range got {
				if nic.Name != tt.want[i].Name {
					t.Errorf("nic[%d].Name = %q, want %q", i, nic.Name, tt.want[i].Name)
				}
				if nic.State != tt.want[i].State {
					t.Errorf("nic[%d].State = %q, want %q", i, nic.State, tt.want[i].State)
				}
				if nic.Rate != tt.want[i].Rate {
					t.Errorf("nic[%d].Rate = %q, want %q", i, nic.Rate, tt.want[i].Rate)
				}
			}
		})
	}
}
