package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func certificatePEM(t *testing.T, key *ecdsa.PrivateKey, dns string, edit func(*x509.Certificate)) []byte {
	t.Helper()
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if edit != nil {
		edit(cert)
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCertificateVerification(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	for _, tc := range []struct {
		name  string
		edit  func(*x509.Certificate)
		valid bool
	}{
		{"valid", nil, true},
		{"wrong dns", func(c *x509.Certificate) { c.DNSNames = []string{"another.invalid"} }, false},
		{"expired", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Second) }, false},
		{"future", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) }, false},
		{"wrong usage", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fingerprint, err := validateCertificate(certificatePEM(t, key, "probe.invalid", tc.edit), keyPEM, nil, "probe.invalid", true, time.Now())
			if (err == nil) != tc.valid || tc.valid && len(fingerprint) != 64 {
				t.Fatalf("fingerprint=%q error=%v", fingerprint, err)
			}
		})
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := validateCertificate(certificatePEM(t, other, "probe.invalid", nil), keyPEM, nil, "probe.invalid", true, time.Now()); err == nil {
		t.Fatal("accepted another private key's certificate")
	}
}

func TestCertManagerIssuance(t *testing.T) {
	for _, mode := range []string{"issued", "denied", "pending", "replaced", "invalid certificate", "collision"} {
		t.Run(mode, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			issuerReads := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					obj.SetUID(types.UID("fixture-" + obj.GetObjectKind().GroupVersionKind().Kind))
					return c.Create(ctx, obj, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, objectKey, obj, opts...); err != nil {
						return err
					}
					u, ok := obj.(*unstructured.Unstructured)
					if !ok {
						return nil
					}
					if u.GetKind() == "Issuer" {
						issuerReads++
						if issuerReads == 1 || mode == "pending" {
							return nil
						}
					}
					u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
					if u.GetKind() != "CertificateRequest" {
						return nil
					}
					switch mode {
					case "denied":
						u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Denied", "status": "True", "reason": "Policy"}}}
						return nil
					case "replaced":
						u.SetUID("replacement")
						return nil
					case "invalid certificate":
						return nil
					}
					secret := &corev1.Secret{}
					if err := c.Get(ctx, objectKey, secret); err != nil {
						t.Fatal(err)
					}
					if u.GetAnnotations()["cert-manager.io/private-key-secret-name"] != secret.Name {
						t.Fatal("self-signed request has no signing key reference")
					}
					block, _ := pem.Decode(secret.Data[corev1.TLSPrivateKeyKey])
					key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
					if err != nil {
						t.Fatal(err)
					}
					encoded, _, _ := unstructured.NestedString(u.Object, "spec", "request")
					csrPEM, _ := base64.StdEncoding.DecodeString(encoded)
					block, _ = pem.Decode(csrPEM)
					csr, err := x509.ParseCertificateRequest(block.Bytes)
					if err != nil || csr.CheckSignature() != nil {
						t.Fatalf("invalid CSR: %v", err)
					}
					cert := certificatePEM(t, key.(*ecdsa.PrivateKey), csr.DNSNames[0], nil)
					return unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString(cert), "status", "certificate")
				},
			}).Build()
			pod := podIdentity{"infra-smoketest", "cert-probe", "pod-uid"}
			if mode == "collision" {
				if err := c.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pod.name, Namespace: pod.namespace}, Data: map[string][]byte{"untouched": []byte("original")}}); err != nil {
					t.Fatal(err)
				}
			}
			timeout := time.Second
			if mode == "pending" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			fingerprint, err := certManager(ctx, c, pod, definition.IssuerRef{}, time.Millisecond)
			if mode == "issued" {
				if err != nil || len(fingerprint) != 64 || issuerReads < 2 {
					t.Fatalf("issuance: %q %v", fingerprint, err)
				}
			} else if err == nil {
				t.Fatal("accepted failed issuance")
			}
			if mode == "pending" && (!errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Issuer Ready")) {
				t.Fatalf("lost timeout diagnostic: %v", err)
			}
			secret := &corev1.Secret{}
			if getErr := c.Get(context.Background(), client.ObjectKey{Namespace: pod.namespace, Name: pod.name}, secret); getErr != nil {
				t.Fatal(getErr)
			}
			if mode == "collision" {
				if !apierrors.IsAlreadyExists(err) || string(secret.Data["untouched"]) != "original" {
					t.Fatal("existing secret was adopted or modified")
				}
			} else if len(secret.OwnerReferences) != 1 || string(secret.OwnerReferences[0].UID) != pod.uid {
				t.Fatal("fixture is not owned by this probe Pod")
			}
		})
	}
}

func testCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := certificatePEM(t, key, "test-ca.invalid", func(cert *x509.Certificate) {
		cert.IsCA = true
		cert.BasicConstraintsValid = true
		cert.KeyUsage = x509.KeyUsageCertSign
		cert.ExtKeyUsage = nil
	})
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert, certPEM
}

func TestExistingCertManagerIssuer(t *testing.T) {
	for _, tc := range []struct {
		name, kind, failure string
		selfSigned          bool
	}{
		{name: "namespaced CA", kind: "Issuer"},
		{name: "cluster CA", kind: "ClusterIssuer"},
		{name: "namespaced self-signed", kind: "Issuer", selfSigned: true},
		{name: "cluster self-signed", kind: "ClusterIssuer", selfSigned: true},
		{name: "missing", kind: "Issuer", failure: "missing"},
		{name: "not ready", kind: "Issuer", failure: "pending"},
		{name: "forbidden", kind: "ClusterIssuer", failure: "forbidden"},
		{name: "replaced", kind: "Issuer", failure: "replaced"},
		{name: "denied", kind: "ClusterIssuer", failure: "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pod := podIdentity{"infra-smoketest", "cert-probe", "pod-uid"}
			existing := CertManagerObject(tc.kind, pod.namespace, "shared-issuer")
			if tc.kind == "ClusterIssuer" {
				existing.SetNamespace("")
			}
			existing.SetUID("existing-issuer-uid")
			existing.Object["spec"] = map[string]any{"ca": map[string]any{"secretName": "shared-ca"}}
			if tc.selfSigned {
				existing.Object["spec"] = map[string]any{"selfSigned": map[string]any{}}
			}
			existing.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
			caKey, ca, caPEM := testCA(t)
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			creates, issuerReads := 0, 0
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.failure != "missing" {
				builder = builder.WithObjects(existing.DeepCopy())
			}
			c := builder.WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					creates++
					if obj.GetObjectKind().GroupVersionKind().Kind == "Issuer" || obj.GetObjectKind().GroupVersionKind().Kind == "ClusterIssuer" {
						t.Fatal("created an issuer despite explicit selection")
					}
					if !tc.selfSigned {
						if _, ok := obj.(*corev1.Secret); ok {
							t.Fatal("CA issuance should keep the request key in memory")
						}
					}
					obj.SetUID(types.UID("fixture-" + obj.GetObjectKind().GroupVersionKind().Kind))
					return c.Create(ctx, obj, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, objectKey, obj, opts...); err != nil {
						return err
					}
					u, ok := obj.(*unstructured.Unstructured)
					if !ok {
						return nil
					}
					if u.GetKind() == tc.kind {
						issuerReads++
						switch tc.failure {
						case "forbidden":
							return apierrors.NewForbidden(schema.GroupResource{Group: "cert-manager.io", Resource: "clusterissuers"}, u.GetName(), errors.New("no access"))
						case "replaced":
							if issuerReads > 1 {
								u.SetUID("replacement-uid")
							}
						case "pending":
							u.Object["status"] = map[string]any{}
							if issuerReads > 1 {
								cancel()
							}
						}
						return nil
					}
					if u.GetKind() != "CertificateRequest" {
						return nil
					}
					name, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "name")
					kind, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "kind")
					group, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "group")
					if name != existing.GetName() || kind != tc.kind || group != "cert-manager.io" {
						t.Fatal("request did not select the existing issuer")
					}
					if tc.failure == "denied" {
						u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Denied", "status": "True", "reason": "Policy"}}}
						return nil
					}
					encoded, _, _ := unstructured.NestedString(u.Object, "spec", "request")
					csrPEM, _ := base64.StdEncoding.DecodeString(encoded)
					block, _ := pem.Decode(csrPEM)
					csr, err := x509.ParseCertificateRequest(block.Bytes)
					if err != nil || csr.CheckSignature() != nil {
						t.Fatalf("invalid CSR: %v", err)
					}
					var certPEM []byte
					if tc.selfSigned {
						secret := &corev1.Secret{}
						if err := c.Get(ctx, objectKey, secret); err != nil {
							t.Fatal(err)
						}
						if u.GetAnnotations()["cert-manager.io/private-key-secret-name"] != secret.Name {
							t.Fatal("missing self-signed key annotation")
						}
						block, _ := pem.Decode(secret.Data[corev1.TLSPrivateKeyKey])
						key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
						if err != nil {
							t.Fatal(err)
						}
						certPEM = certificatePEM(t, key.(*ecdsa.PrivateKey), csr.DNSNames[0], nil)
					} else {
						if u.GetAnnotations()["cert-manager.io/private-key-secret-name"] != "" {
							t.Fatal("CA request unexpectedly exposes a private-key Secret")
						}
						cert := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: csr.Subject, DNSNames: csr.DNSNames,
							NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
							KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
						der, err := x509.CreateCertificate(rand.Reader, cert, ca, csr.PublicKey, caKey)
						if err != nil {
							t.Fatal(err)
						}
						certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
					}
					u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
						"certificate": base64.StdEncoding.EncodeToString(certPEM), "ca": base64.StdEncoding.EncodeToString(caPEM)}
					return nil
				},
			}).Build()
			fingerprint, err := certManager(ctx, c, pod, definition.IssuerRef{Name: existing.GetName(), Kind: tc.kind}, time.Millisecond)
			if tc.failure == "" {
				if err != nil || len(fingerprint) != 64 {
					t.Fatalf("existing issuer failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("unusable existing issuer passed")
			}
			if (tc.failure == "missing" || tc.failure == "forbidden") && creates != 0 {
				t.Fatal("created fixtures after failed issuer lookup")
			}
			if tc.failure == "pending" && (!errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Issuer Ready")) {
				t.Fatalf("pending issuer error lost diagnostic: %v", err)
			}
			if tc.failure != "missing" {
				// Bypass the observation interceptor to compare the persisted issuer.
				list := &unstructured.UnstructuredList{}
				list.SetAPIVersion("cert-manager.io/v1")
				list.SetKind(tc.kind + "List")
				if err := c.List(context.Background(), list); err != nil || len(list.Items) != 1 {
					t.Fatalf("existing issuer disappeared: %v", err)
				}
				if list.Items[0].GetUID() != existing.GetUID() || len(list.Items[0].GetOwnerReferences()) != 0 {
					t.Fatal("existing issuer was replaced or adopted")
				}
			}
		})
	}
}

func TestIssuedCertificateChainValidation(t *testing.T) {
	caKey, ca, caPEM := testCA(t)
	intermediateKey, intermediate, _ := testCA(t)
	intermediate.Subject.CommonName = "intermediate"
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediate, ca, &intermediateKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err = x509.ParseCertificate(intermediateDER)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), DNSNames: []string{"probe.invalid"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, intermediate, &key.PublicKey, intermediateKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	chainPEM := append(append([]byte{}, leafPEM...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediateDER})...)
	_, _, wrongCA := testCA(t)
	selfSigned := certificatePEM(t, key, "probe.invalid", nil)
	for _, tc := range []struct {
		name      string
		chain, ca []byte
		valid     bool
	}{
		{"valid chain", chainPEM, caPEM, true},
		{"missing intermediate", leafPEM, caPEM, false},
		{"wrong CA", chainPEM, wrongCA, false},
		{"malformed CA", chainPEM, []byte("not PEM"), false},
		{"leaf as CA", chainPEM, leafPEM, false},
		{"untrusted self-signed", selfSigned, caPEM, false},
		{"no CA cannot trust returned chain", chainPEM, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateCertificate(tc.chain, keyPEM, tc.ca, "probe.invalid", false, time.Now())
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, got %v", tc.valid, err)
			}
		})
	}
}
