// Copyright 2016 The go-ethereum Authors
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

package mcclient

import moaccore "github.com/MOACChain/MoacVnode"

// Verify that Client implements the MOAC interfaces.
var (
	_ = moaccore.ChainReader(&Client{})
	_ = moaccore.TransactionReader(&Client{})
	_ = moaccore.ChainStateReader(&Client{})
	_ = moaccore.ChainSyncReader(&Client{})
	_ = moaccore.ContractCaller(&Client{})
	_ = moaccore.GasEstimator(&Client{})
	_ = moaccore.GasPricer(&Client{})
	_ = moaccore.LogFilterer(&Client{})
	_ = moaccore.PendingStateReader(&Client{})
	// _ = moaccore.PendingStateEventer(&Client{})
	_ = moaccore.PendingContractCaller(&Client{})
)
