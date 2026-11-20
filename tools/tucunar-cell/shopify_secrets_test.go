package canarynode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadShopifySecretsNormalizesDomainsAndRequiresEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shopify-secrets.json")
	if err := os.WriteFile(path, []byte(`{" Shop-A.myshopify.com ":"secret-a","shop-b.myshopify.com":"secret-b"}`), 0600); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}
	secrets, err := loadShopifySecrets(path)
	if err != nil {
		t.Fatalf("loadShopifySecrets() error = %v", err)
	}
	server := &Server{shopifySecrets: secrets}
	if got, ok := server.shopifySecret("SHOP-A.MYSHOPIFY.COM"); !ok || got != "secret-a" {
		t.Fatalf("normalized shop secret = %q, %v", got, ok)
	}
	if _, ok := server.shopifySecret("unknown.myshopify.com"); ok {
		t.Fatal("unknown shop unexpectedly received a configured secret")
	}
}

func TestShopifySecretUsesGlobalOnlyWithoutPerShopFile(t *testing.T) {
	server := &Server{secret: "development-secret"}
	if got, ok := server.shopifySecret("any-shop.myshopify.com"); !ok || got != "development-secret" {
		t.Fatalf("global fallback = %q, %v", got, ok)
	}
	server.secret = ""
	if got, ok := server.shopifySecret("any-shop.myshopify.com"); !ok || got != "" {
		t.Fatalf("empty development fallback = %q, %v", got, ok)
	}
}
