// Copyright 2021 The go-ethereum Authors
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

package types

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

type PankuTx struct {
	ChainID          *big.Int
	Nonce            uint64
	GasTipCap        *big.Int // a.k.a. maxPriorityFeePerGas
	GasFeeCap        *big.Int // a.k.a. maxFeePerGas
	Gas              uint64
	To               *common.Address `rlp:"nil"` // nil means contract creation
	Value            *big.Int
	Data             []byte
	AccessList       AccessList
	StrictAccessList AccessList

	// Signature values
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`
}

// Change TODO: 增加一个修改交易中AL的方法
func (tx *PankuTx) Change(MsgTAL []AccessTuple) bool {
	tx.AccessList = MsgTAL
	return true
}

// copy creates a deep copy of the transaction data and initializes all fields.
func (tx *PankuTx) copy() TxData {
	cpy := &PankuTx{
		Nonce: tx.Nonce,
		To:    copyAddressPtr(tx.To),
		Data:  common.CopyBytes(tx.Data),
		Gas:   tx.Gas,
		// These are copied below.
		AccessList:       make(AccessList, len(tx.AccessList)),
		StrictAccessList: make(AccessList, len(tx.StrictAccessList)),
		Value:            new(big.Int),
		ChainID:          new(big.Int),
		GasTipCap:        new(big.Int),
		GasFeeCap:        new(big.Int),
		V:                new(big.Int),
		R:                new(big.Int),
		S:                new(big.Int),
	}
	copy(cpy.AccessList, tx.AccessList)
	copy(cpy.StrictAccessList, tx.StrictAccessList)
	if tx.Value != nil {
		cpy.Value.Set(tx.Value)
	}
	if tx.ChainID != nil {
		cpy.ChainID.Set(tx.ChainID)
	}
	if tx.GasTipCap != nil {
		cpy.GasTipCap.Set(tx.GasTipCap)
	}
	if tx.GasFeeCap != nil {
		cpy.GasFeeCap.Set(tx.GasFeeCap)
	}
	if tx.V != nil {
		cpy.V.Set(tx.V)
	}
	if tx.R != nil {
		cpy.R.Set(tx.R)
	}
	if tx.S != nil {
		cpy.S.Set(tx.S)
	}
	return cpy
}

// accessors for innerTx.
func (tx *PankuTx) txType() byte                 { return PankuTxType }
func (tx *PankuTx) chainID() *big.Int            { return tx.ChainID }
func (tx *PankuTx) accessList() AccessList       { return tx.AccessList }
func (tx *PankuTx) strictAccessList() AccessList { return tx.StrictAccessList }
func (tx *PankuTx) data() []byte                 { return tx.Data }
func (tx *PankuTx) gas() uint64                  { return tx.Gas }
func (tx *PankuTx) gasFeeCap() *big.Int          { return tx.GasFeeCap }
func (tx *PankuTx) gasTipCap() *big.Int          { return tx.GasTipCap }
func (tx *PankuTx) gasPrice() *big.Int           { return tx.GasFeeCap }
func (tx *PankuTx) value() *big.Int              { return tx.Value }
func (tx *PankuTx) nonce() uint64                { return tx.Nonce }
func (tx *PankuTx) to() *common.Address          { return tx.To }
func (tx *PankuTx) blobGas() uint64              { return 0 }
func (tx *PankuTx) blobGasFeeCap() *big.Int      { return nil }
func (tx *PankuTx) blobHashes() []common.Hash    { return nil }

func (tx *PankuTx) effectiveGasPrice(dst *big.Int, baseFee *big.Int) *big.Int {
	if baseFee == nil {
		return dst.Set(tx.GasFeeCap)
	}
	tip := dst.Sub(tx.GasFeeCap, baseFee)
	if tip.Cmp(tx.GasTipCap) > 0 {
		tip.Set(tx.GasTipCap)
	}
	return tip.Add(tip, baseFee)
}

func (tx *PankuTx) rawSignatureValues() (v, r, s *big.Int) {
	return tx.V, tx.R, tx.S
}

func (tx *PankuTx) setSignatureValues(chainID, v, r, s *big.Int) {
	tx.ChainID, tx.V, tx.R, tx.S = chainID, v, r, s
}

// NewTransaction creates an unsigned legacy transaction.
// Deprecated: use NewTx instead.
func NewPankuTransaction(nonce uint64, to common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte, gasFee *big.Int, tip *big.Int, al AccessList) *Transaction {
	return NewTx(&PankuTx{
		ChainID:    big.NewInt(1),
		Nonce:      nonce,
		To:         &to,
		Value:      amount,
		Gas:        gasLimit,
		Data:       data,
		AccessList: al,
		GasTipCap:  tip,
		GasFeeCap:  gasFee,
	})
}

// NewContractCreation creates an unsigned legacy transaction.
// Deprecated: use NewTx instead.
func NewPankuContractCreation(nonce uint64, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte, gasFee *big.Int, tip *big.Int, al AccessList) *Transaction {
	return NewTx(&PankuTx{
		ChainID:    big.NewInt(1),
		Nonce:      nonce,
		Value:      amount,
		Gas:        gasLimit,
		Data:       data,
		AccessList: al,
		GasTipCap:  tip,
		GasFeeCap:  gasFee,
	})
}
