// Copyright 2017 The go-ethereum Authors

package types

import (
	"github.com/MOACChain/MoacLib/common"
)

type QueryContract struct {
	Block           uint           `json:"queryInBlock" gencodec:"required"`
	ContractAddress common.Address `json:"contractAddress"`
}
