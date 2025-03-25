package rpchelper

import (
	"context"
	"errors"
	"fmt"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/config3"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	"github.com/ledgerwatch/erigon-lib/kv/rawdbv3"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/systemcontracts"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	borfinality "github.com/ledgerwatch/erigon/polygon/bor/finality"
	"github.com/ledgerwatch/erigon/polygon/bor/finality/whitelist"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/zk/sequencer"
)

// unable to decode supplied params, or an invalid number of parameters
type nonCanonocalHashError struct{ hash libcommon.Hash }

func (e nonCanonocalHashError) ErrorCode() int { return -32603 }

func (e nonCanonocalHashError) Error() string {
	return fmt.Sprintf("hash %x is not currently canonical", e.hash)
}

func GetBlockNumber(blockNrOrHash rpc.BlockNumberOrHash, tx kv.Tx, filters *Filters) (uint64, libcommon.Hash, bool, error) {
	return _GetBlockNumber(blockNrOrHash.RequireCanonical, blockNrOrHash, tx, filters)
}

func GetCanonicalBlockNumber(blockNrOrHash rpc.BlockNumberOrHash, tx kv.Tx, filters *Filters) (uint64, libcommon.Hash, bool, error) {
	return _GetBlockNumber(true, blockNrOrHash, tx, filters)
}

func _GetBlockNumber(requireCanonical bool, blockNrOrHash rpc.BlockNumberOrHash, tx kv.Tx, filters *Filters) (blockNumber uint64, hash libcommon.Hash, latest bool, err error) {
	var finishedBlockNumber uint64

	if !sequencer.IsSequencer() {
		finishedBlockNumber, err = stages.GetStageProgress(tx, stages.Finish)
		if err != nil {
			return 0, libcommon.Hash{}, false, fmt.Errorf("getting finished block number: %w", err)
		}
	} else {
		finishedBlockNumber, err = stages.GetStageProgress(tx, stages.Execution)
		if err != nil {
			return 0, libcommon.Hash{}, false, fmt.Errorf("getting finished block number: %w", err)
		}
	}

	var ok bool
	hash, ok = blockNrOrHash.Hash()
	if !ok {
		number := *blockNrOrHash.BlockNumber
		switch number {
		case rpc.LatestBlockNumber:
			blockNumber = finishedBlockNumber
		case rpc.EarliestBlockNumber:
			blockNumber = 0
		case rpc.FinalizedBlockNumber:
			if whitelist.GetWhitelistingService() != nil {
				num := borfinality.GetFinalizedBlockNumber(tx)
				if num == 0 {
					// nolint
					return 0, libcommon.Hash{}, false, errors.New("No finalized block")
				}

				blockNum := borfinality.CurrentFinalizedBlock(tx, num).NumberU64()
				blockHash := rawdb.ReadHeaderByNumber(tx, blockNum).Hash()
				return blockNum, blockHash, false, nil
			}
			blockNumber, err = GetFinalizedBlockNumber(tx)
			if err != nil {
				return 0, libcommon.Hash{}, false, err
			}
		case rpc.SafeBlockNumber:
			// [zkevm] safe not available, returns finilized instead
			// blockNumber, err = GetSafeBlockNumber(tx)
			blockNumber, err = GetFinalizedBlockNumber(tx)
			if err != nil {
				return 0, libcommon.Hash{}, false, err
			}
		case rpc.PendingBlockNumber:
			pendingBlock := filters.LastPendingBlock()
			if pendingBlock == nil {
				blockNumber = finishedBlockNumber
			} else {
				return pendingBlock.NumberU64(), pendingBlock.Hash(), false, nil
			}
		case rpc.LatestExecutedBlockNumber:
			blockNumber, err = stages.GetStageProgress(tx, stages.Execution)
			if err != nil {
				return 0, libcommon.Hash{}, false, fmt.Errorf("getting latest executed block number: %w", err)
			}
		default:
			blockNumber = uint64(number.Int64())
			if blockNumber > finishedBlockNumber {
				return 0, libcommon.Hash{}, false, fmt.Errorf("block with number %d not found", blockNumber)
			}
		}
		hash, err = rawdb.ReadCanonicalHash(tx, blockNumber)
		if err != nil {
			return 0, libcommon.Hash{}, false, err
		}
	} else {
		number := rawdb.ReadHeaderNumber(tx, hash)
		if number == nil {
			return 0, libcommon.Hash{}, false, fmt.Errorf("block %x not found", hash)
		}
		blockNumber = *number

		ch, err := rawdb.ReadCanonicalHash(tx, blockNumber)
		if err != nil {
			return 0, libcommon.Hash{}, false, err
		}
		if requireCanonical && ch != hash {
			return 0, libcommon.Hash{}, false, nonCanonocalHashError{hash}
		}
	}
	return blockNumber, hash, blockNumber == finishedBlockNumber, nil
}

func CreateStateReader(ctx context.Context, tx kv.Tx, blockNrOrHash rpc.BlockNumberOrHash, txnIndex int, filters *Filters, stateCache kvcache.Cache, historyV3 bool, chainName string) (state.StateReader, error) {
	blockNumber, _, latest, err := _GetBlockNumber(true, blockNrOrHash, tx, filters)
	if err != nil {
		return nil, err
	}
	return CreateStateReaderFromBlockNumber(ctx, tx, blockNumber, latest, txnIndex, stateCache, historyV3, chainName)
}

func CreateStateReaderFromBlockNumber(ctx context.Context, tx kv.Tx, blockNumber uint64, latest bool, txnIndex int, stateCache kvcache.Cache, historyV3 bool, chainName string) (state.StateReader, error) {
	if latest {
		cacheView, err := stateCache.View(ctx, tx)
		if err != nil {
			return nil, err
		}
		return state.NewCachedReader2(cacheView, tx), nil
	}
	return CreateHistoryStateReader(tx, blockNumber+1, txnIndex, historyV3, chainName)
}

func CreateHistoryStateReader(tx kv.Tx, blockNumber uint64, txnIndex int, historyV3 bool, chainName string) (state.StateReader, error) {
	if !historyV3 {
		r := state.NewPlainState(tx, blockNumber, systemcontracts.SystemContractCodeLookup[chainName])
		// r.SetTrace(true)
		return r, nil
	}
	r := state.NewHistoryReaderV3()
	r.SetTx(tx)
	// r.SetTrace(true)
	minTxNum, err := rawdbv3.TxNums.Min(tx, blockNumber)
	if err != nil {
		return nil, err
	}
	r.SetTxNum(uint64(int(minTxNum) + txnIndex + 1))
	return r, nil
}

func NewLatestStateReader(tx kv.Getter) state.StateReader {
	if config3.EnableHistoryV4InTest {
		panic("implement me")
		// b.pendingReader = state.NewReaderV4(b.pendingReaderTx.(kv.TemporalTx))
	}
	return state.NewPlainStateReader(tx)
}

func NewLatestStateWriter(tx kv.RwTx, blockNum uint64) state.StateWriter {
	if config3.EnableHistoryV4InTest {
		panic("implement me")
	}
	return state.NewPlainStateWriter(tx, tx, blockNum)
}
