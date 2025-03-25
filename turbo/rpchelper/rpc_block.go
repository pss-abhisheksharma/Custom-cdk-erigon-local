package rpchelper

import (
	"fmt"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"

	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/zk/hermez_db"
	"github.com/ledgerwatch/erigon/zk/sequencer"
)

var UnknownBlockError = &rpc.CustomError{
	Code:    -39001,
	Message: "Unknown block",
}

func GetLatestFinishedBlockNumber(tx kv.Tx) (uint64, error) {
	var blockNum uint64
	var err error
	if sequencer.IsSequencer() {
		blockNum, err = stages.GetStageProgress(tx, stages.Execution)
	} else {
		blockNum, err = stages.GetStageProgress(tx, stages.Finish)
	}
	if err != nil {
		return 0, fmt.Errorf("getting latest block number: %w", err)
	}

	return blockNum, nil
}

func GetFinalizedBlockNumber(tx kv.Tx) (uint64, error) {
	// get highest verified batch
	highestVerifiedBatchNo, err := stages.GetStageProgress(tx, stages.L1VerificationsBatchNo)
	if err != nil {
		return 0, err
	}

	hermezDb := hermez_db.NewHermezDbReader(tx)
	// we've got the highest batch to execute to, now get it's highest block
	highestVerifiedBlock, _, err := hermezDb.GetHighestBlockInBatch(highestVerifiedBatchNo)
	if err != nil {
		return 0, err
	}

	var highestBlockNumber uint64
	if sequencer.IsSequencer() {
		highestBlockNumber, err = stages.GetStageProgress(tx, stages.Execution)
	} else {
		highestBlockNumber, err = stages.GetStageProgress(tx, stages.Finish)
	}
	if err != nil {
		return 0, fmt.Errorf("getting latest finished block number: %w", err)
	}

	blockNumber := highestVerifiedBlock
	if highestBlockNumber < blockNumber {
		blockNumber = highestBlockNumber
	}

	return blockNumber, nil
}

func GetSafeBlockNumber(tx kv.Tx) (uint64, error) {
	forkchoiceSafeHash := rawdb.ReadForkchoiceSafe(tx)
	if forkchoiceSafeHash != (libcommon.Hash{}) {
		forkchoiceSafeNum := rawdb.ReadHeaderNumber(tx, forkchoiceSafeHash)
		if forkchoiceSafeNum != nil {
			return *forkchoiceSafeNum, nil
		}
	}
	return 0, UnknownBlockError
}

func GetLatestExecutedBlockNumber(tx kv.Tx) (uint64, error) {
	blockNum, err := stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return 0, err
	}
	return blockNum, err
}
