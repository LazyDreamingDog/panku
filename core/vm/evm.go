package vm

import (
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// emptyCodeHash is used by create to ensure deployment is disallowed to already
// deployed contract addresses (relevant after the account abstraction).
var emptyCodeHash = crypto.Keccak256Hash(nil)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc func(StateDB, common.Address, *big.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc func(StateDB, common.Address, common.Address, *big.Int)
	// GetHashFunc returns the n'th block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

func (evm *EVM) precompile(addr common.Address) (PrecompiledContract, bool) {
	var precompiles map[common.Address]PrecompiledContract
	switch {
	case evm.chainRules.IsCancun:
		precompiles = PrecompiledContractsCancun
	case evm.chainRules.IsBerlin:
		precompiles = PrecompiledContractsBerlin
	case evm.chainRules.IsIstanbul:
		precompiles = PrecompiledContractsIstanbul
	case evm.chainRules.IsByzantium:
		precompiles = PrecompiledContractsByzantium
	default:
		precompiles = PrecompiledContractsHomestead
	}
	p, ok := precompiles[addr]
	return p, ok
}

// BlockContext provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type BlockContext struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        uint64         // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
	BaseFee     *big.Int       // Provides information for BASEFEE
	Random      *common.Hash   // Provides information for PREVRANDAO
}

// TxContext provides the EVM with information about a transaction.
// All fields can change between transactions.
type TxContext struct {
	// Message information
	Origin     common.Address // Provides information for ORIGIN
	GasPrice   *big.Int       // Provides information for GASPRICE
	BlobHashes []common.Hash  // Provides information for BLOBHASH
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context BlockContext
	TxContext
	// StateDB gives access to the underlying state
	StateDB StateDB
	// Depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig
	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules
	// virtual machine configuration options used to initialise the
	// evm.
	Config Config
	// global (to this context) ethereum virtual machine
	// used throughout the execution of the tx.
	interpreter *EVMInterpreter
	// abort is used to abort the EVM calling operations
	abort atomic.Bool
	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64
}

// NewEVM returns a new EVM. The returned EVM is not thread safe and should
// only ever be used *once*.
func NewEVM(blockCtx BlockContext, txCtx TxContext, statedb StateDB, chainConfig *params.ChainConfig, config Config) *EVM {
	evm := &EVM{
		Context:     blockCtx,
		TxContext:   txCtx,
		StateDB:     statedb,
		Config:      config,
		chainConfig: chainConfig,
		chainRules:  chainConfig.Rules(blockCtx.BlockNumber, blockCtx.Random != nil, blockCtx.Time),
	}
	evm.interpreter = NewEVMInterpreter(evm)
	return evm
}

// Reset resets the EVM with a new transaction context.Reset
// This is not threadsafe and should only be done very cautiously.
func (evm *EVM) Reset(txCtx TxContext, statedb StateDB) {
	evm.TxContext = txCtx
	evm.StateDB = statedb
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	evm.abort.Store(true)
}

// Cancelled returns true if Cancel has been called
func (evm *EVM) Cancelled() bool {
	return evm.abort.Load()
}

// Interpreter returns the current interpreter
func (evm *EVM) Interpreter() *EVMInterpreter {
	return evm.interpreter
}

// SetBlockContext updates the block context of the EVM.
func (evm *EVM) SetBlockContext(blockCtx BlockContext) {
	evm.Context = blockCtx
	num := blockCtx.BlockNumber
	timestamp := blockCtx.Time
	evm.chainRules = evm.chainConfig.Rules(num, blockCtx.Random != nil, timestamp)
}

// Call以给定的输入作为参数，执行与addr相关联的合约
// 它还处理所需的任何必要的值转移，并采取必要的步骤来创建帐户，并在执行错误或值转移失败的情况下逆转状态。
func (evm *EVM) Call(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, leftOverGas uint64, err error, IsParallel bool) {
	// 如果我们试图在调用深度限制之上执行，则失败
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth, true
	}
	// 如果我们试图转移超过可用余额，则失败
	if value.Sign() != 0 && !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance, true
	}
	snapshot := evm.StateDB.Snapshot()
	p, isPrecompile := evm.precompile(addr)
	debug := evm.Config.Tracer != nil

	// TODO: 检测目的地地址是否存在
	if !evm.StateDB.Exist(addr) { // 目的地址不存在，则直接退出
		if !isPrecompile && evm.chainRules.IsEIP158 && value.Sign() == 0 {
			// Call一个不存在的账户，什么都不做，只是ping追踪器
			if debug {
				if evm.depth == 0 {
					evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
					evm.Config.Tracer.CaptureEnd(ret, 0, nil)
				} else {
					evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
					evm.Config.Tracer.CaptureExit(ret, 0, nil)
				}
			}
			err = fmt.Errorf("目标地址不存在")
			return nil, gas, err, true
		}
		evm.StateDB.CreateAccount(addr) // 创建账户
	}

	// 转帐前已经分组，因此这一步不再需要额外判断
	evm.Context.Transfer(evm.StateDB, caller.Address(), addr, value) // 执行转账交易	TODO: 后续考虑并行转账该怎么处理

	// 在调试模式下捕获跟踪程序启动/结束事件
	if debug {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
			defer func(startGas uint64) { // Lazy evaluation of the parameters
				evm.Config.Tracer.CaptureEnd(ret, startGas-gas, err)
			}(gas)
		} else {
			// Handle tracer events for entering and exiting a call frame
			evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
			defer func(startGas uint64) {
				evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
			}(gas)
		}
	}

	// TODO: 预编译合约，后续可能需要修改
	if isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas)
	} else {
		// 不是预编译合约，初始化一个新的合约，并设置EVM要使用的代码
		code := evm.StateDB.GetCode(addr)
		if len(code) == 0 {
			ret, err = nil, nil // 合约没有代码则直接返回，油费没有变化
		} else {
			addrCopy := addr
			// 如果账户没有密码，我们可以在这里中止
			// 深度检查已经完成，并在上面处理预编译
			contract := NewContract(caller, AccountRef(addrCopy), value, gas)
			contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), code)
			// 执行智能合约功能
			ret, err, IsParallel = evm.interpreter.Run(contract, input, false, TrueAccessList, IsSerial)
			gas = contract.Gas
		}
	}
	// 当EVM返回错误或设置上面的创建代码时，我们将恢复到快照并消耗剩余的gas
	// 此外，当我们在homestead时，这也会计算代码存储气体错误。
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			gas = 0
		}
	} else if IsSerial { // 交易执行不出错，且为串行队列则更改AccessList
		// TODO: 替换stateDB中的AccessList
		StateAccessList := evm.StateDB.GetAccessList() // stateDB的AL
		StateAccessList.Addresses = TrueAccessList.Addresses
		StateAccessList.Slots = TrueAccessList.Slots
		// TODO: 后续stateDB中可能会有一项新的AccessList类型，到时候需要将真实的AccessList存到对应位置
	}

	// TODO: 新增一种快照回滚条件，当在并行组执行交易时，该交易无法并行执行则也需要进行快照回滚
	if err == nil && IsSerial == false && IsParallel == false {
		fmt.Println("并行组中交易无法并行执行")
		evm.StateDB.RevertToSnapshot(snapshot) // 快照回滚
	}

	return ret, gas, err, IsParallel
}

// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
// TODO: New Change 传入参数增加TrueAccessList *JudgeAccessList，表示实际获取到的AL
// TODO: New Change 传入参数增加 IsSerial bool，表示是否是串行队列，如果是true则表示当前是串行队列，遇到AL不一样的不再退出
func (evm *EVM) CallCode(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, leftOverGas uint64, err error, IsParallel bool) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		IsParallel = true // 没有执行
		return nil, gas, ErrDepth, IsParallel
	}
	// Fail if we're trying to transfer more than the available balance
	// Note although it's noop to transfer X ether to caller itself. But
	// if caller doesn't have enough balance, it would be an error to allow
	// over-charging itself. So the check here is necessary.
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		IsParallel = true // TODO: 需要再想想
		return nil, gas, ErrInsufficientBalance, IsParallel
	}
	var snapshot = evm.StateDB.Snapshot()

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.Config.Tracer.CaptureEnter(CALLCODE, caller.Address(), addr, input, gas, value)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas)
	} else {
		addrCopy := addr
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, AccountRef(caller.Address()), value, gas)
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		// TODO: 传入TrueAccessList与IsSerial
		ret, err, IsParallel = evm.interpreter.Run(contract, input, false, TrueAccessList, IsSerial)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			gas = 0
		}
	}

	// TODO: 新增一种快照回滚条件，当在并行组执行交易时，该交易无法并行执行则也需要进行快照回滚
	if err == nil && IsSerial == false && IsParallel == false {
		fmt.Println("并行组中交易无法并行执行")
		evm.StateDB.RevertToSnapshot(snapshot) // 快照回滚
	}

	return ret, gas, err, IsParallel
}

// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(caller ContractRef, addr common.Address, input []byte, gas uint64, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, leftOverGas uint64, err error, IsParallel bool) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		IsParallel = true
		return nil, gas, ErrDepth, IsParallel
	}
	var snapshot = evm.StateDB.Snapshot()

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		// NOTE: caller must, at all times be a contract. It should never happen
		// that caller is something other than a Contract.
		parent := caller.(*Contract)
		// DELEGATECALL inherits value from parent call
		evm.Config.Tracer.CaptureEnter(DELEGATECALL, caller.Address(), addr, input, gas, parent.value)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas)
		IsParallel = true // TODO: 预编译合约假设都可以并行执行
	} else {
		addrCopy := addr
		// Initialise a new contract and make initialise the delegate values
		contract := NewContract(caller, AccountRef(caller.Address()), nil, gas).AsDelegate()
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		ret, err, IsParallel = evm.interpreter.Run(contract, input, false, TrueAccessList, IsSerial)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			gas = 0
		}
	}

	// TODO: 新增一种快照回滚条件，当在并行组执行交易时，该交易无法并行执行则也需要进行快照回滚
	if err == nil && IsSerial == false && IsParallel == false {
		fmt.Println("并行组中交易无法并行执行")
		evm.StateDB.RevertToSnapshot(snapshot) // 快照回滚
	}

	return ret, gas, err, IsParallel
}

// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
func (evm *EVM) StaticCall(caller ContractRef, addr common.Address, input []byte, gas uint64, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, leftOverGas uint64, err error, IsParallel bool) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth, true
	}
	// We take a snapshot here. This is a bit counter-intuitive, and could probably be skipped.
	// However, even a staticcall is considered a 'touch'. On mainnet, static calls were introduced
	// after all empty accounts were deleted, so this is not required. However, if we omit this,
	// then certain tests start failing; stRevertTest/RevertPrecompiledTouchExactOOG.json.
	// We could change this, but for now it's left for legacy reasons
	var snapshot = evm.StateDB.Snapshot()

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, big0)

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.Config.Tracer.CaptureEnter(STATICCALL, caller.Address(), addr, input, gas, nil)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunPrecompiledContract(p, input, gas)
	} else {
		// At this point, we use a copy of address. If we don't, the go compiler will
		// leak the 'contract' to the outer scope, and make allocation for 'contract'
		// even if the actual execution ends on RunPrecompiled above.
		addrCopy := addr
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, AccountRef(addrCopy), new(big.Int), gas)
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		// When an error was returned by the EVM or when setting the creation code
		// above we revert to the snapshot and consume any gas remaining. Additionally
		// when we're in Homestead this also counts for code storage gas errors.
		ret, err, IsParallel = evm.interpreter.Run(contract, input, true, TrueAccessList, IsSerial)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			gas = 0
		}
	}

	// TODO: 新增一种快照回滚条件，当在并行组执行交易时，该交易无法并行执行则也需要进行快照回滚
	if err == nil && IsSerial == false && IsParallel == false {
		fmt.Println("并行组中交易无法并行执行")
		evm.StateDB.RevertToSnapshot(snapshot) // 快照回滚
	}

	return ret, gas, err, IsParallel
}

type codeAndHash struct {
	code []byte
	hash common.Hash
}

func (c *codeAndHash) Hash() common.Hash {
	if c.hash == (common.Hash{}) {
		c.hash = crypto.Keccak256Hash(c.code)
	}
	return c.hash
}

// create函数：创建智能合约
// TODO: 传入参数增加 TAL 和 IsSerial，返回参数增加 IsParallel
func (evm *EVM) create(caller ContractRef, codeAndHash *codeAndHash, gas uint64, value *big.Int,
	address common.Address, typ OpCode, TrueAccessList *JudgeAccessList, IsSerial bool) ([]byte, common.Address, uint64, error, bool) {
	// 检查调用深度，CallCreateDepth uint64 = 1024 一个合约最多调用1024个合约
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth, true
	}
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance, true
	}
	nonce := evm.StateDB.GetNonce(caller.Address())
	if nonce+1 < nonce {
		return nil, common.Address{}, gas, ErrNonceUintOverflow, true
	}
	evm.StateDB.SetNonce(caller.Address(), nonce+1)
	// 我们在获取快照之前将其添加到访问列表中。即使创造失败了，访问列表的更改不应该回滚
	if evm.chainRules.IsBerlin {
		evm.StateDB.AddAddressToAccessList(address)
	}

	// 确保在指定的地址没有现有的合约
	contractHash := evm.StateDB.GetCodeHash(address)
	if evm.StateDB.GetNonce(address) != 0 || (contractHash != (common.Hash{}) && contractHash != emptyCodeHash) {
		return nil, common.Address{}, 0, ErrContractAddressCollision, true
	}

	// 在stateDB中创建一个新账户
	snapshot := evm.StateDB.Snapshot() // 保存快照
	evm.StateDB.CreateAccount(address)
	if evm.chainRules.IsEIP158 {
		evm.StateDB.SetNonce(address, 1)
	}

	// 转帐前已经分组，因此这一步不再需要额外判断
	evm.Context.Transfer(evm.StateDB, caller.Address(), address, value)

	// 初始化一个新的合约
	contract := NewContract(caller, AccountRef(address), value, gas)
	contract.SetCodeOptionalHash(&address, codeAndHash)

	if evm.Config.Tracer != nil {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureStart(evm, caller.Address(), address, true, codeAndHash.code, gas, value)
		} else {
			evm.Config.Tracer.CaptureEnter(typ, caller.Address(), address, codeAndHash.code, gas, value)
		}
	}

	// IsParallel 当前交易是否支持并行执行
	// 实际执行智能合约代码
	ret, err, IsParallel := evm.interpreter.Run(contract, nil, false, TrueAccessList, IsSerial)

	// TODO: 替换stateDB中的AccessList
	if IsSerial {
		// 只有串行队列需要改AccessList
		StateAccessList := evm.StateDB.GetAccessList() // stateDB的AL
		StateAccessList.Addresses = TrueAccessList.Addresses
		StateAccessList.Slots = TrueAccessList.Slots
		// TODO: 后续stateDB中可能会有一项新的AccessList类型，到时候需要将真实的AccessList存到对应位置
	}

	// TODO: 错误判断与处理

	// 检查是否超过最大代码大小，如果超过则赋值err
	if err == nil && evm.chainRules.IsEIP158 && len(ret) > params.MaxCodeSize {
		err = ErrMaxCodeSizeExceeded
	}

	// 如果EIP-3541启用，拒绝以0xEF开头的代码 TODO: 后续可能需要删除
	if err == nil && len(ret) >= 1 && ret[0] == 0xEF && evm.chainRules.IsLondon {
		err = ErrInvalidCode
	}

	// 如果合约创建成功并且没有返回错误，计算存储代码所需的gas
	// 如果由于没有足够的gas而无法存储代码，则设置一个错误，并让下面的错误检查条件处理它
	if err == nil {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if contract.UseGas(createDataGas) {
			evm.StateDB.SetCode(address, ret)
		} else {
			err = ErrCodeStoreOutOfGas
		}
	}

	// 当EVM返回错误或设置上面的创建代码时，我们将恢复到快照并消耗剩余的gas
	// 此外，当我们在homestead时，这也会计算代码存储气体错误 TODO: 后续可能需要删除
	if err != nil && (evm.chainRules.IsHomestead || err != ErrCodeStoreOutOfGas) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != ErrExecutionReverted {
			contract.UseGas(contract.Gas) // 如果出错，则消耗该交易所有的gas
		}
	}

	// TODO: 新增一种快照回滚条件，当在并行组执行交易时，该交易无法并行执行 则也需要进行快照回滚
	if err == nil && IsSerial == false && IsParallel == false {
		fmt.Println("并行组中交易无法并行执行")
		evm.StateDB.RevertToSnapshot(snapshot) // 快照回滚
	}

	if evm.Config.Tracer != nil {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureEnd(ret, gas-contract.Gas, err)
		} else {
			evm.Config.Tracer.CaptureExit(ret, gas-contract.Gas, err)
		}
	}
	return ret, address, contract.Gas, err, IsParallel
}

// Create creates a new contract using code as 部署代码. TODO: change
func (evm *EVM) Create(caller ContractRef, code []byte, gas uint64, value *big.Int, TrueAccessList *JudgeAccessList,
	IsSerial bool) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error, IsParallel bool) {
	contractAddr = crypto.CreateAddress(caller.Address(), evm.StateDB.GetNonce(caller.Address()))
	return evm.create(caller, &codeAndHash{code: code}, gas, value, contractAddr, CREATE, TrueAccessList, IsSerial)
}

// Create2 creates a new contract using code as 部署代码. TODO: change
//
// Create2与Create的不同之处在于Create2使用keccak256(0xff ++ msg.Sender ++ salt ++ keccak256(init_code))[12:]
// 而不是通常的Sender -and-nonce-hash作为合约初始化的地址。
func (evm *EVM) Create2(caller ContractRef, code []byte, gas uint64, endowment *big.Int, salt *uint256.Int, TrueAccessList *JudgeAccessList, IsSerial bool) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error, IsParallel bool) {
	codeAndHash := &codeAndHash{code: code}
	contractAddr = crypto.CreateAddress2(caller.Address(), salt.Bytes32(), codeAndHash.Hash().Bytes())
	return evm.create(caller, codeAndHash, gas, endowment, contractAddr, CREATE2, TrueAccessList, IsSerial)
}

// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }
