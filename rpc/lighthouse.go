package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	gtypes "github.com/ethereum/go-ethereum/core/types"

	lru "github.com/hashicorp/golang-lru"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/sirupsen/logrus"
)

// LighthouseClient holds the Lighthouse client info
type LighthouseClient struct {
	endpoint            string
	assignmentsCache    *lru.Cache
	assignmentsCacheMux *sync.Mutex
	signer              gtypes.Signer
}

// NewLighthouseClient is used to create a new Lighthouse client
func NewLighthouseClient(endpoint string, chainID *big.Int) (*LighthouseClient, error) {
	signer := gtypes.NewLondonSigner(chainID)
	client := &LighthouseClient{
		endpoint:            endpoint,
		assignmentsCacheMux: &sync.Mutex{},
		signer:              signer,
	}
	client.assignmentsCache, _ = lru.New(10)

	return client, nil
}

func (lc *LighthouseClient) GetNewBlockChan() chan *types.Block {
	blkCh := make(chan *types.Block, 10)
	go func() {
		// poll sync status 2 times per slot
		t := time.NewTicker(time.Second * time.Duration(utils.Config.Chain.Config.SecondsPerSlot) / 2)

		lastHeadSlot := uint64(0)
		headResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/headers/head", lc.endpoint))
		if err == nil {
			var parsedHead StandardBeaconHeaderResponse
			err = json.Unmarshal(headResp, &parsedHead)
			if err != nil {
				logger.Warnf("failed to decode head, starting blocks channel at slot 0")
			} else {
				lastHeadSlot = uint64(parsedHead.Data.Header.Message.Slot)
			}
		} else {
			logger.Warnf("failed to fetch head, starting blocks channel at slot 0")
		}

		for {
			<-t.C
			syncingResp, err := lc.get(fmt.Sprintf("%s/eth/v1/node/syncing", lc.endpoint))
			if err != nil {
				logger.Warnf("failed to retrieve syncing status: %v", err)
				continue
			}
			var parsedSyncing StandardSyncingResponse
			err = json.Unmarshal(syncingResp, &parsedSyncing)
			if err != nil {
				logger.Warnf("failed to decode syncing status: %v", err)
				continue
			}
			for slot := lastHeadSlot; slot < uint64(parsedSyncing.Data.HeadSlot); slot++ {
				blks, err := lc.GetBlocksBySlot(slot)
				if err != nil {
					logger.Warnf("failed to fetch block(s) for slot %d: %v", slot, err)
					continue
				}
				for _, blk := range blks {
					blkCh <- blk
				}
				lastHeadSlot = slot
			}
		}
	}()
	return blkCh
}

// GetChainHead gets the chain head from Lighthouse
func (lc *LighthouseClient) GetChainHead() (*types.ChainHead, error) {
	headResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/headers/head", lc.endpoint))
	if err != nil {
		return nil, fmt.Errorf("error retrieving chain head: %v", err)
	}

	var parsedHead StandardBeaconHeaderResponse
	err = json.Unmarshal(headResp, &parsedHead)
	if err != nil {
		return nil, fmt.Errorf("error parsing chain head: %v", err)
	}

	id := parsedHead.Data.Header.Message.StateRoot
	if parsedHead.Data.Header.Message.Slot == 0 {
		id = "genesis"
	}
	finalityResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/finality_checkpoints", lc.endpoint, id))
	if err != nil {
		return nil, fmt.Errorf("error retrieving finality checkpoints of head: %v", err)
	}

	var parsedFinality StandardFinalityCheckpointsResponse
	err = json.Unmarshal(finalityResp, &parsedFinality)
	if err != nil {
		return nil, fmt.Errorf("error parsing finality checkpoints of head: %v", err)
	}

	return &types.ChainHead{
		HeadSlot:                   uint64(parsedHead.Data.Header.Message.Slot),
		HeadEpoch:                  uint64(parsedHead.Data.Header.Message.Slot) / utils.Config.Chain.Config.SlotsPerEpoch,
		HeadBlockRoot:              utils.MustParseHex(parsedHead.Data.Root),
		FinalizedSlot:              uint64(parsedFinality.Data.Finalized.Epoch) * utils.Config.Chain.Config.SlotsPerEpoch,
		FinalizedEpoch:             uint64(parsedFinality.Data.Finalized.Epoch),
		FinalizedBlockRoot:         utils.MustParseHex(parsedFinality.Data.Finalized.Root),
		JustifiedSlot:              uint64(parsedFinality.Data.CurrentJustified.Epoch) * utils.Config.Chain.Config.SlotsPerEpoch,
		JustifiedEpoch:             uint64(parsedFinality.Data.CurrentJustified.Epoch),
		JustifiedBlockRoot:         utils.MustParseHex(parsedFinality.Data.CurrentJustified.Root),
		PreviousJustifiedSlot:      uint64(parsedFinality.Data.PreviousJustified.Epoch) * utils.Config.Chain.Config.SlotsPerEpoch,
		PreviousJustifiedEpoch:     uint64(parsedFinality.Data.PreviousJustified.Epoch),
		PreviousJustifiedBlockRoot: utils.MustParseHex(parsedFinality.Data.PreviousJustified.Root),
	}, nil
}

func (lc *LighthouseClient) GetValidatorQueue() (*types.ValidatorQueue, error) {
	// pre-filter the status, to return much less validators, thus much faster!
	validatorsResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/head/validators?status=pending_queued,exited", lc.endpoint))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validator for head valiqdator queue check: %v", err)
	}

	var parsedValidators StandardValidatorsResponse
	err = json.Unmarshal(validatorsResp, &parsedValidators)
	if err != nil {
		return nil, fmt.Errorf("error parsing queue validators: %v", err)
	}
	// TODO: maybe track more status counts in the future?
	activatingValidatorCount := uint64(0)
	exitingValidatorCount := uint64(0)
	for _, validator := range parsedValidators.Data {
		switch validator.Status {
		case "pending_initialized":
			break
		case "pending_queued":
			activatingValidatorCount += 1
			break
		case "active_ongoing", "active_exiting", "active_slashed":
			break
		case "exited_unslashed", "exited_slashed":
			exitingValidatorCount += exitingValidatorCount
			break
		case "withdrawal_possible", "withdrawal_done":
			break
		default:
			return nil, fmt.Errorf("unrecognized validator status (validator %d): %s", validator.Index, validator.Status)
		}
	}
	return &types.ValidatorQueue{
		Activating: activatingValidatorCount,
		Exititing:  exitingValidatorCount,
	}, nil
}

// GetEpochAssignments will get the epoch assignments from Lighthouse RPC api
func (lc *LighthouseClient) GetEpochAssignments(epoch uint64) (*types.EpochAssignments, error) {
	lc.assignmentsCacheMux.Lock()
	defer lc.assignmentsCacheMux.Unlock()

	var err error

	cachedValue, found := lc.assignmentsCache.Get(epoch)
	if found {
		return cachedValue.(*types.EpochAssignments), nil
	}

	proposerResp, err := lc.get(fmt.Sprintf("%s/eth/v1/validator/duties/proposer/%d", lc.endpoint, epoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving proposer duties: %v", err)
	}
	var parsedProposerResponse StandardProposerDutiesResponse
	err = json.Unmarshal(proposerResp, &parsedProposerResponse)
	if err != nil {
		return nil, fmt.Errorf("error parsing proposer duties: %v", err)
	}

	// fetch the block root that the proposer data is dependent on
	headerResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/headers/%s", lc.endpoint, parsedProposerResponse.DependentRoot))
	if err != nil {
		return nil, fmt.Errorf("error retrieving chain header: %v", err)
	}
	var parsedHeader StandardBeaconHeaderResponse
	err = json.Unmarshal(headerResp, &parsedHeader)
	if err != nil {
		return nil, fmt.Errorf("error parsing chain header: %v", err)
	}
	depStateRoot := parsedHeader.Data.Header.Message.StateRoot

	// Now use the state root to make a consistent committee query
	committeesResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/committees?epoch=%d", lc.endpoint, depStateRoot, epoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving committees data: %w", err)
	}
	var parsedCommittees StandardCommitteesResponse
	err = json.Unmarshal(committeesResp, &parsedCommittees)
	if err != nil {
		return nil, fmt.Errorf("error parsing committees data: %w", err)
	}

	assignments := &types.EpochAssignments{
		ProposerAssignments: make(map[uint64]uint64),
		AttestorAssignments: make(map[string]uint64),
	}

	// propose
	for _, duty := range parsedProposerResponse.Data {
		assignments.ProposerAssignments[uint64(duty.Slot)] = uint64(duty.ValidatorIndex)
	}

	// attest
	for _, committee := range parsedCommittees.Data {
		for i, valIndex := range committee.Validators {
			valIndexU64, err := strconv.ParseUint(valIndex, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("epoch %d committee %d index %d has bad validator index %q", epoch, committee.Index, i, valIndex)
			}
			k := utils.FormatAttestorAssignmentKey(uint64(committee.Slot), uint64(committee.Index), uint64(i))
			assignments.AttestorAssignments[k] = valIndexU64
		}
	}

	if epoch >= utils.Config.Chain.Config.AltairForkEpoch {
		syncCommitteeState := depStateRoot
		if epoch == utils.Config.Chain.Config.AltairForkEpoch {
			syncCommitteeState = fmt.Sprintf("%d", utils.Config.Chain.Config.AltairForkEpoch*utils.Config.Chain.Config.SlotsPerEpoch)
		}
		parsedSyncCommittees, err := lc.GetSyncCommittee(syncCommitteeState, epoch)
		if err != nil {
			return nil, err
		}
		assignments.SyncAssignments = make([]uint64, len(parsedSyncCommittees.Validators))

		// sync
		for i, valIndexStr := range parsedSyncCommittees.Validators {
			valIndexU64, err := strconv.ParseUint(valIndexStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("in sync_committee for epoch %d validator %d has bad validator index: %q", epoch, i, valIndexStr)
			}
			assignments.SyncAssignments[i] = valIndexU64
		}
	}

	if len(assignments.AttestorAssignments) > 0 && len(assignments.ProposerAssignments) > 0 {
		lc.assignmentsCache.Add(epoch, assignments)
	}

	return assignments, nil
}

// GetEpochData will get the epoch data from Lighthouse RPC api
func (lc *LighthouseClient) GetEpochData(epoch uint64) (*types.EpochData, error) {
	wg := &sync.WaitGroup{}
	mux := &sync.Mutex{}
	var err error

	data := &types.EpochData{}
	data.Epoch = epoch

	validatorsResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validators", lc.endpoint, epoch*utils.Config.Chain.Config.SlotsPerEpoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validators for epoch %v: %v", epoch, err)
	}

	var parsedValidators StandardValidatorsResponse
	err = json.Unmarshal(validatorsResp, &parsedValidators)

	if err != nil {
		return nil, fmt.Errorf("error parsing epoch validators: %v", err)
	}

	epoch1d := int64(epoch) - 225
	epoch7d := int64(epoch) - 225*7
	epoch31d := int64(epoch) - 225*31

	var validatorBalances1d map[uint64]uint64
	var validatorBalances7d map[uint64]uint64
	var validatorBalances31d map[uint64]uint64

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances1d, err = lc.getBalancesForEpoch(epoch1d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for epoch %v (1d): %v", epoch1d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for epoch %v (1d) took %v", len(parsedValidators.Data), epoch1d, time.Since(start))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances7d, err = lc.getBalancesForEpoch(epoch7d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for epoch %v (7d): %v", epoch7d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for epoch %v (7d) took %v", len(parsedValidators.Data), epoch7d, time.Since(start))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances31d, err = lc.getBalancesForEpoch(epoch31d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for epoch %v (31d): %v", epoch31d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for epoch %v (31d) took %v", len(parsedValidators.Data), epoch31d, time.Since(start))
	}()
	wg.Wait()

	for _, validator := range parsedValidators.Data {
		data.Validators = append(data.Validators, &types.Validator{
			Index:                      uint64(validator.Index),
			PublicKey:                  utils.MustParseHex(validator.Validator.Pubkey),
			WithdrawalCredentials:      utils.MustParseHex(validator.Validator.WithdrawalCredentials),
			Balance:                    uint64(validator.Balance),
			EffectiveBalance:           uint64(validator.Validator.EffectiveBalance),
			Slashed:                    validator.Validator.Slashed,
			ActivationEligibilityEpoch: uint64(validator.Validator.ActivationEligibilityEpoch),
			ActivationEpoch:            uint64(validator.Validator.ActivationEpoch),
			ExitEpoch:                  uint64(validator.Validator.ExitEpoch),
			WithdrawableEpoch:          uint64(validator.Validator.WithdrawableEpoch),
			Balance1d:                  validatorBalances1d[uint64(validator.Index)],
			Balance7d:                  validatorBalances7d[uint64(validator.Index)],
			Balance31d:                 validatorBalances31d[uint64(validator.Index)],
			Status:                     validator.Status,
		})
	}

	logger.Printf("retrieved data for %v validators for epoch %v", len(data.Validators), epoch)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		data.ValidatorAssignmentes, err = lc.GetEpochAssignments(epoch)
		if err != nil {
			logrus.Errorf("error retrieving assignments for epoch %v: %v", epoch, err)
			return
		}
		logger.Printf("retrieved validator assignment data for epoch %v", epoch)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		data.EpochParticipationStats, err = lc.GetValidatorParticipation(epoch)
		if err != nil {
			logger.Errorf("error retrieving epoch participation statistics for epoch %v: %v", epoch, err)
			data.EpochParticipationStats = &types.ValidatorParticipation{
				Epoch:                   epoch,
				Finalized:               false,
				GlobalParticipationRate: 1.0,
				VotedEther:              0,
				EligibleEther:           0,
			}
		}
	}()

	// Retrieve all blocks for the epoch
	data.Blocks = make(map[uint64]map[string]*types.Block)

	for slot := epoch * utils.Config.Chain.Config.SlotsPerEpoch; slot <= (epoch+1)*utils.Config.Chain.Config.SlotsPerEpoch-1; slot++ {
		if slot == 0 || utils.SlotToTime(slot).After(time.Now()) { // Currently slot 0 returns all blocks, also skip asking for future blocks
			continue
		}
		wg.Add(1)
		go func(slot uint64) {
			defer wg.Done()
			blocks, err := lc.GetBlocksBySlot(slot)

			if err != nil {
				logger.Errorf("error retrieving blocks for slot %v: %v", slot, err)
				return
			}

			for _, block := range blocks {
				mux.Lock()
				if data.Blocks[block.Slot] == nil {
					data.Blocks[block.Slot] = make(map[string]*types.Block)
				}
				data.Blocks[block.Slot][fmt.Sprintf("%x", block.BlockRoot)] = block
				mux.Unlock()
			}
		}(slot)
	}
	wg.Wait()
	logger.Printf("retrieved %v blocks for epoch %v", len(data.Blocks), epoch)

	// Fill up missed and scheduled blocks
	for slot, proposer := range data.ValidatorAssignmentes.ProposerAssignments {
		_, found := data.Blocks[slot]
		if !found {
			// Proposer was assigned but did not yet propose a block
			data.Blocks[slot] = make(map[string]*types.Block)
			data.Blocks[slot]["0x0"] = &types.Block{
				Status:            0,
				Canonical:         true,
				Proposer:          proposer,
				BlockRoot:         []byte{0x0},
				Slot:              slot,
				ParentRoot:        []byte{},
				StateRoot:         []byte{},
				Signature:         []byte{},
				RandaoReveal:      []byte{},
				Graffiti:          []byte{},
				BodyRoot:          []byte{},
				Eth1Data:          &types.Eth1Data{},
				ProposerSlashings: make([]*types.ProposerSlashing, 0),
				AttesterSlashings: make([]*types.AttesterSlashing, 0),
				Attestations:      make([]*types.Attestation, 0),
				Deposits:          make([]*types.Deposit, 0),
				VoluntaryExits:    make([]*types.VoluntaryExit, 0),
				SyncAggregate:     nil,
			}

			if utils.SlotToTime(slot).After(time.Now().Add(time.Second * -60)) {
				// Block is in the future, set status to scheduled
				data.Blocks[slot]["0x0"].Status = 0
				data.Blocks[slot]["0x0"].BlockRoot = []byte{0x0}
			} else {
				// Block is in the past, set status to missed
				data.Blocks[slot]["0x0"].Status = 2
				data.Blocks[slot]["0x0"].BlockRoot = []byte{0x1}
			}
		}
	}

	return data, nil
}

func uint64List(li []uint64Str) []uint64 {
	out := make([]uint64, len(li), len(li))
	for i, v := range li {
		out[i] = uint64(v)
	}
	return out
}

func (lc *LighthouseClient) getBalancesForEpoch(epoch int64) (map[uint64]uint64, error) {

	if epoch < 0 {
		epoch = 0
	}

	var err error

	validatorBalances := make(map[uint64]uint64)

	resp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validator_balances", lc.endpoint, epoch*int64(utils.Config.Chain.Config.SlotsPerEpoch)))
	if err != nil {
		return validatorBalances, err
	}

	var parsedResponse StandardValidatorBalancesResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("error parsing response for validator_balances")
	}

	for _, b := range parsedResponse.Data {
		validatorBalances[uint64(b.Index)] = uint64(b.Balance)
	}

	return validatorBalances, nil
}

func (lc *LighthouseClient) GetBlockByBlockroot(blockroot []byte) (*types.Block, error) {
	resHeaders, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/headers/0x%x", lc.endpoint, blockroot))
	if err != nil {
		if err == notFoundErr {
			// no block found
			return &types.Block{}, nil
		}
		return nil, fmt.Errorf("error retrieving headers for blockroot 0x%x: %v", blockroot, err)
	}
	var parsedHeaders StandardBeaconHeaderResponse
	err = json.Unmarshal(resHeaders, &parsedHeaders)
	if err != nil {
		return nil, fmt.Errorf("error parsing header-response for blockroot 0x%x: %v", blockroot, err)
	}

	slot := uint64(parsedHeaders.Data.Header.Message.Slot)

	resp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/blocks/%s", lc.endpoint, parsedHeaders.Data.Root))
	if err != nil {
		return nil, fmt.Errorf("error retrieving block data at slot %v: %v", slot, err)
	}

	var parsedResponse StandardV2BlockResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		logger.Errorf("error parsing block data at slot %v: %v", parsedHeaders.Data.Header.Message.Slot, err)
		return nil, fmt.Errorf("error parsing block-response at slot %v: %v", slot, err)
	}

	return lc.blockFromResponse(&parsedHeaders, &parsedResponse)
}

// GetBlocksBySlot will get the blocks by slot from Lighthouse RPC api
func (lc *LighthouseClient) GetBlocksBySlot(slot uint64) ([]*types.Block, error) {
	resHeaders, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/headers/%d", lc.endpoint, slot))
	if err != nil {
		if err == notFoundErr {
			// no block found
			return []*types.Block{}, nil
		}
		return nil, fmt.Errorf("error retrieving headers at slot %v: %v", slot, err)
	}
	var parsedHeaders StandardBeaconHeaderResponse
	err = json.Unmarshal(resHeaders, &parsedHeaders)
	if err != nil {
		return nil, fmt.Errorf("error parsing header-response at slot %v: %v", slot, err)
	}

	resp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/blocks/%s", lc.endpoint, parsedHeaders.Data.Root))
	if err != nil {
		return nil, fmt.Errorf("error retrieving block data at slot %v: %v", slot, err)
	}

	var parsedResponse StandardV2BlockResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		logger.Errorf("error parsing block data at slot %v: %v", slot, err)
		return nil, fmt.Errorf("error parsing block-response at slot %v: %v", slot, err)
	}

	block, err := lc.blockFromResponse(&parsedHeaders, &parsedResponse)
	if err != nil {
		return nil, err
	}
	return []*types.Block{block}, nil
}

func (lc *LighthouseClient) blockFromResponse(parsedHeaders *StandardBeaconHeaderResponse, parsedResponse *StandardV2BlockResponse) (*types.Block, error) {
	parsedBlock := parsedResponse.Data
	slot := uint64(parsedHeaders.Data.Header.Message.Slot)
	block := &types.Block{
		Status:       1,
		Canonical:    parsedHeaders.Data.Canonical,
		Proposer:     uint64(parsedBlock.Message.ProposerIndex),
		BlockRoot:    utils.MustParseHex(parsedHeaders.Data.Root),
		Slot:         slot,
		ParentRoot:   utils.MustParseHex(parsedBlock.Message.ParentRoot),
		StateRoot:    utils.MustParseHex(parsedBlock.Message.StateRoot),
		Signature:    parsedBlock.Signature,
		RandaoReveal: utils.MustParseHex(parsedBlock.Message.Body.RandaoReveal),
		Graffiti:     utils.MustParseHex(parsedBlock.Message.Body.Graffiti),
		Eth1Data: &types.Eth1Data{
			DepositRoot:  utils.MustParseHex(parsedBlock.Message.Body.Eth1Data.DepositRoot),
			DepositCount: uint64(parsedBlock.Message.Body.Eth1Data.DepositCount),
			BlockHash:    utils.MustParseHex(parsedBlock.Message.Body.Eth1Data.BlockHash),
		},
		ProposerSlashings: make([]*types.ProposerSlashing, len(parsedBlock.Message.Body.ProposerSlashings)),
		AttesterSlashings: make([]*types.AttesterSlashing, len(parsedBlock.Message.Body.AttesterSlashings)),
		Attestations:      make([]*types.Attestation, len(parsedBlock.Message.Body.Attestations)),
		Deposits:          make([]*types.Deposit, len(parsedBlock.Message.Body.Deposits)),
		VoluntaryExits:    make([]*types.VoluntaryExit, len(parsedBlock.Message.Body.VoluntaryExits)),
	}

	epochAssignments, err := lc.GetEpochAssignments(slot / utils.Config.Chain.Config.SlotsPerEpoch)
	if err != nil {
		return nil, err
	}

	if agg := parsedBlock.Message.Body.SyncAggregate; agg != nil {
		bits := utils.MustParseHex(agg.SyncCommitteeBits)

		if utils.Config.Chain.Config.SyncCommitteeSize != uint64(len(bits)*8) {
			return nil, fmt.Errorf("sync-aggregate-bits-size does not match sync-committee-size: %v != %v", len(bits)*8, utils.Config.Chain.Config.SyncCommitteeSize)
		}

		block.SyncAggregate = &types.SyncAggregate{
			SyncCommitteeValidators:    epochAssignments.SyncAssignments,
			SyncCommitteeBits:          bits,
			SyncAggregateParticipation: syncCommitteeParticipation(bits),
			SyncCommitteeSignature:     utils.MustParseHex(agg.SyncCommitteeSignature),
		}
	}

	if payload := parsedBlock.Message.Body.ExecutionPayload; payload != nil && !bytes.Equal(payload.ParentHash, make([]byte, 32)) {
		txs := make([]*types.Transaction, 0, len(payload.Transactions))
		for i, rawTx := range payload.Transactions {
			tx := &types.Transaction{Raw: rawTx}
			var decTx gtypes.Transaction
			if err := decTx.UnmarshalBinary(rawTx); err != nil {
				return nil, fmt.Errorf("error parsing tx %d block %x: %v", i, payload.BlockHash, err)
			} else {
				h := decTx.Hash()
				tx.TxHash = h[:]
				tx.AccountNonce = decTx.Nonce()
				// big endian
				tx.Price = decTx.GasPrice().Bytes()
				tx.GasLimit = decTx.Gas()
				sender, err := lc.signer.Sender(&decTx)
				if err != nil {
					return nil, fmt.Errorf("transaction with invalid sender (tx hash: %x): %v", h, err)
				}
				tx.Sender = sender.Bytes()
				if v := decTx.To(); v != nil {
					tx.Recipient = v.Bytes()
				} else {
					tx.Recipient = []byte{}
				}
				tx.Amount = decTx.Value().Bytes()
				tx.Payload = decTx.Data()
				tx.MaxPriorityFeePerGas = decTx.GasTipCap().Uint64()
				tx.MaxFeePerGas = decTx.GasFeeCap().Uint64()
			}
			txs = append(txs, tx)
		}
		block.ExecutionPayload = &types.ExecutionPayload{
			ParentHash:    payload.ParentHash,
			FeeRecipient:  payload.FeeRecipient,
			StateRoot:     payload.StateRoot,
			ReceiptsRoot:  payload.ReceiptsRoot,
			LogsBloom:     payload.LogsBloom,
			Random:        payload.PrevRandao,
			BlockNumber:   uint64(payload.BlockNumber),
			GasLimit:      uint64(payload.GasLimit),
			GasUsed:       uint64(payload.GasUsed),
			Timestamp:     uint64(payload.Timestamp),
			ExtraData:     payload.ExtraData,
			BaseFeePerGas: uint64(payload.BaseFeePerGas),
			BlockHash:     payload.BlockHash,
			Transactions:  txs,
		}
	}

	// TODO: this is legacy from old lighthouse API. Does it even still apply?
	if block.Eth1Data.DepositCount > 2147483647 { // Sometimes the lighthouse node does return bogus data for the DepositCount value
		block.Eth1Data.DepositCount = 0
	}

	for i, proposerSlashing := range parsedBlock.Message.Body.ProposerSlashings {
		block.ProposerSlashings[i] = &types.ProposerSlashing{
			ProposerIndex: uint64(proposerSlashing.SignedHeader1.Message.ProposerIndex),
			Header1: &types.Block{
				Slot:       uint64(proposerSlashing.SignedHeader1.Message.Slot),
				ParentRoot: utils.MustParseHex(proposerSlashing.SignedHeader1.Message.ParentRoot),
				StateRoot:  utils.MustParseHex(proposerSlashing.SignedHeader1.Message.StateRoot),
				Signature:  utils.MustParseHex(proposerSlashing.SignedHeader1.Signature),
				BodyRoot:   utils.MustParseHex(proposerSlashing.SignedHeader1.Message.BodyRoot),
			},
			Header2: &types.Block{
				Slot:       uint64(proposerSlashing.SignedHeader2.Message.Slot),
				ParentRoot: utils.MustParseHex(proposerSlashing.SignedHeader2.Message.ParentRoot),
				StateRoot:  utils.MustParseHex(proposerSlashing.SignedHeader2.Message.StateRoot),
				Signature:  utils.MustParseHex(proposerSlashing.SignedHeader2.Signature),
				BodyRoot:   utils.MustParseHex(proposerSlashing.SignedHeader2.Message.BodyRoot),
			},
		}
	}

	for i, attesterSlashing := range parsedBlock.Message.Body.AttesterSlashings {
		block.AttesterSlashings[i] = &types.AttesterSlashing{
			Attestation1: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation1.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation1.Data.Index),
					BeaconBlockRoot: utils.MustParseHex(attesterSlashing.Attestation1.Data.BeaconBlockRoot),
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation1.Data.Source.Epoch),
						Root:  utils.MustParseHex(attesterSlashing.Attestation1.Data.Source.Root),
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation1.Data.Target.Epoch),
						Root:  utils.MustParseHex(attesterSlashing.Attestation1.Data.Target.Root),
					},
				},
				Signature:        utils.MustParseHex(attesterSlashing.Attestation1.Signature),
				AttestingIndices: uint64List(attesterSlashing.Attestation1.AttestingIndices),
			},
			Attestation2: &types.IndexedAttestation{
				Data: &types.AttestationData{
					Slot:            uint64(attesterSlashing.Attestation2.Data.Slot),
					CommitteeIndex:  uint64(attesterSlashing.Attestation2.Data.Index),
					BeaconBlockRoot: utils.MustParseHex(attesterSlashing.Attestation2.Data.BeaconBlockRoot),
					Source: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation2.Data.Source.Epoch),
						Root:  utils.MustParseHex(attesterSlashing.Attestation2.Data.Source.Root),
					},
					Target: &types.Checkpoint{
						Epoch: uint64(attesterSlashing.Attestation2.Data.Target.Epoch),
						Root:  utils.MustParseHex(attesterSlashing.Attestation2.Data.Target.Root),
					},
				},
				Signature:        utils.MustParseHex(attesterSlashing.Attestation2.Signature),
				AttestingIndices: uint64List(attesterSlashing.Attestation2.AttestingIndices),
			},
		}
	}

	for i, attestation := range parsedBlock.Message.Body.Attestations {
		a := &types.Attestation{
			AggregationBits: utils.MustParseHex(attestation.AggregationBits),
			Attesters:       []uint64{},
			Data: &types.AttestationData{
				Slot:            uint64(attestation.Data.Slot),
				CommitteeIndex:  uint64(attestation.Data.Index),
				BeaconBlockRoot: utils.MustParseHex(attestation.Data.BeaconBlockRoot),
				Source: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Source.Epoch),
					Root:  utils.MustParseHex(attestation.Data.Source.Root),
				},
				Target: &types.Checkpoint{
					Epoch: uint64(attestation.Data.Target.Epoch),
					Root:  utils.MustParseHex(attestation.Data.Target.Root),
				},
			},
			Signature: utils.MustParseHex(attestation.Signature),
		}

		aggregationBits := bitfield.Bitlist(a.AggregationBits)
		assignments, err := lc.GetEpochAssignments(a.Data.Slot / utils.Config.Chain.Config.SlotsPerEpoch)
		if err != nil {
			return nil, fmt.Errorf("error receiving epoch assignment for epoch %v: %v", a.Data.Slot/utils.Config.Chain.Config.SlotsPerEpoch, err)
		}

		for i := uint64(0); i < aggregationBits.Len(); i++ {
			if aggregationBits.BitAt(i) {
				validator, found := assignments.AttestorAssignments[utils.FormatAttestorAssignmentKey(a.Data.Slot, a.Data.CommitteeIndex, i)]
				if !found { // This should never happen!
					validator = 0
					logger.Errorf("error retrieving assigned validator for attestation %v of block %v for slot %v committee index %v member index %v", i, block.Slot, a.Data.Slot, a.Data.CommitteeIndex, i)
				}
				a.Attesters = append(a.Attesters, validator)
			}
		}

		block.Attestations[i] = a
	}

	for i, deposit := range parsedBlock.Message.Body.Deposits {
		d := &types.Deposit{
			Proof:                 nil,
			PublicKey:             utils.MustParseHex(deposit.Data.Pubkey),
			WithdrawalCredentials: utils.MustParseHex(deposit.Data.WithdrawalCredentials),
			Amount:                uint64(deposit.Data.Amount),
			Signature:             utils.MustParseHex(deposit.Data.Signature),
		}

		block.Deposits[i] = d
	}

	for i, voluntaryExit := range parsedBlock.Message.Body.VoluntaryExits {
		block.VoluntaryExits[i] = &types.VoluntaryExit{
			Epoch:          uint64(voluntaryExit.Message.Epoch),
			ValidatorIndex: uint64(voluntaryExit.Message.ValidatorIndex),
			Signature:      utils.MustParseHex(voluntaryExit.Signature),
		}
	}

	return block, nil
}

func syncCommitteeParticipation(bits []byte) float64 {
	participating := 0
	for i := 0; i < int(utils.Config.Chain.Config.SyncCommitteeSize); i++ {
		if utils.BitAtVector(bits, i) {
			participating++
		}
	}
	return float64(participating) / float64(utils.Config.Chain.Config.SyncCommitteeSize)
}

// GetValidatorParticipation will get the validator participation from the Lighthouse RPC api
func (lc *LighthouseClient) GetValidatorParticipation(epoch uint64) (*types.ValidatorParticipation, error) {
	resp, err := lc.get(fmt.Sprintf("%s/lighthouse/validator_inclusion/%d/global", lc.endpoint, epoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validator participation data for epoch %v: %v", epoch, err)
	}

	var parsedResponse LighthouseValidatorParticipationResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("error parsing validator participation data for epoch %v: %v", epoch, err)
	}

	return &types.ValidatorParticipation{
		Epoch: epoch,
		// technically there are rules for delayed finalization through previous epoch, applied only later. But good enough, this matches previous wonky behavior
		Finalized:               float32(parsedResponse.Data.CurrentEpochTargetAttestingGwei)/float32(parsedResponse.Data.CurrentEpochActiveGwei) > (float32(2) / float32(3)),
		GlobalParticipationRate: float32(parsedResponse.Data.PreviousEpochTargetAttestingGwei) / float32(parsedResponse.Data.CurrentEpochActiveGwei),
		VotedEther:              uint64(parsedResponse.Data.PreviousEpochTargetAttestingGwei),
		EligibleEther:           uint64(parsedResponse.Data.CurrentEpochActiveGwei),
	}, nil
}

func (lc *LighthouseClient) GetFinalityCheckpoints(epoch uint64) (*types.FinalityCheckpoints, error) {
	// finalityResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/finality_checkpoints", lc.endpoint, id))
	// if err != nil {
	//      return nil, fmt.Errorf("error retrieving finality checkpoints of head: %v", err)
	// }
	return &types.FinalityCheckpoints{}, nil
}

func (lc *LighthouseClient) GetSyncCommittee(stateID string, epoch uint64) (*StandardSyncCommittee, error) {
	syncCommitteesResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%s/sync_committees?epoch=%d", lc.endpoint, stateID, epoch))
	if err != nil {
		return nil, fmt.Errorf("error retrieving sync_committees for epoch %v (state: %v): %w", epoch, stateID, err)
	}
	var parsedSyncCommittees StandardSyncCommitteesResponse
	err = json.Unmarshal(syncCommitteesResp, &parsedSyncCommittees)
	if err != nil {
		return nil, fmt.Errorf("error parsing sync_committees data for epoch %v (state: %v): %w", epoch, stateID, err)
	}
	return &parsedSyncCommittees.Data, nil
}

// GetSlotData will get the slot data
func (lc *LighthouseClient) GetSlotData(block *types.Block) (*types.SlotData, error) {
	wg := &sync.WaitGroup{}
	var err error

	slot := utils.EpochOfSlot(block.Slot)
	epoch := utils.EpochOfSlot(slot)
	data := &types.SlotData{}
	data.Epoch = epoch
	data.Slot = slot

	validatorsResp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validators", lc.endpoint, slot))
	if err != nil {
		return nil, fmt.Errorf("error retrieving validators for slot %v: %v", slot, err)
	}
	var parsedValidators StandardValidatorsResponse
	err = json.Unmarshal(validatorsResp, &parsedValidators)
	if err != nil {
		return nil, fmt.Errorf("error parsing epoch validators: %v", err)
	}

	slot1d := int64(slot) - 7200
	slot7d := int64(slot) - 7200*7
	slot31d := int64(slot) - 7200*31

	var validatorBalances1d map[uint64]uint64
	var validatorBalances7d map[uint64]uint64
	var validatorBalances31d map[uint64]uint64

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances1d, err = lc.GetBalancesForSlot(slot1d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (1d): %v", slot1d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (1d) took %v", len(parsedValidators.Data), slot1d, time.Since(start))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances7d, err = lc.GetBalancesForSlot(slot7d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (7d): %v", slot7d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (7d) took %v", len(parsedValidators.Data), slot7d, time.Since(start))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		var err error
		validatorBalances31d, err = lc.GetBalancesForSlot(slot31d)
		if err != nil {
			logrus.Errorf("error retrieving validator balances for slot %v (31d): %v", slot31d, err)
			return
		}
		logger.Printf("retrieved data for %v validator balances for slot %v (31d) took %v", len(parsedValidators.Data), slot31d, time.Since(start))
	}()
	wg.Wait()

	// Retrieve a block for the slot
	data.Blocks = make(map[uint64]map[string]*types.Block)
	if data.Blocks[block.Slot] == nil {
		data.Blocks[block.Slot] = make(map[string]*types.Block)
	}
	data.Blocks[block.Slot][fmt.Sprintf("%x", block.BlockRoot)] = block
	logger.Printf("retrieved a block for slot %v", slot)

	// Retrieve the validator set for the slot
	data.Validators = make([]*types.Validator, 0)

	for _, validator := range parsedValidators.Data {
		data.Validators = append(data.Validators, &types.Validator{
			Index:                      uint64(validator.Index),
			PublicKey:                  utils.MustParseHex(validator.Validator.Pubkey),
			WithdrawalCredentials:      utils.MustParseHex(validator.Validator.WithdrawalCredentials),
			Balance:                    uint64(validator.Balance),
			EffectiveBalance:           uint64(validator.Validator.EffectiveBalance),
			Slashed:                    validator.Validator.Slashed,
			ActivationEligibilityEpoch: uint64(validator.Validator.ActivationEligibilityEpoch),
			ActivationEpoch:            uint64(validator.Validator.ActivationEpoch),
			ExitEpoch:                  uint64(validator.Validator.ExitEpoch),
			WithdrawableEpoch:          uint64(validator.Validator.WithdrawableEpoch),
			Balance1d:                  validatorBalances1d[uint64(validator.Index)],
			Balance7d:                  validatorBalances7d[uint64(validator.Index)],
			Balance31d:                 validatorBalances31d[uint64(validator.Index)],
			Status:                     validator.Status,
		})
	}

	logger.Printf("retrieved data for %v validators for slot %v", len(data.Validators), slot)

	return data, nil
}

func (lc *LighthouseClient) GetBalancesForSlot(slot int64) (map[uint64]uint64, error) {
	if slot < 0 {
		slot = 0
	}

	var err error

	validatorBalances := make(map[uint64]uint64)

	resp, err := lc.get(fmt.Sprintf("%s/eth/v1/beacon/states/%d/validator_balances", lc.endpoint, slot))
	if err != nil {
		return validatorBalances, err
	}

	var parsedResponse StandardValidatorBalancesResponse
	err = json.Unmarshal(resp, &parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("error parsing response for validator_balances")
	}

	for _, b := range parsedResponse.Data {
		validatorBalances[uint64(b.Index)] = uint64(b.Balance)
	}

	return validatorBalances, nil
}

var notFoundErr = errors.New("not found 404")

func (lc *LighthouseClient) get(url string) ([]byte, error) {
	// t0 := time.Now()
	// defer func() { fmt.Println(url, time.Since(t0)) }()
	client := &http.Client{Timeout: time.Second * 120}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, notFoundErr
		}
		return nil, fmt.Errorf("error-response: %s", data)
	}

	return data, err
}
