package azureidentity_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/azureidentity"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" {
		// This test fails on MacOS for some reason. See https://github.com/coder/coder/issues/12978
		t.Skip()
	}

	mustTime := func(layout string, value string) time.Time {
		ti, err := time.Parse(layout, value)
		require.NoError(t, err)
		return ti
	}

	for _, tc := range []struct {
		date    time.Time
		name    string
		payload string
		vmID    string
	}{{
		name:    "regular",
		payload: "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",
		vmID:    "bd8e7443-24a0-41f3-b949-8baf4fd1c573",
		date:    mustTime(time.RFC3339, "2023-02-01T00:00:00Z"),
	}, {
		name:    "govcloud",
		payload: "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",
		vmID:    "990878d4-068a-4ac4-9ee9-1231d2218ef2",
		date:    mustTime(time.RFC3339, "2023-04-01T00:00:00Z"),
	}, {
		name:    "rsa",
		payload: "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",
		vmID:    "960a4b4a-dab2-44ef-9b73-7753043b4f16",
		date:    mustTime(time.RFC3339, "2024-04-22T17:32:44Z"),
	}} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vm, err := azureidentity.Validate(context.Background(), tc.payload, azureidentity.Options{
				VerifyOptions: x509.VerifyOptions{
					CurrentTime: tc.date,
				},
				Offline: true,
			})
			require.NoError(t, err)
			require.Equal(t, tc.vmID, vm)
		})
	}
}

func TestExpiresSoon(t *testing.T) {
	t.Parallel()
	const threshold = 1

	for _, c := range azureidentity.Certificates {
		block, rest := pem.Decode([]byte(c))
		require.Zero(t, len(rest))
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)

		expiresSoon := cert.NotAfter.Before(time.Now().AddDate(0, threshold, 0))
		if expiresSoon {
			t.Errorf("certificate expires within %d months %s: %s", threshold, cert.NotAfter, cert.Subject.CommonName)
		} else {
			url := "no issuing url"
			if len(cert.IssuingCertificateURL) > 0 {
				url = cert.IssuingCertificateURL[0]
			}
			t.Logf("certificate %q doesn't expire for a while (%s)", cert.Subject.CommonName, url)
		}
	}
}
