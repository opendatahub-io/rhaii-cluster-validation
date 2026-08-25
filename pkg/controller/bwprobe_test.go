package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeployLoopbackBWProbeJobs_NoFlatTopologyIsNoOp(t *testing.T) {
	c, _ := newTestController(nil)
	c.gpuNodes = []string{"node-a"}

	if err := c.deployLoopbackBWProbeJobs(context.Background(), nil); err != nil {
		t.Fatalf("deployLoopbackBWProbeJobs() error = %v", err)
	}
	if c.bwProbeMaxMatrixSize != 0 {
		t.Errorf("expected bwProbeMaxMatrixSize = 0 when no flat topology is present, got %d", c.bwProbeMaxMatrixSize)
	}

	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("expected no BW probe jobs to be created, got %d", len(jobs.Items))
	}
}

func TestCollectLoopbackBWResults_NoMatchingJobs(t *testing.T) {
	c, _ := newTestController(nil)

	results, err := c.collectLoopbackBWResults(context.Background(), checkJobLabelKey+"="+bwProbeLabelValue)
	if err != nil {
		t.Fatalf("collectLoopbackBWResults() error = %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results, got %v", results)
	}
}

func TestWaitAndCollectLoopbackBWProbeJobs_NoJobsReturnsImmediately(t *testing.T) {
	c, _ := newTestController(nil)

	results, err := c.waitAndCollectLoopbackBWProbeJobs(context.Background())
	if err != nil {
		t.Fatalf("waitAndCollectLoopbackBWProbeJobs() error = %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results when no BW probe jobs exist, got %v", results)
	}
}

func TestWaitForPodsGone_ReturnsWhenNoPodsMatch(t *testing.T) {
	c, _ := newTestController(nil)

	done := make(chan struct{})
	go func() {
		c.waitForPodsGone("app=does-not-exist", 10*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3500 * time.Millisecond):
		t.Fatal("waitForPodsGone did not return promptly (on the first 2s tick) when no pods match")
	}
}
