// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2015-2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/addrmgr"
	"github.com/decred/dcrd/blockchain/stake/v2"
	"github.com/decred/dcrd/blockchain/standalone"
	"github.com/decred/dcrd/blockchain/v2"
	"github.com/decred/dcrd/blockchain/v2/indexers"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v2"
	"github.com/decred/dcrd/connmgr/v2"
	"github.com/decred/dcrd/database/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrd/fees/v2"
	"github.com/decred/dcrd/gcs/v2"
	"github.com/decred/dcrd/gcs/v2/blockcf"
	"github.com/decred/dcrd/internal/version"
	"github.com/decred/dcrd/lru"
	"github.com/decred/dcrd/mempool/v3"
	"github.com/decred/dcrd/mining/v2"
	"github.com/decred/dcrd/peer/v2"
	"github.com/decred/dcrd/txscript/v2"
	"github.com/decred/dcrd/wire"
)

const (
	// defaultServices describes the default services that are supported by
	// the server.
	defaultServices = wire.SFNodeNetwork | wire.SFNodeCF

	// defaultRequiredServices describes the default services that are
	// required to be supported by outbound peers.
	defaultRequiredServices = wire.SFNodeNetwork

	// defaultTargetOutbound is the default number of outbound peers to
	// target.
	defaultTargetOutbound = 8

	// connectionRetryInterval is the base amount of time to wait in between
	// retries when connecting to persistent peers.  It is adjusted by the
	// number of retries such that there is a retry backoff.
	connectionRetryInterval = time.Second * 5

	// maxProtocolVersion is the max protocol version the server supports.
	maxProtocolVersion = wire.NodeCFVersion

	// maxKnownAddrsPerPeer is the maximum number of items to keep in the
	// per-peer known address cache.
	maxKnownAddrsPerPeer = 10000
)

var (
	// userAgentName is the user agent name and is used to help identify
	// ourselves to other Decred peers.
	userAgentName = "dcrd"

	// userAgentVersion is the user agent version and is used to help
	// identify ourselves to other peers.
	userAgentVersion = fmt.Sprintf("%d.%d.%d", version.Major, version.Minor,
		version.Patch)
)

// broadcastMsg provides the ability to house a Decred message to be broadcast
// to all connected peers except specified excluded peers.
type broadcastMsg struct {
	message      wire.Message
	excludePeers []*serverPeer
}

// broadcastInventoryAdd is a type used to declare that the InvVect it contains
// needs to be added to the rebroadcast map
type broadcastInventoryAdd relayMsg

// broadcastInventoryDel is a type used to declare that the InvVect it contains
// needs to be removed from the rebroadcast map
type broadcastInventoryDel *wire.InvVect

// broadcastPruneInventory is a type used to declare that rebroadcast
// inventory entries need to be filtered and removed where necessary
type broadcastPruneInventory struct{}

// relayMsg packages an inventory vector along with the newly discovered
// inventory and a flag that determines if the relay should happen immediately
// (it will be put into a trickle queue if false) so the relay has access to
// that information.
type relayMsg struct {
	invVect   *wire.InvVect
	data      interface{}
	immediate bool
}

// updatePeerHeightsMsg is a message sent from the blockmanager to the server
// after a new block has been accepted. The purpose of the message is to update
// the heights of peers that were known to announce the block before we
// connected it to the main chain or recognized it as an orphan. With these
// updates, peer heights will be kept up to date, allowing for fresh data when
// selecting sync peer candidacy.
type updatePeerHeightsMsg struct {
	newHash    *chainhash.Hash
	newHeight  int64
	originPeer *serverPeer
}

// peerState maintains state of inbound, persistent, outbound peers as well
// as banned peers and outbound groups.
type peerState struct {
	inboundPeers    map[int32]*serverPeer
	outboundPeers   map[int32]*serverPeer
	persistentPeers map[int32]*serverPeer
	banned          map[string]time.Time
	outboundGroups  map[string]int

	// suggestions represents public network address suggestions from outbound
	// peers.
	suggestions    map[addrmgr.NetworkAddress]map[string]int32
	suggestionsMtx sync.Mutex
}

// ConnectionsWithIP returns the number of connections with the given IP.
func (ps *peerState) ConnectionsWithIP(ip net.IP) int {
	var total int
	for _, p := range ps.inboundPeers {
		if ip.Equal(p.NA().IP) {
			total++
		}
	}
	for _, p := range ps.outboundPeers {
		if ip.Equal(p.NA().IP) {
			total++
		}
	}
	for _, p := range ps.persistentPeers {
		if ip.Equal(p.NA().IP) {
			total++
		}
	}
	return total
}

// Count returns the count of all known peers.
func (ps *peerState) Count() int {
	return len(ps.inboundPeers) + len(ps.outboundPeers) +
		len(ps.persistentPeers)
}

// forAllOutboundPeers is a helper function that runs closure on all outbound
// peers known to peerState.
func (ps *peerState) forAllOutboundPeers(closure func(sp *serverPeer)) {
	for _, e := range ps.outboundPeers {
		closure(e)
	}
	for _, e := range ps.persistentPeers {
		closure(e)
	}
}

// forAllPeers is a helper function that runs closure on all peers known to
// peerState.
func (ps *peerState) forAllPeers(closure func(sp *serverPeer)) {
	for _, e := range ps.inboundPeers {
		closure(e)
	}
	ps.forAllOutboundPeers(closure)
}

// ResolveLocalAddress picks the best suggested network address from available
// options, per the network interface key provided. The best suggestion, if
// found, is added as a local address.
func (ps *peerState) ResolveLocalAddress(netKey addrmgr.NetworkAddress, addrMgr *addrmgr.AddrManager, services wire.ServiceFlag, port uint16) {
	ps.suggestionsMtx.Lock()
	count := len(ps.suggestions[netKey])
	if count == 0 {
		ps.suggestionsMtx.Unlock()
		return
	}

	var bestSuggestion string
	var bestTally int32
	for suggestion, tally := range ps.suggestions[netKey] {
		switch {
		case bestSuggestion == "", tally > bestTally:
			bestSuggestion = suggestion
			bestTally = tally
		}
	}
	ps.suggestionsMtx.Unlock()

	// A valid best address suggestion must have at least two outbound peers
	// concluding on the same result.
	if bestTally < 2 {
		return
	}

	na, err := addrMgr.HostToNetAddress(bestSuggestion, port, services)
	if err != nil {
		amgrLog.Errorf("unable to generate network address using host %v: "+
			"%v", bestSuggestion, err)
		return
	}

	if !addrMgr.HasLocalAddress(na) {
		addrMgr.AddLocalAddress(na, addrmgr.ManualPrio)
	}
}

// server provides a Decred server for handling communications to and from
// Decred peers.
type server struct {
	// The following variables must only be used atomically.
	// Putting the uint64s first makes them 64-bit aligned for 32-bit systems.
	bytesReceived uint64 // Total bytes received from all peers since start.
	bytesSent     uint64 // Total bytes sent by all peers since start.
	started       int32
	shutdown      int32

	chainParams          *chaincfg.Params
	addrManager          *addrmgr.AddrManager
	connManager          *connmgr.ConnManager
	sigCache             *txscript.SigCache
	subsidyCache         *standalone.SubsidyCache
	rpcServer            *rpcServer
	blockManager         *blockManager
	bg                   *BgBlkTmplGenerator
	chain                *blockchain.BlockChain
	txMemPool            *mempool.TxPool
	feeEstimator         *fees.Estimator
	cpuMiner             *CPUMiner
	modifyRebroadcastInv chan interface{}
	newPeers             chan *serverPeer
	donePeers            chan *serverPeer
	banPeers             chan *serverPeer
	query                chan interface{}
	relayInv             chan relayMsg
	broadcast            chan broadcastMsg
	peerHeightsUpdate    chan updatePeerHeightsMsg
	wg                   sync.WaitGroup
	quit                 chan struct{}
	nat                  NAT
	db                   database.DB
	timeSource           blockchain.MedianTimeSource
	services             wire.ServiceFlag
	context              context.Context
	cancel               context.CancelFunc

	// The following fields are used for optional indexes.  They will be nil
	// if the associated index is not enabled.  These fields are set during
	// initial creation of the server and never changed afterwards, so they
	// do not need to be protected for concurrent access.
	txIndex         *indexers.TxIndex
	addrIndex       *indexers.AddrIndex
	existsAddrIndex *indexers.ExistsAddrIndex
	cfIndex         *indexers.CFIndex
}

// serverPeer extends the peer to maintain state shared by the server and
// the blockmanager.
type serverPeer struct {
	*peer.Peer

	connReq         *connmgr.ConnReq
	server          *server
	persistent      bool
	continueHash    *chainhash.Hash
	relayMtx        sync.Mutex
	disableRelayTx  bool
	isWhitelisted   bool
	requestedTxns   map[chainhash.Hash]struct{}
	requestedBlocks map[chainhash.Hash]struct{}
	knownAddresses  lru.Cache
	banScore        connmgr.DynamicBanScore
	quit            chan struct{}

	// addrsSent and getMiningStateSent both track whether or not the peer
	// has already sent the respective request.  It is used to prevent more
	// than one response per connection.
	addrsSent          bool
	getMiningStateSent bool

	// The following chans are used to sync blockmanager and server.
	txProcessed    chan struct{}
	blockProcessed chan struct{}

	// peerNa is network address of the peer connected to.
	peerNa    *wire.NetAddress
	peerNaMtx sync.Mutex
}

// newServerPeer returns a new serverPeer instance. The peer needs to be set by
// the caller.
func newServerPeer(s *server, isPersistent bool) *serverPeer {
	return &serverPeer{
		server:          s,
		persistent:      isPersistent,
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
		knownAddresses:  lru.NewCache(maxKnownAddrsPerPeer),
		quit:            make(chan struct{}),
		txProcessed:     make(chan struct{}, 1),
		blockProcessed:  make(chan struct{}, 1),
	}
}

// newestBlock returns the current best block hash and height using the format
// required by the configuration for the peer package.
func (sp *serverPeer) newestBlock() (*chainhash.Hash, int64, error) {
	best := sp.server.chain.BestSnapshot()
	return &best.Hash, best.Height, nil
}

// addKnownAddresses adds the given addresses to the set of known addresses to
// the peer to prevent sending duplicate addresses.
func (sp *serverPeer) addKnownAddresses(addresses []*wire.NetAddress) {
	for _, na := range addresses {
		sp.knownAddresses.Add(addrmgr.NetAddressKey(na))
	}
}

// addressKnown true if the given address is already known to the peer.
func (sp *serverPeer) addressKnown(na *wire.NetAddress) bool {
	return sp.knownAddresses.Contains(addrmgr.NetAddressKey(na))
}

// setDisableRelayTx toggles relaying of transactions for the given peer.
// It is safe for concurrent access.
func (sp *serverPeer) setDisableRelayTx(disable bool) {
	sp.relayMtx.Lock()
	sp.disableRelayTx = disable
	sp.relayMtx.Unlock()
}

// relayTxDisabled returns whether or not relaying of transactions for the given
// peer is disabled.
// It is safe for concurrent access.
func (sp *serverPeer) relayTxDisabled() bool {
	sp.relayMtx.Lock()
	isDisabled := sp.disableRelayTx
	sp.relayMtx.Unlock()

	return isDisabled
}

// pushAddrMsg sends an addr message to the connected peer using the provided
// addresses.
func (sp *serverPeer) pushAddrMsg(addresses []*wire.NetAddress) {
	// Filter addresses already known to the peer.
	addrs := make([]*wire.NetAddress, 0, len(addresses))
	for _, addr := range addresses {
		if !sp.addressKnown(addr) {
			addrs = append(addrs, addr)
		}
	}
	known, err := sp.PushAddrMsg(addrs)
	if err != nil {
		peerLog.Errorf("Can't push address message to %s: %v", sp.Peer, err)
		sp.Disconnect()
		return
	}
	sp.addKnownAddresses(known)
}

// addBanScore increases the persistent and decaying ban score fields by the
// values passed as parameters. If the resulting score exceeds half of the ban
// threshold, a warning is logged including the reason provided. Further, if
// the score is above the ban threshold, the peer will be banned and
// disconnected.
func (sp *serverPeer) addBanScore(persistent, transient uint32, reason string) {
	// No warning is logged and no score is calculated if banning is disabled.
	if cfg.DisableBanning {
		return
	}
	if sp.isWhitelisted {
		peerLog.Debugf("Misbehaving whitelisted peer %s: %s", sp, reason)
		return
	}

	warnThreshold := cfg.BanThreshold >> 1
	if transient == 0 && persistent == 0 {
		// The score is not being increased, but a warning message is still
		// logged if the score is above the warn threshold.
		score := sp.banScore.Int()
		if score > warnThreshold {
			peerLog.Warnf("Misbehaving peer %s: %s -- ban score is %d, "+
				"it was not increased this time", sp, reason, score)
		}
		return
	}
	score := sp.banScore.Increase(persistent, transient)
	if score > warnThreshold {
		peerLog.Warnf("Misbehaving peer %s: %s -- ban score increased to %d",
			sp, reason, score)
		if score > cfg.BanThreshold {
			peerLog.Warnf("Misbehaving peer %s -- banning and disconnecting",
				sp)
			sp.server.BanPeer(sp)
			sp.Disconnect()
		}
	}
}

// hasServices returns whether or not the provided advertised service flags have
// all of the provided desired service flags set.
func hasServices(advertised, desired wire.ServiceFlag) bool {
	return advertised&desired == desired
}

// OnVersion is invoked when a peer receives a version wire message and is used
// to negotiate the protocol version details as well as kick start the
// communications.
func (sp *serverPeer) OnVersion(p *peer.Peer, msg *wire.MsgVersion) *wire.MsgReject {
	// Update the address manager with the advertised services for outbound
	// connections in case they have changed.  This is not done for inbound
	// connections to help prevent malicious behavior and is skipped when
	// running on the simulation test network since it is only intended to
	// connect to specified peers and actively avoids advertising and
	// connecting to discovered peers.
	//
	// NOTE: This is done before rejecting peers that are too old to ensure
	// it is updated regardless in the case a new minimum protocol version is
	// enforced and the remote node has not upgraded yet.
	isInbound := sp.Inbound()
	remoteAddr := sp.NA()
	addrManager := sp.server.addrManager
	if !cfg.SimNet && !isInbound {
		addrManager.SetServices(remoteAddr, msg.Services)
	}

	// Ignore peers that have a protocol version that is too old.  The peer
	// negotiation logic will disconnect it after this callback returns.
	if msg.ProtocolVersion < int32(wire.InitialProcotolVersion) {
		return nil
	}

	// Reject outbound peers that are not full nodes.
	wantServices := wire.SFNodeNetwork
	if !isInbound && !hasServices(msg.Services, wantServices) {
		missingServices := wantServices & ^msg.Services
		srvrLog.Debugf("Rejecting peer %s with services %v due to not "+
			"providing desired services %v", sp.Peer, msg.Services,
			missingServices)
		reason := fmt.Sprintf("required services %#x not offered",
			uint64(missingServices))
		return wire.NewMsgReject(msg.Command(), wire.RejectNonstandard, reason)
	}

	// Update the address manager and request known addresses from the
	// remote peer for outbound connections.  This is skipped when running
	// on the simulation test network since it is only intended to connect
	// to specified peers and actively avoids advertising and connecting to
	// discovered peers.
	if !cfg.SimNet && !isInbound {
		// Advertise the local address when the server accepts incoming
		// connections and it believes itself to be close to the best
		// known tip.
		if !cfg.DisableListen && sp.server.blockManager.IsCurrent() {
			// Get address that best matches.
			lna := addrManager.GetBestLocalAddress(remoteAddr)
			if addrmgr.IsRoutable(lna) {
				// Filter addresses the peer already knows about.
				addresses := []*wire.NetAddress{lna}
				sp.pushAddrMsg(addresses)
			}
		}

		// Request known addresses if the server address manager needs
		// more.
		if addrManager.NeedMoreAddresses() {
			p.QueueMessage(wire.NewMsgGetAddr(), nil)
		}

		// Mark the address as a known good address.
		addrManager.Good(remoteAddr)
	}

	if !sp.Inbound() {
		sp.peerNaMtx.Lock()
		sp.peerNa = &msg.AddrYou
		sp.peerNaMtx.Unlock()
	}

	// Choose whether or not to relay transactions.
	sp.setDisableRelayTx(msg.DisableRelayTx)

	// Add the remote peer time as a sample for creating an offset against
	// the local clock to keep the network time in sync.
	sp.server.timeSource.AddTimeSample(p.Addr(), msg.Timestamp)

	// Signal the block manager this peer is a new sync candidate.
	sp.server.blockManager.NewPeer(sp)

	// Add valid peer to the server.
	sp.server.AddPeer(sp)
	return nil
}

// OnMemPool is invoked when a peer receives a mempool wire message.  It creates
// and sends an inventory message with the contents of the memory pool up to the
// maximum inventory allowed per message.
func (sp *serverPeer) OnMemPool(p *peer.Peer, msg *wire.MsgMemPool) {
	// A decaying ban score increase is applied to prevent flooding.
	// The ban score accumulates and passes the ban threshold if a burst of
	// mempool messages comes from a peer. The score decays each minute to
	// half of its value.
	sp.addBanScore(0, 33, "mempool")

	// Generate inventory message with the available transactions in the
	// transaction memory pool.  Limit it to the max allowed inventory
	// per message.  The NewMsgInvSizeHint function automatically limits
	// the passed hint to the maximum allowed, so it's safe to pass it
	// without double checking it here.
	txMemPool := sp.server.txMemPool
	txDescs := txMemPool.TxDescs()
	invMsg := wire.NewMsgInvSizeHint(uint(len(txDescs)))

	for i, txDesc := range txDescs {
		iv := wire.NewInvVect(wire.InvTypeTx, txDesc.Tx.Hash())
		invMsg.AddInvVect(iv)
		if i+1 >= wire.MaxInvPerMsg {
			break
		}
	}

	// Send the inventory message if there is anything to send.
	if len(invMsg.InvList) > 0 {
		p.QueueMessage(invMsg, nil)
	}
}

// pushMiningStateMsg pushes a mining state message to the queue for a
// requesting peer.
func (sp *serverPeer) pushMiningStateMsg(height uint32, blockHashes []chainhash.Hash, voteHashes []chainhash.Hash) error {
	// Nothing to send, abort.
	if len(blockHashes) == 0 {
		return nil
	}

	// Construct the mining state request and queue it to be sent.
	msg := wire.NewMsgMiningState()
	msg.Height = height
	for i := range blockHashes {
		err := msg.AddBlockHash(&blockHashes[i])
		if err != nil {
			return err
		}
	}
	for i := range voteHashes {
		err := msg.AddVoteHash(&voteHashes[i])
		if err != nil {
			return err
		}
		if i+1 >= wire.MaxMSBlocksAtHeadPerMsg {
			break
		}
	}

	sp.QueueMessage(msg, nil)

	return nil
}

// OnGetMiningState is invoked when a peer receives a getminings wire message.
// It constructs a list of the current best blocks and votes that should be
// mined on and pushes a miningstate wire message back to the requesting peer.
func (sp *serverPeer) OnGetMiningState(p *peer.Peer, msg *wire.MsgGetMiningState) {
	if sp.getMiningStateSent {
		peerLog.Tracef("Ignoring getminingstate from %v - already sent", sp.Peer)
		return
	}
	sp.getMiningStateSent = true

	// Access the block manager and get the list of best blocks to mine on.
	bm := sp.server.blockManager
	mp := sp.server.txMemPool
	best := sp.server.chain.BestSnapshot()

	// Send out blank mining states if it's early in the blockchain.
	if best.Height < activeNetParams.StakeValidationHeight-1 {
		err := sp.pushMiningStateMsg(0, nil, nil)
		if err != nil {
			peerLog.Warnf("unexpected error while pushing data for "+
				"mining state request: %v", err.Error())
		}

		return
	}

	// Obtain the entire generation of blocks stemming from the parent of
	// the current tip.
	children, err := bm.TipGeneration()
	if err != nil {
		peerLog.Warnf("failed to access block manager to get the generation "+
			"for a mining state request (block: %v): %v", best.Hash, err)
		return
	}

	// Get the list of blocks that are eligible to build on and limit the
	// list to the maximum number of allowed eligible block hashes per
	// mining state message.  There is nothing to send when there are no
	// eligible blocks.
	blockHashes := SortParentsByVotes(mp, best.Hash, children,
		bm.cfg.ChainParams)
	numBlocks := len(blockHashes)
	if numBlocks == 0 {
		return
	}
	if numBlocks > wire.MaxMSBlocksAtHeadPerMsg {
		blockHashes = blockHashes[:wire.MaxMSBlocksAtHeadPerMsg]
	}

	// Construct the set of votes to send.
	voteHashes := make([]chainhash.Hash, 0, wire.MaxMSVotesAtHeadPerMsg)
	for i := range blockHashes {
		// Fetch the vote hashes themselves and append them.
		bh := &blockHashes[i]
		vhsForBlock := mp.VoteHashesForBlock(bh)
		if len(vhsForBlock) == 0 {
			peerLog.Warnf("unexpected error while fetching vote hashes "+
				"for block %v for a mining state request: no vote "+
				"metadata for block", bh)
			return
		}
		voteHashes = append(voteHashes, vhsForBlock...)
	}

	err = sp.pushMiningStateMsg(uint32(best.Height), blockHashes, voteHashes)
	if err != nil {
		peerLog.Warnf("unexpected error while pushing data for "+
			"mining state request: %v", err.Error())
	}
}

// OnMiningState is invoked when a peer receives a miningstate wire message.  It
// requests the data advertised in the message from the peer.
func (sp *serverPeer) OnMiningState(p *peer.Peer, msg *wire.MsgMiningState) {
	err := sp.server.blockManager.RequestFromPeer(sp, msg.BlockHashes,
		msg.VoteHashes)
	if err != nil {
		peerLog.Warnf("couldn't handle mining state message: %v",
			err.Error())
	}
}

// OnTx is invoked when a peer receives a tx wire message.  It blocks until the
// transaction has been fully processed.  Unlock the block handler this does not
// serialize all transactions through a single thread transactions don't rely on
// the previous one in a linear fashion like blocks.
func (sp *serverPeer) OnTx(p *peer.Peer, msg *wire.MsgTx) {
	if cfg.BlocksOnly {
		peerLog.Tracef("Ignoring tx %v from %v - blocksonly enabled",
			msg.TxHash(), p)
		return
	}

	// Add the transaction to the known inventory for the peer.
	// Convert the raw MsgTx to a dcrutil.Tx which provides some convenience
	// methods and things such as hash caching.
	tx := dcrutil.NewTx(msg)
	iv := wire.NewInvVect(wire.InvTypeTx, tx.Hash())
	p.AddKnownInventory(iv)

	// Queue the transaction up to be handled by the block manager and
	// intentionally block further receives until the transaction is fully
	// processed and known good or bad.  This helps prevent a malicious peer
	// from queuing up a bunch of bad transactions before disconnecting (or
	// being disconnected) and wasting memory.
	sp.server.blockManager.QueueTx(tx, sp)
	<-sp.txProcessed
}

// OnBlock is invoked when a peer receives a block wire message.  It blocks
// until the network block has been fully processed.
func (sp *serverPeer) OnBlock(p *peer.Peer, msg *wire.MsgBlock, buf []byte) {
	// Convert the raw MsgBlock to a dcrutil.Block which provides some
	// convenience methods and things such as hash caching.
	block := dcrutil.NewBlockFromBlockAndBytes(msg, buf)

	// Add the block to the known inventory for the peer.
	iv := wire.NewInvVect(wire.InvTypeBlock, block.Hash())
	p.AddKnownInventory(iv)

	// Queue the block up to be handled by the block manager and
	// intentionally block further receives until the network block is fully
	// processed and known good or bad.  This helps prevent a malicious peer
	// from queuing up a bunch of bad blocks before disconnecting (or being
	// disconnected) and wasting memory.  Additionally, this behavior is
	// depended on by at least the block acceptance test tool as the
	// reference implementation processes blocks in the same thread and
	// therefore blocks further messages until the network block has been
	// fully processed.
	sp.server.blockManager.QueueBlock(block, sp)
	<-sp.blockProcessed
}

// OnInv is invoked when a peer receives an inv wire message and is used to
// examine the inventory being advertised by the remote peer and react
// accordingly.  We pass the message down to blockmanager which will call
// QueueMessage with any appropriate responses.
func (sp *serverPeer) OnInv(p *peer.Peer, msg *wire.MsgInv) {
	if !cfg.BlocksOnly {
		if len(msg.InvList) > 0 {
			sp.server.blockManager.QueueInv(msg, sp)
		}
		return
	}

	newInv := wire.NewMsgInvSizeHint(uint(len(msg.InvList)))
	for _, invVect := range msg.InvList {
		if invVect.Type == wire.InvTypeTx {
			peerLog.Infof("Peer %v is announcing transactions -- "+
				"disconnecting", p)
			p.Disconnect()
			return
		}
		err := newInv.AddInvVect(invVect)
		if err != nil {
			peerLog.Errorf("Failed to add inventory vector: %v", err)
			break
		}
	}

	if len(newInv.InvList) > 0 {
		sp.server.blockManager.QueueInv(newInv, sp)
	}
}

// OnHeaders is invoked when a peer receives a headers wire message.  The
// message is passed down to the block manager.
func (sp *serverPeer) OnHeaders(p *peer.Peer, msg *wire.MsgHeaders) {
	sp.server.blockManager.QueueHeaders(msg, sp)
}

// handleGetData is invoked when a peer receives a getdata wire message and is
// used to deliver block and transaction information.
func (sp *serverPeer) OnGetData(p *peer.Peer, msg *wire.MsgGetData) {
	// Ignore empty getdata messages.
	if len(msg.InvList) == 0 {
		return
	}

	numAdded := 0
	notFound := wire.NewMsgNotFound()

	length := len(msg.InvList)
	// A decaying ban score increase is applied to prevent exhausting resources
	// with unusually large inventory queries.
	// Requesting more than the maximum inventory vector length within a short
	// period of time yields a score above the default ban threshold. Sustained
	// bursts of small requests are not penalized as that would potentially ban
	// peers performing IBD.
	// This incremental score decays each minute to half of its value.
	sp.addBanScore(0, uint32(length)*99/wire.MaxInvPerMsg, "getdata")

	// We wait on this wait channel periodically to prevent queuing
	// far more data than we can send in a reasonable time, wasting memory.
	// The waiting occurs after the database fetch for the next one to
	// provide a little pipelining.
	var waitChan chan struct{}
	doneChan := make(chan struct{}, 1)

	for i, iv := range msg.InvList {
		var c chan struct{}
		// If this will be the last message we send.
		if i == length-1 && len(notFound.InvList) == 0 {
			c = doneChan
		} else if (i+1)%3 == 0 {
			// Buffered so as to not make the send goroutine block.
			c = make(chan struct{}, 1)
		}
		var err error
		switch iv.Type {
		case wire.InvTypeTx:
			err = sp.server.pushTxMsg(sp, &iv.Hash, c, waitChan)
		case wire.InvTypeBlock:
			err = sp.server.pushBlockMsg(sp, &iv.Hash, c, waitChan)
		default:
			peerLog.Warnf("Unknown type in inventory request %d",
				iv.Type)
			continue
		}
		if err != nil {
			notFound.AddInvVect(iv)

			// When there is a failure fetching the final entry
			// and the done channel was sent in due to there
			// being no outstanding not found inventory, consume
			// it here because there is now not found inventory
			// that will use the channel momentarily.
			if i == len(msg.InvList)-1 && c != nil {
				<-c
			}
		}
		numAdded++
		waitChan = c
	}
	if len(notFound.InvList) != 0 {
		p.QueueMessage(notFound, doneChan)
	}

	// Wait for messages to be sent. We can send quite a lot of data at this
	// point and this will keep the peer busy for a decent amount of time.
	// We don't process anything else by them in this time so that we
	// have an idea of when we should hear back from them - else the idle
	// timeout could fire when we were only half done sending the blocks.
	if numAdded > 0 {
		<-doneChan
	}
}

// OnGetBlocks is invoked when a peer receives a getblocks wire message.
func (sp *serverPeer) OnGetBlocks(p *peer.Peer, msg *wire.MsgGetBlocks) {
	// Find the most recent known block in the best chain based on the block
	// locator and fetch all of the block hashes after it until either
	// wire.MaxBlocksPerMsg have been fetched or the provided stop hash is
	// encountered.
	//
	// Use the block after the genesis block if no other blocks in the
	// provided locator are known.  This does mean the client will start
	// over with the genesis block if unknown block locators are provided.
	chain := sp.server.chain
	hashList := chain.LocateBlocks(msg.BlockLocatorHashes, &msg.HashStop,
		wire.MaxBlocksPerMsg)

	// Generate inventory message.
	invMsg := wire.NewMsgInv()
	for i := range hashList {
		iv := wire.NewInvVect(wire.InvTypeBlock, &hashList[i])
		invMsg.AddInvVect(iv)
	}

	// Send the inventory message if there is anything to send.
	if len(invMsg.InvList) > 0 {
		invListLen := len(invMsg.InvList)
		if invListLen == wire.MaxBlocksPerMsg {
			// Intentionally use a copy of the final hash so there
			// is not a reference into the inventory slice which
			// would prevent the entire slice from being eligible
			// for GC as soon as it's sent.
			continueHash := invMsg.InvList[invListLen-1].Hash
			sp.continueHash = &continueHash
		}
		p.QueueMessage(invMsg, nil)
	}
}

// OnGetHeaders is invoked when a peer receives a getheaders wire message.
func (sp *serverPeer) OnGetHeaders(p *peer.Peer, msg *wire.MsgGetHeaders) {
	// Ignore getheaders requests if not in sync.
	if !sp.server.blockManager.IsCurrent() {
		return
	}

	// Find the most recent known block in the best chain based on the block
	// locator and fetch all of the headers after it until either
	// wire.MaxBlockHeadersPerMsg have been fetched or the provided stop
	// hash is encountered.
	//
	// Use the block after the genesis block if no other blocks in the
	// provided locator are known.  This does mean the client will start
	// over with the genesis block if unknown block locators are provided.
	chain := sp.server.chain
	headers := chain.LocateHeaders(msg.BlockLocatorHashes, &msg.HashStop)

	// Send found headers to the requesting peer.
	blockHeaders := make([]*wire.BlockHeader, len(headers))
	for i := range headers {
		blockHeaders[i] = &headers[i]
	}
	p.QueueMessage(&wire.MsgHeaders{Headers: blockHeaders}, nil)
}

// enforceNodeCFFlag disconnects the peer if the server is not configured to
// allow committed filters.  Additionally, if the peer has negotiated to a
// protocol version that is high enough to observe the committed filter service
// support bit, it will be banned since it is intentionally violating the
// protocol.
func (sp *serverPeer) enforceNodeCFFlag(cmd string) bool {
	if !hasServices(sp.server.services, wire.SFNodeCF) {
		// Ban the peer if the protocol version is high enough that the peer is
		// knowingly violating the protocol and banning is enabled.
		//
		// NOTE: Even though the addBanScore function already examines whether
		// or not banning is enabled, it is checked here as well to ensure the
		// violation is logged and the peer is disconnected regardless.
		if sp.ProtocolVersion() >= wire.NodeCFVersion && !cfg.DisableBanning {
			// Disconnect the peer regardless of whether it was banned.
			sp.addBanScore(100, 0, cmd)
			sp.Disconnect()
			return false
		}

		// Disconnect the peer regardless of protocol version or banning state.
		peerLog.Debugf("%s sent an unsupported %s request -- disconnecting", sp,
			cmd)
		sp.Disconnect()
		return false
	}

	return true
}

// OnGetCFilter is invoked when a peer receives a getcfilter wire message.
func (sp *serverPeer) OnGetCFilter(p *peer.Peer, msg *wire.MsgGetCFilter) {
	// Disconnect and/or ban depending on the node cf services flag and
	// negotiated protocol version.
	if !sp.enforceNodeCFFlag(msg.Command()) {
		return
	}

	// Ignore request if CFs are disabled or the chain is not yet synced.
	if cfg.NoCFilters || !sp.server.blockManager.IsCurrent() {
		return
	}

	// Check for understood filter type.
	switch msg.FilterType {
	case wire.GCSFilterRegular, wire.GCSFilterExtended:
	default:
		peerLog.Warnf("OnGetCFilter: unsupported filter type %v",
			msg.FilterType)
		return
	}

	filterBytes, err := sp.server.cfIndex.FilterByBlockHash(&msg.BlockHash,
		msg.FilterType)
	if err != nil {
		peerLog.Errorf("OnGetCFilter: failed to fetch cfilter: %v", err)
		return
	}

	// If the filter is not saved in the index (perhaps it was removed as a
	// block was disconnected, or this has always been a sidechain block) build
	// the filter on the spot.
	if len(filterBytes) == 0 {
		block, err := sp.server.chain.BlockByHash(&msg.BlockHash)
		if err != nil {
			peerLog.Errorf("OnGetCFilter: failed to fetch non-mainchain "+
				"block %v: %v", &msg.BlockHash, err)
			return
		}

		var f *gcs.FilterV1
		switch msg.FilterType {
		case wire.GCSFilterRegular:
			f, err = blockcf.Regular(block.MsgBlock())
			if err != nil {
				peerLog.Errorf("OnGetCFilter: failed to build regular "+
					"cfilter for block %v: %v", &msg.BlockHash, err)
				return
			}
		case wire.GCSFilterExtended:
			f, err = blockcf.Extended(block.MsgBlock())
			if err != nil {
				peerLog.Errorf("OnGetCFilter: failed to build extended "+
					"cfilter for block %v: %v", &msg.BlockHash, err)
				return
			}
		default:
			peerLog.Errorf("OnGetCFilter: unhandled filter type %d",
				msg.FilterType)
			return
		}

		filterBytes = f.Bytes()
	}

	peerLog.Tracef("Obtained CF for %v", &msg.BlockHash)

	filterMsg := wire.NewMsgCFilter(&msg.BlockHash, msg.FilterType,
		filterBytes)
	sp.QueueMessage(filterMsg, nil)
}

// OnGetCFHeaders is invoked when a peer receives a getcfheader wire message.
func (sp *serverPeer) OnGetCFHeaders(p *peer.Peer, msg *wire.MsgGetCFHeaders) {
	// Disconnect and/or ban depending on the node cf services flag and
	// negotiated protocol version.
	if !sp.enforceNodeCFFlag(msg.Command()) {
		return
	}

	// Ignore request if CFs are disabled or the chain is not yet synced.
	if cfg.NoCFilters || !sp.server.blockManager.IsCurrent() {
		return
	}

	// Check for understood filter type.
	switch msg.FilterType {
	case wire.GCSFilterRegular, wire.GCSFilterExtended:
	default:
		peerLog.Warnf("OnGetCFilter: unsupported filter type %v",
			msg.FilterType)
		return
	}

	// Find the most recent known block in the best chain based on the block
	// locator and fetch all of the block hashes after it until either
	// wire.MaxCFHeadersPerMsg have been fetched or the provided stop hash is
	// encountered.
	//
	// Use the block after the genesis block if no other blocks in the provided
	// locator are known.  This does mean the served filter headers will start
	// over at the genesis block if unknown block locators are provided.
	chain := sp.server.chain
	hashList := chain.LocateBlocks(msg.BlockLocatorHashes, &msg.HashStop,
		wire.MaxCFHeadersPerMsg)
	if len(hashList) == 0 {
		return
	}

	// Generate cfheaders message and send it.
	cfIndex := sp.server.cfIndex
	headersMsg := wire.NewMsgCFHeaders()
	for i := range hashList {
		// Fetch the raw committed filter header bytes from the database.
		hash := &hashList[i]
		headerBytes, err := cfIndex.FilterHeaderByBlockHash(hash,
			msg.FilterType)
		if err != nil || len(headerBytes) == 0 {
			peerLog.Warnf("Could not obtain CF header for %v: %v", hash, err)
			return
		}

		// Deserialize the hash.
		var header chainhash.Hash
		err = header.SetBytes(headerBytes)
		if err != nil {
			peerLog.Warnf("Committed filter header deserialize failed: %v", err)
			return
		}

		headersMsg.AddCFHeader(&header)
	}
	headersMsg.FilterType = msg.FilterType
	headersMsg.StopHash = hashList[len(hashList)-1]
	sp.QueueMessage(headersMsg, nil)
}

// OnGetCFTypes is invoked when a peer receives a getcftypes wire message.
func (sp *serverPeer) OnGetCFTypes(p *peer.Peer, msg *wire.MsgGetCFTypes) {
	// Disconnect and/or ban depending on the node cf services flag and
	// negotiated protocol version.
	if !sp.enforceNodeCFFlag(msg.Command()) {
		return
	}

	// Ignore request if CFs are disabled.
	if cfg.NoCFilters {
		return
	}

	cfTypesMsg := wire.NewMsgCFTypes([]wire.FilterType{
		wire.GCSFilterRegular, wire.GCSFilterExtended})
	sp.QueueMessage(cfTypesMsg, nil)
}

// OnGetAddr is invoked when a peer receives a getaddr wire message and is used
// to provide the peer with known addresses from the address manager.
func (sp *serverPeer) OnGetAddr(p *peer.Peer, msg *wire.MsgGetAddr) {
	// Don't return any addresses when running on the simulation test
	// network.  This helps prevent the network from becoming another
	// public test network since it will not be able to learn about other
	// peers that have not specifically been provided.
	if cfg.SimNet {
		return
	}

	// Do not accept getaddr requests from outbound peers.  This reduces
	// fingerprinting attacks.
	if !p.Inbound() {
		return
	}

	// Only respond with addresses once per connection.  This helps reduce
	// traffic and further reduces fingerprinting attacks.
	if sp.addrsSent {
		peerLog.Tracef("Ignoring getaddr from %v - already sent", sp.Peer)
		return
	}
	sp.addrsSent = true

	// Get the current known addresses from the address manager.
	addrCache := sp.server.addrManager.AddressCache()

	// Push the addresses.
	sp.pushAddrMsg(addrCache)
}

// OnAddr is invoked when a peer receives an addr wire message and is used to
// notify the server about advertised addresses.
func (sp *serverPeer) OnAddr(p *peer.Peer, msg *wire.MsgAddr) {
	// Ignore addresses when running on the simulation test network.  This
	// helps prevent the network from becoming another public test network
	// since it will not be able to learn about other peers that have not
	// specifically been provided.
	if cfg.SimNet {
		return
	}

	// A message that has no addresses is invalid.
	if len(msg.AddrList) == 0 {
		peerLog.Errorf("Command [%s] from %s does not contain any addresses",
			msg.Command(), p)
		p.Disconnect()
		return
	}

	now := time.Now()
	for _, na := range msg.AddrList {
		// Don't add more address if we're disconnecting.
		if !p.Connected() {
			return
		}

		// Set the timestamp to 5 days ago if it's more than 24 hours
		// in the future so this address is one of the first to be
		// removed when space is needed.
		if na.Timestamp.After(now.Add(time.Minute * 10)) {
			na.Timestamp = now.Add(-1 * time.Hour * 24 * 5)
		}

		// Add address to known addresses for this peer.
		sp.addKnownAddresses([]*wire.NetAddress{na})
	}

	// Add addresses to server address manager.  The address manager handles
	// the details of things such as preventing duplicate addresses, max
	// addresses, and last seen updates.
	// XXX bitcoind gives a 2 hour time penalty here, do we want to do the
	// same?
	sp.server.addrManager.AddAddresses(msg.AddrList, p.NA())
}

// OnRead is invoked when a peer receives a message and it is used to update
// the bytes received by the server.
func (sp *serverPeer) OnRead(p *peer.Peer, bytesRead int, msg wire.Message, err error) {
	sp.server.AddBytesReceived(uint64(bytesRead))
}

// OnWrite is invoked when a peer sends a message and it is used to update
// the bytes sent by the server.
func (sp *serverPeer) OnWrite(p *peer.Peer, bytesWritten int, msg wire.Message, err error) {
	sp.server.AddBytesSent(uint64(bytesWritten))
}

// randomUint16Number returns a random uint16 in a specified input range.  Note
// that the range is in zeroth ordering; if you pass it 1800, you will get
// values from 0 to 1800.
func randomUint16Number(max uint16) uint16 {
	// In order to avoid modulo bias and ensure every possible outcome in
	// [0, max) has equal probability, the random number must be sampled
	// from a random source that has a range limited to a multiple of the
	// modulus.
	var randomNumber uint16
	var limitRange = (math.MaxUint16 / max) * max
	for {
		binary.Read(rand.Reader, binary.LittleEndian, &randomNumber)
		if randomNumber < limitRange {
			return (randomNumber % max)
		}
	}
}

// AddRebroadcastInventory adds 'iv' to the list of inventories to be
// rebroadcasted at random intervals until they show up in a block.
func (s *server) AddRebroadcastInventory(iv *wire.InvVect, data interface{}) {
	// Ignore if shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return
	}

	s.modifyRebroadcastInv <- broadcastInventoryAdd{invVect: iv, data: data}
}

// RemoveRebroadcastInventory removes 'iv' from the list of items to be
// rebroadcasted if present.
func (s *server) RemoveRebroadcastInventory(iv *wire.InvVect) {
	// Ignore if shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return
	}

	s.modifyRebroadcastInv <- broadcastInventoryDel(iv)
}

// PruneRebroadcastInventory filters and removes rebroadcast inventory entries
// where necessary.
func (s *server) PruneRebroadcastInventory() {
	// Ignore if shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return
	}

	s.modifyRebroadcastInv <- broadcastPruneInventory{}
}

// AnnounceNewTransactions generates and relays inventory vectors and notifies
// websocket clients of the passed transactions.  This function should be
// called whenever new transactions are added to the mempool.
func (s *server) AnnounceNewTransactions(txns []*dcrutil.Tx) {
	// Generate and relay inventory vectors for all newly accepted
	// transactions into the memory pool due to the original being
	// accepted.
	for _, tx := range txns {
		// Generate the inventory vector and relay it.
		iv := wire.NewInvVect(wire.InvTypeTx, tx.Hash())
		s.RelayInventory(iv, tx, false)

		if s.rpcServer != nil {
			// Notify websocket clients about mempool transactions.
			s.rpcServer.ntfnMgr.NotifyMempoolTx(tx, true)
		}
	}
}

// TransactionConfirmed marks the provided single confirmation transaction as
// no longer needing rebroadcasting.
func (s *server) TransactionConfirmed(tx *dcrutil.Tx) {
	// Rebroadcasting is only necessary when the RPC server is active.
	if s.rpcServer != nil {
		iv := wire.NewInvVect(wire.InvTypeTx, tx.Hash())
		s.RemoveRebroadcastInventory(iv)
	}
}

// pushTxMsg sends a tx message for the provided transaction hash to the
// connected peer.  An error is returned if the transaction hash is not known.
func (s *server) pushTxMsg(sp *serverPeer, hash *chainhash.Hash, doneChan chan<- struct{}, waitChan <-chan struct{}) error {
	// Attempt to fetch the requested transaction from the pool.  A
	// call could be made to check for existence first, but simply trying
	// to fetch a missing transaction results in the same behavior.
	// Do not allow peers to request transactions already in a block
	// but are unconfirmed, as they may be expensive. Restrict that
	// to the authenticated RPC only.
	tx, err := s.txMemPool.FetchTransaction(hash)
	if err != nil {
		peerLog.Tracef("Unable to fetch tx %v from transaction "+
			"pool: %v", hash, err)

		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return err
	}

	// Once we have fetched data wait for any previous operation to finish.
	if waitChan != nil {
		<-waitChan
	}

	sp.QueueMessage(tx.MsgTx(), doneChan)

	return nil
}

// pushBlockMsg sends a block message for the provided block hash to the
// connected peer.  An error is returned if the block hash is not known.
func (s *server) pushBlockMsg(sp *serverPeer, hash *chainhash.Hash, doneChan chan<- struct{}, waitChan <-chan struct{}) error {
	block, err := sp.server.chain.BlockByHash(hash)
	if err != nil {
		peerLog.Tracef("Unable to fetch requested block hash %v: %v",
			hash, err)

		if doneChan != nil {
			doneChan <- struct{}{}
		}
		return err
	}

	// Once we have fetched data wait for any previous operation to finish.
	if waitChan != nil {
		<-waitChan
	}

	// We only send the channel for this message if we aren't sending
	// an inv straight after.
	var dc chan<- struct{}
	continueHash := sp.continueHash
	sendInv := continueHash != nil && continueHash.IsEqual(hash)
	if !sendInv {
		dc = doneChan
	}
	sp.QueueMessage(block.MsgBlock(), dc)

	// When the peer requests the final block that was advertised in
	// response to a getblocks message which requested more blocks than
	// would fit into a single message, send it a new inventory message
	// to trigger it to issue another getblocks message for the next
	// batch of inventory.
	if sendInv {
		best := sp.server.chain.BestSnapshot()
		invMsg := wire.NewMsgInvSizeHint(1)
		iv := wire.NewInvVect(wire.InvTypeBlock, &best.Hash)
		invMsg.AddInvVect(iv)
		sp.QueueMessage(invMsg, doneChan)
		sp.continueHash = nil
	}
	return nil
}

// handleUpdatePeerHeight updates the heights of all peers who were known to
// announce a block we recently accepted.
func (s *server) handleUpdatePeerHeights(state *peerState, umsg updatePeerHeightsMsg) {
	state.forAllPeers(func(sp *serverPeer) {
		// The origin peer should already have the updated height.
		if sp == umsg.originPeer {
			return
		}

		// This is a pointer to the underlying memory which doesn't
		// change.
		latestBlkHash := sp.LastAnnouncedBlock()

		// Skip this peer if it hasn't recently announced any new blocks.
		if latestBlkHash == nil {
			return
		}

		// If the peer has recently announced a block, and this block
		// matches our newly accepted block, then update their block
		// height.
		if *latestBlkHash == *umsg.newHash {
			sp.UpdateLastBlockHeight(umsg.newHeight)
			sp.UpdateLastAnnouncedBlock(nil)
		}
	})
}

// handleAddPeerMsg deals with adding new peers.  It is invoked from the
// peerHandler goroutine.
func (s *server) handleAddPeerMsg(state *peerState, sp *serverPeer) bool {
	if sp == nil {
		return false
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		srvrLog.Infof("New peer %s ignored - server is shutting down", sp)
		sp.Disconnect()
		return false
	}

	// Disconnect banned peers.
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		srvrLog.Debugf("can't split hostport %v", err)
		sp.Disconnect()
		return false
	}
	if banEnd, ok := state.banned[host]; ok {
		if time.Now().Before(banEnd) {
			srvrLog.Debugf("Peer %s is banned for another %v - disconnecting",
				host, time.Until(banEnd))
			sp.Disconnect()
			return false
		}

		srvrLog.Infof("Peer %s is no longer banned", host)
		delete(state.banned, host)
	}

	// Limit max number of connections from a single IP.  However, allow
	// whitelisted inbound peers and localhost connections regardless.
	isInboundWhitelisted := sp.isWhitelisted && sp.Inbound()
	peerIP := sp.NA().IP
	if cfg.MaxSameIP > 0 && !isInboundWhitelisted && !peerIP.IsLoopback() &&
		state.ConnectionsWithIP(peerIP)+1 > cfg.MaxSameIP {
		srvrLog.Infof("Max connections with %s reached [%d] - "+
			"disconnecting peer", sp, cfg.MaxSameIP)
		sp.Disconnect()
		return false
	}

	// Limit max number of total peers.  However, allow whitelisted inbound
	// peers regardless.
	if state.Count()+1 > cfg.MaxPeers && !isInboundWhitelisted {
		srvrLog.Infof("Max peers reached [%d] - disconnecting peer %s",
			cfg.MaxPeers, sp)
		sp.Disconnect()
		// TODO: how to handle permanent peers here?
		// they should be rescheduled.
		return false
	}

	// Add the new peer and start it.
	srvrLog.Debugf("New peer %s", sp)
	if sp.Inbound() {
		state.inboundPeers[sp.ID()] = sp
	} else {
		state.outboundGroups[addrmgr.GroupKey(sp.NA())]++
		if sp.persistent {
			state.persistentPeers[sp.ID()] = sp
		} else {
			state.outboundPeers[sp.ID()] = sp
		}

		// Fetch the suggested public ip from the outbound peer if
		// there are no prevailing conditions to disable automatic
		// network address discovery.
		//
		// The conditions to disable automatic network address
		// discovery are:
		//	- If there is a proxy set (--proxy, --onion).
		//	- If automatic network address discovery is explicitly
		//		disabled (--nodiscoverip).
		//	- If there is an external ip explicitly set (--externalip).
		//	- If listening has been disabled (--nolisten, listen
		//	disabled because of --connect, etc).
		//	- If Universal Plug and Play is enabled (--upnp).
		//	- If the active network is simnet or regnet.
		if (cfg.Proxy != "" || cfg.OnionProxy != "") ||
			cfg.NoDiscoverIP || len(cfg.ExternalIPs) > 0 ||
			(cfg.DisableListen || len(cfg.Listeners) == 0) || cfg.Upnp ||
			activeNetParams.Name == simNetParams.Name ||
			activeNetParams.Name == regNetParams.Name {
			return true
		}

		sp.peerNaMtx.Lock()
		na := sp.peerNa
		sp.peerNaMtx.Unlock()

		if na == nil {
			return true
		}

		if !s.addrManager.IsPeerNaValid(na, sp.NA()) {
			return true
		}

		port, err := strconv.ParseUint(activeNetParams.DefaultPort, 10, 16)
		if err != nil {
			srvrLog.Errorf("unable to parse active network port: %v", err)
			return true
		}

		id := na.IP.String()
		switch {
		case na.IP.To4() != nil:
			state.suggestionsMtx.Lock()
			state.suggestions[addrmgr.IPv4Address][id]++
			state.suggestionsMtx.Unlock()

			state.ResolveLocalAddress(addrmgr.IPv4Address, s.addrManager,
				s.services, uint16(port))

		case na.IP.To16() != nil:
			state.suggestionsMtx.Lock()
			state.suggestions[addrmgr.IPv6Address][id]++
			state.suggestionsMtx.Unlock()

			state.ResolveLocalAddress(addrmgr.IPv6Address, s.addrManager,
				s.services, uint16(port))

		default:
			return true
		}
	}

	return true
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It is
// invoked from the peerHandler goroutine.
func (s *server) handleDonePeerMsg(state *peerState, sp *serverPeer) {
	var list map[int32]*serverPeer
	if sp.persistent {
		list = state.persistentPeers
	} else if sp.Inbound() {
		list = state.inboundPeers
	} else {
		list = state.outboundPeers
	}
	if _, ok := list[sp.ID()]; ok {
		if !sp.Inbound() && sp.VersionKnown() {
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
		}
		if !sp.Inbound() && sp.connReq != nil {
			s.connManager.Disconnect(sp.connReq.ID())
		}
		delete(list, sp.ID())
		srvrLog.Debugf("Removed peer %s", sp)
		return
	}

	if sp.connReq != nil {
		s.connManager.Disconnect(sp.connReq.ID())
	}

	// Update the address' last seen time if the peer has acknowledged
	// our version and has sent us its version as well.
	if sp.VerAckReceived() && sp.VersionKnown() && sp.NA() != nil {
		s.addrManager.Connected(sp.NA())
	}

	// If we get here it means that either we didn't know about the peer
	// or we purposefully deleted it.
}

// handleBanPeerMsg deals with banning peers.  It is invoked from the
// peerHandler goroutine.
func (s *server) handleBanPeerMsg(state *peerState, sp *serverPeer) {
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		srvrLog.Debugf("can't split ban peer %s %v", sp.Addr(), err)
		return
	}
	direction := directionString(sp.Inbound())
	srvrLog.Infof("Banned peer %s (%s) for %v", host, direction,
		cfg.BanDuration)
	state.banned[host] = time.Now().Add(cfg.BanDuration)
}

// handleRelayInvMsg deals with relaying inventory to peers that are not already
// known to have it.  It is invoked from the peerHandler goroutine.
func (s *server) handleRelayInvMsg(state *peerState, msg relayMsg) {
	state.forAllPeers(func(sp *serverPeer) {
		if !sp.Connected() {
			return
		}

		// If the inventory is a block and the peer prefers headers,
		// generate and send a headers message instead of an inventory
		// message.
		if msg.invVect.Type == wire.InvTypeBlock && sp.WantsHeaders() {
			blockHeader, ok := msg.data.(wire.BlockHeader)
			if !ok {
				peerLog.Warnf("Underlying data for headers" +
					" is not a block header")
				return
			}
			msgHeaders := wire.NewMsgHeaders()
			if err := msgHeaders.AddBlockHeader(&blockHeader); err != nil {
				peerLog.Errorf("Failed to add block"+
					" header: %v", err)
				return
			}
			sp.QueueMessage(msgHeaders, nil)
			return
		}

		if msg.invVect.Type == wire.InvTypeTx {
			// Don't relay the transaction to the peer when it has
			// transaction relaying disabled.
			if sp.relayTxDisabled() {
				return
			}
		}

		// Either queue the inventory to be relayed immediately or with
		// the next batch depending on the immediate flag.
		//
		// It will be ignored in either case if the peer is already
		// known to have the inventory.
		if msg.immediate {
			sp.QueueInventoryImmediate(msg.invVect)
		} else {
			sp.QueueInventory(msg.invVect)
		}
	})
}

// handleBroadcastMsg deals with broadcasting messages to peers.  It is invoked
// from the peerHandler goroutine.
func (s *server) handleBroadcastMsg(state *peerState, bmsg *broadcastMsg) {
	state.forAllPeers(func(sp *serverPeer) {
		if !sp.Connected() {
			return
		}

		for _, ep := range bmsg.excludePeers {
			if sp == ep {
				return
			}
		}

		sp.QueueMessage(bmsg.message, nil)
	})
}

type getConnCountMsg struct {
	reply chan int32
}

type getPeersMsg struct {
	reply chan []*serverPeer
}

type getOutboundGroup struct {
	key   string
	reply chan int
}

type getAddedNodesMsg struct {
	reply chan []*serverPeer
}

type disconnectNodeMsg struct {
	cmp   func(*serverPeer) bool
	reply chan error
}

type connectNodeMsg struct {
	addr      string
	permanent bool
	reply     chan error
}

type removeNodeMsg struct {
	cmp   func(*serverPeer) bool
	reply chan error
}

// handleQuery is the central handler for all queries and commands from other
// goroutines related to peer state.
func (s *server) handleQuery(state *peerState, querymsg interface{}) {
	switch msg := querymsg.(type) {
	case getConnCountMsg:
		nconnected := int32(0)
		state.forAllPeers(func(sp *serverPeer) {
			if sp.Connected() {
				nconnected++
			}
		})
		msg.reply <- nconnected

	case getPeersMsg:
		peers := make([]*serverPeer, 0, state.Count())
		state.forAllPeers(func(sp *serverPeer) {
			if !sp.Connected() {
				return
			}
			peers = append(peers, sp)
		})
		msg.reply <- peers

	case connectNodeMsg:
		// XXX duplicate oneshots?
		// Limit max number of total peers.
		if state.Count() >= cfg.MaxPeers {
			msg.reply <- errors.New("max peers reached")
			return
		}
		for _, peer := range state.persistentPeers {
			if peer.Addr() == msg.addr {
				if msg.permanent {
					msg.reply <- errors.New("peer already connected")
				} else {
					msg.reply <- errors.New("peer exists as a permanent peer")
				}
				return
			}
		}

		netAddr, err := addrStringToNetAddr(msg.addr)
		if err != nil {
			msg.reply <- err
			return
		}

		// TODO: if too many, nuke a non-perm peer.
		go s.connManager.Connect(&connmgr.ConnReq{
			Addr:      netAddr,
			Permanent: msg.permanent,
		})
		msg.reply <- nil
	case removeNodeMsg:
		found := disconnectPeer(state.persistentPeers, msg.cmp, func(sp *serverPeer) {
			// Keep group counts ok since we remove from
			// the list now.
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--

			peerLog.Debugf("Removing persistent peer %s:%d (reqid %d)",
				sp.NA().IP, sp.NA().Port, sp.connReq.ID())
			connReq := sp.connReq

			// Mark the peer's connReq as nil to prevent it from scheduling a
			// re-connect attempt.
			sp.connReq = nil
			s.connManager.Remove(connReq.ID())
		})

		if found {
			msg.reply <- nil
		} else {
			msg.reply <- errors.New("peer not found")
		}
	case getOutboundGroup:
		count, ok := state.outboundGroups[msg.key]
		if ok {
			msg.reply <- count
		} else {
			msg.reply <- 0
		}
	// Request a list of the persistent (added) peers.
	case getAddedNodesMsg:
		// Respond with a slice of the relevant peers.
		peers := make([]*serverPeer, 0, len(state.persistentPeers))
		for _, sp := range state.persistentPeers {
			peers = append(peers, sp)
		}
		msg.reply <- peers
	case disconnectNodeMsg:
		// Check inbound peers. We pass a nil callback since we don't
		// require any additional actions on disconnect for inbound peers.
		found := disconnectPeer(state.inboundPeers, msg.cmp, nil)
		if found {
			msg.reply <- nil
			return
		}

		// Check outbound peers.
		found = disconnectPeer(state.outboundPeers, msg.cmp, func(sp *serverPeer) {
			// Keep group counts ok since we remove from
			// the list now.
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
		})
		if found {
			// If there are multiple outbound connections to the same
			// ip:port, continue disconnecting them all until no such
			// peers are found.
			for found {
				found = disconnectPeer(state.outboundPeers, msg.cmp, func(sp *serverPeer) {
					state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
				})
			}
			msg.reply <- nil
			return
		}

		msg.reply <- errors.New("peer not found")
	}
}

// disconnectPeer attempts to drop the connection of a targeted peer in the
// passed peer list. Targets are identified via usage of the passed
// `compareFunc`, which should return `true` if the passed peer is the target
// peer. This function returns true on success and false if the peer is unable
// to be located. If the peer is found, and the passed callback: `whenFound'
// isn't nil, we call it with the peer as the argument before it is removed
// from the peerList, and is disconnected from the server.
func disconnectPeer(peerList map[int32]*serverPeer, compareFunc func(*serverPeer) bool, whenFound func(*serverPeer)) bool {
	for addr, peer := range peerList {
		if compareFunc(peer) {
			if whenFound != nil {
				whenFound(peer)
			}

			// This is ok because we are not continuing
			// to iterate so won't corrupt the loop.
			delete(peerList, addr)
			peer.Disconnect()
			return true
		}
	}
	return false
}

// newPeerConfig returns the configuration for the given serverPeer.
func newPeerConfig(sp *serverPeer) *peer.Config {
	var userAgentComments []string
	if version.PreRelease != "" {
		userAgentComments = append(userAgentComments, version.PreRelease)
	}

	return &peer.Config{
		Listeners: peer.MessageListeners{
			OnVersion:        sp.OnVersion,
			OnMemPool:        sp.OnMemPool,
			OnGetMiningState: sp.OnGetMiningState,
			OnMiningState:    sp.OnMiningState,
			OnTx:             sp.OnTx,
			OnBlock:          sp.OnBlock,
			OnInv:            sp.OnInv,
			OnHeaders:        sp.OnHeaders,
			OnGetData:        sp.OnGetData,
			OnGetBlocks:      sp.OnGetBlocks,
			OnGetHeaders:     sp.OnGetHeaders,
			OnGetCFilter:     sp.OnGetCFilter,
			OnGetCFHeaders:   sp.OnGetCFHeaders,
			OnGetCFTypes:     sp.OnGetCFTypes,
			OnGetAddr:        sp.OnGetAddr,
			OnAddr:           sp.OnAddr,
			OnRead:           sp.OnRead,
			OnWrite:          sp.OnWrite,
		},
		NewestBlock:       sp.newestBlock,
		HostToNetAddress:  sp.server.addrManager.HostToNetAddress,
		Proxy:             cfg.Proxy,
		UserAgentName:     userAgentName,
		UserAgentVersion:  userAgentVersion,
		UserAgentComments: userAgentComments,
		Net:               sp.server.chainParams.Net,
		Services:          sp.server.services,
		DisableRelayTx:    cfg.BlocksOnly,
		ProtocolVersion:   maxProtocolVersion,
	}
}

// inboundPeerConnected is invoked by the connection manager when a new inbound
// connection is established.  It initializes a new inbound server peer
// instance, associates it with the connection, and starts a goroutine to wait
// for disconnection.
func (s *server) inboundPeerConnected(conn net.Conn) {
	sp := newServerPeer(s, false)
	sp.isWhitelisted = isWhitelisted(conn.RemoteAddr())
	sp.Peer = peer.NewInboundPeer(newPeerConfig(sp))
	sp.AssociateConnection(conn)
	go s.peerDoneHandler(sp)
}

// outboundPeerConnected is invoked by the connection manager when a new
// outbound connection is established.  It initializes a new outbound server
// peer instance, associates it with the relevant state such as the connection
// request instance and the connection itself, and finally notifies the address
// manager of the attempt.
func (s *server) outboundPeerConnected(c *connmgr.ConnReq, conn net.Conn) {
	sp := newServerPeer(s, c.Permanent)
	p, err := peer.NewOutboundPeer(newPeerConfig(sp), c.Addr.String())
	if err != nil {
		srvrLog.Debugf("Cannot create outbound peer %s: %v", c.Addr, err)
		s.connManager.Disconnect(c.ID())
		return
	}
	sp.Peer = p
	sp.connReq = c
	sp.isWhitelisted = isWhitelisted(conn.RemoteAddr())
	sp.AssociateConnection(conn)
	go s.peerDoneHandler(sp)
	s.addrManager.Attempt(sp.NA())
}

// peerDoneHandler handles peer disconnects by notifying the server that it's
// done.
func (s *server) peerDoneHandler(sp *serverPeer) {
	sp.WaitForDisconnect()
	s.donePeers <- sp

	// Only tell block manager we are gone if we ever told it we existed.
	if sp.VersionKnown() {
		s.blockManager.DonePeer(sp)
	}
	close(sp.quit)
}

// peerHandler is used to handle peer operations such as adding and removing
// peers to and from the server, banning peers, and broadcasting messages to
// peers.  It must be run in a goroutine.
func (s *server) peerHandler() {
	// Start the address manager and block manager, both of which are needed
	// by peers.  This is done here since their lifecycle is closely tied
	// to this handler and rather than adding more channels to synchronize
	// things, it's easier and slightly faster to simply start and stop them
	// in this handler.
	s.addrManager.Start()
	s.blockManager.Start()

	srvrLog.Tracef("Starting peer handler")

	state := &peerState{
		inboundPeers:    make(map[int32]*serverPeer),
		persistentPeers: make(map[int32]*serverPeer),
		outboundPeers:   make(map[int32]*serverPeer),
		banned:          make(map[string]time.Time),
		outboundGroups:  make(map[string]int),
		suggestions: map[addrmgr.NetworkAddress]map[string]int32{
			addrmgr.IPv4Address: make(map[string]int32),
			addrmgr.IPv6Address: make(map[string]int32),
		},
	}

	if !cfg.DisableDNSSeed {
		// Add peers discovered through DNS to the address manager.
		params := activeNetParams.Params
		seeds := make([]string, 0, len(params.DNSSeeds))
		for _, seed := range params.DNSSeeds {
			seeds = append(seeds, seed.Host)
		}
		defaultPort, _ := strconv.Atoi(params.DefaultPort)
		connmgr.SeedFromDNS(seeds, uint16(defaultPort), defaultRequiredServices,
			dcrdLookup, func(addrs []*wire.NetAddress) {
				// Bitcoind uses a lookup of the dns seeder here. This
				// is rather strange since the values looked up by the
				// DNS seed lookups will vary quite a lot.
				// to replicate this behaviour we put all addresses as
				// having come from the first one.
				s.addrManager.AddAddresses(addrs, addrs[0])
			})
	}
	go s.connManager.Start()

out:
	for {
		select {
		// New peers connected to the server.
		case p := <-s.newPeers:
			s.handleAddPeerMsg(state, p)

		// Disconnected peers.
		case p := <-s.donePeers:
			s.handleDonePeerMsg(state, p)

		// Block accepted in mainchain or orphan, update peer height.
		case umsg := <-s.peerHeightsUpdate:
			s.handleUpdatePeerHeights(state, umsg)

		// Peer to ban.
		case p := <-s.banPeers:
			s.handleBanPeerMsg(state, p)

		// New inventory to potentially be relayed to other peers.
		case invMsg := <-s.relayInv:
			s.handleRelayInvMsg(state, invMsg)

		// Message to broadcast to all connected peers except those
		// which are excluded by the message.
		case bmsg := <-s.broadcast:
			s.handleBroadcastMsg(state, &bmsg)

		case qmsg := <-s.query:
			s.handleQuery(state, qmsg)

		case <-s.quit:
			// Disconnect all peers on server shutdown.
			state.forAllPeers(func(sp *serverPeer) {
				srvrLog.Tracef("Shutdown peer %s", sp)
				sp.Disconnect()
			})
			break out
		}
	}

	s.connManager.Stop()
	s.blockManager.Stop()
	s.addrManager.Stop()

	// Drain channels before exiting so nothing is left waiting around
	// to send.
cleanup:
	for {
		select {
		case <-s.newPeers:
		case <-s.donePeers:
		case <-s.peerHeightsUpdate:
		case <-s.relayInv:
		case <-s.broadcast:
		case <-s.query:
		default:
			break cleanup
		}
	}
	s.wg.Done()
	srvrLog.Tracef("Peer handler done")
}

// AddPeer adds a new peer that has already been connected to the server.
func (s *server) AddPeer(sp *serverPeer) {
	s.newPeers <- sp
}

// BanPeer bans a peer that has already been connected to the server by ip.
func (s *server) BanPeer(sp *serverPeer) {
	s.banPeers <- sp
}

// RelayInventory relays the passed inventory vector to all connected peers
// that are not already known to have it.
func (s *server) RelayInventory(invVect *wire.InvVect, data interface{}, immediate bool) {
	s.relayInv <- relayMsg{invVect: invVect, data: data, immediate: immediate}
}

// BroadcastMessage sends msg to all peers currently connected to the server
// except those in the passed peers to exclude.
func (s *server) BroadcastMessage(msg wire.Message, exclPeers ...*serverPeer) {
	// XXX: Need to determine if this is an alert that has already been
	// broadcast and refrain from broadcasting again.
	bmsg := broadcastMsg{message: msg, excludePeers: exclPeers}
	s.broadcast <- bmsg
}

// ConnectedCount returns the number of currently connected peers.
func (s *server) ConnectedCount() int32 {
	replyChan := make(chan int32)

	s.query <- getConnCountMsg{reply: replyChan}

	return <-replyChan
}

// OutboundGroupCount returns the number of peers connected to the given
// outbound group key.
func (s *server) OutboundGroupCount(key string) int {
	replyChan := make(chan int)
	s.query <- getOutboundGroup{key: key, reply: replyChan}
	return <-replyChan
}

// AddedNodeInfo returns an array of dcrjson.GetAddedNodeInfoResult structures
// describing the persistent (added) nodes.
func (s *server) AddedNodeInfo() []*serverPeer {
	replyChan := make(chan []*serverPeer)
	s.query <- getAddedNodesMsg{reply: replyChan}
	return <-replyChan
}

// Peers returns an array of all connected peers.
func (s *server) Peers() []*serverPeer {
	replyChan := make(chan []*serverPeer)

	s.query <- getPeersMsg{reply: replyChan}

	return <-replyChan
}

// DisconnectNodeByAddr disconnects a peer by target address. Both outbound and
// inbound nodes will be searched for the target node. An error message will
// be returned if the peer was not found.
func (s *server) DisconnectNodeByAddr(addr string) error {
	replyChan := make(chan error)

	s.query <- disconnectNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.Addr() == addr },
		reply: replyChan,
	}

	return <-replyChan
}

// DisconnectNodeByID disconnects a peer by target node id. Both outbound and
// inbound nodes will be searched for the target node. An error message will be
// returned if the peer was not found.
func (s *server) DisconnectNodeByID(id int32) error {
	replyChan := make(chan error)

	s.query <- disconnectNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.ID() == id },
		reply: replyChan,
	}

	return <-replyChan
}

// RemoveNodeByAddr removes a peer from the list of persistent peers if
// present. An error will be returned if the peer was not found.
func (s *server) RemoveNodeByAddr(addr string) error {
	replyChan := make(chan error)

	s.query <- removeNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.Addr() == addr },
		reply: replyChan,
	}

	return <-replyChan
}

// RemoveNodeByID removes a peer by node ID from the list of persistent peers
// if present. An error will be returned if the peer was not found.
func (s *server) RemoveNodeByID(id int32) error {
	replyChan := make(chan error)

	s.query <- removeNodeMsg{
		cmp:   func(sp *serverPeer) bool { return sp.ID() == id },
		reply: replyChan,
	}

	return <-replyChan
}

// ConnectNode adds `addr' as a new outbound peer. If permanent is true then the
// peer will be persistent and reconnect if the connection is lost.
// It is an error to call this with an already existing peer.
func (s *server) ConnectNode(addr string, permanent bool) error {
	replyChan := make(chan error)

	s.query <- connectNodeMsg{addr: addr, permanent: permanent, reply: replyChan}

	return <-replyChan
}

// AddBytesSent adds the passed number of bytes to the total bytes sent counter
// for the server.  It is safe for concurrent access.
func (s *server) AddBytesSent(bytesSent uint64) {
	atomic.AddUint64(&s.bytesSent, bytesSent)
}

// AddBytesReceived adds the passed number of bytes to the total bytes received
// counter for the server.  It is safe for concurrent access.
func (s *server) AddBytesReceived(bytesReceived uint64) {
	atomic.AddUint64(&s.bytesReceived, bytesReceived)
}

// NetTotals returns the sum of all bytes received and sent across the network
// for all peers.  It is safe for concurrent access.
func (s *server) NetTotals() (uint64, uint64) {
	return atomic.LoadUint64(&s.bytesReceived),
		atomic.LoadUint64(&s.bytesSent)
}

// UpdatePeerHeights updates the heights of all peers who have announced
// the latest connected main chain block, or a recognized orphan. These height
// updates allow us to dynamically refresh peer heights, ensuring sync peer
// selection has access to the latest block heights for each peer.
func (s *server) UpdatePeerHeights(latestBlkHash *chainhash.Hash, latestHeight int64, updateSource *serverPeer) {
	s.peerHeightsUpdate <- updatePeerHeightsMsg{
		newHash:    latestBlkHash,
		newHeight:  latestHeight,
		originPeer: updateSource,
	}
}

// rebroadcastHandler keeps track of user submitted inventories that we have
// sent out but have not yet made it into a block. We periodically rebroadcast
// them in case our peers restarted or otherwise lost track of them.
func (s *server) rebroadcastHandler() {
	// Wait 5 min before first tx rebroadcast.
	timer := time.NewTimer(5 * time.Minute)
	pendingInvs := make(map[wire.InvVect]interface{})

out:
	for {
		select {

		case riv := <-s.modifyRebroadcastInv:
			switch msg := riv.(type) {

			// Incoming InvVects are added to our map of RPC txs.
			case broadcastInventoryAdd:
				pendingInvs[*msg.invVect] = msg.data

			// When an InvVect has been added to a block, we can
			// now remove it, if it was present.
			case broadcastInventoryDel:
				delete(pendingInvs, *msg)

			case broadcastPruneInventory:
				best := s.chain.BestSnapshot()
				nextStakeDiff, err :=
					s.chain.CalcNextRequiredStakeDifficulty()
				if err != nil {
					srvrLog.Errorf("Failed to get next stake difficulty: %v",
						err)
					break
				}

				for iv, data := range pendingInvs {
					tx, ok := data.(*dcrutil.Tx)
					if !ok {
						continue
					}

					txType := stake.DetermineTxType(tx.MsgTx())

					// Remove the ticket rebroadcast if the amount not equal to
					// the current stake difficulty.
					if txType == stake.TxTypeSStx &&
						tx.MsgTx().TxOut[0].Value != nextStakeDiff {
						delete(pendingInvs, iv)
						srvrLog.Debugf("Pending ticket purchase broadcast "+
							"inventory for tx %v removed. Ticket value not "+
							"equal to stake difficulty.", tx.Hash())
						continue
					}

					// Remove the ticket rebroadcast if it has already expired.
					if txType == stake.TxTypeSStx &&
						blockchain.IsExpired(tx, best.Height) {
						delete(pendingInvs, iv)
						srvrLog.Debugf("Pending ticket purchase broadcast "+
							"inventory for tx %v removed. Transaction "+
							"expired.", tx.Hash())
						continue
					}

					// Remove the revocation rebroadcast if the associated
					// ticket has been revived.
					if txType == stake.TxTypeSSRtx {
						refSStxHash := tx.MsgTx().TxIn[0].PreviousOutPoint.Hash
						if !s.chain.CheckLiveTicket(refSStxHash) {
							delete(pendingInvs, iv)
							srvrLog.Debugf("Pending revocation broadcast "+
								"inventory for tx %v removed. "+
								"Associated ticket was revived.", tx.Hash())
							continue
						}
					}
				}
			}

		case <-timer.C:
			// Any inventory we have has not made it into a block
			// yet. We periodically resubmit them until they have.
			for iv, data := range pendingInvs {
				ivCopy := iv
				s.RelayInventory(&ivCopy, data, false)
			}

			// Process at a random time up to 30mins (in seconds)
			// in the future.
			timer.Reset(time.Second *
				time.Duration(randomUint16Number(1800)))

		case <-s.quit:
			break out
		}
	}

	timer.Stop()

	// Drain channels before exiting so nothing is left waiting around
	// to send.
cleanup:
	for {
		select {
		case <-s.modifyRebroadcastInv:
		default:
			break cleanup
		}
	}
	s.wg.Done()
}

// Start begins accepting connections from peers.
func (s *server) Start() {
	// Already started?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}

	srvrLog.Trace("Starting server")

	// Start the peer handler which in turn starts the address and block
	// managers.
	s.wg.Add(1)
	go s.peerHandler()

	if s.nat != nil {
		s.wg.Add(1)
		go s.upnpUpdateThread()
	}

	if !cfg.DisableRPC {
		s.wg.Add(1)

		// Start the rebroadcastHandler, which ensures user tx received by
		// the RPC server are rebroadcast until being included in a block.
		go s.rebroadcastHandler()

		s.rpcServer.Start()
	}

	// Start the background block template generator if the config provides
	// a mining address.
	if len(cfg.MiningAddrs) > 0 {
		s.wg.Add(1)
		go func(s *server) {
			s.bg.Run(s.context)
			s.wg.Done()
		}(s)
	}

	// Start the CPU miner if generation is enabled.
	if cfg.Generate {
		s.cpuMiner.Start()
	}
}

// Stop gracefully shuts down the server by stopping and disconnecting all
// peers and the main listener.
func (s *server) Stop() error {
	// Make sure this only happens once.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		srvrLog.Infof("Server is already in the process of shutting down")
		return nil
	}

	srvrLog.Warnf("Server shutting down")

	// Stop the background block template generator.
	if len(cfg.miningAddrs) > 0 {
		minrLog.Info("Background block template generator shutting down")
		s.cancel()
	}

	// Stop the CPU miner if needed.
	if cfg.Generate && s.cpuMiner != nil {
		s.cpuMiner.Stop()
	}

	// Shutdown the RPC server if it's not disabled.
	if !cfg.DisableRPC && s.rpcServer != nil {
		s.rpcServer.Stop()
	}

	s.feeEstimator.Close()

	// Signal the remaining goroutines to quit.
	close(s.quit)
	return nil
}

// WaitForShutdown blocks until the main listener and peer handlers are stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}

// parseListeners splits the list of listen addresses passed in addrs into
// IPv4 and IPv6 slices and returns them.  This allows easy creation of the
// listeners on the correct interface "tcp4" and "tcp6".  It also properly
// detects addresses which apply to "all interfaces" and adds the address to
// both slices.
func parseListeners(addrs []string) ([]string, []string, bool, error) {
	ipv4ListenAddrs := make([]string, 0, len(addrs)*2)
	ipv6ListenAddrs := make([]string, 0, len(addrs)*2)
	haveWildcard := false

	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			// Shouldn't happen due to already being normalized.
			return nil, nil, false, err
		}

		// Empty host or host of * on plan9 is both IPv4 and IPv6.
		if host == "" || (host == "*" && runtime.GOOS == "plan9") {
			ipv4ListenAddrs = append(ipv4ListenAddrs, addr)
			ipv6ListenAddrs = append(ipv6ListenAddrs, addr)
			haveWildcard = true
			continue
		}

		// Strip IPv6 zone id if present since net.ParseIP does not
		// handle it.
		zoneIndex := strings.LastIndex(host, "%")
		if zoneIndex > 0 {
			host = host[:zoneIndex]
		}

		// Parse the IP.
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, nil, false, fmt.Errorf("'%s' is not a "+
				"valid IP address", host)
		}

		// To4 returns nil when the IP is not an IPv4 address, so use
		// this determine the address type.
		if ip.To4() == nil {
			ipv6ListenAddrs = append(ipv6ListenAddrs, addr)
		} else {
			ipv4ListenAddrs = append(ipv4ListenAddrs, addr)
		}
	}
	return ipv4ListenAddrs, ipv6ListenAddrs, haveWildcard, nil
}

func (s *server) upnpUpdateThread() {
	// Go off immediately to prevent code duplication, thereafter we renew
	// lease every 15 minutes.
	timer := time.NewTimer(0 * time.Second)
	lport, _ := strconv.ParseInt(activeNetParams.DefaultPort, 10, 16)
	first := true
out:
	for {
		select {
		case <-timer.C:
			// TODO: pick external port more cleverly
			// TODO: know which ports we are listening to on an external net.
			// TODO: if specific listen port doesn't work then ask for wildcard
			// listen port?
			// XXX this assumes timeout is in seconds.
			listenPort, err := s.nat.AddPortMapping("tcp", int(lport), int(lport),
				"dcrd listen port", 20*60)
			if err != nil {
				srvrLog.Warnf("can't add UPnP port mapping: %v", err)
			}
			if first && err == nil {
				// TODO: look this up periodically to see if upnp domain changed
				// and so did ip.
				externalip, err := s.nat.GetExternalAddress()
				if err != nil {
					srvrLog.Warnf("UPnP can't get external address: %v", err)
					continue out
				}
				na := wire.NewNetAddressIPPort(externalip, uint16(listenPort),
					s.services)
				err = s.addrManager.AddLocalAddress(na, addrmgr.UpnpPrio)
				if err != nil {
					srvrLog.Warnf("Failed to add UPnP local address %s: %v",
						na.IP.String(), err)
				} else {
					srvrLog.Warnf("Successfully bound via UPnP to %s",
						addrmgr.NetAddressKey(na))
					first = false
				}
			}
			timer.Reset(time.Minute * 15)
		case <-s.quit:
			break out
		}
	}

	timer.Stop()

	if err := s.nat.DeletePortMapping("tcp", int(lport), int(lport)); err != nil {
		srvrLog.Warnf("unable to remove UPnP port mapping: %v", err)
	} else {
		srvrLog.Debugf("successfully disestablished UPnP port mapping")
	}

	s.wg.Done()
}

// standardScriptVerifyFlags returns the script flags that should be used when
// executing transaction scripts to enforce additional checks which are required
// for the script to be considered standard.  Note these flags are different
// than what is required for the consensus rules in that they are more strict.
func standardScriptVerifyFlags(chain *blockchain.BlockChain) (txscript.ScriptFlags, error) {
	scriptFlags := mempool.BaseStandardVerifyFlags

	// Enable validation of OP_SHA256 if the stake vote for the agenda is
	// active.
	isActive, err := chain.IsLNFeaturesAgendaActive()
	if err != nil {
		return 0, err
	}
	if isActive {
		scriptFlags |= txscript.ScriptVerifySHA256
	}
	return scriptFlags, nil
}

// newServer returns a new dcrd server configured to listen on addr for the
// Decred network type specified by chainParams.  Use start to begin accepting
// connections from peers.
func newServer(listenAddrs []string, db database.DB, chainParams *chaincfg.Params, dataDir string, interrupt <-chan struct{}) (*server, error) {
	services := defaultServices
	if cfg.NoCFilters {
		services &^= wire.SFNodeCF
	}

	amgr := addrmgr.New(cfg.DataDir, dcrdLookup)

	var listeners []net.Listener
	var nat NAT
	if !cfg.DisableListen {
		ipv4Addrs, ipv6Addrs, wildcard, err :=
			parseListeners(listenAddrs)
		if err != nil {
			return nil, err
		}
		listeners = make([]net.Listener, 0, len(ipv4Addrs)+len(ipv6Addrs))
		discover := true
		if len(cfg.ExternalIPs) != 0 {
			discover = false
			// if this fails we have real issues.
			port, _ := strconv.ParseUint(
				activeNetParams.DefaultPort, 10, 16)

			for _, sip := range cfg.ExternalIPs {
				eport := uint16(port)
				host, portstr, err := net.SplitHostPort(sip)
				if err != nil {
					// no port, use default.
					host = sip
				} else {
					port, err := strconv.ParseUint(
						portstr, 10, 16)
					if err != nil {
						srvrLog.Warnf("Can not parse "+
							"port from %s for "+
							"externalip: %v", sip,
							err)
						continue
					}
					eport = uint16(port)
				}
				na, err := amgr.HostToNetAddress(host, eport,
					services)
				if err != nil {
					srvrLog.Warnf("Not adding %s as "+
						"externalip: %v", sip, err)
					continue
				}

				err = amgr.AddLocalAddress(na, addrmgr.ManualPrio)
				if err != nil {
					amgrLog.Warnf("Skipping specified external IP: %v", err)
				}
			}
		} else if discover && cfg.Upnp {
			nat, err = Discover()
			if err != nil {
				srvrLog.Warnf("Can't discover upnp: %v", err)
			}
			// nil nat here is fine, just means no upnp on network.
		}

		// TODO: nonstandard port...
		if wildcard {
			port, err :=
				strconv.ParseUint(activeNetParams.DefaultPort,
					10, 16)
			if err != nil {
				// I can't think of a cleaner way to do this...
				goto nowc
			}
			addrs, err := net.InterfaceAddrs()
			if err != nil {
				srvrLog.Warnf("Unable to get interface addresses: %v", err)
			}
			for _, a := range addrs {
				ip, _, err := net.ParseCIDR(a.String())
				if err != nil {
					continue
				}
				na := wire.NewNetAddressIPPort(ip,
					uint16(port), services)
				if discover {
					err = amgr.AddLocalAddress(na, addrmgr.InterfacePrio)
					if err != nil {
						amgrLog.Debugf("Skipping local address: %v", err)
					}
				}
			}
		}
	nowc:

		for _, addr := range ipv4Addrs {
			listener, err := net.Listen("tcp4", addr)
			if err != nil {
				srvrLog.Warnf("Can't listen on %s: %v", addr,
					err)
				continue
			}
			listeners = append(listeners, listener)

			if discover {
				if na, err := amgr.DeserializeNetAddress(addr); err == nil {
					err = amgr.AddLocalAddress(na, addrmgr.BoundPrio)
					if err != nil {
						amgrLog.Warnf("Skipping bound address: %v", err)
					}
				}
			}
		}

		for _, addr := range ipv6Addrs {
			listener, err := net.Listen("tcp6", addr)
			if err != nil {
				srvrLog.Warnf("Can't listen on %s: %v", addr,
					err)
				continue
			}
			listeners = append(listeners, listener)
			if discover {
				if na, err := amgr.DeserializeNetAddress(addr); err == nil {
					err = amgr.AddLocalAddress(na, addrmgr.BoundPrio)
					if err != nil {
						amgrLog.Debugf("Skipping bound address: %v", err)
					}
				}
			}
		}

		if len(listeners) == 0 {
			return nil, errors.New("no valid listen address")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := server{
		chainParams:          chainParams,
		addrManager:          amgr,
		newPeers:             make(chan *serverPeer, cfg.MaxPeers),
		donePeers:            make(chan *serverPeer, cfg.MaxPeers),
		banPeers:             make(chan *serverPeer, cfg.MaxPeers),
		query:                make(chan interface{}),
		relayInv:             make(chan relayMsg, cfg.MaxPeers),
		broadcast:            make(chan broadcastMsg, cfg.MaxPeers),
		quit:                 make(chan struct{}),
		modifyRebroadcastInv: make(chan interface{}),
		peerHeightsUpdate:    make(chan updatePeerHeightsMsg),
		nat:                  nat,
		db:                   db,
		timeSource:           blockchain.NewMedianTime(),
		services:             services,
		sigCache:             txscript.NewSigCache(cfg.SigCacheMaxSize),
		subsidyCache:         standalone.NewSubsidyCache(chainParams),
		context:              ctx,
		cancel:               cancel,
	}

	// Create the transaction and address indexes if needed.
	//
	// CAUTION: the txindex needs to be first in the indexes array because
	// the addrindex uses data from the txindex during catchup.  If the
	// addrindex is run first, it may not have the transactions from the
	// current block indexed.
	var indexes []indexers.Indexer
	if cfg.TxIndex || cfg.AddrIndex {
		// Enable transaction index if address index is enabled since it
		// requires it.
		if !cfg.TxIndex {
			indxLog.Infof("Transaction index enabled because it " +
				"is required by the address index")
			cfg.TxIndex = true
		} else {
			indxLog.Info("Transaction index is enabled")
		}

		s.txIndex = indexers.NewTxIndex(db)
		indexes = append(indexes, s.txIndex)
	}
	if cfg.AddrIndex {
		indxLog.Info("Address index is enabled")
		s.addrIndex = indexers.NewAddrIndex(db, chainParams)
		indexes = append(indexes, s.addrIndex)
	}
	if !cfg.NoExistsAddrIndex {
		indxLog.Info("Exists address index is enabled")
		s.existsAddrIndex = indexers.NewExistsAddrIndex(db, chainParams)
		indexes = append(indexes, s.existsAddrIndex)
	}
	if !cfg.NoCFilters {
		indxLog.Info("CF index is enabled")
		s.cfIndex = indexers.NewCfIndex(db, chainParams)
		indexes = append(indexes, s.cfIndex)
	}

	feC := fees.EstimatorConfig{
		MinBucketFee: cfg.minRelayTxFee,
		MaxBucketFee: dcrutil.Amount(fees.DefaultMaxBucketFeeMultiplier) * cfg.minRelayTxFee,
		MaxConfirms:  fees.DefaultMaxConfirmations,
		FeeRateStep:  fees.DefaultFeeRateStep,
		DatabaseFile: path.Join(dataDir, "feesdb"),

		// 1e5 is the previous (up to 1.1.0) mempool.DefaultMinRelayTxFee that
		// un-upgraded wallets will be using, so track this particular rate
		// explicitly. Note that bumping this value will cause the existing fees
		// database to become invalid and will force nodes to explicitly delete
		// it.
		ExtraBucketFee: 1e5,
	}
	fe, err := fees.NewEstimator(&feC)
	if err != nil {
		return nil, err
	}
	s.feeEstimator = fe

	// Create an index manager if any of the optional indexes are enabled.
	var indexManager blockchain.IndexManager
	if len(indexes) > 0 {
		indexManager = indexers.NewManager(db, indexes, chainParams)
	}

	// Create a new block chain instance with the appropriate configuration.
	s.chain, err = blockchain.New(&blockchain.Config{
		DB:          s.db,
		Interrupt:   interrupt,
		ChainParams: s.chainParams,
		TimeSource:  s.timeSource,
		Notifications: func(notification *blockchain.Notification) {
			if s.blockManager != nil {
				s.blockManager.handleBlockchainNotification(notification)
			}
		},
		SigCache:     s.sigCache,
		SubsidyCache: s.subsidyCache,
		IndexManager: indexManager,
	})
	if err != nil {
		return nil, err
	}

	txC := mempool.Config{
		Policy: mempool.Policy{
			MaxTxVersion:         2,
			DisableRelayPriority: cfg.NoRelayPriority,
			AcceptNonStd:         cfg.AcceptNonStd,
			FreeTxRelayLimit:     cfg.FreeTxRelayLimit,
			MaxOrphanTxs:         cfg.MaxOrphanTxs,
			MaxOrphanTxSize:      defaultMaxOrphanTxSize,
			MaxSigOpsPerTx:       blockchain.MaxSigOpsPerBlock / 5,
			MinRelayTxFee:        cfg.minRelayTxFee,
			AllowOldVotes:        cfg.AllowOldVotes,
			StandardVerifyFlags: func() (txscript.ScriptFlags, error) {
				return standardScriptVerifyFlags(s.chain)
			},
			AcceptSequenceLocks: s.chain.IsFixSeqLocksAgendaActive,
		},
		ChainParams: chainParams,
		NextStakeDifficulty: func() (int64, error) {
			return s.chain.BestSnapshot().NextStakeDiff, nil
		},
		FetchUtxoView:    s.chain.FetchUtxoView,
		BlockByHash:      s.chain.BlockByHash,
		BestHash:         func() *chainhash.Hash { return &s.chain.BestSnapshot().Hash },
		BestHeight:       func() int64 { return s.chain.BestSnapshot().Height },
		CalcSequenceLock: s.chain.CalcSequenceLock,
		SubsidyCache:     s.subsidyCache,
		SigCache:         s.sigCache,
		PastMedianTime: func() time.Time {
			return s.chain.BestSnapshot().MedianTime
		},
		AddrIndex:                 s.addrIndex,
		ExistsAddrIndex:           s.existsAddrIndex,
		AddTxToFeeEstimation:      s.feeEstimator.AddMemPoolTransaction,
		RemoveTxFromFeeEstimation: s.feeEstimator.RemoveMemPoolTransaction,
		OnVoteReceived: func(voteTx *dcrutil.Tx) {
			if s.bg != nil {
				s.bg.VoteReceived(voteTx)
			}
		},
	}
	s.txMemPool = mempool.New(&txC)
	s.blockManager, err = newBlockManager(&blockManagerConfig{
		PeerNotifier:       &s,
		Chain:              s.chain,
		ChainParams:        s.chainParams,
		SubsidyCache:       s.subsidyCache,
		TimeSource:         s.timeSource,
		FeeEstimator:       s.feeEstimator,
		TxMemPool:          s.txMemPool,
		BgBlkTmplGenerator: s.bg,
		NotifyWinningTickets: func(wtnd *WinningTicketsNtfnData) {
			if s.rpcServer != nil {
				s.rpcServer.ntfnMgr.NotifyWinningTickets(wtnd)
			}
		},
		PruneRebroadcastInventory: s.PruneRebroadcastInventory,
		RpcServer: func() *rpcServer {
			return s.rpcServer
		},
	})
	if err != nil {
		return nil, err
	}

	// Create the mining policy and block template generator based on the
	// configuration options.
	//
	// NOTE: The CPU miner relies on the mempool, so the mempool has to be
	// created before calling the function to create the CPU miner.
	policy := mining.Policy{
		BlockMinSize:      cfg.BlockMinSize,
		BlockMaxSize:      cfg.BlockMaxSize,
		BlockPrioritySize: cfg.BlockPrioritySize,
		TxMinFreeFee:      cfg.minRelayTxFee,
	}
	tg := newBlkTmplGenerator(&policy, s.txMemPool, s.timeSource, s.sigCache,
		s.subsidyCache, s.chainParams, s.chain, s.blockManager)

	// Create the background block template generator if the config has a
	// mining address.
	if len(cfg.miningAddrs) > 0 {
		s.bg = newBgBlkTmplGenerator(tg, cfg.miningAddrs, cfg.SimNet)
	}

	s.cpuMiner = newCPUMiner(&cpuminerConfig{
		ChainParams:                s.chainParams,
		PermitConnectionlessMining: cfg.SimNet,
		BlockTemplateGenerator:     tg,
		MiningAddrs:                cfg.miningAddrs,
		ProcessBlock:               s.blockManager.ProcessBlock,
		ConnectedCount:             s.ConnectedCount,
		IsCurrent:                  s.blockManager.IsCurrent,
	})

	// Only setup a function to return new addresses to connect to when
	// not running in connect-only mode.  The simulation network is always
	// in connect-only mode since it is only intended to connect to
	// specified peers and actively avoid advertising and connecting to
	// discovered peers in order to prevent it from becoming a public test
	// network.
	var newAddressFunc func() (net.Addr, error)
	if !cfg.SimNet && len(cfg.ConnectPeers) == 0 {
		newAddressFunc = func() (net.Addr, error) {
			for tries := 0; tries < 100; tries++ {
				addr := s.addrManager.GetAddress()
				if addr == nil {
					break
				}

				// Address will not be invalid, local or unroutable
				// because addrmanager rejects those on addition.
				// Just check that we don't already have an address
				// in the same group so that we are not connecting
				// to the same network segment at the expense of
				// others.
				key := addrmgr.GroupKey(addr.NetAddress())
				if s.OutboundGroupCount(key) != 0 {
					continue
				}

				// only allow recent nodes (10mins) after we failed 30
				// times
				if tries < 30 && time.Since(addr.LastAttempt()) < 10*time.Minute {
					continue
				}

				// allow nondefault ports after 50 failed tries.
				if fmt.Sprintf("%d", addr.NetAddress().Port) !=
					activeNetParams.DefaultPort && tries < 50 {
					continue
				}

				addrString := addrmgr.NetAddressKey(addr.NetAddress())
				return addrStringToNetAddr(addrString)
			}

			return nil, errors.New("no valid connect address")
		}
	}

	// Create a connection manager.
	targetOutbound := defaultTargetOutbound
	if cfg.MaxPeers < targetOutbound {
		targetOutbound = cfg.MaxPeers
	}
	cmgr, err := connmgr.New(&connmgr.Config{
		Listeners:      listeners,
		OnAccept:       s.inboundPeerConnected,
		RetryDuration:  connectionRetryInterval,
		TargetOutbound: uint32(targetOutbound),
		Dial:           dcrdDial,
		OnConnection:   s.outboundPeerConnected,
		GetNewAddress:  newAddressFunc,
	})
	if err != nil {
		return nil, err
	}
	s.connManager = cmgr

	// Start up persistent peers.
	permanentPeers := cfg.ConnectPeers
	if len(permanentPeers) == 0 {
		permanentPeers = cfg.AddPeers
	}
	for _, addr := range permanentPeers {
		tcpAddr, err := addrStringToNetAddr(addr)
		if err != nil {
			return nil, err
		}

		go s.connManager.Connect(&connmgr.ConnReq{
			Addr:      tcpAddr,
			Permanent: true,
		})
	}

	if !cfg.DisableRPC {
		s.rpcServer, err = newRPCServer(cfg.RPCListeners, tg, &s)
		if err != nil {
			return nil, err
		}

		// Signal process shutdown when the RPC server requests it.
		go func() {
			<-s.rpcServer.RequestedProcessShutdown()
			shutdownRequestChannel <- struct{}{}
		}()
	}

	return &s, nil
}

// addrStringToNetAddr takes an address in the form of 'host:port' and returns
// a net.Addr which maps to the original address with any host names resolved
// to IP addresses.
func addrStringToNetAddr(addr string) (net.Addr, error) {
	host, strPort, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Attempt to look up an IP address associated with the parsed host.
	// The dcrdLookup function will transparently handle performing the
	// lookup over Tor if necessary.
	ips, err := dcrdLookup(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses found for %s", host)
	}

	port, err := strconv.Atoi(strPort)
	if err != nil {
		return nil, err
	}

	return &net.TCPAddr{
		IP:   ips[0],
		Port: port,
	}, nil
}

// isWhitelisted returns whether the IP address is included in the whitelisted
// networks and IPs.
func isWhitelisted(addr net.Addr) bool {
	if len(cfg.whitelists) == 0 {
		return false
	}

	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		srvrLog.Warnf("Unable to SplitHostPort on '%s': %v", addr, err)
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		srvrLog.Warnf("Unable to parse IP '%s'", addr)
		return false
	}

	for _, ipnet := range cfg.whitelists {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}
