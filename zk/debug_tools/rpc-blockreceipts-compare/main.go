package main

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/ledgerwatch/erigon/zk/debug_tools"
)

// compare block hashes and binary search the first block where they mismatch
// then print the block number and the field differences
func main() {
	ctx := context.Background()
	rpcConfig, err := debug_tools.GetConf()
	if err != nil {
		panic(fmt.Sprintf("RPGCOnfig: %s", err))
	}

	rpcClientRemote, err := ethclient.Dial(rpcConfig.Url)
	if err != nil {
		panic(fmt.Sprintf("ethclient.Dial: %s", err))
	}
	rpcClientLocal, err := ethclient.Dial("http://localhost:8545")
	if err != nil {
		panic(fmt.Sprintf("ethclient.Dial: %s", err))
	}

	// highest block number
	highestBlockRemote, err := rpcClientRemote.BlockNumber(ctx)
	if err != nil {
		panic(fmt.Sprintf("rpcClientRemote.BlockNumber: %s", err))
	}
	highestBlockLocal, err := rpcClientLocal.BlockNumber(ctx)
	if err != nil {
		panic(fmt.Sprintf("rpcClientLocal.BlockNumber: %s", err))
	}
	highestBlockNumber := highestBlockRemote
	if highestBlockLocal < highestBlockRemote {
		highestBlockNumber = highestBlockLocal
	}

	log.Warn("Starting block traces mismatch check", "highestBlockRemote", highestBlockRemote, "highestBlockLocal", highestBlockLocal, "working highestBlockNumber", highestBlockNumber)

	lowestBlockNumber := uint64(0)
	checkBlockNumber := highestBlockNumber

	var blockRemote, blockLocal *types.Block
	for i := lowestBlockNumber; i < checkBlockNumber; i += 100 {
		if i%10000 == 0 {
			log.Warn("Checking block", "blockNumber", i)
		}
		// get blocks
		blockRemote, blockLocal, err = getBlocks(ctx, rpcClientLocal, rpcClientRemote, i)
		if err != nil {
			log.Error(fmt.Sprintf("blockNum: %d, error getBlockTraces: %s", i, err))
			return
		}

		if blockRemote.Hash() != blockLocal.Hash() {
			log.Warn("Blocks mismatch", "blockNumber", i)
		}
		// if match := debug_tools.CompareBlocks(ctx, false, blockRemote, blockLocal, rpcClientLocal, rpcClientRemote); !match {
		// 	log.Warn("Mismatch found", "blockNum", i)
		// }
	}
	log.Warn("Check finished!")
}

func getBlocks(ctx context.Context, clientLocal, clientRemote *ethclient.Client, blockNum uint64) (*types.Block, *types.Block, error) {
	blockNumBig := new(big.Int).SetUint64(blockNum)
	blockLocal, err := clientLocal.BlockByNumber(ctx, blockNumBig)
	if err != nil {
		return nil, nil, fmt.Errorf("clientLocal.BlockByNumber: %s", err)
	}
	blockRemote, err := clientRemote.BlockByNumber(ctx, blockNumBig)
	if err != nil {
		return nil, nil, fmt.Errorf("clientRemote.BlockByNumber: %s", err)
	}
	return blockLocal, blockRemote, nil
}
