package bhavcopy

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

func zipOf(t *testing.T, name, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	require.NoError(t, err)
	_, err = w.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestFetch_UnzipsAndParses_And404MeansNoFile(t *testing.T) {
	payload := zipOf(t, "cm30JUN2015bhav.csv", "testdata/cm30JUN2015bhav.csv")
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Path == "/2015-06-30" {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	f := &Fetcher{
		Client:    srv.Client(),
		UserAgent: "verdict-machine-test",
		URLFor:    func(d time.Time) string { return srv.URL + "/" + d.Format("2006-01-02") },
	}
	bars, found, err := f.Fetch(context.Background(), market.Day(2015, 6, 30))
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, bars, 1487, "1447 EQ plus 40 BE")
	require.Equal(t, "verdict-machine-test", gotUA)

	bars, found, err = f.Fetch(context.Background(), market.Day(2015, 6, 28))
	require.NoError(t, err)
	require.False(t, found, "404 is a non-trading day, not an error")
	require.Empty(t, bars)
}
