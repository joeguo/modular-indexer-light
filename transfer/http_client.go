package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/btcsuite/btcd/btcjson"
)

var DefaultBitcoinClient *HttpGetter

type HttpGetter struct {
	URL      string
	Username string
	Password string
	client   *http.Client
}

type HttpError struct {
	Code    int
	Message string
}

func init() {
	DefaultBitcoinClient = NewHttpGetter(defaultURL, "", "")
}

func NewHttpGetter(host, username, password string) *HttpGetter {
	return &HttpGetter{
		URL:      host,
		Username: username,
		Password: password,
		client:   &http.Client{Timeout: 3 * time.Second},
	}
}

func (r *HttpGetter) post(data interface{}, headers map[string]string) ([]byte, error) {
	param, err := json.Marshal(data)
	if nil != err {
		return nil, err
	}
	req, err := http.NewRequest("POST", r.URL, bytes.NewBuffer(param))
	if nil != err {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/json")
	if len(headers) > 0 {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	resp, err := r.client.Do(req)
	if nil != err {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (r *HttpGetter) GetBlockHash(blockHeight uint) (string, error) {
	type param struct {
		Method string `json:"method"`
		Params []uint `json:"params"`
	}

	body, err := r.post(param{
		Method: "getblockhash",
		Params: []uint{blockHeight},
	}, nil)
	if nil != err {
		return "", err
	}

	type Result struct {
		Result string
		Error  HttpError
	}
	var ret Result
	if err := json.Unmarshal(body, &ret); nil != err {
		return "", err
	}
	if ret.Error.Code != 0 {
		return "", errors.New(ret.Error.Message)
	}
	return ret.Result, nil
}

func (r *HttpGetter) GetRawTransaction(txID string) (*btcjson.TxRawResult, error) {

	type txReq struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}

	body, err := r.post(txReq{
		Method: "getrawtransaction",
		Params: []interface{}{txID, true},
	}, nil)
	if nil != err {
		return nil, err
	}

	type Result struct {
		Result *btcjson.TxRawResult
		Error  HttpError
	}
	var ret Result
	if err := json.Unmarshal(body, &ret); nil != err {
		return nil, err
	}
	if ret.Error.Code != 0 {
		return nil, errors.New(ret.Error.Message)
	}
	return ret.Result, nil
}

func (r *HttpGetter) GetOutput(txID string, index int) (*btcjson.Vout, error) {
	type txReq struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}

	body, err := r.post(txReq{
		Method: "getrawtransaction",
		Params: []interface{}{txID, true},
	}, nil)
	if nil != err {
		return nil, err
	}

	type Result struct {
		Result *btcjson.TxRawResult
		Error  HttpError
	}
	var ret Result
	if err := json.Unmarshal(body, &ret); nil != err {
		return nil, err
	}
	if ret.Error.Code != 0 {
		return nil, errors.New(ret.Error.Message)
	}

	if len(ret.Result.Vout) < index+1 {
		return nil, fmt.Errorf("RawTransction not have enough vout, cap %d, need %d", len(ret.Result.Vout), index)
	}
	return &ret.Result.Vout[index], nil
}

func (r *HttpGetter) GetBlock1(hash string) (*btcjson.GetBlockVerboseResult, error) {
	type param struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}

	body, err := r.post(param{
		Method: "getblock",
		Params: []interface{}{hash, 1},
	}, nil)
	if nil != err {
		return nil, err
	}

	type result struct {
		Result *btcjson.GetBlockVerboseResult
		Error  HttpError
	}

	var ret result
	if err := json.Unmarshal(body, &ret); nil != err {
		return nil, err
	}

	return ret.Result, nil
}
