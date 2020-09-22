// Copyright 2020 The Celo Authors
// This file is part of the celo library.
//
// The celo library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The celo library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the celo library. If not, see <http://www.gnu.org/licenses/>.

package replica

import (
	"errors"
	"io"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	lvlerrors "github.com/syndtr/goleveldb/leveldb/errors"
)

type State interface {
	// mutation functions
	SetStartValidatingBlock(blockNumber *big.Int) error
	SetStopValidatingBlock(blockNumber *big.Int) error
	ShouldStartCore(seq *big.Int) bool
	ShouldStopCore(seq *big.Int) bool
	MakeReplica()
	MakePrimary()
	Close() error

	// view functions
	IsPrimaryForSeq(seq *big.Int) bool
	Summary() *ReplicaStateSummary
}

// ReplicaState stores info on this node being a primary or replica
type replicaStateImpl struct {
	isReplica            bool // Overridden by start/stop blocks if start/stop is enabled.
	enabled              bool
	startValidatingBlock *big.Int
	stopValidatingBlock  *big.Int

	rsdb *ReplicaStateDB
	mu   *sync.RWMutex
}

// NewState creates a replicaState in the given replica state and opens or creates the replica state DB at `path`.
func NewState(isReplica bool, path string) State {
	db, err := OpenReplicaStateDB(path)
	if err != nil {
		log.Crit("Can't open ReplicaStateDB", "err", err, "dbpath", path)
	}
	rs, err := db.GetReplicaState()
	if err == lvlerrors.ErrNotFound {
		rs = &replicaStateImpl{
			isReplica: isReplica,
			mu:        new(sync.RWMutex),
		}
	} else if err != nil {
		log.Warn("Can't read ReplicaStateDB at startup", "err", err, "dbpath", path)
	}
	rs.rsdb = db
	db.StoreReplicaState(rs)
	return rs
}

// Close closes the replica state database
func (rs *replicaStateImpl) Close() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.rsdb.Close()
}

// SetStartValidatingBlock sets the start block in the range [start, stop)
func (rs *replicaStateImpl) SetStartValidatingBlock(blockNumber *big.Int) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	defer rs.rsdb.StoreReplicaState(rs)

	if blockNumber == nil {
		rs.startValidatingBlock = nil
		return nil
	}

	if rs.stopValidatingBlock != nil && !(blockNumber.Cmp(rs.stopValidatingBlock) < 0) {
		return errors.New("Start block number should be less than the stop block number")
	}

	rs.enabled = true
	rs.startValidatingBlock = blockNumber
	return nil
}

// SetStopValidatingBlock sets the stop block in the range [start, stop)
func (rs *replicaStateImpl) SetStopValidatingBlock(blockNumber *big.Int) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	defer rs.rsdb.StoreReplicaState(rs)

	if blockNumber == nil {
		rs.stopValidatingBlock = nil
		return nil
	}

	if rs.startValidatingBlock != nil && !(blockNumber.Cmp(rs.startValidatingBlock) > 0) {
		return errors.New("Stop block number should be greater than the start block number")
	}

	rs.enabled = true
	rs.stopValidatingBlock = blockNumber
	return nil
}

// MakeReplica makes this node a replica & clears all start/stop blocks.
func (rs *replicaStateImpl) MakeReplica() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	defer rs.rsdb.StoreReplicaState(rs)

	rs.enabled = false
	rs.startValidatingBlock = nil
	rs.stopValidatingBlock = nil
	rs.isReplica = true
}

// MakePrimary makes this node a primary & clears all start/stop blocks.
func (rs *replicaStateImpl) MakePrimary() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	defer rs.rsdb.StoreReplicaState(rs)

	rs.enabled = false
	rs.startValidatingBlock = nil
	rs.stopValidatingBlock = nil
	rs.isReplica = false
}

// ShouldStartCore returns true if the backend should start the istanbul core.
// Also updates replica state if the core should start.
func (rs *replicaStateImpl) ShouldStartCore(seq *big.Int) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.isPrimaryForSeq(seq) && rs.isReplica {
		defer rs.rsdb.StoreReplicaState(rs)

		if rs.shouldSwitchToPrimary(seq) {
			rs.enabled = false
			rs.startValidatingBlock = nil
			rs.stopValidatingBlock = nil
		}
		rs.isReplica = false
		return true
	}
	return false
}

// ShouldStopCore returns true if the backend should stop the istanbul core.
// Also updates replica state if the core should stop.
func (rs *replicaStateImpl) ShouldStopCore(seq *big.Int) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !rs.isPrimaryForSeq(seq) && !rs.isReplica {
		defer rs.rsdb.StoreReplicaState(rs)

		if rs.shouldSwitchToReplica(seq) {
			rs.enabled = false
			rs.startValidatingBlock = nil
			rs.stopValidatingBlock = nil
		}
		rs.isReplica = true
		return true
	}
	return false
}

// IsPrimaryForSeq determines is this node is the primary validator.
// If start/stop checking is enabled (via a call to start/stop at block)
// determine if start <= seq < stop. If not enabled, check if this was
// set up with replica mode.
func (rs *replicaStateImpl) IsPrimaryForSeq(seq *big.Int) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.isPrimaryForSeq(seq)
}

func (rs *replicaStateImpl) shouldSwitchToPrimary(blockNumber *big.Int) bool {
	if !rs.enabled {
		return false
	}
	// start <= seq w/ no stop -> primary
	if rs.startValidatingBlock != nil && rs.startValidatingBlock.Cmp(blockNumber) <= 0 {
		if rs.stopValidatingBlock == nil {
			return true
		}
	}

	return false
}
func (rs *replicaStateImpl) shouldSwitchToReplica(blockNumber *big.Int) bool {
	if !rs.enabled {
		return false
	}
	// start <= stop < seq -> replica
	if rs.stopValidatingBlock != nil && rs.stopValidatingBlock.Cmp(blockNumber) <= 0 {
		return true
	}
	return false
}

// isPrimaryForSeq determines is this node is the primary validator.
// If start/stop checking is enabled (via a call to start/stop at block)
// determine if start <= seq < stop. If not enabled, check if this was
// set up with replica mode.
func (rs *replicaStateImpl) isPrimaryForSeq(seq *big.Int) bool {
	if !rs.enabled {
		return !rs.isReplica
	}
	// Return start <= seq < stop with start/stop at +-inf if nil
	if rs.startValidatingBlock != nil && seq.Cmp(rs.startValidatingBlock) < 0 {
		return false
	}
	if rs.stopValidatingBlock != nil && seq.Cmp(rs.stopValidatingBlock) >= 0 {
		return false
	}
	return true
}

type ReplicaStateSummary struct {
	State                string   `json:"state"`
	Enabled              bool     `json:"enabled"`
	IsReplica            bool     `json:"isReplica"`
	StartValidatingBlock *big.Int `json:"startValidatingBlock"`
	StopValidatingBlock  *big.Int `json:"stopValidatingBlock"`
}

func (rs *replicaStateImpl) Summary() *ReplicaStateSummary {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	// String explanation of replica state
	var state string
	if rs.isReplica && !rs.enabled {
		state = "Replica"
	} else if rs.isReplica && rs.enabled {
		state = "Replica waiting to start"
	} else if !rs.isReplica && !rs.enabled {
		state = "Primary"
	} else if !rs.isReplica && rs.enabled {
		state = "Primary in given range"
	}

	summary := &ReplicaStateSummary{
		State:                state,
		IsReplica:            rs.isReplica,
		Enabled:              rs.enabled,
		StartValidatingBlock: rs.startValidatingBlock,
		StopValidatingBlock:  rs.stopValidatingBlock,
	}

	return summary
}

type replicaStateRLP struct {
	IsReplica            bool
	Enabled              bool
	StartValidatingBlock *big.Int
	StopValidatingBlock  *big.Int
}

// EncodeRLP should write the RLP encoding of its receiver to w.
// If the implementation is a pointer method, it may also be
// called for nil pointers.
//
// Implementations should generate valid RLP. The data written is
// not verified at the moment, but a future version might. It is
// recommended to write only a single value but writing multiple
// values or no value at all is also permitted.
func (rs *replicaStateImpl) EncodeRLP(w io.Writer) error {
	entry := replicaStateRLP{
		IsReplica:            rs.isReplica,
		Enabled:              rs.enabled,
		StartValidatingBlock: rs.startValidatingBlock,
		StopValidatingBlock:  rs.stopValidatingBlock,
	}
	return rlp.Encode(w, entry)
}

// The DecodeRLP method should read one value from the given
// Stream. It is not forbidden to read less or more, but it might
// be confusing.
func (rs *replicaStateImpl) DecodeRLP(stream *rlp.Stream) error {
	var data replicaStateRLP
	err := stream.Decode(&data)
	if err != nil {
		return err
	}

	rs.mu = new(sync.RWMutex)
	rs.isReplica = data.IsReplica
	rs.enabled = data.Enabled
	rs.startValidatingBlock = data.StartValidatingBlock
	rs.stopValidatingBlock = data.StopValidatingBlock

	return nil
}
