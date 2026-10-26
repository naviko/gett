package netx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"gett/internal/term"
)

const (
	speedDown = "https://speed.cloudflare.com/__down?bytes="
	speedUp   = "https://speed.cloudflare.com/__up"
	downBytes = 50_000_000 // the endpoint refuses larger requests
	upBytes   = 25_000_000
)

// Speed measures latency, download and upload against Cloudflare.
func Speed(ctx context.Context, client *http.Client) error {
	lat, err := latency(ctx, client)
	if err != nil {
		return err
	}
	line("%s", padRow("latency", ms(lat)))

	prog := term.NewProgress()
	bar := prog.Bar("download", downBytes, true)
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%d", speedDown, downBytes), nil)
	resp, err := client.Do(req)
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		bar.Abort()
		prog.Shutdown()
		return fmt.Errorf("speed test refused (%s)", resp.Status)
	}
	n, err := io.Copy(bar.Writer(io.Discard), resp.Body)
	resp.Body.Close()
	bar.Done()
	if err != nil {
		prog.Shutdown()
		return err
	}
	down := time.Since(start)

	bar = prog.Bar("upload", upBytes, true)
	start = time.Now()
	body := bytes.Repeat([]byte{0x5a}, upBytes)
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, speedUp, bar.Reader(bytes.NewReader(body)))
	req.ContentLength = upBytes
	resp, err = client.Do(req)
	if err != nil {
		bar.Abort()
		prog.Shutdown()
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	up := time.Since(start)
	bar.Done()
	prog.Wait()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("speed test refused (%s)", resp.Status)
	}

	line("%s", padRow("download", mbit(n, down)))
	line("%s", padRow("upload", mbit(upBytes, up)))
	return nil
}

// latency is the median of five small requests.
func latency(ctx context.Context, client *http.Client) (time.Duration, error) {
	var ds []time.Duration
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, speedDown+"0", nil)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], nil
}

func mbit(n int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f Mbit/s", float64(n)*8/d.Seconds()/1e6)
}
