// nolint:unused
package v2

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/types"
)

// Events

// XXX: The handle API would be much simpler if it return a single event, an
// Event, which embeds a terminationEvent if it wants to terminate the routine.

type scFinishedEv struct {
	priorityNormal
}

type scPrunePeerEv struct {
	priorityHigh
	peerID p2p.ID
	reason error
}

type scschedulerrFailure struct {
	priorityHigh
	peerID p2p.ID
	time   time.Time
	reason error
}

// XXX: unify with bcBlockResponse
type tmpBlockResponse struct {
	priorityNormal
	peerID p2p.ID
	time   time.Time
	height int64
	size   int64
	block  *types.Block
}

type scBlockRequest struct {
	priorityNormal
	peerID p2p.ID
	height int64
}

type scBlockReceived struct {
	priorityNormal
	peerID p2p.ID
	height int64
	size   int64
	block  *types.Block
}

type scPeerError struct {
	priorityHigh
	peerID p2p.ID
	reason error
}

// XXX: make this fetal?
type scSchedulerFail struct {
	priorityHigh
	reason error
}

type tryPrunePeer struct {
	priorityHigh
	time time.Time
}

type trySchedule struct {
	priorityHigh
	time time.Time
}

type scPeerPruned struct {
	priorityHigh
	peerID p2p.ID
}

type blockState int

const (
	blockStateUnknown blockState = iota
	blockStateNew
	blockStatePending
	blockStateReceived
	blockStateProcessed
)

func (e blockState) String() string {
	switch e {
	case blockStateUnknown:
		return "Unknown"
	case blockStateNew:
		return "New"
	case blockStatePending:
		return "Pending"
	case blockStateReceived:
		return "Received"
	case blockStateProcessed:
		return "Processed"
	default:
		return fmt.Sprintf("unknown blockState: %d", e)
	}
}

type peerState int

const (
	peerStateNew = iota
	peerStateReady
	peerStateRemoved
)

func (e peerState) String() string {
	switch e {
	case peerStateNew:
		return "New"
	case peerStateReady:
		return "Ready"
	case peerStateRemoved:
		return "Removed"
	default:
		return fmt.Sprintf("unknown peerState: %d", e)
	}
}

type scPeer struct {
	peerID      p2p.ID
	state       peerState
	height      int64
	lastTouched time.Time
	lastRate    int64
}

func newScPeer(peerID p2p.ID) *scPeer {
	return &scPeer{
		peerID:      peerID,
		state:       peerStateNew,
		height:      -1,
		lastTouched: time.Time{},
	}
}

// The scheduler is a composite data structure which allows a schedulerr to keep
// track of which blocks have been schedulerd into which state.
type scheduler struct {
	initHeight int64
	// a list of blocks in which blockState
	blockStates map[int64]blockState

	// a map of peerID to scheduler specific peer struct `scPeer` used to keep
	// track of peer specific state
	peers map[p2p.ID]*scPeer

	// a map of heights to the peer we are waiting for a response from
	pendingBlocks map[int64]p2p.ID

	// the time at which a block was put in blockStatePending
	pendingTime map[int64]time.Time

	// the peerID of the peer which put the block in blockStateReceived
	receivedBlocks map[int64]p2p.ID

	targetPending  uint32 // the number of blocks we want in blockStatePending
	targetReceived uint32 // the number of blocks we want in blockStateReceived
	minRecvRate    int64
	peerTimeout    time.Duration
}

func newScheduler(initHeight int64) *scheduler {
	sc := scheduler{
		initHeight:     initHeight,
		blockStates:    make(map[int64]blockState),
		peers:          make(map[p2p.ID]*scPeer),
		pendingBlocks:  make(map[int64]p2p.ID),
		pendingTime:    make(map[int64]time.Time),
		receivedBlocks: make(map[int64]p2p.ID),
	}

	sc.setStateAtHeight(initHeight, blockStateNew)

	return &sc
}

func (sc *scheduler) addPeer(peerID p2p.ID) error {
	if _, ok := sc.peers[peerID]; ok {
		return fmt.Errorf("Cannot add duplicate peer %s", peerID)
	}
	sc.peers[peerID] = newScPeer(peerID)
	return nil
}

func (sc *scheduler) touchPeer(peerID p2p.ID, time time.Time) error {
	peer, ok := sc.peers[peerID]
	if !ok {
		return fmt.Errorf("Couldn't find peer %s", peerID)
	}

	if peer.state == peerStateRemoved {
		return fmt.Errorf("Tried to touch peer in peerStateRemoved")
	}

	peer.lastTouched = time

	return nil
}

func (sc *scheduler) removePeer(peerID p2p.ID) error {
	peer, ok := sc.peers[peerID]
	if !ok {
		return fmt.Errorf("Couldn't find peer %s", peerID)
	}

	if peer.state == peerStateRemoved {
		return fmt.Errorf("Tried to remove peer %s in peerStateRemoved", peerID)
	}

	for height, pendingPeerID := range sc.pendingBlocks {
		if pendingPeerID == peerID {
			sc.setStateAtHeight(height, blockStateNew)
			delete(sc.pendingTime, height)
			delete(sc.pendingBlocks, height)
		}
	}

	for height, rcvPeerID := range sc.receivedBlocks {
		if rcvPeerID == peerID {
			sc.setStateAtHeight(height, blockStateNew)
			delete(sc.receivedBlocks, height)
		}
	}

	peer.state = peerStateRemoved

	return nil
}

func (sc *scheduler) setPeerHeight(peerID p2p.ID, height int64) error {
	peer, ok := sc.peers[peerID]
	if !ok {
		return fmt.Errorf("Can't find peer %s", peerID)
	}

	if peer.state == peerStateRemoved {
		return fmt.Errorf("Cannot set peer height for a peer in peerStateRemoved")
	}

	if height < peer.height {
		return fmt.Errorf("Cannot move peer height lower. from %d to %d", peer.height, height)
	}

	peer.height = height
	peer.state = peerStateReady
	for i := sc.minHeight(); i <= height; i++ {
		if sc.getStateAtHeight(i) == blockStateUnknown {
			sc.setStateAtHeight(i, blockStateNew)
		}
	}

	return nil
}

func (sc *scheduler) getStateAtHeight(height int64) blockState {
	if height < sc.initHeight {
		return blockStateProcessed
	} else if state, ok := sc.blockStates[height]; ok {
		return state
	} else {
		return blockStateUnknown
	}
}

func (sc *scheduler) getPeersAtHeight(height int64) []*scPeer {
	peers := []*scPeer{}
	for _, peer := range sc.peers {
		if peer.height >= height {
			peers = append(peers, peer)
		}
	}

	return peers
}

func (sc *scheduler) peersInactiveSince(duration time.Duration, now time.Time) []p2p.ID {
	peers := []p2p.ID{}
	for _, peer := range sc.peers {
		if now.Sub(peer.lastTouched) > duration {
			peers = append(peers, peer.peerID)
		}
	}

	return peers
}

func (sc *scheduler) peersSlowerThan(minSpeed int64) []p2p.ID {
	peers := []p2p.ID{}
	for _, peer := range sc.peers {
		if peer.lastRate < minSpeed {
			peers = append(peers, peer.peerID)
		}
	}

	return peers
}

func (sc *scheduler) setStateAtHeight(height int64, state blockState) {
	sc.blockStates[height] = state
}

func (sc *scheduler) markReceived(peerID p2p.ID, height int64, size int64, now time.Time) error {
	peer, ok := sc.peers[peerID]
	if !ok {
		return fmt.Errorf("Can't find peer %s", peerID)
	}

	if peer.state == peerStateRemoved {
		return fmt.Errorf("Cannot receive blocks from removed peer %s", peerID)
	}

	if state := sc.getStateAtHeight(height); state != blockStatePending || sc.pendingBlocks[height] != peerID {
		return fmt.Errorf("Received block %d from peer %s without being requested", height, peerID)
	}

	pendingTime, ok := sc.pendingTime[height]
	if !ok || now.Sub(pendingTime) <= 0 {
		return fmt.Errorf("Clock error. Block %d received at %s but requested at %s",
			height, pendingTime, now)
	}

	peer.lastRate = size / int64(now.Sub(pendingTime).Seconds())

	sc.setStateAtHeight(height, blockStateReceived)
	delete(sc.pendingBlocks, height)
	delete(sc.pendingTime, height)

	sc.receivedBlocks[height] = peerID

	return nil
}

func (sc *scheduler) markPending(peerID p2p.ID, height int64, time time.Time) error {
	peer, ok := sc.peers[peerID]
	if !ok {
		return fmt.Errorf("Can't find peer %s", peerID)
	}

	state := sc.getStateAtHeight(height)
	if state != blockStateNew {
		return fmt.Errorf("Block %d should be in blockStateNew but was %s", height, state)
	}

	if peer.state != peerStateReady {
		return fmt.Errorf("Cannot scheduler %d from %s in %s", height, peerID, peer.state)
	}

	if height > peer.height {
		return fmt.Errorf("Cannot request height %d from peer %s who is at height %d",
			height, peerID, peer.height)
	}

	sc.setStateAtHeight(height, blockStatePending)
	sc.pendingBlocks[height] = peerID
	// XXX: to make this more accurate we can introduce a message from
	// the IO routine which indicates the time the request was put on the wire
	sc.pendingTime[height] = time

	return nil
}

func (sc *scheduler) markProcessed(height int64) error {
	state := sc.getStateAtHeight(height)
	if state != blockStateReceived {
		return fmt.Errorf("Can't mark height %d received from block state %s", height, state)
	}

	delete(sc.receivedBlocks, height)

	sc.setStateAtHeight(height, blockStateProcessed)

	return nil
}

// allBlockProcessed returns true if all blocks are in blockStateProcessed and
// determines if the scheduler has been completed
func (sc *scheduler) allBlocksProcessed() bool {
	for _, state := range sc.blockStates {
		if state != blockStateProcessed {
			return false
		}
	}
	return true
}

// highest block | state == blockStateNew
func (sc *scheduler) maxHeight() int64 {
	var max int64 = 0
	for height, state := range sc.blockStates {
		if state == blockStateNew && height > max {
			max = height
		}
	}

	return max
}

// lowest block | state == blockStateNew
func (sc *scheduler) minHeight() int64 {
	var min int64 = math.MaxInt64
	for height, state := range sc.blockStates {
		if state == blockStateNew && height < min {
			min = height
		}
	}

	return min
}

func (sc *scheduler) pendingFrom(peerID p2p.ID) []int64 {
	heights := []int64{}
	for height, pendingPeerID := range sc.pendingBlocks {
		if pendingPeerID == peerID {
			heights = append(heights, height)
		}
	}
	return heights
}

func (sc *scheduler) selectPeer(peers []*scPeer) *scPeer {
	// FIXME: properPeerSelector
	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)

	return peers[r.Intn(len(peers))]
}

// XXX: this duplicates the logic of peersInactiveSince and peersSlowerThan
func (sc *scheduler) prunablePeers(peerTimout time.Duration, minRecvRate int64, now time.Time) []p2p.ID {
	prunable := []p2p.ID{}
	for peerID, peer := range sc.peers {
		if now.Sub(peer.lastTouched) > peerTimout || peer.lastRate < minRecvRate {
			prunable = append(prunable, peerID)
		}
	}

	return prunable
}

func (sc *scheduler) numBlockInState(targetState blockState) uint32 {
	var num uint32 = 0
	for _, state := range sc.blockStates {
		if state == targetState {
			num++
		}
	}
	return num
}

// Handlers

// XXX: This handler will probably get the block first and then pass it on to the processor
func (sc *scheduler) handleBlockResponse(event tmpBlockResponse) (Event, error) {
	err := sc.touchPeer(event.peerID, event.time)
	if err != nil {
		return scPeerError{peerID: event.peerID, reason: err}, nil
	}

	err = sc.markReceived(event.peerID, event.block.Height, event.size, event.time)
	if err != nil {
		return scPeerError{peerID: event.peerID, reason: err}, nil
	}

	return scBlockReceived{peerID: event.peerID, block: event.block}, nil
}

func (sc *scheduler) handleBlockProcessed(event pcBlockProcessed) (Event, error) {
	err := sc.markProcessed(event.height)
	if err != nil {
		return scSchedulerFail{reason: err}, nil // this should be fatal
	}

	if sc.allBlocksProcessed() {
		return scFinishedEv{}, nil
	}

	return noOp, nil
}

// XXX: unify types peerError
func (sc *scheduler) handlePeerError(event peerError) (Event, error) {
	err := sc.removePeer(event.peerID)
	if err != nil {
		return scSchedulerFail{reason: err}, nil // xxx: bit extreme
	}

	return noOp, nil
}

func (sc *scheduler) handleTryPrunePeer(event tryPrunePeer) (Event, error) {
	prunablePeers := sc.prunablePeers(sc.peerTimeout, sc.minRecvRate, event.time)

	if len(prunablePeers) > 0 {
		peerID := prunablePeers[0]
		err := sc.removePeer(peerID)
		if err != nil {
			return scSchedulerFail{reason: err}, nil // xxx: should be fatal
		}
		return scPeerPruned{peerID: peerID}, nil
	}
	return noOp, nil
}

func (sc *scheduler) handleTrySchedule(event trySchedule) (Event, error) {
	pendingBlocks := sc.numBlockInState(blockStatePending)
	receivedBlocks := sc.numBlockInState(blockStateReceived)
	todo := math.Min(float64(sc.targetPending-pendingBlocks), float64(sc.targetReceived-receivedBlocks))
	if todo > 0 {
		height := sc.minHeight()

		if sc.getStateAtHeight(height) == blockStateNew {
			allPeers := sc.getPeersAtHeight(height)
			bestPeer := sc.selectPeer(allPeers) // XXX: maybe this should return a p2p.ID
			err := sc.markPending(bestPeer.peerID, height, event.time)
			if err != nil {
				return scSchedulerFail{reason: err}, nil // XXX: peerError might be more appropriate
			}
			return scBlockRequest{peerID: bestPeer.peerID, height: height}, nil
		}
	}
	return noOp, nil
}

type bcStatusResponse struct {
	priorityNormal
	peerID p2p.ID
	height int64
}

func (sc *scheduler) handleStatusResponse(event bcStatusResponse) (Event, error) {
	err := sc.setPeerHeight(event.peerID, event.height)
	if err != nil {
		return scPeerError{peerID: event.peerID, reason: err}, nil
	}

	return noOp, nil
}

func (sc *scheduler) handle(event Event) (Event, error) {
	switch event := event.(type) {
	case bcStatusResponse:
		nextEvent, err := sc.handleStatusResponse(event)
		return nextEvent, err
	case trySchedule:
		nextEvent, err := sc.handleTrySchedule(event)
		return nextEvent, err
	case tryPrunePeer:
		nextEvent, err := sc.handleTryPrunePeer(event)
		return nextEvent, err
	case peerError:
		nextEvent, err := sc.handlePeerError(event)
		return nextEvent, err
	case tmpBlockResponse:
		nextEvent, err := sc.handleBlockResponse(event)
		return nextEvent, err
	}
	return noOp, nil
}
