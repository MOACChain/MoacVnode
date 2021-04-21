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

package params

import (
	"fmt"
)

//2018/03/20, 1st release is Pangu 0.8.0
//2018/07/28, nuwa test version 1.0.0
//2018/09/20, nuwa version 1.0.3, fixed memory leaking issue under pressure test.
//2019/07/12, nuwa version 1.0.10, allow multiple contracts to be deployed on one AppChain.
//2020/02/08, nuwa version 1.1.0, release in 2020. It contains all the updates in the v1.0.x version.
//2020/07/01, nuwa version 1.1.5, support the RandDrop AppChain.
//2021/01/24, fuxi version 2.0.0, support solidity 0.8.x in evm for testnet.
//2021/02/28, fuxi version 2.0.1, support solidity 0.8.x in evm for mainnet.
//2021/03/15, fuxi version 2.0.2, release to defuse the difficulty bomb on testnet at block 5042000.
//2021/03/25, fuxi version 2.0.3, release to defuse the difficulty bomb on mainnet at block height 6462000.
//2021/04/06, fuxi version 2.0.4, fixe the error when VNODE read block states and improve the stability of VNODE.
//2021/04/18, fuxi version 2.0.5, enables the web3 RPC commands after block height 5260000 on testnet.

const (
	VersionName  = "fuxi" // Major version name in the Roadmap: Pangu 0.8; Nuwa 1.0; Fuxi 2.0; Shennong 2.0+;
	VersionMajor = 2      // Major version component of the current release
	VersionMinor = 0      // Minor version component of the current release
	VersionPatch = 5      // Patch version component of the current release
	VersionMeta  = "beta" // Version metadata to append to the version string, rc/stable
)

// Version holds the textual version string with Full name.
var VersionWithName = func() string {
	v := fmt.Sprintf("%s %d.%d.%d", VersionName, VersionMajor, VersionMinor, VersionPatch)
	if VersionMeta != "" {
		v += "-" + VersionMeta
	}
	return v
}()

// Version holds the textual version string.
var Version = func() string {
	v := fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)
	if VersionMeta != "" {
		v += "-" + VersionMeta
	}
	return v
}()

// VersionNum only returns the version number.
var VersionNum = func() string {
	v := fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)
	return v
}()

func VersionWithCommit(gitCommit string) string {
	vsn := Version
	if len(gitCommit) >= 8 {
		vsn += "-" + gitCommit[:8]
	}
	return vsn
}
