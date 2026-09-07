package bhavcopy

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// DefaultUserAgent is what nsearchives.nseindia.com expects; bare clients get blocked.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Fetcher downloads one session's bhavcopy zip and parses the CSV inside.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
	URLFor    func(time.Time) string
}

// NewFetcher returns a Fetcher against the real NSE archive with a 60s timeout.
func NewFetcher() *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: 60 * time.Second}, UserAgent: DefaultUserAgent, URLFor: URLFor}
}

// Fetch returns the EQ bars for date d. found is false when NSE has no file
// for that date (HTTP 404), which is how holidays and weekends present.
func (f *Fetcher) Fetch(ctx context.Context, d time.Time) (bars []market.Bar, found bool, err error) {
	url := f.URLFor(d)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: read: %w", url, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: not a zip (%d bytes): %w", url, len(body), err)
	}
	for _, entry := range zr.File {
		if !strings.HasSuffix(strings.ToLower(entry.Name), ".csv") {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return nil, false, err
		}
		bars, err = Parse(entry.Name, rc)
		rc.Close()
		if err != nil {
			return nil, false, err
		}
		return bars, true, nil
	}
	return nil, false, fmt.Errorf("fetch %s: zip has no csv", url)
}
