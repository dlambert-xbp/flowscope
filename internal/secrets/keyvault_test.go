package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// fakeKVClient lets us exercise KeyVaultSource without ever opening a
// network connection or authenticating to Azure. The factory is
// injected via newCli (see NewKeyVaultSource); tests construct the
// source manually so they own the factory.
type fakeKVClient struct {
	values   map[string]string // key: name|version
	getCalls int
	err      error
}

func (f *fakeKVClient) GetSecret(_ context.Context, name, version string, _ *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error) {
	f.getCalls++
	if f.err != nil {
		return azsecrets.GetSecretResponse{}, f.err
	}
	v, ok := f.values[name+"|"+version]
	if !ok {
		return azsecrets.GetSecretResponse{}, errors.New("not found")
	}
	return azsecrets.GetSecretResponse{
		Secret: azsecrets.Secret{Value: &v},
	}, nil
}

func newTestKVSource(fake *fakeKVClient) *KeyVaultSource {
	return &KeyVaultSource{
		newCli: func(_ string, _ azcore.TokenCredential) (kvClient, error) {
			return fake, nil
		},
		clients: make(map[string]kvClient),
	}
}

func TestKeyVaultSource_Load(t *testing.T) {
	fake := &fakeKVClient{values: map[string]string{
		"snmp-master|": "kv-supplied-master-32-bytes-or-more",
	}}
	src := newTestKVSource(fake)

	got, err := src.Load(context.Background(), "kv:https://example-vault.vault.azure.net/secrets/snmp-master")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "kv-supplied-master-32-bytes-or-more" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyVaultSource_LoadWithVersion(t *testing.T) {
	fake := &fakeKVClient{values: map[string]string{
		"snmp-master|abc123": "versioned-value",
	}}
	src := newTestKVSource(fake)

	got, err := src.Load(context.Background(), "kv:https://example-vault.vault.azure.net/secrets/snmp-master/abc123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "versioned-value" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyVaultSource_ClientCached(t *testing.T) {
	// Two Loads against the same vault URL must reuse the underlying
	// client. That keeps AAD token cache + HTTP/2 connection warm
	// across repeat resolves, which matters when alert + api both
	// resolve the same key at startup in a single binary's lifetime.
	fake := &fakeKVClient{values: map[string]string{"snmp-master|": "v"}}
	calls := 0
	src := &KeyVaultSource{
		newCli: func(_ string, _ azcore.TokenCredential) (kvClient, error) {
			calls++
			return fake, nil
		},
		clients: make(map[string]kvClient),
	}
	for i := 0; i < 3; i++ {
		if _, err := src.Load(context.Background(), "kv:https://example-vault.vault.azure.net/secrets/snmp-master"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("newCli called %d times, want 1", calls)
	}
	if fake.getCalls != 3 {
		t.Fatalf("GetSecret called %d times, want 3", fake.getCalls)
	}
}

func TestKeyVaultSource_EmptyValue(t *testing.T) {
	empty := ""
	fake := &fakeKVClient{values: map[string]string{"name|": empty}}
	src := newTestKVSource(fake)

	_, err := src.Load(context.Background(), "kv:https://example-vault.vault.azure.net/secrets/name")
	if err == nil {
		t.Fatal("expected error for empty value from Key Vault")
	}
}

func TestKeyVaultSource_PropagatesError(t *testing.T) {
	fake := &fakeKVClient{err: errors.New("forbidden")}
	src := newTestKVSource(fake)

	_, err := src.Load(context.Background(), "kv:https://example-vault.vault.azure.net/secrets/name")
	if err == nil {
		t.Fatal("expected error from underlying GetSecret")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error %v should contain 'forbidden'", err)
	}
}

func TestParseKVRef(t *testing.T) {
	cases := []struct {
		ref         string
		wantVault   string
		wantName    string
		wantVersion string
		wantErr     bool
	}{
		{
			ref:       "kv:https://example.vault.azure.net/secrets/master",
			wantVault: "https://example.vault.azure.net",
			wantName:  "master",
		},
		{
			ref:         "kv:https://example.vault.azure.net/secrets/master/v123",
			wantVault:   "https://example.vault.azure.net",
			wantName:    "master",
			wantVersion: "v123",
		},
		{ref: "kv:", wantErr: true},
		{ref: "kv:http://insecure.vault.azure.net/secrets/x", wantErr: true},
		{ref: "kv:https://example.vault.azure.net/", wantErr: true},
		{ref: "kv:https://example.vault.azure.net/keys/x", wantErr: true},
		{ref: "kv:https://example.vault.azure.net/secrets/", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			v, n, ver, err := parseKVRef(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got vault=%q name=%q version=%q", v, n, ver)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v != tc.wantVault || n != tc.wantName || ver != tc.wantVersion {
				t.Fatalf("got vault=%q name=%q version=%q; want %q/%q/%q",
					v, n, ver, tc.wantVault, tc.wantName, tc.wantVersion)
			}
		})
	}
}
