// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Marc Berhault (marc@cockroachlabs.com)

// +build acceptance

package acceptance

import (
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/acceptance/cluster"
	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
)

func countRangeReplicas(db *client.DB) (int, error) {
	desc := &roachpb.RangeDescriptor{}
	if err := db.GetProto(keys.RangeDescriptorKey(roachpb.RKeyMin), desc); err != nil {
		return 0, err
	}
	return len(desc.Replicas), nil
}

func checkRangeReplication(t util.Tester, c *cluster.LocalCluster, d time.Duration) {
	// Always talk to node 0.
	client, dbStopper := makeClient(t, c.ConnString(0))
	defer dbStopper.Stop()

	wantedReplicas := 3
	if len(c.Nodes) < 3 {
		wantedReplicas = len(c.Nodes)
	}

	log.Infof("waiting for first range to have %d replicas", wantedReplicas)

	util.SucceedsWithin(t, d, func() error {
		select {
		case <-stopper:
			t.Fatalf("interrupted")
			return nil
		case <-time.After(1 * time.Second):
		}

		foundReplicas, err := countRangeReplicas(client)
		if err != nil {
			return err
		}

		log.Infof("found %d replicas", foundReplicas)
		if foundReplicas >= wantedReplicas {
			return nil
		}
		return fmt.Errorf("expected %d replicas, only found %d", wantedReplicas, foundReplicas)
	})
}

func TestRangeReplication(t *testing.T) {
	if *numLocal == 0 {
		t.Skip("skipping since not run against local cluster")
	}
	l := cluster.CreateLocal(*numLocal, *logDir, stopper) // intentionally using local cluster
	l.Start()
	defer l.AssertAndStop(t)

	checkRangeReplication(t, l, 20*time.Second)
}
