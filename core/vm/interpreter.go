// Copyright 2014 The go-ethereum Authors
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

package vm

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

// Config are the configuration options for the Interpreter
type Config struct {
	Tracer                  EVMLogger // Opcode logger
	NoBaseFee               bool      // Forces the EIP-1559 baseFee to 0 (needed for 0 price calls)
	EnablePreimageRecording bool      // Enables recording of SHA3/keccak preimages
	ExtraEips               []int     // Additional EIPS that are to be enabled
}

// ScopeContext contains the things that are per-call, such as stack and memory,
// but not transients like pc and gas
type ScopeContext struct {
	Memory   *Memory
	Stack    *Stack
	Contract *Contract
}

// EVMInterpreter represents an EVM interpreter
type EVMInterpreter struct {
	evm   *EVM
	table *JumpTable

	hasher    crypto.KeccakState // Keccak256 hasher instance shared across opcodes
	hasherBuf common.Hash        // Keccak256 hasher result array shared aross opcodes

	readOnly   bool   // Whether to throw on stateful modifications
	returnData []byte // Last CALL's return data for subsequent reuse
}

// NewEVMInterpreter returns a new instance of the Interpreter.
func NewEVMInterpreter(evm *EVM) *EVMInterpreter {
	// If jump table was not initialised we set the default one.
	var table *JumpTable
	switch {
	case evm.chainRules.IsCancun:
		table = &cancunInstructionSet
	case evm.chainRules.IsShanghai:
		table = &shanghaiInstructionSet
	case evm.chainRules.IsMerge:
		table = &mergeInstructionSet
	case evm.chainRules.IsLondon:
		table = &londonInstructionSet
	case evm.chainRules.IsBerlin:
		table = &berlinInstructionSet
	case evm.chainRules.IsIstanbul:
		table = &istanbulInstructionSet
	case evm.chainRules.IsConstantinople:
		table = &constantinopleInstructionSet
	case evm.chainRules.IsByzantium:
		table = &byzantiumInstructionSet
	case evm.chainRules.IsEIP158:
		table = &spuriousDragonInstructionSet
	case evm.chainRules.IsEIP150:
		table = &tangerineWhistleInstructionSet
	case evm.chainRules.IsHomestead:
		table = &homesteadInstructionSet
	default:
		table = &frontierInstructionSet
	}
	var extraEips []int
	if len(evm.Config.ExtraEips) > 0 {
		// Deep-copy jumptable to prevent modification of opcodes in other tables
		table = copyJumpTable(table)
	}
	for _, eip := range evm.Config.ExtraEips {
		if err := EnableEIP(eip, table); err != nil {
			// Disable it, so caller can check if it's activated or not
			log.Error("EIP activation failed", "eip", eip, "error", err)
		} else {
			extraEips = append(extraEips, eip)
		}
	}
	evm.Config.ExtraEips = extraEips
	return &EVMInterpreter{evm: evm, table: table}
}

// Run函数：执行智能合约代码
// 返回值中后两项为智能合约实际执行中访问到的真实AccessList，以及是否需要将当前交易放到串行队列中，如果IsParallel是false则表示当前交易不能并行执行，需要放到串行队列中
// TODO: 传入参数增加TrueAccessList *JudgeAccessList，表示实际获取到的AL
// TODO: 传入参数增加 IsSerial bool，表示是否是串行队列，如果是true则表示当前是串行队列，遇到AL不一样的不再退出
func (in *EVMInterpreter) Run(contract *Contract, input []byte, readOnly bool, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, err error, IsParallel bool) {
	// 增加合约调用深度，限制为1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// 确保只有在我们还没有进入只读状态时才设置readOnly。这也确保了子调用的readOnly标志没有被移除。
	if readOnly && !in.readOnly {
		in.readOnly = true // 将上一级调用的readOnly改为true
		defer func() { in.readOnly = false }()
	}

	// 重置前一个调用的返回数据。保留旧的缓冲区并不重要，因为每次返回调用无论如何都会返回新数据。
	in.returnData = nil

	// 如果没有代码，就不要为执行而烦恼。
	if len(contract.Code) == 0 {
		return nil, nil, true // 没有代码，一定可以并行运行，因此IsParallel设置为true
	}

	// 定义变量
	var (
		op          OpCode        // 当前的操作码
		mem         = NewMemory() // 绑定的内存
		stack       = newstack()  // 本地堆栈
		callContext = &ScopeContext{
			Memory:   mem,
			Stack:    stack,
			Contract: contract,
		}
		pc   = uint64(0) // 程序计数器，初始化为0
		cost uint64
		// 跟踪器使用的副本
		pcCopy  uint64                        // 延迟的EVMLogger所需的？
		gasCopy uint64                        // 用于EVMLogger在执行前记录剩余汽油费
		logged  bool                          // EVMLogger应该忽略已经记录的步骤
		res     []byte                        // 操作码执行函数的结果
		debug   = in.evm.Config.Tracer != nil // 是否有追踪器，用于debug
	)

	// capturestate在堆栈返回到池之前需要堆栈，不要移动这个函数的位置
	defer func() {
		returnStack(stack)
	}()

	// 复制合约输入内容
	contract.Input = input

	if debug {
		defer func() {
			if err != nil {
				if !logged {
					in.evm.Config.Tracer.CaptureState(pcCopy, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err)
				} else {
					in.evm.Config.Tracer.CaptureFault(pcCopy, op, gasCopy, cost, callContext, in.evm.depth, err)
				}
			}
		}()
	}

	// TrueAccessList是总的AccessList，每个操作获得的真实TAL都会汇总到TrueAccessList中
	IsParallel = true
	// TODO: 如果不能执行并行操作，则直接将当前交易放到串行队列，退出run函数

	for {
		// 每一个操作的AccessList
		PartTrueAccessList := NewJudgeAccessList()
		if debug {
			// 捕获用于跟踪的预执行值
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}
		// 从跳转表中获取操作并验证堆栈，以确保有足够的堆栈项可用于执行操作
		op = contract.GetOp(pc)      // 获取第一个opcode
		operation := in.table[op]    // 查询对应的op，operation应该是个函数
		cost = operation.constantGas // 记录第一步操作消耗的汽油费，追踪器用到
		// 验证堆栈
		if sLen := stack.len(); sLen < operation.minStack { // 栈的深度小于操作函数的最小栈深度
			return nil, &ErrStackUnderflow{stackLen: sLen, required: operation.minStack}, true // todo:暂时设置为true
		} else if sLen > operation.maxStack { // 栈的深度大于操作函数的最大栈深度
			return nil, &ErrStackOverflow{stackLen: sLen, limit: operation.maxStack}, true // todo:暂时设置为true
		}
		if !contract.UseGas(cost) { // 扣除该函数操作对应的汽油费，之前通过transfer函数向合约转账
			return nil, ErrOutOfGas, true // todo:暂时设置为true
		}
		if operation.dynamicGas != nil { // 所有使用动态内存的操作也有动态汽油费成本
			var memorySize uint64
			// 计算新的内存大小并扩展内存以适合操作
			// 内存检查需要在评估动态汽油费部分之前完成，以检测计算溢出
			if operation.memorySize != nil {
				memSize, overflow := operation.memorySize(stack)
				if overflow {
					return nil, ErrGasUintOverflow, true // todo:暂时设置为true
				}
				// 内存扩展为32字节的字。汽油费也是用文字来计算的。
				if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
					return nil, ErrGasUintOverflow, true // todo:暂时设置为true
				}
			}
			// 消耗汽油并返回错误，如果没有足够的气体可用。 显式设置成本，以便捕获状态延迟方法可以获得适当的成本
			var dynamicCost uint64
			// !!!!!!!!!!!!!!!!!!!!!! may error
			if op == SSTORE {
				fmt.Println("Let me see AL")
			}
			dynamicCost, err = operation.dynamicGas(in.evm, contract, stack, mem, memorySize) // 计算动态内存消耗的汽油费
			cost += dynamicCost                                                               // for tracing
			if err != nil || !contract.UseGas(dynamicCost) {                                  // 合约余额不足
				return nil, ErrOutOfGas, true // todo:暂时设置为true
			}
			// Do tracing before memory expansion
			if debug {
				in.evm.Config.Tracer.CaptureState(pc, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err) // 更新accessList
				logged = true                                                                                           // 记录日志
			}
			if memorySize > 0 { // 重置动态内存
				mem.Resize(memorySize)
			}
		} else if debug {
			in.evm.Config.Tracer.CaptureState(pc, op, gasCopy, cost, callContext, in.returnData, in.evm.depth, err) // 更新accessList
			logged = true                                                                                           // 记录日志
		}

		// 获取到本次操作实际调用的AccessList
		PartTrueAccessList.GetTrueAccessList(op, callContext)

		// 合并AL
		// TODO: TrueAccessList 是一个指针，应该在run函数参数内传入
		// 我们明确一点，并行组执行的时候不会更改AL，出错了直接退出回滚，把不能并行的交易放在串行队列中，只有串行队列中每个交易才更改AL
		// 但是串行队列更改AL也不是在run函数中修改，run函数需要传入是否是串行的参数，作用在于遇到AL不一样的不再退出
		isOk := TrueAccessList.CombineTrueAccessList(PartTrueAccessList)
		if !isOk {
			fmt.Println("合并错误") // TODO: 需要后续处理
		}

		// TODO: 判断是否有超出的部分，只有超过且并行队列才将交易放到串行队列中
		if IsSerial == false { // 只在并行队列判断即可
			result, _, _, _ := PartTrueAccessList.ConflictDetection(in.evm.StateDB.GetAccessList())
			// ! fasle => AL发生了冲突，放入串行队列，true => 当前所有AL都符合要求，交易可以并行执行
			if !result && !IsSerial {
				IsParallel = false // 当前交易无法并行执行
			}
		}

		// 实际执行操作 TODO: 执行操作条件：可以并行执行 IsParallel = true，或者属于串行队列 IsSerial = true
		if IsParallel || IsSerial {
			// TODO: 暂时设置，如果是opCreate和opCreate2则之间放入串行队列
			if op == CREATE || op == CREATE2 {
				IsParallel = false
				if IsSerial {
					res, err, _ = operation.executeAL(&pc, in, callContext, TrueAccessList, IsSerial)
				}
				// 不是串行队列的Create函数不执行
			} else if op == DELEGATECALL || op == CALL || op == STATICCALL || op == CALLCODE {
				// TODO: 只有这四个操作会继续调用run函数，因此传入TrueAccessList和IsSerial，返回是否可以并行执行
				res, err, IsParallel = operation.executeAL(&pc, in, callContext, TrueAccessList, IsSerial)
			} else {
				// TODO: 其它操作不会调用run函数，也就不会改变TrueAccessList和IsSerial
				res, err = operation.execute(&pc, in, callContext)
			}
		}

		if err != nil {
			break
		}
		// TODO: 退出run函数判断依据：非串行队列 IsSerial = false，且不能并行 IsParallel = false
		if IsParallel == false && IsSerial == false {
			break
		}
		pc++ // 程序计数器递增，进行下一段代码的相关操作
	}

	if err == errStopToken {
		err = nil // clear stop token error
	}

	return res, err, IsParallel // 修改TrueAccessList只需要在串行队列完成就行
}
