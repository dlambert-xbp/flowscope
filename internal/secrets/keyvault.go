package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// KeyVaultSource resolves "kv:https://<vault>.vault.azure.net/secrets/<name>[/<version>]"
// refs against Azure Key Vault.
//
// Authentication uses azidentity.DefaultAzureCredential, which inside
// an AKS cluster with Workload Identity configured falls through to
// the federated identity step (AZURE_FEDERATED_TOKEN_FILE +
// AZURE_TENANT_ID + AZURE_CLIENT_ID set on the pod by the workload
// identity mutating webhook). Outside the cluster — e.g. on an
// operator's laptop — it falls through to environment, managed
// identity, or `az login` cached creds as documented by the Azure SDK.
//
// We deliberately do NOT pin to ManagedIdentityCredential or
// WorkloadIdentityCredential directly. DefaultAzureCredential composes
// them all and gives operators the option to debug locally with `az
// login` without code changes, which matters more than shaving the
// fallback chain.
//
// The vault client is constructed per-vault (not per-call) and cached
// on the source so repeated Loads against the same vault reuse the
// HTTP/2 connection and AAD token cache.
type KeyVaultSource struct {
	cred    azcore.TokenCredential
	newCli  func(vaultURL string, cred azcore.TokenCredential) (kvClient, error)
	clients map[string]kvClient
}

// kvClient is the slice of azsecrets.Client we actually use. Keeping
// this an internal interface lets tests inject a fake without pulling
// in the Azure HTTP stack.
type kvClient interface {
	GetSecret(ctx context.Context, name, version string, options *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}

// NewKeyVaultSource builds a KeyVaultSource backed by
// DefaultAzureCredential. Construction can fail if the Azure SDK
// cannot enumerate any credential source (e.g. unconfigured CLI on a
// dev box) — the error is propagated up so the operator sees it at
// startup rather than at first secret resolution.
func NewKeyVaultSource() (*KeyVaultSource, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: azidentity: %w", err)
	}
	return &KeyVaultSource{
		cred:    cred,
		newCli:  defaultNewKVClient,
		clients: make(map[string]kvClient),
	}, nil
}

func defaultNewKVClient(vaultURL string, cred azcore.TokenCredential) (kvClient, error) {
	cli, err := azsecrets.NewClient(vaultURL, cred, nil)
	if err != nil {
		return nil, err
	}
	return cli, nil
}

// Load implements Source. The ref grammar is:
//
//	kv:https://<vault>.vault.azure.net/secrets/<name>
//	kv:https://<vault>.vault.azure.net/secrets/<name>/<version>
//
// When no version is supplied azsecrets returns the current version,
// which is what we want for boot-time loading.
func (k *KeyVaultSource) Load(ctx context.Context, ref string) (string, error) {
	vaultURL, name, version, err := parseKVRef(ref)
	if err != nil {
		return "", err
	}
	cli, ok := k.clients[vaultURL]
	if !ok {
		cli, err = k.newCli(vaultURL, k.cred)
		if err != nil {
			return "", fmt.Errorf("secrets: keyvault client for %s: %w", vaultURL, err)
		}
		k.clients[vaultURL] = cli
	}
	resp, err := cli.GetSecret(ctx, name, version, nil)
	if err != nil {
		return "", fmt.Errorf("secrets: keyvault GetSecret %s/%s: %w", vaultURL, name, err)
	}
	if resp.Value == nil || *resp.Value == "" {
		return "", fmt.Errorf("secrets: keyvault %s/%s returned empty value", vaultURL, name)
	}
	return *resp.Value, nil
}

// parseKVRef breaks a "kv:..." ref into (vaultURL, secretName,
// version). Version is "" when unspecified, which azsecrets treats as
// "current". Returns an error for any URL that does not match the
// /secrets/<name>[/<version>] layout — we'd rather fail fast than
// silently mis-route to a different vault path.
func parseKVRef(ref string) (vaultURL, name, version string, err error) {
	raw := strings.TrimPrefix(ref, "kv:")
	if raw == "" {
		return "", "", "", errors.New("secrets: kv ref: missing URL")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", "", fmt.Errorf("secrets: kv ref parse: %w", perr)
	}
	if u.Scheme != "https" || u.Host == "" {
		return "", "", "", fmt.Errorf("secrets: kv ref %q: must be https://<vault>...", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "secrets" || parts[1] == "" {
		return "", "", "", fmt.Errorf("secrets: kv ref %q: expected /secrets/<name>[/<version>]", raw)
	}
	vaultURL = u.Scheme + "://" + u.Host
	name = parts[1]
	if len(parts) >= 3 {
		version = parts[2]
	}
	return vaultURL, name, version, nil
}
