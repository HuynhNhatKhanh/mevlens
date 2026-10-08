package eth

// Header is the subset of a JSON-RPC block object the observatory needs, as returned
// by eth_getBlockByNumber(n, false). Arbitrum-specific fields are included.
type Header struct {
	Number        Quantity `json:"number"`
	Hash          Hash     `json:"hash"`
	ParentHash    Hash     `json:"parentHash"`
	Timestamp     Quantity `json:"timestamp"`
	BaseFee       Quantity `json:"baseFeePerGas"` // fits in 64 bits on Arbitrum (~1e7 wei)
	GasUsed       Quantity `json:"gasUsed"`
	L1BlockNumber Quantity `json:"l1BlockNumber"` // Arbitrum: L1 block the sequencer referenced
	Transactions  []Hash   `json:"transactions"`
}

// Receipt is the subset of a JSON-RPC transaction receipt the observatory needs.
// Receipts carry everything required for classification except calldata, which
// lets ingestion use a single eth_getBlockReceipts call per block.
type Receipt struct {
	TxHash            Hash     `json:"transactionHash"`
	TxIndex           Quantity `json:"transactionIndex"`
	BlockHash         Hash     `json:"blockHash"`
	BlockNumber       Quantity `json:"blockNumber"`
	From              Address  `json:"from"`
	To                Address  `json:"to"` // zero for contract creation (JSON null)
	Type              Quantity `json:"type"`
	Status            Quantity `json:"status"`
	GasUsed           Quantity `json:"gasUsed"`
	GasUsedForL1      Quantity `json:"gasUsedForL1"` // Arbitrum: L1 data cost expressed in L2 gas
	EffectiveGasPrice Quantity `json:"effectiveGasPrice"`
	Timeboosted       bool     `json:"timeboosted"` // Arbitrum: sequenced via the Timeboost express lane
	Logs              []Log    `json:"logs"`
}

// Succeeded reports whether the transaction executed without reverting.
func (r *Receipt) Succeeded() bool { return r.Status == 1 }

// Log is an EVM log entry.
type Log struct {
	Address  Address  `json:"address"`
	Topics   []Hash   `json:"topics"`
	Data     Data     `json:"data"`
	LogIndex Quantity `json:"logIndex"`
	TxIndex  Quantity `json:"transactionIndex"`
	Removed  bool     `json:"removed"`
}

// Topic returns the i-th topic, or false if absent.
func (l *Log) Topic(i int) (Hash, bool) {
	if i < 0 || i >= len(l.Topics) {
		return Hash{}, false
	}
	return l.Topics[i], true
}

// Block bundles a header with all of its receipts, in transaction order.
type Block struct {
	Header   Header
	Receipts []Receipt
}
