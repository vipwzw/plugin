// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dpos

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/33cn/chain33/types"
	"reflect"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	dpostype "github.com/33cn/plugin/plugin/consensus/dpos/types"
	ttypes "github.com/33cn/plugin/plugin/consensus/dpos/types"
	dty "github.com/33cn/plugin/plugin/dapp/dposvote/types"
	"github.com/golang/protobuf/proto"
)

//-----------------------------------------------------------------------------
// Config

const (
	continueToVote = 0
	voteSuccess    = 1
	voteFail       = 2
)

// Errors define
var (
	ErrInvalidVoteSignature      = errors.New("Error invalid vote signature")
	ErrInvalidVoteReplySignature = errors.New("Error invalid vote reply signature")
	ErrInvalidNotifySignature    = errors.New("Error invalid notify signature")
)

//-----------------------------------------------------------------------------

var (
	msgQueueSize = 1000
)

// internally generated messages which may update the state
type timeoutInfo struct {
	Duration time.Duration `json:"duration"`
	State    int           `json:"state"`
}

func (ti *timeoutInfo) String() string {
	return fmt.Sprintf("%v", ti.Duration)
}

type vrfStatusInfo struct {
	Cycle int64

}

// ConsensusState handles execution of the consensus algorithm.
// It processes votes and proposals, and upon reaching agreement,
// commits blocks to the chain and executes them against the application.
// The internal state machine receives input from peers, the internal validator, and from a timer.
type ConsensusState struct {
	// config details
	client             *Client
	privValidator      ttypes.PrivValidator // for signing votes
	privValidatorIndex int

	// internal state
	mtx          sync.Mutex
	validatorMgr ValidatorMgr // State until height-1.

	// state changes may be triggered by msgs from peers,
	// msgs from ourself, or by timeouts
	peerMsgQueue     chan MsgInfo
	internalMsgQueue chan MsgInfo
	timeoutTicker    TimeoutTicker

	broadcastChannel chan<- MsgInfo
	ourID            ID
	started          uint32 // atomic
	stopped          uint32 // atomic
	Quit             chan struct{}

	//当前状态
	dposState State

	//所有选票，包括自己的和从网络中接收到的
	dposVotes []*dpostype.DPosVote

	//当前达成共识的选票
	currentVote *dpostype.VoteItem
	lastVote    *dpostype.VoteItem

	myVote     *dpostype.DPosVote
	lastMyVote *dpostype.DPosVote

	notify     *dpostype.DPosNotify
	lastNotify *dpostype.DPosNotify

	//所有选票，包括自己的和从网络中接收到的
	cachedVotes []*dpostype.DPosVote

	cachedNotify *dpostype.DPosNotify

	cycleBoundaryMap map[int64] *dty.DposCBInfo
}

// NewConsensusState returns a new ConsensusState.
func NewConsensusState(client *Client, valMgr ValidatorMgr) *ConsensusState {
	cs := &ConsensusState{
		client:           client,
		peerMsgQueue:     make(chan MsgInfo, msgQueueSize),
		internalMsgQueue: make(chan MsgInfo, msgQueueSize),
		timeoutTicker:    NewTimeoutTicker(),

		Quit:      make(chan struct{}),
		dposState: InitStateObj,
		dposVotes: nil,
		cycleBoundaryMap: make(map[int64] *dty.DposCBInfo),
	}

	cs.updateToValMgr(valMgr)

	return cs
}

// SetOurID method
func (cs *ConsensusState) SetOurID(id ID) {
	cs.ourID = id
}

// SetBroadcastChannel method
func (cs *ConsensusState) SetBroadcastChannel(broadcastChannel chan<- MsgInfo) {
	cs.broadcastChannel = broadcastChannel
}

// IsRunning method
func (cs *ConsensusState) IsRunning() bool {
	return atomic.LoadUint32(&cs.started) == 1 && atomic.LoadUint32(&cs.stopped) == 0
}

//----------------------------------------
// String returns a string.
func (cs *ConsensusState) String() string {
	// better not to access shared variables
	return fmt.Sprintf("ConsensusState") //(H:%v R:%v S:%v", cs.Height, cs.Round, cs.Step)
}

// GetValidatorMgr returns a copy of the ValidatorMgr.
func (cs *ConsensusState) GetValidatorMgr() ValidatorMgr {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	return cs.validatorMgr.Copy()
}

// GetValidators returns a copy of the current validators.
func (cs *ConsensusState) GetValidators() []*ttypes.Validator {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	return cs.validatorMgr.Validators.Copy().Validators
}

// SetPrivValidator sets the private validator account for signing votes.
func (cs *ConsensusState) SetPrivValidator(priv ttypes.PrivValidator, index int) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.privValidator = priv
	cs.privValidatorIndex = index
}

// SetTimeoutTicker sets the local timer. It may be useful to overwrite for testing.
func (cs *ConsensusState) SetTimeoutTicker(timeoutTicker TimeoutTicker) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.timeoutTicker = timeoutTicker
}

// Start It start first time starts the timeout receive routines.
func (cs *ConsensusState) Start() {
	if atomic.CompareAndSwapUint32(&cs.started, 0, 1) {
		if atomic.LoadUint32(&cs.stopped) == 1 {
			dposlog.Error("ConsensusState already stoped")
		}
		cs.timeoutTicker.Start()

		// now start the receiveRoutine
		go cs.receiveRoutine()

		// schedule the first round!
		cs.scheduleDPosTimeout(time.Second*3, InitStateType)
	}
}

// Stop timer and receive routine
func (cs *ConsensusState) Stop() {
	cs.timeoutTicker.Stop()
	cs.Quit <- struct{}{}
}

// Attempt to schedule a timeout (by sending timeoutInfo on the tickChan)
func (cs *ConsensusState) scheduleDPosTimeout(duration time.Duration, stateType int) {
	cs.timeoutTicker.ScheduleTimeout(timeoutInfo{Duration: duration, State: stateType})
}

// send a msg into the receiveRoutine regarding our own proposal, block part, or vote
func (cs *ConsensusState) sendInternalMessage(mi MsgInfo) {
	select {
	case cs.internalMsgQueue <- mi:
	default:
		// NOTE: using the go-routine means our votes can
		// be processed out of order.
		// TODO: use CList here for strict determinism and
		// attempt push to internalMsgQueue in receiveRoutine
		dposlog.Info("Internal msg queue is full. Using a go-routine")
		go func() { cs.internalMsgQueue <- mi }()
	}
}

// Updates ConsensusState and increments height to match that of state.
// The round becomes 0 and cs.Step becomes ttypes.RoundStepNewHeight.
func (cs *ConsensusState) updateToValMgr(valMgr ValidatorMgr) {
	cs.validatorMgr = valMgr
}

//-----------------------------------------
// the main go routines

// receiveRoutine handles messages which may cause state transitions.
// it's argument (n) is the number of messages to process before exiting - use 0 to run forever
// It keeps the RoundState and is the only thing that updates it.
// Updates (state transitions) happen on timeouts, complete proposals, and 2/3 majorities.
// ConsensusState must be locked before any internal state is updated.
func (cs *ConsensusState) receiveRoutine() {
	defer func() {
		if r := recover(); r != nil {
			dposlog.Error("CONSENSUS FAILURE!!!", "err", r, "stack", string(debug.Stack()))
		}
	}()

	for {
		var mi MsgInfo

		select {
		case mi = <-cs.peerMsgQueue:
			// handles proposals, block parts, votes
			// may generate internal events (votes, complete proposals, 2/3 majorities)
			cs.handleMsg(mi)
		case mi = <-cs.internalMsgQueue:
			// handles proposals, block parts, votes
			cs.handleMsg(mi)
		case ti := <-cs.timeoutTicker.Chan(): // tockChan:
			// if the timeout is relevant to the rs
			// go to the next step
			cs.handleTimeout(ti)
		case <-cs.Quit:
			dposlog.Info("ConsensusState recv quit signal.")
			return
		}
	}
}

// state transitions on complete-proposal, 2/3-any, 2/3-one
func (cs *ConsensusState) handleMsg(mi MsgInfo) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	var err error
	msg, peerID, peerIP := mi.Msg, string(mi.PeerID), mi.PeerIP
	switch msg := msg.(type) {
	case *dpostype.DPosVote:
		cs.dposState.recvVote(cs, msg)
	case *dpostype.DPosNotify:
		cs.dposState.recvNotify(cs, msg)
	case *dpostype.DPosVoteReply:
		cs.dposState.recvVoteReply(cs, msg)
	case *dty.DposCBInfo:
		cs.dposState.recvCBInfo(cs, msg)
	default:
		dposlog.Error("Unknown msg type", "msg", msg.String(), "peerid", peerID, "peerip", peerIP)
	}
	if err != nil {
		dposlog.Error("Error with msg", "type", reflect.TypeOf(msg), "peerid", peerID, "peerip", peerIP, "err", err, "msg", msg)
	}
}

func (cs *ConsensusState) handleTimeout(ti timeoutInfo) {
	dposlog.Debug("Received tock", "timeout", ti.Duration, "state", StateTypeMapping[ti.State])

	// the timeout will now cause a state transition
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	//由具体的状态来处理超时消息
	cs.dposState.timeOut(cs)
}

// IsProposer method
func (cs *ConsensusState) IsProposer() bool {
	if cs.currentVote != nil {
		return bytes.Equal(cs.currentVote.VotedNodeAddress, cs.privValidator.GetAddress())
	}

	return false
}

// SetState method
func (cs *ConsensusState) SetState(state State) {
	cs.dposState = state
}

// SaveVote method
func (cs *ConsensusState) SaveVote() {
	if cs.lastVote == nil {
		cs.lastVote = cs.currentVote
	} else if cs.currentVote != nil && !bytes.Equal(cs.currentVote.VoteID, cs.lastVote.VoteID) {
		cs.lastVote = cs.currentVote
	}
}

// SetCurrentVote method
func (cs *ConsensusState) SetCurrentVote(vote *dpostype.VoteItem) {
	cs.currentVote = vote
}

// SaveMyVote method
func (cs *ConsensusState) SaveMyVote() {
	if cs.lastMyVote == nil {
		cs.lastMyVote = cs.myVote
	} else if cs.myVote != nil && !bytes.Equal(cs.myVote.Signature, cs.lastMyVote.Signature) {
		cs.lastMyVote = cs.myVote
	}
}

// SetMyVote method
func (cs *ConsensusState) SetMyVote(vote *dpostype.DPosVote) {
	cs.myVote = vote
}

// SaveNotify method
func (cs *ConsensusState) SaveNotify() {
	if cs.lastNotify == nil {
		cs.lastNotify = cs.notify
	} else if cs.notify != nil && !bytes.Equal(cs.notify.Signature, cs.lastNotify.Signature) {
		cs.lastNotify = cs.notify
	}
}

// SetNotify method
func (cs *ConsensusState) SetNotify(notify *dpostype.DPosNotify) {
	if cs.notify != nil && !bytes.Equal(cs.notify.Signature, notify.Signature) {
		cs.lastNotify = cs.notify
	}

	cs.notify = notify
}

// CacheNotify method
func (cs *ConsensusState) CacheNotify(notify *dpostype.DPosNotify) {
	cs.cachedNotify = notify
}

// ClearCachedNotify method
func (cs *ConsensusState) ClearCachedNotify() {
	cs.cachedNotify = nil
}

// AddVotes method
func (cs *ConsensusState) AddVotes(vote *dpostype.DPosVote) {
	repeatFlag := false
	addrExistFlag := false
	index := -1

	if cs.lastVote != nil && vote.VoteItem.PeriodStart < cs.lastVote.PeriodStop {
		dposlog.Info("Old vote, discard it", "vote.PeriodStart", vote.VoteItem.PeriodStart, "last vote.PeriodStop", cs.lastVote.PeriodStop)

		return
	}

	for i := 0; i < len(cs.dposVotes); i++ {
		if bytes.Equal(cs.dposVotes[i].Signature, vote.Signature) {
			repeatFlag = true
			break
		} else if bytes.Equal(cs.dposVotes[i].VoterNodeAddress, vote.VoterNodeAddress) {
			addrExistFlag = true
			index = i
			break
		}
	}

	//有重复投票，则不需要处理
	if repeatFlag {
		return
	}

	//投票不重复，如果地址也不重复，则直接加入;如果地址重复了，则替换老的投票
	if !addrExistFlag {
		cs.dposVotes = append(cs.dposVotes, vote)
	} else if vote.VoteTimestamp > cs.dposVotes[index].VoteTimestamp {
		cs.dposVotes[index] = vote
	}
}

// CacheVotes method
func (cs *ConsensusState) CacheVotes(vote *dpostype.DPosVote) {
	repeatFlag := false
	addrExistFlag := false
	index := -1

	for i := 0; i < len(cs.cachedVotes); i++ {
		if bytes.Equal(cs.cachedVotes[i].Signature, vote.Signature) {
			repeatFlag = true
			break
		} else if bytes.Equal(cs.cachedVotes[i].VoterNodeAddress, vote.VoterNodeAddress) {
			addrExistFlag = true
			index = i
			break
		}
	}

	//有重复投票，则不需要处理
	if repeatFlag {
		return
	}

	//投票不重复，如果地址也不重复，则直接加入;如果地址重复了，则替换老的投票
	if !addrExistFlag {
		cs.cachedVotes = append(cs.cachedVotes, vote)
	} else if vote.VoteTimestamp > cs.cachedVotes[index].VoteTimestamp {
		/*
			if index == len(cs.cachedVotes) - 1 {
				cs.cachedVotes = append(cs.cachedVotes, vote)
			}else {
				cs.cachedVotes = append(cs.cachedVotes[:index], cs.dposVotes[(index + 1):]...)
				cs.cachedVotes = append(cs.cachedVotes, vote)
			}
		*/
		cs.cachedVotes[index] = vote
	}
}

// CheckVotes method
func (cs *ConsensusState) CheckVotes() (ty int, vote *dpostype.VoteItem) {
	major32 := int(dposDelegateNum * 2 / 3)

	//总的票数还不够2/3，先不做决定
	if len(cs.dposVotes) < major32 {
		return continueToVote, nil
	}

	voteStat := map[string]int{}
	for i := 0; i < len(cs.dposVotes); i++ {
		key := string(cs.dposVotes[i].VoteItem.VoteID)
		if _, ok := voteStat[key]; ok {
			voteStat[key]++
		} else {
			voteStat[key] = 1
		}
	}

	key := ""
	value := 0

	for k, v := range voteStat {
		if v > value {
			value = v
			key = k
		}
	}

	//如果一个节点的投票数已经过2/3，则返回最终票数超过2/3的选票
	if value >= major32 {
		for i := 0; i < len(cs.dposVotes); i++ {
			if key == string(cs.dposVotes[i].VoteItem.VoteID) {
				return voteSuccess, cs.dposVotes[i].VoteItem
			}
		}
	} else if (value + (int(dposDelegateNum) - len(cs.dposVotes))) < major32 {
		//得票最多的节点，即使后续所有票都选它，也不满足2/3多数，不能达成共识。
		return voteFail, nil
	}

	return continueToVote, nil
}

// ClearVotes method
func (cs *ConsensusState) ClearVotes() {
	cs.dposVotes = nil
	cs.currentVote = nil
	cs.myVote = nil
}

// ClearCachedVotes method
func (cs *ConsensusState) ClearCachedVotes() {
	cs.cachedVotes = nil
}

// VerifyVote method
func (cs *ConsensusState) VerifyVote(vote *dpostype.DPosVote) bool {
	// Check validator
	index, val := cs.validatorMgr.Validators.GetByAddress(vote.VoterNodeAddress)
	if index == -1 && val == nil {
		dposlog.Info("The voter is not a legal validator, so discard this vote", "vote", vote.String())
		return false
	}
	// Verify signature
	pubkey, err := dpostype.ConsensusCrypto.PubKeyFromBytes(val.PubKey)
	if err != nil {
		dposlog.Error("Error pubkey from bytes", "err", err)
		return false
	}

	voteTmp := &dpostype.Vote{DPosVote: vote}
	if err := voteTmp.Verify(cs.validatorMgr.ChainID, pubkey); err != nil {
		dposlog.Error("Verify vote signature failed", "err", err)
		return false
	}

	return true
}

// VerifyNotify method
func (cs *ConsensusState) VerifyNotify(notify *dpostype.DPosNotify) bool {
	// Check validator
	index, val := cs.validatorMgr.Validators.GetByAddress(notify.NotifyNodeAddress)
	if index == -1 && val == nil {
		dposlog.Info("The notifier is not a legal validator, so discard this notify", "notify", notify.String())
		return false
	}
	// Verify signature
	pubkey, err := dpostype.ConsensusCrypto.PubKeyFromBytes(val.PubKey)
	if err != nil {
		dposlog.Error("Error pubkey from bytes", "err", err)
		return false
	}

	notifyTmp := &dpostype.Notify{DPosNotify: notify}
	if err := notifyTmp.Verify(cs.validatorMgr.ChainID, pubkey); err != nil {
		dposlog.Error("Verify vote signature failed", "err", err)
		return false
	}

	return true
}

// QueryCycleBoundaryInfo method
func (cs *ConsensusState) QueryCycleBoundaryInfo(cycle int64)(*dty.DposCBInfo, error){
	req := &dty.DposCBQuery{Cycle: cycle, Ty: dty.QueryCBInfoByCycle}
	param, err := proto.Marshal(req)
	if err != nil {
		dposlog.Error("Marshal DposCBQuery failed", "err", err)
		return nil, err
	}
	msg := cs.client.GetQueueClient().NewMessage("execs", types.EventBlockChainQuery,
		&types.ChainExecutor{
			Driver: dty.DPosX,
			FuncName: dty.FuncNameQueryCBInfoByCycle,
			StateHash: zeroHash[:],
			Param:param,
		})

	err = cs.client.GetQueueClient().Send(msg, true)
	if err != nil {
		dposlog.Error("send DposCBQuery to dpos exec failed", "err", err)
		return nil, err
	}

	msg, err = cs.client.GetQueueClient().Wait(msg)
	if err != nil {
		dposlog.Error("send DposCBQuery wait failed", "err", err)
		return nil, err
	}

	return msg.GetData().(types.Message).(*dty.DposCBInfo), nil
}

// InitCycleBoundaryInfo method
func (cs *ConsensusState) InitCycleBoundaryInfo(){
	now := time.Now().Unix()
	task := DecideTaskByTime(now)

	info, err := cs.QueryCycleBoundaryInfo(task.cycle)
	if err == nil && info != nil {
		//cs.cycleBoundaryMap[task.cycle] = info
		cs.UpdateCBInfo(info)
		return
	}

	info, err = cs.QueryCycleBoundaryInfo(task.cycle - 1)
	if err == nil && info != nil {
		//cs.cycleBoundaryMap[task.cycle] = info
		cs.UpdateCBInfo(info)
	}

	return
}

func (cs *ConsensusState) UpdateCBInfo(info *dty.DposCBInfo) {
	valueNumber := len(cs.cycleBoundaryMap)
	if valueNumber == 0 {
		cs.cycleBoundaryMap[info.Cycle] = info
		return
	}

	oldestCycle := int64(0)
	for k, _ := range cs.cycleBoundaryMap {
		if k == info.Cycle {
			cs.cycleBoundaryMap[info.Cycle] = info
			return
		} else {
			if oldestCycle == 0 {
				oldestCycle = k
			} else if oldestCycle > k {
				oldestCycle = k
			}
		}
	}

	if valueNumber >= 5 {
		delete(cs.cycleBoundaryMap, oldestCycle)
	}

	cs.cycleBoundaryMap[info.Cycle] = info
}

func (cs *ConsensusState) GetCBInfoByCircle(cycle int64) (info *dty.DposCBInfo) {
	if v, ok := cs.cycleBoundaryMap[cycle];ok {
		info = v
		return info
	}

	return nil
}

// VerifyNotify method
func (cs *ConsensusState) VerifyCBInfo(info *dty.DposCBInfo) bool {
	// Verify signature
	bPubkey, err := hex.DecodeString(info.Pubkey)
	if err != nil {
		return false
	}
	pubkey, err := dpostype.ConsensusCrypto.PubKeyFromBytes(bPubkey)
	if err != nil {
		dposlog.Error("Error pubkey from bytes", "err", err)
		return false
	}

	bSig, err := hex.DecodeString(info.Signature)
	if err != nil {
		dposlog.Error("Error signature from bytes", "err", err)
		return false
	}

	sig, err := ttypes.ConsensusCrypto.SignatureFromBytes(bSig)
	if err != nil {
		dposlog.Error("CBInfo Verify failed", "err", err)
		return false
	}

	buf := new(bytes.Buffer)

	canonical := dty.CanonicalOnceCBInfo{
		Cycle: info.Cycle,
		StopHeight: info.StopHeight,
		StopHash: info.StopHash,
		Pubkey: info.Pubkey,
	}

	byteCB, err := json.Marshal(&canonical)
	if err != nil {
		dposlog.Error("Error Marshal failed: ", "err", err)
		return false
	}

	_, err = buf.Write(byteCB)
	if err != nil {
		dposlog.Error("Error buf.Write failed: ", "err", err)
		return false
	}

	if !pubkey.VerifyBytes(buf.Bytes(), sig) {
		dposlog.Error("Error Verify Bytes failed: ", "err", err)
		return false
	}

	return true
}

func (cs *ConsensusState) SendCBTx(info *dty.DposCBInfo) bool {
	err := cs.privValidator.SignCBInfo(info)
	if err != nil {
		dposlog.Error("SignCBInfo failed.", "err", err)
		return false
	} else {
		tx, err := cs.client.CreateRecordCBTx(info)
		if err != nil {
			dposlog.Error("CreateRecordCBTx failed.", "err", err)
			return false
		} else {
			cs.privValidator.SignTx(tx)
			dposlog.Info("Sign RecordCBTx.")
			//将交易发往交易池中，方便后续重启或者新加入的超级节点查询
			msg := cs.client.GetQueueClient().NewMessage("mempool", types.EventTx, tx)
			err = cs.client.GetQueueClient().Send(msg, false)
			if err != nil {
				dposlog.Error("Send RecordCBTx to mempool failed.", "err", err)
				return false
			} else {
				dposlog.Error("Send RecordCBTx to mempool ok.", "err", err)
			}
		}
	}

	return true
}

func (cs *ConsensusState) SendRegistVrfMTx(info *dty.DposVrfMRegist) bool {
	tx, err := cs.client.CreateRegVrfMTx(info)
	if err != nil {
		dposlog.Error("CreateRegVrfMTx failed.", "err", err)
		return false
	} else {
		cs.privValidator.SignTx(tx)
		dposlog.Info("Sign RegistVrfMTx.")
		//将交易发往交易池中，方便后续重启或者新加入的超级节点查询
		msg := cs.client.GetQueueClient().NewMessage("mempool", types.EventTx, tx)
		err = cs.client.GetQueueClient().Send(msg, false)
		if err != nil {
			dposlog.Error("Send RegistVrfMTx to mempool failed.", "err", err)
			return false
		} else {
			dposlog.Error("Send RegistVrfMTx to mempool ok.", "err", err)
		}
	}

	return true
}

func (cs *ConsensusState) SendRegistVrfRPTx(info *dty.DposVrfRPRegist) bool {
	tx, err := cs.client.CreateRegVrfRPTx(info)
	if err != nil {
		dposlog.Error("CreateRegVrfRPTx failed.", "err", err)
		return false
	} else {
		cs.privValidator.SignTx(tx)
		dposlog.Info("Sign RegVrfRPTx.")
		//将交易发往交易池中，方便后续重启或者新加入的超级节点查询
		msg := cs.client.GetQueueClient().NewMessage("mempool", types.EventTx, tx)
		err = cs.client.GetQueueClient().Send(msg, false)
		if err != nil {
			dposlog.Error("Send RegVrfRPTx to mempool failed.", "err", err)
			return false
		} else {
			dposlog.Error("Send RegVrfRPTx to mempool ok.", "err", err)
		}
	}

	return true
}

func (cs *ConsensusState) QueryVrf(info *dty.DposCBInfo) bool {
