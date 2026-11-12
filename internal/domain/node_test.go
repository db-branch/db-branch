package domain_test

import (
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestNode_一定時間連絡がなければ停止とみなす(t *testing.T) {
	n := domain.Node{LastSeen: now}
	if n.Down(now.Add(29*time.Second), 30*time.Second) {
		t.Error("29s: should be up")
	}
	if !n.Down(now.Add(31*time.Second), 30*time.Second) {
		t.Error("31s: should be down")
	}
}

func TestClusterProtocol_全ノードが話せる一番低い版(t *testing.T) {
	if got := domain.ClusterProtocol(nil, 4); got != 4 {
		t.Errorf("no members yet: %d", got)
	}
	nodes := []domain.Node{{Protocol: 4}, {Protocol: 3}, {Protocol: 4}}
	if got := domain.ClusterProtocol(nodes, 4); got != 3 {
		t.Errorf("mixed: %d", got)
	}
	if got := domain.ClusterProtocol(append(nodes, domain.Node{}), 4); got != 1 {
		t.Errorf("a node from before protocols: %d", got)
	}
}
