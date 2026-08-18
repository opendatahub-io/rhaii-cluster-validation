package checks

import "testing"

func TestMergeNodeReports_AppendsResultsForSameNode(t *testing.T) {
	gpuReports := []NodeReport{
		{Node: "node-1", Results: []Result{{Name: "gpu_driver_version", Status: StatusPass}}},
		{Node: "node-2", Results: []Result{{Name: "gpu_driver_version", Status: StatusFail}}},
	}
	netReports := []NodeReport{
		{Node: "node-1", Results: []Result{{Name: "rdma_devices_detected", Status: StatusPass}}},
	}

	merged := MergeNodeReports(gpuReports, netReports)

	if len(merged) != 2 {
		t.Fatalf("expected 2 merged node reports, got %d", len(merged))
	}
	if merged[0].Node != "node-1" || len(merged[0].Results) != 2 {
		t.Errorf("node-1: expected 2 merged results, got %d (%+v)", len(merged[0].Results), merged[0])
	}
	if merged[1].Node != "node-2" || len(merged[1].Results) != 1 {
		t.Errorf("node-2: expected 1 result, got %d (%+v)", len(merged[1].Results), merged[1])
	}
}

func TestMergeNodeReports_PreservesFirstAppearanceOrder(t *testing.T) {
	a := []NodeReport{{Node: "b"}, {Node: "a"}}
	b := []NodeReport{{Node: "c"}, {Node: "a"}}

	merged := MergeNodeReports(a, b)

	var order []string
	for _, r := range merged {
		order = append(order, r.Node)
	}
	want := []string{"b", "a", "c"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order = %v, want %v", order, want)
			break
		}
	}
}

func TestMergeNodeReports_EmptyInput(t *testing.T) {
	if merged := MergeNodeReports(); merged != nil {
		t.Errorf("expected nil for no input, got %+v", merged)
	}
	if merged := MergeNodeReports(nil, nil); merged != nil {
		t.Errorf("expected nil for empty report sets, got %+v", merged)
	}
}
