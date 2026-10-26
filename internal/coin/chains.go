package coin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

var chains = map[string]*chain{
	"btc":  mempoolChain("BTC", "https://mempool.space/api", "mempool.space"),
	"ltc":  mempoolChain("LTC", "https://litecoinspace.org/api", "litecoinspace.org"),
	"doge": blockcypherChain("DOGE", "doge"),
	"dash": blockcypherChain("DASH", "dash"),
	"bch":  haskoinChain(),
	"eth":  blockscoutChain(),
	"sol":  solanaChain(),
	"trx":  tronChain(),
}

// --- mempool.space API (btc, ltc) -------------------------------------------

func mempoolChain(symbol, base, source string) *chain {
	c := &chain{Symbol: symbol, Decimals: 8, Source: source}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			return []request{{"GET", base + "/tx/" + id, ""}}
		}
		return []request{{"GET", base + "/address/" + id, ""}}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			funded, spent := bigint(get(d, "chain_stats.funded_txo_sum")), bigint(get(d, "chain_stats.spent_txo_sum"))
			rows := []row{}
			if funded != nil && spent != nil {
				rows = append(rows, row{"balance", amount(new(big.Int).Sub(funded, spent), 8, symbol)})
			}
			rows = append(rows,
				row{"received", amount(funded, 8, symbol)},
				row{"sent", amount(spent, 8, symbol)},
				row{"transactions", str(get(d, "chain_stats.tx_count"))},
			)
			if n := bigint(get(d, "mempool_stats.tx_count")); n != nil && n.Sign() > 0 {
				pf, ps := bigint(get(d, "mempool_stats.funded_txo_sum")), bigint(get(d, "mempool_stats.spent_txo_sum"))
				rows = append(rows, row{"pending", fmt.Sprintf("%s transactions, %s", n, amount(new(big.Int).Sub(pf, ps), 8, symbol))})
			}
			return rows
		}
		confirmed := get(d, "status.confirmed") == true
		rows := []row{{"status", map[bool]string{true: "confirmed", false: "unconfirmed"}[confirmed]}}
		if confirmed {
			rows = append(rows, row{"block", str(get(d, "status.block_height"))}, row{"time", unixTime(get(d, "status.block_time"), false)})
		}
		rows = append(rows,
			row{"amount", amount(sumList(get(d, "vout"), "value"), 8, symbol)},
			row{"fee", amount(get(d, "fee"), 8, symbol)},
			row{"inputs", strconv.Itoa(count(get(d, "vin")))},
			row{"outputs", strconv.Itoa(count(get(d, "vout")))},
			row{"size", str(get(d, "size")) + " bytes"},
		)
		if get(d, "vin.0.is_coinbase") == true {
			rows = append(rows, row{"coinbase", "yes"})
		}
		return rows
	}
	return c
}

// --- BlockCypher (doge, dash) -----------------------------------------------

func blockcypherChain(symbol, slug string) *chain {
	base := "https://api.blockcypher.com/v1/" + slug + "/main"
	c := &chain{Symbol: symbol, Decimals: 8, Source: "blockcypher.com"}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			return []request{{"GET", base + "/txs/" + id, ""}}
		}
		return []request{{"GET", base + "/addrs/" + id + "/balance", ""}}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			rows := []row{
				{"balance", amount(get(d, "final_balance"), 8, symbol)},
				{"received", amount(get(d, "total_received"), 8, symbol)},
				{"sent", amount(get(d, "total_sent"), 8, symbol)},
				{"transactions", str(get(d, "final_n_tx"))},
			}
			if n := bigint(get(d, "unconfirmed_n_tx")); n != nil && n.Sign() > 0 {
				rows = append(rows, row{"pending", fmt.Sprintf("%s transactions, %s", n, amount(get(d, "unconfirmed_balance"), 8, symbol))})
			}
			return rows
		}
		rows := []row{}
		if get(d, "confirmed") != nil {
			rows = append(rows, row{"status", "confirmed"}, row{"block", str(get(d, "block_height"))},
				row{"confirmations", str(get(d, "confirmations"))}, row{"time", isoTime(get(d, "confirmed"))})
		} else {
			rows = append(rows, row{"status", "unconfirmed"}, row{"received", isoTime(get(d, "received"))})
		}
		return append(rows,
			row{"amount", amount(get(d, "total"), 8, symbol)},
			row{"fee", amount(get(d, "fees"), 8, symbol)},
			row{"inputs", str(get(d, "vin_sz"))},
			row{"outputs", str(get(d, "vout_sz"))},
			row{"size", str(get(d, "size")) + " bytes"},
		)
	}
	return c
}

// --- blockchain.com haskoin (bch) -------------------------------------------

func haskoinChain() *chain {
	base := "https://api.blockchain.info/haskoin-store/bch"
	c := &chain{Symbol: "BCH", Decimals: 8, Source: "blockchain.com"}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			return []request{{"GET", base + "/transaction/" + id, ""}}
		}
		return []request{{"GET", base + "/address/" + id + "/balance", ""}}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			confirmed, received := bigint(get(d, "confirmed")), bigint(get(d, "received"))
			rows := []row{{"balance", amount(confirmed, 8, "BCH")}, {"received", amount(received, 8, "BCH")}}
			if confirmed != nil && received != nil {
				rows = append(rows, row{"sent", amount(new(big.Int).Sub(received, confirmed), 8, "BCH")})
			}
			rows = append(rows, row{"transactions", str(get(d, "txs"))}, row{"unspent outputs", str(get(d, "utxo"))})
			if n := bigint(get(d, "unconfirmed")); n != nil && n.Sign() != 0 {
				rows = append(rows, row{"pending", amount(n, 8, "BCH")})
			}
			return rows
		}
		rows := []row{}
		if h := get(d, "block.height"); h != nil {
			rows = append(rows, row{"status", "confirmed"}, row{"block", str(h)})
		} else {
			rows = append(rows, row{"status", "unconfirmed"})
		}
		if t := unixTime(get(d, "time"), false); t != "" {
			rows = append(rows, row{"time", t})
		}
		return append(rows,
			row{"amount", amount(sumList(get(d, "outputs"), "value"), 8, "BCH")},
			row{"fee", amount(get(d, "fee"), 8, "BCH")},
			row{"inputs", strconv.Itoa(count(get(d, "inputs")))},
			row{"outputs", strconv.Itoa(count(get(d, "outputs")))},
			row{"size", str(get(d, "size")) + " bytes"},
		)
	}
	return c
}

// --- Blockscout (eth) -------------------------------------------------------

func blockscoutChain() *chain {
	base := "https://eth.blockscout.com/api/v2"
	c := &chain{Symbol: "ETH", Decimals: 18, Source: "eth.blockscout.com"}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			return []request{{"GET", base + "/transactions/" + id, ""}}
		}
		return []request{{"GET", base + "/addresses/" + id, ""}, {"GET", base + "/addresses/" + id + "/counters", ""}}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			rows := []row{{"balance", amount(get(d, "coin_balance"), 18, "ETH")}}
			if rate, bal := bigint(get(d, "exchange_rate")), bigint(get(d, "coin_balance")); rate != nil && bal != nil {
				usd := new(big.Float).Mul(new(big.Float).SetInt(bal), new(big.Float).SetInt(rate))
				usd.Quo(usd, big.NewFloat(1e18))
				rows = append(rows, row{"value", fmt.Sprintf("%.2f USD", usd)})
			}
			if len(docs) > 1 {
				rows = append(rows, row{"transactions", str(get(docs[1], "transactions_count"))},
					row{"token transfers", str(get(docs[1], "token_transfers_count"))})
			}
			if n := str(get(d, "ens_domain_name")); n != "" {
				rows = append(rows, row{"ens", n})
			}
			rows = append(rows, row{"contract", yesNo(get(d, "is_contract") == true)})
			return rows
		}
		status := str(get(d, "status"))
		if status == "ok" {
			status = "success"
		}
		if r := str(get(d, "revert_reason")); r != "" {
			status += " (" + r + ")"
		}
		block := get(d, "block_number")
		if block == nil {
			block = get(d, "block")
		}
		rows := []row{{"status", status}, {"block", str(block)}, {"confirmations", str(get(d, "confirmations"))},
			{"time", isoTime(get(d, "timestamp"))}, {"from", str(get(d, "from.hash"))}}
		if to := str(get(d, "to.hash")); to != "" {
			rows = append(rows, row{"to", to})
		} else if cc := str(get(d, "created_contract.hash")); cc != "" {
			rows = append(rows, row{"created contract", cc})
		}
		rows = append(rows, row{"value", amount(get(d, "value"), 18, "ETH")}, row{"fee", amount(get(d, "fee.value"), 18, "ETH")})
		if m := str(get(d, "method")); m != "" {
			rows = append(rows, row{"method", m})
		}
		return rows
	}
	return c
}

// --- Solana JSON-RPC --------------------------------------------------------

func solanaChain() *chain {
	rpc := "https://api.mainnet-beta.solana.com"
	c := &chain{Symbol: "SOL", Decimals: 9, Source: "api.mainnet-beta.solana.com"}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			return []request{{"POST", rpc, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":[%q,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0}]}`, id)}}
		}
		return []request{
			{"POST", rpc, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[%q]}`, id)},
			{"POST", rpc, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":[%q,{"limit":1000}]}`, id)},
		}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			rows := []row{{"balance", amount(get(d, "result.value"), 9, "SOL")}}
			if len(docs) > 1 {
				n := count(get(docs[1], "result"))
				txs := strconv.Itoa(n)
				if n >= 1000 {
					txs = "1000 or more"
				}
				rows = append(rows, row{"transactions", txs})
				if t := unixTime(get(docs[1], "result.0.blockTime"), false); t != "" {
					rows = append(rows, row{"last activity", t})
				}
			}
			return rows
		}
		status := "success"
		if get(d, "result.meta.err") != nil {
			status = "failed"
		}
		rows := []row{{"status", status}, {"slot", str(get(d, "result.slot"))}, {"time", unixTime(get(d, "result.blockTime"), false)},
			{"fee", amount(get(d, "result.meta.fee"), 9, "SOL")}}
		if signer := str(get(d, "result.transaction.message.accountKeys.0.pubkey")); signer != "" {
			rows = append(rows, row{"signer", signer})
		}
		pre, post := bigint(get(d, "result.meta.preBalances.0")), bigint(get(d, "result.meta.postBalances.0"))
		if pre != nil && post != nil {
			rows = append(rows, row{"signer change", amount(new(big.Int).Sub(post, pre), 9, "SOL")})
		}
		rows = append(rows, row{"instructions", strconv.Itoa(count(get(d, "result.transaction.message.instructions")))})
		return rows
	}
	return c
}

// --- TronGrid (trx) ---------------------------------------------------------

func tronChain() *chain {
	base := "https://api.trongrid.io"
	c := &chain{Symbol: "TRX", Decimals: 6, Source: "trongrid.io"}
	c.requests = func(k kind, id string) []request {
		if k == transaction {
			body := fmt.Sprintf(`{"value":%q}`, id)
			return []request{{"POST", base + "/wallet/gettransactionbyid", body}, {"POST", base + "/wallet/gettransactioninfobyid", body}}
		}
		return []request{{"GET", base + "/v1/accounts/" + id, ""}}
	}
	c.summarize = func(k kind, id string, docs []any) []row {
		d := docs[0]
		if k == address {
			a := get(d, "data.0")
			rows := []row{{"balance", amount(get(a, "balance"), 6, "TRX")}}
			if name, err := hex.DecodeString(str(get(a, "account_name"))); err == nil && len(name) > 0 {
				rows = append(rows, row{"name", string(name)})
			}
			if t := str(get(a, "type")); t != "" {
				rows = append(rows, row{"type", strings.ToLower(t)})
			}
			if t := unixTime(get(a, "create_time"), true); t != "" {
				rows = append(rows, row{"created", t})
			}
			if t := unixTime(get(a, "latest_opration_time"), true); t != "" {
				rows = append(rows, row{"last activity", t})
			}
			if n := count(get(a, "trc20")); n > 0 {
				rows = append(rows, row{"trc20 tokens", strconv.Itoa(n)})
			}
			return rows
		}
		status := str(get(d, "ret.0.contractRet"))
		if status == "" {
			status = "unknown"
		}
		rows := []row{{"status", strings.ToLower(status)}}
		if len(docs) > 1 {
			rows = append(rows, row{"block", str(get(docs[1], "blockNumber"))}, row{"time", unixTime(get(docs[1], "blockTimeStamp"), true)})
		}
		ctype := str(get(d, "raw_data.contract.0.type"))
		rows = append(rows, row{"type", ctype})
		val := get(d, "raw_data.contract.0.parameter.value")
		if from := tronAddress(str(get(val, "owner_address"))); from != "" {
			rows = append(rows, row{"from", from})
		}
		if to := tronAddress(str(get(val, "to_address"))); to != "" {
			rows = append(rows, row{"to", to})
		} else if ca := tronAddress(str(get(val, "contract_address"))); ca != "" {
			rows = append(rows, row{"contract", ca})
		}
		if ctype == "TransferContract" {
			rows = append(rows, row{"amount", amount(get(val, "amount"), 6, "TRX")})
		}
		if len(docs) > 1 {
			if fee := get(docs[1], "fee"); fee != nil {
				rows = append(rows, row{"fee", amount(fee, 6, "TRX")})
			} else {
				rows = append(rows, row{"fee", "0 TRX"})
			}
			if e := get(docs[1], "receipt.energy_usage_total"); e != nil {
				rows = append(rows, row{"energy", str(e)})
			}
		}
		return rows
	}
	return c
}

// tronAddress converts a hex "41..." address to the familiar base58 "T..." form.
func tronAddress(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 21 {
		return ""
	}
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return base58(append(b, second[:4]...))
}

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	var out []byte
	mod := new(big.Int)
	base := big.NewInt(58)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
