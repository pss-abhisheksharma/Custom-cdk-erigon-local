package txpool

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/zk/legacy_executor_verifier"
	"github.com/ledgerwatch/log/v3"
	"github.com/status-im/keycard-go/hexutils"
)

type LimboSubPoolProcessor struct {
	zkCfg       *ethconfig.Zk
	chainConfig *chain.Config
	db          kv.RwDB
	txPool      *TxPool
	verifier    *legacy_executor_verifier.LegacyExecutorVerifier
	quit        <-chan struct{}
}

func NewLimboSubPoolProcessor(ctx context.Context, zkCfg *ethconfig.Zk, chainConfig *chain.Config, db kv.RwDB, txPool *TxPool, verifier *legacy_executor_verifier.LegacyExecutorVerifier) *LimboSubPoolProcessor {
	return &LimboSubPoolProcessor{
		zkCfg:       zkCfg,
		chainConfig: chainConfig,
		db:          db,
		txPool:      txPool,
		verifier:    verifier,
		quit:        ctx.Done(),
	}
}

func (_this *LimboSubPoolProcessor) StartWork() {
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
	LOOP:
		for {
			select {
			case <-_this.quit:
				break LOOP
			case <-tick.C:
				_this.run()
			}
		}
	}()
}

func (_this *LimboSubPoolProcessor) run() {
	log.Info("[Limbo pool processor] Starting")
	defer log.Info("[Limbo pool processor] End")

	ctx := context.Background()
	limboBlocksDetails := _this.txPool.GetUncheckedLimboBlocksDetailsClonedWeak()

	size := len(limboBlocksDetails)
	if size == 0 {
		return
	}

	totalTransactions := 0
	processedTransactions := 0
	for _, limboBlock := range limboBlocksDetails {
		for _, limboTx := range limboBlock.Transactions {
			if !limboTx.hasRoot() {
				return
			}
			totalTransactions++
		}
	}

	tx, err := _this.db.BeginRo(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback()

	// we just need some counter variable with large used values in order verify not to complain
	batchCounters := vm.NewBatchCounterCollector(256, 1, _this.zkCfg.VirtualCountersSmtReduction, true, nil)
	unlimitedCounters := batchCounters.NewCounters().UsedAsMap()
	for k := range unlimitedCounters {
		unlimitedCounters[k] = math.MaxInt32
	}

	invalidTxs := []*string{}
	invalidBlocksIndices := []int{}
	lastAddedInvalidBlockIndex := -1

	for i, limboBlock := range limboBlocksDetails {
		for _, limboTx := range limboBlock.Transactions {
			request := legacy_executor_verifier.NewVerifierRequest(limboBlock.ForkId, limboBlock.BatchNumber, []uint64{limboBlock.BlockNumber}, limboTx.Root, unlimitedCounters)
			err := _this.verifier.VerifySync(tx, request, limboBlock.Witness, limboTx.StreamBytes, limboBlock.BlockTimestamp, limboBlock.L1InfoTreeMinTimestamps)
			if err != nil {
				idHash := hexutils.BytesToHex(limboTx.Hash[:])
				invalidTxs = append(invalidTxs, &idHash)
				if lastAddedInvalidBlockIndex != i {
					invalidBlocksIndices = append(invalidBlocksIndices, i)
					lastAddedInvalidBlockIndex = i
				}
				log.Info("[Limbo pool processor]", "invalid tx", limboTx.Hash, "err", err)
				continue
			}

			processedTransactions++
			log.Info("[Limbo pool processor]", "valid tx", limboTx.Hash, "progress", fmt.Sprintf("transactions: %d of %d, blocks: %d of %d", processedTransactions, totalTransactions, i+1, len(limboBlocksDetails)))
		}
	}

	_this.txPool.MarkProcessedLimboDetails(size, invalidBlocksIndices, invalidTxs)
}
