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
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
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
		// vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg) // ! ERROR
		signer = types.MakeSigner(p.config, header.Number, header.Time)
	)

	// 获取到的当前区块所有的交易序列
	txsOri := block.Transactions()

	// 调用分组函数
	txs := ClassifyTx(txsOri, signer)

	// 获取组数
	var RouNum int = 0
	for range txs {
		RouNum++
	}

	rouchan := make(chan MesReturn, RouNum) // 创建一个带缓冲的通道，并行
	rouchan1 := make(chan MesReturn, 1)     // 创建一个带缓冲的通道，串行
	var wg sync.WaitGroup                   // 等待组，并行
	var wg1 sync.WaitGroup                  // 等待组，串行
	var RouStatedbArr []*state.StateDB      // 存每个线程的stateDB
	var RouEvmArr []*vm.EVM                 // 存每个线程的evm，stateDB最终会赋值到evm中

	// 拷贝stateDB与evm
	for range txs {
		eachStateDB := statedb.Copy()
		eachEvm := vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		RouStatedbArr = append(RouStatedbArr, eachStateDB)
		RouEvmArr = append(RouEvmArr, eachEvm)
		wg.Add(1) // 计数器+1 // ! 防止出现第二个线程还没+1的时候第一个线程已经执行完的情况
	}
	index := 0 // stateDB计数

	for _, value := range txs {
		// 新建AllMessage
		EachAllMessage := NewAllMessage(p.config, gp, RouStatedbArr[index], blockNumber, blockHash, usedGas, RouEvmArr[index], signer, header)
		// key是组号(可能没有意义)，value是排好序的交易组
		go rouTest(value, &wg, rouchan, EachAllMessage, false, index) // 并行交易队列
		index++
	}

	wg.Wait() // 等待并行组执行结束

	// 提交stateDB状态到内存
	for i := 0; i < len(RouStatedbArr); i++ {
		RouStatedbArr[i].Finalise(true)
		// 合并obj
		spo, so := RouStatedbArr[i].GetPendingObj()
		statedb.SetPendingObj(spo)
		statedb.UpdateStateObj(so)
	}

	// 串行交易队列
	var SerialTxList []*types.Transaction

	if len(rouchan) == 0 {
		fmt.Println("ERROR 并行线程返回通道为空")
		return nil, nil, 0, fmt.Errorf("ERROR 并行线程返回通道为空")
	} else {
		for v := range rouchan {
			if !v.IsSuccess {
				fmt.Println("交易验证出错，验证失败")
				// 验证出错，函数直接退出循环
				return nil, nil, 0, fmt.Errorf(v.ErrorMessage, v.Error)
			}
			fmt.Println("Congratulations 一组并行交易验证成功")
			receipts = append(receipts, v.newReceipt...)
			allLogs = append(allLogs, v.newAllLogs...)
			SerialTxList = append(SerialTxList, v.SingleTxList...)
			if len(rouchan) == 0 {
				fmt.Println("并行 channel返回值处理完成")
				break
			}
		}
	}

	// ? 串行交易队列排序
	if len(SerialTxList) > 1 {
		fmt.Println("串行队列中存在交易")
		SortSerialTX(SerialTxList)

		// 串行队列的stateDB // ! 直接用stateDB就行
		// eachStateDB := statedb.Copy()

		// 拷贝evm
		eachEvm := vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		// 新建AllMessage
		EachAllMessage := NewAllMessage(p.config, gp, statedb, blockNumber, blockHash, usedGas, eachEvm, signer, header)

		wg1.Add(1)                                                         // 计数器+1
		go rouTest(SerialTxList, &wg1, rouchan1, EachAllMessage, true, -1) // TODO: 穿行交易队列

		wg1.Wait() // 等待串行组执行结束

		for v := range rouchan1 {
			if !v.IsSuccess {
				fmt.Println("交易验证出错，验证失败")
				// 验证出错，函数直接退出循环
				return nil, nil, 0, fmt.Errorf(v.ErrorMessage, v.Error)
			} else {
				fmt.Println("Congratulations 一组串行交易验证成功")
			}
			receipts = append(receipts, v.newReceipt...)
			allLogs = append(allLogs, v.newAllLogs...)
			if len(rouchan1) == 0 { // 串行组这里管道长度为0
				fmt.Println("串行 channel返回值处理完成")
				break
			}
		}

		// 并行交易执行结束
		// 提交stateDB状态到内存
		statedb.Finalise(true) // ? 没有copy所以不用继续合并
	}

	// commit
	BlockHash, err := statedb.Commit(true)
	if err != nil {
		fmt.Print("commit函数出错")
		return nil, nil, 0, fmt.Errorf("commit函数出错", err)
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
func rouTest(txs []*types.Transaction, wg *sync.WaitGroup, RouChan chan MesReturn, message *AllMessage, IsSerial bool, id int) {
	// ! TODO: 注意 管道中存储的是一个组的运行结果，不是一个交易的运行结果

	defer wg.Done() // 计数器-1
	id_str := strconv.Itoa(id)
	if id == -1 {
		id_str = "main"
	}
	var result = "In thread: " + id_str + " the stateDB is: {\n"
	for _, stobj := range message.StateDB.GetStateObj() {
		result += "\t{\n"
		result += "\t\tAddress: " + stobj.Address().Hex() + ",\n"
		nonce_str := strconv.Itoa(int(stobj.Nonce()))
		result += "\t\tNonce: " + nonce_str + ",\n"
		result += "\t}\n"
	}
	result += "}\n"
	if id == -1 {
		fmt.Println(result)
	}

	result = "In thread: " + id_str + " the txs are: {\n"
	for _, tx := range txs {
		result += "\t{\n"
		from, _ := message.Signer.Sender(tx)
		result += "\t\tFrom: " + from.Hex() + ",\n"
		nonce_str := strconv.Itoa(int(tx.Nonce()))
		result += "\t\tNonce: " + nonce_str + ",\n"
		result += "\t\tTo: " + tx.To().Hex() + ",\n"
		result += "\t}\n"
	}
	result += "}\n"
	//	fmt.Println(result)

	// 处理成功交易收据树
	var newReceipt []*types.Receipt
	// logs
	var newAllLogs []*types.Log
	// 串行队列
	var SerialTxList []*types.Transaction
	// 是否出错
	var IsErr bool
	IsErr = true
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
			// 验证只要出错就直接返回
			IsErr = true
			break
		}

		// 交易执行前打印相关信息
		result = "In thread: " + id_str + " executing tx: {\n"
		result += "\t{\n"
		result += "\t\tFrom: " + msg.From.Hex() + ",\n"
		result += "\t\tTo: " + msg.To.Hex() + ",\n"
		result += "\t\tNonce: " + strconv.Itoa(int(msg.Nonce)) + ",\n"
		result += "\t\tAccessList: {"
		for _, v := range msg.AccessList {
			result += v.Address.Hex() + ", "
		}
		result += "},\n"
		result += "\t}\n"
		result += "}\n"
		fmt.Println(result)

		// TODO: 执行交易
		receipt, err := applyTransaction(msg, message.Config, message.Gp, message.StateDB, message.BlockNumber, message.BlockHash, txs[i], message.UsedGas, message.Vmenv)

		// 并行交易，无法并行执行 => 放到串行组，不报错
		if !msg.IsParallel && !msg.IsSerial {
			result = "In thread: " + id_str + " cannot put into parallel queue while exeuting tx: {\n"
			result += "\t{\n"
			result += "\t\tFrom: " + msg.From.Hex() + ",\n"
			result += "\t\tTo: " + msg.To.Hex() + ",\n"
			result += "\t\tNonce: " + strconv.Itoa(int(msg.Nonce)) + ",\n"
			result += "\t}\n"
			result += "}\n"
			fmt.Println(result)
		}

		// 不报错，交易可以并行或者交易在串行队列 => 成功执行
		if err == nil && (msg.IsParallel || msg.IsSerial) {
			result = "In thread: " + id_str + " successfully execute tx: {\n"
			result += "\t{\n"
			result += "\t\tFrom: " + msg.From.Hex() + ",\n"
			result += "\t\tTo: " + msg.To.Hex() + ",\n"
			result += "\t\tNonce: " + strconv.Itoa(int(msg.Nonce)) + ",\n"
			result += "\t}\n"
			result += "}\n"
			result += "The new State is: {\n"
			result += "\t{\n"
			result += "\t\tAddress: " + msg.From.Hex() + ",\n"
			if message.StateDB.GetNonce(msg.From) == 0 {
				fmt.Println("????")
			}
			nonce_str := strconv.Itoa(int(message.StateDB.GetNonce(msg.From)))
			result += "\t\tNonce: " + nonce_str + ",\n"
			result += "\t}\n"
			result += "}\n"
			fmt.Println(result)
		}

		// 验证时，交易执行出错则直接返回
		if err != nil {
			result = "In thread: " + id_str + " Fatal error while exeuting tx: {\n"
			result += "\t{\n"
			result += "\t\tFrom: " + msg.From.Hex() + ",\n"
			result += "\t\tTo: " + msg.To.Hex() + ",\n"
			result += "\t\tNonce: " + strconv.Itoa(int(msg.Nonce)) + ",\n"
			result += "\t}\n"
			result += "}\n"
			fmt.Println(result)

			var returnMsg = MesReturn{
				IsSuccess:    false,
				ErrorMessage: "could not apply tx" + txs[i].Hash().Hex(),
				Error:        err,
				newReceipt:   nil,
				newAllLogs:   nil,
				SingleTxList: nil,
			}
			RouChan <- returnMsg
			// return
			IsErr = true
			break
		}

		IsErr = false

		// 到这里有err的已经返回了，所以不需要额外判断
		// TODO: 串行队列需要更改原交易的AccessList，执行后的msg中的AccessList已经是最新的AccessList
		if IsSerial {
			judge := txs[i].ChangeAccessList(msg.AccessList) // 已检查
			if !judge {
				fmt.Println("当前交易不存在AccessList，无法修改交易的AccessList") // pankutx不存在这个判断
			} else {
				fmt.Println("串行队列交易的AccessList修改成功")
			}
		}

		// 交易能否并行执行
		IsParallel := msg.IsParallel

		// 交易没有错，只是要被踢出当前队列，放入串行队列
		if IsParallel == false && IsSerial == false {
			// 串行交易队列
			SerialTxList = append(SerialTxList, txs[i])
			tempAddr, _ := message.Signer.Sender(txs[i])

			// todo 将同一address的交易全部去除
			t := i + 1
			for ; t < len(txs); t++ {
				ta, _ := message.Signer.Sender(txs[t])
				if bytes.Compare(tempAddr[:], ta[:]) == 0 {
					SerialTxList = append(SerialTxList, txs[t])
				} else {
					break
				}
			}

			txs = txs[t:]
			i = -1 // i 重置
		} else {
			newReceipt = append(newReceipt, receipt)         // 收据树
			newAllLogs = append(newAllLogs, receipt.Logs...) // logs
		}
	}

	if !IsErr {
		// 验证结束，所有交易都没有报错（包括需要放到串行队列里的交易）
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
		if err == nil {
			fmt.Println("这笔交易没法在当前队列中执行")
		}
		return nil, err
	}
	if result.Err != nil { // 执行出错
		fmt.Println("虚拟机中交易执行出错")
		return nil, result.Err
	}

	if msg.IsSerial == false && msg.IsParallel == false {
		// 并行队列无法串行执行，无需执行后续步骤
		return nil, nil
	} else {
		// Update the state with pending changes.
		var root []byte
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
