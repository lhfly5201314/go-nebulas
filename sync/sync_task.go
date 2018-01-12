// Copyright (C) 2018 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package sync

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/nebulasio/go-nebulas/util/byteutils"

	"github.com/gogo/protobuf/proto"
	"github.com/nebulasio/go-nebulas/core"
	"github.com/nebulasio/go-nebulas/core/pb"
	"github.com/nebulasio/go-nebulas/net"
	"github.com/nebulasio/go-nebulas/net/p2p"
	"github.com/nebulasio/go-nebulas/sync/pb"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"
)

var (
	ErrInvalidChainChunksMessageData    = errors.New("invalid ChainChunks message data")
	ErrWrongChainChunksMessageData      = errors.New("wrong ChainChunks message data")
	ErrInvalidChainChunkDataMessageData = errors.New("invalid ChainChunkData message data")
	ErrWrongChainChunkDataMessageData   = errors.New("wrong ChainChunkData message data")
)

type SyncTask struct {
	quitCh                         chan bool
	statusCh                       chan error
	tailBlock                      *core.Block
	netService                     p2p.Manager
	chunk                          *Chunk
	syncMutex                      sync.Mutex
	chainSyncPeersCount            int
	maxConsistentChunkHeadersCount int
	maxConsistentChunkHeaders      *syncpb.ChunkHeaders
	allChunkHeaders                map[string]*syncpb.ChunkHeaders
	chunkHeadersRootHashCounter    map[string]int
	chainSyncDoneCh                chan bool
	chainChunkDataSyncPosition     int
	chainChunkDataStatus           map[int]bool
	chinGetChunkDataDoneCh         chan bool

	// debug fields.
	chainSyncRetryCount int
}

func NewSyncTask(tailBlock *core.Block, netService p2p.Manager, chunk *Chunk) *SyncTask {
	return &SyncTask{
		quitCh:                         make(chan bool, 1),
		statusCh:                       make(chan error, 1),
		tailBlock:                      tailBlock,
		netService:                     netService,
		chunk:                          chunk,
		chainSyncPeersCount:            0,
		maxConsistentChunkHeadersCount: 0,
		maxConsistentChunkHeaders:      nil,
		allChunkHeaders:                make(map[string]*syncpb.ChunkHeaders),
		chunkHeadersRootHashCounter:    make(map[string]int),
		chainSyncDoneCh:                make(chan bool, 1),
		chainChunkDataSyncPosition:     0,
		chainChunkDataStatus:           make(map[int]bool),
		chinGetChunkDataDoneCh:         make(chan bool, 1),
		// debug fields.
		chainSyncRetryCount: 0,
	}
}

func (st *SyncTask) Start() {
	logging.VLog().WithFields(logrus.Fields{
		"tailBlockHeight": st.tailBlock.Height(),
		"tailBlockHash":   st.tailBlock.Hash().String(),
	}).Info("Starting active syncing process.")

	st.startSyncLoop()
}

func (st *SyncTask) Stop() {
	st.quitCh <- true
}

func (st *SyncTask) startSyncLoop() {
	go func() {
		for {
			// start chain sync.
			st.sendChainSync()

			syncTicker := time.NewTicker(30 * time.Second)
			for {
				select {
				case <-st.quitCh:
					logging.VLog().Debug("Stopping sync loop.")
					return
				case <-syncTicker.C:
					if !st.hasEnoughChunkHeaders() {
						st.reset()
						st.sendChainSync()
						continue
					}
				case <-st.chainSyncDoneCh:
					// go to next step.
					break
				}
			}

			// start get chunk data.
			logging.VLog().Debug("Starting GetChain from peers.")
			st.sendChainGetChunk()

			getChunkTimeoutTicker := time.NewTicker(10 * time.Second)
			for {
				select {
				case <-st.quitCh:
					logging.VLog().Debug("Stopping sync loop.")
					return
				case <-getChunkTimeoutTicker.C:
					// for the timeout peer, send message again.
					st.checkChainGetChunkTimeout()
				case <-st.chinGetChunkDataDoneCh:
					// finished.
					return
				}
			}
		}
	}()
}

func (st *SyncTask) reset() {
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	st.chainSyncPeersCount = 0
	st.maxConsistentChunkHeadersCount = 0
	st.maxConsistentChunkHeaders = nil
	st.allChunkHeaders = make(map[string]*syncpb.ChunkHeaders)
	st.chunkHeadersRootHashCounter = make(map[string]int)
	st.chainChunkDataStatus = make(map[int]bool)
	st.chainChunkDataSyncPosition = 0
}

func (st *SyncTask) sendChainSync() {
	st.chainSyncRetryCount++
	logging.VLog().Debugf("Starting ChainSync at %d times.", st.chainSyncRetryCount)

	pbBlock, err := st.tailBlock.ToProto()
	if err != nil {
		return
	}

	chunkSync := &syncpb.Sync{
		TailBlock: pbBlock.(*corepb.Block),
	}

	data, err := proto.Marshal(chunkSync)
	if err != nil {
		return
	}

	// send message to peers.
	st.chainSyncPeersCount = st.netService.SendMessageToPeers(net.ChainSync, data,
		net.MessagePriorityLow, new(p2p.ChainSyncPeersFilter))
}

func (st *SyncTask) processChunkHeaders(message net.Message) {
	if st.hasEnoughChunkHeaders() {
		return
	}

	chunkHeaders := new(syncpb.ChunkHeaders)
	if err := proto.Unmarshal(message.Data().([]byte), chunkHeaders); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkHeaders message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChainChunksMessageData)
		return
	}

	// verify data.
	if ok, err := VerifyChunkHeaders(chunkHeaders); ok == false {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkHeaders message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrWrongChainChunksMessageData)
		return
	}

	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	rootHash := byteutils.Hex(chunkHeaders.Root)
	count := st.chunkHeadersRootHashCounter[rootHash] + 1
	st.chunkHeadersRootHashCounter[rootHash] = count

	if count > st.maxConsistentChunkHeadersCount {
		st.maxConsistentChunkHeadersCount = count
		st.maxConsistentChunkHeaders = chunkHeaders
	}

	st.allChunkHeaders[rootHash] = chunkHeaders

	if st.hasEnoughChunkHeaders() {
		st.chainSyncDoneCh <- true
	}
}

func (st *SyncTask) sendChainGetChunk() {
	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	currentSyncChunkDataCount := 0
	chainChunkDataSyncPosition := 0
	for i := 0; i < len(st.maxConsistentChunkHeaders.ChunkHeaders) && currentSyncChunkDataCount < ConcurrentSyncChunkDataCount; i++ {
		if st.chainChunkDataStatus[i] == true {
			continue
		}

		currentSyncChunkDataCount++
		chainChunkDataSyncPosition = i

		st.sendChainGetChunkMessage(i)
	}

	st.chainChunkDataSyncPosition = chainChunkDataSyncPosition
}

func (st *SyncTask) checkChainGetChunkTimeout() {
	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	for i := 0; i < st.chainChunkDataSyncPosition; i++ {
		if st.chainChunkDataStatus[i] == true {
			continue
		}

		st.sendChainGetChunkMessage(i)
	}
}

func (st *SyncTask) sendChainGetChunkMessage(chunkHeaderIndex int) {
	chunkHeader := st.maxConsistentChunkHeaders.ChunkHeaders[chunkHeaderIndex]
	data, err := proto.Marshal(chunkHeader)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
		}).Warn("Failed to marshal ChunkHeader.")
		return
	}
	st.netService.SendMessageToPeers(net.ChainGetChunk, data, net.MessagePriorityLow, new(p2p.RandomPeerFilter))
}

func (st *SyncTask) processChunkData(message net.Message) {
	chunkData := new(syncpb.ChunkData)
	if err := proto.Unmarshal(message.Data().([]byte), chunkData); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkData message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChainChunkDataMessageData)
		return
	}

	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	// verify chunk data.
	chunkDataIndex := -1
	var chunkHeader *syncpb.ChunkHeader

	for i := 0; i < len(st.maxConsistentChunkHeaders.ChunkHeaders); i++ {
		chunkHeader = st.maxConsistentChunkHeaders.ChunkHeaders[i]
		if bytes.Compare(chunkHeader.Root, chunkData.Root) == 0 {
			chunkDataIndex = i
			break
		}
	}

	if chunkDataIndex < 0 {
		logging.VLog().WithFields(logrus.Fields{
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkData message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrWrongChainChunkDataMessageData)
		return
	}

	if st.chainChunkDataStatus[chunkDataIndex] == true {
		logging.VLog().WithFields(logrus.Fields{
			"pid": message.MessageFrom(),
		}).Debug("Duplicated ChainChunkData message data.")
		return
	}

	if ok, err := VerifyChunkData(chunkHeader, chunkData); ok == false {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkData message data.")
		st.netService.ClosePeer(message.MessageFrom(), err)
		return
	}

	st.chainChunkDataStatus[chunkDataIndex] = true
	st.chunk.processChunkData(chunkData)
}

func (st *SyncTask) hasEnoughChunkHeaders() bool {
	ret := st.maxConsistentChunkHeadersCount >= int(math.Sqrt(float64(st.chainSyncPeersCount)))
	if ret {
		logging.VLog().WithFields(logrus.Fields{
			"chainSyncPeersCount":               st.chainSyncPeersCount,
			"chainSyncRetryCount":               st.chainSyncRetryCount,
			"maxConsistentChunkHeadersCount":    st.maxConsistentChunkHeadersCount,
			"maxConsistentChunkHeadersRootHash": byteutils.Hex(st.maxConsistentChunkHeaders.Root),
		}).Debug("Received enough chunk headers.")
	}
	return ret
}
