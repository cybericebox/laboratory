package grpc

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
)

// oidTenantEpoch is the private extension a client certificate carries: the enrollment epoch it was issued in and the UID of
// the Tenant it was issued for. The arc is internal to the platform (it is not meant to be understood by anyone else).
var oidTenantEpoch = asn1.ObjectIdentifier{1, 3, 6, 1, 3, 2026, 1, 1}

type epochClaim struct {
	Epoch     int64
	TenantUID string `asn1:"utf8"`
}

// epochExtension is the extension to put on a certificate. It is not critical: the CA's own verification ignores it, the
// agent reads it (see checkEpoch).
func epochExtension(epoch int64, tenantUID string) (pkix.Extension, error) {
	der, err := asn1.Marshal(epochClaim{Epoch: epoch, TenantUID: tenantUID})
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{Id: oidTenantEpoch, Value: der}, nil
}

// certEpoch reads the epoch claim of a certificate. ok is false for a certificate without one (issued before the epoch was a
// number); an extension that is present but malformed is an error, never "no claim".
func certEpoch(c *x509.Certificate) (claim epochClaim, ok bool, err error) {
	for _, e := range c.Extensions {
		if !e.Id.Equal(oidTenantEpoch) {
			continue
		}
		rest, err := asn1.Unmarshal(e.Value, &claim)
		if err != nil || len(rest) != 0 {
			return epochClaim{}, false, fmt.Errorf("the epoch extension of the client certificate is malformed")
		}
		return claim, true, nil
	}
	return epochClaim{}, false, nil
}
