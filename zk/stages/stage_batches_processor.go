package stages

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/core/rawdb"
	ethTypes "github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/zk/datastream/types"
	txtype "github.com/ledgerwatch/erigon/zk/tx"
	"github.com/ledgerwatch/erigon/zk/utils"
	"github.com/ledgerwatch/log/v3"
)

var (
	ErrorTriggeredUnwind = errors.New("triggered unwind")
	ErrorSkippedBlock    = errors.New("skipped block")
)

type ProcessorErigonDb interface {
	WriteHeader(batchNo *big.Int, blockHash common.Hash, stateRoot, txHash, parentHash common.Hash, coinbase common.Address, ts, gasLimit uint64, chainConfig *chain.Config) (*ethTypes.Header, error)
	WriteBody(batchNo *big.Int, headerHash common.Hash, txs []ethTypes.Transaction) error
	ReadCanonicalHash(L2BlockNumber uint64) (common.Hash, error)
}

type ProcessorHermezDb interface {
	WriteForkId(batchNumber uint64, forkId uint64) error
	WriteForkIdBlockOnce(forkId, blockNum uint64) error
	WriteBlockBatch(l2BlockNumber uint64, batchNumber uint64) error
	WriteEffectiveGasPricePercentage(txHash common.Hash, effectiveGasPricePercentage uint8) error

	WriteStateRoot(l2BlockNumber uint64, rpcRoot common.Hash) error
	GetStateRoot(l2BlockNumber uint64) (common.Hash, error)

	CheckGlobalExitRootWritten(ger common.Hash) (bool, error)
	WriteBlockGlobalExitRoot(l2BlockNo uint64, ger common.Hash) error
	WriteGlobalExitRoot(ger common.Hash) error

	WriteReusedL1InfoTreeIndex(l2BlockNo uint64) error
	WriteBlockL1BlockHash(l2BlockNo uint64, l1BlockHash common.Hash) error
	WriteBatchGlobalExitRoot(batchNumber uint64, ger *types.GerUpdate) error
	WriteIntermediateTxStateRoot(l2BlockNumber uint64, txHash common.Hash, rpcRoot common.Hash) error
	WriteBlockL1InfoTreeIndex(blockNumber uint64, l1Index uint64) error
	WriteLatestUsedGer(blockNo uint64, ger common.Hash) error
	WriteInvalidBatch(batchNumber uint64) error
	WriteBatchEnd(lastBlockHeight uint64) error
	GetBatchNoByL2Block(l2BlockNumber uint64) (uint64, error)
}

type DsQueryClient interface {
	GetL2BlockByNumber(blockNum uint64) (*types.FullL2Block, error)
	GetProgressAtomic() *atomic.Uint64
}

type BatchesProcessor struct {
	ctx       context.Context
	logPrefix string
	tx        kv.RwTx
	hermezDb  ProcessorHermezDb
	eriDb     ProcessorErigonDb
	syncBlockLimit,
	debugBlockLimit,
	debugStepAfter,
	debugStep,
	stageProgressBlockNo,
	highestHashableL2BlockNo,
	lastForkId uint64
	highestL1InfoTreeIndex uint32
	dsQueryClient          DsQueryClient
	progressChan           chan uint64
	unwindFn               func(uint64) (uint64, error)

	highestSeenBatchNo,
	lastBlockHeight,
	blocksWritten,
	highestVerifiedBatch uint64
	lastBlockRoot,
	lastBlockHash common.Hash
	chainConfig  *chain.Config
	miningConfig *params.MiningConfig
}

func NewBatchesProcessor(
	ctx context.Context,
	logPrefix string,
	tx kv.RwTx,
	hermezDb ProcessorHermezDb,
	eriDb ProcessorErigonDb,
	syncBlockLimit, debugBlockLimit, debugStepAfter, debugStep, stageProgressBlockNo, stageProgressBatchNo uint64,
	lastProcessedBlockHash common.Hash,
	dsQueryClient DsQueryClient,
	progressChan chan uint64,
	chainConfig *chain.Config,
	miningConfig *params.MiningConfig,
	unwindFn func(uint64) (uint64, error),
) (*BatchesProcessor, error) {
	highestVerifiedBatch, err := stages.GetStageProgress(tx, stages.L1VerificationsBatchNo)
	if err != nil {
		return nil, errors.New("could not retrieve l1 verifications batch no progress")
	}

	lastForkId, err := stages.GetStageProgress(tx, stages.ForkId)
	if err != nil {
		return nil, fmt.Errorf("failed to get last fork id, %w", err)
	}

	return &BatchesProcessor{
		ctx:                  ctx,
		logPrefix:            logPrefix,
		tx:                   tx,
		hermezDb:             hermezDb,
		eriDb:                eriDb,
		syncBlockLimit:       syncBlockLimit,
		debugBlockLimit:      debugBlockLimit,
		debugStep:            debugStep,
		debugStepAfter:       debugStepAfter,
		stageProgressBlockNo: stageProgressBlockNo,
		lastBlockHeight:      stageProgressBlockNo,
		highestSeenBatchNo:   stageProgressBatchNo,
		highestVerifiedBatch: highestVerifiedBatch,
		dsQueryClient:        dsQueryClient,
		progressChan:         progressChan,
		lastBlockHash:        lastProcessedBlockHash,
		lastBlockRoot:        emptyHash,
		lastForkId:           lastForkId,
		unwindFn:             unwindFn,
		chainConfig:          chainConfig,
		miningConfig:         miningConfig,
	}, nil
}

func (p *BatchesProcessor) ProcessEntry(entry interface{}) (endLoop bool, err error) {
	switch entry := entry.(type) {
	case *types.BatchStart:
		return false, p.processBatchStartEntry(entry)
	case *types.BatchEnd:
		return false, p.processBatchEndEntry(entry)
	case *types.FullL2Block:
		return p.processFullBlock(entry)
	case *types.GerUpdate:
		return false, p.processGerUpdate(entry)
	case nil: // we use nil to indicate the end of stream read
		return true, nil
	default:
		return false, fmt.Errorf("unknown entry type: %T", entry)
	}
}

func (p *BatchesProcessor) processGerUpdate(gerUpdate *types.GerUpdate) error {
	if gerUpdate.GlobalExitRoot == emptyHash {
		log.Warn(fmt.Sprintf("[%s] Skipping GER update with empty root", p.logPrefix))
		return nil
	}

	// NB: we won't get these post Etrog (fork id 7)
	if err := p.hermezDb.WriteBatchGlobalExitRoot(gerUpdate.BatchNumber, gerUpdate); err != nil {
		return fmt.Errorf("write batch global exit root error: %w", err)
	}

	return nil
}

func (p *BatchesProcessor) processBatchEndEntry(batchEnd *types.BatchEnd) (err error) {
	if batchEnd.StateRoot != p.lastBlockRoot {
		log.Debug(fmt.Sprintf("[%s] batch end state root mismatches last block's: %x, expected: %x", p.logPrefix, batchEnd.StateRoot, p.lastBlockRoot))
	}
	// keep a record of the last block processed when we receive the batch end
	if err = p.hermezDb.WriteBatchEnd(p.lastBlockHeight); err != nil {
		return err
	}
	return nil
}

func (p *BatchesProcessor) processBatchStartEntry(batchStart *types.BatchStart) (err error) {
	// check if the batch is invalid so that we can replicate this over in the stream
	// when we re-populate it
	if batchStart.BatchType == types.BatchTypeInvalid {
		if err = p.hermezDb.WriteInvalidBatch(batchStart.Number); err != nil {
			return err
		}
		// we need to write the fork here as well because the batch will never get processed as it is invalid
		// but, we need it re-populate our own stream
		if err = p.hermezDb.WriteForkId(batchStart.Number, batchStart.ForkId); err != nil {
			return err
		}
	}

	return nil
}

func (p *BatchesProcessor) unwind(blockNum uint64) (uint64, error) {
	unwindBlock, err := p.unwindFn(blockNum)
	if err != nil {
		return 0, err
	}

	return unwindBlock, nil
}

func (p *BatchesProcessor) processFullBlock(blockEntry *types.FullL2Block) (endLoop bool, err error) {
	log.Debug(fmt.Sprintf("[%s] Retrieved %d (%s) block from stream", p.logPrefix, blockEntry.L2BlockNumber, blockEntry.L2Blockhash.String()))
	if p.syncBlockLimit > 0 && blockEntry.L2BlockNumber >= p.syncBlockLimit {
		// stop the node going into a crazy loop
		log.Info(fmt.Sprintf("[%s] Sync block limit reached, stopping stage", p.logPrefix), "blockLimit", p.syncBlockLimit, "block", blockEntry.L2BlockNumber)
		return true, nil
	}

	if blockEntry.BatchNumber > p.highestSeenBatchNo && p.lastForkId < blockEntry.ForkId {
		if blockEntry.ForkId >= uint64(chain.ImpossibleForkId) {
			message := fmt.Sprintf("unsupported fork id %d received from the data stream", blockEntry.ForkId)
			panic(message)
		}
		if err = stages.SaveStageProgress(p.tx, stages.ForkId, blockEntry.ForkId); err != nil {
			return false, fmt.Errorf("save stage progress error: %w", err)
		}
		p.lastForkId = blockEntry.ForkId
		if err = p.hermezDb.WriteForkId(blockEntry.BatchNumber, blockEntry.ForkId); err != nil {
			return false, fmt.Errorf("write fork id error: %w", err)
		}
		// NOTE (RPC): avoided use of 'writeForkIdBlockOnce' by reading instead batch by forkId, and then lowest block number in batch
	}

	// ignore genesis or a repeat of the last block
	if blockEntry.L2BlockNumber == 0 {
		return false, nil
	}
	// skip but warn on already processed blocks
	if blockEntry.L2BlockNumber <= p.stageProgressBlockNo {
		dbBatchNum, err := p.hermezDb.GetBatchNoByL2Block(blockEntry.L2BlockNumber)
		if err != nil {
			return false, err
		}

		if blockEntry.L2BlockNumber == p.stageProgressBlockNo && dbBatchNum == blockEntry.BatchNumber {
			// only warn if the block is very old, we expect the very latest block to be requested
			// when the stage is fired up for the first time
			log.Warn(fmt.Sprintf("[%s] Skipping block %d, already processed", p.logPrefix, blockEntry.L2BlockNumber))
			return false, nil
		}

		// if the block is older or the batch number is different, we need to unwind because the block has definately changed
		log.Warn(fmt.Sprintf("[%s] Block already processed. Triggering unwind...", p.logPrefix),
			"block", blockEntry.L2BlockNumber, "ds batch", blockEntry.BatchNumber, "db batch", dbBatchNum)
		if _, err := p.unwind(blockEntry.L2BlockNumber); err != nil {
			return false, err
		}
		return false, ErrorTriggeredUnwind
	}

	var dbParentBlockHash common.Hash
	if blockEntry.L2BlockNumber > 1 {
		dbParentBlockHash, err = p.eriDb.ReadCanonicalHash(p.lastBlockHeight)
		if err != nil {
			return false, fmt.Errorf("failed to retrieve parent block hash for datastream block %d: %w",
				blockEntry.L2BlockNumber, err)
		}
	}

	if p.lastBlockHeight > 0 && dbParentBlockHash != p.lastBlockHash {
		// unwind/rollback blocks until the latest common ancestor block
		log.Warn(fmt.Sprintf("[%s] Parent block hashes mismatch on block %d. Triggering unwind...", p.logPrefix, blockEntry.L2BlockNumber),
			"db parent block hash", dbParentBlockHash,
			"ds parent block number", p.lastBlockHeight,
			"ds parent block hash", p.lastBlockHash,
			"ds parent block number", blockEntry.L2BlockNumber-1,
		)
		//parent blockhash is wrong, so unwind to it, then restat stream from it to get the correct one
		if _, err := p.unwind(blockEntry.L2BlockNumber - 1); err != nil {
			return false, err
		}
		return false, ErrorTriggeredUnwind
	}

	// unwind if we already have this block
	if blockEntry.L2BlockNumber < p.lastBlockHeight+1 {
		log.Warn(fmt.Sprintf("[%s] Block %d, already processed unwinding...", p.logPrefix, blockEntry.L2BlockNumber))
		if _, err := p.unwind(blockEntry.L2BlockNumber); err != nil {
			return false, err
		}

		return false, ErrorTriggeredUnwind
	}

	// check for sequential block numbers
	if blockEntry.L2BlockNumber > p.lastBlockHeight+1 {
		return false, ErrorSkippedBlock
	}

	// batch boundary - record the highest hashable block number (last block in last full batch)
	if blockEntry.BatchNumber > p.highestSeenBatchNo {
		p.highestHashableL2BlockNo = blockEntry.L2BlockNumber - 1
	}
	p.highestSeenBatchNo = blockEntry.BatchNumber

	/////// DEBUG BISECTION ///////
	// exit stage when debug bisection flags set and we're at the limit block
	if p.debugBlockLimit > 0 && blockEntry.L2BlockNumber > p.debugBlockLimit {
		log.Info(fmt.Sprintf("[%s] Debug limit reached, stopping stage\n", p.logPrefix))
		endLoop = true
	}

	// if we're above StepAfter, and we're at a step, move the stages on
	if p.debugStep > 0 && p.debugStepAfter > 0 && blockEntry.L2BlockNumber > p.debugStepAfter {
		if blockEntry.L2BlockNumber%p.debugStep == 0 {
			log.Info(fmt.Sprintf("[%s] Debug step reached, stopping stage\n", p.logPrefix))
			endLoop = true
		}
	}
	/////// END DEBUG BISECTION ///////

	// store our finalized state if this batch matches the highest verified batch number on the L1
	if blockEntry.BatchNumber == p.highestVerifiedBatch {
		rawdb.WriteForkchoiceFinalized(p.tx, blockEntry.L2Blockhash)
	}

	if p.lastBlockHash != emptyHash {
		blockEntry.ParentHash = p.lastBlockHash
	} else {
		// first block in the loop so read the parent hash
		previousHash, err := p.eriDb.ReadCanonicalHash(blockEntry.L2BlockNumber - 1)
		if err != nil {
			return false, fmt.Errorf("failed to get genesis header: %w", err)
		}
		blockEntry.ParentHash = previousHash
	}

	if err := p.writeL2Block(blockEntry); err != nil {
		return false, fmt.Errorf("writeL2Block error: %w", err)
	}

	p.dsQueryClient.GetProgressAtomic().Store(blockEntry.L2BlockNumber)

	// make sure to capture the l1 info tree index changes so we can store progress
	if blockEntry.L1InfoTreeIndex > p.highestL1InfoTreeIndex {
		p.highestL1InfoTreeIndex = blockEntry.L1InfoTreeIndex
	}

	p.lastBlockHash = blockEntry.L2Blockhash
	p.lastBlockRoot = blockEntry.StateRoot

	p.lastBlockHeight = blockEntry.L2BlockNumber
	p.blocksWritten++
	p.progressChan <- p.blocksWritten

	if p.debugBlockLimit == 0 {
		endLoop = false
	}
	return endLoop, nil
}

// writeL2Block writes L2Block to ErigonDb and HermezDb
// writes header, body, forkId and blockBatch
func (p *BatchesProcessor) writeL2Block(l2Block *types.FullL2Block) error {
	bn := new(big.Int).SetUint64(l2Block.L2BlockNumber)
	txs := make([]ethTypes.Transaction, 0, len(l2Block.L2Txs))
	for _, transaction := range l2Block.L2Txs {
		ltx, _, err := txtype.DecodeTx(transaction.Encoded, transaction.EffectiveGasPricePercentage, l2Block.ForkId)
		if err != nil {
			return fmt.Errorf("decode tx error: %w", err)
		}
		txs = append(txs, ltx)

		if err := p.hermezDb.WriteEffectiveGasPricePercentage(ltx.Hash(), transaction.EffectiveGasPricePercentage); err != nil {
			return fmt.Errorf("write effective gas price percentage error: %w", err)
		}

		if err := p.hermezDb.WriteStateRoot(l2Block.L2BlockNumber, transaction.IntermediateStateRoot); err != nil {
			return fmt.Errorf("write rpc root error: %w", err)
		}

		if err := p.hermezDb.WriteIntermediateTxStateRoot(l2Block.L2BlockNumber, ltx.Hash(), transaction.IntermediateStateRoot); err != nil {
			return fmt.Errorf("write rpc root error: %w", err)
		}
	}
	txCollection := ethTypes.Transactions(txs)
	txHash := ethTypes.DeriveSha(txCollection)

	var gasLimit uint64
	if !p.chainConfig.IsNormalcy(l2Block.L2BlockNumber) {
		gasLimit = utils.GetBlockGasLimitForFork(l2Block.ForkId)
	} else {
		gasLimit = p.miningConfig.GasLimit
	}

	if _, err := p.eriDb.WriteHeader(bn, l2Block.L2Blockhash, l2Block.StateRoot, txHash, l2Block.ParentHash, l2Block.Coinbase, uint64(l2Block.Timestamp), gasLimit, p.chainConfig); err != nil {
		return fmt.Errorf("write header error: %w", err)
	}

	didStoreGer := false
	l1InfoTreeIndexReused := false

	if l2Block.GlobalExitRoot != emptyHash {
		gerWritten, err := p.hermezDb.CheckGlobalExitRootWritten(l2Block.GlobalExitRoot)
		if err != nil {
			return fmt.Errorf("get global exit root error: %w", err)
		}

		if !gerWritten {
			if err := p.hermezDb.WriteBlockGlobalExitRoot(l2Block.L2BlockNumber, l2Block.GlobalExitRoot); err != nil {
				return fmt.Errorf("write block global exit root error: %w", err)
			}

			if err := p.hermezDb.WriteGlobalExitRoot(l2Block.GlobalExitRoot); err != nil {
				return fmt.Errorf("write global exit root error: %w", err)
			}
			didStoreGer = true
		}
	}

	if l2Block.L1BlockHash != emptyHash {
		if err := p.hermezDb.WriteBlockL1BlockHash(l2Block.L2BlockNumber, l2Block.L1BlockHash); err != nil {
			return fmt.Errorf("write block global exit root error: %w", err)
		}
	}

	if l2Block.L1InfoTreeIndex != 0 {
		if err := p.hermezDb.WriteBlockL1InfoTreeIndex(l2Block.L2BlockNumber, uint64(l2Block.L1InfoTreeIndex)); err != nil {
			return err
		}

		// if the info tree index of this block is lower than the highest we've seen
		// we need to write the GER and l1 block hash regardless of the logic above.
		// this can only happen in post etrog blocks, and we need the GER/L1 block hash
		// for the stream and also for the block info root to be correct
		if l2Block.L1InfoTreeIndex <= p.highestL1InfoTreeIndex {
			l1InfoTreeIndexReused = true
			if err := p.hermezDb.WriteBlockGlobalExitRoot(l2Block.L2BlockNumber, l2Block.GlobalExitRoot); err != nil {
				return fmt.Errorf("write block global exit root error: %w", err)
			}
			if err := p.hermezDb.WriteBlockL1BlockHash(l2Block.L2BlockNumber, l2Block.L1BlockHash); err != nil {
				return fmt.Errorf("write block global exit root error: %w", err)
			}
			if err := p.hermezDb.WriteReusedL1InfoTreeIndex(l2Block.L2BlockNumber); err != nil {
				return fmt.Errorf("write reused l1 info tree index error: %w", err)
			}
		}
	}

	// if we haven't reused the l1 info tree index, and we have also written the GER
	// then we need to write the latest used GER for this batch to the table
	// we always want the last written GER in this table as it's at the batch level, so it can and should
	// be overwritten
	if !l1InfoTreeIndexReused && didStoreGer {
		if err := p.hermezDb.WriteLatestUsedGer(l2Block.L2BlockNumber, l2Block.GlobalExitRoot); err != nil {
			return fmt.Errorf("write latest used ger error: %w", err)
		}
	}

	if err := p.eriDb.WriteBody(bn, l2Block.L2Blockhash, txs); err != nil {
		return fmt.Errorf("write body error: %w", err)
	}

	if err := p.hermezDb.WriteForkId(l2Block.BatchNumber, l2Block.ForkId); err != nil {
		return fmt.Errorf("write block batch error: %w", err)
	}

	if err := p.hermezDb.WriteForkIdBlockOnce(l2Block.ForkId, l2Block.L2BlockNumber); err != nil {
		return fmt.Errorf("write fork id block error: %w", err)
	}

	if err := p.hermezDb.WriteBlockBatch(l2Block.L2BlockNumber, l2Block.BatchNumber); err != nil {
		return fmt.Errorf("write block batch error: %w", err)
	}

	return nil
}

func (p *BatchesProcessor) AtLeastOneBlockWritten() bool {
	return p.lastBlockHeight > 0
}

func (p *BatchesProcessor) LastBlockHeight() uint64 {
	return p.lastBlockHeight
}

func (p *BatchesProcessor) HighestSeenBatchNumber() uint64 {
	return p.highestSeenBatchNo
}

func (p *BatchesProcessor) HighestVerifiedBatchNumber() uint64 {
	return p.highestVerifiedBatch
}

func (p *BatchesProcessor) LastForkId() uint64 {
	return p.lastForkId
}

func (p *BatchesProcessor) TotalBlocksWritten() uint64 {
	return p.blocksWritten
}

func (p *BatchesProcessor) HighestHashableL2BlockNo() uint64 {
	return p.highestHashableL2BlockNo
}

func (p *BatchesProcessor) SetNewTx(tx kv.RwTx) {
	p.tx = tx
}
