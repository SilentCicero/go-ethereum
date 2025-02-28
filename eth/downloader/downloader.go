package downloader

import (
	"bytes"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"gopkg.in/fatih/set.v0"
)

var (
	MinHashFetch  = 512  // Minimum amount of hashes to not consider a peer stalling
	MaxHashFetch  = 2048 // Amount of hashes to be fetched per retrieval request
	MaxBlockFetch = 128  // Amount of blocks to be fetched per retrieval request

	hashTTL         = 5 * time.Second  // Time it takes for a hash request to time out
	blockSoftTTL    = 3 * time.Second  // Request completion threshold for increasing or decreasing a peer's bandwidth
	blockHardTTL    = 3 * blockSoftTTL // Maximum time allowance before a block request is considered expired
	crossCheckCycle = time.Second      // Period after which to check for expired cross checks

	maxBannedHashes = 4096 // Number of bannable hashes before phasing old ones out
)

var (
	errLowTd            = errors.New("peers TD is too low")
	ErrBusy             = errors.New("busy")
	errUnknownPeer      = errors.New("peer is unknown or unhealthy")
	ErrBadPeer          = errors.New("action from bad peer ignored")
	ErrStallingPeer     = errors.New("peer is stalling")
	errBannedHead       = errors.New("peer head hash already banned")
	errNoPeers          = errors.New("no peers to keep download active")
	ErrPendingQueue     = errors.New("pending items in queue")
	ErrTimeout          = errors.New("timeout")
	ErrEmptyHashSet     = errors.New("empty hash set by peer")
	errPeersUnavailable = errors.New("no peers available or all peers tried for block download process")
	errAlreadyInPool    = errors.New("hash already in pool")
	ErrInvalidChain     = errors.New("retrieved hash chain is invalid")
	ErrCrossCheckFailed = errors.New("block cross-check failed")
	errCancelHashFetch  = errors.New("hash fetching cancelled (requested)")
	errCancelBlockFetch = errors.New("block downloading cancelled (requested)")
	errNoSyncActive     = errors.New("no sync active")
)

type hashCheckFn func(common.Hash) bool
type getBlockFn func(common.Hash) *types.Block
type chainInsertFn func(types.Blocks) (int, error)
type hashIterFn func() (common.Hash, error)

type blockPack struct {
	peerId string
	blocks []*types.Block
}

type hashPack struct {
	peerId string
	hashes []common.Hash
}

type crossCheck struct {
	expire time.Time
	parent common.Hash
}

type Downloader struct {
	mux *event.TypeMux

	queue  *queue                      // Scheduler for selecting the hashes to download
	peers  *peerSet                    // Set of active peers from which download can proceed
	checks map[common.Hash]*crossCheck // Pending cross checks to verify a hash chain
	banned *set.Set                    // Set of hashes we've received and banned

	// Statistics
	importStart time.Time // Instance when the last blocks were taken from the cache
	importQueue []*Block  // Previously taken blocks to check import progress
	importDone  int       // Number of taken blocks already imported from the last batch
	importLock  sync.Mutex

	// Callbacks
	hasBlock hashCheckFn
	getBlock getBlockFn

	// Status
	synchronising int32
	notified      int32

	// Channels
	newPeerCh chan *peer
	hashCh    chan hashPack
	blockCh   chan blockPack

	cancelCh   chan struct{} // Channel to cancel mid-flight syncs
	cancelLock sync.RWMutex  // Lock to protect the cancel channel in delivers
}

// Block is an origin-tagged blockchain block.
type Block struct {
	RawBlock   *types.Block
	OriginPeer string
}

func New(mux *event.TypeMux, hasBlock hashCheckFn, getBlock getBlockFn) *Downloader {
	// Create the base downloader
	downloader := &Downloader{
		mux:       mux,
		queue:     newQueue(),
		peers:     newPeerSet(),
		hasBlock:  hasBlock,
		getBlock:  getBlock,
		newPeerCh: make(chan *peer, 1),
		hashCh:    make(chan hashPack, 1),
		blockCh:   make(chan blockPack, 1),
	}
	// Inject all the known bad hashes
	downloader.banned = set.New()
	for hash, _ := range core.BadHashes {
		downloader.banned.Add(hash)
	}
	return downloader
}

// Stats retrieves the current status of the downloader.
func (d *Downloader) Stats() (pending int, cached int, importing int, estimate time.Duration) {
	// Fetch the download status
	pending, cached = d.queue.Size()

	// Figure out the import progress
	d.importLock.Lock()
	defer d.importLock.Unlock()

	for len(d.importQueue) > 0 && d.hasBlock(d.importQueue[0].RawBlock.Hash()) {
		d.importQueue = d.importQueue[1:]
		d.importDone++
	}
	importing = len(d.importQueue)

	// Make an estimate on the total sync
	estimate = 0
	if d.importDone > 0 {
		estimate = time.Since(d.importStart) / time.Duration(d.importDone) * time.Duration(pending+cached+importing)
	}
	return
}

// Synchronising returns the state of the downloader
func (d *Downloader) Synchronising() bool {
	return atomic.LoadInt32(&d.synchronising) > 0
}

// RegisterPeer injects a new download peer into the set of block source to be
// used for fetching hashes and blocks from.
func (d *Downloader) RegisterPeer(id string, head common.Hash, getHashes hashFetcherFn, getBlocks blockFetcherFn) error {
	// If the peer wants to send a banned hash, reject
	if d.banned.Has(head) {
		glog.V(logger.Debug).Infoln("Register rejected, head hash banned:", id)
		return errBannedHead
	}
	// Otherwise try to construct and register the peer
	glog.V(logger.Detail).Infoln("Registering peer", id)
	if err := d.peers.Register(newPeer(id, head, getHashes, getBlocks)); err != nil {
		glog.V(logger.Error).Infoln("Register failed:", err)
		return err
	}
	return nil
}

// UnregisterPeer remove a peer from the known list, preventing any action from
// the specified peer.
func (d *Downloader) UnregisterPeer(id string) error {
	glog.V(logger.Detail).Infoln("Unregistering peer", id)
	if err := d.peers.Unregister(id); err != nil {
		glog.V(logger.Error).Infoln("Unregister failed:", err)
		return err
	}
	return nil
}

// Synchronise will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if it's TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
func (d *Downloader) Synchronise(id string, hash common.Hash) error {
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&d.synchronising, 0, 1) {
		return ErrBusy
	}
	defer atomic.StoreInt32(&d.synchronising, 0)

	// If the head hash is banned, terminate immediately
	if d.banned.Has(hash) {
		return ErrInvalidChain
	}
	// Post a user notification of the sync (only once per session)
	if atomic.CompareAndSwapInt32(&d.notified, 0, 1) {
		glog.V(logger.Info).Infoln("Block synchronisation started")
	}

	// Create cancel channel for aborting mid-flight
	d.cancelLock.Lock()
	d.cancelCh = make(chan struct{})
	d.cancelLock.Unlock()

	// Abort if the queue still contains some leftover data
	if _, cached := d.queue.Size(); cached > 0 && d.queue.GetHeadBlock() != nil {
		return ErrPendingQueue
	}
	// Reset the queue and peer set to clean any internal leftover state
	d.queue.Reset()
	d.peers.Reset()
	d.checks = make(map[common.Hash]*crossCheck)

	// Retrieve the origin peer and initiate the downloading process
	p := d.peers.Peer(id)
	if p == nil {
		return errUnknownPeer
	}

	return d.syncWithPeer(p, hash)
}

// TakeBlocks takes blocks from the queue and yields them to the caller.
func (d *Downloader) TakeBlocks() []*Block {
	blocks := d.queue.TakeBlocks()
	if len(blocks) > 0 {
		d.importLock.Lock()
		d.importStart = time.Now()
		d.importQueue = blocks
		d.importDone = 0
		d.importLock.Unlock()
	}
	return blocks
}

// Has checks if the downloader knows about a particular hash, meaning that its
// either already downloaded of pending retrieval.
func (d *Downloader) Has(hash common.Hash) bool {
	return d.queue.Has(hash)
}

// syncWithPeer starts a block synchronization based on the hash chain from the
// specified peer and head hash.
func (d *Downloader) syncWithPeer(p *peer, hash common.Hash) (err error) {
	d.mux.Post(StartEvent{})
	defer func() {
		// reset on error
		if err != nil {
			d.Cancel()
			d.mux.Post(FailedEvent{err})
		} else {
			d.mux.Post(DoneEvent{})
		}
	}()

	glog.V(logger.Debug).Infoln("Synchronizing with the network using:", p.id)
	if err = d.fetchHashes(p, hash); err != nil {
		return err
	}
	if err = d.fetchBlocks(); err != nil {
		return err
	}
	glog.V(logger.Debug).Infoln("Synchronization completed")

	return nil
}

// Cancel cancels all of the operations and resets the queue. It returns true
// if the cancel operation was completed.
func (d *Downloader) Cancel() bool {
	// If we're not syncing just return.
	hs, bs := d.queue.Size()
	if atomic.LoadInt32(&d.synchronising) == 0 && hs == 0 && bs == 0 {
		return false
	}
	// Close the current cancel channel
	d.cancelLock.Lock()
	select {
	case <-d.cancelCh:
		// Channel was already closed
	default:
		close(d.cancelCh)
	}
	d.cancelLock.Unlock()

	// Reset the queue and import statistics
	d.queue.Reset()

	d.importLock.Lock()
	d.importQueue = nil
	d.importDone = 0
	d.importLock.Unlock()

	return true
}

// XXX Make synchronous
func (d *Downloader) fetchHashes(p *peer, h common.Hash) error {
	var (
		start  = time.Now()
		active = p             // active peer will help determine the current active peer
		head   = common.Hash{} // common and last hash

		timeout     = time.NewTimer(0)                // timer to dump a non-responsive active peer
		attempted   = make(map[string]bool)           // attempted peers will help with retries
		crossTicker = time.NewTicker(crossCheckCycle) // ticker to periodically check expired cross checks
	)
	defer crossTicker.Stop()
	defer timeout.Stop()

	glog.V(logger.Debug).Infof("Downloading hashes (%x) from %s", h[:4], p.id)
	<-timeout.C // timeout channel should be initially empty.

	getHashes := func(from common.Hash) {
		active.getHashes(from)
		timeout.Reset(hashTTL)
	}

	// Add the hash to the queue, and start hash retrieval.
	d.queue.Insert([]common.Hash{h})
	getHashes(h)

	attempted[p.id] = true
	for finished := false; !finished; {
		select {
		case <-d.cancelCh:
			return errCancelHashFetch

		case hashPack := <-d.hashCh:
			// Make sure the active peer is giving us the hashes
			if hashPack.peerId != active.id {
				glog.V(logger.Debug).Infof("Received hashes from incorrect peer(%s)", hashPack.peerId)
				break
			}
			timeout.Stop()

			// Make sure the peer actually gave something valid
			if len(hashPack.hashes) == 0 {
				glog.V(logger.Debug).Infof("Peer (%s) responded with empty hash set", active.id)
				return ErrEmptyHashSet
			}
			for index, hash := range hashPack.hashes {
				if d.banned.Has(hash) {
					glog.V(logger.Debug).Infof("Peer (%s) sent a known invalid chain", active.id)

					d.queue.Insert(hashPack.hashes[:index+1])
					if err := d.banBlocks(active.id, hash); err != nil {
						glog.V(logger.Debug).Infof("Failed to ban batch of blocks: %v", err)
					}
					return ErrInvalidChain
				}
			}
			// Determine if we're done fetching hashes (queue up all pending), and continue if not done
			done, index := false, 0
			for index, head = range hashPack.hashes {
				if d.hasBlock(head) || d.queue.GetBlock(head) != nil {
					glog.V(logger.Debug).Infof("Found common hash %x", head[:4])
					hashPack.hashes = hashPack.hashes[:index]
					done = true
					break
				}
			}
			// Insert all the new hashes, but only continue if got something useful
			inserts := d.queue.Insert(hashPack.hashes)
			if len(inserts) == 0 && !done {
				glog.V(logger.Debug).Infof("Peer (%s) responded with stale hashes", active.id)
				return ErrBadPeer
			}
			if !done {
				// Check that the peer is not stalling the sync
				if len(inserts) < MinHashFetch {
					return ErrStallingPeer
				}
				// Try and fetch a random block to verify the hash batch
				// Skip the last hash as the cross check races with the next hash fetch
				cross := rand.Intn(len(inserts) - 1)
				origin, parent := inserts[cross], inserts[cross+1]
				glog.V(logger.Detail).Infof("Cross checking (%s) with %x/%x", active.id, origin, parent)

				d.checks[origin] = &crossCheck{
					expire: time.Now().Add(blockSoftTTL),
					parent: parent,
				}
				active.getBlocks([]common.Hash{origin})

				// Also fetch a fresh
				getHashes(head)
				continue
			}
			// We're done, prepare the download cache and proceed pulling the blocks
			offset := 0
			if block := d.getBlock(head); block != nil {
				offset = int(block.NumberU64() + 1)
			}
			d.queue.Prepare(offset)
			finished = true

		case blockPack := <-d.blockCh:
			// Cross check the block with the random verifications
			if blockPack.peerId != active.id || len(blockPack.blocks) != 1 {
				continue
			}
			block := blockPack.blocks[0]
			if check, ok := d.checks[block.Hash()]; ok {
				if block.ParentHash() != check.parent {
					return ErrCrossCheckFailed
				}
				delete(d.checks, block.Hash())
			}

		case <-crossTicker.C:
			// Iterate over all the cross checks and fail the hash chain if they're not verified
			for hash, check := range d.checks {
				if time.Now().After(check.expire) {
					glog.V(logger.Debug).Infof("Cross check timeout for %x", hash)
					return ErrCrossCheckFailed
				}
			}

		case <-timeout.C:
			glog.V(logger.Debug).Infof("Peer (%s) didn't respond in time for hash request", p.id)

			var p *peer // p will be set if a peer can be found
			// Attempt to find a new peer by checking inclusion of peers best hash in our
			// already fetched hash list. This can't guarantee 100% correctness but does
			// a fair job. This is always either correct or false incorrect.
			for _, peer := range d.peers.AllPeers() {
				if d.queue.Has(peer.head) && !attempted[peer.id] {
					p = peer
					break
				}
			}
			// if all peers have been tried, abort the process entirely or if the hash is
			// the zero hash.
			if p == nil || (head == common.Hash{}) {
				return ErrTimeout
			}
			// set p to the active peer. this will invalidate any hashes that may be returned
			// by our previous (delayed) peer.
			active = p
			getHashes(head)
			glog.V(logger.Debug).Infof("Hash fetching switched to new peer(%s)", p.id)
		}
	}
	glog.V(logger.Debug).Infof("Downloaded hashes (%d) in %v", d.queue.Pending(), time.Since(start))

	return nil
}

// fetchBlocks iteratively downloads the entire schedules block-chain, taking
// any available peers, reserving a chunk of blocks for each, wait for delivery
// and periodically checking for timeouts.
func (d *Downloader) fetchBlocks() error {
	glog.V(logger.Debug).Infoln("Downloading", d.queue.Pending(), "block(s)")
	start := time.Now()

	// Start a ticker to continue throttled downloads and check for bad peers
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

out:
	for {
		select {
		case <-d.cancelCh:
			return errCancelBlockFetch

		case <-d.hashCh:
			// Out of bounds hashes received, ignore them

		case blockPack := <-d.blockCh:
			// Short circuit if it's a stale cross check
			if len(blockPack.blocks) == 1 {
				block := blockPack.blocks[0]
				if _, ok := d.checks[block.Hash()]; ok {
					delete(d.checks, block.Hash())
					break
				}
			}
			// If the peer was previously banned and failed to deliver it's pack
			// in a reasonable time frame, ignore it's message.
			if peer := d.peers.Peer(blockPack.peerId); peer != nil {
				// Deliver the received chunk of blocks, and demote in case of errors
				err := d.queue.Deliver(blockPack.peerId, blockPack.blocks)
				switch err {
				case nil:
					// If no blocks were delivered, demote the peer (need the delivery above)
					if len(blockPack.blocks) == 0 {
						peer.Demote()
						peer.SetIdle()
						glog.V(logger.Detail).Infof("%s: no blocks delivered", peer)
						break
					}
					// All was successful, promote the peer
					peer.Promote()
					peer.SetIdle()
					glog.V(logger.Detail).Infof("%s: delivered %d blocks", peer, len(blockPack.blocks))

				case ErrInvalidChain:
					// The hash chain is invalid (blocks are not ordered properly), abort
					return err

				case errNoFetchesPending:
					// Peer probably timed out with its delivery but came through
					// in the end, demote, but allow to to pull from this peer.
					peer.Demote()
					peer.SetIdle()
					glog.V(logger.Detail).Infof("%s: out of bound delivery", peer)

				case errStaleDelivery:
					// Delivered something completely else than requested, usually
					// caused by a timeout and delivery during a new sync cycle.
					// Don't set it to idle as the original request should still be
					// in flight.
					peer.Demote()
					glog.V(logger.Detail).Infof("%s: stale delivery", peer)

				default:
					// Peer did something semi-useful, demote but keep it around
					peer.Demote()
					peer.SetIdle()
					glog.V(logger.Detail).Infof("%s: delivery partially failed: %v", peer, err)
				}
			}

		case <-ticker.C:
			// Short circuit if we lost all our peers
			if d.peers.Len() == 0 {
				return errNoPeers
			}
			// Check for block request timeouts and demote the responsible peers
			badPeers := d.queue.Expire(blockHardTTL)
			for _, pid := range badPeers {
				if peer := d.peers.Peer(pid); peer != nil {
					peer.Demote()
					glog.V(logger.Detail).Infof("%s: block delivery timeout", peer)
				}
			}
			// If there are unrequested hashes left start fetching from the available peers
			if d.queue.Pending() > 0 {
				// Throttle the download if block cache is full and waiting processing
				if d.queue.Throttle() {
					break
				}
				// Send a download request to all idle peers, until throttled
				idlePeers := d.peers.IdlePeers()
				for _, peer := range idlePeers {
					// Short circuit if throttling activated since above
					if d.queue.Throttle() {
						break
					}
					// Get a possible chunk. If nil is returned no chunk
					// could be returned due to no hashes available.
					request := d.queue.Reserve(peer, peer.Capacity())
					if request == nil {
						continue
					}
					if glog.V(logger.Detail) {
						glog.Infof("%s: requesting %d blocks", peer, len(request.Hashes))
					}
					// Fetch the chunk and check for error. If the peer was somehow
					// already fetching a chunk due to a bug, it will be returned to
					// the queue
					if err := peer.Fetch(request); err != nil {
						glog.V(logger.Error).Infof("Peer %s received double work", peer.id)
						d.queue.Cancel(request)
					}
				}
				// Make sure that we have peers available for fetching. If all peers have been tried
				// and all failed throw an error
				if d.queue.InFlight() == 0 {
					return errPeersUnavailable
				}

			} else if d.queue.InFlight() == 0 {
				// When there are no more queue and no more in flight, We can
				// safely assume we're done. Another part of the process will  check
				// for parent errors and will re-request anything that's missing
				break out
			}
		}
	}
	glog.V(logger.Detail).Infoln("Downloaded block(s) in", time.Since(start))
	return nil
}

// banBlocks retrieves a batch of blocks from a peer feeding us invalid hashes,
// and bans the head of the retrieved batch.
//
// This method only fetches one single batch as the goal is not ban an entire
// (potentially long) invalid chain - wasting a lot of time in the meanwhile -,
// but rather to gradually build up a blacklist if the peer keeps reconnecting.
func (d *Downloader) banBlocks(peerId string, head common.Hash) error {
	glog.V(logger.Debug).Infof("Banning a batch out of %d blocks from %s", d.queue.Pending(), peerId)

	// Ask the peer being banned for a batch of blocks from the banning point
	peer := d.peers.Peer(peerId)
	if peer == nil {
		return nil
	}
	request := d.queue.Reserve(peer, MaxBlockFetch)
	if request == nil {
		return nil
	}
	if err := peer.Fetch(request); err != nil {
		return err
	}
	// Wait a bit for the reply to arrive, and ban if done so
	timeout := time.After(blockHardTTL)
	for {
		select {
		case <-d.cancelCh:
			return errCancelBlockFetch

		case <-timeout:
			return ErrTimeout

		case <-d.hashCh:
			// Out of bounds hashes received, ignore them

		case blockPack := <-d.blockCh:
			blocks := blockPack.blocks

			// Short circuit if it's a stale cross check
			if len(blocks) == 1 {
				block := blocks[0]
				if _, ok := d.checks[block.Hash()]; ok {
					delete(d.checks, block.Hash())
					break
				}
			}
			// Short circuit if it's not from the peer being banned
			if blockPack.peerId != peerId {
				break
			}
			// Short circuit if no blocks were returned
			if len(blocks) == 0 {
				return errors.New("no blocks returned to ban")
			}
			// Reconstruct the original chain order and ensure we're banning the correct blocks
			types.BlockBy(types.Number).Sort(blocks)
			if bytes.Compare(blocks[0].Hash().Bytes(), head.Bytes()) != 0 {
				return errors.New("head block not the banned one")
			}
			index := 0
			for _, block := range blocks[1:] {
				if bytes.Compare(block.ParentHash().Bytes(), blocks[index].Hash().Bytes()) != 0 {
					break
				}
				index++
			}
			// Ban the head hash and phase out any excess
			d.banned.Add(blocks[index].Hash())
			for d.banned.Size() > maxBannedHashes {
				var evacuate common.Hash

				d.banned.Each(func(item interface{}) bool {
					// Skip any hard coded bans
					if core.BadHashes[item.(common.Hash)] {
						return true
					}
					evacuate = item.(common.Hash)
					return false
				})
				d.banned.Remove(evacuate)
			}
			glog.V(logger.Debug).Infof("Banned %d blocks from: %s", index+1, peerId)
			return nil
		}
	}
}

// DeliverBlocks injects a new batch of blocks received from a remote node.
// This is usually invoked through the BlocksMsg by the protocol handler.
func (d *Downloader) DeliverBlocks(id string, blocks []*types.Block) error {
	// Make sure the downloader is active
	if atomic.LoadInt32(&d.synchronising) == 0 {
		return errNoSyncActive
	}
	// Deliver or abort if the sync is canceled while queuing
	d.cancelLock.RLock()
	cancel := d.cancelCh
	d.cancelLock.RUnlock()

	select {
	case d.blockCh <- blockPack{id, blocks}:
		return nil

	case <-cancel:
		return errNoSyncActive
	}
}

// DeliverHashes injects a new batch of hashes received from a remote node into
// the download schedule. This is usually invoked through the BlockHashesMsg by
// the protocol handler.
func (d *Downloader) DeliverHashes(id string, hashes []common.Hash) error {
	// Make sure the downloader is active
	if atomic.LoadInt32(&d.synchronising) == 0 {
		return errNoSyncActive
	}
	// Deliver or abort if the sync is canceled while queuing
	d.cancelLock.RLock()
	cancel := d.cancelCh
	d.cancelLock.RUnlock()

	select {
	case d.hashCh <- hashPack{id, hashes}:
		return nil

	case <-cancel:
		return errNoSyncActive
	}
}
