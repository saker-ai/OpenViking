package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCryptoRoundTrip(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.In_ = strings.NewReader("hello secret world")

	// Encrypt stdin to a buffer, then decrypt back.
	encryptOut := &bytes.Buffer{}
	rt.Out = encryptOut
	encCmd := silence(CryptoCmd(rt))
	require.NoError(t, Execute(encCmd, []string{"encrypt", "--passphrase=pw"}))
	require.NotEmpty(t, encryptOut.String())

	rt.In_ = strings.NewReader(encryptOut.String())
	decOut := &bytes.Buffer{}
	rt.Out = decOut
	decCmd := silence(CryptoCmd(rt))
	require.NoError(t, Execute(decCmd, []string{"decrypt", "--passphrase=pw"}))
	assert.Equal(t, "hello secret world", decOut.String())
}

func TestCryptoWrongPassphrase(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.In_ = strings.NewReader("secret")

	encOut := &bytes.Buffer{}
	rt.Out = encOut
	encCmd := silence(CryptoCmd(rt))
	require.NoError(t, Execute(encCmd, []string{"encrypt", "--passphrase=correct"}))

	rt.In_ = strings.NewReader(encOut.String())
	rt.Out = new(bytes.Buffer)
	decCmd := silence(CryptoCmd(rt))
	err := Execute(decCmd, []string{"decrypt", "--passphrase=wrong"})
	require.Error(t, err)
}

func TestCryptoMissingPassphrase(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.In_ = strings.NewReader("data")
	cmd := silence(CryptoCmd(rt))
	err := Execute(cmd, []string{"encrypt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "passphrase")
}

func TestDoctorCmd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.0","commit":"abc"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	rt, out, _ := newTestRuntime(t, srv)

	cmd := silence(DoctorCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	output := out.String()
	assert.Contains(t, output, "ov version:")
	assert.Contains(t, output, "all probes OK")
}

func TestDoctorCmdFailsOnBadProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	rt, _, errOut := newTestRuntime(t, srv)

	cmd := silence(DoctorCmd(rt))
	err := Execute(cmd, []string{})
	require.Error(t, err)
	assert.Contains(t, errOut.String(), "FAIL")
}
