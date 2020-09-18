package storage

import (
	"context"
	"sync"
	"testing"

	tutils "github.com/filecoin-project/specs-actors/support/testing"

	"github.com/filecoin-project/go-state-types/crypto"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
)

var dummyCid cid.Cid

func init() {
	dummyCid, _ = cid.Parse("bafkqaaa")
}

type proveRes struct {
	posts []miner.SubmitWindowedPoStParams
	err   error
}

type mockAPI struct {
	ch            *changeHandler
	periodStart   abi.ChainEpoch
	deadlineIdx   uint64
	proveResult   chan *proveRes
	submitResult  chan error
	onStateChange chan struct{}

	tsLock sync.RWMutex
	ts     *types.TipSet

	abortCalledLock sync.RWMutex
	abortCalled     bool
}

func newMockAPI() *mockAPI {
	return &mockAPI{
		proveResult:   make(chan *proveRes),
		onStateChange: make(chan struct{}),
		submitResult:  make(chan error),
		periodStart:   abi.ChainEpoch(0),
	}
}

func (m *mockAPI) makeTs(t *testing.T, h abi.ChainEpoch) *types.TipSet {
	m.tsLock.Lock()
	defer m.tsLock.Unlock()

	m.ts = makeTs(t, h)
	return m.ts
}

func makeTs(t *testing.T, h abi.ChainEpoch) *types.TipSet {
	var parents []cid.Cid
	msgcid := dummyCid

	a, _ := address.NewFromString("t00")
	b, _ := address.NewFromString("t02")
	var ts, err = types.NewTipSet([]*types.BlockHeader{
		{
			Height: h,
			Miner:  a,

			Parents: parents,

			Ticket: &types.Ticket{VRFProof: []byte{byte(h % 2)}},

			ParentStateRoot:       dummyCid,
			Messages:              msgcid,
			ParentMessageReceipts: dummyCid,

			BlockSig:     &crypto.Signature{Type: crypto.SigTypeBLS},
			BLSAggregate: &crypto.Signature{Type: crypto.SigTypeBLS},
		},
		{
			Height: h,
			Miner:  b,

			Parents: parents,

			Ticket: &types.Ticket{VRFProof: []byte{byte((h + 1) % 2)}},

			ParentStateRoot:       dummyCid,
			Messages:              msgcid,
			ParentMessageReceipts: dummyCid,

			BlockSig:     &crypto.Signature{Type: crypto.SigTypeBLS},
			BLSAggregate: &crypto.Signature{Type: crypto.SigTypeBLS},
		},
	})

	require.NoError(t, err)

	return ts
}

func (m *mockAPI) setDeadlineParams(periodStart abi.ChainEpoch, deadlineIdx uint64) {
	m.periodStart = periodStart
	m.deadlineIdx = deadlineIdx
}

func (m *mockAPI) StateMinerProvingDeadline(ctx context.Context, address address.Address, key types.TipSetKey) (*dline.Info, error) {
	m.tsLock.RLock()
	defer m.tsLock.RUnlock()

	currentEpoch := m.ts.Height()
	deadline := miner.NewDeadlineInfo(m.periodStart, m.deadlineIdx, currentEpoch)
	return deadline, nil
}

func (m *mockAPI) startGeneratePoST(
	ctx context.Context,
	ts *types.TipSet,
	deadline *dline.Info,
	completeGeneratePoST CompleteGeneratePoSTCb,
) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer cancel()

		select {
		case psRes := <-m.proveResult:
			completeGeneratePoST(psRes.posts, psRes.err)
		case <-ctx.Done():
			completeGeneratePoST(nil, ctx.Err())
		}
	}()

	return cancel
}

func (m *mockAPI) startSubmitPoST(
	ctx context.Context,
	ts *types.TipSet,
	deadline *dline.Info,
	posts []miner.SubmitWindowedPoStParams,
	completeSubmitPoST CompleteSubmitPoSTCb,
) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer cancel()

		select {
		case err := <-m.submitResult:
			completeSubmitPoST(err)
		case <-ctx.Done():
			completeSubmitPoST(ctx.Err())
		}
	}()

	return cancel
}

func (m *mockAPI) onAbort(ts *types.TipSet, deadline *dline.Info) {
	m.abortCalledLock.Lock()
	defer m.abortCalledLock.Unlock()
	m.abortCalled = true
}

func (m *mockAPI) wasAbortCalled() bool {
	m.abortCalledLock.RLock()
	defer m.abortCalledLock.RUnlock()
	return m.abortCalled
}

func (m *mockAPI) failPost(err error, ts *types.TipSet, deadline *dline.Info) {
}

func (m *mockAPI) setChangeHandler(ch *changeHandler) {
	m.ch = ch
}

// TestChangeHandlerBasic verifies we can generate a proof and submit it
func TestChangeHandlerBasic(t *testing.T) {
	periodStart := abi.ChainEpoch(0)
	deadlineIdx := uint64(0)
	s := makeScaffolding(t, periodStart, deadlineIdx)
	mock := s.mock

	defer s.ch.shutdown()
	s.ch.start()

	// Trigger a head change
	go triggerHeadChange(t, s, abi.ChainEpoch(1))

	// Should start proving
	<-s.ch.proveHdlr.processedHeadChanges
	// Submitter doesn't have anything to do yet
	<-s.ch.submitHdlr.processedHeadChanges

	// Send a response to the call to generate proofs
	posts := []miner.SubmitWindowedPoStParams{{Deadline: deadlineIdx}}
	mock.proveResult <- &proveRes{posts: posts}

	// Should move to proving complete
	<-s.ch.proveHdlr.processedPostResults

	// Move to the correct height to submit the proof
	go func() {
		ts := mock.makeTs(t, 1+SubmitConfidence)
		err := s.ch.update(s.ctx, nil, ts)
		require.NoError(t, err)
	}()

	// Should move to submitting state
	<-s.ch.submitHdlr.processedHeadChanges

	// Send a response to the submit call
	mock.submitResult <- nil

	// Should move to the complete state
	<-s.ch.submitHdlr.processedSubmitResults
}

type smScaffolding struct {
	ctx         context.Context
	mock        *mockAPI
	ch          *changeHandler
	periodStart abi.ChainEpoch
	deadlineIdx uint64
}

func makeScaffolding(t *testing.T, periodStart abi.ChainEpoch, deadlineIdx uint64) *smScaffolding {
	ctx := context.Background()
	actor := tutils.NewActorAddr(t, "actor")
	mock := newMockAPI()
	ch := newChangeHandler(mock, actor)
	mock.setChangeHandler(ch)
	mock.setDeadlineParams(periodStart, deadlineIdx)

	phcs := make(chan *headChange)
	ch.proveHdlr.processedHeadChanges = phcs
	pprs := make(chan *postResult)
	ch.proveHdlr.processedPostResults = pprs

	shcs := make(chan *headChange)
	ch.submitHdlr.processedHeadChanges = shcs
	ssrs := make(chan *submitResult)
	ch.submitHdlr.processedSubmitResults = ssrs

	return &smScaffolding{
		ctx:         ctx,
		mock:        mock,
		ch:          ch,
		periodStart: periodStart,
		deadlineIdx: deadlineIdx,
	}
}

func triggerHeadChange(t *testing.T, s *smScaffolding, height abi.ChainEpoch) {
	ts := s.mock.makeTs(t, height)
	err := s.ch.update(s.ctx, nil, ts)
	require.NoError(t, err)
}
