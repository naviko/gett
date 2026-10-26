package coin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

func rowsOf(name string, k kind, bodies ...string) map[string]string {
	var docs []any
	for _, b := range bodies {
		docs = append(docs, parseJSON([]byte(b)))
	}
	out := map[string]string{}
	for _, r := range chains[name].summarize(k, "x", docs) {
		out[r.Label] = r.Value
	}
	return out
}

func expect(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestAmount(t *testing.T) {
	cases := []struct {
		in   string
		dec  int
		want string
	}{
		{"5768739852", 8, "57.68739852 BTC"},
		{"6222206467666", 8, "62222.06467666 BTC"},
		{"4385334472513834264", 8, "43853344725.13834264 BTC"},
		{"0", 8, "0 BTC"},
		{"1", 18, "0.000000000000000001 BTC"},
		{"1000000000000000000", 18, "1 BTC"},
		{"-500", 9, "-0.0000005 BTC"},
	}
	for _, c := range cases {
		if got := amount(parseJSON([]byte(c.in)), c.dec, "BTC"); got != c.want {
			t.Errorf("amount(%s, %d) = %q, want %q", c.in, c.dec, got, c.want)
		}
	}
	if amount("abc", 8, "X") != "unknown" || amount(nil, 8, "X") != "unknown" {
		t.Error("bad input should be unknown")
	}
}

func TestMempoolSummaries(t *testing.T) {
	addr := `{"address":"1A1z","chain_stats":{"funded_txo_count":81051,"funded_txo_sum":5768739852,"spent_txo_count":0,"spent_txo_sum":0,"tx_count":67116},"mempool_stats":{"funded_txo_count":1,"funded_txo_sum":3672,"spent_txo_count":0,"spent_txo_sum":0,"tx_count":1}}`
	expect(t, rowsOf("btc", address, addr), map[string]string{
		"balance": "57.68739852 BTC", "received": "57.68739852 BTC", "sent": "0 BTC", "transactions": "67116", "pending": "1 transactions, 0.00003672 BTC",
	})
	tx := `{"txid":"4a5e","vin":[{"is_coinbase":true}],"vout":[{"value":5000000000}],"size":204,"fee":0,"status":{"confirmed":true,"block_height":0,"block_time":1231006505}}`
	expect(t, rowsOf("btc", transaction, tx), map[string]string{
		"status": "confirmed", "block": "0", "time": "2009-01-03 18:15:05 UTC", "amount": "50 BTC", "fee": "0 BTC", "inputs": "1", "outputs": "1", "coinbase": "yes",
	})
	expect(t, rowsOf("ltc", address, `{"chain_stats":{"funded_txo_sum":843741411250315,"spent_txo_sum":843741336263553,"tx_count":494},"mempool_stats":{"tx_count":0}}`), map[string]string{
		"balance": "0.74986762 LTC", "transactions": "494",
	})
}

func TestBlockcypherSummaries(t *testing.T) {
	addr := `{"address":"DH5y","total_received":4385334472513834264,"total_sent":4385328250307366598,"balance":6222206467666,"unconfirmed_balance":0,"final_balance":6222206467666,"n_tx":1485,"unconfirmed_n_tx":0,"final_n_tx":1485}`
	expect(t, rowsOf("doge", address, addr), map[string]string{
		"balance": "62222.06467666 DOGE", "received": "43853344725.13834264 DOGE", "transactions": "1485",
	})
	tx := `{"hash":"4a5e","block_height":0,"total":5000000000,"fees":0,"size":204,"confirmed":"2009-01-03T18:15:05Z","confirmations":973003,"vin_sz":1,"vout_sz":1}`
	expect(t, rowsOf("dash", transaction, tx), map[string]string{
		"status": "confirmed", "confirmations": "973003", "time": "2009-01-03 18:15:05 UTC", "amount": "50 DASH", "size": "204 bytes",
	})
}

func TestHaskoinSummary(t *testing.T) {
	addr := `{"address":"bitcoincash:qpm2","confirmed":5593767221,"unconfirmed":0,"utxo":11723,"txs":11723,"received":5593767221}`
	expect(t, rowsOf("bch", address, addr), map[string]string{"balance": "55.93767221 BCH", "sent": "0 BCH", "transactions": "11723"})
}

func TestBlockscoutSummaries(t *testing.T) {
	addr := `{"coin_balance":"1500000000000000000","exchange_rate":"2000","is_contract":false,"ens_domain_name":"vitalik.eth","hash":"0xd8"}`
	counters := `{"transactions_count":"1234","token_transfers_count":"56"}`
	expect(t, rowsOf("eth", address, addr, counters), map[string]string{
		"balance": "1.5 ETH", "value": "3000.00 USD", "transactions": "1234", "token transfers": "56", "ens": "vitalik.eth", "contract": "no",
	})
	tx := `{"status":"ok","result":"success","block_number":46147,"confirmations":26220425,"timestamp":"2015-08-07T03:30:33.000000Z","from":{"hash":"0xA1E4"},"to":{"hash":"0x5DF9"},"value":"31337","fee":{"type":"actual","value":"1050000000000000"},"gas_used":"21000","method":null}`
	expect(t, rowsOf("eth", transaction, tx), map[string]string{
		"status": "success", "block": "46147", "from": "0xA1E4", "to": "0x5DF9", "value": "0.000000000000031337 ETH", "fee": "0.00105 ETH", "time": "2015-08-07 03:30:33 UTC",
	})
}

func TestSolanaSummaries(t *testing.T) {
	bal := `{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":1500000000},"id":1}`
	sigs := `{"jsonrpc":"2.0","result":[{"signature":"abc","blockTime":1792893756}],"id":1}`
	expect(t, rowsOf("sol", address, bal, sigs), map[string]string{"balance": "1.5 SOL", "transactions": "1", "last activity": "2026-10-25 02:02:36 UTC"})
	tx := `{"jsonrpc":"2.0","result":{"blockTime":1792893756,"meta":{"err":null,"fee":10000,"preBalances":[8955943330],"postBalances":[10277872252]},"slot":461080589,"transaction":{"message":{"accountKeys":[{"pubkey":"Vote111"}],"instructions":[{}]}}},"id":1}`
	expect(t, rowsOf("sol", transaction, tx), map[string]string{
		"status": "success", "slot": "461080589", "fee": "0.00001 SOL", "signer": "Vote111", "signer change": "1.321928922 SOL", "instructions": "1",
	})
}

func TestTronSummaries(t *testing.T) {
	acct := `{"data":[{"address":"4174472e","balance":14493835459,"account_name":"57494e4b","type":"Contract","create_time":1600000000000,"trc20":[{"a":"1"},{"b":"2"}]}],"success":true}`
	expect(t, rowsOf("trx", address, acct), map[string]string{"balance": "14493.835459 TRX", "name": "WINK", "type": "contract", "trc20 tokens": "2", "created": "2020-09-13 12:26:40 UTC"})
	tx := `{"ret":[{"contractRet":"SUCCESS"}],"raw_data":{"timestamp":1792893357000,"contract":[{"type":"TransferContract","parameter":{"value":{"amount":1000000,"owner_address":"41a614f803b6fd780986a42c78ec9c7f77e6ded13c","to_address":"41a614f803b6fd780986a42c78ec9c7f77e6ded13c"}}}]}}`
	info := `{"blockNumber":87406724,"blockTimeStamp":1792893378000,"fee":1100000,"receipt":{"energy_usage_total":13263,"result":"SUCCESS"}}`
	expect(t, rowsOf("trx", transaction, tx, info), map[string]string{
		"status": "success", "block": "87406724", "type": "TransferContract", "amount": "1 TRX", "fee": "1.1 TRX", "energy": "13263",
		"from": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "to": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
	})
}

func TestShapeAndRun(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"chain_stats":{"funded_txo_sum":100000000,"spent_txo_sum":0,"tx_count":1},"mempool_stats":{"tx_count":0}}`)
	}))
	defer srv.Close()
	chains["test"] = mempoolChain("TST", srv.URL, "test")
	defer delete(chains, "test")
	c, _ := httpx.NewClient(httpx.Config{})

	// Piped: raw body, byte for byte.
	tty := false
	term.StdoutIsTTY = &tty
	defer func() { term.StdoutIsTTY = nil }()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := Run(context.Background(), "test", "1A1z", c, nil)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if out := <-done; !strings.HasPrefix(out, `{"chain_stats"`) || gotPath != "/address/1A1z" {
		t.Fatalf("piped output or path wrong: %q %s", out, gotPath)
	}

	// 64 hex is a transaction.
	tty = true
	r, w, _ = os.Pipe()
	os.Stdout = w
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err = Run(context.Background(), "test", strings.Repeat("ab", 32), c, nil)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if out := <-done; gotPath != "/tx/"+strings.Repeat("ab", 32) || !strings.Contains(out, "transaction") {
		t.Fatalf("tx shape not detected: %s %q", gotPath, out)
	}

	// 404 → exit 22.
	err = Run(context.Background(), "test", "missing", c, nil)
	if errs.Code(err) != 22 {
		t.Fatalf("expected exit 22, got %v", err)
	}
}

func TestBase58(t *testing.T) {
	if got := tronAddress("41a614f803b6fd780986a42c78ec9c7f77e6ded13c"); got != "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" {
		t.Fatalf("tron address = %s", got)
	}
	if tronAddress("zz") != "" || tronAddress("41a6") != "" {
		t.Fatal("bad hex must give empty")
	}
}
