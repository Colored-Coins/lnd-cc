package lndcc

import (
	"bytes"
	"fmt"
	"os"

	"github.com/parnurzeal/gorequest"

	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var dustAmount = 546
var ccEncodingUrl = os.Getenv("CC_ENCODING_URL")
var ccTxoUrl = os.Getenv("CC_TXO_URL")

// ColoredCoin transfer instruction
type Instruction struct {
	Skip    bool   `json:"skip"`
	Range   bool   `json:"range"`
	Percent bool   `json:"percent"`
	Output  uint32 `json:"output"`
	Amount  int    `json:"amount"` // 64?
}

// ColoredCoin transaction output color data
type TxoData struct {
	AssetId string         `json:"assetId"`
	Value   btcutil.Amount `json:"value"`
}

func (d TxoData) String() string {
	return fmt.Sprintf("%d of %s", d.Value, d.AssetId)
}

// Transform regular transactions into colored-coins-encoded ones,
// by re-encoding the standard output values into OP_RETURN-embedded
// instructions and replacing the actual output value with dust amounts
// @FIXME currently assumes a single-input tx
func ColorifyTx(tx *wire.MsgTx, isFunding bool) (*wire.MsgTx, error) {

	newTx := wire.NewMsgTx()
	newTx.Version = tx.Version

	for _, txIn := range tx.TxIn {
		newTx.AddTxIn(txIn)
	}

	var insts []Instruction

	for i, txOut := range tx.TxOut {
		// hijack the output value and re-encode it as a colored coin instruction
		insts = append(insts, Instruction{
			Skip: false, Range: false, Percent: false,
			Output: uint32(i),
			Amount: int(txOut.Value),
		})
		if isFunding {
			// make sure the funding output has enough funding for fees and output dust
			// @TODO leftover is wasted, better to split everything that's available instead
			newTx.AddTxOut(wire.NewTxOut(int64(dustAmount*15), txOut.PkScript))
		} else {
			// use dust amounts for outputs of the commit/close txs
			newTx.AddTxOut(wire.NewTxOut(int64(dustAmount), txOut.PkScript))
		}
	}

	// encode colored coin instructions
	opReturn, err := encodeInstructions(insts)
	if err != nil {
		return nil, err
	}

	// build wrapping OP_RETURN script
	var script bytes.Buffer
	if err := script.WriteByte(txscript.OP_RETURN); err != nil {
		return nil, err
	}
	if err := wire.WriteVarBytes(&script, 0, opReturn); err != nil {
		return nil, err
	}

	// create OP_RETURN output
	newTx.AddTxOut(wire.NewTxOut(int64(0), script.Bytes()))

	return newTx, nil
}

// Encodes the transfer instructions via cc-encoding-api
func encodeInstructions(insts []Instruction) ([]byte, error) {
	_, body, errs := gorequest.New().
		Post(fmt.Sprintf("%s/%s", ccEncodingUrl, "encode")).
		Set("Content-Type", "application/json").
		Send(insts).
		EndBytes()

	if errs != nil {
		return nil, errs[0]
	}

	return body, nil
}

// Get TXO color data via cc-txo-color
func GetTxoData(out wire.OutPoint) (*TxoData, error) {
	var txoData TxoData

	_, _, errs := gorequest.New().
		Get(fmt.Sprintf("%s/%s/%d", ccTxoUrl, out.Hash, out.Index)).
		EndStruct(&txoData)

	if errs != nil {
		return nil, errs[0]
	}

	return &txoData, nil
}

// unused, not needed for now (both sides independently re-construct the txs)
// uses "fmt", "encoding/json" and "errors" (currently unimported)
/*
func DecodeInstructions(opReturn []byte) ([]Instruction, error) {
	_, body, errs := gorequest.New().
		Post(ccEncodingUrl + "payment/decode/bulk").
		Set("Content-Type", "application/json").
		Send("hex", fmt.Sprintf("%02x", opReturn)).
		EndBytes()
	if errs != nil { return nil, errs[0] }

	var insts []Instruction
	json.Unmarshal(body, &insts)
	return insts, nil
}
*/
