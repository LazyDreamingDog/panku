package tests

import (
	"fmt"
	"math/big"
	"net"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/executor"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/pb"
	"github.com/ethereum/go-ethereum/trie"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/grpclog"
)

const (
	// testCode is the testing contract binary code which will initialises some
	// variables in constructor
	testCode = "0x60806040527fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0060005534801561003457600080fd5b5060fc806100436000396000f3fe6080604052348015600f57600080fd5b506004361060325760003560e01c80630c4dae8814603757806398a213cf146053575b600080fd5b603d607e565b6040518082815260200191505060405180910390f35b607c60048036036020811015606757600080fd5b81019080803590602001909291905050506084565b005b60005481565b806000819055507fe9e44f9f7da8c559de847a3232b57364adc0354f15a2cd8dc636d54396f9587a6000546040518082815260200191505060405180910390a15056fea265627a7a723058208ae31d9424f2d0bc2a3da1a5dd659db2d71ec322a17db8f87e19e209e3a1ff4a64736f6c634300050a0032"

	// testGas is the gas required for contract deployment.
	testGas = 144109
)

var (
	Address          = "127.0.0.1:9876"
	testTxPoolConfig legacypool.Config
	db               = rawdb.NewMemoryDatabase()
	eip1559Config    *params.ChainConfig

	// Test accounts
	testBankKey, _  = crypto.GenerateKey()
	testBankAddress = crypto.PubkeyToAddress(testBankKey.PublicKey)
	testBankFunds   = big.NewInt(1000000000000000000)

	testUserKey, _  = crypto.GenerateKey()
	testUserAddress = crypto.PubkeyToAddress(testUserKey.PublicKey)
)

func init() {
	testTxPoolConfig = legacypool.DefaultConfig
	testTxPoolConfig.Journal = ""
	cpy := *params.TestChainConfig
	eip1559Config = &cpy
	eip1559Config.BerlinBlock = common.Big0
	eip1559Config.LondonBlock = common.Big0
}

type testBlockChain struct {
	config        *params.ChainConfig
	gasLimit      atomic.Uint64
	statedb       *state.StateDB
	chainHeadFeed *event.Feed
}

func newTestBlockChain(config *params.ChainConfig, gasLimit uint64, statedb *state.StateDB, chainHeadFeed *event.Feed) *testBlockChain {
	bc := testBlockChain{config: config, statedb: statedb, chainHeadFeed: new(event.Feed)}
	bc.gasLimit.Store(gasLimit)
	return &bc
}

func (bc *testBlockChain) Config() *params.ChainConfig {
	return bc.config
}

func (bc *testBlockChain) CurrentBlock() *types.Header {
	return &types.Header{
		Number:   new(big.Int),
		GasLimit: bc.gasLimit.Load(),
	}
}

func (bc *testBlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return types.NewBlock(bc.CurrentBlock(), nil, nil, nil, trie.NewStackTrie(nil))
}

func (bc *testBlockChain) StateAt(common.Hash) (*state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *testBlockChain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return bc.chainHeadFeed.Subscribe(ch)
}

func newRandomTx(creation bool, nonce uint64) *types.Transaction {
	var tx *types.Transaction
	gasPrice := big.NewInt(10 * params.InitialBaseFee)
	// if creation {
	// 	tx, _ = types.SignTx(types.NewContractCreation(nonce, big.NewInt(0), testGas, gasPrice, common.FromHex(testCode)), types.HomesteadSigner{}, testBankKey)
	// } else {
	// 	tx, _ = types.SignTx(types.NewTransaction(nonce, testUserAddress, big.NewInt(1000), params.TxGas, gasPrice, nil), types.HomesteadSigner{}, testBankKey)
	// }
	tx, _ = types.SignTx(types.NewTransaction(nonce, testUserAddress, big.NewInt(1000), params.TxGas, gasPrice, nil), types.HomesteadSigner{}, testBankKey)
	return tx
}

// 测试共识和执行的交互
func TestCAE(t *testing.T) {
	// init
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(db), nil)
	statedb.SetBalance(testBankAddress, testBankFunds)
	blockchain := newTestBlockChain(eip1559Config, 10000000, statedb, new(event.Feed))

	// 实例化TxPool
	epool := legacypool.New(testTxPoolConfig, blockchain)
	etxpool, _ := txpool.New(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain, []txpool.SubPool{epool})
	defer etxpool.Close()

	ppool := legacypool.New(testTxPoolConfig, blockchain)
	ptxpool, _ := txpool.New(new(big.Int).SetUint64(testTxPoolConfig.PriceLimit), blockchain, []txpool.SubPool{ppool})
	defer ptxpool.Close()

	// 启动共识客户端
	conn, err := grpc.Dial("127.0.0.1:9080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial gRPC server: %v", err)
	}
	p2pClient := pb.NewP2PClient(conn)

	// 实例化executorService
	es := executor.NewExecutorService(etxpool, ptxpool, p2pClient)

	// 启动服务端
	listen, err := net.Listen("tcp", Address)
	if err != nil {
		grpclog.Fatalf("Failed to listen: %v", err)
	}
	defer listen.Close()

	s := grpc.NewServer()
	pb.RegisterExecutorServer(s, es)
	fmt.Println("Listen on " + Address)
	grpclog.Println("Listen on " + Address)

	go s.Serve(listen)

	// 构造随机交易
	var tempTxs []*txpool.Transaction
	for i := 0; i < 2; i++ {
		ptx := &txpool.Transaction{Tx: newRandomTx(i%2 == 0, uint64(i))}
		tempTxs = append(tempTxs, ptx)
	}
	ppool.Add(tempTxs, true, false)

	// go es.SendLoop()
	// go es.ExecuteLoop()

	// es.SendLoop()
	// es.ExecuteLoop()
	for {
	}
}

// TODO: 测试交易执行函数
func TestTxExec(t *testing.T) {
	// init
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabase(db), nil)
	statedb.SetBalance(testBankAddress, testBankFunds)
	blockchain := newTestBlockChain(eip1559Config, 10000000, statedb, new(event.Feed))

	// 构造随机交易
	var tempTxs []*types.Transaction // tempTxs 包含了临时交易
	for i := 0; i < 1; i++ {
		ptx := newRandomTx(i%2 == 0, uint64(i))
		tempTxs = append(tempTxs, ptx) //
	}

	for {
	}
}
