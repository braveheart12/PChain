package core

import (
	abci "github.com/tendermint/abci/types"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
)

//-----------------------------------------------------------------------------

func ABCIQuery(context *RPCDataContext, path string, data []byte, prove bool) (*ctypes.ResultABCIQuery, error) {
	resQuery, err := context.proxyAppQuery.QuerySync(abci.RequestQuery{
		Path:  path,
		Data:  data,
		Prove: prove,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("ABCIQuery", " path:", path, " data:", data, " result:", resQuery)
	return &ctypes.ResultABCIQuery{resQuery}, nil
}

func ABCIInfo(context *RPCDataContext) (*ctypes.ResultABCIInfo, error) {
	resInfo, err := context.proxyAppQuery.InfoSync()
	if err != nil {
		return nil, err
	}
	return &ctypes.ResultABCIInfo{resInfo}, nil
}
