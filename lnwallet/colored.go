package lnwallet

import (
	"bytes"
	"os"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcd/txscript"
	"github.com/parnurzeal/gorequest"
)

var dustAmount = 546
var coluMagicBytes = []byte{ 0x43, 0x43, 0x02 } // Colu Protocol { 0x43, 0x43 } + Version { 0x02 }
var urlBase = os.Getenv("CC_SRVC_URL")

// ColoredCoin transfer instruction
type CcInstruction struct {
	Skip    bool   `json:"skip"`
	Range   bool   `json:"range"`
	Percent bool   `json:"percent"`
	Output  uint32 `json:"output"`
	Amount  int    `json:"amount"` // 64?
}

// Transform regular transactions into colored-coins-encoded ones,
// by re-encoding the standard output values into OP_RETURN-embedded
// instructions and replacing the actual output value with dust amounts
// @XXX nadav: currently assumes a single-input tx
func ColorifyTx(tx *wire.MsgTx, isFunding bool) (*wire.MsgTx, error) {

	newTx := wire.NewMsgTx()
	newTx.Version = tx.Version

	for _, txIn := range tx.TxIn {
		newTx.AddTxIn(txIn)
	}

	var insts []CcInstruction

	for i, txOut := range tx.TxOut {
		// hijack the output value and re-encode it as a colored coin instruction
		insts = append(insts, CcInstruction{
			Skip: false, Range: false, Percent: false,
			Output: uint32(i),
			Amount: int(txOut.Value),
		})
		if isFunding {
			// @XXX nadav: commitTx has no way of telling the btc-denominated value of outputs,
			//      since it was hijacked to represent the colored-asset-denominated value.
			//      for now, make sure that the fundingTx has a matching amount of both so that
			//      we're able to properly sign it (segwit signs over input values, too)
			newTx.AddTxOut(wire.NewTxOut(int64(txOut.Value), txOut.PkScript))
		} else {
			// for commit/close txs, always create outputs of dust amount
			newTx.AddTxOut(wire.NewTxOut(int64(dustAmount), txOut.PkScript))
		}
	}

	// encode colored coin instructions
	opReturn, err := EncodeCcInstructions(insts)
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

// encodes via a local nodejs server that provides a low-level protocol serialization api
func EncodeCcInstructions(insts []CcInstruction) ([]byte, error) {
	_, body, errs := gorequest.New().
		Post(urlBase + "payment/encode/bulk").
		Set("Content-Type", "application/json").
		Send(insts).
		EndBytes()
	if errs != nil { return nil, errs[0] }

	return append(coluMagicBytes, body...), nil
}

// unused, not needed for now (both sides independently re-construct the txs)
// uses "fmt", "encoding/json" and "errors" (currently unimported)
/*
func DecodeCcInstructions(opReturn []byte) ([]CcInstruction, error) {
	if !bytes.Equal(opReturn[:4], coluMagicBytes) {
		return nil, errors.New("Invalid magic bytes")
	}

	_, body, errs := gorequest.New().
		Post(urlBase + "payment/decode/bulk").
		Set("Content-Type", "application/json").
		Send("hex", fmt.Sprintf("%02x", opReturn[4:])).
		EndBytes()
	if errs != nil { return nil, errs[0] }

	var insts []CcInstruction
	json.Unmarshal(body, &insts)
	return insts, nil
}
*/
