// Copyright 2014 The Cockroach Authors.
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
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/util"
	yaml "gopkg.in/yaml.v1"
)

// Attributes specifies a list of arbitrary strings describing
// node topology, store type, and machine capabilities.
type Attributes []string

// IsSubset returns whether attributes list b is a subset of
// attributes list a.
func (a Attributes) IsSubset(b Attributes) bool {
	m := map[string]struct{}{}
	for _, s := range []string(b) {
		m[s] = struct{}{}
	}
	for _, s := range []string(a) {
		if _, ok := m[s]; !ok {
			return false
		}
	}
	return true
}

// SortedString returns a sorted, de-duplicated, comma-separated list
// of the attributes.
func (a Attributes) SortedString() string {
	m := map[string]struct{}{}
	for _, s := range []string(a) {
		m[s] = struct{}{}
	}
	var attrs []string
	for a := range m {
		attrs = append(attrs, a)
	}
	sort.Strings(attrs)
	return strings.Join(attrs, ",")
}

// Replica describes a replica location by node ID (corresponds to a
// host:port via lookup on gossip network), store ID (corresponds to
// a physical device, unique per node) and range ID. Datacenter and
// DiskType are provided to optimize reads. Replicas are stored in
// Range lookup records (meta1, meta2).
type Replica struct {
	NodeID  int32
	StoreID int32
	RangeID int64
	Attrs   Attributes // combination of node & store attributes
}

// RangeDescriptor is the value stored in a range metadata key.
// A range is described using an inclusive start key, a non-inclusive end key,
// and a list of replicas where the range is stored.
type RangeDescriptor struct {
	// StartKey is the first key which may be contained by this range.
	StartKey Key
	// EndKey marks the end of the range's possible keys.  EndKey itself is not
	// contained in this range - it will be contained in the immediately
	// subsequent range.
	EndKey Key
	// List of replicas where this range is stored
	Replicas []Replica
}

// ContainsKey returns whether this RangeDescriptor contains the specified key.
func (r *RangeDescriptor) ContainsKey(key Key) bool {
	return !key.Less(r.StartKey) && key.Less(r.EndKey)
}

// ContainsKeyRange returns whether this RangeDescriptor contains the specified
// key range from start to end.
func (r *RangeDescriptor) ContainsKeyRange(start, end Key) bool {
	if len(end) == 0 {
		end = start
	}
	if end.Less(start) {
		panic(fmt.Sprintf("start key is larger than end key %q > %q", string(start), string(end)))
	}
	return !start.Less(r.StartKey) && !r.EndKey.Less(end)
}

// LookupKey returns the metadata key at which this range descriptor should be
// stored as a value.
func (r *RangeDescriptor) LookupKey() Key {
	return RangeMetaKey(r.EndKey)
}

// StoreCapacity contains capacity information for a storage device.
type StoreCapacity struct {
	Capacity  int64
	Available int64
}

// PercentAvail computes the percentage of disk space that is available.
func (sc StoreCapacity) PercentAvail() float64 {
	return float64(sc.Available) / float64(sc.Capacity)
}

// NodeDescriptor holds details on node physical/network topology.
type NodeDescriptor struct {
	NodeID  int32
	Address net.Addr
	Attrs   Attributes // node specific attributes (e.g. datacenter, machine info)
}

// StoreDescriptor holds store information including store attributes,
// node descriptor and store capacity.
type StoreDescriptor struct {
	StoreID  int32
	Attrs    Attributes // store specific attributes (e.g. ssd, hdd, mem)
	Node     NodeDescriptor
	Capacity StoreCapacity
}

// CombinedAttrs returns the full list of attributes for the store,
// including both the node and store attributes.
func (s *StoreDescriptor) CombinedAttrs() Attributes {
	var a []string
	a = append(a, []string(s.Node.Attrs)...)
	a = append(a, []string(s.Attrs)...)
	return Attributes(a)
}

// Less compares two StoreDescriptors based on percentage of disk available.
func (s StoreDescriptor) Less(b util.Ordered) bool {
	return s.Capacity.PercentAvail() < b.(StoreDescriptor).Capacity.PercentAvail()
}

// AcctConfig holds accounting configuration.
type AcctConfig struct {
	ClusterID string
}

// PermConfig holds permission configuration, specifying read/write ACLs.
type PermConfig struct {
	Read  []string `yaml:"read,omitempty"`  // ACL lists users with read permissions
	Write []string `yaml:"write,omitempty"` // ACL lists users with write permissions
}

// CanRead does a linear search for user to verify read permission.
func (p *PermConfig) CanRead(user string) bool {
	for _, u := range p.Read {
		if u == user {
			return true
		}
	}
	return false
}

// CanWrite does a linear search for user to verify write permission.
func (p *PermConfig) CanWrite(user string) bool {
	for _, u := range p.Write {
		if u == user {
			return true
		}
	}
	return false
}

// ZoneConfig holds configuration that is needed for a range of KV pairs.
type ZoneConfig struct {
	// Replicas is a slice of Attributes, each describing required
	// capabilities of each replica in the zone.
	Replicas      []Attributes `yaml:"replicas,omitempty,flow"`
	RangeMinBytes int64        `yaml:"range_min_bytes,omitempty"`
	RangeMaxBytes int64        `yaml:"range_max_bytes,omitempty"`
}

// ParseZoneConfig parses a YAML serialized ZoneConfig.
func ParseZoneConfig(in []byte) (*ZoneConfig, error) {
	z := &ZoneConfig{}
	err := yaml.Unmarshal(in, z)
	return z, err
}

// ToYAML serializes a ZoneConfig as YAML.
func (z *ZoneConfig) ToYAML() ([]byte, error) {
	return yaml.Marshal(z)
}

// ChooseRandomReplica returns a replica selected at random or nil if none exist.
func ChooseRandomReplica(replicas []Replica) *Replica {
	if len(replicas) == 0 {
		return nil
	}
	r := util.CachedRand
	return &replicas[r.Intn(len(replicas))]
}
