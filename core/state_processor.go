// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)
	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
	)

	// 获取到的当前区块所有的交易序列
	txsOri := block.Transactions()
	// 将 []*Transaction 类型转为 map[address][]*Transaction 类型，调用分组函数
	// txsChange := make(map[common.Address][]*types.Transaction)
	// txsChange[common.Address{114}] = txsOri

	// 调用分组函数
	txs := ClassifyTx(txsOri, signer)

	// 模拟分组结果
	// txs := make(map[int][]*types.Transaction) // ? txs中就是分好组的交易列表

	// 获取组数
	var RouNum int = 0
	for range txs {
		RouNum++
	}

	rouchan := make(chan MesReturn, RouNum) // 创建一个带缓冲的通道
	var wg sync.WaitGroup                   // 等待组
	var RouLineArr []*state.StateDB         // 存每个线程的stateDB

	// 拷贝stateDB
	for _, _ = range txs {
		eachStateDB := statedb.Copy()
		RouLineArr = append(RouLineArr, eachStateDB)
	}
	index := 0 // stateDB计数

	for _, value := range txs {
		// 新建AllMessage
		EachAllMessage := NewAllMessage(p.config, gp, RouLineArr[index], blockNumber, blockHash, usedGas, vmenv, signer, header)
		// key是组号(可能没有意义)，value是排好序的交易组
		go rouTest(value, &wg, rouchan, EachAllMessage, false) // 并行交易队列
		index++
	}

	wg.Wait() // 等待并行组执行结束

	// 提交stateDB状态到内存
	for i := 0; i < len(RouLineArr); i++ {
		RouLineArr[i].Finalise(true)
	}

	// 串行交易队列
	var SerialTxList []*types.Transaction

	// ? TODO: 是否需要加一个空返回值的判断？
	// ! 当你从管道中读取元素时，如果管道是空的，range循环将会阻塞
	// ! 你可能想要添加一个超时机制或者使用带有接收操作的for循环来确保你的程序不会无限期地等待
	for v := range rouchan {
		if !v.IsSuccess {
			fmt.Println("交易验证出错")
			// 验证出错，函数直接退出循环
			return nil, nil, 0, fmt.Errorf(v.ErrorMessage, v.Error)
		}
		fmt.Println("交易验证成功 bingo")
		receipts = append(receipts, v.newReceipt...)
		allLogs = append(allLogs, v.newAllLogs...)
		SerialTxList = append(SerialTxList, v.SingleTxList...)
		if len(rouchan) == 0 {
			fmt.Println("============ channel is end ============")
			break
		}
	}

	// 串行交易队列排序
	if len(SerialTxList) > 1 {
		SortSerialTX(SerialTxList)
	}
	// 串行队列的stateDB
	eachStateDB := statedb.Copy()

	// 新建AllMessage
	EachAllMessage := NewAllMessage(p.config, gp, eachStateDB, blockNumber, blockHash, usedGas, vmenv, signer, header)

	go rouTest(SerialTxList, &wg, rouchan, EachAllMessage, true) // TODO: 穿行交易队列

	for v := range rouchan {
		if !v.IsSuccess {
			fmt.Println("交易验证出错")
			// 验证出错，函数直接退出循环
			return nil, nil, 0, fmt.Errorf(v.ErrorMessage, v.Error)
		}
		receipts = append(receipts, v.newReceipt...)
		allLogs = append(allLogs, v.newAllLogs...)
		if len(rouchan) == 0 {
			fmt.Println("channel is end")
			break
		}
	}

	wg.Wait() // 等待并行组执行结束

	// 并行交易执行结束
	// 提交stateDB状态到内存
	eachStateDB.Finalise(true)

	// commit
	BlockHash, err := statedb.Commit(true)
	if err != nil {
		fmt.Print("commit函数出错")
	}

	fmt.Println(BlockHash) // TODO: 后续可能需要修改

	withdrawals := block.Withdrawals()
	if len(withdrawals) > 0 && !p.config.IsShanghai(block.Number(), block.Time()) {
		return nil, nil, 0, errors.New("withdrawals before shanghai")
	}
	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles(), withdrawals)

	return receipts, allLogs, *usedGas, nil
}

// TODO: 并行执行交易所需要的数据类型和方法

// SortSerialTX 串行队列排序方法
func SortSerialTX(SingleTxList []*types.Transaction) {
	// 使用 sort.Slice 排序函数对第二项进行排序
	sort.Slice(SingleTxList, func(i, j int) bool {
		// todo：1 Nonce从小到大
		if SingleTxList[i].Nonce() < SingleTxList[j].Nonce() {
			return true
		} else if SingleTxList[i].Nonce() == SingleTxList[j].Nonce() {
			// todo：2 GasPrice从大到小
			if SingleTxList[i].GasPrice().Cmp(SingleTxList[j].GasPrice()) == 1 {
				return true
			} else if SingleTxList[i].GasPrice().Cmp(SingleTxList[j].GasPrice()) == 0 {
				// todo：3 哈希值从小到大
				if SingleTxList[i].Hash().Less(SingleTxList[j].Hash()) {
					return true
				}
			}
		}
		return false
	})
}

// AllMessage 作为多线程函数的传入参数，以一个线程为单位
type AllMessage struct {
	Config      *params.ChainConfig
	Gp          *GasPool
	StateDB     *state.StateDB
	BlockNumber *big.Int
	BlockHash   common.Hash
	UsedGas     *uint64
	Vmenv       *vm.EVM
	Signer      types.Signer
	Header      *types.Header
}

// NewAllMessage todo : 新建，复制AllMessage结构体
func NewAllMessage(Config *params.ChainConfig, Gp *GasPool, StateDB *state.StateDB, BlockNumber *big.Int,
	BlockHash common.Hash, UsedGas *uint64, Vmenv *vm.EVM, Signer types.Signer, Header *types.Header) *AllMessage {
	AllMessage := &AllMessage{
		Config:      Config,
		Gp:          Gp,
		StateDB:     StateDB,
		BlockNumber: BlockNumber,
		BlockHash:   BlockHash,
		UsedGas:     UsedGas,
		Vmenv:       Vmenv,
		Signer:      Signer,
		Header:      Header,
	}
	return AllMessage
}

type MesReturn struct {
	// 交易出错了吗，false表示交易出错
	IsSuccess bool
	// 交易出错原因，字符串
	ErrorMessage string
	// todo:err内容
	Error error
	// 成功交易收据树
	newReceipt []*types.Receipt
	// logs
	newAllLogs []*types.Log
	// 串行队列
	SingleTxList []*types.Transaction
}

// TxGoroutine todo：线程池执行交易 TODO: 新增参数 IsSerial bool 表明当前交易队列是否是串行队列
func rouTest(txs []*types.Transaction, wg *sync.WaitGroup, RouChan chan MesReturn, message *AllMessage, IsSerial bool) {
	wg.Add(1)       // ? 进来前计数器+1，结束后计数器-1
	defer wg.Done() // 计数器-1

	// 处理成功交易收据树
	var newReceipt []*types.Receipt
	// logs
	var newAllLogs []*types.Log
	// 串行队列
	var SerialTxList []*types.Transaction

	for i := 0; i < len(txs); i++ {
		// TODO: 组装msg，msg包含了区块中所有的交易的信息，AccessList包含在msg中
		msg, err := TransactionToMessage(txs[i], message.Signer, message.Header.BaseFee, IsSerial) // 将交易转换为消息msg，包含交易的必要信息
		// 消息转换出错，直接返回
		if err != nil {
			var returnMsg = MesReturn{
				IsSuccess:    false,
				ErrorMessage: "could not turn to message tx" + txs[i].Hash().Hex(),
				Error:        err,
				newReceipt:   nil,
				newAllLogs:   nil,
				SingleTxList: nil,
			}
			RouChan <- returnMsg
			return // 验证只要出错就直接返回
		}

		// TODO: 执行交易
		receipt, err := applyTransaction(msg, message.Config, message.Gp, message.StateDB, message.BlockNumber, message.BlockHash, txs[i], message.UsedGas, message.Vmenv)
		// 验证时，交易执行出错则直接返回
		if err != nil {
			var returnMsg = MesReturn{
				IsSuccess:    false,
				ErrorMessage: "could not apply tx" + txs[i].Hash().Hex(),
				Error:        err,
				newReceipt:   nil,
				newAllLogs:   nil,
				SingleTxList: nil,
			}
			RouChan <- returnMsg
			return
		}

		// TODO: 串行队列需要更改原交易的AccessList，执行后的msg中的AccessList已经是最新的AccessList
		if IsSerial {
			judge := txs[i].ChangeAccessList(msg.AccessList) // 验证交易是否需要更改串行队列交易的AccessList TODO: 还需要讨论
			if !judge {
				fmt.Println("当前交易不存在AccessList，无法修改交易的AccessList")
			} else {
				fmt.Println("串行队列交易的AccessList修改成功")
			}
		}

		// 交易能否并行执行
		IsParallel := msg.IsParallel

		// todo:交易没有错，更新收据树和记录
		newReceipt = append(newReceipt, receipt)         // 收据树
		newAllLogs = append(newAllLogs, receipt.Logs...) // logs

		if IsParallel == false && IsSerial == false {
			// 串行交易队列
			SerialTxList = append(SerialTxList, txs[i])
		}
	}

	// 验证结束
	var returnMsg = MesReturn{
		IsSuccess:    true,
		ErrorMessage: "nil",
		Error:        nil,
		newReceipt:   newReceipt,
		newAllLogs:   newAllLogs,
		SingleTxList: SerialTxList,
	}
	RouChan <- returnMsg
}

func applyTransaction(msg *Message, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (*types.Receipt, error) {
	// 创建一个用于EVM环境的新上下文
	txContext := NewEVMTxContext(msg)
	evm.Reset(txContext, statedb)

	// 设置Hash
	statedb.SetTxContext(tx.Hash(), 0)

	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(evm, msg, gp)
	if result == nil { // 提前返回
		return nil, fmt.Errorf("空的result")
	}
	if result.Err != nil { // 执行出错
		return nil, result.Err
	}
	if err != nil { // 后续步骤出错
		return nil, err
	}

	// Update the state with pending changes.
	var root []byte
	if config.IsByzantium(blockNumber) {
		// TODO : Finalise重构了
		statedb.Finalise(true)
		// statedb.Finalise()
	} else {
		// CHANGE : 这里是计算中间状态，可以去掉
		// root = statedb.IntermediateRoot(config.IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex()) // TODO: no use
	return receipt, err
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
// TODO: worker.go的入口函数
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config, IsSerial bool) (*types.Receipt, error, bool) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee, IsSerial)
	if err != nil {
		return nil, err, true
	}
	// tx.ChangeAccessList(msg.AccessList)
	// Create a new context to be used in the EVM environment
	blockContext := NewEVMBlockContext(header, bc, author)
	vmenv := vm.NewEVM(blockContext, vm.TxContext{}, statedb, config, cfg)
	receipt, err := applyTransaction(msg, config, gp, statedb, header.Number, header.Hash(), tx, usedGas, vmenv)
	IsParallel := msg.IsParallel // 是否可以并行执行交易

	// TODO: 修改AccessList
	if IsSerial {
		// 串行队列交易需要修改AccessList
		judge := tx.ChangeAccessList(msg.AccessList)
		if !judge {
			fmt.Println("当前交易不存在AccessList，无法修改交易的AccessList")
		} else {
			fmt.Println("串行队列交易的AccessList修改成功")
		}
	}

	return receipt, err, IsParallel
}
