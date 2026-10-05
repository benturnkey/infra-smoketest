package definition

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

const CertManagerIssuerNameEnv = "CERT_MANAGER_ISSUER_NAME"
const CertManagerIssuerKindEnv = "CERT_MANAGER_ISSUER_KIND"

// IssuerRef selects a cert-manager.io issuer. An empty Name requests a fresh
// namespaced SelfSigned Issuer; existing namespaced issuers must be in the Run's
// namespace, as required by CertificateRequest's issuerRef.
type IssuerRef struct {
	Name string
	Kind string
}

func ParseIssuerRef(name, kind string) (IssuerRef, error) {
	if name == "" && kind != "" {
		return IssuerRef{}, fmt.Errorf("%s requires %s", CertManagerIssuerKindEnv, CertManagerIssuerNameEnv)
	}
	if name != "" && len(validation.IsDNS1123Subdomain(name)) != 0 {
		return IssuerRef{}, fmt.Errorf("%s must be a valid Kubernetes resource name", CertManagerIssuerNameEnv)
	}
	if kind == "" {
		kind = "Issuer"
	}
	if kind != "Issuer" && kind != "ClusterIssuer" {
		return IssuerRef{}, fmt.Errorf("%s must be Issuer or ClusterIssuer", CertManagerIssuerKindEnv)
	}
	return IssuerRef{Name: name, Kind: kind}, nil
}
