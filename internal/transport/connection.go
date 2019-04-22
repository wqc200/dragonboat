// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/lni/dragonboat/internal/utils/logutil"
	"github.com/lni/dragonboat/raftio"
	pb "github.com/lni/dragonboat/raftpb"
)

const (
	streamingChanLength = 16
)

var (
	// ErrStopped is the error returned to indicate that the connection has
	// already been stopped.
	ErrStopped = errors.New("connection stopped")
	// ErrStreamSnapshot is the error returned to indicate that snapshot
	// streaming failed.
	ErrStreamSnapshot = errors.New("stream snapshot failed")
)

// Sink is the chunk sink for receiving generated snapshot chunk.
type Sink struct {
	l *connection
}

// Receive receives a snapshot chunk.
func (s *Sink) Receive(chunk pb.SnapshotChunk) (bool, bool) {
	return s.l.SendSnapshotChunk(chunk)
}

// ClusterID returns the cluster ID of the source node.
func (s *Sink) ClusterID() uint64 {
	return s.l.clusterID
}

// ToNodeID returns the node ID of the node intended to get and handle the
// received snapshot chunk.
func (s *Sink) ToNodeID() uint64 {
	return s.l.nodeID
}

type connection struct {
	clusterID          uint64
	nodeID             uint64
	deploymentID       uint64
	streaming          bool
	ctx                context.Context
	rpc                raftio.IRaftRPC
	ch                 chan pb.SnapshotChunk
	conn               raftio.ISnapshotConnection
	stopc              chan struct{}
	failed             chan struct{}
	streamChunkSent    atomic.Value
	preStreamChunkSend atomic.Value
}

func newConnection(ctx context.Context,
	clusterID uint64, nodeID uint64,
	did uint64, streaming bool, sz int, rpc raftio.IRaftRPC,
	stopc chan struct{}) *connection {
	l := &connection{
		clusterID:    clusterID,
		nodeID:       nodeID,
		deploymentID: did,
		streaming:    streaming,
		ctx:          ctx,
		rpc:          rpc,
		stopc:        stopc,
		failed:       make(chan struct{}),
	}
	var chsz int
	if streaming {
		chsz = streamingChanLength
	} else {
		chsz = sz
	}
	l.ch = make(chan pb.SnapshotChunk, chsz)
	return l
}

func (l *connection) close() {
	if l.conn != nil {
		l.conn.Close()
	}
}

func (l *connection) connect(addr string) error {
	conn, err := l.rpc.GetSnapshotConnection(l.ctx, addr)
	if err != nil {
		plog.Errorf("failed to get a connection to %s, %v", addr, err)
		return err
	}
	l.conn = conn
	return nil
}

func (l *connection) sendSavedSnapshot(m pb.Message) {
	chunks := splitSnapshotMessage(m)
	if len(chunks) != cap(l.ch) {
		plog.Panicf("cap of ch is %d, want %d", cap(l.ch), len(chunks))
	}
	for _, chunk := range chunks {
		select {
		case l.ch <- chunk:
		}
	}
}

func (l *connection) SendSnapshotChunk(chunk pb.SnapshotChunk) (bool, bool) {
	select {
	case l.ch <- chunk:
		return true, false
	case <-l.failed:
		return false, false
	case <-l.stopc:
		return false, true
	}
}

func (l *connection) process() error {
	if l.conn == nil {
		plog.Panicf("trying to process on nil ch, not connected?")
	}
	if l.streaming {
		return l.streamSnapshot()
	}
	return l.processSavedSnapshot()
}

func (l *connection) streamSnapshot() error {
	for {
		select {
		case <-l.stopc:
			return ErrStopped
		case chunk := <-l.ch:
			chunk.DeploymentId = l.deploymentID
			if chunk.ChunkCount == PoisonChunkCount {
				plog.Infof("poison chunk received")
				return ErrStreamSnapshot
			}
			if err := l.sendSnapshotChunk(chunk, l.conn); err != nil {
				plog.Errorf("stream snapshot chunk to %s failed",
					logutil.DescribeNode(chunk.ClusterId, chunk.NodeId))
				return err
			}
			if chunk.ChunkCount == LastChunkCount {
				return nil
			}
		}
	}
}

func (l *connection) processSavedSnapshot() error {
	chunks := make([]pb.SnapshotChunk, 0)
	for {
		select {
		case <-l.stopc:
			return ErrStopped
		case chunk := <-l.ch:
			if len(chunks) == 0 && chunk.ChunkId != 0 {
				panic("chunk alignment error")
			}
			chunks = append(chunks, chunk)
			if chunk.ChunkId+1 == chunk.ChunkCount {
				return l.sendChunks(chunks)
			}
		}
	}
}

func (l *connection) sendChunks(chunks []pb.SnapshotChunk) error {
	for _, chunk := range chunks {
		chunkData := make([]byte, snapChunkSize)
		data, err := loadSnapshotChunkData(chunk, chunkData)
		if err != nil {
			plog.Errorf("failed to read the snapshot chunk, %v", err)
			return err
		}
		chunk.Data = data
		chunk.DeploymentId = l.deploymentID
		if err := l.sendSnapshotChunk(chunk, l.conn); err != nil {
			plog.Debugf("snapshot to %s failed",
				logutil.DescribeNode(chunk.ClusterId, chunk.NodeId))
			return err
		}
		if v := l.streamChunkSent.Load(); v != nil {
			v.(func(pb.SnapshotChunk))(chunk)
		}
	}
	return nil
}

func (l *connection) sendSnapshotChunk(c pb.SnapshotChunk,
	conn raftio.ISnapshotConnection) error {
	if v := l.preStreamChunkSend.Load(); v != nil {
		plog.Infof("pre stream chunk send set")
		updated, shouldSend := v.(StreamChunkSendFunc)(c)
		plog.Infof("shoudSend: %t", shouldSend)
		if !shouldSend {
			plog.Infof("not sending the chunk!")
			return errChunkSendSkipped
		}
		return conn.SendSnapshotChunk(updated)
	}
	return conn.SendSnapshotChunk(c)
}