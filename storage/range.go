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
	"bytes"
	"encoding/gob"
	"reflect"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/hlc"
	"github.com/cockroachdb/cockroach/util"
	"github.com/golang/glog"
)

// init pre-registers RangeDescriptor and PrefixConfigMap types.
func init() {
	gob.Register(RangeDescriptor{})
	gob.Register(StoreDescriptor{})
	gob.Register(PrefixConfigMap{})
	gob.Register(&AcctConfig{})
	gob.Register(&PermConfig{})
	gob.Register(&ZoneConfig{})
}

// ttlClusterIDGossip is time-to-live for cluster ID. The cluster ID
// serves as the sentinel gossip key which informs a node whether or
// not it's connected to the primary gossip network and not just a
// partition. As such it must expire on a reasonable basis and be
// continually re-gossipped. The replica which is the raft leader of
// the first range gossips it.
const ttlClusterIDGossip = 30 * time.Second

// configPrefixes describes administrative configuration maps
// affecting ranges of the key-value map by key prefix.
var configPrefixes = []struct {
	keyPrefix Key         // Range key prefix
	gossipKey string      // Gossip key
	configI   interface{} // Config struct interface
	dirty     bool        // Info in this config has changed; need to re-init and gossip
}{
	{KeyConfigAccountingPrefix, gossip.KeyConfigAccounting, AcctConfig{}, true},
	{KeyConfigPermissionPrefix, gossip.KeyConfigPermission, PermConfig{}, true},
	{KeyConfigZonePrefix, gossip.KeyConfigZone, ZoneConfig{}, true},
}

// The following are the method names supported by the KV API.
const (
	Contains            = "Contains"
	Get                 = "Get"
	Put                 = "Put"
	ConditionalPut      = "ConditionalPut"
	Increment           = "Increment"
	Scan                = "Scan"
	Delete              = "Delete"
	DeleteRange         = "DeleteRange"
	EndTransaction      = "EndTransaction"
	AccumulateTS        = "AccumulateTS"
	ReapQueue           = "ReapQueue"
	EnqueueUpdate       = "EnqueueUpdate"
	EnqueueMessage      = "EnqueueMessage"
	InternalRangeLookup = "InternalRangeLookup"
)

// readMethods specifies the set of methods which read and return data.
var readMethods = map[string]struct{}{
	Contains:            struct{}{},
	Get:                 struct{}{},
	ConditionalPut:      struct{}{},
	Increment:           struct{}{},
	Scan:                struct{}{},
	ReapQueue:           struct{}{},
	InternalRangeLookup: struct{}{},
}

// writeMethods specifies the set of methods which write data.
var writeMethods = map[string]struct{}{
	Put:            struct{}{},
	ConditionalPut: struct{}{},
	Increment:      struct{}{},
	Delete:         struct{}{},
	DeleteRange:    struct{}{},
	EndTransaction: struct{}{},
	AccumulateTS:   struct{}{},
	ReapQueue:      struct{}{},
	EnqueueUpdate:  struct{}{},
	EnqueueMessage: struct{}{},
}

// NeedReadPerm returns true if the specified method requires read permissions.
func NeedReadPerm(method string) bool {
	_, ok := readMethods[method]
	return ok
}

// NeedWritePerm returns true if the specified method requires write permissions.
func NeedWritePerm(method string) bool {
	_, ok := writeMethods[method]
	return ok
}

// IsReadOnly returns true if the specified method only requires read permissions.
func IsReadOnly(method string) bool {
	return !NeedWritePerm(method)
}

// A RangeMetadata holds information about the range.  This includes the cluster
// ID, the range ID, and a RangeDescriptor describing the contents of the range.
type RangeMetadata struct {
	RangeDescriptor
	ClusterID string
	RangeID   int64
}

// A Range is a contiguous keyspace with writes managed via an
// instance of the Raft consensus algorithm. Many ranges may exist
// in a store and they are unlikely to be contiguous. Ranges are
// independent units and are responsible for maintaining their own
// integrity by replacing failed replicas, splitting and merging
// as appropriate.
type Range struct {
	Meta      RangeMetadata
	engine    Engine         // The underlying key-value store
	allocator *allocator     // Makes allocation decisions
	gossip    *gossip.Gossip // Range may gossip based on contents
	raft      chan *Cmd      // Raft commands
	closer    chan struct{}  // Channel for closing the range

	sync.RWMutex                     // Protects readQ, tsCache & respCache.
	readQ        *ReadQueue          // Reads queued behind pending writes
	tsCache      *ReadTimestampCache // Most recent read timestamps for keys / key ranges
	respCache    *ResponseCache      // Provides idempotence for retries
}

// NewRange initializes the range starting at key.
func NewRange(meta RangeMetadata, clock *hlc.HLClock, engine Engine,
	allocator *allocator, gossip *gossip.Gossip) *Range {
	r := &Range{
		Meta:      meta,
		engine:    engine,
		allocator: allocator,
		gossip:    gossip,
		raft:      make(chan *Cmd, 10), // TODO(spencer): remove
		closer:    make(chan struct{}),
		readQ:     NewReadQueue(),
		tsCache:   NewReadTimestampCache(clock),
		respCache: NewResponseCache(meta.RangeID, engine),
	}
	return r
}

// Start begins gossiping and starts the raft command processing
// loop in a goroutine.
func (r *Range) Start() {
	r.maybeGossipClusterID()
	r.maybeGossipFirstRange()
	r.maybeGossipConfigs()
	go r.processRaft() // TODO(spencer): remove
	// Only start gossiping if this range is the first range.
	if r.IsFirstRange() {
		go r.startGossip()
	}
}

// Stop ends the log processing loop.
func (r *Range) Stop() {
	close(r.closer)
}

// IsFirstRange returns true if this is the first range.
func (r *Range) IsFirstRange() bool {
	return bytes.Equal(r.Meta.StartKey, KeyMin)
}

// IsLeader returns true if this range replica is the raft leader.
// TODO(spencer): this is always true for now.
func (r *Range) IsLeader() bool {
	return true
}

// ContainsKey returns whether this range contains the specified key.
func (r *Range) ContainsKey(key Key) bool {
	return r.Meta.ContainsKey(key)
}

// ContainsKeyRange returns whether this range contains the specified
// key range from start to end.
func (r *Range) ContainsKeyRange(start, end Key) bool {
	return r.Meta.ContainsKeyRange(start, end)
}

// EnqueueCmd enqueues a command to Raft.
func (r *Range) EnqueueCmd(cmd *Cmd) error {
	r.raft <- cmd
	return <-cmd.done
}

// ReadOnlyCmd updates the read timestamp cache and waits for any
// overlapping writes currently processing through Raft ahead of us to
// clear via the read queue.
func (r *Range) ReadOnlyCmd(method string, header *RequestHeader, args, reply interface{}) error {
	r.Lock()
	r.tsCache.Add(header.Key, header.EndKey, header.Timestamp)
	var wg sync.WaitGroup
	r.readQ.AddRead(header.Key, header.EndKey, &wg)
	r.Unlock()
	wg.Wait()

	// It's possible that arbitrary delays (e.g. major GC, VM
	// de-prioritization, etc.) could cause the execution of this read
	// command to occur AFTER the range replica has lost leadership.
	//
	// There is a chance that we waited on writes, and although they
	// were committed to the log, they weren't successfully applied to
	// this replica's state machine. We re-verify leadership before
	// reading to make sure that all pending writes are persisted.
	//
	// There are some elaborate cases where we might have lost
	// leadership and then regained it during the delay, but this is ok
	// because any writes during that period necessarily had higher
	// timestamps. This is because the read-timestamp-cache prevents it
	// for the active leader and leadership changes force the
	// read-timestamp-cache to reset its high water mark.
	if !r.IsLeader() {
		// TODO(spencer): when we happen to know the leader, fill it in here via replica.
		return &NotLeaderError{}
	}
	return r.executeCmd(method, args, reply)
}

// ReadWriteCmd first consults the response cache to determine whether
// this command has already been sent to the range. If a response is
// found, it's returned immediately and not submitted to raft. Next,
// the read timestamp cache is checked to determine if any newer reads
// to this command's affected keys have been made. If so, this
// command's timestamp is moved forward. Finally the keys affected by
// this command are added as pending writes to the read queue and the
// command is submitted to Raft. Upon completion, the write is removed
// from the read queue and the reply is added to the repsonse cache.
func (r *Range) ReadWriteCmd(method string, header *RequestHeader, args, reply interface{}) error {
	// Check the response cache in case this is a replay. This call
	// may block if the same command is already underway.
	if ok, err := r.respCache.GetResponse(header.CmdID, reply); ok || err != nil {
		if ok { // this is a replay! extract error for return
			err := reflect.ValueOf(reply).Elem().FieldByName("Error").Interface()
			if err != nil {
				return err.(error)
			}
			return nil
		}
		// In this case there was an error reading from the response
		// cache. Instead of failing the request just because we can't
		// decode the reply in the response cache, we proceed as though
		// idempotence has expired.
		glog.Errorf("unable to read result for %+v from the response cache: %v", args, err)
	}

	// One of the prime invariants of Cockroach is that a mutating command
	// cannot write a key with an earlier timestamp than the most recent
	// read of the same key. So first order of business here is to check
	// the read timestamp cache for reads which are more recent than the
	// timestamp of this write. If more recent, we simply update the
	// write's timestamp before enqueuing it for execution. When the write
	// returns, the updated timestamp will inform the final commit
	// timestamp.
	r.Lock() // Protect access to timestamp cache and read queue.
	if ts := r.tsCache.GetMax(header.Key, header.EndKey); header.Timestamp.Less(ts) {
		// Update both the incoming request and outgoing reply timestamps.
		ts.Logical++ // increment logical component by one to differentiate.
		header.Timestamp = ts
		replyVal := reflect.ValueOf(reply)
		reflect.Indirect(replyVal).FieldByName("Timestamp").Set(reflect.ValueOf(ts))
	}

	// The next step is to add the write to the read queue to inform
	// subsequent reads that there is a pending write. Reads which
	// overlap pending writes must wait for those writes to complete.
	wKey := r.readQ.AddWrite(header.Key, header.EndKey)
	r.Unlock()

	// Create command and enqueue for Raft.
	cmd := &Cmd{
		Method:   method,
		Args:     args,
		Reply:    reply,
		ReadOnly: IsReadOnly(method),
		done:     make(chan error, 1),
	}
	// This waits for the command to complete.
	err := r.EnqueueCmd(cmd)

	// Now that the command has completed, remove the pending write.
	r.Lock()
	r.readQ.RemoveWrite(wKey)
	r.Unlock()

	return err
}

// processRaft processes read/write commands, sending them to the Raft
// consensus algorithm. This method processes indefinitely or until
// Range.Stop() is invoked.
//
// TODO(spencer): this is pretty temporary. Just executing commands
//   immediately until Raft is in place.
//
// TODO(bdarnell): when Raft elects this range replica as the leader,
//   we need to be careful to do the following before the range is
//   allowed to believe it's the leader and begin to accept writes and
//   reads:
//     - Push noop command to raft followers in order to verify the
//       committed entries in the log.
//     - Apply all committed log entries to the state machine.
//     - Signal the range to clear its read timestamp, response caches
//       and pending read queue.
//     - Signal the range that it's now the leader with the duration
//       of its leader lease.
//   If we don't do this, then a read which was previously gated on
//   the former leader waiting for overlapping writes to commit to
//   the underlying state machine, might transit to the new leader
//   and be able to access the new leader's state machine BEFORE
//   the overlapping writes are applied.
func (r *Range) processRaft() {
	for {
		select {
		case cmd := <-r.raft:
			cmd.done <- r.executeCmd(cmd.Method, cmd.Args, cmd.Reply)
		case <-r.closer:
			return
		}
	}
}

// startGossip periodically gossips the cluster ID if it's the
// first range and the raft leader.
func (r *Range) startGossip() {
	ticker := time.NewTicker(ttlClusterIDGossip / 2)
	for {
		select {
		case <-ticker.C:
			r.maybeGossipClusterID()
			r.maybeGossipFirstRange()
		case <-r.closer:
			return
		}
	}
}

// maybeGossipClusterID gossips the cluster ID if this range is
// the start of the key space and the raft leader.
func (r *Range) maybeGossipClusterID() {
	if r.gossip != nil && r.IsFirstRange() && r.IsLeader() {
		if err := r.gossip.AddInfo(gossip.KeyClusterID, r.Meta.ClusterID, ttlClusterIDGossip); err != nil {
			glog.Errorf("failed to gossip cluster ID %s: %v", r.Meta.ClusterID, err)
		}
	}
}

// maybeGossipFirstRange gossips the range locations if this range is
// the start of the key space and the raft leader.
func (r *Range) maybeGossipFirstRange() {
	if r.gossip != nil && r.IsFirstRange() && r.IsLeader() {
		if err := r.gossip.AddInfo(gossip.KeyFirstRangeMetadata, r.Meta.RangeDescriptor, 0*time.Second); err != nil {
			glog.Errorf("failed to gossip first range metadata: %v", err)
		}
	}
}

// maybeGossipConfigs gossips configuration maps if their data falls
// within the range, this replica is the raft leader, and their
// contents are marked dirty. Configuration maps include accounting,
// permissions, and zones.
func (r *Range) maybeGossipConfigs() {
	if r.gossip != nil && r.IsLeader() {
		for _, cp := range configPrefixes {
			if cp.dirty && r.ContainsKey(cp.keyPrefix) {
				configMap, err := r.loadConfigMap(cp.keyPrefix, cp.configI)
				if err != nil {
					glog.Errorf("failed loading %s config map: %v", cp.gossipKey, err)
					continue
				} else {
					if err := r.gossip.AddInfo(cp.gossipKey, configMap, 0*time.Second); err != nil {
						glog.Errorf("failed to gossip %s configMap: %v", cp.gossipKey, err)
						continue
					}
				}
				cp.dirty = false
			}
		}
	}
}

// loadConfigMap scans the config entries under keyPrefix and
// instantiates/returns a config map. Prefix configuration maps
// include accounting, permissions, and zones.
func (r *Range) loadConfigMap(keyPrefix Key, configI interface{}) (PrefixConfigMap, error) {
	// TODO(spencer): need to make sure range splitting never
	// crosses a configuration map's key prefix.
	kvs, err := r.engine.scan(keyPrefix, PrefixEndKey(keyPrefix), 0)
	if err != nil {
		return nil, err
	}
	var configs []*PrefixConfig
	for _, kv := range kvs {
		// Instantiate an instance of the config type by unmarshalling
		// gob encoded config from the Value into a new instance of configI.
		config := reflect.New(reflect.TypeOf(configI)).Interface()
		if err := gob.NewDecoder(bytes.NewBuffer(kv.value)).Decode(config); err != nil {
			return nil, util.Errorf("unable to unmarshal config key %s: %v", string(kv.key), err)
		}
		configs = append(configs, &PrefixConfig{Prefix: bytes.TrimPrefix(kv.key, keyPrefix), Config: config})
	}
	return NewPrefixConfigMap(configs)
}

// executeCmd switches over the method and multiplexes to execute the
// appropriate storage API command.
func (r *Range) executeCmd(method string, args, reply interface{}) error {
	switch method {
	case Contains:
		r.Contains(args.(*ContainsRequest), reply.(*ContainsResponse))
	case Get:
		r.Get(args.(*GetRequest), reply.(*GetResponse))
	case Put:
		r.Put(args.(*PutRequest), reply.(*PutResponse))
	case ConditionalPut:
		r.ConditionalPut(args.(*ConditionalPutRequest), reply.(*ConditionalPutResponse))
	case Increment:
		r.Increment(args.(*IncrementRequest), reply.(*IncrementResponse))
	case Delete:
		r.Delete(args.(*DeleteRequest), reply.(*DeleteResponse))
	case DeleteRange:
		r.DeleteRange(args.(*DeleteRangeRequest), reply.(*DeleteRangeResponse))
	case Scan:
		r.Scan(args.(*ScanRequest), reply.(*ScanResponse))
	case EndTransaction:
		r.EndTransaction(args.(*EndTransactionRequest), reply.(*EndTransactionResponse))
	case AccumulateTS:
		r.AccumulateTS(args.(*AccumulateTSRequest), reply.(*AccumulateTSResponse))
	case ReapQueue:
		r.ReapQueue(args.(*ReapQueueRequest), reply.(*ReapQueueResponse))
	case EnqueueUpdate:
		r.EnqueueUpdate(args.(*EnqueueUpdateRequest), reply.(*EnqueueUpdateResponse))
	case EnqueueMessage:
		r.EnqueueMessage(args.(*EnqueueMessageRequest), reply.(*EnqueueMessageResponse))
	case InternalRangeLookup:
		r.InternalRangeLookup(args.(*InternalRangeLookupRequest), reply.(*InternalRangeLookupResponse))
	default:
		return util.Errorf("unrecognized command type: %s", method)
	}

	// Add this command's result to the response cache if this is a
	// read/write method. This must be done as part of the execution of
	// raft commands so that every replica maintains the same responses
	// to continue request idempotence when leadership changes.
	if !IsReadOnly(method) {
		cmdID := reflect.ValueOf(args).Elem().FieldByName("CmdID").Interface().(ClientCmdID)
		if putErr := r.respCache.PutResponse(cmdID, reply); putErr != nil {
			glog.Errorf("unable to write result of %+v: %+v to the response cache: %v",
				args, reply, putErr)
		}
	}

	// Return the error (if any) set in the reply.
	err := reflect.ValueOf(reply).Elem().FieldByName("Error").Interface()
	if err != nil {
		return err.(error)
	}
	return nil
}

// Contains verifies the existence of a key in the key value store.
func (r *Range) Contains(args *ContainsRequest, reply *ContainsResponse) {
	val, err := r.engine.get(args.Key)
	if err != nil {
		reply.Error = err
		return
	}
	if val != nil {
		reply.Exists = true
	}
}

// Get returns the value for a specified key.
func (r *Range) Get(args *GetRequest, reply *GetResponse) {
	val, err := r.engine.get(args.Key)
	reply.Value = Value{Bytes: val}
	reply.Error = err
}

// Put sets the value for a specified key.
func (r *Range) Put(args *PutRequest, reply *PutResponse) {
	reply.Error = r.internalPut(args.Key, args.Value)
}

// ConditionalPut sets the value for a specified key only if
// the expected value matches. If not, the return value contains
// the actual value.
func (r *Range) ConditionalPut(args *ConditionalPutRequest, reply *ConditionalPutResponse) {
	// Handle check for non-existence of key.
	val, err := r.engine.get(args.Key)
	if err != nil {
		reply.Error = err
		return
	}
	if args.ExpValue.Bytes == nil && val != nil {
		reply.Error = util.Errorf("key %q already exists", args.Key)
		return
	} else if args.ExpValue.Bytes != nil {
		// Handle check for existence when there is no key.
		if val == nil {
			reply.Error = util.Errorf("key %q does not exist", args.Key)
			return
		} else if !bytes.Equal(args.ExpValue.Bytes, val) {
			// TODO(Jiang-Ming): provide the correct timestamp once switch to use MVCC
			reply.ActualValue = &Value{Bytes: val}
			reply.Error = util.Errorf("key %q does not match existing", args.Key)
			return
		}
	}

	reply.Error = r.internalPut(args.Key, args.Value)
}

// internalPut is the guts of the put method, called from both Put()
// and ConditionalPut().
func (r *Range) internalPut(key Key, value Value) error {
	// Put the value.
	// TODO(Tobias): Turn this into a writebatch with account stats in a reusable way.
	// This requires use of RocksDB's merge operator to implement increasable counters
	if err := r.engine.put(key, value.Bytes); err != nil {
		return err
	}
	// Check whether this put has modified a configuration map.
	for _, cp := range configPrefixes {
		if bytes.HasPrefix(key, cp.keyPrefix) {
			cp.dirty = true
			r.maybeGossipConfigs()
			break
		}
	}
	return nil
}

// Increment increments the value (interpreted as varint64 encoded) and
// returns the newly incremented value (encoded as varint64). If no value
// exists for the key, zero is incremented.
func (r *Range) Increment(args *IncrementRequest, reply *IncrementResponse) {
	reply.NewValue, reply.Error = increment(r.engine, args.Key, args.Increment)
}

// Delete deletes the key and value specified by key.
func (r *Range) Delete(args *DeleteRequest, reply *DeleteResponse) {
	if err := r.engine.clear(args.Key); err != nil {
		reply.Error = err
	}
}

// DeleteRange deletes the range of key/value pairs specified by
// start and end keys.
func (r *Range) DeleteRange(args *DeleteRangeRequest, reply *DeleteRangeResponse) {
	reply.Error = util.Error("unimplemented")
}

// Scan scans the key range specified by start key through end key up
// to some maximum number of results. The last key of the iteration is
// returned with the reply.
func (r *Range) Scan(args *ScanRequest, reply *ScanResponse) {
	kvs, err := r.engine.scan(args.Key, args.EndKey, args.MaxResults)
	if err != nil {
		reply.Error = err
		return
	}
	reply.Rows = make([]KeyValue, len(kvs))
	for idx, kv := range kvs {
		// TODO(Jiang-Ming): provide the correct timestamp and checksum once switch to mvcc
		reply.Rows[idx] = KeyValue{Key: kv.key, Value: Value{Bytes: kv.value}}
	}
}

// EndTransaction either commits or aborts (rolls back) an extant
// transaction according to the args.Commit parameter.
func (r *Range) EndTransaction(args *EndTransactionRequest, reply *EndTransactionResponse) {
	reply.Error = util.Error("unimplemented")
}

// AccumulateTS is used internally to aggregate statistics over key
// ranges throughout the distributed cluster.
func (r *Range) AccumulateTS(args *AccumulateTSRequest, reply *AccumulateTSResponse) {
	reply.Error = util.Error("unimplemented")
}

// ReapQueue destructively queries messages from a delivery inbox
// queue. This method must be called from within a transaction.
func (r *Range) ReapQueue(args *ReapQueueRequest, reply *ReapQueueResponse) {
	reply.Error = util.Error("unimplemented")
}

// EnqueueUpdate sidelines an update for asynchronous execution.
// AccumulateTS updates are sent this way. Eventually-consistent indexes
// are also built using update queues. Crucially, the enqueue happens
// as part of the caller's transaction, so is guaranteed to be
// executed if the transaction succeeded.
func (r *Range) EnqueueUpdate(args *EnqueueUpdateRequest, reply *EnqueueUpdateResponse) {
	reply.Error = util.Error("unimplemented")
}

// EnqueueMessage enqueues a message (Value) for delivery to a
// recipient inbox.
func (r *Range) EnqueueMessage(args *EnqueueMessageRequest, reply *EnqueueMessageResponse) {
	reply.Error = util.Error("unimplemented")
}

// InternalRangeLookup looks up the metadata info for the given args.Key.
// args.Key should be a metadata key, which are of the form "\0\0meta[12]<encoded_key>".
func (r *Range) InternalRangeLookup(args *InternalRangeLookupRequest, reply *InternalRangeLookupResponse) {
	if !bytes.HasPrefix(args.Key, KeyMetaPrefix) {
		reply.Error = util.Errorf("invalid metadata key: %q", args.Key)
		return
	}

	// Validate that key is not outside the range. A range ends just
	// before its Meta.EndKey.
	if !args.Key.Less(r.Meta.EndKey) {
		reply.Error = util.Errorf("key outside the range %v with end key %q", r.Meta.RangeID, r.Meta.EndKey)
		return
	}

	// We want to search for the metadata key just greater than args.Key.
	nextKey := NextKey(args.Key)
	kvs, err := r.engine.scan(nextKey, KeyMax, 1)
	if err != nil {
		reply.Error = err
		return
	}
	// We should have gotten the key with the same metadata level prefix as we queried.
	metaPrefix := args.Key[0:len(KeyMeta1Prefix)]
	if len(kvs) != 1 || !bytes.HasPrefix(kvs[0].key, metaPrefix) {
		reply.Error = util.Errorf("key not found in range %v", r.Meta.RangeID)
		return
	}

	if err = gob.NewDecoder(bytes.NewBuffer(kvs[0].value)).Decode(&reply.Range); err != nil {
		reply.Error = err
		return
	}
	if args.Key.Less(reply.Range.StartKey) {
		// args.Key doesn't belong to this range. We are perhaps searching the wrong node?
		reply.Error = util.Errorf("no range found for key %q in range: %+v", args.Key, r.Meta)
		return
	}
	reply.EndKey = kvs[0].key
}
