package mining

import (
	"sort"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/account"
	"github.com/bytom/blockchain/txbuilder"
	"github.com/bytom/consensus"
	"github.com/bytom/errors"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
	"github.com/bytom/protocol/state"
	"github.com/bytom/protocol/validation"
	// "github.com/bytom/protocol/vm"
	"github.com/bytom/protocol/vm/vmutil"
)

var CoinbaseFlags = "/P2SH/bytomd/"

// createCoinbaseTx returns a coinbase transaction paying an appropriate subsidy
// based on the passed block height to the provided address.  When the address
// is nil, the coinbase transaction will instead be redeemable by anyone.
func createCoinbaseTx(accountManager *account.Manager, amount uint64, blockHeight uint64) (tx *types.Tx, err error) {
	amount += consensus.BlockSubsidy(blockHeight)
	arbitrary := append([]byte{0x00}, []byte(strconv.FormatUint(blockHeight, 10))...)

	var script []byte
	if accountManager == nil {
		script, err = vmutil.DefaultCoinbaseProgram()
	} else {
		script, err = accountManager.GetCoinbaseControlProgram()
		arbitrary = append(arbitrary, accountManager.GetCoinbaseArbitrary()...)
	}
	if err != nil {
		return nil, err
	}

	// TODO
	// 	insts, err := vm.ParseProgram(script)
	// 	for _, inst := range insts {
	// 		log.Info(inst)
	// 	}
	// func (b *Builder) AddData(data []byte) *Builder {
	// 	b.program = append(b.program, vm.PushdataBytes(data)...)
	//  	return b
	//  }

	if len(arbitrary) > consensus.CoinbaseArbitrarySizeLimit {
		return nil, validation.ErrCoinbaseArbitraryOversize
	}

	builder := txbuilder.NewBuilder(time.Now())
	if err = builder.AddInput(types.NewCoinbaseInput(arbitrary), &txbuilder.SigningInstruction{}); err != nil {
		return nil, err
	}
	if err = builder.AddOutput(types.NewTxOutput(*consensus.BTMAssetID, amount, script)); err != nil {
		return nil, err
	}
	_, txData, err := builder.Build()
	if err != nil {
		return nil, err
	}

	byteData, err := txData.MarshalText()
	if err != nil {
		return nil, err
	}
	txData.SerializedSize = uint64(len(byteData))

	tx = &types.Tx{
		TxData: *txData,
		Tx:     types.MapTx(txData),
	}
	return tx, nil
}

// TODO
// txsize
// timerange
// Sizelimit is not specified explicitly while there actually exists a upper
// bound for block_size according to MaxBlockGas
type BlockTemplate struct {
	Block           *types.Block
	Seed            *bc.Hash
	ValidPayAddress bool

	//btcd
	// Fees contains the amount of fees each transaction in the generated
	// template pays in base units.  Since the first transaction is the
	// coinbase, the first entry (offset 0) will contain the negative of the
	// sum of the fees of all other transactions.
	Fees []int64
	// SigOpCosts contains the number of signature operations each
	// transaction in the generated template performs.
	SigOpCosts []int64 `json:"sig_op_costs,omitempty"`
	// WitnessCommitment is a commitment to the witness data (if any)
	// within the block. This field will only be populted once segregated
	// witness has been activated, and the block contains a transaction
	// which has witness data.
	WitnessCommitment []byte `json:"witness_commitment,omitempty"`
}

// NewBlockTemplate returns a new block template that is ready to be solved
func NewBlockTemplate(c *protocol.Chain, txPool *protocol.TxPool, accountManager *account.Manager) (template *BlockTemplate, err error) {
	view := state.NewUtxoViewpoint()
	txStatus := bc.NewTransactionStatus()
	txStatus.SetStatus(0, false)
	txEntries := []*bc.Tx{nil}
	gasUsed := uint64(0)
	txFee := uint64(0)

	// get preblock info for generate next block
	preBlockHeader := c.BestBlockHeader()
	preBlockHash := preBlockHeader.Hash()
	nextBlockHeight := preBlockHeader.Height + 1
	nextBits, err := c.CalcNextBits(&preBlockHash)
	if err != nil {
		return nil, err
	}

	b := &types.Block{
		BlockHeader: types.BlockHeader{
			Version:           1,
			Height:            nextBlockHeight,
			PreviousBlockHash: preBlockHash,
			Timestamp:         uint64(time.Now().Unix()),
			BlockCommitment:   types.BlockCommitment{},
			Bits:              nextBits,
		},
	}
	bcBlock := &bc.Block{BlockHeader: &bc.BlockHeader{Height: nextBlockHeight}}
	b.Transactions = []*types.Tx{nil}

	txs := txPool.GetTransactions()
	sort.Sort(byTime(txs))
	for _, txDesc := range txs {
		tx := txDesc.Tx.Tx
		gasOnlyTx := false

		if err := c.GetTransactionsUtxo(view, []*bc.Tx{tx}); err != nil {
			log.WithField("error", err).Error("mining block generate skip tx due to")
			txPool.RemoveTransaction(&tx.ID)
			continue
		}

		gasStatus, err := validation.ValidateTx(tx, bcBlock)
		if err != nil {
			if !gasStatus.GasValid {
				log.WithField("error", err).Error("mining block generate skip tx due to")
				txPool.RemoveTransaction(&tx.ID)
				continue
			}
			gasOnlyTx = true
		}

		if gasUsed+uint64(gasStatus.GasUsed) > consensus.MaxBlockGas {
			break
		}

		if err := view.ApplyTransaction(bcBlock, tx, gasOnlyTx); err != nil {
			log.WithField("error", err).Error("mining block generate skip tx due to")
			txPool.RemoveTransaction(&tx.ID)
			continue
		}

		txStatus.SetStatus(len(b.Transactions), gasOnlyTx)
		b.Transactions = append(b.Transactions, txDesc.Tx)
		txEntries = append(txEntries, tx)
		gasUsed += uint64(gasStatus.GasUsed)
		txFee += txDesc.Fee

		if gasUsed == consensus.MaxBlockGas {
			break
		}
	}

	// creater coinbase transaction
	b.Transactions[0], err = createCoinbaseTx(accountManager, txFee, nextBlockHeight)
	if err != nil {
		return nil, errors.Wrap(err, "fail on createCoinbaseTx")
	}
	txEntries[0] = b.Transactions[0].Tx

	b.BlockHeader.BlockCommitment.TransactionsMerkleRoot, err = bc.TxMerkleRoot(txEntries)
	if err != nil {
		return nil, err
	}

	b.BlockHeader.BlockCommitment.TransactionStatusHash, err = bc.TxStatusMerkleRoot(txStatus.VerifyStatus)

	seed, err := c.CalcNextSeed(&b.BlockHeader.PreviousBlockHash)
	if err != nil {
		log.Errorf("NewBlockTemplate: failed on calc next seed: %v", err)
		return
	}

	validPayAddress := false
	if accountManager != nil {
		if _, err := accountManager.GetMiningAddress(); err == nil {
			validPayAddress = true
		}
	}

	template = &BlockTemplate{
		Block:           b,
		Seed:            seed,
		ValidPayAddress: validPayAddress,
	}

	return template, err
}
