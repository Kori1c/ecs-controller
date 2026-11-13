package cloud

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestCredentialClassificationIgnoresSignedTransportErrors(t *testing.T) {
	signedURL := "https://metrics.aliyuncs.com/?AccessKeyId=test-key&Signature=test-signature&Action=DescribeMetricList"
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, &net.DNSError{Err: "i/o timeout", Name: "metrics.aliyuncs.com", Server: "127.0.0.11:53", IsTimeout: true}, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset by peer")}, tls.RecordHeaderError{Msg: "TLS handshake failure"}, x509.UnknownAuthorityError{}, io.EOF} {
		err := fmt.Errorf("CMS 实例流量暂不可用: %w", &url.Error{Op: "Get", URL: signedURL, Err: cause})
		if IsCredentialError(err) || IsPermissionError(err) {
			t.Fatalf("transport error misclassified: %v", err)
		}
	}
	for _, err := range []error{nil, errors.New("unauthorized AccessKeyId Signature"), &APIError{Code: "InternalError", Message: signedURL}, &APIError{Code: "Throttling.User"}, &APIError{Code: "InvalidSignatureAlgorithm.NotSupported"}} {
		if IsCredentialError(err) || IsPermissionError(err) {
			t.Fatalf("non-authentication failure misclassified: %v", err)
		}
	}
}
func TestCredentialAndPermissionCodesRemainDistinctThroughWrapping(t *testing.T) {
	for _, tc := range []struct {
		code                   string
		credential, permission bool
	}{
		{"InvalidAccessKeyId.NotFound", true, false}, {"InvalidAccessKeyId.Inactive", true, false}, {"SignatureDoesNotMatch", true, false}, {"InvalidSignature", true, false}, {"SecurityTokenExpired", true, false},
		{"Forbidden.RAM", false, true}, {"Forbidden", false, true}, {"UnauthorizedOperation", false, true}, {"Unauthorized", false, true}, {"InvalidAccessKeyIdLookalike", false, false}, {"InternalError", false, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := fmt.Errorf("CMS failed: %w", &APIError{Code: tc.code, HTTPStatus: 403})
			if (tc.credential || tc.permission) && IsNotFound(err) {
				t.Fatal("authentication/permission failure mistaken for missing resource")
			}
			if IsCredentialError(err) != tc.credential || IsPermissionError(err) != tc.permission {
				t.Fatalf("wrong classification for %s", tc.code)
			}
		})
	}
}
