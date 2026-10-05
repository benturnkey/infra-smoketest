package probe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CertManagerObject uses the installed v1 API without depending on cert-manager's
// controller libraries. The controller also uses it to clean up probe fixtures.
func CertManagerObject(kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("cert-manager.io/v1")
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func certManager(ctx context.Context, c client.Client, pod podIdentity, ref definition.IssuerRef, interval time.Duration) (string, error) {
	createIssuer := ref.Name == ""
	issuer := CertManagerObject("Issuer", pod.namespace, pod.name)
	selfSigned := true
	if createIssuer {
		issuer.Object["spec"] = map[string]any{"selfSigned": map[string]any{}}
	} else {
		issuer = CertManagerObject(ref.Kind, pod.namespace, ref.Name)
		if ref.Kind == "ClusterIssuer" {
			issuer.SetNamespace("")
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(issuer), issuer); err != nil {
			return "", fmt.Errorf("get existing %s %s: %w", ref.Kind, ref.Name, err)
		}
		_, selfSigned, _ = unstructured.NestedMap(issuer.Object, "spec", "selfSigned")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dnsName := pod.name + ".invalid"
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
	}, key)
	if err != nil {
		return "", err
	}
	owner := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.name, UID: types.UID(pod.uid)}}
	var fixtures []client.Object
	if selfSigned {
		// SelfSigned issuers need the request's private key to sign it. For other
		// issuers the key stays in memory; no private-key Secret is necessary.
		fixtures = append(fixtures, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pod.name, Namespace: pod.namespace, OwnerReferences: owner},
			Data: map[string][]byte{corev1.TLSPrivateKeyKey: keyPEM}})
	}
	if createIssuer {
		issuer.SetOwnerReferences(owner)
		fixtures = append(fixtures, issuer)
	}
	request := CertManagerObject("CertificateRequest", pod.namespace, pod.name)
	request.SetOwnerReferences(owner)
	if selfSigned {
		request.SetAnnotations(map[string]string{"cert-manager.io/private-key-secret-name": pod.name})
	}
	request.Object["spec"] = map[string]any{
		"request":   base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		"issuerRef": map[string]any{"name": issuer.GetName(), "kind": issuer.GetKind(), "group": "cert-manager.io"},
		"duration":  "1h", "usages": []any{"digital signature", "server auth"},
	}
	// Never adopt or overwrite an existing object. The controller removes these
	// Pod-owned fixtures after the Pod has stopped, including timeout/cancellation.
	for _, obj := range append(fixtures, request) {
		if err := c.Create(ctx, obj); err != nil {
			return "", fmt.Errorf("create %T %s: %w", obj, pod.name, err)
		}
	}
	var fingerprint string
	err = poll(ctx, interval, func() (bool, error) {
		for _, obj := range []*unstructured.Unstructured{issuer, request} {
			expected := obj.GetUID()
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err), err
			}
			if obj.GetUID() != expected {
				return true, fmt.Errorf("%s identity changed", obj.GetKind())
			}
			conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			ready := false
			for _, raw := range conditions {
				condition, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if condition["type"] == "Denied" && condition["status"] == "True" || condition["type"] == "InvalidRequest" && condition["status"] == "True" || condition["type"] == "Ready" && condition["status"] == "False" && condition["reason"] == "Failed" {
					return true, fmt.Errorf("%s failed: %v: %v", obj.GetKind(), condition["reason"], condition["message"])
				}
				if condition["type"] == "Ready" && condition["status"] == "True" {
					ready = true
				}
			}
			if !ready {
				return false, fmt.Errorf("waiting for %s Ready: %v", obj.GetKind(), conditions)
			}
		}
		encoded, _, _ := unstructured.NestedString(request.Object, "status", "certificate")
		certPEM, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return true, fmt.Errorf("invalid issued certificate encoding: %w", err)
		}
		encodedCA, _, _ := unstructured.NestedString(request.Object, "status", "ca")
		caPEM, err := base64.StdEncoding.DecodeString(encodedCA)
		if err != nil {
			return true, fmt.Errorf("invalid issuer CA encoding: %w", err)
		}
		fingerprint, err = validateCertificate(certPEM, keyPEM, caPEM, dnsName, selfSigned, time.Now())
		return true, err
	})
	return fingerprint, err
}

func validateCertificate(certPEM, keyPEM, caPEM []byte, dnsName string, selfSigned bool, now time.Time) (string, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("issued certificate/key pair is invalid: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != dnsName {
		return "", fmt.Errorf("issued certificate does not contain the probe's exact DNS name")
	}
	roots := x509.NewCertPool()
	if selfSigned {
		if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
			return "", fmt.Errorf("invalid self-signed certificate: %w", err)
		}
		roots.AddCert(cert)
	} else if len(caPEM) > 0 {
		// The issuer supplies its trust bundle on this authenticated request.
		// Reject leaf certificates as trust anchors rather than trusting the
		// returned leaf to verify itself.
		for len(bytes.TrimSpace(caPEM)) > 0 {
			block, rest := pem.Decode(caPEM)
			if block == nil || block.Type != "CERTIFICATE" {
				return "", fmt.Errorf("invalid issuer CA PEM")
			}
			ca, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !ca.IsCA || !ca.BasicConstraintsValid {
				return "", fmt.Errorf("issuer CA bundle contains an invalid CA certificate")
			}
			roots.AddCert(ca)
			caPEM = rest
		}
	} else {
		roots, err = x509.SystemCertPool()
		if err != nil {
			return "", fmt.Errorf("load system CA roots: %w", err)
		}
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		intermediate, err := x509.ParseCertificate(der)
		if err != nil {
			return "", fmt.Errorf("invalid issued certificate chain: %w", err)
		}
		intermediates.AddCert(intermediate)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: dnsName, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return "", fmt.Errorf("issued certificate verification failed: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}
