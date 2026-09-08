package nseindex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// DefaultUserAgent is what nsearchives.nseindia.com expects; bare clients get
// blocked, exactly as with the equity bhavcopy.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// URLFor is the archive path for one session.
func URLFor(d time.Time) string {
	return "https://nsearchives.nseindia.com/content/indices/ind_close_all_" +
		d.Format("02012006") + ".csv"
}

// MirrorURLFor is niftyindices.com's copy of the same file. It is a fallback
// rather than the primary, because a mirror that silently diverged would be
// worse than one that is simply absent; Fetch records which host answered.
func MirrorURLFor(d time.Time) string {
	return "https://niftyindices.com/Daily_Snapshot/ind_close_all_" +
		d.Format("02012006") + ".csv"
}

// Fetcher downloads one session's index CSV.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
	URLFor    func(time.Time) string
	MirrorFor func(time.Time) string
}

// NewFetcher returns a Fetcher against the real archive with a 60s timeout.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client:    &http.Client{Timeout: 60 * time.Second},
		UserAgent: DefaultUserAgent,
		URLFor:    URLFor,
		MirrorFor: MirrorURLFor,
	}
}

// Fetch returns one session's levels. found is false when the archive has no
// file for that date, which is how holidays, weekends and pre-2012 dates
// present.
//
// **The archive answers a missing file two different ways and only one of them
// is an HTTP 404.** Dates before the archive begins -- and some inside it, such
// as 2015-12-01 -- come back HTTP 200 with an HTML error page. A loader that
// trusted the status code would hand that page to the CSV parser and record a
// hard failure for what is really an absent file, and a backfill would stop
// dead on the first one. So the body is sniffed, and anything that is not the
// archive's header is treated as absent rather than as a fault.
func (f *Fetcher) Fetch(ctx context.Context, d time.Time) (levels []market.IndexLevel, anomalies []string, found bool, err error) {
	body, found, err := f.get(ctx, f.URLFor(d))
	if err != nil {
		return nil, nil, false, err
	}
	if !found && f.MirrorFor != nil {
		// One retry against the mirror. NSE's archive occasionally serves the
		// soft-404 page for a session it does have.
		body, found, err = f.get(ctx, f.MirrorFor(d))
		if err != nil {
			return nil, nil, false, err
		}
	}
	if !found {
		return nil, nil, false, nil
	}
	levels, anomalies, err = Parse(bytes.NewReader(body), d)
	if err != nil {
		return nil, anomalies, true, err
	}
	return levels, anomalies, true, nil
}

// get returns the body when the response is a real CSV. It reports found=false
// for a 404 and for the HTML page a 200 can carry.
func (f *Fetcher) get(ctx context.Context, url string) ([]byte, bool, error) {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: read: %w", url, err)
	}
	if !looksLikeArchiveCSV(body) {
		return nil, false, nil
	}
	return body, true, nil
}

// looksLikeArchiveCSV checks for the archive's own first column name at the
// start of the body. Matching the header rather than merely rejecting "<" is
// deliberate: a truncated file, a login redirect rendered as text, or a
// maintenance notice would all pass a looser test and then fail deep inside the
// parser as a hard error on a date that is simply unavailable.
func looksLikeArchiveCSV(body []byte) bool {
	head := body
	if len(head) > 256 {
		head = head[:256]
	}
	return strings.HasPrefix(strings.TrimSpace(string(head)), "Index Name,")
}
