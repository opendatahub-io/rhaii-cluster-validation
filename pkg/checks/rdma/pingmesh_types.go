package rdma

import (
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// PingMeshCategory classifies a NIC pair as rail or cross-rail.
type PingMeshCategory string

const (
	PingMeshCategoryRail  PingMeshCategory = "rail"
	PingMeshCategoryXRail PingMeshCategory = "xrail"
)

// PingMeshPairResult is the per-NIC-pair result emitted by client pods.
type PingMeshPairResult struct {
	SrcDev string `json:"src_dev"`
	DstDev string `json:"dst_dev"`
	Pass   bool   `json:"pass"`
	Error  string `json:"error,omitempty"`
}

// PingMeshReport holds summary + matrix for the main JSON report.
// Detailed failures go to a separate ConfigMap to avoid size bloat.
type PingMeshReport struct {
	Summary map[string]PingMeshCheckSummary `json:"summary"`
	Matrix  []PingMeshNodePair              `json:"matrix"`
}

// PingMeshCheckSummary represents one of the two aggregate checks (rail / xrail).
type PingMeshCheckSummary struct {
	Status  checks.Status `json:"status"`
	Passed  int           `json:"passed"`
	Total   int           `json:"total"`
	Message string        `json:"message"`
}

// PingMeshNodePair holds per-node-pair connectivity counts.
type PingMeshNodePair struct {
	NodeA string        `json:"node_a"`
	NodeB string        `json:"node_b"`
	Rail  PingMeshCount `json:"rail"`
	XRail PingMeshCount `json:"xrail"`
	All   PingMeshCount `json:"all"`
}

// PingMeshCount is a simple passed/total pair.
type PingMeshCount struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// PingMeshFailuresReport is stored in a separate ConfigMap (only when failures exist).
type PingMeshFailuresReport struct {
	Timestamp time.Time         `json:"timestamp"`
	Failures  []PingMeshFailure `json:"failures"`
}

// PingMeshFailure records a single NIC pair that failed connectivity.
type PingMeshFailure struct {
	NodeA    string           `json:"node_a"`
	NodeB    string           `json:"node_b"`
	SrcDev   string           `json:"src_dev"`
	DstDev   string           `json:"dst_dev"`
	Category PingMeshCategory `json:"category"`
	Error    string           `json:"error"`
	Attempt  int              `json:"attempt"`
}
